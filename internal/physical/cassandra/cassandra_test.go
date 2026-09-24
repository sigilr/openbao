// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cassandra

import (
	"os"
	"reflect"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"

	cassandrahelper "github.com/openbao/openbao/v2/internal/helper/testhelpers/cassandra"
)

func TestCassandraBackend(t *testing.T) {
	if testing.Short() {
		t.Skipf("skipping in short mode")
	}
	if os.Getenv("OPENBAO_CI_GO_TEST_RACE") != "" {
		t.Skip("skipping race test in CI pending https://github.com/gocql/gocql/pull/1474")
	}

	// Cap the JVM heap explicitly: Cassandra's default auto-sizing scales
	// off total host memory, which can over-commit (and get OOM-killed) on
	// memory-constrained CI/dev hosts.
	host, cleanup := cassandrahelper.PrepareTestContainer(
		t,
		cassandrahelper.Env("MAX_HEAP_SIZE=1024M"),
		cassandrahelper.Env("HEAP_NEWSIZE=256M"),
	)
	defer cleanup()

	logger := logging.NewVaultLogger(log.Debug)
	b, err := NewCassandraBackend(map[string]string{
		"hosts":                       host.ConnectionURL(),
		"protocol_version":            "3",
		"connection_timeout":          "5",
		"initial_connection_timeout":  "5",
		"simple_retry_policy_retries": "3",
	}, logger)
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}

func TestCassandraBackendBuckets(t *testing.T) {
	t.Parallel()

	expectations := map[string][]string{
		"":          {"."},
		"a":         {"."},
		"a/b":       {".", "a"},
		"a/b/c/d/e": {".", "a", "a/b", "a/b/c", "a/b/c/d"},
	}

	b := &CassandraBackend{}
	for input, expected := range expectations {
		actual := b.buckets(input)
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("bad: %v expected: %v", actual, expected)
		}
	}
}
