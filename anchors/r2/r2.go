// Package r2 implements core.Anchor (design doc §8.3, P6) on an
// S3-compatible object store with Object Lock enabled -- Cloudflare R2 is
// the intended production carrier (docs/RUNBOOK.md "Choosing an Anchor
// carrier"), but this package speaks the plain S3 API so any
// Object-Lock-capable S3-compatible endpoint (including a local MinIO
// container in tests) works identically.
//
// This module is deliberately split out of the root github.com/azex-ai/ledger
// module (mirroring chains/evm's split for go-ethereum): the AWS SDK is a
// substantial dependency tree that a library consumer who does not use this
// particular Anchor carrier should never have to pull into their build.
// Only a consumer that imports github.com/azex-ai/ledger/anchors/r2
// compiles against it.
//
// # What this package deliberately does not do
//
//   - It does not configure Object Lock on the bucket, set a default
//     retention period, or create the bucket. Those are one-time,
//     deployment-time operations (docs/RUNBOOK.md's "Choosing an Anchor
//     carrier" section documents them) -- a per-Publish-call adapter is the
//     wrong place to repeat them, and getting them wrong there would be
//     silent (Object Lock's guarantee comes entirely from the bucket-level
//     configuration; this package cannot verify it from the outside any
//     more than anchortest can, see core.Anchor's doc comment).
//   - It does not read the environment or derive credentials on its own
//     (abstractions.md Environment Parity, mirroring chains/evm.ClientSet's
//     own doc comment) -- Config is built and injected by the caller's
//     composition root.
package r2

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/azex-ai/ledger/core"
)

// Config configures Anchor's connection to an S3-compatible endpoint.
// Every field is required unless noted -- there are no environment-derived
// defaults (see package doc's "does not read the environment").
type Config struct {
	// Endpoint is the full S3-compatible endpoint URL, e.g.
	// "https://<account-id>.r2.cloudflarestorage.com" for Cloudflare R2, or
	// a local MinIO container's address in tests. The caller derives this
	// (from a Cloudflare account ID, or wherever else) -- this package only
	// consumes the final URL.
	Endpoint string
	// AccessKeyID / SecretAccessKey authorize this Anchor's requests. For
	// production, this MUST be the "ledger side" credential documented in
	// docs/RUNBOOK.md — scoped to GetObject + PutObject on Key alone, never
	// DeleteObject or broader bucket permissions.
	AccessKeyID     string
	SecretAccessKey string
	// Bucket is the Object-Lock-enabled bucket to publish into.
	Bucket string
	// Key is the single object key this Anchor reads and writes -- see the
	// package doc for why one versioned key, not one key per seq, is
	// sufficient to satisfy core.Anchor's contract.
	Key string
	// Region is the S3 region to sign requests for. Cloudflare R2 ignores
	// this value operationally but the SDK still requires a non-empty
	// string to construct signed requests; R2's own documentation says to
	// pass "auto". Object-storage servers that do check region (MinIO in
	// tests, real AWS S3) should pass their actual region.
	Region string
}

// Anchor implements core.Anchor by storing the current (seq, head) pair as
// a single JSON object at Config.Key, relying on the bucket's Object Lock
// configuration to make every past VERSION of that key un-overwritable and
// un-deletable once written (package doc). Publish rewrites Key's current
// version on every successful call; Object Lock retains the version it
// replaces, not just the latest one -- so nothing published is ever lost,
// even though Head only ever needs to resolve the single latest version.
type Anchor struct {
	client *s3.Client
	bucket string
	key    string
}

var _ core.Anchor = (*Anchor)(nil)

// head is the on-the-wire shape of the object stored at Config.Key.
type head struct {
	Seq  int64  `json:"seq"`
	Head string `json:"head"` // hex-encoded
}

// New builds an Anchor from cfg. It performs no network calls itself --
// dialing happens lazily on the first Publish/Head call, same as any other
// S3 SDK client.
func New(cfg Config) (*Anchor, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("r2: config: endpoint required: %w", core.ErrInvalidInput)
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("r2: config: access key id and secret access key required: %w", core.ErrInvalidInput)
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("r2: config: bucket required: %w", core.ErrInvalidInput)
	}
	if cfg.Key == "" {
		return nil, fmt.Errorf("r2: config: key required: %w", core.ErrInvalidInput)
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: true,
	})

	return &Anchor{client: client, bucket: cfg.Bucket, key: cfg.Key}, nil
}

// Publish implements core.Anchor. It checks the existing recorded head
// BEFORE writing anything (docs/RUNBOOK.md, board #55 task instructions):
// under Object Lock, a wrongly-written version cannot be taken back, so the
// mismatch check has to happen on the read side, never discovered after
// the fact by comparing a write that already landed.
func (a *Anchor) Publish(ctx context.Context, seq int64, headBytes []byte) error {
	curSeq, curHead, err := a.Head(ctx)
	if err != nil {
		return fmt.Errorf("r2: publish: read current head: %w", err)
	}

	switch {
	case seq == curSeq && curSeq != 0:
		if !bytes.Equal(curHead, headBytes) {
			return fmt.Errorf("r2: publish: seq %d already published with a different head", seq)
		}
		return nil // idempotent replay -- nothing to write.
	case seq > curSeq:
		// New head -- falls through to the write below.
	default:
		// seq < curSeq (or seq == curSeq == 0, which is not a meaningful
		// publish -- 0 is Head's own "empty" sentinel, never a real seq).
		// core.Anchor's doc comment only specifies exact-replay behavior
		// (checked above); it takes no position on an out-of-order seq
		// older than the current head (anchortest's package doc says so
		// explicitly). Rejecting rather than silently accepting is the
		// safe default here: Head's contract is "the HIGHEST seq the
		// anchor knows about", and blindly overwriting Key with an older
		// seq would violate that for every future Head call.
		return fmt.Errorf("r2: publish: seq %d is not newer than the current head (seq %d): %w", seq, curSeq, core.ErrInvalidInput)
	}

	body, err := json.Marshal(head{Seq: seq, Head: hex.EncodeToString(headBytes)})
	if err != nil {
		return fmt.Errorf("r2: publish: marshal: %w", err)
	}

	_, err = a.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(a.key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("r2: publish: put object: %w", err)
	}
	return nil
}

// Head implements core.Anchor. It always reads the object's current
// version directly from the bucket -- never a local cache, never the
// ledger database (core.Anchor's doc comment).
func (a *Anchor) Head(ctx context.Context) (int64, []byte, error) {
	out, err := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(a.key),
	})
	if err != nil {
		if isNotFound(err) {
			return 0, nil, nil
		}
		return 0, nil, fmt.Errorf("r2: head: get object: %w", err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("r2: head: read body: %w", err)
	}

	var h head
	if err := json.Unmarshal(data, &h); err != nil {
		return 0, nil, fmt.Errorf("r2: head: %s/%s is malformed: %w", a.bucket, a.key, err)
	}
	headBytes, err := hex.DecodeString(h.Head)
	if err != nil {
		return 0, nil, fmt.Errorf("r2: head: %s/%s: decode head: %w", a.bucket, a.key, err)
	}
	return h.Seq, headBytes, nil
}

// isNotFound reports whether err is S3's "no such key" response -- the
// expected shape of Head on an anchor nothing has been published to yet
// (core.Anchor's doc comment: "or 0 if empty", never an error).
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	// R2 (and some other S3-compatible servers) reply with a generic 404
	// rather than the NoSuchKey-shaped error AWS S3 itself returns for
	// GetObject; smithy's generic HTTP response error carries the status
	// code either way, so fall back to that.
	var httpErr *smithyhttp.ResponseError
	if errors.As(err, &httpErr) {
		return httpErr.HTTPStatusCode() == 404
	}
	return false
}
