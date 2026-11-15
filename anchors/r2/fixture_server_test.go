//go:build s3server

package r2_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// With `-tags s3server` (`make test-r2-s3server`) every test in this package
// runs against a REAL S3-compatible server instead of the in-process fake,
// plus the Object Lock test (r2_objectlock_test.go) that only a real server
// can serve. The server is read from the environment, defaulting to a MinIO
// on this machine with MinIO's stock root credential:
//
//	LEDGER_TEST_S3_ENDPOINT           http://127.0.0.1:9000
//	LEDGER_TEST_S3_ACCESS_KEY_ID      minioadmin
//	LEDGER_TEST_S3_SECRET_ACCESS_KEY  minioadmin
//
// It must implement S3 versioning and Object Lock (MinIO does). Nothing is
// skipped for a missing server: built with the tag, an unreachable server
// fails the tests.
const (
	defaultS3Endpoint  = "http://127.0.0.1:9000"
	defaultS3AccessKey = "minioadmin"
	defaultS3SecretKey = "minioadmin" //nolint:gosec // MinIO's stock local root credential, test-only
)

func s3Server(t *testing.T) (endpoint, accessKey, secret string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping real-S3-server test")
	}
	get := func(name, fallback string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return fallback
	}
	return get("LEDGER_TEST_S3_ENDPOINT", defaultS3Endpoint),
		get("LEDGER_TEST_S3_ACCESS_KEY_ID", defaultS3AccessKey),
		get("LEDGER_TEST_S3_SECRET_ACCESS_KEY", defaultS3SecretKey)
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// testBucket is a new plain (unlocked, unversioned) bucket
// ledger-test-<random> on the real server; t.Cleanup empties and deletes it,
// so nothing is left behind.
func testBucket(t *testing.T) (endpoint, accessKey, secret, bucket string) {
	t.Helper()
	endpoint, accessKey, secret = s3Server(t)
	bucket = "ledger-test-" + randomSuffix(t)
	client := newRawClient(endpoint, accessKey, secret)
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket %s on the S3 server (is it running? docs/TESTING.md, `make test-r2-s3server`): %v", bucket, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		var token *string
		for {
			out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), ContinuationToken: token})
			if err != nil {
				t.Errorf("cleanup: list %s: %v", bucket, err)
				return
			}
			for _, obj := range out.Contents {
				if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: obj.Key}); err != nil {
					t.Errorf("cleanup: delete %s/%s: %v", bucket, aws.ToString(obj.Key), err)
				}
			}
			if !aws.ToBool(out.IsTruncated) {
				break
			}
			token = out.NextContinuationToken
		}
		if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("cleanup: delete bucket %s: %v", bucket, err)
		}
	})
	return endpoint, accessKey, secret, bucket
}
