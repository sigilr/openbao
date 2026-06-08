// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// Package runner dispatches incoming requests from the hub to the actual
// built-in database plugins (postgres, mysql, valkey, …) running in-process
// inside the spoke daemon.
//
// PluginRunner holds a long-lived cache of `dbplugin.Database` instances
// keyed by the hub's `instance_id`. The hub generates that id on first
// Initialize and persists it in the database mount's config; every subsequent
// NewUser/UpdateUser/DeleteUser carries it. This fixes the earlier design
// where every request ran as a one-shot subprocess: state (DB connection,
// rotated root credentials, prepared statements) is now preserved between
// calls, which is what the dbplugin v5 contract assumes.
//
// On a cache miss (spoke restart with hub still holding the id), the runner
// transparently re-Initializes from the config carried in the request so
// callers never see the difference.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	dbMySQL "github.com/openbao/openbao/plugins/database/mysql"
	dbPostgres "github.com/openbao/openbao/plugins/database/postgresql"
	dbValkey "github.com/openbao/openbao/plugins/database/valkey"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
)

// PluginRunner holds the cache of long-lived plugin instances. Safe for
// concurrent use: a sync.Mutex guards the map; once a plugin is taken out of
// the map by load(), callers operate on it without holding the runner mutex.
type PluginRunner struct {
	mu      sync.Mutex
	plugins map[string]*pluginEntry
}

type pluginEntry struct {
	pluginName string
	db         dbplugin.Database
}

func NewPluginRunner() *PluginRunner {
	return &PluginRunner{plugins: make(map[string]*pluginEntry)}
}

// ExecuteRequest is the single entry point called for every inbound request
// from the hub. It parses the JSON, dispatches on `method`, and returns the
// JSON-encoded reply.
func (r *PluginRunner) ExecuteRequest(requestJSON string) (string, error) {
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return "", fmt.Errorf("parse request: %w", err)
	}

	method, ok := req["method"].(string)
	if !ok {
		return "", fmt.Errorf("missing method")
	}
	instanceID, _ := req["instance_id"].(string)
	if instanceID == "" {
		return "", fmt.Errorf("missing instance_id")
	}
	pluginName, _ := req["plugin_name"].(string)

	ctx := context.Background()

	switch method {
	case "Initialize":
		return r.handleInitialize(ctx, instanceID, pluginName, req)
	case "NewUser":
		return r.withPlugin(ctx, instanceID, pluginName, req, r.handleNewUser)
	case "UpdateUser":
		return r.withPlugin(ctx, instanceID, pluginName, req, r.handleUpdateUser)
	case "DeleteUser":
		return r.withPlugin(ctx, instanceID, pluginName, req, r.handleDeleteUser)
	case "Close":
		return r.handleClose(ctx, instanceID)
	default:
		return "", fmt.Errorf("unknown method: %s", method)
	}
}

// --- Cache primitives -------------------------------------------------------

func (r *PluginRunner) get(instanceID string) (*pluginEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.plugins[instanceID]
	return e, ok
}

func (r *PluginRunner) put(instanceID string, entry *pluginEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.plugins[instanceID]; ok {
		// Re-Initialize for the same id: dispose of the previous instance so
		// its DB connection is released. The new one replaces it atomically.
		_ = old.db.Close()
	}
	r.plugins[instanceID] = entry
}

func (r *PluginRunner) remove(instanceID string) *pluginEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.plugins[instanceID]
	delete(r.plugins, instanceID)
	return e
}

// withPlugin loads the cached plugin for instanceID; if the cache is cold
// (e.g. spoke restarted), it lazy-inits from the config carried in the
// request. This keeps the system self-healing: hub callers don't have to
// catch and replay Initialize on cache misses.
func (r *PluginRunner) withPlugin(
	ctx context.Context,
	instanceID string,
	pluginName string,
	req map[string]interface{},
	handler func(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error),
) (string, error) {
	if entry, ok := r.get(instanceID); ok {
		return handler(ctx, entry.db, req)
	}

	cfg, _ := req["config"].(map[string]interface{})
	if cfg == nil {
		return "", fmt.Errorf("instance %s not cached and request carries no config to re-init", instanceID)
	}
	plugin, err := loadPlugin(pluginName)
	if err != nil {
		return "", err
	}
	if _, err := plugin.Initialize(ctx, dbplugin.InitializeRequest{
		Config:           cfg,
		VerifyConnection: false,
	}); err != nil {
		_ = plugin.Close()
		return "", fmt.Errorf("lazy initialize: %w", err)
	}
	r.put(instanceID, &pluginEntry{pluginName: pluginName, db: plugin})
	return handler(ctx, plugin, req)
}

// --- Plugin loader ---------------------------------------------------------

// loadPlugin creates a fresh plugin instance of the named type. We hold the
// imports here so the spoke daemon binary statically links them all.
func loadPlugin(pluginName string) (dbplugin.Database, error) {
	var factory func() (interface{}, error)
	switch pluginName {
	case "postgresql-database-plugin":
		factory = dbPostgres.New
	case "mysql-database-plugin":
		factory = dbMySQL.New(dbMySQL.DefaultUserNameTemplate)
	case "valkey-database-plugin", "redis-database-plugin":
		factory = dbValkey.New
	default:
		return nil, fmt.Errorf("unknown plugin: %s", pluginName)
	}
	raw, err := factory()
	if err != nil {
		return nil, fmt.Errorf("create plugin %s: %w", pluginName, err)
	}
	db, ok := raw.(dbplugin.Database)
	if !ok {
		return nil, fmt.Errorf("plugin %s does not implement dbplugin.Database", pluginName)
	}
	return db, nil
}

// --- Method handlers -------------------------------------------------------

func (r *PluginRunner) handleInitialize(ctx context.Context, instanceID, pluginName string, req map[string]interface{}) (string, error) {
	cfg, ok := req["config"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("missing config")
	}
	verifyConnection, _ := req["verify_connection"].(bool)

	plugin, err := loadPlugin(pluginName)
	if err != nil {
		return "", err
	}
	resp, err := plugin.Initialize(ctx, dbplugin.InitializeRequest{
		Config:           cfg,
		VerifyConnection: verifyConnection,
	})
	if err != nil {
		_ = plugin.Close()
		return "", fmt.Errorf("initialize: %w", err)
	}
	r.put(instanceID, &pluginEntry{pluginName: pluginName, db: plugin})

	out, err := json.Marshal(map[string]interface{}{"config": resp.Config})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (r *PluginRunner) handleNewUser(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
	usernameConfig, ok := req["username_config"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("missing username_config")
	}
	password, _ := req["password"].(string)
	expiration, err := asInt64(req["expiration"])
	if err != nil {
		return "", fmt.Errorf("expiration: %w", err)
	}
	statements := stringSlice(req["statements"])

	resp, err := plugin.NewUser(ctx, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{
			DisplayName: stringField(usernameConfig, "display_name"),
			RoleName:    stringField(usernameConfig, "role_name"),
		},
		Password:   password,
		Expiration: time.Unix(expiration, 0),
		Statements: dbplugin.Statements{Commands: statements},
	})
	if err != nil {
		return "", fmt.Errorf("NewUser: %w", err)
	}
	out, err := json.Marshal(map[string]interface{}{"username": resp.Username})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (r *PluginRunner) handleUpdateUser(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
	username, ok := req["username"].(string)
	if !ok {
		return "", fmt.Errorf("missing username")
	}
	update := dbplugin.UpdateUserRequest{Username: username}

	if pw, ok := req["password"].(map[string]interface{}); ok {
		update.Password = &dbplugin.ChangePassword{
			NewPassword: stringField(pw, "new_password"),
			Statements:  dbplugin.Statements{Commands: stringSlice(pw["statements"])},
		}
	}
	if ex, ok := req["expiration"].(map[string]interface{}); ok {
		newExp, err := asInt64(ex["new_expiration"])
		if err != nil {
			return "", fmt.Errorf("expiration.new_expiration: %w", err)
		}
		update.Expiration = &dbplugin.ChangeExpiration{
			NewExpiration: time.Unix(newExp, 0),
			Statements:    dbplugin.Statements{Commands: stringSlice(ex["statements"])},
		}
	}
	if _, err := plugin.UpdateUser(ctx, update); err != nil {
		return "", fmt.Errorf("UpdateUser: %w", err)
	}
	return "{}", nil
}

func (r *PluginRunner) handleDeleteUser(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
	username, ok := req["username"].(string)
	if !ok {
		return "", fmt.Errorf("missing username")
	}
	if _, err := plugin.DeleteUser(ctx, dbplugin.DeleteUserRequest{
		Username:   username,
		Statements: dbplugin.Statements{Commands: stringSlice(req["statements"])},
	}); err != nil {
		return "", fmt.Errorf("DeleteUser: %w", err)
	}
	return "{}", nil
}

func (r *PluginRunner) handleClose(_ context.Context, instanceID string) (string, error) {
	entry := r.remove(instanceID)
	if entry == nil {
		// Idempotent: closing an unknown id is fine; hub may have lost track
		// after a spoke restart.
		return "{}", nil
	}
	if err := entry.db.Close(); err != nil {
		return "", fmt.Errorf("close: %w", err)
	}
	return "{}", nil
}

// --- Decode helpers --------------------------------------------------------

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func stringSlice(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// asInt64 coerces a JSON-decoded number to int64. encoding/json gives us
// float64 by default; json.Number is used when the decoder is configured for
// precise numbers. Cover both so this code works regardless of the upstream
// decode mode.
func asInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case nil:
		return 0, fmt.Errorf("nil")
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}
