// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package spanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/spanner"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/v2/internal/helper/useragent"
)

// Verify Backend satisfies the correct interfaces
//
// Unlike postgresql/cockroachdb, this backend does not implement
// physical.TransactionalBackend. The old one-shot batch Transaction API it
// used to implement no longer exists; the current interactive
// BeginTx/Put/Get/.../Commit contract expects a handle callers can operate
// on incrementally, but Cloud Spanner's read-write transaction API
// (client.ReadWriteTransaction) is closure-based -- the whole set of reads
// and writes runs inside one function the SDK invokes and internally
// retries on ABORTED. Bridging that into an interactive handle would need
// a goroutine relaying each call across a channel to the closure, which is
// exactly the kind of complexity that, applied to CockroachDB's commit-
// retry loop earlier in this series, caused a real connection-pool
// deadlock under concurrent load; that isn't a risk worth taking on here
// for a capability the original backend never had either.
var _ physical.Backend = (*Backend)(nil)

const (
	// envDatabase is the name of the environment variable to search for the
	// database name.
	envDatabase = "GOOGLE_SPANNER_DATABASE"

	// envHAEnabled is the name of the environment variable to search for the
	// boolean indicating if HA is enabled.
	envHAEnabled = "GOOGLE_SPANNER_HA_ENABLED"

	// envHATable is the name of the environment variable to search for the table
	// name to use for HA.
	envHATable = "GOOGLE_SPANNER_HA_TABLE"

	// envTable is the name of the environment variable to search for the table
	// name.
	envTable = "GOOGLE_SPANNER_TABLE"

	// defaultTable is the default table name if none is specified.
	defaultTable = "Vault"

	// defaultHASuffix is the default suffix to apply to the table name if no
	// HA table is provided.
	defaultHASuffix = "HA"
)

var (
	// metricDelete is the key for the metric for measuring a Delete call.
	metricDelete = []string{"spanner", "delete"}

	// metricGet is the key for the metric for measuring a Get call.
	metricGet = []string{"spanner", "get"}

	// metricList is the key for the metric for measuring a List call.
	metricList = []string{"spanner", "list"}

	// metricPut is the key for the metric for measuring a Put call.
	metricPut = []string{"spanner", "put"}
)

// Backend implements physical.Backend and describes the steps necessary to
// persist data using Google Cloud Spanner.
type Backend struct {
	// database is the name of the database to use for data storage and retrieval.
	// This is supplied as part of user configuration.
	database string

	// table is the name of the table in the database.
	table string

	// client is the API client and permitPool is the allowed concurrent uses of
	// the client.
	client     *spanner.Client
	permitPool *physical.PermitPool

	// haTable is the name of the table to use for HA in the database.
	haTable string

	// haEnabled indicates if high availability is enabled. Default: true.
	haEnabled bool

	// haClient is the API client. This is managed separately from the main client
	// because a flood of requests should not block refreshing the TTLs on the
	// lock.
	//
	// This value will be nil if haEnabled is false.
	haClient *spanner.Client

	// logger is the internal logger.
	logger log.Logger
}

// NewBackend creates a new Google Spanner storage backend with the given
// configuration. This uses the official Golang Cloud SDK and therefore supports
// specifying credentials via envvars, credential files, etc.
func NewBackend(c map[string]string, logger log.Logger) (physical.Backend, error) {
	logger.Debug("configuring backend")

	// Database name
	database := os.Getenv(envDatabase)
	if database == "" {
		database = c["database"]
	}
	if database == "" {
		return nil, errors.New("missing database name")
	}

	// Table name
	table := os.Getenv(envTable)
	if table == "" {
		table = c["table"]
	}
	if table == "" {
		table = defaultTable
	}

	// HA table name
	haTable := os.Getenv(envHATable)
	if haTable == "" {
		haTable = c["ha_table"]
	}
	if haTable == "" {
		haTable = table + defaultHASuffix
	}

	// HA configuration
	haClient := (*spanner.Client)(nil)
	haEnabled := false
	haEnabledStr := os.Getenv(envHAEnabled)
	if haEnabledStr == "" {
		haEnabledStr = c["ha_enabled"]
	}
	if haEnabledStr != "" {
		var err error
		haEnabled, err = strconv.ParseBool(haEnabledStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse HA enabled: %w", err)
		}
	}
	if haEnabled {
		logger.Debug("creating HA client")
		ctx := context.Background()
		var err error
		haClient, err = spanner.NewClient(
			ctx, database,
			option.WithUserAgent(useragent.String()),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create HA client: %w", err)
		}
	}

	// Max parallel
	maxParallel, err := extractInt(c["max_parallel"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse max_parallel: %w", err)
	}

	logger.Debug(
		"configuration",
		"database", database,
		"table", table,
		"haEnabled", haEnabled,
		"haTable", haTable,
		"maxParallel", maxParallel,
	)

	logger.Debug("creating client")
	ctx := context.Background()
	client, err := spanner.NewClient(
		ctx, database,
		option.WithUserAgent(useragent.String()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create spanner client: %w", err)
	}

	return &Backend{
		database:   database,
		table:      table,
		client:     client,
		permitPool: physical.NewPermitPool(maxParallel),

		haEnabled: haEnabled,
		haTable:   haTable,
		haClient:  haClient,

		logger: logger,
	}, nil
}

// Put creates or updates an entry.
func (b *Backend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince(metricPut, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	// Insert
	m := spanner.InsertOrUpdateMap(b.table, map[string]any{
		"Key":   entry.Key,
		"Value": entry.Value,
	})
	if _, err := b.client.Apply(ctx, []*spanner.Mutation{m}); err != nil {
		return fmt.Errorf("failed to put data: %w", err)
	}
	return nil
}

// Get fetches an entry. If there is no entry, this function returns nil.
func (b *Backend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince(metricGet, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	// Read
	row, err := b.client.Single().ReadRow(ctx, b.table, spanner.Key{key}, []string{"Value"})
	if spanner.ErrCode(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read value for %q: %w", key, err)
	}

	var value []byte
	if err := row.Column(0, &value); err != nil {
		return nil, fmt.Errorf("failed to decode value into bytes: %w", err)
	}

	return &physical.Entry{
		Key:   key,
		Value: value,
	}, nil
}

// Delete deletes an entry with the given key.
func (b *Backend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince(metricDelete, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	// Delete
	m := spanner.Delete(b.table, spanner.Key{key})
	if _, err := b.client.Apply(ctx, []*spanner.Mutation{m}); err != nil {
		return fmt.Errorf("failed to delete key: %w", err)
	}

	return nil
}

// listAll enumerates all keys with the given prefix, sorted (Spanner's
// result order for this query is not otherwise predictable). Shared by
// List and ListPage.
func (b *Backend) listAll(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince(metricList, time.Now())

	// Pooling
	b.permitPool.Acquire()
	defer b.permitPool.Release()

	// Sanitize
	safeTable := sanitizeTable(b.table)

	// List
	iter := b.client.Single().Query(ctx, spanner.Statement{
		SQL: "SELECT Key FROM " + safeTable + " WHERE STARTS_WITH(Key, @prefix)",
		Params: map[string]any{
			"prefix": prefix,
		},
	})
	defer iter.Stop()

	var keys []string

	for {
		row, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read row: %w", err)
		}

		var key string
		if err := row.Column(0, &key); err != nil {
			return nil, fmt.Errorf("failed to decode key into string: %w", err)
		}

		// The results will include the full prefix (folder) and any deeply-nested
		// prefixes (subfolders). OpenBao expects only the top-most things to be
		// included.
		key = strings.TrimPrefix(key, prefix)
		if i := strings.Index(key, "/"); i == -1 {
			// Add objects only from the current 'folder'
			keys = append(keys, key)
		} else {
			// Add truncated 'folder' paths
			keys = strutil.AppendIfMissing(keys, key[:i+1])
		}
	}

	// Sort because the resulting order is not predictable
	sort.Strings(keys)

	return keys, nil
}

// List enumerates all keys with the given prefix.
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

// sanitizeTable attempts to sanitize the table name.
func sanitizeTable(s string) string {
	end := strings.IndexRune(s, 0)
	if end > -1 {
		s = s[:end]
	}
	return strings.ReplaceAll(s, `"`, `""`)
}
