// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package documentdb implements an OpenBao v5 database plugin for the
// open-source DocumentDB engine ( https://github.com/documentdb/documentdb ).
// DocumentDB is a PostgreSQL extension that exposes a MongoDB-compatible
// wire protocol via a gateway process (the same engine that powers Azure
// Cosmos DB for MongoDB vCore). Clients connect with any MongoDB driver,
// so this plugin uses the official mongo-driver.
//
// Defaults differ from the AWS-managed offering of a similar name:
//   - Retryable writes are not forced off — the OSS engine supports them.
//   - TLS is not mandated, but the upstream docker quickstart enables
//     TLS with a self-signed cert; pair `tls_ca` / `tls_ca_path` (or the
//     `insecure` flag for dev clusters) with `?tls=true` in the
//     connection_url when running against that setup.
package documentdb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/database/helper/connutil"
	"github.com/openbao/openbao/sdk/v2/database/helper/dbutil"
	"github.com/openbao/openbao/sdk/v2/helper/template"
	"github.com/openbao/openbao/sdk/v2/logical"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
)

const (
	docDBTypeName = "documentdb"

	defaultUserNameTemplate = `{{ printf "v-%s-%s-%s-%s" (.DisplayName | truncate 15) (.RoleName | truncate 15) (random 20) (unix_time) | replace "." "-" | truncate 100 }}`
)

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// DocumentDB implements dbplugin.Database. Creation statements are JSON
// role documents: `{"db":"admin", "roles":[{"role":"readWrite"}, ...]}`.
type DocumentDB struct {
	cp *docDBConnectionProducer

	usernameProducer template.StringTemplate
}

type docDBConnectionProducer struct {
	ConnectionURL string `mapstructure:"connection_url"`
	Username      string `mapstructure:"username"`
	Password      string `mapstructure:"password"`

	// TLSCAData / TLSCAPath load a custom CA bundle when the gateway
	// presents a non-publicly-trusted certificate (e.g. the upstream
	// docker quickstart issues a self-signed cert).
	TLSCAData []byte `mapstructure:"tls_ca"`
	TLSCAPath string `mapstructure:"tls_ca_path"`
	// Insecure skips TLS verification. Intended for development clusters
	// only — pairs with the upstream docker quickstart's self-signed
	// gateway cert. Do not use in production.
	Insecure bool `mapstructure:"insecure"`

	ConnectTimeout         time.Duration `mapstructure:"connect_timeout"`
	SocketTimeout          time.Duration `mapstructure:"socket_timeout"`
	ServerSelectionTimeout time.Duration `mapstructure:"server_selection_timeout"`

	Initialized   bool
	RawConfig     map[string]interface{}
	clientOptions *options.ClientOptions
	client        *mongo.Client
	sync.Mutex
}

var (
	_ dbplugin.Database       = (*DocumentDB)(nil)
	_ logical.PluginVersioner = (*DocumentDB)(nil)
)

func New() (interface{}, error) {
	db := newDocumentDB()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newDocumentDB() *DocumentDB {
	return &DocumentDB{cp: &docDBConnectionProducer{}}
}

func (d *DocumentDB) secretValues() map[string]string {
	return map[string]string{d.cp.Password: "[password]"}
}

func (d *DocumentDB) Type() (string, error) {
	return docDBTypeName, nil
}

func (d *DocumentDB) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (d *DocumentDB) Close() error {
	return d.cp.Close()
}

func (d *DocumentDB) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	d.cp.Lock()
	defer d.cp.Unlock()

	d.cp.RawConfig = req.Config
	if err := mapstructure.WeakDecode(req.Config, d.cp); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if d.cp.ConnectionURL == "" {
		return dbplugin.InitializeResponse{}, errors.New("connection_url cannot be empty")
	}

	usernameTemplate, err := strutil.GetString(req.Config, "username_template")
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("failed to retrieve username_template: %w", err)
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
	d.usernameProducer = up

	opts, err := d.cp.makeClientOpts()
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	d.cp.clientOptions = opts
	d.cp.Initialized = true

	if req.VerifyConnection {
		client, err := d.cp.createClient(ctx)
		if err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
		if err := client.Ping(ctx, readpref.Primary()); err != nil {
			_ = client.Disconnect(ctx)
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
		d.cp.client = client
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser parses the JSON role doc and runs createUser on the named db.
func (d *DocumentDB) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	if len(req.Statements.Commands) == 0 {
		return dbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	username, err := d.usernameProducer.Generate(req.UsernameConfig)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	var stmt docDBStatement
	if err := json.Unmarshal([]byte(req.Statements.Commands[0]), &stmt); err != nil {
		return dbplugin.NewUserResponse{}, err
	}
	if stmt.DB == "" {
		stmt.DB = "admin"
	}
	if len(stmt.Roles) == 0 {
		return dbplugin.NewUserResponse{}, errors.New("roles array is required in creation statement")
	}

	cmd := createUserCommand{
		Username: username,
		Password: req.Password,
		Roles:    stmt.Roles.toStandardRolesArray(),
	}
	if err := d.runCommandWithRetry(ctx, stmt.DB, cmd); err != nil {
		return dbplugin.NewUserResponse{}, err
	}
	return dbplugin.NewUserResponse{Username: username}, nil
}

func (d *DocumentDB) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Password == nil {
		return dbplugin.UpdateUserResponse{}, nil
	}
	return dbplugin.UpdateUserResponse{}, d.changeUserPassword(ctx, req.Username, req.Password.NewPassword)
}

func (d *DocumentDB) changeUserPassword(ctx context.Context, username, password string) error {
	cs, err := connstring.Parse(d.cp.getURL())
	if err != nil {
		return err
	}
	database := cs.Database
	if username == d.cp.Username || database == "" {
		database = "admin"
	}
	return d.runCommandWithRetry(ctx, database, &updateUserCommand{
		Username: username, Password: password,
	})
}

func (d *DocumentDB) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	stmt := docDBStatement{}
	if len(req.Statements.Commands) == 1 {
		if err := json.Unmarshal([]byte(req.Statements.Commands[0]), &stmt); err != nil {
			return dbplugin.DeleteUserResponse{}, err
		}
	} else if len(req.Statements.Commands) > 1 {
		return dbplugin.DeleteUserResponse{}, fmt.Errorf("expected 0 or 1 revocation statements, got %d", len(req.Statements.Commands))
	}
	if stmt.DB == "" {
		stmt.DB = "admin"
	}
	err := d.runCommandWithRetry(ctx, stmt.DB, &dropUserCommand{
		Username:     req.Username,
		WriteConcern: writeconcern.Majority(),
	})
	var cErr mongo.CommandError
	if errors.As(err, &cErr) && cErr.Name == "UserNotFound" {
		log.Default().Warn("DocumentDB user was deleted prior to lease revocation", "user", req.Username)
		return dbplugin.DeleteUserResponse{}, nil
	}
	return dbplugin.DeleteUserResponse{}, err
}

func (d *DocumentDB) runCommandWithRetry(ctx context.Context, db string, cmd interface{}) error {
	client, err := d.cp.Connection(ctx)
	if err != nil {
		return err
	}
	result := client.Database(db).RunCommand(ctx, cmd, nil)
	err = result.Err()
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF") {
		client, err = d.cp.Connection(ctx)
		if err != nil {
			return err
		}
		return client.Database(db).RunCommand(ctx, cmd, nil).Err()
	}
	return err
}

// ---- connection producer ---------------------------------------------------

// getURL templates {{username}}/{{password}} into the connection URL.
func (c *docDBConnectionProducer) getURL() string {
	return dbutil.QueryHelper(c.ConnectionURL, map[string]string{
		"username": c.Username,
		"password": c.Password,
	})
}

func (c *docDBConnectionProducer) Connection(ctx context.Context) (*mongo.Client, error) {
	if !c.Initialized {
		return nil, connutil.ErrNotInitialized
	}
	c.Lock()
	defer c.Unlock()

	if c.client != nil {
		if err := c.client.Ping(ctx, readpref.Primary()); err == nil {
			return c.client, nil
		}
		_ = c.client.Disconnect(ctx)
	}
	client, err := c.createClient(ctx)
	if err != nil {
		return nil, err
	}
	c.client = client
	return client, nil
}

func (c *docDBConnectionProducer) createClient(ctx context.Context) (*mongo.Client, error) {
	if c.clientOptions == nil {
		return nil, errors.New("missing client options")
	}
	urlStr := c.getURL()
	return mongo.Connect(ctx, options.MergeClientOptions(options.Client().ApplyURI(urlStr), c.clientOptions))
}

func (c *docDBConnectionProducer) Close() error {
	c.Lock()
	defer c.Unlock()
	if c.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.client.Disconnect(ctx); err != nil {
			return err
		}
	}
	c.client = nil
	return nil
}

func (c *docDBConnectionProducer) makeClientOpts() (*options.ClientOptions, error) {
	opts := options.Client()

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.Insecure,
	}
	pool := x509.NewCertPool()
	loaded := false
	if len(c.TLSCAData) > 0 {
		if !pool.AppendCertsFromPEM(c.TLSCAData) {
			return nil, errors.New("failed to parse tls_ca PEM")
		}
		loaded = true
	}
	if c.TLSCAPath != "" {
		pem, err := os.ReadFile(c.TLSCAPath)
		if err != nil {
			return nil, fmt.Errorf("read tls_ca_path: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("failed to parse tls_ca_path PEM")
		}
		loaded = true
	}
	if loaded {
		tlsCfg.RootCAs = pool
	}
	opts.SetTLSConfig(tlsCfg)

	if c.SocketTimeout == 0 {
		opts.SetSocketTimeout(time.Minute)
	} else {
		opts.SetSocketTimeout(c.SocketTimeout)
	}
	if c.ConnectTimeout == 0 {
		opts.SetConnectTimeout(time.Minute)
	} else {
		opts.SetConnectTimeout(c.ConnectTimeout)
	}
	if c.ServerSelectionTimeout > 0 {
		opts.SetServerSelectionTimeout(c.ServerSelectionTimeout)
	}

	// The open-source documentdb engine supports retryable writes (the
	// PostgreSQL backend handles retry semantics). Leave the driver's
	// default in place; operators who hit issues against an older
	// gateway can disable retries via `?retryWrites=false` in
	// connection_url.

	return opts, nil
}

// ---- commands --------------------------------------------------------------

type createUserCommand struct {
	Username string        `bson:"createUser"`
	Password string        `bson:"pwd,omitempty"`
	Roles    []interface{} `bson:"roles"`
}

type updateUserCommand struct {
	Username string `bson:"updateUser"`
	Password string `bson:"pwd"`
}

type dropUserCommand struct {
	Username     string                     `bson:"dropUser"`
	WriteConcern *writeconcern.WriteConcern `bson:"writeConcern"`
}

type docDBRole struct {
	Role string `json:"role" bson:"role"`
	DB   string `json:"db"   bson:"db"`
}

type docDBRoles []docDBRole

type docDBStatement struct {
	DB    string     `json:"db"`
	Roles docDBRoles `json:"roles"`
}

func (roles docDBRoles) toStandardRolesArray() []interface{} {
	var out []interface{}
	for _, role := range roles {
		if role.DB == "" {
			out = append(out, role.Role)
		} else {
			out = append(out, role)
		}
	}
	return out
}
