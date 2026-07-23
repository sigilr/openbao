// Copyright (c) HashiCorp, Inc.
// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/database/helper/dbutil"
	"github.com/openbao/openbao/sdk/v2/helper/template"
	"github.com/openbao/openbao/sdk/v2/logical"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
)

const (
	mongoDBTypeName = "mongodb"

	defaultUserNameTemplate = `{{ printf "v-%s-%s-%s-%s" (.DisplayName | truncate 15) (.RoleName | truncate 15) (random 20) (unix_time) | replace "." "-" | truncate 100 }}`
)

// ReportedVersion is overridable at build time so the plugin version surfaces
// in plugin-info responses. Matches the pattern other built-in DB plugins use.
var ReportedVersion = ""

// MongoDB implements the v5 dbplugin.Database contract by talking directly
// to a MongoDB cluster over the official Go driver. NewUser / DeleteUser are
// MongoDB role documents (JSON in `creation_statements`), not SQL.
type MongoDB struct {
	*mongoDBConnectionProducer

	usernameProducer template.StringTemplate
}

var (
	_ dbplugin.Database       = (*MongoDB)(nil)
	_ logical.PluginVersioner = (*MongoDB)(nil)
)

// New is the factory entry point referenced from helper/builtinplugins/registry.go.
func New() (interface{}, error) {
	db := newMongoDB()
	dbType := dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues)
	return dbType, nil
}

func newMongoDB() *MongoDB {
	connProducer := &mongoDBConnectionProducer{
		Type: mongoDBTypeName,
	}
	return &MongoDB{mongoDBConnectionProducer: connProducer}
}

// Type returns the TypeName for this backend.
func (m *MongoDB) Type() (string, error) {
	return mongoDBTypeName, nil
}

func (m *MongoDB) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (m *MongoDB) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	m.Lock()
	defer m.Unlock()

	m.RawConfig = req.Config

	usernameTemplate, err := strutil.GetString(req.Config, "username_template")
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("failed to retrieve username_template: %w", err)
	}
	if usernameTemplate == "" {
		usernameTemplate = defaultUserNameTemplate
	}

	up, err := template.NewTemplate(template.Template(usernameTemplate))
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("unable to initialize username template: %w", err)
	}
	m.usernameProducer = up

	if _, err := m.usernameProducer.Generate(dbplugin.UsernameMetadata{}); err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("invalid username template: %w", err)
	}

	if err := m.loadConfig(req.Config); err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	// Mark initialized once config is valid; actual connection is opened
	// lazily on the first request unless VerifyConnection is set.
	m.Initialized = true

	if req.VerifyConnection {
		client, err := m.createClient(ctx)
		if err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}

		if err := client.Ping(ctx, readpref.Primary()); err != nil {
			_ = client.Disconnect(ctx)
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
		m.client = client
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser parses Commands[0] as a {db, roles} JSON document and runs a
// createUser command against the named database. If no db is given,
// defaults to "admin".
func (m *MongoDB) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	if len(req.Statements.Commands) == 0 {
		return dbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	username, err := m.usernameProducer.Generate(req.UsernameConfig)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	var mongoCS mongoDBStatement
	if err := json.Unmarshal([]byte(req.Statements.Commands[0]), &mongoCS); err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	if mongoCS.DB == "" {
		mongoCS.DB = "admin"
	}

	if len(mongoCS.Roles) == 0 {
		return dbplugin.NewUserResponse{}, errors.New("roles array is required in creation statement")
	}

	createUserCmd := createUserCommand{
		Username: username,
		Password: req.Password,
		Roles:    mongoCS.Roles.toStandardRolesArray(),
	}

	if err := m.runCommandWithRetry(ctx, mongoCS.DB, createUserCmd); err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	return dbplugin.NewUserResponse{Username: username}, nil
}

// UpdateUser supports password change only — MongoDB doesn't have a native
// VALID UNTIL on users, so Expiration is enforced by OpenBao leases plus the
// DeleteUser revoke path.
func (m *MongoDB) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Password != nil {
		return dbplugin.UpdateUserResponse{}, m.changeUserPassword(ctx, req.Username, req.Password.NewPassword)
	}
	return dbplugin.UpdateUserResponse{}, nil
}

func (m *MongoDB) changeUserPassword(ctx context.Context, username, password string) error {
	connURL := m.getConnectionURL()
	cs, err := connstring.Parse(connURL)
	if err != nil {
		return err
	}

	// Root-rotation passes m.Username; the user lives in the "admin" db. For
	// dynamic users we update them in the database carried by the connection
	// URL, falling back to "admin" if none is set.
	database := cs.Database
	if username == m.Username || database == "" {
		database = "admin"
	}

	return m.runCommandWithRetry(ctx, database, &updateUserCommand{
		Username: username,
		Password: password,
	})
}

// DeleteUser drops the user via the dropUser command. revocation_statements,
// if provided, follow the same {db, ...} JSON shape used by creation; the
// db is the authentication database, defaulting to "admin".
func (m *MongoDB) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	var revocationStatement string
	switch len(req.Statements.Commands) {
	case 0:
		revocationStatement = `{}`
	case 1:
		revocationStatement = req.Statements.Commands[0]
	default:
		return dbplugin.DeleteUserResponse{}, fmt.Errorf("expected 0 or 1 revocation statements, got %d", len(req.Statements.Commands))
	}

	var mongoCS mongoDBStatement
	if err := json.Unmarshal([]byte(revocationStatement), &mongoCS); err != nil {
		return dbplugin.DeleteUserResponse{}, err
	}

	db := mongoCS.DB
	if db == "" {
		db = "admin"
	}

	wc := writeconcern.Majority()
	opts, err := m.getWriteConcern()
	if err != nil {
		return dbplugin.DeleteUserResponse{}, err
	}
	if opts != nil {
		wc = opts.WriteConcern
	}

	dropUserCmd := &dropUserCommand{
		Username:     req.Username,
		WriteConcern: wc,
	}

	err = m.runCommandWithRetry(ctx, db, dropUserCmd)
	// If the user is already gone, treat as a successful revoke — Vault may
	// race with manual cleanup and we don't want a retry storm.
	var cErr mongo.CommandError
	if errors.As(err, &cErr) && cErr.Name == "UserNotFound" {
		log.Default().Warn("MongoDB user was deleted prior to lease revocation", "user", req.Username)
		return dbplugin.DeleteUserResponse{}, nil
	}

	return dbplugin.DeleteUserResponse{}, err
}

// runCommandWithRetry retries once on EOF — long-lived mongo connections
// occasionally drop and the driver surfaces this as an io.EOF on the first
// attempt; we reconnect and retry transparently.
func (m *MongoDB) runCommandWithRetry(ctx context.Context, db string, cmd interface{}) error {
	client, err := m.Connection(ctx)
	if err != nil {
		return err
	}

	result := client.Database(db).RunCommand(ctx, cmd, nil)
	err = result.Err()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF"):
		client, err = m.Connection(ctx)
		if err != nil {
			return err
		}
		result = client.Database(db).RunCommand(ctx, cmd, nil)
		return result.Err()
	default:
		return err
	}
}
