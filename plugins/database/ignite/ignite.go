// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package ignite implements an OpenBao v5 database plugin for Apache
// Ignite using the Ignite REST API. Dynamic credentials become native
// SQL users created via CREATE USER / ALTER USER / DROP USER, which
// Ignite 2.5+ supports when persistence is enabled and
// `authenticationEnabled=true` is set on the cluster.
package ignite

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	igniteTypeName = "ignite"

	// Ignite identifiers are case-folded and capped. Cap at 32 for safety.
	defaultUserNameTemplate = `{{ printf "v_%s_%s_%s" (.DisplayName | truncate 8) (.RoleName | truncate 8) (random 8) | replace "-" "_" | uppercase | truncate 32 }}`
)

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Ignite implements dbplugin.Database. The Ignite REST API requires the
// caller to pick a `cmd` per request; `qrydistexe` runs a SQL statement
// on the cluster's PUBLIC cache.
type Ignite struct {
	mu sync.Mutex

	config           *igniteConfig
	httpClient       *http.Client
	usernameProducer template.StringTemplate
}

type igniteConfig struct {
	URL       string `mapstructure:"url"`
	Username  string `mapstructure:"username"`
	Password  string `mapstructure:"password"`
	CacheName string `mapstructure:"cache_name"`

	CACert     string `mapstructure:"ca_cert"`
	CAPath     string `mapstructure:"ca_path"`
	ClientCert string `mapstructure:"client_cert"`
	ClientKey  string `mapstructure:"client_key"`
	Insecure   bool   `mapstructure:"insecure"`
}

var (
	_ dbplugin.Database       = (*Ignite)(nil)
	_ logical.PluginVersioner = (*Ignite)(nil)
)

func New() (interface{}, error) {
	db := newIgnite()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newIgnite() *Ignite {
	return &Ignite{}
}

func (i *Ignite) secretValues() map[string]string {
	if i.config == nil {
		return map[string]string{}
	}
	return map[string]string{i.config.Password: "[password]"}
}

func (i *Ignite) Type() (string, error) {
	return igniteTypeName, nil
}

func (i *Ignite) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (i *Ignite) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.httpClient != nil {
		i.httpClient.CloseIdleConnections()
	}
	i.httpClient = nil
	return nil
}

func (i *Ignite) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	cfg := &igniteConfig{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if cfg.URL == "" {
		return dbplugin.InitializeResponse{}, errors.New("url is required")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return dbplugin.InitializeResponse{}, errors.New("username and password are required")
	}
	if cfg.CacheName == "" {
		cfg.CacheName = "PUBLIC"
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

	i.config = cfg
	i.httpClient = client
	i.usernameProducer = up

	if req.VerifyConnection {
		if err := i.ping(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser runs every command in creation_statements through the Ignite
// REST API. Statements are not parameterized because Ignite's CREATE USER
// DDL doesn't accept parameters; instead, the username is uppercased +
// underscored and validated to a safe identifier set (rejects single
// quotes, semicolons, and double quotes).
func (i *Ignite) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	if len(req.Statements.Commands) == 0 {
		return dbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	username, err := i.usernameProducer.Generate(req.UsernameConfig)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}
	username = strings.ToUpper(strings.ReplaceAll(username, "-", "_"))
	if err := safeIdentifier(username); err != nil {
		return dbplugin.NewUserResponse{}, err
	}
	if err := safePassword(req.Password); err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	bindings := map[string]string{
		"name":     username,
		"username": username,
		"password": req.Password,
	}

	for _, stmt := range req.Statements.Commands {
		for _, q := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			rendered := renderTemplate(q, bindings)
			if err := i.execSQL(ctx, rendered); err != nil {
				return dbplugin.NewUserResponse{}, fmt.Errorf("execute creation statement: %w", err)
			}
		}
	}

	return dbplugin.NewUserResponse{Username: username}, nil
}

func (i *Ignite) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	if req.Password == nil {
		return dbplugin.UpdateUserResponse{}, nil
	}
	if err := safeIdentifier(req.Username); err != nil {
		return dbplugin.UpdateUserResponse{}, err
	}
	if err := safePassword(req.Password.NewPassword); err != nil {
		return dbplugin.UpdateUserResponse{}, err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	stmt := fmt.Sprintf(`ALTER USER "%s" WITH PASSWORD '%s'`, req.Username, req.Password.NewPassword)
	if err := i.execSQL(ctx, stmt); err != nil {
		return dbplugin.UpdateUserResponse{}, fmt.Errorf("alter user: %w", err)
	}
	return dbplugin.UpdateUserResponse{}, nil
}

func (i *Ignite) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	if err := safeIdentifier(req.Username); err != nil {
		return dbplugin.DeleteUserResponse{}, err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	stmts := req.Statements.Commands
	if len(stmts) == 0 {
		stmts = []string{fmt.Sprintf(`DROP USER "%s"`, req.Username)}
	}
	bindings := map[string]string{"name": req.Username, "username": req.Username}
	for _, s := range stmts {
		for _, q := range strutil.ParseArbitraryStringSlice(s, ";") {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			rendered := renderTemplate(q, bindings)
			if err := i.execSQL(ctx, rendered); err != nil {
				return dbplugin.DeleteUserResponse{}, fmt.Errorf("execute revocation statement: %w", err)
			}
		}
	}
	return dbplugin.DeleteUserResponse{}, nil
}

// --- HTTP / SQL helpers ----------------------------------------------------

func (i *Ignite) ping(ctx context.Context) error {
	q := url.Values{}
	q.Set("cmd", "version")
	q.Set("ignite.login", i.config.Username)
	q.Set("ignite.password", i.config.Password)
	return i.doGet(ctx, q)
}

func (i *Ignite) execSQL(ctx context.Context, statement string) error {
	q := url.Values{}
	q.Set("cmd", "qryfldexe")
	q.Set("cacheName", i.config.CacheName)
	q.Set("pageSize", "1")
	q.Set("qry", statement)
	q.Set("ignite.login", i.config.Username)
	q.Set("ignite.password", i.config.Password)
	return i.doGet(ctx, q)
}

func (i *Ignite) doGet(ctx context.Context, q url.Values) error {
	base := strings.TrimRight(i.config.URL, "/")
	full := fmt.Sprintf("%s/ignite?%s", base, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	resp, err := i.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ignite REST failed: %s: %s", resp.Status, string(b))
	}
	var env struct {
		SuccessStatus int    `json:"successStatus"`
		Error         string `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err == nil && env.SuccessStatus != 0 {
		return fmt.Errorf("ignite REST: status=%d error=%s", env.SuccessStatus, env.Error)
	}
	return nil
}

// renderTemplate replaces {{key}} with bindings[key]. We deliberately do
// not use html/template here because Ignite DDL needs the values
// inserted literally (already validated by safeIdentifier/safePassword).
func renderTemplate(in string, bindings map[string]string) string {
	for k, v := range bindings {
		in = strings.ReplaceAll(in, "{{"+k+"}}", v)
	}
	return in
}

// safeIdentifier rejects identifiers that would break DDL string escaping.
func safeIdentifier(s string) error {
	if s == "" {
		return errors.New("empty identifier")
	}
	for _, r := range s {
		switch r {
		case '"', '\'', ';', '`':
			return fmt.Errorf("identifier %q contains forbidden character %q", s, r)
		}
	}
	return nil
}

// safePassword rejects passwords that would terminate the SQL string we
// build (single quote) — we don't want to introduce SQL injection in DDL.
func safePassword(p string) error {
	if p == "" {
		return errors.New("empty password")
	}
	if strings.ContainsRune(p, '\'') {
		return errors.New("password contains a single quote, which Ignite DDL can't escape safely")
	}
	return nil
}

func newHTTPClient(cfg *igniteConfig) (*http.Client, error) {
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
