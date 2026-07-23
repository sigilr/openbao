// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package weaviate implements an OpenBao v5 database plugin for the
// Weaviate vector database. **Weaviate's self-hosted server has no
// runtime user/key-management API** — API keys are loaded from the
// AUTHENTICATION_APIKEY_ALLOWED_KEYS environment variable at startup.
// This plugin is therefore a static-credentials shim:
//
//   - Initialize parses config and (with VerifyConnection=true) calls
//     `/v1/.well-known/ready` with the configured API key as a Bearer
//     token.
//   - NewUser returns an explicit error pointing operators at static
//     roles or external configuration management.
//   - UpdateUser is a no-op against the server but returns success on
//     password updates so OpenBao static-role rotation works as a
//     coordination mechanism.
//   - DeleteUser is a no-op.
//
// The plugin still provides value for operators who want OpenBao to be
// the source of truth for the Weaviate API key and its rotation
// schedule, while configuration management or a sidecar applies the
// actual change.
package weaviate

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

const weaviateTypeName = "weaviate"

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Weaviate implements dbplugin.Database in static-only mode.
type Weaviate struct {
	mu     sync.Mutex
	config *weaviateConfig
	client *http.Client
}

type weaviateConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`

	CACert     string `mapstructure:"ca_cert"`
	CAPath     string `mapstructure:"ca_path"`
	ClientCert string `mapstructure:"client_cert"`
	ClientKey  string `mapstructure:"client_key"`
	Insecure   bool   `mapstructure:"insecure"`
}

var (
	_ dbplugin.Database       = (*Weaviate)(nil)
	_ logical.PluginVersioner = (*Weaviate)(nil)
)

func New() (interface{}, error) {
	db := newWeaviate()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newWeaviate() *Weaviate {
	return &Weaviate{}
}

func (w *Weaviate) secretValues() map[string]string {
	if w.config == nil {
		return map[string]string{}
	}
	return map[string]string{w.config.APIKey: "[api_key]"}
}

func (w *Weaviate) Type() (string, error) {
	return weaviateTypeName, nil
}

func (w *Weaviate) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (w *Weaviate) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil {
		w.client.CloseIdleConnections()
	}
	w.client = nil
	return nil
}

func (w *Weaviate) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg := &weaviateConfig{}
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
	w.config = cfg
	w.client = client

	if req.VerifyConnection {
		if err := w.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser is unsupported on Weaviate — see the package comment.
func (w *Weaviate) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	return dbplugin.NewUserResponse{}, errors.New(
		"dynamic credentials are not supported by Weaviate self-hosted (API keys are loaded from " +
			"AUTHENTICATION_APIKEY_ALLOWED_KEYS at startup); use static-roles to track a manually-provisioned key, " +
			"or run a sidecar that updates the server config on UpdateUser",
	)
}

// UpdateUser is a no-op against the server but returns success on
// password updates so static-role rotation tracks the new credential.
func (w *Weaviate) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser is a no-op.
func (w *Weaviate) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	return dbplugin.DeleteUserResponse{}, nil
}

func (w *Weaviate) healthcheck(ctx context.Context) error {
	base := strings.TrimRight(w.config.URL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/.well-known/ready", nil)
	if err != nil {
		return err
	}
	if w.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.config.APIKey)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("weaviate /v1/.well-known/ready failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

func newHTTPClient(cfg *weaviateConfig) (*http.Client, error) {
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
