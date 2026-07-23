// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package db2 implements a **static-credentials-only** OpenBao v5
// database plugin for IBM Db2.
//
// Why static-only? Db2 has no pure-Go driver — the canonical client
// (github.com/ibmdb/go_ibm_db) requires CGO and the IBM CLI Driver
// installed on the build host. That conflicts with OpenBao's
// CGO_ENABLED=0 build target. In addition, native Db2 user management
// (CREATE USER / ALTER USER / DROP USER) requires the `AUTH_NATIVE`
// security plugin loaded; most production Db2 instances delegate to OS
// users or LDAP. Implementing dynamic credentials would be misleading
// for the typical deployment.
//
// This plugin therefore:
//   - Verifies the configured Db2 REST endpoint is reachable (optional).
//   - Returns an explicit "not supported" error from NewUser.
//   - Treats UpdateUser as a no-op against the server (so static-role
//     rotation still tracks the credential value in OpenBao).
//   - Treats DeleteUser as a no-op.
//
// Operators who need dynamic Db2 credentials should run a sidecar that
// uses the Db2 REST API (or `db2` CLI) to apply OpenBao's rotated value
// to the server's auth plugin.
package db2

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const db2TypeName = "db2"

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// DB2 implements dbplugin.Database in static-only mode.
type DB2 struct {
	mu     sync.Mutex
	config *db2Config
	client *http.Client
}

type db2Config struct {
	// URL is the optional Db2 REST API base URL (e.g. http://db2:50000).
	// If empty, VerifyConnection is a no-op.
	URL      string `mapstructure:"url"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`

	CACert     string `mapstructure:"ca_cert"`
	CAPath     string `mapstructure:"ca_path"`
	ClientCert string `mapstructure:"client_cert"`
	ClientKey  string `mapstructure:"client_key"`
	Insecure   bool   `mapstructure:"insecure"`
}

var (
	_ dbplugin.Database       = (*DB2)(nil)
	_ logical.PluginVersioner = (*DB2)(nil)
)

func New() (interface{}, error) {
	db := newDB2()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newDB2() *DB2 {
	return &DB2{}
}

func (d *DB2) secretValues() map[string]string {
	if d.config == nil {
		return map[string]string{}
	}
	return map[string]string{d.config.Password: "[password]"}
}

func (d *DB2) Type() (string, error) {
	return db2TypeName, nil
}

func (d *DB2) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (d *DB2) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		d.client.CloseIdleConnections()
	}
	d.client = nil
	return nil
}

func (d *DB2) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cfg := &db2Config{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	client, err := newHTTPClient(cfg)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	d.config = cfg
	d.client = client

	if req.VerifyConnection && cfg.URL != "" {
		if err := d.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser is unsupported — see the package comment.
func (d *DB2) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	return dbplugin.NewUserResponse{}, errors.New(
		"dynamic credentials are not supported by this Db2 plugin (no pure-Go driver + most prod deployments delegate auth to OS or LDAP); " +
			"use static-roles to track a manually-provisioned credential, or run a sidecar that applies the rotated value to the Db2 auth plugin",
	)
}

// UpdateUser is a no-op against the server but returns success on
// password updates so static-role rotation tracks the new credential.
func (d *DB2) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser is a no-op.
func (d *DB2) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	return dbplugin.DeleteUserResponse{}, nil
}

func (d *DB2) healthcheck(ctx context.Context) error {
	base := strings.TrimRight(d.config.URL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/dbapi/v4/host_status", nil)
	if err != nil {
		return err
	}
	if d.config.Username != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(d.config.Username + ":" + d.config.Password))
		req.Header.Set("Authorization", "Basic "+auth)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("db2 host_status failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

func newHTTPClient(cfg *db2Config) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.Insecure,
	}
	if cfg.CACert != "" || cfg.CAPath != "" {
		pool := x509.NewCertPool()
		if cfg.CACert != "" {
			if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
				return nil, errors.New("failed to parse ca_cert PEM")
			}
		}
		if cfg.CAPath != "" {
			pem, err := os.ReadFile(cfg.CAPath)
			if err != nil {
				return nil, fmt.Errorf("read ca_path: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("failed to parse ca_path PEM")
			}
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.ClientCert != "" && cfg.ClientKey != "" {
		cert, err := tls.X509KeyPair([]byte(cfg.ClientCert), []byte(cfg.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}
