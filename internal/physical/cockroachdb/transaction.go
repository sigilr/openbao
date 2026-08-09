// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cockroachdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/openbao/openbao/sdk/v2/physical"
)

// CockroachDBBackendTransaction wraps a single underlying *sql.Tx.
//
// Unlike PostgreSQL, CockroachDB only ever runs at SERIALIZABLE isolation:
// under heavy write contention against overlapping key ranges (including a
// shared secondary index range, which every key under the same top-level
// "folder" touches), Commit can fail with a retryable "transaction retry"
// error (SQLSTATE 40001) that PostgreSQL's weaker default isolation would
// not have surfaced. This type deliberately does not retry internally --
// per https://www.cockroachlabs.com/docs/stable/transaction-retry-error-reference,
// CockroachDB expects the retry to replay the full set of statements that
// were issued against the failed transaction, which the interactive
// BeginTx/Put/Get/.../Commit contract used here has no way to safely redo
// on the backend's own initiative (the caller has already observed
// whatever Get/List results the failed attempt returned). Callers that
// need resilience against transient conflicts should retry the whole
// BeginTx..Commit sequence when Commit returns physical.ErrTransactionCommitFailure.
type CockroachDBBackendTransaction struct {
	l  sync.Mutex
	b  *CockroachDBBackend
	tx *sql.Tx

	readOnly       bool
	haveWritten    bool
	haveFinishedTx bool
}

func (c *CockroachDBBackend) newTransaction(ctx context.Context, readOnly bool) (physical.Transaction, error) {
	// Grab a transaction permit pool entry so that we can limit the number of
	// concurrent transactions.
	c.txnPermitPool.Acquire()

	// CockroachDB only ever runs at SERIALIZABLE isolation; it silently
	// upgrades any weaker request, so ask for it explicitly.
	tx, err := c.client.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
		ReadOnly:  readOnly,
	})
	if err != nil {
		c.txnPermitPool.Release()
		return nil, fmt.Errorf("failed to start underlying cockroachdb transaction: %w", err)
	}

	return &CockroachDBBackendTransaction{
		b:        c,
		tx:       tx,
		readOnly: readOnly,
	}, nil
}

func (c *CockroachDBBackend) BeginTx(ctx context.Context) (physical.Transaction, error) {
	return c.newTransaction(ctx, false)
}

func (c *CockroachDBBackend) BeginReadOnlyTx(ctx context.Context) (physical.Transaction, error) {
	return c.newTransaction(ctx, true)
}

func (t *CockroachDBBackendTransaction) Put(ctx context.Context, entry *physical.Entry) error {
	t.l.Lock()
	defer t.l.Unlock()

	if t.readOnly {
		return physical.ErrTransactionReadOnly
	}
	if t.haveFinishedTx {
		return physical.ErrTransactionAlreadyCommitted
	}

	parentPath, path, key := t.b.splitKey(entry.Key)

	if _, err := t.tx.ExecContext(ctx, t.b.putQuery, parentPath, path, key, entry.Value); err != nil {
		return err
	}

	t.haveWritten = true

	return nil
}

func (t *CockroachDBBackendTransaction) Delete(ctx context.Context, fullPath string) error {
	t.l.Lock()
	defer t.l.Unlock()

	if t.readOnly {
		return physical.ErrTransactionReadOnly
	}
	if t.haveFinishedTx {
		return physical.ErrTransactionAlreadyCommitted
	}

	_, path, key := t.b.splitKey(fullPath)

	if _, err := t.tx.ExecContext(ctx, t.b.deleteQuery, path, key); err != nil {
		return err
	}

	t.haveWritten = true

	return nil
}

func (t *CockroachDBBackendTransaction) Get(ctx context.Context, fullPath string) (*physical.Entry, error) {
	t.l.Lock()
	defer t.l.Unlock()

	if t.haveFinishedTx {
		return nil, physical.ErrTransactionAlreadyCommitted
	}

	_, path, key := t.b.splitKey(fullPath)

	var result []byte
	err := t.tx.QueryRowContext(ctx, t.b.getQuery, path, key).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &physical.Entry{
		Key:   fullPath,
		Value: result,
	}, nil
}

func (t *CockroachDBBackendTransaction) List(ctx context.Context, prefix string) ([]string, error) {
	t.l.Lock()
	defer t.l.Unlock()

	if t.haveFinishedTx {
		return nil, physical.ErrTransactionAlreadyCommitted
	}

	rows, err := t.tx.QueryContext(ctx, t.b.listQuery, "/"+prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("failed to scan rows: %w", err)
		}
		keys = append(keys, key)
	}

	return keys, nil
}

func (t *CockroachDBBackendTransaction) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	t.l.Lock()
	defer t.l.Unlock()

	if t.haveFinishedTx {
		return nil, physical.ErrTransactionAlreadyCommitted
	}

	var rows *sql.Rows
	var err error
	if limit <= 0 {
		rows, err = t.tx.QueryContext(ctx, t.b.listPageQuery, "/"+prefix, after)
	} else {
		rows, err = t.tx.QueryContext(ctx, t.b.listPageLimitedQuery, "/"+prefix, after, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("failed to scan rows: %w", err)
		}
		keys = append(keys, key)
	}

	return keys, nil
}

func (t *CockroachDBBackendTransaction) Commit(ctx context.Context) error {
	if t.readOnly || !t.haveWritten {
		return t.Rollback(ctx)
	}

	t.l.Lock()
	defer t.l.Unlock()

	if t.haveFinishedTx {
		return physical.ErrTransactionAlreadyCommitted
	}

	defer func() {
		t.b.txnPermitPool.Release()
		t.haveFinishedTx = true
	}()

	if err := t.b.validateFence(ctx); err != nil {
		return err
	}

	if err := t.tx.Commit(); err != nil {
		return fmt.Errorf("%v: %w", err, physical.ErrTransactionCommitFailure)
	}

	return nil
}

func (t *CockroachDBBackendTransaction) Rollback(ctx context.Context) error {
	t.l.Lock()
	defer t.l.Unlock()

	if t.haveFinishedTx {
		return physical.ErrTransactionAlreadyCommitted
	}

	defer func() {
		t.b.txnPermitPool.Release()
		t.haveFinishedTx = true
	}()

	return t.tx.Rollback()
}
