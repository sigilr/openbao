// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package gcs

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	log "github.com/hashicorp/go-hclog"
	multierror "github.com/hashicorp/go-multierror"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/v2/internal/helper/useragent"
)

// Verify Backend satisfies the correct interfaces
var _ physical.Backend = (*Backend)(nil)

const (
	// envBucket is the name of the environment variable to search for the
	// storage bucket name.
	envBucket = "GOOGLE_STORAGE_BUCKET"

	// envChunkSize is the environment variable to search for the chunk size for
	// requests.
	envChunkSize = "GOOGLE_STORAGE_CHUNK_SIZE"

	// envHAEnabled is the name of the environment variable to search for the
	// boolean indicating if HA is enabled.
	envHAEnabled = "GOOGLE_STORAGE_HA_ENABLED"

	// defaultChunkSize is the number of bytes the writer will attempt to write in
	// a single request.
	defaultChunkSize = "8192"

	// objectDelimiter is the string to use to delimit objects.
	objectDelimiter = "/"
)

var (
	// metricDelete is the key for the metric for measuring a Delete call.
	metricDelete = []string{"gcs", "delete"}

	// metricGet is the key for the metric for measuring a Get call.
	metricGet = []string{"gcs", "get"}

	// metricList is the key for the metric for measuring a List call.
	metricList = []string{"gcs", "list"}

	// metricPut is the key for the metric for measuring a Put call.
	metricPut = []string{"gcs", "put"}
)

// Backend implements physical.Backend and describes the steps necessary to
// persist data in Google Cloud Storage.
type Backend struct {
	// bucket is the name of the bucket to use for data storage and retrieval.
	bucket string

	// chunkSize is the chunk size to use for requests.
	chunkSize int

	// client is the API client and permitPool is the allowed concurrent uses of
	// the client.
	client     *storage.Client
	permitPool *physical.PermitPool

	// haEnabled indicates if HA is enabled.
	haEnabled bool

	// haClient is the API client. This is managed separately from the main client
	// because a flood of requests should not block refreshing the TTLs on the
	// lock.
	//
	// This value will be nil if haEnabled is false.
	haClient *storage.Client

	// logger is an internal logger.
	logger log.Logger
}

// NewBackend constructs a Google Cloud Storage backend with the given
// configuration. This uses the official Golang Cloud SDK and therefore supports
// specifying credentials via envvars, credential files, etc. from environment
// variables or a service account file.
//
// Clients are constructed with storage.WithJSONReads: the client's default
// read path (XML) is slated to become JSON in a future SDK release anyway,
// and reads are otherwise the one operation here that doesn't already use
// the JSON API.
func NewBackend(c map[string]string, logger log.Logger) (physical.Backend, error) {
	logger.Debug("configuring backend")

	// Bucket name
	bucket := os.Getenv(envBucket)
	if bucket == "" {
		bucket = c["bucket"]
	}
	if bucket == "" {
		return nil, errors.New("missing bucket name")
	}

	// Chunk size
	chunkSizeStr := os.Getenv(envChunkSize)
	if chunkSizeStr == "" {
		chunkSizeStr = c["chunk_size"]
	}
	if chunkSizeStr == "" {
		chunkSizeStr = defaultChunkSize
	}
	chunkSize, err := strconv.Atoi(chunkSizeStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse chunk_size: %w", err)
	}

	// Values are specified as kb, but the API expects them as bytes.
	chunkSize *= 1024

	// HA configuration
	haClient := (*storage.Client)(nil)
	haEnabled := false
	haEnabledStr := os.Getenv(envHAEnabled)
	if haEnabledStr == "" {
		haEnabledStr = c["ha_enabled"]
	}
	if haEnabledStr != "" {
		haEnabled, err = strconv.ParseBool(haEnabledStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse HA enabled: %w", err)
		}
	}
	if haEnabled {
		logger.Debug("creating client")
		ctx := context.Background()
		haClient, err = storage.NewClient(ctx, option.WithUserAgent(useragent.String()), storage.WithJSONReads())
		if err != nil {
			return nil, fmt.Errorf("failed to create HA storage client: %w", err)
		}
	}

	// Max parallel
	maxParallel, err := extractInt(c["max_parallel"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse max_parallel: %w", err)
	}

	logger.Debug(
		"configuration",
		"bucket", bucket,
		"chunk_size", chunkSize,
		"ha_enabled", haEnabled,
		"max_parallel", maxParallel,
	)

	logger.Debug("creating client")
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithUserAgent(useragent.String()), storage.WithJSONReads())
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}

	return &Backend{
		bucket:     bucket,
		chunkSize:  chunkSize,
		client:     client,
		permitPool: physical.NewPermitPool(maxParallel),

		haEnabled: haEnabled,
		haClient:  haClient,

		logger: logger,
	}, nil
}

// Put is used to insert or update an entry
func (b *Backend) Put(ctx context.Context, entry *physical.Entry) (retErr error) {
	defer metrics.MeasureSince(metricPut, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	// Insert
	w := b.client.Bucket(b.bucket).Object(entry.Key).NewWriter(ctx)
	w.ChunkSize = b.chunkSize
	md5Array := md5.Sum(entry.Value)
	w.MD5 = md5Array[:]
	defer func() {
		if closeErr := w.Close(); closeErr != nil {
			retErr = multierror.Append(retErr, fmt.Errorf("error closing connection: %w", closeErr))
		}
	}()

	if _, err := w.Write(entry.Value); err != nil {
		return fmt.Errorf("failed to put data: %w", err)
	}
	return nil
}

// Get fetches an entry. If no entry exists, this function returns nil.
func (b *Backend) Get(ctx context.Context, key string) (retEntry *physical.Entry, retErr error) {
	defer metrics.MeasureSince(metricGet, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	// Read
	r, err := b.client.Bucket(b.bucket).Object(key).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read value for %q: %w", key, err)
	}

	defer func() {
		if closeErr := r.Close(); closeErr != nil {
			retErr = multierror.Append(retErr, fmt.Errorf("error closing connection: %w", closeErr))
		}
	}()

	value, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read value into a string: %w", err)
	}

	return &physical.Entry{
		Key:   key,
		Value: value,
	}, nil
}

// Delete deletes an entry with the given key
func (b *Backend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince(metricDelete, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	// Delete
	err := b.client.Bucket(b.bucket).Object(key).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("failed to delete key %q: %w", key, err)
	}
	return nil
}

// listAll returns the sorted set of keys under a given prefix, up to the
// next prefix; GCS's Delimiter listing mode folds "folders" for us
// server-side, and returns results in lexicographic order. Shared by List
// and ListPage.
func (b *Backend) listAll(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince(metricList, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	iter := b.client.Bucket(b.bucket).Objects(ctx, &storage.Query{
		Prefix:    prefix,
		Delimiter: objectDelimiter,
		Versions:  false,
	})

	keys := []string{}

	for {
		objAttrs, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read object: %w", err)
		}

		var path string
		if objAttrs.Prefix != "" {
			// "subdirectory"
			path = objAttrs.Prefix
		} else {
			// file
			path = objAttrs.Name
		}

		// get relative file/dir just like "basename"
		key := strings.TrimPrefix(path, prefix)
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys, nil
}

// List is used to list all the keys under a given prefix, up to the next
// prefix.
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	return b.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, starting
// strictly after the "after" key, up to "limit" keys.
func (b *Backend) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	keys, err := b.listAll(ctx, prefix)
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

// extractInt is a helper function that takes a string and converts that string
// to an int, but accounts for the empty string.
func extractInt(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}
