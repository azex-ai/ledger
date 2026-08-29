package r2_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/azex-ai/ledger/anchors/r2"
	"github.com/azex-ai/ledger/anchortest"
	"github.com/azex-ai/ledger/core"
)

// minioFixture is a real S3-compatible endpoint (MinIO, via testcontainers)
// standing in for the production Cloudflare R2 bucket -- R2's own account
// setup is deployment-time (docs/RUNBOOK.md "Choosing an Anchor carrier");
// what this test proves is that r2.Anchor's Publish/Head implementation
// correctly satisfies core.Anchor's contract against a real S3 API, not
// that R2 itself works (that connectivity is verified by the deployer per
// the RUNBOOK, same as any other carrier).
func minioFixture(t *testing.T) (endpoint, accessKey, secret string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping MinIO-backed integration test")
	}

	ctx := context.Background()
	container, err := tcminio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z")
	if err != nil {
		if strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
			t.Skip("Docker daemon not running, skipping integration test")
		}
		t.Fatalf("start minio container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate minio container: %v", err)
		}
	})

	endpoint, err = container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio connection string: %v", err)
	}
	return "http://" + endpoint, container.Username, container.Password
}

// createLockedBucket creates bucket with Object Lock enabled -- the same
// bucket-level configuration docs/RUNBOOK.md asks a production deployer to
// set up on the real R2 bucket. Object Lock requires versioning, which
// CreateBucket enables implicitly when ObjectLockEnabledForBucket is set;
// no default retention period is configured here (that is a deployment
// choice, not something r2.Anchor's client logic depends on -- see r2.go's
// package doc).
func createLockedBucket(t *testing.T, endpoint, accessKey, secret, bucket string) {
	t.Helper()
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secret, ""),
		UsePathStyle: true,
	})
	_, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket:                     aws.String(bucket),
		ObjectLockEnabledForBucket: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("create locked bucket: %v", err)
	}
}

func TestAnchor_Conformance(t *testing.T) {
	endpoint, accessKey, secret := minioFixture(t)
	const bucket = "ledger-anchor-conformance"
	createLockedBucket(t, endpoint, accessKey, secret, bucket)

	anchortest.RunConformance(t, func() core.Anchor {
		a, err := r2.New(r2.Config{
			Endpoint:        endpoint,
			AccessKeyID:     accessKey,
			SecretAccessKey: secret,
			Bucket:          bucket,
			Key:             "anchor/head.json",
			Region:          "us-east-1",
		})
		if err != nil {
			t.Fatalf("new anchor: %v", err)
		}
		return a
	})
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

// TestAnchor_PublishRejectsOlderSeq pins the behavior r2.go's Publish
// documents but anchortest deliberately leaves untested (its package doc:
// "Ordering of a seq other than an exact replay" is not part of
// core.Anchor's contract) -- r2.Anchor's own added strictness is refusing
// a seq older than the current head, rather than silently corrupting
// Head's "highest seq" guarantee.
func TestAnchor_PublishRejectsOlderSeq(t *testing.T) {
	endpoint, accessKey, secret := minioFixture(t)
	const bucket = "ledger-anchor-older-seq"
	createLockedBucket(t, endpoint, accessKey, secret, bucket)

	a, err := r2.New(r2.Config{
		Endpoint:        endpoint,
		AccessKeyID:     accessKey,
		SecretAccessKey: secret,
		Bucket:          bucket,
		Key:             "anchor/head.json",
		Region:          "us-east-1",
	})
	if err != nil {
		t.Fatalf("new anchor: %v", err)
	}

	ctx := context.Background()
	head1 := []byte("11111111111111111111111111111111")
	head2 := []byte("22222222222222222222222222222222")
	if err := a.Publish(ctx, 5, head1); err != nil {
		t.Fatalf("Publish(5, ...): %v", err)
	}
	if err := a.Publish(ctx, 6, head2); err != nil {
		t.Fatalf("Publish(6, ...): %v", err)
	}

	staleErr := a.Publish(ctx, 3, []byte("stale"))
	if staleErr == nil {
		t.Fatal("Publish(3, ...) after head advanced to 6: expected error, got nil")
	}
	if !errors.Is(staleErr, core.ErrInvalidInput) {
		t.Fatalf("Publish(3, ...) error = %v, want wrapping core.ErrInvalidInput", staleErr)
	}

	seq, head, err := a.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if seq != 6 || string(head) != string(head2) {
		t.Fatalf("Head after rejected stale publish = (%d, %q), want (6, %q) -- unchanged", seq, head, head2)
	}
}
