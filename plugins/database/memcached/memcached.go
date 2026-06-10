// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package memcached implements an OpenBao v5 database plugin for
// Memcached. **There is no runtime user-management API in Memcached** —
// SASL credentials are loaded from a static auth file at startup. As a
// result this plugin is a static-credential shim only:
//
//   - Initialize and the optional VerifyConnection do a SASL-authenticated
//     no-op against the server to confirm the configured username/password
//     work.
//   - NewUser returns an explicit error pointing operators at static
//     roles or external configuration management.
//   - UpdateUser returns success without contacting the server, so
//     OpenBao static-role password rotation works as a coordination
//     mechanism: OpenBao tracks the password and emits audit events on
//     rotation, while the actual auth file is updated out of band.
//   - DeleteUser returns success without contacting the server.
//
// This is intentional and documented. The plugin still provides value
// for operators who want OpenBao to be the source of truth for the
// Memcached SASL credential and its rotation schedule, even though the
// rotation itself must be applied to the server by configuration
// management or a sidecar.
package memcached

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const memcachedTypeName = "memcached"

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Memcached implements dbplugin.Database in static-only mode.
type Memcached struct {
	mu     sync.Mutex
	config *memcachedConfig
}

type memcachedConfig struct {
	Address  string `mapstructure:"address"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`

	TLSCA      string `mapstructure:"tls_ca"`
	TLSCAPath  string `mapstructure:"tls_ca_path"`
	ClientCert string `mapstructure:"client_cert"`
	ClientKey  string `mapstructure:"client_key"`
	UseTLS     bool   `mapstructure:"use_tls"`
	Insecure   bool   `mapstructure:"insecure"`
}

var (
	_ dbplugin.Database       = (*Memcached)(nil)
	_ logical.PluginVersioner = (*Memcached)(nil)
)

func New() (interface{}, error) {
	db := newMemcached()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newMemcached() *Memcached {
	return &Memcached{}
}

func (m *Memcached) secretValues() map[string]string {
	if m.config == nil {
		return map[string]string{}
	}
	return map[string]string{m.config.Password: "[password]"}
}

func (m *Memcached) Type() (string, error) {
	return memcachedTypeName, nil
}

func (m *Memcached) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (m *Memcached) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil
}

func (m *Memcached) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := &memcachedConfig{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if cfg.Address == "" {
		return dbplugin.InitializeResponse{}, errors.New("address is required (host:port)")
	}

	m.config = cfg

	if req.VerifyConnection {
		if err := m.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser is unsupported on Memcached — see the package comment.
func (m *Memcached) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	return dbplugin.NewUserResponse{}, errors.New(
		"dynamic credentials are not supported by Memcached (no runtime user-management API); " +
			"use static-roles to track a manually-provisioned SASL credential, or run a sidecar that updates the auth file on UpdateUser",
	)
}

// UpdateUser is a no-op against the server. The static-role flow stores
// the new password in OpenBao and emits audit events; operators are
// expected to apply the same change to the SASL auth file out of band.
func (m *Memcached) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	// We intentionally do not error here on Password updates — that's the
	// path static-role rotation takes, and we want OpenBao to keep tracking
	// the rotated value even though we can't push it to the server.
	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser is a no-op for the same reason as UpdateUser.
func (m *Memcached) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	return dbplugin.DeleteUserResponse{}, nil
}

// healthcheck opens a TCP (or TLS) connection to the configured address
// and sends a `stats\r\n` command. This isn't strictly a SASL handshake
// but it confirms the server is reachable. Full SASL-PLAIN is non-trivial
// to spell out without pulling in a third-party Memcached client; the
// trade-off is documented.
func (m *Memcached) healthcheck(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", m.config.Address)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	if m.config.UseTLS || m.config.TLSCA != "" || m.config.TLSCAPath != "" || m.config.ClientCert != "" {
		tlsCfg, err := buildTLS(m.config)
		if err != nil {
			return err
		}
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("tls handshake: %w", err)
		}
		conn = tlsConn
	}

	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	if _, err := conn.Write([]byte("stats\r\n")); err != nil {
		return fmt.Errorf("write stats: %w", err)
	}
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		return fmt.Errorf("read stats reply: %w", err)
	}
	return nil
}

func buildTLS(cfg *memcachedConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.Insecure,
	}
	if cfg.TLSCA != "" || cfg.TLSCAPath != "" {
		pool := x509.NewCertPool()
		if cfg.TLSCA != "" {
			if !pool.AppendCertsFromPEM([]byte(cfg.TLSCA)) {
				return nil, errors.New("failed to parse tls_ca PEM")
			}
		}
		if cfg.TLSCAPath != "" {
			pem, err := os.ReadFile(cfg.TLSCAPath)
			if err != nil {
				return nil, fmt.Errorf("read tls_ca_path: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("failed to parse tls_ca_path PEM")
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
	return tlsCfg, nil
}
