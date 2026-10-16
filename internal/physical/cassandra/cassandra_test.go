// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cassandra

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gocql/gocql"
	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"

	cassandrahelper "github.com/openbao/openbao/v2/internal/helper/testhelpers/cassandra"
	"github.com/openbao/openbao/v2/internal/helper/testhelpers/certhelpers"
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

// TestSetupCassandraTLS_ServerName checks the TLS config against a server whose certificate
// only lists a DNS name, the way gocql uses it for a node it discovered from the cluster: it
// dials the node by IP and, unless ServerName is set, verifies the certificate against that IP.
func TestSetupCassandraTLS_ServerName(t *testing.T) {
	t.Parallel()

	const serverName = "cassandra.demo.svc"

	ca := certhelpers.NewCert(t, certhelpers.CommonName("cassandra-ca"), certhelpers.IsCA(true), certhelpers.SelfSign())
	server := certhelpers.NewCert(t, certhelpers.CommonName(serverName), certhelpers.DNS(serverName), certhelpers.Parent(ca))
	client := certhelpers.NewCert(t, certhelpers.CommonName("vault"), certhelpers.Parent(ca))

	bundle := filepath.Join(t.TempDir(), "pem_bundle")
	if err := os.WriteFile(bundle, bytes.Join([][]byte{client.Pem, client.PrivateKeyPEM(), ca.Pem}, nil), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{server.TLSCert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()

	handshake := func(conf map[string]string) error {
		cluster := gocql.NewCluster()
		if err := setupCassandraTLS(conf, cluster); err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		// the address of a node discovered from the cluster
		dialed, err := gocql.WrapTLS(context.Background(), conn, "10.0.0.7:9042", cluster.SslOpts.Config)
		if err != nil {
			return err
		}
		return dialed.Conn.Close()
	}

	conf := map[string]string{"tls": "1", "pem_bundle_file": bundle}
	if err := handshake(conf); err == nil || !strings.Contains(err.Error(), "10.0.0.7") {
		t.Fatalf("expected the certificate check against the node IP to fail, got: %v", err)
	}

	conf["tls_server_name"] = serverName
	if err := handshake(conf); err != nil {
		t.Fatalf("expected the handshake to succeed with tls_server_name: %v", err)
	}

	conf["tls_server_name"] = "other.demo.svc"
	if err := handshake(conf); err == nil {
		t.Fatal("expected the handshake to fail with a name the certificate doesn't cover")
	}
}
