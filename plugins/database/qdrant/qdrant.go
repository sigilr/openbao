// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package qdrant implements an OpenBao v5 database plugin for the
// Qdrant vector database. **Qdrant has no runtime user/key-management
// API** — the API key is loaded from the QDRANT__SERVICE__API_KEY env var
// at startup. This plugin is therefore a static-credentials shim:
//
//   - Initialize verifies the configured API key works by calling the
//     /readyz endpoint with the key as `api-key` header.
//   - NewUser returns an explicit error pointing operators at static
//     roles or external configuration management.
//   - UpdateUser is a no-op against the server but returns success on
//     password updates so OpenBao static-role rotation works as a
//     coordination mechanism.
//   - DeleteUser is a no-op.
//
// The plugin still provides value for operators who want OpenBao to be
// the source of truth for the Qdrant API key and its rotation schedule,
// while configuration management or a sidecar applies the actual change.
package qdrant

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

const qdrantTypeName = "qdrant"

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Qdrant implements dbplugin.Database in static-only mode.
type Qdrant struct {
	mu     sync.Mutex
	config *qdrantConfig
	client *http.Client
}

type qdrantConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`

	CACert     string `mapstructure:"ca_cert"`
	CAPath     string `mapstructure:"ca_path"`
	ClientCert string `mapstructure:"client_cert"`
	ClientKey  string `mapstructure:"client_key"`
	Insecure   bool   `mapstructure:"insecure"`
}

var (
	_ dbplugin.Database       = (*Qdrant)(nil)
	_ logical.PluginVersioner = (*Qdrant)(nil)
)

func New() (interface{}, error) {
	db := newQdrant()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newQdrant() *Qdrant {
	return &Qdrant{}
}

func (q *Qdrant) secretValues() map[string]string {
	if q.config == nil {
		return map[string]string{}
	}
	return map[string]string{q.config.APIKey: "[api_key]"}
}

func (q *Qdrant) Type() (string, error) {
	return qdrantTypeName, nil
}

func (q *Qdrant) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (q *Qdrant) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.client != nil {
		q.client.CloseIdleConnections()
	}
	q.client = nil
	return nil
}

func (q *Qdrant) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	cfg := &qdrantConfig{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if cfg.URL == "" {
		return dbplugin.InitializeResponse{}, errors.New("url is required")
	}

	client, err := newHTTPClient(cfg)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	q.config = cfg
	q.client = client

	if req.VerifyConnection {
		if err := q.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser is unsupported on Qdrant — see the package comment.
func (q *Qdrant) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	return dbplugin.NewUserResponse{}, errors.New(
		"dynamic credentials are not supported by Qdrant (the API key is loaded from QDRANT__SERVICE__API_KEY at startup); " +
			"use static-roles to track a manually-provisioned API key, or run a sidecar that updates the server config on UpdateUser",
	)
}

// UpdateUser is a no-op against the server. Static-role rotation flows
// through this method and we want OpenBao to keep tracking the rotated
// value even though we can't push it to Qdrant.
func (q *Qdrant) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser is a no-op.
func (q *Qdrant) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	return dbplugin.DeleteUserResponse{}, nil
}

func (q *Qdrant) healthcheck(ctx context.Context) error {
	base := strings.TrimRight(q.config.URL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
	if err != nil {
		return err
	}
	if q.config.APIKey != "" {
		req.Header.Set("api-key", q.config.APIKey)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qdrant /readyz failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

func newHTTPClient(cfg *qdrantConfig) (*http.Client, error) {
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
