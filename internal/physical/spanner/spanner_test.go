// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package spanner

import (
	"context"
	"testing"

	googlespanner "cloud.google.com/go/spanner"
	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	spanneremulator "github.com/openbao/openbao/v2/internal/helper/testhelpers/spanneremulator"
)

const (
	testTable   = "Vault"
	testHATable = "VaultHA"
)

const testDDL = `CREATE TABLE ` + testTable + ` (
	Key   STRING(MAX) NOT NULL,
	Value BYTES(MAX),
) PRIMARY KEY (Key)`

const testHADDL = `CREATE TABLE ` + testHATable + ` (
	Key       STRING(MAX) NOT NULL,
	Value     STRING(MAX),
	Identity  STRING(MAX),
	Timestamp TIMESTAMP,
) PRIMARY KEY (Key)`

func testCleanup(t testing.TB, table string) {
	t.Helper()

	ctx := context.Background()
	client, err := googlespanner.NewClient(ctx, spanneremulator.DatabaseName)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	m := googlespanner.Delete(table, googlespanner.AllKeys())
	if _, err := client.Apply(ctx, []*googlespanner.Mutation{m}); err != nil {
		t.Fatal(err)
	}
}

func TestBackend(t *testing.T) {
	cleanup := spanneremulator.PrepareTestContainer(t, testDDL, testHADDL)
	defer cleanup()

	testCleanup(t, testTable)
	defer testCleanup(t, testTable)

	backend, err := NewBackend(map[string]string{
		"database":   spanneremulator.DatabaseName,
		"table":      testTable,
		"ha_enabled": "false",
	}, logging.NewVaultLogger(log.Debug))
	if err != nil {
		t.Fatal(err)
	}

	physical.ExerciseBackend(t, backend)
	physical.ExerciseBackend_ListPrefix(t, backend)
}

func TestHABackend(t *testing.T) {
	cleanup := spanneremulator.PrepareTestContainer(t, testDDL, testHADDL)
	defer cleanup()

	testCleanup(t, testTable)
	testCleanup(t, testHATable)
	defer func() {
		testCleanup(t, testTable)
		testCleanup(t, testHATable)
	}()

	config := map[string]string{
		"database":   spanneremulator.DatabaseName,
		"table":      testTable,
		"ha_table":   testHATable,
		"ha_enabled": "true",
	}

	b1, err := NewBackend(config, logging.NewVaultLogger(log.Debug))
	if err != nil {
		t.Fatal(err)
	}
	b2, err := NewBackend(config, logging.NewVaultLogger(log.Debug))
	if err != nil {
		t.Fatal(err)
	}

	ha1, ok1 := b1.(physical.HABackend)
	ha2, ok2 := b2.(physical.HABackend)
	if !ok1 || !ok2 {
		t.Fatal("Spanner backend does not implement HABackend")
	}

	physical.ExerciseHABackend(t, ha1, ha2)
}
