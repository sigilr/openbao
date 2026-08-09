// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cockroachdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/parseutil"
	"github.com/hashicorp/go-uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/database/helper/dbutil"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	// The lock TTL matches the default that Consul's API uses, 15 seconds.
	// Used as part of SQL commands to set/extend lock expiry time relative to
	// database clock.
	CockroachDBLockTTLSeconds = 15

	// The amount of time to wait between the lock renewals
	CockroachDBLockRenewInterval = 5 * time.Second

	// CockroachDBLockRetryInterval is the amount of time to wait
	// if a lock fails before trying again.
	CockroachDBLockRetryInterval = time.Second
)

// Verify CockroachDBBackend satisfies the correct interfaces
var (
	_ physical.Backend              = (*CockroachDBBackend)(nil)
	_ physical.TransactionalBackend = (*CockroachDBBackend)(nil)
	_ physical.HABackend            = (*CockroachDBBackend)(nil)
	_ physical.Lock                 = (*CockroachDBLock)(nil)
)

// CockroachDBBackend is a physical backend that stores data within a
// CockroachDB database. CockroachDB speaks the PostgreSQL wire protocol
// and mirrors most of its SQL surface, so the schema and query shapes here
// follow internal/physical/postgresql closely -- the main differences are
// the driver connection setup and dropping an explicit COLLATE clause that
// CockroachDB's collation support doesn't recognize by the same name.
type CockroachDBBackend struct {
	table           string
	tableConstraint string
	index           string

	client *sql.DB

	putQuery             string
	getQuery             string
	deleteQuery          string
	listQuery            string
	listPageQuery        string
	listPageLimitedQuery string

	haTable                  string
	haTableConstraint        string
	haGetLockValueQuery      string
	haUpsertLockIdentityExec string
	haRenewLockIdentityExec  string
	haDeleteLockExec         string
	haCheckLockHeldQuery     string

	haEnabled     bool
	logger        log.Logger
	txnPermitPool *physical.PermitPool

	fenceLock sync.RWMutex
	fence     *CockroachDBLock
}

// CockroachDBLock implements a lock using a CockroachDB client.
type CockroachDBLock struct {
	backend    *CockroachDBBackend
	value, key string
	identity   string
	lock       sync.Mutex

	renewTicker *time.Ticker

	// ttlSeconds is how long a lock is valid for
	ttlSeconds int

	// renewInterval is how much time to wait between lock renewals. must be << ttl
	renewInterval time.Duration

	// retryInterval is how much time to wait between attempts to grab the lock
	retryInterval time.Duration
}

// NewCockroachDBBackend constructs a CockroachDB backend using the given
// API client, server address, credentials, and database.
func NewCockroachDBBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	connURL, ok := conf["connection_url"]
	if !ok || connURL == "" {
		return nil, fmt.Errorf("missing connection_url")
	}

	unquotedTable, ok := conf["table"]
	if !ok {
		unquotedTable = "openbao_kv_store"
	}
	quotedTable := dbutil.QuoteIdentifier(unquotedTable)

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	var err error
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		logger.Debug("max_parallel set", "max_parallel", maxParInt)
	} else {
		maxParInt = physical.DefaultParallelOperations
	}

	txnMaxParStr, ok := conf["transaction_max_parallel"]
	var txnMaxParInt int
	if ok {
		txnMaxParInt, err = strconv.Atoi(txnMaxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing transaction_max_parallel parameter: %w", err)
		}
		logger.Debug("transaction_max_parallel set", "transaction_max_parallel", txnMaxParInt)
	} else {
		txnMaxParInt = physical.DefaultParallelTransactions
	}

	db, err := sql.Open("pgx", connURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to cockroachdb: %w", err)
	}
	db.SetMaxOpenConns(maxParInt)

	unquotedHaTable, ok := conf["ha_table"]
	if !ok {
		unquotedHaTable = "openbao_ha_locks"
	}
	quotedHaTable := dbutil.QuoteIdentifier(unquotedHaTable)

	c := &CockroachDBBackend{
		table:           quotedTable,
		tableConstraint: dbutil.QuoteIdentifier(unquotedTable + "_pkey"),
		index:           dbutil.QuoteIdentifier(unquotedTable + "_idx"),
		client:          db,
		putQuery: "INSERT INTO " + quotedTable + " VALUES($1, $2, $3, $4)" +
			" ON CONFLICT (path, key) DO " +
			" UPDATE SET (parent_path, path, key, value) = ($1, $2, $3, $4)",
		getQuery:    "SELECT value FROM " + quotedTable + " WHERE path = $1 AND key = $2",
		deleteQuery: "DELETE FROM " + quotedTable + " WHERE path = $1 AND key = $2",
		listQuery: "SELECT key FROM " + quotedTable + " WHERE path = $1" +
			" UNION ALL SELECT DISTINCT substring(substr(path, length($1)+1) from '^.*?/') FROM " + quotedTable +
			" WHERE parent_path LIKE $1 || '%'" +
			" ORDER BY key",
		listPageQuery: "SELECT key FROM " + quotedTable + " WHERE path = $1 AND key > $2" +
			" UNION ALL SELECT DISTINCT substring(substr(path, length($1)+1) from '^.*?/') FROM " + quotedTable +
			" WHERE parent_path LIKE $1 || '%' AND substring(substr(path, length($1)+1) from '^.*?/') > $2" +
			" ORDER BY key",
		listPageLimitedQuery: "SELECT key FROM " + quotedTable + " WHERE path = $1 AND key > $2" +
			" UNION ALL SELECT DISTINCT substring(substr(path, length($1)+1) from '^.*?/') FROM " + quotedTable +
			" WHERE parent_path LIKE $1 || '%' AND substring(substr(path, length($1)+1) from '^.*?/') > $2" +
			" ORDER BY key LIMIT $3",
		haTable:           quotedHaTable,
		haTableConstraint: dbutil.QuoteIdentifier(unquotedHaTable + "_pkey"),
		haGetLockValueQuery:
		// only read non expired data
		" SELECT ha_value FROM " + quotedHaTable + " WHERE NOW() <= valid_until AND ha_key = $1 ",
		haUpsertLockIdentityExec:
		// $1=identity $2=ha_key $3=ha_value $4=TTL in seconds
		// update only steals an expired lock
		" INSERT INTO " + quotedHaTable + " as t (ha_identity, ha_key, ha_value, valid_until) VALUES ($1, $2, $3, NOW() + $4::INT * INTERVAL '1 second'  ) " +
			" ON CONFLICT (ha_key) DO " +
			" UPDATE SET (ha_identity, ha_key, ha_value, valid_until) = ($1, $2, $3, NOW() + $4::INT * INTERVAL '1 second') " +
			" WHERE (t.valid_until < NOW() AND t.ha_key = $2)",
		haRenewLockIdentityExec:
		// Same parameters as haUpsertLockIdentityExec; just for the renewal
		// flow instead of the lock creation flow.
		//
		// update only renews our lock; it will not steal it and will not
		// create it if it doesn't exist.
		" UPDATE " + quotedHaTable +
			" SET (ha_identity, ha_key, ha_value, valid_until) = ($1, $2, $3, NOW() + $4::INT * INTERVAL '1 second') " +
			" WHERE (ha_identity = $1 AND ha_key = $2)  ",
		haDeleteLockExec:
		// $1=ha_identity $2=ha_key
		" DELETE FROM " + quotedHaTable + " WHERE ha_identity=$1 AND ha_key=$2 ",
		haCheckLockHeldQuery:
		// $1=ha_identity $2=ha_key $3=ha_value
		" SELECT COUNT(*) FROM " + quotedHaTable + " WHERE " +
			" ha_identity=$1 AND ha_key=$2 AND ha_value=$3 AND valid_until > NOW()  ",
		logger:        logger,
		txnPermitPool: physical.NewPermitPool(txnMaxParInt),
		haEnabled:     conf["ha_enabled"] == "true",

		// No initial fence, but if a fence is here, we'll validate it inside
		// write transactions.
		fence: nil,
	}

	rawSkipCreateTable, ok := conf["skip_create_table"]
	if !ok {
		rawSkipCreateTable = "false"
	}
	skipCreateTable, err := parseutil.ParseBool(rawSkipCreateTable)
	if err != nil {
		return nil, fmt.Errorf("failed to parse value for `skip_create_table`: %w", err)
	}
	if !skipCreateTable {
		if err := c.createTables(); err != nil {
			return nil, fmt.Errorf("failed to create tables: %w", err)
		}
	}

	return c, nil
}

func (c *CockroachDBBackend) createTables() error {
	txn, err := c.client.BeginTx(context.TODO(), &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	defer txn.Rollback() //nolint:errcheck

	// CockroachDB's collation support doesn't recognize the "C" locale name
	// PostgreSQL uses for a byte-order collation, so these columns are left
	// uncollated; CockroachDB's default STRING comparison is already a
	// straightforward byte/codepoint ordering.
	createTableQuery := "CREATE TABLE IF NOT EXISTS " + c.table + " (" +
		`parent_path TEXT NOT NULL,` +
		`  path        TEXT,` +
		`  key         TEXT,` +
		`  value       BYTES,` +
		`  CONSTRAINT ` + c.tableConstraint + ` PRIMARY KEY (path, key)` +
		`);`
	if _, err := txn.Exec(createTableQuery); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			c.logger.Warn("skipping table creation: already created by another process")
			return nil
		}
		return fmt.Errorf("failed to execute create query: %w", err)
	}

	createIndexQuery := `CREATE INDEX IF NOT EXISTS ` + c.index + ` ON ` + c.table + ` (parent_path);`
	if _, err := txn.Exec(createIndexQuery); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			c.logger.Warn("skipping index creation: already created by another process")
			return nil
		}
		return fmt.Errorf("failed to create index on table: %w", err)
	}

	if c.haEnabled {
		createTableQuery := `CREATE TABLE IF NOT EXISTS ` + c.haTable + ` (` +
			`  ha_key      TEXT NOT NULL,` +
			`  ha_identity TEXT NOT NULL,` +
			`  ha_value    TEXT,` +
			`  valid_until TIMESTAMP WITH TIME ZONE NOT NULL,` +
			`  CONSTRAINT ` + c.haTableConstraint + ` PRIMARY KEY (ha_key)` +
			`);`
		if _, err := txn.Exec(createTableQuery); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				c.logger.Warn("skipping table creation: already created by another process")
				return nil
			}
			return fmt.Errorf("failed to create ha table: %w", err)
		}
	}

	if err := txn.Commit(); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			c.logger.Warn("skipping table creation: already created by another process")
			return nil
		}
		return fmt.Errorf("failed to apply transaction: %w", err)
	}

	return nil
}

// splitKey is a helper to split a full path key into individual
// parts: parentPath, path, key
func (c *CockroachDBBackend) splitKey(fullPath string) (string, string, string) {
	var parentPath string
	var path string

	pieces := strings.Split(fullPath, "/")
	depth := len(pieces)
	key := pieces[depth-1]

	switch depth {
	case 1:
		parentPath = ""
		path = "/"
	case 2:
		parentPath = "/"
		path = "/" + pieces[0] + "/"
	default:
		parentPath = "/" + strings.Join(pieces[:depth-2], "/") + "/"
		path = "/" + strings.Join(pieces[:depth-1], "/") + "/"
	}

	return parentPath, path, key
}

// Put is used to insert or update an entry.
func (c *CockroachDBBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"cockroachdb", "put"}, time.Now())

	parentPath, path, key := c.splitKey(entry.Key)

	if err := c.validateFence(ctx); err != nil {
		return err
	}

	_, err := c.client.ExecContext(ctx, c.putQuery, parentPath, path, key, entry.Value)
	return err
}

// Get is used to fetch an entry.
func (c *CockroachDBBackend) Get(ctx context.Context, fullPath string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"cockroachdb", "get"}, time.Now())

	_, path, key := c.splitKey(fullPath)

	var result []byte
	err := c.client.QueryRowContext(ctx, c.getQuery, path, key).Scan(&result)
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

// Delete is used to permanently delete an entry
func (c *CockroachDBBackend) Delete(ctx context.Context, fullPath string) error {
	defer metrics.MeasureSince([]string{"cockroachdb", "delete"}, time.Now())

	_, path, key := c.splitKey(fullPath)

	if err := c.validateFence(ctx); err != nil {
		return err
	}

	_, err := c.client.ExecContext(ctx, c.deleteQuery, path, key)
	return err
}

// List is used to list all the keys under a given
// prefix, up to the next prefix.
func (c *CockroachDBBackend) List(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"cockroachdb", "list"}, time.Now())

	rows, err := c.client.QueryContext(ctx, c.listQuery, "/"+prefix)
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

// ListPage is used to list all the keys under a given
// prefix, after the given key, up to the given limit.
func (c *CockroachDBBackend) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	defer metrics.MeasureSince([]string{"cockroachdb", "list-page"}, time.Now())

	var rows *sql.Rows
	var err error
	if limit <= 0 {
		rows, err = c.client.QueryContext(ctx, c.listPageQuery, "/"+prefix, after)
	} else {
		rows, err = c.client.QueryContext(ctx, c.listPageLimitedQuery, "/"+prefix, after, limit)
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

// LockWith is used for mutual exclusion based on the given key.
func (c *CockroachDBBackend) LockWith(key, value string) (physical.Lock, error) {
	identity, err := uuid.GenerateUUID()
	if err != nil {
		return nil, err
	}
	return &CockroachDBLock{
		backend:       c,
		key:           key,
		value:         value,
		identity:      identity,
		ttlSeconds:    CockroachDBLockTTLSeconds,
		renewInterval: CockroachDBLockRenewInterval,
		retryInterval: CockroachDBLockRetryInterval,
	}, nil
}

func (c *CockroachDBBackend) HAEnabled() bool {
	return c.haEnabled
}

func (c *CockroachDBBackend) RegisterActiveNodeLock(l physical.Lock) error {
	lock, ok := l.(*CockroachDBLock)
	if !ok {
		return fmt.Errorf("expected CockroachDBLock; got %T", l)
	}

	c.fenceLock.Lock()
	defer c.fenceLock.Unlock()
	c.fence = lock

	return nil
}

func (c *CockroachDBBackend) validateFence(ctx context.Context) error {
	c.fenceLock.RLock()
	defer c.fenceLock.RUnlock()

	if c.fence == nil {
		return nil
	}

	if physical.IsUnfencedWrite(ctx) {
		return nil
	}

	held, err := c.fence.IsActivelyHeld(ctx)
	if err != nil {
		return fmt.Errorf("%v: err from database: %w", physical.ErrFencedWriteFailed, err)
	}
	if !held {
		return fmt.Errorf("%v: lock changed ownership", physical.ErrFencedWriteFailed)
	}

	return nil
}

// Lock tries to acquire the lock by repeatedly trying to create a record in the
// CockroachDB table. It will block until either the stop channel is closed or
// the lock could be acquired successfully. The returned channel will be closed
// once the lock in the CockroachDB table cannot be renewed, either due to an
// error speaking to CockroachDB or because someone else has taken it.
func (l *CockroachDBLock) Lock(stopCh <-chan struct{}) (<-chan struct{}, error) {
	l.lock.Lock()
	defer l.lock.Unlock()

	var (
		success = make(chan struct{})
		errs    = make(chan error)
		leader  = make(chan struct{})
	)
	go l.tryToLock(stopCh, success, errs)

	select {
	case <-success:
		l.renewTicker = time.NewTicker(l.renewInterval)
		go l.periodicallyRenewLock(leader)
	case err := <-errs:
		return nil, err
	case <-stopCh:
		return nil, nil
	}

	return leader, nil
}

// Unlock releases the lock by deleting the lock record from the
// CockroachDB table.
func (l *CockroachDBLock) Unlock() error {
	c := l.backend

	if l.renewTicker != nil {
		l.renewTicker.Stop()
	}

	_, err := c.client.Exec(c.haDeleteLockExec, l.identity, l.key)
	return err
}

// Value checks whether or not the lock is held by any instance of CockroachDBLock,
// including this one, and returns the current value.
func (l *CockroachDBLock) Value() (bool, string, error) {
	c := l.backend
	var result string
	err := c.client.QueryRow(c.haGetLockValueQuery, l.key).Scan(&result)

	switch {
	case err == nil:
		return true, result, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, "", nil
	default:
		return false, "", err
	}
}

// IsActivelyHeld returns true if and only if this lock is active. Returns
// false if the lock is held by another caller or an error occurred.
func (l *CockroachDBLock) IsActivelyHeld(ctx context.Context) (bool, error) {
	c := l.backend

	var result int
	err := c.client.QueryRowContext(ctx, c.haCheckLockHeldQuery, l.identity, l.key, l.value).Scan(&result)

	switch {
	case err == nil:
		return result == 1, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

// tryToLock tries to create a new item in CockroachDB every `retryInterval`.
// As long as the item cannot be created (because it already exists), it will
// be retried. If the operation fails due to an error, it is sent to the errors
// channel. When the lock could be acquired successfully, the success channel
// is closed.
func (l *CockroachDBLock) tryToLock(stop <-chan struct{}, success chan struct{}, errs chan error) {
	ticker := time.NewTicker(l.retryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			gotlock, err := l.writeItem(l.backend.haUpsertLockIdentityExec)
			switch {
			case err != nil:
				errs <- err
				return
			case gotlock:
				close(success)
				return
			}
		}
	}
}

func (l *CockroachDBLock) periodicallyRenewLock(done chan struct{}) {
	for range l.renewTicker.C {
		gotlock, err := l.writeItem(l.backend.haRenewLockIdentityExec)
		if err != nil || !gotlock {
			close(done)
			l.renewTicker.Stop()

			if err != nil {
				l.backend.logger.Error("lock renewal failed", "key", l.key, "err", err)
			}

			return
		}
	}
}

// writeItem attempts to put/update the CockroachDB item using condition
// expressions to evaluate the TTL. Returns true if the lock was obtained,
// false if not. If false, error may be nil (someone else has the lock) or
// non-nil (something unexpected happened).
func (l *CockroachDBLock) writeItem(query string) (bool, error) {
	c := l.backend

	// Set a timeout on lock renewal: ensure we block at most 2/3rds of the
	// total lock period, so we notify on leadership loss before another node
	// could acquire the lock and take over as leader.
	ctx, cancel := context.WithTimeout(context.Background(), CockroachDBLockTTLSeconds*2/3*time.Second)
	defer cancel()

	sqlResult, err := c.client.ExecContext(ctx, query, l.identity, l.key, l.value, l.ttlSeconds)
	if err != nil {
		return false, err
	}
	if sqlResult == nil {
		return false, errors.New("empty SQL response received")
	}

	ar, err := sqlResult.RowsAffected()
	if err != nil {
		return false, err
	}
	return ar == 1, nil
}
