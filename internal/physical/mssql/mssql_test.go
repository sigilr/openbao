// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package mssql

import (
	"strconv"
	"testing"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	mssqlhelper "github.com/openbao/openbao/v2/internal/helper/testhelpers/mssql"
)

// TestInvalidIdentifier checks validity of an identifier
func TestInvalidIdentifier(t *testing.T) {
	t.Parallel()

	testcases := map[string]bool{
		"name":             true,
		"_name":            true,
		"Name":             true,
		"#name":            false,
		"?Name":            false,
		"9name":            false,
		"@name":            false,
		"$name":            false,
		" name":            false,
		"n ame":            false,
		"n4444444":         true,
		"_4321098765":      true,
		"_##$$@@__":        true,
		"_123name#@":       true,
		"name!":            false,
		"name%":            false,
		"name^":            false,
		"name&":            false,
		"name*":            false,
		"name(":            false,
		"name)":            false,
		"nåame":            true,
		"åname":            true,
		"name'":            false,
		"nam`e":            false,
		"пример":           true,
		"_#Āā@#$_ĂĄąćĈĉĊċ": true,
		"ÛÜÝÞßàáâ":         true,
		"豈更滑a23$#@":        true,
	}

	for i, expected := range testcases {
		if !isInvalidIdentifier(i) != expected {
			t.Fatalf("unexpected identifier %s: expected validity %v", i, expected)
		}
	}
}

// These tests each spin up their own dedicated MSSQL container via
// dockertest and are intentionally not run with t.Parallel(): running
// several MSSQL server bootstraps concurrently is prone to resource
// contention (and consequent connection flakiness) on constrained CI/dev
// hosts (see the equivalent comment in internal/physical/mysql).

func TestMSSQLBackend(t *testing.T) {
	cleanup, host, port := mssqlhelper.PrepareTestContainer(t)
	defer cleanup()

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewMSSQLBackend(map[string]string{
		"server":   host,
		"port":     strconv.Itoa(port),
		"database": "test",
		"table":    "test",
		"schema":   "test",
		"username": mssqlhelper.Username,
		"password": mssqlhelper.Password,
	}, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	defer func() {
		m := b.(*MSSQLBackend)
		if _, err := m.client.Exec("DROP TABLE " + m.dbTable); err != nil {
			t.Fatalf("Failed to drop table: %v", err)
		}
	}()

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}

func TestMSSQLBackend_schema(t *testing.T) {
	cleanup, host, port := mssqlhelper.PrepareTestContainer(t)
	defer cleanup()

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewMSSQLBackend(map[string]string{
		"server":   host,
		"port":     strconv.Itoa(port),
		"database": "test",
		"schema":   "customschema",
		"table":    "test",
		"username": mssqlhelper.Username,
		"password": mssqlhelper.Password,
	}, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	defer func() {
		m := b.(*MSSQLBackend)
		if _, err := m.client.Exec("DROP TABLE " + m.dbTable); err != nil {
			t.Fatalf("Failed to drop table: %v", err)
		}
	}()

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}
