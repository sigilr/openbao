// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package milvus implements an OpenBao v5 database plugin for Milvus 2.x
// using its HTTP RESTful API v2 user management endpoints.
package milvus

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/database/helper/dbutil"
	"github.com/openbao/openbao/sdk/v2/helper/template"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	milvusTypeName = "milvus"

	// Milvus usernames are limited to 32 characters in 2.4+. Cap the
	// template at 32 to avoid the server-side rejection.
	defaultUserNameTemplate = `{{ printf "v-%s-%s-%s" (.DisplayName | truncate 8) (.RoleName | truncate 8) (random 10) | replace "." "-" | truncate 32 }}`
)

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Milvus implements dbplugin.Database via the HTTP RESTful API v2
// (`/v2/vectordb/users/...` and `/v2/vectordb/roles/...`). creation_statements
// is a JSON role doc `{"roles":["role1"]}` listing pre-existing roles to grant.
type Milvus struct {
	mu sync.Mutex

	config           *milvusConfig
	httpClient       *http.Client
	usernameProducer template.StringTemplate
}

type milvusConfig struct {
	URL      string `mapstructure:"url"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	Token    string `mapstructure:"token"` // alternate to username/password (Zilliz Cloud style)
	DBName   string `mapstructure:"db_name"`

	CACert     string `mapstructure:"ca_cert"`
	CAPath     string `mapstructure:"ca_path"`
	ClientCert string `mapstructure:"client_cert"`
	ClientKey  string `mapstructure:"client_key"`
	Insecure   bool   `mapstructure:"insecure"`
}

type milvusStatement struct {
	Roles []string `json:"roles"`
}

var (
	_ dbplugin.Database       = (*Milvus)(nil)
	_ logical.PluginVersioner = (*Milvus)(nil)
)

func New() (interface{}, error) {
	db := newMilvus()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newMilvus() *Milvus {
	return &Milvus{}
}

func (m *Milvus) secretValues() map[string]string {
	if m.config == nil {
		return map[string]string{}
	}
	out := map[string]string{}
	if m.config.Password != "" {
		out[m.config.Password] = "[password]"
	}
	if m.config.Token != "" {
		out[m.config.Token] = "[token]"
	}
	return out
}

func (m *Milvus) Type() (string, error) {
	return milvusTypeName, nil
}

func (m *Milvus) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (m *Milvus) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpClient != nil {
		m.httpClient.CloseIdleConnections()
	}
	m.httpClient = nil
	return nil
}

func (m *Milvus) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := &milvusConfig{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if cfg.URL == "" {
		return dbplugin.InitializeResponse{}, errors.New("url is required")
	}
	if cfg.Token == "" && (cfg.Username == "" || cfg.Password == "") {
		return dbplugin.InitializeResponse{}, errors.New("either token, or both username and password are required")
	}

	client, err := newHTTPClient(cfg)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	usernameTemplate, err := strutil.GetString(req.Config, "username_template")
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if usernameTemplate == "" {
		usernameTemplate = defaultUserNameTemplate
	}
	up, err := template.NewTemplate(template.Template(usernameTemplate))
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("invalid username_template: %w", err)
	}
	if _, err := up.Generate(dbplugin.UsernameMetadata{}); err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("invalid username template: %w", err)
	}

	m.config = cfg
	m.httpClient = client
	m.usernameProducer = up

	if req.VerifyConnection {
		if err := m.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser creates the user, then grants each role from the statement. If
// a grant fails the plugin drops the half-configured user.
func (m *Milvus) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	if len(req.Statements.Commands) == 0 {
		return dbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	var stmt milvusStatement
	if err := json.Unmarshal([]byte(req.Statements.Commands[0]), &stmt); err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("creation_statements must be a JSON role doc: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	username, err := m.usernameProducer.Generate(req.UsernameConfig)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	if err := m.doJSON(ctx, "users/create", map[string]string{
		"userName": username,
		"password": req.Password,
	}); err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("create user: %w", err)
	}

	cleanup := func(opErr error) (dbplugin.NewUserResponse, error) {
		_ = m.doJSON(ctx, "users/drop", map[string]string{"userName": username})
		return dbplugin.NewUserResponse{}, opErr
	}

	for _, role := range stmt.Roles {
		if role == "" {
			continue
		}
		if err := m.doJSON(ctx, "users/grant_role", map[string]string{
			"userName": username,
			"roleName": role,
		}); err != nil {
			return cleanup(fmt.Errorf("grant role %q: %w", role, err))
		}
	}

	return dbplugin.NewUserResponse{Username: username}, nil
}

func (m *Milvus) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	if req.Password == nil {
		return dbplugin.UpdateUserResponse{}, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.doJSON(ctx, "users/update_password", map[string]string{
		"userName":    req.Username,
		"newPassword": req.Password.NewPassword,
	}); err != nil {
		return dbplugin.UpdateUserResponse{}, fmt.Errorf("update password: %w", err)
	}
	return dbplugin.UpdateUserResponse{}, nil
}

func (m *Milvus) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.doJSON(ctx, "users/drop", map[string]string{"userName": req.Username}); err != nil {
		return dbplugin.DeleteUserResponse{}, fmt.Errorf("drop user: %w", err)
	}
	return dbplugin.DeleteUserResponse{}, nil
}

// --- HTTP helpers -----------------------------------------------------------

func (m *Milvus) healthcheck(ctx context.Context) error {
	// users/list is the cheapest authenticated call on /v2/vectordb.
	return m.doJSON(ctx, "users/list", map[string]interface{}{})
}

func (m *Milvus) doJSON(ctx context.Context, op string, body interface{}) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return err
	}
	path := fmt.Sprintf("v2/vectordb/%s", op)
	req, err := m.newRequest(ctx, http.MethodPost, path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s failed: %s: %s", path, resp.Status, string(b))
	}
	// Milvus returns 200 with {"code": <non-zero>, "message": "..."} for
	// API-level errors. Parse the envelope so we don't silently succeed.
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &env); err == nil && env.Code != 0 {
		return fmt.Errorf("milvus %s: code=%d message=%s", path, env.Code, env.Message)
	}
	return nil
}

func (m *Milvus) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	base := strings.TrimRight(m.config.URL, "/")
	full := fmt.Sprintf("%s/%s", base, strings.TrimLeft(path, "/"))
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, err
	}
	if m.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.config.Token)
	} else {
		req.SetBasicAuth(m.config.Username, m.config.Password)
	}
	if m.config.DBName != "" {
		req.Header.Set("dbName", m.config.DBName)
	}
	return req, nil
}

func newHTTPClient(cfg *milvusConfig) (*http.Client, error) {
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
