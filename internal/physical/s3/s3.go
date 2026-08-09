// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/parseutil"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// Verify S3Backend satisfies the correct interfaces
var _ physical.Backend = (*S3Backend)(nil)

// S3Backend is a physical backend that stores data
// within an S3 bucket.
type S3Backend struct {
	bucket     string
	path       string
	kmsKeyId   string
	client     *s3.Client
	logger     log.Logger
	permitPool *physical.PermitPool
}

// NewS3Backend constructs an S3 backend using a pre-existing
// bucket. Credentials can be provided to the backend, sourced
// from the environment, AWS credential files, or by IAM role.
func NewS3Backend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		bucket = conf["bucket"]
		if bucket == "" {
			return nil, fmt.Errorf("'bucket' must be set")
		}
	}

	pathPrefix := conf["path"]

	accessKey := conf["access_key"]
	secretKey := conf["secret_key"]
	sessionToken := conf["session_token"]

	endpoint := os.Getenv("AWS_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = conf["endpoint"]
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
		if region == "" {
			region = conf["region"]
			if region == "" {
				region = "us-east-1"
			}
		}
	}
	s3ForcePathStyleStr, ok := conf["s3_force_path_style"]
	if !ok {
		s3ForcePathStyleStr = "false"
	}
	s3ForcePathStyleBool, err := parseutil.ParseBool(s3ForcePathStyleStr)
	if err != nil {
		return nil, fmt.Errorf("invalid boolean set for s3_force_path_style: %q", s3ForcePathStyleStr)
	}
	disableSSLStr, ok := conf["disable_ssl"]
	if !ok {
		disableSSLStr = "false"
	}
	disableSSLBool, err := parseutil.ParseBool(disableSSLStr)
	if err != nil {
		return nil, fmt.Errorf("invalid boolean set for disable_ssl: %q", disableSSLStr)
	}

	ctx := context.Background()

	pooledTransport := cleanhttp.DefaultPooledTransport()
	pooledTransport.MaxIdleConnsPerHost = consts.ExpirationRestoreWorkerCount

	loadOpts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithHTTPClient(&http.Client{Transport: pooledTransport}),
	}
	if accessKey != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken),
		))
	}

	awsConf, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(awsConf, func(o *s3.Options) {
		o.UsePathStyle = s3ForcePathStyleBool
		if endpoint != "" {
			// BaseEndpoint requires a scheme; disable_ssl only makes sense
			// (and is only consulted) against a custom, non-AWS endpoint.
			if !strings.Contains(endpoint, "://") {
				if disableSSLBool {
					endpoint = "http://" + endpoint
				} else {
					endpoint = "https://" + endpoint
				}
			}
			o.BaseEndpoint = aws.String(endpoint)
		}
	})

	if _, err := client.ListObjects(ctx, &s3.ListObjectsInput{Bucket: aws.String(bucket)}); err != nil {
		return nil, fmt.Errorf("unable to access bucket %q in region %q: %w", bucket, region, err)
	}

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		logger.Debug("max_parallel set", "max_parallel", maxParInt)
	}

	s := &S3Backend{
		client:     client,
		bucket:     bucket,
		path:       pathPrefix,
		kmsKeyId:   conf["kms_key_id"],
		logger:     logger,
		permitPool: physical.NewPermitPool(maxParInt),
	}
	return s, nil
}

// Put is used to insert or update an entry
func (s *S3Backend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"s3", "put"}, time.Now())

	s.permitPool.Acquire()
	defer s.permitPool.Release()

	key := path.Join(s.path, entry.Key)

	putObjectInput := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(entry.Value),
	}

	if s.kmsKeyId != "" {
		putObjectInput.ServerSideEncryption = "aws:kms"
		putObjectInput.SSEKMSKeyId = aws.String(s.kmsKeyId)
	}

	_, err := s.client.PutObject(ctx, putObjectInput)
	return err
}

// Get is used to fetch an entry
func (s *S3Backend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"s3", "get"}, time.Now())

	s.permitPool.Acquire()
	defer s.permitPool.Release()

	fullKey := path.Join(s.path, key)

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(fullKey),
	})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close() //nolint:errcheck
	}
	if err != nil {
		var respErr *smithyhttp.ResponseError
		if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
			// Return nil on 404s, error on anything else
			return nil, nil
		}
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("got nil response from S3 but no error")
	}

	data := bytes.NewBuffer(nil)
	if resp.ContentLength != nil {
		data = bytes.NewBuffer(make([]byte, 0, *resp.ContentLength))
	}
	if _, err := io.Copy(data, resp.Body); err != nil {
		return nil, err
	}

	// Strip path prefix
	if s.path != "" {
		key = strings.TrimPrefix(fullKey, s.path+"/")
	}

	return &physical.Entry{
		Key:   key,
		Value: data.Bytes(),
	}, nil
}

// Delete is used to permanently delete an entry
func (s *S3Backend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"s3", "delete"}, time.Now())

	s.permitPool.Acquire()
	defer s.permitPool.Release()

	key = path.Join(s.path, key)

	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

// listAll returns the sorted set of keys under a given prefix, up to the
// next prefix; S3's Delimiter="/" listing mode folds "folders" for us
// server-side, unlike the KV-style backends elsewhere in this package.
// listAll is shared by List and ListPage.
func (s *S3Backend) listAll(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"s3", "list"}, time.Now())

	s.permitPool.Acquire()
	defer s.permitPool.Release()

	fullPrefix := path.Join(s.path, prefix)

	// Validate prefix (if present) is ending with a "/"
	if fullPrefix != "" && !strings.HasSuffix(fullPrefix, "/") {
		fullPrefix += "/"
	}

	params := &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(fullPrefix),
		Delimiter: aws.String("/"),
	}

	keys := []string{}

	paginator := s3.NewListObjectsV2Paginator(s.client, params)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		// Add truncated 'folder' paths
		for _, commonPrefix := range page.CommonPrefixes {
			if commonPrefix.Prefix == nil {
				continue
			}
			keys = append(keys, strings.TrimPrefix(*commonPrefix.Prefix, fullPrefix))
		}
		// Add objects only from the current 'folder'
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			keys = append(keys, strings.TrimPrefix(*obj.Key, fullPrefix))
		}
	}

	sort.Strings(keys)

	return keys, nil
}

// List is used to list all the keys under a given prefix, up to the next
// prefix.
func (s *S3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	return s.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, starting
// strictly after the "after" key, up to "limit" keys.
func (s *S3Backend) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	keys, err := s.listAll(ctx, prefix)
	if err != nil {
		return nil, err
	}

	start := sort.SearchStrings(keys, after)
	for start < len(keys) && keys[start] <= after {
		start++
	}

	end := len(keys)
	if limit > 0 && start+limit < end {
		end = start + limit
	}

	return keys[start:end], nil
}
