// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	dbMySQL "github.com/openbao/openbao/plugins/database/mysql"
	dbPostgres "github.com/openbao/openbao/plugins/database/postgresql"
	dbValkey "github.com/openbao/openbao/plugins/database/valkey"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
)

// PluginRunner loads and executes built-in database plugins
type PluginRunner struct{}

func NewPluginRunner() *PluginRunner {
	return &PluginRunner{}
}

// LoadPlugin creates an instance of the specified plugin
func (r *PluginRunner) LoadPlugin(pluginName string) (dbplugin.Database, error) {
	// Always create a new plugin instance since plugin-runner runs as one-shot
	// and doesn't maintain state between invocations
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

	pluginInterface, err := factory()
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin %s: %w", pluginName, err)
	}

	plugin, ok := pluginInterface.(dbplugin.Database)
	if !ok {
		return nil, fmt.Errorf("plugin %s does not implement Database interface", pluginName)
	}

	return plugin, nil
}

// ExecuteRequest handles a plugin method call
func (r *PluginRunner) ExecuteRequest(requestJSON string) (string, error) {
	// Parse request
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return "", fmt.Errorf("failed to parse request: %w", err)
	}

	pluginName, ok := req["plugin_name"].(string)
	if !ok {
		return "", fmt.Errorf("missing plugin_name")
	}

	method, ok := req["method"].(string)
	if !ok {
		return "", fmt.Errorf("missing method")
	}

	// Load plugin
	plugin, err := r.LoadPlugin(pluginName)
	if err != nil {
		return "", err
	}

	ctx := context.Background()

	// Execute method
	switch method {
	case "Initialize":
		return r.handleInitialize(ctx, plugin, req)
	case "NewUser":
		return r.handleNewUser(ctx, plugin, req)
	case "UpdateUser":
		return r.handleUpdateUser(ctx, plugin, req)
	case "DeleteUser":
		return r.handleDeleteUser(ctx, plugin, req)
	default:
		return "", fmt.Errorf("unknown method: %s", method)
	}
}

func (r *PluginRunner) handleInitialize(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
	config, ok := req["config"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("missing config")
	}

	verifyConnection, _ := req["verify_connection"].(bool)

	initReq := dbplugin.InitializeRequest{
		Config:           config,
		VerifyConnection: verifyConnection,
	}

	resp, err := plugin.Initialize(ctx, initReq)
	if err != nil {
		return "", fmt.Errorf("Initialize failed: %w", err)
	}

	result := map[string]interface{}{
		"config": resp.Config,
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to serialize response: %w", err)
	}

	return string(resultJSON), nil
}

func (r *PluginRunner) handleNewUser(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
	// Initialize plugin if config is provided (needed because plugin-runner runs as one-shot)
	if config, ok := req["config"].(map[string]interface{}); ok {
		initReq := dbplugin.InitializeRequest{
			Config:           config,
			VerifyConnection: false,
		}
		if _, err := plugin.Initialize(ctx, initReq); err != nil {
			return "", fmt.Errorf("failed to initialize plugin: %w", err)
		}
	}

	usernameConfig, ok := req["username_config"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("missing username_config")
	}

	password, ok := req["password"].(string)
	if !ok {
		return "", fmt.Errorf("missing password")
	}

	expirationUnix, ok := req["expiration"].(float64)
	if !ok {
		return "", fmt.Errorf("missing expiration")
	}

	statements, _ := req["statements"].([]interface{})
	stmtStrings := make([]string, 0, len(statements))
	for _, stmt := range statements {
		if s, ok := stmt.(string); ok {
			stmtStrings = append(stmtStrings, s)
		}
	}

	newUserReq := dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{
			DisplayName: getString(usernameConfig, "display_name"),
			RoleName:    getString(usernameConfig, "role_name"),
		},
		Password:   password,
		Expiration: time.Unix(int64(expirationUnix), 0),
		Statements: dbplugin.Statements{
			Commands: stmtStrings,
		},
	}

	resp, err := plugin.NewUser(ctx, newUserReq)
	if err != nil {
		return "", fmt.Errorf("NewUser failed: %w", err)
	}

	result := map[string]interface{}{
		"username": resp.Username,
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to serialize response: %w", err)
	}

	return string(resultJSON), nil
}

func (r *PluginRunner) handleUpdateUser(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
	// Initialize plugin if config is provided
	if config, ok := req["config"].(map[string]interface{}); ok {
		initReq := dbplugin.InitializeRequest{
			Config:           config,
			VerifyConnection: false,
		}
		if _, err := plugin.Initialize(ctx, initReq); err != nil {
			return "", fmt.Errorf("failed to initialize plugin: %w", err)
		}
	}

	username, ok := req["username"].(string)
	if !ok {
		return "", fmt.Errorf("missing username")
	}

	updateReq := dbplugin.UpdateUserRequest{
		Username: username,
	}

	if passwordData, ok := req["password"].(map[string]interface{}); ok {
		newPassword := getString(passwordData, "new_password")
		statements := getStringSlice(passwordData, "statements")
		
		updateReq.Password = &dbplugin.ChangePassword{
			NewPassword: newPassword,
			Statements: dbplugin.Statements{
				Commands: statements,
			},
		}
	}

	if expirationData, ok := req["expiration"].(map[string]interface{}); ok {
		newExpirationUnix, _ := expirationData["new_expiration"].(float64)
		statements := getStringSlice(expirationData, "statements")
		
		updateReq.Expiration = &dbplugin.ChangeExpiration{
			NewExpiration: time.Unix(int64(newExpirationUnix), 0),
			Statements: dbplugin.Statements{
				Commands: statements,
			},
		}
	}

	_, err := plugin.UpdateUser(ctx, updateReq)
	if err != nil {
		return "", fmt.Errorf("UpdateUser failed: %w", err)
	}

	return "{}", nil
}

func (r *PluginRunner) handleDeleteUser(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
	// Initialize plugin if config is provided
	if config, ok := req["config"].(map[string]interface{}); ok {
		initReq := dbplugin.InitializeRequest{
			Config:           config,
			VerifyConnection: false,
		}
		if _, err := plugin.Initialize(ctx, initReq); err != nil {
			return "", fmt.Errorf("failed to initialize plugin: %w", err)
		}
	}

	username, ok := req["username"].(string)
	if !ok {
		return "", fmt.Errorf("missing username")
	}

	statements := getStringSlice(req, "statements")

	deleteReq := dbplugin.DeleteUserRequest{
		Username: username,
		Statements: dbplugin.Statements{
			Commands: statements,
		},
	}

	_, err := plugin.DeleteUser(ctx, deleteReq)
	if err != nil {
		return "", fmt.Errorf("DeleteUser failed: %w", err)
	}

	return "{}", nil
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getStringSlice(m map[string]interface{}, key string) []string {
	if arr, ok := m[key].([]interface{}); ok {
		result := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}


