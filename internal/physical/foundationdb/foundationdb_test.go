// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build foundationdb

package foundationdb

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	log "github.com/hashicorp/go-hclog"
	uuid "github.com/hashicorp/go-uuid"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/v2/internal/helper/testhelpers/foundationdb"
)

func connectToFoundationDB(clusterFile string) (*fdb.Database, error) {
	if err := fdb.APIVersion(minAPIVersion); err != nil {
		return nil, fmt.Errorf("failed to set FDB API version: %w", err)
	}

	db, err := fdb.Open(clusterFile, []byte("DB"))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return &db, nil
}

func cleanupTopDir(clusterFile, topDir string) error {
	db, err := connectToFoundationDB(clusterFile)
	if err != nil {
		return fmt.Errorf("could not connect to FDB for cleanup: %w", err)
	}

	if _, err := directory.Root().Remove(db, []string{topDir}); err != nil {
		return fmt.Errorf("could not remove directory: %w", err)
	}

	return nil
}

func TestFoundationDBPathDecoration(t *testing.T) {
	cases := map[string][]byte{
		"foo":              []byte("/\x01foo"),
		"foo/":             []byte("/\x01foo/"),
		"foo/bar":          []byte("/\x02foo/\x01bar"),
		"foo/bar/":         []byte("/\x02foo/\x01bar/"),
		"foo/bar/baz":      []byte("/\x02foo/\x02bar/\x01baz"),
		"foo/bar/baz/":     []byte("/\x02foo/\x02bar/\x01baz/"),
		"foo/bar/baz/quux": []byte("/\x02foo/\x02bar/\x02baz/\x01quux"),
	}

	for path, expected := range cases {
		decorated, err := decoratePath(path)
		if err != nil {
			t.Fatalf("path %s error: %s", path, err)
		}

		if !bytes.Equal(expected, decorated) {
			t.Fatalf("path %s expected %v got %v", path, expected, decorated)
		}

		undecorated := undecoratePath(decorated)
		if undecorated != path {
			t.Fatalf("expected %s got %s", path, undecorated)
		}
	}
}

// TestFoundationDBBackend exercises a live FoundationDB cluster. It requires
// this test binary to be built with -tags foundationdb and linked against
// the native libfdb_c client library (see this package's README.md), and
// either a cluster file at FOUNDATIONDB_CLUSTER_FILE or a working Docker
// daemon to start a disposable one.
func TestFoundationDBBackend(t *testing.T) {
	if testing.Short() {
		t.Skipf("skipping in short mode")
	}

	testUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatalf("foundationdb: could not generate UUID for top-level directory: %s", err)
	}

	topDir := fmt.Sprintf("vault-test-%s", testUUID)

	clusterFile := os.Getenv("FOUNDATIONDB_CLUSTER_FILE")
	if clusterFile == "" {
		var cleanup func()
		cleanup, clusterFile = foundationdb.PrepareTestContainer(t)
		defer cleanup()
	}

	// Remove any leftover test data before starting, and once done
	if err := cleanupTopDir(clusterFile, topDir); err != nil {
		t.Fatalf("foundationdb: could not cleanup test data before starting test: %s", err)
	}
	defer func() {
		if err := cleanupTopDir(clusterFile, topDir); err != nil {
			t.Fatalf("foundationdb: could not cleanup test data at end of test: %s", err)
		}
	}()

	logger := logging.NewVaultLogger(log.Debug)
	config := map[string]string{
		"path":         topDir,
		"api_version":  "520",
		"cluster_file": clusterFile,
	}

	b, err := NewFDBBackend(config, logger)
	if err != nil {
		t.Fatalf("foundationdb: failed to create new backend: %s", err)
	}

	b2, err := NewFDBBackend(config, logger)
	if err != nil {
		t.Fatalf("foundationdb: failed to create new backend: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
	physical.ExerciseHABackend(t, b.(physical.HABackend), b2.(physical.HABackend))
}
