package r2_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/azex-ai/ledger/anchors/r2"
	"github.com/azex-ai/ledger/anchors/r2/internal/s3test"
	"github.com/azex-ai/ledger/anchortest"
	"github.com/azex-ai/ledger/core"
)

// createBucket creates a plain bucket on the in-process fake server
// (internal/s3test). The fake has no Object Lock; the tests in this file
// exercise anchors/r2's own logic -- create-only publishing, the listing,
// the fail-closed parse -- which does not depend on it. The WORM guarantee
// the production bucket adds (docs/RUNBOOK.md) is a property of the real
// server and is asserted against one in r2_worm_test.go (`s3worm` build tag,
// `make test-r2-worm`).
func createBucket(t *testing.T, endpoint, accessKey, secret, bucket string) {
	t.Helper()
	client := newRawClient(endpoint, accessKey, secret)
	if _, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
}

// newRawClient is the "attacker's" client: the same S3 API this package
// speaks, used directly, without going through Publish. This is what makes
// the out-of-band phases in the conformance suite real rather than
// hypothetical -- the credential it holds is the ledger's own.
func newRawClient(endpoint, accessKey, secret string) *s3.Client {
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secret, ""),
		UsePathStyle: true,
	})
}

const testPrefix = "anchor/head"

func TestAnchor_Conformance(t *testing.T) {
	endpoint, accessKey, secret := s3test.Fixture(t)
	const bucket = "ledger-anchor-conformance"
	createBucket(t, endpoint, accessKey, secret, bucket)

	newAnchor := func() core.Anchor {
		a, err := r2.New(r2.Config{
			Endpoint:        endpoint,
			AccessKeyID:     accessKey,
			SecretAccessKey: secret,
			Bucket:          bucket,
			Key:             testPrefix,
			Region:          "us-east-1",
		})
		if err != nil {
			t.Fatalf("new anchor: %v", err)
		}
		return a
	}

	// WithOutOfBandWrite lends the suite this package's own client, bypassing
	// Publish entirely -- the attack the audit found (M-4): a plain
	// PutObject with the ledger's credential. Without the hook the
	// corresponding phase reports as skipped, which for THIS implementation
	// would leave its central guarantee unexercised.
	raw := newRawClient(endpoint, accessKey, secret)
	anchortest.RunConformance(t, newAnchor, anchortest.WithOutOfBandWrite(func(seq int64, head []byte) error {
		body, err := json.Marshal(map[string]any{"seq": seq, "head": hex.EncodeToString(head)})
		if err != nil {
			return err
		}
		_, err = raw.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(fmt.Sprintf("%s/seq-%020d.json", testPrefix, seq)),
			Body:   bytes.NewReader(body),
		})
		return err
	}))
}

func TestNew_ValidatesConfig(t *testing.T) {
	base := r2.Config{
		Endpoint:        "http://localhost:9000",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		Bucket:          "bucket",
		Key:             "anchor/head.json",
	}

	tests := []struct {
		name   string
		mutate func(*r2.Config)
	}{
		{"missing endpoint", func(c *r2.Config) { c.Endpoint = "" }},
		{"missing access key", func(c *r2.Config) { c.AccessKeyID = "" }},
		{"missing secret", func(c *r2.Config) { c.SecretAccessKey = "" }},
		{"missing bucket", func(c *r2.Config) { c.Bucket = "" }},
		{"missing key", func(c *r2.Config) { c.Key = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			_, err := r2.New(cfg)
			if !errors.Is(err, core.ErrInvalidInput) {
				t.Fatalf("New(%+v) error = %v, want wrapping core.ErrInvalidInput", cfg, err)
			}
		})
	}
}

// TestAnchor_PublishIsCreateOnlyPerSeq pins the layout change (2026-09-02
// audit, tamper-evident.md M-4 / C-M4). Each seq is its own immutable
// object, so:
//
//   - publishing an older seq that was never published is allowed and
//     CANNOT move the head (the old single-key layout had to refuse it; here
//     refusing would only block a legitimate backfill);
//   - re-publishing a seq with different bytes is refused, and nothing is
//     overwritten;
//   - re-publishing it with identical bytes is an idempotent success.
func TestAnchor_PublishIsCreateOnlyPerSeq(t *testing.T) {
	endpoint, accessKey, secret := s3test.Fixture(t)
	const bucket = "ledger-anchor-create-only"
	createBucket(t, endpoint, accessKey, secret, bucket)

	a, err := r2.New(r2.Config{
		Endpoint:        endpoint,
		AccessKeyID:     accessKey,
		SecretAccessKey: secret,
		Bucket:          bucket,
		Key:             testPrefix,
		Region:          "us-east-1",
	})
	if err != nil {
		t.Fatalf("new anchor: %v", err)
	}

	ctx := context.Background()
	head5 := []byte("55555555555555555555555555555555")
	head6 := []byte("66666666666666666666666666666666")
	if err := a.Publish(ctx, 5, head5); err != nil {
		t.Fatalf("Publish(5, ...): %v", err)
	}
	if err := a.Publish(ctx, 6, head6); err != nil {
		t.Fatalf("Publish(6, ...): %v", err)
	}

	// An older, previously unpublished seq: accepted, head unchanged.
	if err := a.Publish(ctx, 3, []byte("33333333333333333333333333333333")); err != nil {
		t.Fatalf("Publish(3, ...) for a seq that was never published: %v", err)
	}
	assertHead(t, a, 6, head6)

	// Same seq, different bytes: refused, nothing overwritten.
	if err := a.Publish(ctx, 6, []byte("ffffffffffffffffffffffffffffffff")); err == nil {
		t.Fatal("Publish(6, <different bytes>): expected error, got nil")
	}
	assertHead(t, a, 6, head6)

	// Same seq, same bytes: idempotent success.
	if err := a.Publish(ctx, 6, head6); err != nil {
		t.Fatalf("idempotent replay of Publish(6, ...): %v", err)
	}
	assertHead(t, a, 6, head6)

	// seq 0 is Head's "empty" sentinel, never a real publish.
	if err := a.Publish(ctx, 0, head6); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("Publish(0, ...) error = %v, want wrapping core.ErrInvalidInput", err)
	}
}

// TestAnchor_HeadFailsClosedOnAForeignObject pins the fail-closed listing
// (working-agreements.md §3): an object under this anchor's prefix that is
// not one of its seq objects makes Head return an error instead of a
// confident number. Silently skipping it would mean answering "the head is
// 2" about a prefix somebody else is also writing to.
func TestAnchor_HeadFailsClosedOnAForeignObject(t *testing.T) {
	endpoint, accessKey, secret := s3test.Fixture(t)
	const bucket = "ledger-anchor-foreign-object"
	createBucket(t, endpoint, accessKey, secret, bucket)

	a, err := r2.New(r2.Config{
		Endpoint:        endpoint,
		AccessKeyID:     accessKey,
		SecretAccessKey: secret,
		Bucket:          bucket,
		Key:             testPrefix,
		Region:          "us-east-1",
	})
	if err != nil {
		t.Fatalf("new anchor: %v", err)
	}
	ctx := context.Background()
	if err := a.Publish(ctx, 1, []byte("11111111111111111111111111111111")); err != nil {
		t.Fatalf("Publish(1, ...): %v", err)
	}

	raw := newRawClient(endpoint, accessKey, secret)
	if _, err := raw.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(testPrefix + "/notes.txt"),
		Body:   bytes.NewReader([]byte("hello")),
	}); err != nil {
		t.Fatalf("write a foreign object: %v", err)
	}

	if _, _, err := a.Head(ctx); err == nil {
		t.Fatal("Head with a foreign object under the prefix: expected an error, got nil")
	}
}

func assertHead(t *testing.T, a core.Anchor, wantSeq int64, wantHead []byte) {
	t.Helper()
	seq, head, err := a.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if seq != wantSeq || !bytes.Equal(head, wantHead) {
		t.Fatalf("Head = (%d, %x), want (%d, %x)", seq, head, wantSeq, wantHead)
	}
}
