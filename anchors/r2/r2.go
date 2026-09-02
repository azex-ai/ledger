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
// # Layout: one object per seq
//
// Each published attestation gets its OWN object, at
// "<Config.Key>/seq-<20-digit-zero-padded-seq>.json", written with a
// conditional create (If-None-Match: "*"). Head lists the prefix and
// resolves the HIGHEST seq present, then reads that one object.
//
// This replaced a single mutable object at Config.Key, and the reason is the
// 2026-09-02 audit's M-4. Under the old layout the trusted head was whatever
// the latest version of one key said, and the ledger's own credential holds
// PutObject on it -- so a single out-of-band PutObject of {"seq":0,...}
// rolled the published head backwards, Object Lock notwithstanding (Object
// Lock protects each past VERSION from being deleted or overwritten; it does
// not stop a NEW version from becoming the current one, and nothing in this
// library ever read version history). core.Anchor.Head's contract is "the
// highest seq ever published, never regressing", and the old layout could
// not deliver it against the credential it was deployed with.
//
// What the new layout guarantees, and how:
//
//   - Head never regresses: seq N's object exists forever once created, so
//     MAX(seq) over the prefix cannot go down. The remaining way to lower it
//     is DELETING objects, which is why the ledger-side credential must not
//     carry DeleteObject (docs/RUNBOOK.md) -- note that Object Lock alone
//     does NOT save you here: a plain DELETE on a versioned bucket writes a
//     delete MARKER, which is permitted under retention and hides the object
//     from a listing. Retention does stop deletion of a specific version,
//     which is what this package's own tests pin.
//   - A published seq's CONTENT cannot be quietly changed to something
//     self-consistent: the conditional create refuses to overwrite where the
//     server honours it, and Publish reads the existing object back and
//     compares bytes before treating a repeat as an idempotent replay. Even
//     against a server that honours neither, rewriting seq N's head is not
//     enough for an attacker: service.VerifyLedger compares the anchored head
//     against the DB's own root_hash for that seq, and the DB row's signature
//     covers root_hash -- so making the two agree requires the P5/P6 signing
//     key, which is the one thing this whole mechanism assumes the
//     database-credential holder does not have.
//
// Higher-seq INJECTION (writing seq 9999999 with garbage) is deliberately
// left possible-but-loud rather than prevented: VerifyLedger reports "anchor
// knows about seq X but the DB chain only reaches Y" as TAMPERED. A forward
// jump cannot hide anything; only a backward move can, and that is what the
// layout makes impossible.
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
//   - It does not read object VERSION history. Version history is a
//     post-hoc forensic record for a human, not an input to verification --
//     see the reasoning above for why verification does not need it.
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
	"strings"

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
	// Key is the object key PREFIX this Anchor owns. One object per
	// published seq lives under it, at
	// "<Key>/seq-<20-digit-zero-padded>.json" -- see the package doc's
	// "Layout: one object per seq" for why a single mutable key could not
	// satisfy core.Anchor.Head's no-regression contract.
	//
	// A trailing slash is optional and ignored. The prefix must be owned by
	// this anchor alone: Head fails closed if it finds an object under it
	// that does not match the seq-object naming, rather than ignoring it and
	// possibly ignoring a real head.
	Key string
	// Region is the S3 region to sign requests for. Cloudflare R2 ignores
	// this value operationally but the SDK still requires a non-empty
	// string to construct signed requests; R2's own documentation says to
	// pass "auto". Object-storage servers that do check region (MinIO in
	// tests, real AWS S3) should pass their actual region.
	Region string
}

// Anchor implements core.Anchor by storing ONE immutable JSON object per
// published seq under Config.Key's prefix (package doc: "Layout: one object
// per seq"). Publish creates seq's object and never overwrites one; Head
// lists the prefix and resolves the highest seq present.
type Anchor struct {
	client *s3.Client
	bucket string
	prefix string
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

	return &Anchor{client: client, bucket: cfg.Bucket, prefix: strings.TrimSuffix(cfg.Key, "/")}, nil
}

// seqObjectPattern is the object name for one published seq. Zero-padded to
// 20 digits (int64's maximum is 19) so lexicographic key order -- the only
// order S3 listing offers -- equals numeric seq order. Head parses the
// number anyway rather than relying on that, but the padding keeps a bucket
// listing readable by a human and makes pagination behave.
const seqObjectPattern = "seq-%020d.json"

// objectKey is seq's object key under this anchor's prefix.
func (a *Anchor) objectKey(seq int64) string {
	return fmt.Sprintf("%s/"+seqObjectPattern, a.prefix, seq)
}

// seqFromObjectKey is objectKey's inverse. ok=false means the key is not one
// of this anchor's seq objects, which Head treats as a hard error rather
// than skipping -- an unexpected object under a prefix this anchor is
// documented to own alone is either a misconfiguration (two deployments
// sharing a prefix) or someone writing where they should not, and neither
// should resolve to a confident answer about the head.
func seqFromObjectKey(prefix, key string) (int64, bool) {
	name := strings.TrimPrefix(key, prefix+"/")
	if name == key || strings.Contains(name, "/") {
		return 0, false
	}
	var seq int64
	if _, err := fmt.Sscanf(name, seqObjectPattern, &seq); err != nil {
		return 0, false
	}
	// Round-trip check: rejects "seq-1.json" (unpadded) and any other
	// spelling that would sort wrongly or collide.
	if fmt.Sprintf(seqObjectPattern, seq) != name {
		return 0, false
	}
	return seq, true
}

// Publish implements core.Anchor. It creates seq's own object and never
// overwrites an existing one:
//
//  1. read seq's object. If it exists, this is a replay: identical bytes
//     succeed (core.Anchor's idempotency requirement), different bytes are
//     an error and nothing is written.
//  2. otherwise write it with If-None-Match: "*", so a server that honours
//     conditional writes (Cloudflare R2 does; so do recent MinIO releases)
//     enforces create-only on its side too, and a race with another writer
//     loses rather than clobbers.
//
// Step 1 exists in addition to step 2 because an S3-compatible server that
// silently ignores the If-None-Match header would otherwise turn a
// mismatched replay into a silent overwrite -- the exact failure design doc
// §8.3 point 2 exists to prevent. Step 2 exists in addition to step 1
// because step 1 alone is a read-then-write, and the guarantee should not
// rest on this library being the only writer.
//
// Publishing an OLDER seq that has not been published yet is allowed and
// harmless: each seq is its own object, so it cannot displace the head
// (Head resolves the maximum). That is a change from the single-key layout,
// which had to refuse it; here refusing would only prevent a legitimate
// backfill of a gap.
func (a *Anchor) Publish(ctx context.Context, seq int64, headBytes []byte) error {
	if seq <= 0 {
		// 0 is Head's own "empty" sentinel, never a real seq.
		return fmt.Errorf("r2: publish: seq must be positive, got %d: %w", seq, core.ErrInvalidInput)
	}

	existing, found, err := a.readSeqObject(ctx, seq)
	if err != nil {
		return err
	}
	if found {
		if !bytes.Equal(existing, headBytes) {
			return fmt.Errorf("r2: publish: seq %d already published with a different head", seq)
		}
		return nil // idempotent replay -- nothing to write.
	}

	body, err := json.Marshal(head{Seq: seq, Head: hex.EncodeToString(headBytes)})
	if err != nil {
		return fmt.Errorf("r2: publish: marshal: %w", err)
	}

	_, err = a.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(a.objectKey(seq)),
		Body:   bytes.NewReader(body),
		// Create-only. On a server that honours it, a second writer for the
		// same seq gets 412 rather than overwriting what is already there.
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		if isPreconditionFailed(err) {
			// Someone created seq's object between step 1 and here. Not an
			// error if it says the same thing; a genuine conflict if not.
			existing, found, readErr := a.readSeqObject(ctx, seq)
			if readErr != nil {
				return readErr
			}
			if found && bytes.Equal(existing, headBytes) {
				return nil
			}
			return fmt.Errorf("r2: publish: seq %d already published with a different head", seq)
		}
		return fmt.Errorf("r2: publish: put object: %w", err)
	}
	return nil
}

// Head implements core.Anchor: the HIGHEST seq ever published, resolved by
// listing this anchor's prefix -- never a local cache, never the ledger
// database (core.Anchor's doc comment), and never "whatever was written
// last" (its no-regression contract).
func (a *Anchor) Head(ctx context.Context) (int64, []byte, error) {
	var highest int64
	var continuationToken *string
	for {
		out, err := a.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(a.bucket),
			Prefix:            aws.String(a.prefix + "/"),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return 0, nil, fmt.Errorf("r2: head: list objects: %w", err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			seq, ok := seqFromObjectKey(a.prefix, key)
			if !ok {
				// Fail closed: see seqFromObjectKey's doc comment. Ignoring
				// it would mean answering confidently about a prefix that
				// contains something this anchor did not put there.
				return 0, nil, fmt.Errorf("r2: head: unexpected object %q under prefix %q: this prefix must contain only this anchor's seq objects", key, a.prefix)
			}
			if seq > highest {
				highest = seq
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken
	}
	if highest == 0 {
		// Nothing published yet (core.Anchor: "or 0 if empty", never an
		// error).
		return 0, nil, nil
	}

	headBytes, found, err := a.readSeqObject(ctx, highest)
	if err != nil {
		return 0, nil, err
	}
	if !found {
		// It was listed a moment ago. Something is deleting objects under
		// this prefix, which is precisely what must never happen silently.
		return 0, nil, fmt.Errorf("r2: head: object for seq %d was listed but could not be read", highest)
	}
	return highest, headBytes, nil
}

// readSeqObject reads and decodes seq's object. found=false with a nil error
// means the object does not exist; every other failure is an error, never a
// zero value that a caller might read as "empty".
func (a *Anchor) readSeqObject(ctx context.Context, seq int64) (headBytes []byte, found bool, err error) {
	key := a.objectKey(seq)
	out, err := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("r2: read %s/%s: %w", a.bucket, key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, fmt.Errorf("r2: read %s/%s: read body: %w", a.bucket, key, err)
	}
	var h head
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, false, fmt.Errorf("r2: %s/%s is malformed: %w", a.bucket, key, err)
	}
	if h.Seq != seq {
		// The object's own body disagrees with the key it is stored under.
		// Nothing legitimate produces that, and picking either value would
		// be guessing.
		return nil, false, fmt.Errorf("r2: %s/%s claims seq %d: object body and key disagree", a.bucket, key, h.Seq)
	}
	decoded, err := hex.DecodeString(h.Head)
	if err != nil {
		return nil, false, fmt.Errorf("r2: %s/%s: decode head: %w", a.bucket, key, err)
	}
	return decoded, true, nil
}

// isPreconditionFailed reports whether err is the 412 an S3 server returns
// when If-None-Match: "*" hits an object that already exists.
func isPreconditionFailed(err error) bool {
	var httpErr *smithyhttp.ResponseError
	if errors.As(err, &httpErr) {
		return httpErr.HTTPStatusCode() == 412
	}
	return false
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
