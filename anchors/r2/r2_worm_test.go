//go:build s3worm

package r2_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/azex-ai/ledger/anchors/r2"
)

// This file needs a REAL S3-compatible server with Object Lock support
// (MinIO, or R2 itself): the guarantee it asserts belongs to the server, and
// the in-process fake the rest of this package's tests use (internal/s3test)
// does not implement Object Lock. It is behind the `s3worm` build tag and run
// by `make test-r2-worm` (docs/TESTING.md). It is never skipped for a missing
// server: built with the tag, an unreachable server fails it.
//
// The server is read from the environment, defaulting to a MinIO on this
// machine with MinIO's stock root credential:
//
//	LEDGER_TEST_S3_ENDPOINT           http://127.0.0.1:9000
//	LEDGER_TEST_S3_ACCESS_KEY_ID      minioadmin
//	LEDGER_TEST_S3_SECRET_ACCESS_KEY  minioadmin
const (
	defaultWORMEndpoint  = "http://127.0.0.1:9000"
	defaultWORMAccessKey = "minioadmin"
	defaultWORMSecretKey = "minioadmin" //nolint:gosec // MinIO's stock local root credential, test-only

	// wormBucket is ONE bucket reused by every run, not a bucket per run:
	// its objects carry a COMPLIANCE retention, so neither they nor the
	// bucket can be deleted until the retention expires -- a per-run bucket
	// would leave one more undeletable bucket behind on every run. Runs are
	// isolated by key prefix (wormPrefix) instead.
	wormBucket = "ledger-test-worm"
)

func wormServer(t *testing.T) (endpoint, accessKey, secret string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping the real-server Object Lock test")
	}
	get := func(name, fallback string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return fallback
	}
	return get("LEDGER_TEST_S3_ENDPOINT", defaultWORMEndpoint),
		get("LEDGER_TEST_S3_ACCESS_KEY_ID", defaultWORMAccessKey),
		get("LEDGER_TEST_S3_SECRET_ACCESS_KEY", defaultWORMSecretKey)
}

// ensureLockedBucket makes wormBucket exist with Object Lock enabled AND a
// default retention period -- the same bucket-level configuration
// docs/RUNBOOK.md asks a production deployer to set up on the real R2
// bucket. Idempotent: a bucket this credential already owns is reused.
//
// The retention is not decoration (2026-09-02 audit, F-m11): a bucket with
// Object Lock enabled and NO default retention leaves every object freely
// deletable, so a suite against it runs plain S3 semantics while the code's
// comments talk about WORM.
//
// COMPLIANCE mode, 1 day: GOVERNANCE mode can be bypassed by a caller with
// s3:BypassGovernanceRetention, which would make the assertion depend on the
// test credential's policy rather than on the lock.
func ensureLockedBucket(t *testing.T, client *s3.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket:                     aws.String(wormBucket),
		ObjectLockEnabledForBucket: aws.Bool(true),
	})
	var owned *types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &owned) {
		t.Fatalf("create locked bucket %s on the S3 server (is it running? see docs/TESTING.md, `make test-r2-worm`): %v",
			wormBucket, err)
	}
	_, err = client.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
		Bucket: aws.String(wormBucket),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{
			ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule: &types.ObjectLockRule{
				DefaultRetention: &types.DefaultRetention{
					Mode: types.ObjectLockRetentionModeCompliance,
					Days: aws.Int32(1),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("configure default object-lock retention on %s (a bucket created without Object Lock cannot "+
			"gain it later -- remove it once its objects' retention has expired): %v", wormBucket, err)
	}
}

// wormPrefix is this run's own key prefix inside wormBucket.
func wormPrefix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random prefix: %v", err)
	}
	return "anchor/worm-" + hex.EncodeToString(b[:])
}

// TestAnchor_ObjectLockRefusesDeletingAPublishedVersion is the WORM
// assertion this package's tests were missing entirely (2026-09-02 audit,
// F-m11): the bucket had Object Lock "enabled" with no retention, so nothing
// was actually protected and the suite proved nothing about immutability.
//
// It also documents the guarantee's real boundary, which the audit's M-4
// hinges on: retention stops deletion of a specific VERSION, and does NOT
// stop a plain DELETE from writing a delete marker that hides the object
// from a listing. That is why docs/RUNBOOK.md's ledger-side credential must
// not carry DeleteObject at all -- and why service.VerifyLedger records
// every anchor observation (migration 018) instead of trusting the carrier
// to be un-erasable.
func TestAnchor_ObjectLockRefusesDeletingAPublishedVersion(t *testing.T) {
	endpoint, accessKey, secret := wormServer(t)
	raw := newRawClient(endpoint, accessKey, secret)
	ensureLockedBucket(t, raw)
	prefix := wormPrefix(t)

	a, err := r2.New(r2.Config{
		Endpoint:        endpoint,
		AccessKeyID:     accessKey,
		SecretAccessKey: secret,
		Bucket:          wormBucket,
		Key:             prefix,
		Region:          "us-east-1",
	})
	if err != nil {
		t.Fatalf("new anchor: %v", err)
	}
	ctx := context.Background()
	head1 := []byte("11111111111111111111111111111111")
	if err := a.Publish(ctx, 1, head1); err != nil {
		t.Fatalf("Publish(1, ...): %v", err)
	}

	key := fmt.Sprintf("%s/seq-%020d.json", prefix, 1)
	got, err := raw.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(wormBucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("head object: %v", err)
	}
	versionID := aws.ToString(got.VersionId)
	if versionID == "" {
		t.Fatal("the bucket must be versioned for Object Lock to mean anything; got no version id")
	}
	if got.ObjectLockMode != types.ObjectLockModeCompliance {
		t.Fatalf("published object lock mode = %q, want COMPLIANCE -- the default retention is not being applied, "+
			"so this test would be testing plain S3 semantics", got.ObjectLockMode)
	}

	if _, err := raw.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    aws.String(wormBucket),
		Key:       aws.String(key),
		VersionId: aws.String(versionID),
	}); err == nil {
		t.Fatal("deleting a retained version of a published seq: expected the lock to refuse it, got nil")
	}

	// The published head is still readable and unchanged.
	assertHead(t, a, 1, head1)
}
