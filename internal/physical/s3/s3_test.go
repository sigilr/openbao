// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package s3

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	s3helper "github.com/openbao/openbao/v2/internal/helper/testhelpers/s3"
)

// TestS3Backend exercises the backend against a local LocalStack S3
// instance (or a real bucket, if AWS_S3_ENDPOINT/AWS credentials are
// provided in the environment).
func TestS3Backend(t *testing.T) {
	cleanup, svccfg := s3helper.PrepareTestContainer(t)
	defer cleanup()

	ctx := context.Background()
	region := "us-east-1"

	awsConf, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(svccfg.AccessKey, svccfg.SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	conn := s3.NewFromConfig(awsConf, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(svccfg.Endpoint)
	})

	randInt := rand.New(rand.NewSource(time.Now().UnixNano())).Int()
	bucket := fmt.Sprintf("openbao-s3-testacc-%d", randInt)

	if _, err := conn.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("unable to create test bucket: %s", err)
	}
	defer func() {
		// Gotta list all the objects and delete them before being able to
		// delete the bucket.
		listResp, _ := conn.ListObjects(ctx, &s3.ListObjectsInput{Bucket: aws.String(bucket)})

		var objects types.Delete
		for _, key := range listResp.Contents {
			objects.Objects = append(objects.Objects, types.ObjectIdentifier{Key: key.Key})
		}
		if len(objects.Objects) > 0 {
			conn.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &objects}) //nolint:errcheck
		}

		if _, err := conn.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("err: %s", err)
		}
	}()

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewS3Backend(map[string]string{
		"bucket":              bucket,
		"path":                "test/openbao",
		"access_key":          svccfg.AccessKey,
		"secret_key":          svccfg.SecretKey,
		"region":              region,
		"endpoint":            svccfg.Endpoint,
		"s3_force_path_style": "true",
	}, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}

// TestS3BackendSseKms exercises SSE-KMS support against real AWS S3 and
// KMS; LocalStack's community edition does not implement KMS, so unlike
// TestS3Backend this remains an acceptance test gated on BAO_ACC and real
// AWS credentials, matching how it worked before this backend was removed.
func TestS3BackendSseKms(t *testing.T) {
	if os.Getenv("BAO_ACC") == "" {
		t.Skip("skipping as this test requires BAO_ACC and real AWS credentials/KMS access")
	}

	logger := logging.NewVaultLogger(log.Debug)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	awsConf, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := awsConf.Credentials.Retrieve(ctx); err != nil {
		t.Skip("Skipping because AWS credentials could not be resolved. See https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configuring-sdk.html for information on how to set up AWS credentials.")
	}

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-east-1"
	}
	// If empty, the default AWS endpoints will be used.
	endpoint := os.Getenv("AWS_S3_ENDPOINT")

	conn := s3.NewFromConfig(awsConf, func(o *s3.Options) {
		o.Region = region
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})

	randInt := rand.New(rand.NewSource(time.Now().UnixNano())).Int()
	bucket := fmt.Sprintf("openbao-s3-testacc-%d", randInt)

	if _, err := conn.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("unable to create test bucket: %s", err)
	}
	defer func() {
		listResp, _ := conn.ListObjects(context.Background(), &s3.ListObjectsInput{Bucket: aws.String(bucket)})

		var objects types.Delete
		for _, key := range listResp.Contents {
			objects.Objects = append(objects.Objects, types.ObjectIdentifier{Key: key.Key})
		}
		if len(objects.Objects) > 0 {
			conn.DeleteObjects(context.Background(), &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &objects}) //nolint:errcheck
		}

		if _, err := conn.DeleteBucket(context.Background(), &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("err: %s", err)
		}
	}()

	b, err := NewS3Backend(map[string]string{
		"bucket":     bucket,
		"kms_key_id": "alias/aws/s3",
		"path":       "test/openbao",
	}, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}
