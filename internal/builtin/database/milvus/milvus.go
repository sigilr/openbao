// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package milvus implements an OpenBao v5 database plugin for Milvus 2.x
// using its HTTP RESTful API v2 user management endpoints.
package milvus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/hashicorp/go-secure-stdlib/strutil"
	milvusclient "github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/database/helper/dbutil"
	"github.com/openbao/openbao/sdk/v2/helper/template"
	"github.com/openbao/openbao/sdk/v2/logical"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	client           milvusclient.Client
	usernameProducer template.StringTemplate
}
type milvusConfig struct {
	URL      string `mapstructure:"url"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	Token    string `mapstructure:"token"`
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

func New() (any, error) {
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
	if m.client != nil {
		if err := m.client.Close(); err != nil {
			return err
		}
	}
	m.client = nil
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

	clientConfig, err := newMilvusClientConfig(cfg)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	client, err := milvusclient.NewClient(ctx, *clientConfig)
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("create Milvus client: %w", err)
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
	m.client = client
	m.usernameProducer = up

	if req.VerifyConnection {
		if _, err := m.client.GetVersion(ctx); err != nil {
			_ = m.client.Close()
			m.client = nil
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

func newMilvusClientConfig(cfg *milvusConfig) (*milvusclient.Config, error) {
	clientConfig := &milvusclient.Config{
		Address:  cfg.URL,
		Username: cfg.Username,
		Password: cfg.Password,
		APIKey:   cfg.Token,
		DBName:   cfg.DBName,
	}

	if cfg.CACert == "" && cfg.CAPath == "" && cfg.ClientCert == "" && cfg.ClientKey == "" && !cfg.Insecure {
		return clientConfig, nil
	}

	if (cfg.ClientCert == "") != (cfg.ClientKey == "") {
		return nil, errors.New("client_cert and client_key must be provided together")
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.Insecure,
	}

	if cfg.CACert != "" || cfg.CAPath != "" {
		pool := x509.NewCertPool()
		if cfg.CACert != "" && !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
			return nil, errors.New("failed to parse ca_cert PEM")
		}
		if cfg.CAPath != "" {
			caPEM, err := os.ReadFile(cfg.CAPath)
			if err != nil {
				return nil, fmt.Errorf("read ca_path: %w", err)
			}
			if !pool.AppendCertsFromPEM(caPEM) {
				return nil, errors.New("failed to parse ca_path PEM")
			}
		}
		tlsConfig.RootCAs = pool
	}

	if cfg.ClientCert != "" {
		certificate, err := tls.X509KeyPair([]byte(cfg.ClientCert), []byte(cfg.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("client cert/key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}

	clientConfig.EnableTLSAuth = true
	clientConfig.DialOptions = append(
		append([]grpc.DialOption{}, milvusclient.DefaultGrpcOpts...),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
	)
	return clientConfig, nil
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

	if err := m.client.CreateCredential(ctx, username, req.Password); err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("failed to create Milvus user: %w", err)
	}

	for _, role := range stmt.Roles {
		if role == "" {
			continue
		}
		if err := m.client.AddUserRole(ctx, username, role); err != nil {
			if cleanupErr := m.client.DeleteCredential(ctx, username); cleanupErr != nil {
				return dbplugin.NewUserResponse{}, fmt.Errorf("failed to grant role %q: %w; failed to remove partially created user: %v", role, err, cleanupErr)
			}
			return dbplugin.NewUserResponse{}, fmt.Errorf("failed to grant role %q: %w", role, err)
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

	// OpenBao does not retain or provide the managed user's previous password.
	// Milvus permits a configured superuser to reset another user's password
	// without verifying the old password, so leave oldPassword empty here.
	if err := m.client.UpdateCredential(ctx, req.Username, "", req.Password.NewPassword); err != nil {
		return dbplugin.UpdateUserResponse{}, fmt.Errorf("failed to update Milvus user password: %w", err)
	}
	return dbplugin.UpdateUserResponse{}, nil
}

func (m *Milvus) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.client.DeleteCredential(ctx, req.Username); err != nil {
		return dbplugin.DeleteUserResponse{}, fmt.Errorf("failed to delete Milvus user: %w", err)
	}
	return dbplugin.DeleteUserResponse{}, nil
}
