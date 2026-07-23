// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package hazelcast implements a **static-credentials-only** OpenBao v5
// database plugin for Hazelcast IMDG / Platform.
//
// Hazelcast OSS has no runtime user-management API — authentication is
// configured in the member XML (`<security>`) at startup. Hazelcast
// Enterprise's `Permissions` API can be reconfigured at runtime, but
// the plugin keeps the surface uniform across editions and treats both
// as static.
//
// The plugin:
//   - Pings the configured REST endpoint (`/hazelcast/health/ready`) with
//     Basic Auth to verify reachability when VerifyConnection=true.
//   - Returns "not supported" from NewUser.
//   - Treats UpdateUser as a no-op against the server (so static-role
//     rotation still tracks the credential value in OpenBao).
//   - Treats DeleteUser as a no-op.
package hazelcast

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

const hazelcastTypeName = "hazelcast"

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Hazelcast implements dbplugin.Database in static-only mode.
type Hazelcast struct {
	mu     sync.Mutex
	config *hazelcastConfig
	client *http.Client
}

type hazelcastConfig struct {
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
	_ dbplugin.Database       = (*Hazelcast)(nil)
	_ logical.PluginVersioner = (*Hazelcast)(nil)
)

func New() (interface{}, error) {
	db := newHazelcast()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newHazelcast() *Hazelcast {
	return &Hazelcast{}
}

func (h *Hazelcast) secretValues() map[string]string {
	if h.config == nil {
		return map[string]string{}
	}
	return map[string]string{h.config.Password: "[password]"}
}

func (h *Hazelcast) Type() (string, error) {
	return hazelcastTypeName, nil
}

func (h *Hazelcast) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (h *Hazelcast) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		h.client.CloseIdleConnections()
	}
	h.client = nil
	return nil
}

func (h *Hazelcast) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	cfg := &hazelcastConfig{}
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
	h.config = cfg
	h.client = client

	if req.VerifyConnection {
		if err := h.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser is unsupported — see the package comment.
func (h *Hazelcast) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	return dbplugin.NewUserResponse{}, errors.New(
		"dynamic credentials are not supported by Hazelcast OSS (auth is configured in member XML at startup); " +
			"use static-roles to track a manually-provisioned credential, or run a sidecar that updates the cluster config on UpdateUser",
	)
}

// UpdateUser is a no-op against the server but returns success on
// password updates so static-role rotation tracks the new credential.
func (h *Hazelcast) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser is a no-op.
func (h *Hazelcast) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	return dbplugin.DeleteUserResponse{}, nil
}

func (h *Hazelcast) healthcheck(ctx context.Context) error {
	base := strings.TrimRight(h.config.URL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/hazelcast/health/ready", nil)
	if err != nil {
		return err
	}
	if h.config.Username != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(h.config.Username + ":" + h.config.Password))
		req.Header.Set("Authorization", "Basic "+auth)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hazelcast /health/ready failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

func newHTTPClient(cfg *hazelcastConfig) (*http.Client, error) {
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
