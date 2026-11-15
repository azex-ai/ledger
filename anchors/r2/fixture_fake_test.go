//go:build !s3server

package r2_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/azex-ai/ledger/anchors/r2/internal/s3test"
)

// testBucket, by default, is an empty bucket on a fresh in-process fake S3
// server (internal/s3test): no container, no external service. The same
// tests run against a real S3-compatible server with `-tags s3server`
// (fixture_server_test.go, `make test-r2-s3server`). The fake has no Object
// Lock; the one test that needs it exists only under that tag.
func testBucket(t *testing.T) (endpoint, accessKey, secret, bucket string) {
	t.Helper()
	endpoint, accessKey, secret = s3test.Fixture(t)
	bucket = "ledger-anchor-test"
	client := newRawClient(endpoint, accessKey, secret)
	if _, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return endpoint, accessKey, secret, bucket
}
