// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package mssql

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/v2/internal/helper/testhelpers/certhelpers"
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

func TestBuildConnectionString(t *testing.T) {
	t.Parallel()

	password := `p;a"s=s word`
	cfg, err := msdsn.Parse(buildConnectionString([][2]string{
		{"server", "db.example.com"},
		{"user id", "sa"},
		{"password", password},
		{"port", "1444"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.example.com" || cfg.Port != 1444 || cfg.User != "sa" || cfg.Password != password {
		t.Fatalf("unexpected config: host=%q port=%d user=%q password=%q", cfg.Host, cfg.Port, cfg.User, cfg.Password)
	}
}

func TestParseTLSConfig(t *testing.T) {
	t.Parallel()

	testcases := map[string]struct {
		conf    map[string]string
		params  [][2]string
		caFile  string
		wantErr bool
	}{
		"none": {
			conf: map[string]string{},
		},
		"all": {
			conf: map[string]string{
				"encrypt":                  "TRUE",
				"trust_server_certificate": "false",
				"tls_ca_file":              "/etc/ca.crt",
				"tls_server_name":          "db.example.com",
				"tls_min_version":          "tls12",
			},
			params: [][2]string{
				{"encrypt", "true"},
				{"trustservercertificate", "false"},
				{"hostnameincertificate", "db.example.com"},
				{"tlsmin", "1.2"},
			},
			caFile: "/etc/ca.crt",
		},
		"invalid encrypt": {
			conf:    map[string]string{"encrypt": "yes"},
			wantErr: true,
		},
		"invalid trust_server_certificate": {
			conf:    map[string]string{"encrypt": "true", "trust_server_certificate": "maybe"},
			wantErr: true,
		},
		"invalid tls_min_version": {
			conf:    map[string]string{"encrypt": "true", "tls_min_version": "1.2"},
			wantErr: true,
		},
		"tls option without encrypt": {
			conf:    map[string]string{"tls_ca_file": "/etc/ca.crt"},
			wantErr: true,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			params, caFile, err := parseTLSConfig(tc.conf)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(params, tc.params) || caFile != tc.caFile {
				t.Fatalf("got params %v and CA file %q, expected %v and %q", params, caFile, tc.params, tc.caFile)
			}
		})
	}
}

func TestMSSQLBackend_TLS(t *testing.T) {
	cleanup, host, port, caPEM := mssqlhelper.PrepareTLSTestContainer(t)
	defer cleanup()

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	otherCA := certhelpers.NewCert(t, certhelpers.IsCA(true), certhelpers.SelfSign())
	otherCAFile := filepath.Join(t.TempDir(), "other-ca.crt")
	if err := os.WriteFile(otherCAFile, otherCA.Pem, 0o600); err != nil {
		t.Fatal(err)
	}

	logger := logging.NewVaultLogger(log.Debug)

	newBackend := func(tlsConf map[string]string) (physical.Backend, error) {
		conf := map[string]string{
			"server":            host,
			"port":              strconv.Itoa(port),
			"database":          "test",
			"table":             "test",
			"username":          mssqlhelper.Username,
			"password":          mssqlhelper.Password,
			"connectiontimeout": "10",
		}
		maps.Copy(conf, tlsConf)
		return NewMSSQLBackend(conf, logger)
	}

	failures := map[string]map[string]string{
		"unknown CA": {
			"encrypt":         "true",
			"tls_server_name": mssqlhelper.TLSServerName,
		},
		"other CA": {
			"encrypt":         "true",
			"tls_ca_file":     otherCAFile,
			"tls_server_name": mssqlhelper.TLSServerName,
		},
		"wrong server name": {
			"encrypt":     "true",
			"tls_ca_file": caFile,
		},
	}
	for name, conf := range failures {
		t.Run(name, func(t *testing.T) {
			if _, err := newBackend(conf); err == nil || !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("expected a certificate verification error, got: %v", err)
			}
		})
	}

	t.Run("trust server certificate", func(t *testing.T) {
		if _, err := newBackend(map[string]string{
			"encrypt":                  "true",
			"trust_server_certificate": "true",
		}); err != nil {
			t.Fatal(err)
		}
	})

	b, err := newBackend(map[string]string{
		"encrypt":         "true",
		"tls_ca_file":     caFile,
		"tls_server_name": mssqlhelper.TLSServerName,
		"tls_min_version": "tls12",
	})
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
