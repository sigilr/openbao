// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package remotedb

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openbao/openbao/plugins/database/remote-db-plugin/internal/agentserver"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
)

const (
	expirationFormat = "2006-01-02 15:04:05-0700"
	defaultAgentPort = 50052
)

var (
	serverOnce     sync.Once
	serverStartErr error
)

type Dialect struct {
	TypeName                     string
	BuildCmd                     func(connURL, stmt string) string
	BuildVerifyCmd               func(connURL string) string
	DefaultNewUserStmts          []string
	DefaultUpdatePasswordStmts   []string
	DefaultUpdateExpirationStmts []string
	DefaultDeleteUserStmts       []string
}

type RemoteDB struct {
	dialect       Dialect
	agentPort     int
	spokeName     string
	connectionURL string
}

var _ dbplugin.Database = (*RemoteDB)(nil)

func New(dialect Dialect) func() (interface{}, error) {
	return func() (interface{}, error) {
		db := &RemoteDB{dialect: dialect}
		return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
	}
}

func (r *RemoteDB) secretValues() map[string]string {
	return map[string]string{
		r.connectionURL: "[connection_url]",
	}
}

func (r *RemoteDB) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	spokeName, err := getConfigString(req.Config, "spoke_name")
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	connectionURL, err := getConfigString(req.Config, "connection_url")
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	agentPort := defaultAgentPort
	if v, ok := req.Config["agent_port"]; ok {
		if p, ok := v.(int); ok && p > 0 {
			agentPort = p
		}
	}

	r.agentPort = agentPort
	r.spokeName = spokeName
	r.connectionURL = connectionURL

	serverOnce.Do(func() {
		serverStartErr = agentserver.Instance().Start(agentPort)
	})
	if serverStartErr != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("failed to start embedded spoke server: %w", serverStartErr)
	}

	if req.VerifyConnection && r.dialect.BuildVerifyCmd != nil {
		verifyCmd := r.dialect.BuildVerifyCmd(r.connectionURL)
		if _, err := r.runOnSpoke(ctx, verifyCmd); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("connection verification failed: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

func (r *RemoteDB) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	username := generateUsername(req.UsernameConfig)
	expiration := req.Expiration.Format(expirationFormat)

	stmts := req.Statements.Commands
	if len(stmts) == 0 {
		stmts = r.dialect.DefaultNewUserStmts
	}

	replacements := map[string]string{
		"{{name}}":       username,
		"{{username}}":   username,
		"{{password}}":   req.Password,
		"{{expiration}}": expiration,
	}

	for _, stmt := range stmts {
		cmd := r.dialect.BuildCmd(r.connectionURL, applyTemplates(stmt, replacements))
		if _, err := r.runOnSpoke(ctx, cmd); err != nil {
			return dbplugin.NewUserResponse{}, fmt.Errorf("failed to create user %q: %w", username, err)
		}
	}

	return dbplugin.NewUserResponse{Username: username}, nil
}

func (r *RemoteDB) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Password != nil {
		stmts := req.Password.Statements.Commands
		if len(stmts) == 0 {
			stmts = r.dialect.DefaultUpdatePasswordStmts
		}
		replacements := map[string]string{
			"{{name}}":     req.Username,
			"{{username}}": req.Username,
			"{{password}}": req.Password.NewPassword,
		}
		for _, stmt := range stmts {
			cmd := r.dialect.BuildCmd(r.connectionURL, applyTemplates(stmt, replacements))
			if _, err := r.runOnSpoke(ctx, cmd); err != nil {
				return dbplugin.UpdateUserResponse{}, fmt.Errorf("failed to update password for %q: %w", req.Username, err)
			}
		}
	}

	if req.Expiration != nil && len(r.dialect.DefaultUpdateExpirationStmts) > 0 {
		expiration := req.Expiration.NewExpiration.Format(expirationFormat)
		stmts := req.Expiration.Statements.Commands
		if len(stmts) == 0 {
			stmts = r.dialect.DefaultUpdateExpirationStmts
		}
		replacements := map[string]string{
			"{{name}}":       req.Username,
			"{{username}}":   req.Username,
			"{{expiration}}": expiration,
		}
		for _, stmt := range stmts {
			cmd := r.dialect.BuildCmd(r.connectionURL, applyTemplates(stmt, replacements))
			if _, err := r.runOnSpoke(ctx, cmd); err != nil {
				return dbplugin.UpdateUserResponse{}, fmt.Errorf("failed to update expiration for %q: %w", req.Username, err)
			}
		}
	}

	return dbplugin.UpdateUserResponse{}, nil
}

func (r *RemoteDB) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	stmts := req.Statements.Commands
	if len(stmts) == 0 {
		stmts = r.dialect.DefaultDeleteUserStmts
	}
	replacements := map[string]string{
		"{{name}}":     req.Username,
		"{{username}}": req.Username,
	}
	for _, stmt := range stmts {
		cmd := r.dialect.BuildCmd(r.connectionURL, applyTemplates(stmt, replacements))
		if _, err := r.runOnSpoke(ctx, cmd); err != nil {
			return dbplugin.DeleteUserResponse{}, fmt.Errorf("failed to delete user %q: %w", req.Username, err)
		}
	}

	return dbplugin.DeleteUserResponse{}, nil
}

func (r *RemoteDB) Type() (string, error) {
	return r.dialect.TypeName, nil
}

func (r *RemoteDB) Close() error {
	return nil
}

func (r *RemoteDB) runOnSpoke(ctx context.Context, command string) (string, error) {
	output, err := agentserver.Instance().RunCommand(ctx, r.spokeName, command)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(output, "Error:") {
		return "", fmt.Errorf("spoke %q returned error: %s", r.spokeName, output)
	}
	return output, nil
}

func generateUsername(cfg dbplugin.UsernameMetadata) string {
	display := truncate(cfg.DisplayName, 8)
	role := truncate(cfg.RoleName, 8)
	ts := truncate(fmt.Sprintf("%d", time.Now().UnixNano()), 12)
	return truncate(fmt.Sprintf("v-%s-%s-%s", display, role, ts), 63)
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func applyTemplates(stmt string, replacements map[string]string) string {
	for k, v := range replacements {
		stmt = strings.ReplaceAll(stmt, k, v)
	}
	return stmt
}

func getConfigString(config map[string]interface{}, key string) (string, error) {
	v, ok := config[key]
	if !ok {
		return "", fmt.Errorf("missing required config field %q", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("config field %q must be a non-empty string", key)
	}
	return s, nil
}
