// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package valkey

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/database/helper/credsutil"
	"github.com/valkey-io/valkey-go"
)

const (
	valkeyTypeName        = "valkey"
	defaultValkeyUserRule = `["~*", "+@read"]`
	defaultTimeout        = 20000 * time.Millisecond
	maxKeyLength          = 64
)

var _ dbplugin.Database = &ValkeyDB{}

// Type that combines the custom plugins Valkey database connection configuration options and the Vault CredentialsProducer
// used for generating user information for the Valkey database.
type ValkeyDB struct {
	*valkeyDBConnectionProducer
	credsutil.CredentialsProducer
}

// New implements builtinplugins.BuiltinFactory
func New() (any, error) {
	db := new()
	// Wrap the plugin with middleware to sanitize errors
	dbType := dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues)
	return dbType, nil
}

func new() *ValkeyDB {
	connProducer := &valkeyDBConnectionProducer{}
	connProducer.Type = valkeyTypeName

	db := &ValkeyDB{
		valkeyDBConnectionProducer: connProducer,
	}

	return db
}

func (c *ValkeyDB) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	err := c.valkeyDBConnectionProducer.Initialize(ctx, req.Config, req.VerifyConnection)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	resp := dbplugin.InitializeResponse{
		Config: req.Config,
	}
	return resp, nil
}

func (c *ValkeyDB) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	// Grab the lock
	c.Lock()
	defer c.Unlock()

	username, err := credsutil.GenerateUsername(
		credsutil.DisplayName(req.UsernameConfig.DisplayName, maxKeyLength),
		credsutil.RoleName(req.UsernameConfig.RoleName, maxKeyLength),
	)
	if err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("failed to generate username: %w", err)
	}
	username = strings.ToUpper(username)

	db, err := c.getConnection(ctx)
	if err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("failed to get connection: %w", err)
	}

	err = newUser(ctx, db, username, req)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	resp := dbplugin.NewUserResponse{
		Username: username,
	}

	return resp, nil
}

func (c *ValkeyDB) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Password != nil {
		err := c.changeUserPassword(ctx, req.Username, req.Password.NewPassword)
		return dbplugin.UpdateUserResponse{}, err
	}
	return dbplugin.UpdateUserResponse{}, nil
}

func (c *ValkeyDB) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	c.Lock()
	defer c.Unlock()

	db, err := c.getConnection(ctx)
	if err != nil {
		return dbplugin.DeleteUserResponse{}, fmt.Errorf("failed to make connection: %w", err)
	}

	// Close the database connection to ensure no new connections come in
	defer func() {
		if err := c.close(); err != nil {
			logger := hclog.New(&hclog.LoggerOptions{})
			logger.Error("defer close failed", "error", err)
		}
	}()

	cmd := db.B().AclDeluser().Username(req.Username).Build()
	if err := db.Do(ctx, cmd).Error(); err != nil {
		return dbplugin.DeleteUserResponse{}, err
	}

	return dbplugin.DeleteUserResponse{}, nil
}

func newUser(ctx context.Context, db valkey.Client, username string, req dbplugin.NewUserRequest) error {
	statements := removeEmpty(req.Statements.Commands)
	if len(statements) == 0 {
		statements = append(statements, defaultValkeyUserRule)
	}

	logger := hclog.New(&hclog.LoggerOptions{})
	var aclRules []string

	// Try to unmarshal the first statement as JSON array
	// If it fails, assume it's a raw string array of ACL rules
	err := json.Unmarshal([]byte(statements[0]), &aclRules)
	if err != nil {
		logger.Warn("Failed to unmarshal creation statements as JSON; applying as a raw string array.", "error", err)
		aclRules = statements
	}

	tokens := append([]string{"ACL", "SETUSER", username, "ON", ">" + req.Password}, aclRules...)
	cmd := db.B().Arbitrary(tokens...).Build()

	return db.Do(ctx, cmd).Error()
}

func (c *ValkeyDB) changeUserPassword(ctx context.Context, username, password string) error {
	c.Lock()
	defer c.Unlock()

	db, err := c.getConnection(ctx)
	if err != nil {
		return err
	}

	// Close the database connection to ensure no new connections come in
	defer func() {
		if err := c.close(); err != nil {
			logger := hclog.New(&hclog.LoggerOptions{})
			logger.Error("defer close failed", "error", err)
		}
	}()

	getCmd := db.B().AclGetuser().Username(username).Build()
	res := db.Do(ctx, getCmd)
	if err := res.Error(); err != nil {
		return fmt.Errorf("reset of passwords for user %s failed in changeUserPassword: %w", username, err)
	}

	msg, err := res.ToMessage()
	if err != nil {
		return fmt.Errorf("reset of passwords for user %s failed in changeUserPassword: %w", username, err)
	}
	if msg.IsNil() {
		return fmt.Errorf("changeUserPassword for user %s failed: user not found", username)
	}

	setCmd := db.B().AclSetuser().Username(username).Rule("RESETPASS", ">"+password).Build()
	if err := db.Do(ctx, setCmd).Error(); err != nil {
		return err
	}

	return nil
}

func removeEmpty(strs []string) []string {
	var newStrs []string
	for _, str := range strs {
		str = strings.TrimSpace(str)
		if str == "" {
			continue
		}
		newStrs = append(newStrs, str)
	}

	return newStrs
}

func computeTimeout(ctx context.Context) (timeout time.Duration) {
	deadline, ok := ctx.Deadline()
	if ok {
		return time.Until(deadline)
	}
	return defaultTimeout
}

func (c *ValkeyDB) getConnection(ctx context.Context) (valkey.Client, error) {
	db, err := c.Connection(ctx)
	if err != nil {
		return nil, err
	}
	return db.(valkey.Client), nil
}

func (c *ValkeyDB) Type() (string, error) {
	return valkeyTypeName, nil
}
