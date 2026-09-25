// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package mysql

import (
	"bytes"
	"strings"
	"testing"
	"time"

	stdmysql "github.com/go-sql-driver/mysql"
	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"

	mysqlhelper "github.com/openbao/openbao/v2/internal/helper/testhelpers/mysql"
)

// These tests each spin up their own dedicated MySQL container via
// dockertest and are intentionally not run with t.Parallel(): running
// several MySQL server bootstraps concurrently is prone to resource
// contention (and consequent connection flakiness) on constrained CI/dev
// hosts.

func TestMySQLPlaintextCatch(t *testing.T) {
	cleanup, connURL := mysqlhelper.PrepareTestContainer(t, false, "secret")
	defer cleanup()

	cfg, err := stdmysql.ParseDSN(connURL)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log.DefaultOutput = &buf
	logger := logging.NewVaultLogger(log.Debug)

	if _, err := NewMySQLBackend(map[string]string{
		"address":                      cfg.Addr,
		"database":                     "test",
		"table":                        "test",
		"username":                     cfg.User,
		"password":                     cfg.Passwd,
		"plaintext_connection_allowed": "false",
	}, logger); err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	_, rest, _ := strings.Cut(buf.String(), " ")

	if !strings.Contains(rest, "credentials will be sent in plaintext") {
		t.Fatalf("No warning of plaintext credentials occurred")
	}
}

func TestMySQLBackend(t *testing.T) {
	cleanup, connURL := mysqlhelper.PrepareTestContainer(t, false, "secret")
	defer cleanup()

	cfg, err := stdmysql.ParseDSN(connURL)
	if err != nil {
		t.Fatal(err)
	}

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewMySQLBackend(map[string]string{
		"address":                      cfg.Addr,
		"database":                     "test",
		"table":                        "test",
		"username":                     cfg.User,
		"password":                     cfg.Passwd,
		"plaintext_connection_allowed": "true",
		"max_connection_lifetime":      "1",
	}, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	defer func() {
		m := b.(*MySQLBackend)
		if _, err := m.client.Exec("DROP TABLE IF EXISTS " + m.dbTable + " ," + m.dbLockTable); err != nil {
			t.Fatalf("Failed to drop table: %v", err)
		}
	}()

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}

func TestMySQLHABackend(t *testing.T) {
	cleanup, connURL := mysqlhelper.PrepareTestContainer(t, false, "secret")
	defer cleanup()

	cfg, err := stdmysql.ParseDSN(connURL)
	if err != nil {
		t.Fatal(err)
	}

	logger := logging.NewVaultLogger(log.Debug)
	config := map[string]string{
		"address":                      cfg.Addr,
		"database":                     "test",
		"table":                        "test",
		"username":                     cfg.User,
		"password":                     cfg.Passwd,
		"ha_enabled":                   "true",
		"plaintext_connection_allowed": "true",
	}

	b, err := NewMySQLBackend(config, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	defer func() {
		m := b.(*MySQLBackend)
		if _, err := m.client.Exec("DROP TABLE IF EXISTS " + m.dbTable + " ," + m.dbLockTable); err != nil {
			t.Fatalf("Failed to drop table: %v", err)
		}
	}()

	b2, err := NewMySQLBackend(config, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	physical.ExerciseHABackend(t, b.(physical.HABackend), b2.(physical.HABackend))
}

// TestMySQLHABackend_LockFailPanic is a regression test for the panic shown
// in https://github.com/hashicorp/vault/issues/8203 and patched in
// https://github.com/hashicorp/vault/pull/8229.
func TestMySQLHABackend_LockFailPanic(t *testing.T) {
	cleanup, connURL := mysqlhelper.PrepareTestContainer(t, false, "secret")

	cfg, err := stdmysql.ParseDSN(connURL)
	if err != nil {
		t.Fatal(err)
	}

	if err := mysqlhelper.TestCredsExist(t, connURL, cfg.User, cfg.Passwd); err != nil {
		t.Fatalf("Could not connect with new credentials: %s", err)
	}

	logger := logging.NewVaultLogger(log.Debug)
	config := map[string]string{
		"address":                      cfg.Addr,
		"database":                     cfg.DBName,
		"table":                        "test",
		"username":                     cfg.User,
		"password":                     cfg.Passwd,
		"ha_enabled":                   "true",
		"plaintext_connection_allowed": "true",
	}

	b, err := NewMySQLBackend(config, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	b2, err := NewMySQLBackend(config, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	b1ha := b.(physical.HABackend)
	b2ha := b2.(physical.HABackend)

	// Copied from ExerciseHABackend - ensuring things are normal at this point
	// Get the lock
	lock, err := b1ha.LockWith("foo", "bar")
	if err != nil {
		t.Fatalf("initial lock: %v", err)
	}

	// Attempt to lock
	leaderCh, err := lock.Lock(nil)
	if err != nil {
		t.Fatalf("lock attempt 1: %v", err)
	}
	if leaderCh == nil {
		t.Fatalf("missing leaderCh")
	}

	// Check the value
	held, val, err := lock.Value()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !held {
		t.Errorf("should be held")
	}
	if val != "bar" {
		t.Errorf("expected value bar: %v", err)
	}

	// Second acquisition should fail
	lock2, err := b2ha.LockWith("foo", "baz")
	if err != nil {
		t.Fatalf("lock 2: %v", err)
	}

	stopCh := make(chan struct{})
	time.AfterFunc(10*time.Second, func() {
		close(stopCh)
	})

	// Attempt to lock - can't lock because lock1 is held - this is normal
	leaderCh2, err := lock2.Lock(stopCh)
	if err != nil {
		t.Fatalf("stop lock 2: %v", err)
	}
	if leaderCh2 != nil {
		t.Errorf("should not have gotten leaderCh: %v", leaderCh2)
	}
	// end normal

	// Clean up the database. When Lock() is called, a new connection is created
	// using the configuration. If that connection cannot be created, there was a
	// panic due to not returning with the connection error. Here we intentionally
	// break the config for b2, so a new connection can't be made, which would
	// trigger the panic shown in https://github.com/hashicorp/vault/issues/8203.
	cleanup()

	stopCh2 := make(chan struct{})
	time.AfterFunc(10*time.Second, func() {
		close(stopCh2)
	})
	leaderCh2, err = lock2.Lock(stopCh2)
	if err == nil {
		t.Fatalf("expected error, got none, leaderCh2=%v", leaderCh2)
	}
}

func TestValidateDBTable(t *testing.T) {
	t.Parallel()

	type testCase struct {
		database  string
		table     string
		expectErr bool
	}

	tests := map[string]testCase{
		"empty database & table":        {"", "", true},
		"empty database":                {"", "a", true},
		"empty table":                   {"a", "", true},
		"ascii database":                {"abcde", "a", false},
		"ascii table":                   {"a", "abcde", false},
		"ascii database & table":        {"abcde", "abcde", false},
		"only whitespace db":            {"     ", "a", true},
		"only whitespace table":         {"a", "     ", true},
		"whitespace prefix db":          {" bcde", "a", true},
		"whitespace middle db":          {"ab de", "a", true},
		"whitespace suffix db":          {"abcd ", "a", true},
		"whitespace prefix table":       {"a", " bcde", true},
		"whitespace middle table":       {"a", "ab de", true},
		"whitespace suffix table":       {"a", "abcd ", true},
		"backtick prefix db":            {"`bcde", "a", true},
		"backtick middle db":            {"ab`de", "a", true},
		"backtick suffix db":            {"abcd`", "a", true},
		"backtick prefix table":         {"a", "`bcde", true},
		"backtick middle table":         {"a", "ab`de", true},
		"backtick suffix table":         {"a", "abcd`", true},
		"single quote prefix db":        {"'bcde", "a", true},
		"single quote middle db":        {"ab'de", "a", true},
		"single quote suffix db":        {"abcd'", "a", true},
		"single quote prefix table":     {"a", "'bcde", true},
		"single quote middle table":     {"a", "ab'de", true},
		"single quote suffix table":     {"a", "abcd'", true},
		"double quote prefix db":        {`"bcde`, "a", true},
		"double quote middle db":        {`ab"de`, "a", true},
		"double quote suffix db":        {`abcd"`, "a", true},
		"double quote prefix table":     {"a", `"bcde`, true},
		"double quote middle table":     {"a", `ab"de`, true},
		"double quote suffix table":     {"a", `abcd"`, true},
		"0x0000 prefix db":              {str(0x0000, 'b', 'c'), "a", true},
		"0x0000 middle db":              {str('a', 0x0000, 'c'), "a", true},
		"0x0000 suffix db":              {str('a', 'b', 0x0000), "a", true},
		"0x0000 prefix table":           {"a", str(0x0000, 'b', 'c'), true},
		"0x0000 middle table":           {"a", str('a', 0x0000, 'c'), true},
		"0x0000 suffix table":           {"a", str('a', 'b', 0x0000), true},
		"unicode > 0xFFFF prefix db":    {str(0x10000, 'b', 'c'), "a", true},
		"unicode > 0xFFFF middle db":    {str('a', 0x10000, 'c'), "a", true},
		"unicode > 0xFFFF suffix db":    {str('a', 'b', 0x10000), "a", true},
		"unicode > 0xFFFF prefix table": {"a", str(0x10000, 'b', 'c'), true},
		"unicode > 0xFFFF middle table": {"a", str('a', 0x10000, 'c'), true},
		"unicode > 0xFFFF suffix table": {"a", str('a', 'b', 0x10000), true},
		"non-printable prefix db":       {str(0x0001, 'b', 'c'), "a", true},
		"non-printable middle db":       {str('a', 0x0001, 'c'), "a", true},
		"non-printable suffix db":       {str('a', 'b', 0x0001), "a", true},
		"non-printable prefix table":    {"a", str(0x0001, 'b', 'c'), true},
		"non-printable middle table":    {"a", str('a', 0x0001, 'c'), true},
		"non-printable suffix table":    {"a", str('a', 'b', 0x0001), true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateDBTable(test.database, test.table)
			if test.expectErr && err == nil {
				t.Fatalf("err expected, got nil")
			}
			if !test.expectErr && err != nil {
				t.Fatalf("no error expected, got: %s", err)
			}
		})
	}
}

func str(r ...rune) string {
	return string(r)
}
