// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cockroachdb

import (
	"context"
	"os"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	cockroachdbhelper "github.com/openbao/openbao/v2/internal/helper/testhelpers/cockroachdb"
)

func TestCockroachDBBackend(t *testing.T) {
	cleanup, connURL := cockroachdbhelper.PrepareTestContainer(t)
	defer cleanup()

	hae := os.Getenv("CR_HA_ENABLED")
	if hae == "" {
		hae = "true"
	}

	logger := logging.NewVaultLogger(log.Debug)

	b1, err := NewCockroachDBBackend(map[string]string{
		"connection_url": connURL,
		"table":          "openbao_kv_store",
		"ha_table":       "openbao_ha_locks",
		"ha_enabled":     hae,
	}, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	b2, err := NewCockroachDBBackend(map[string]string{
		"connection_url": connURL,
		"table":          "openbao_kv_store",
		"ha_table":       "openbao_ha_locks",
		"ha_enabled":     hae,
	}, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	defer func() {
		truncate(t, b1)
		truncate(t, b2)
	}()

	physical.ExerciseBackend(t, b1)
	truncate(t, b1)
	physical.ExerciseBackend_ListPrefix(t, b1)
	truncate(t, b1)
	testTransactionBasics(t, b1.(physical.TransactionalBackend))
	truncate(t, b1)

	ha1, ok1 := b1.(physical.HABackend)
	ha2, ok2 := b2.(physical.HABackend)
	if !ok1 || !ok2 {
		t.Fatalf("CockroachDB does not implement HABackend")
	}

	if ha1.HAEnabled() && ha2.HAEnabled() {
		logger.Info("Running ha backend tests")
		physical.ExerciseHABackend(t, ha1, ha2)
	}
}

// testTransactionBasics covers single-threaded commit/rollback semantics
// only. It intentionally does not run the shared
// physical.ExerciseTransactionalBackend suite: that suite's concurrent
// exclusive-writer stress test assumes commits never fail, which does not
// hold for CockroachDB -- writes from many concurrent transactions land in
// the same secondary index range (every key sharing a top-level "folder"
// touches the same parent_path index entries), and CockroachDB's
// SERIALIZABLE-only isolation surfaces that as retryable "transaction
// retry" errors (SQLSTATE 40001) from Commit, which this backend
// deliberately does not retry internally (see the comment on
// CockroachDBBackendTransaction in transaction.go).
func testTransactionBasics(t *testing.T, b physical.TransactionalBackend) {
	ctx := context.Background()

	// Committing or rolling back an empty transaction should succeed;
	// doing so twice should fail the second time.
	txn, err := b.BeginTx(ctx)
	require.NoError(t, err, "failed to begin read-write transaction")
	require.NoError(t, txn.Commit(ctx), "failed to commit transaction with no entries")
	require.Error(t, txn.Commit(ctx), "expected double commit of transaction to fail")

	txn, err = b.BeginReadOnlyTx(ctx)
	require.NoError(t, err, "failed to begin read-only transaction")
	require.NoError(t, txn.Commit(ctx), "failed to commit read-only transaction with no entries")
	require.Error(t, txn.Commit(ctx), "expected double commit of read-only transaction to fail")

	txn, err = b.BeginTx(ctx)
	require.NoError(t, err, "failed to begin second read-write transaction")
	require.NoError(t, txn.Rollback(ctx), "failed to rollback transaction with no entries")
	require.Error(t, txn.Rollback(ctx), "expected double rollback of transaction to fail")

	// Read-only transactions should reject writes.
	txn, err = b.BeginReadOnlyTx(ctx)
	require.NoError(t, err, "failed to begin read-only transaction")
	err = txn.Put(ctx, &physical.Entry{Key: "foo", Value: []byte("foo")})
	require.ErrorIs(t, err, physical.ErrTransactionReadOnly)
	require.NoError(t, txn.Rollback(ctx))

	// Writes issued before reads should commit OK, and be visible within
	// the same transaction.
	foo := &physical.Entry{Key: "foo", Value: []byte("foo")}
	txn, err = b.BeginTx(ctx)
	require.NoError(t, err, "failed to start new transaction")
	require.NoError(t, txn.Put(ctx, foo), "failed to write entry")
	entry, err := txn.Get(ctx, "foo")
	require.NoError(t, err, "failed to read entry")
	require.NotNil(t, entry, "expected to get a non-empty entry")
	require.Equal(t, "foo", string(entry.Value), "expected written value")
	entries, err := txn.List(ctx, "")
	require.NoError(t, err, "failed to list entries")
	require.Equal(t, []string{"foo"}, entries, "expected one entry in storage")
	require.NoError(t, txn.Commit(ctx), "failed to commit transaction")

	// Reads issued before writes should also commit OK.
	txn, err = b.BeginTx(ctx)
	require.NoError(t, err, "failed to start new transaction")
	entries, err = txn.List(ctx, "")
	require.NoError(t, err, "failed to list entries")
	require.Equal(t, []string{"foo"}, entries, "expected one entry in storage")
	entry, err = txn.Get(ctx, "foo")
	require.NoError(t, err, "failed to read entry")
	require.NotNil(t, entry, "expected to get a non-empty entry")
	require.Equal(t, "foo", string(entry.Value), "expected written value")
	require.NoError(t, txn.Delete(ctx, "foo"), "failed to delete entry")
	require.NoError(t, txn.Commit(ctx), "failed to commit transaction")

	// A rolled-back write should not be visible afterward.
	txn, err = b.BeginTx(ctx)
	require.NoError(t, err, "failed to start new transaction")
	require.NoError(t, txn.Put(ctx, &physical.Entry{Key: "bar", Value: []byte("bar")}))
	require.NoError(t, txn.Rollback(ctx))
	entry, err = b.Get(ctx, "bar")
	require.NoError(t, err)
	require.Nil(t, entry, "expected rolled-back write not to be visible")

	// Ensure we left it as we found it.
	entries, err = b.List(ctx, "")
	require.NoError(t, err, "failed to list storage entries")
	require.Empty(t, entries, "expected nothing in storage")
}

func truncate(t *testing.T, b physical.Backend) {
	t.Helper()

	crdb := b.(*CockroachDBBackend)
	if _, err := crdb.client.Exec("TRUNCATE TABLE " + crdb.table); err != nil {
		t.Fatalf("Failed to truncate table: %v", err)
	}
	if crdb.haEnabled {
		if _, err := crdb.client.Exec("TRUNCATE TABLE " + crdb.haTable); err != nil {
			t.Fatalf("Failed to truncate ha table: %v", err)
		}
	}
}
