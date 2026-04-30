// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// Package remotepostgres implements an OpenBao database plugin that routes all
// PostgreSQL operations through a built-in spoke server instead of dialing the
// database directly. This allows hub-side OpenBao to manage credentials for
// PostgreSQL instances that live in remote spoke clusters and are only reachable
// from within those clusters.
//
// No external grpc-agent process is needed. OpenBao embeds a spoke server
// that spoke-agent binaries (running inside spoke cluster pods) connect to.
//
// Configuration (bao write database/config/<name>):
//
//	plugin_name    = "remote-postgres-database-plugin"
//	agent_port     = "50052"             // port OpenBao listens on for spoke agents (default: 50052)
//	spoke_name     = "spoke-cluster-1"   // --name used by spoke-agent pod
//	connection_url = "postgresql://admin:pass@postgres-svc.demo.svc:5432/mydb"
package remotepostgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openbao/openbao/plugins/database/remote-postgres/internal/agentserver"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
)

const (
	remotePostgresTypeName = "remote-postgres"
	expirationFormat       = "2006-01-02 15:04:05-0700"
	defaultAgentPort       = 50052
)

// serverOnce ensures the embedded spoke server is started exactly once
// across all plugin instances in this OpenBao process.
var (
	serverOnce     sync.Once
	serverStartErr error
)

// compile-time interface check
var _ dbplugin.Database = (*RemotePostgres)(nil)

// RemotePostgres is the plugin struct. It holds the config needed to reach the
// spoke cluster and the target PostgreSQL connection string.
type RemotePostgres struct {
	agentPort     int
	spokeName     string
	connectionURL string
}

// New is the factory function called by dbplugin.ServeMultiplex.
func New() (interface{}, error) {
	db := &RemotePostgres{}
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

// secretValues returns values that should be redacted from error messages.
func (r *RemotePostgres) secretValues() map[string]string {
	return map[string]string{
		r.connectionURL: "[connection_url]",
	}
}

// Initialize is called when the admin runs:
//
//	bao write database/config/<name> plugin_name=remote-postgres-database-plugin \
//	  spoke_name=... connection_url=... [agent_port=50052]
func (r *RemotePostgres) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
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

	// Start the embedded spoke server once per OpenBao process.
	// Subsequent Initialize calls (for other database configs) reuse it.
	serverOnce.Do(func() {
		serverStartErr = agentserver.Instance().Start(agentPort)
	})
	if serverStartErr != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("failed to start embedded spoke server: %w", serverStartErr)
	}

	if req.VerifyConnection {
		if _, err := r.runOnSpoke(ctx, fmt.Sprintf(`psql %s -c "SELECT 1"`, shellQuote(r.connectionURL))); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("connection verification failed: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser creates a temporary PostgreSQL role. Called when a client requests a
// credential from OpenBao (GET /v1/database/creds/<role>).
//
// If no creation statements are configured on the role, the default statement
// creates a login role valid until the lease expiry.
func (r *RemotePostgres) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	username := generateUsername(req.UsernameConfig)
	expiration := req.Expiration.Format(expirationFormat)

	stmts := req.Statements.Commands
	if len(stmts) == 0 {
		stmts = []string{
			fmt.Sprintf(`CREATE ROLE "%s" WITH LOGIN PASSWORD '%s' VALID UNTIL '%s';`,
				username, req.Password, expiration),
		}
	}

	for _, stmt := range stmts {
		stmt = applyTemplates(stmt, map[string]string{
			"{{name}}":       username,
			"{{username}}":   username,
			"{{password}}":   req.Password,
			"{{expiration}}": expiration,
		})
		cmd := fmt.Sprintf(`psql %s -c %s`, shellQuote(r.connectionURL), shellQuote(stmt))
		if _, err := r.runOnSpoke(ctx, cmd); err != nil {
			return dbplugin.NewUserResponse{}, fmt.Errorf("failed to create user %q: %w", username, err)
		}
	}

	return dbplugin.NewUserResponse{Username: username}, nil
}

// UpdateUser rotates a password or renews the expiration of an existing role.
func (r *RemotePostgres) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Password != nil {
		stmts := req.Password.Statements.Commands
		if len(stmts) == 0 {
			stmts = []string{fmt.Sprintf(`ALTER ROLE "%s" WITH PASSWORD '%s';`, req.Username, req.Password.NewPassword)}
		}
		for _, stmt := range stmts {
			stmt = applyTemplates(stmt, map[string]string{
				"{{name}}":     req.Username,
				"{{username}}": req.Username,
				"{{password}}": req.Password.NewPassword,
			})
			cmd := fmt.Sprintf(`psql %s -c %s`, shellQuote(r.connectionURL), shellQuote(stmt))
			if _, err := r.runOnSpoke(ctx, cmd); err != nil {
				return dbplugin.UpdateUserResponse{}, fmt.Errorf("failed to update password for %q: %w", req.Username, err)
			}
		}
	}

	if req.Expiration != nil {
		expiration := req.Expiration.NewExpiration.Format(expirationFormat)
		stmts := req.Expiration.Statements.Commands
		if len(stmts) == 0 {
			stmts = []string{fmt.Sprintf(`ALTER ROLE "%s" VALID UNTIL '%s';`, req.Username, expiration)}
		}
		for _, stmt := range stmts {
			stmt = applyTemplates(stmt, map[string]string{
				"{{name}}":       req.Username,
				"{{username}}":   req.Username,
				"{{expiration}}": expiration,
			})
			cmd := fmt.Sprintf(`psql %s -c %s`, shellQuote(r.connectionURL), shellQuote(stmt))
			if _, err := r.runOnSpoke(ctx, cmd); err != nil {
				return dbplugin.UpdateUserResponse{}, fmt.Errorf("failed to update expiration for %q: %w", req.Username, err)
			}
		}
	}

	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser revokes a previously created role. Called when a lease expires or
// is explicitly revoked.
func (r *RemotePostgres) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	stmts := req.Statements.Commands
	if len(stmts) == 0 {
		// Revoke all and drop. Two statements separated by semicolon are
		// sent as a single psql invocation.
		stmts = []string{fmt.Sprintf(
			`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM "%s"; DROP ROLE IF EXISTS "%s";`,
			req.Username, req.Username,
		)}
	}

	for _, stmt := range stmts {
		stmt = applyTemplates(stmt, map[string]string{
			"{{name}}":     req.Username,
			"{{username}}": req.Username,
		})
		cmd := fmt.Sprintf(`psql %s -c %s`, shellQuote(r.connectionURL), shellQuote(stmt))
		if _, err := r.runOnSpoke(ctx, cmd); err != nil {
			return dbplugin.DeleteUserResponse{}, fmt.Errorf("failed to delete user %q: %w", req.Username, err)
		}
	}

	return dbplugin.DeleteUserResponse{}, nil
}

// Type returns the plugin type name used in OpenBao's plugin catalog.
func (r *RemotePostgres) Type() (string, error) {
	return remotePostgresTypeName, nil
}

// Close is a no-op; there is no persistent connection to close.
func (r *RemotePostgres) Close() error {
	return nil
}

// runOnSpoke sends command to the target spoke via the embedded agentserver
// and waits for the output. The spoke-agent pod (running inside the spoke
// cluster) executes the command locally where ClusterIP services are reachable.
func (r *RemotePostgres) runOnSpoke(ctx context.Context, command string) (string, error) {
	output, err := agentserver.Instance().RunCommand(ctx, r.spokeName, command)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(output, "Error:") {
		return "", fmt.Errorf("spoke %q returned error: %s", r.spokeName, output)
	}
	return output, nil
}

// generateUsername creates a short, unique PostgreSQL-safe username (<= 63 chars).
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

// shellQuote wraps s in single quotes, escaping any existing single quotes.
// This is safe for use as a shell argument passed to bash -lc.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// applyTemplates replaces all keys in replacements within stmt.
func applyTemplates(stmt string, replacements map[string]string) string {
	for k, v := range replacements {
		stmt = strings.ReplaceAll(stmt, k, v)
	}
	return stmt
}

// getConfigString extracts a required string key from an Initialize config map.
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
