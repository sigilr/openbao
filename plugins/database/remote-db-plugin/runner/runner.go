// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

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
	"log"
	"sync"
	"sync/atomic"
	"time"

	dbMySQL "github.com/openbao/openbao/plugins/database/mysql"
	dbPostgres "github.com/openbao/openbao/plugins/database/postgresql"
	dbValkey "github.com/openbao/openbao/plugins/database/valkey"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
)

// PluginRunner holds the cache of long-lived plugin instances. Safe for
// concurrent use: r.mu guards `plugins` and `loading`; once a plugin is taken
// out of `plugins` by get/load, callers operate on it without holding r.mu.
//
// `loading` carries a per-instance-id mutex used to single-flight cold-cache
// loads — without it, two concurrent requests for the same id can both call
// Initialize then put(), and the second put() closes the first plugin's DB
// connection underneath whatever handler is still using it.
type PluginRunner struct {
	mu      sync.Mutex
	plugins map[string]*pluginEntry
	loading map[string]*sync.Mutex

	idleTTL       time.Duration // 0 disables idle eviction
	evictorOnce   sync.Once
	evictorActive bool // set under evictorOnce so tests can detect
}

type pluginEntry struct {
	pluginName string
	db         dbplugin.Database
	lastUsed   time.Time

	// refs is the count of in-flight handlers currently using db. evictIdle
	// only evicts entries with refs == 0; without this an entry whose
	// handler holds entry.db longer than idleTTL would race with Close.
	// Bumped under r.mu by withPlugin around the handler call.
	refs atomic.Int32
}

// DefaultIdleTTL is the period of inactivity after which a cached plugin
// instance is closed and removed. Catches the case where the hub forgot to
// send a Close (mount deletion while the spoke was offline, hub crash that
// lost track of the instance_id, ...).
const DefaultIdleTTL = 24 * time.Hour

func NewPluginRunner() *PluginRunner {
	return NewPluginRunnerWithTTL(DefaultIdleTTL)
}

// NewPluginRunnerWithTTL constructs a runner with a custom idle TTL. Set
// idleTTL to 0 to disable eviction (useful for tests).
func NewPluginRunnerWithTTL(idleTTL time.Duration) *PluginRunner {
	return &PluginRunner{
		plugins: make(map[string]*pluginEntry),
		loading: make(map[string]*sync.Mutex),
		idleTTL: idleTTL,
	}
}

// StartIdleEvictor launches a background goroutine that closes plugins whose
// lastUsed is older than idleTTL. Cancellable via ctx; the goroutine returns
// when ctx is done. Idempotent — calling it more than once is a no-op (only
// the first call spawns an evictor).
func (r *PluginRunner) StartIdleEvictor(ctx context.Context) {
	if r.idleTTL <= 0 {
		return
	}
	r.evictorOnce.Do(func() {
		r.evictorActive = true
		go func() {
			// Check at roughly 1/4 the TTL so an idle entry is evicted within
			// a reasonable window past the deadline, without thrashing on a
			// short TTL.
			tick := r.idleTTL / 4
			if tick < time.Minute {
				tick = time.Minute
			}
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					r.evictIdle(now)
				}
			}
		}()
	})
}

func (r *PluginRunner) evictIdle(now time.Time) {
	if r.idleTTL <= 0 {
		return
	}
	type evicted struct {
		id    string
		entry *pluginEntry
	}
	r.mu.Lock()
	var toClose []evicted
	for id, e := range r.plugins {
		if e.refs.Load() > 0 {
			// In-flight handler. Skip — it'll bump lastUsed when it
			// releases, or stay long enough to be picked up next tick.
			continue
		}
		if now.Sub(e.lastUsed) > r.idleTTL {
			toClose = append(toClose, evicted{id, e})
			delete(r.plugins, id)
		}
	}
	r.mu.Unlock()
	for _, ev := range toClose {
		if err := ev.entry.db.Close(); err != nil {
			log.Printf("[runner] idle-evict close instance %s: %v", ev.id, err)
		}
	}
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
	if ok {
		e.lastUsed = time.Now()
	}
	return e, ok
}

func (r *PluginRunner) put(instanceID string, entry *pluginEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.plugins[instanceID]; ok {
		// Re-Initialize for the same id: dispose of the previous instance so
		// its DB connection is released. The new one replaces it atomically.
		// Log on error so leaked statements or reset-on-close issues do not
		// silently disappear during a redo-Initialize.
		if err := old.db.Close(); err != nil {
			log.Printf("[runner] close prior plugin for instance %s: %v", instanceID, err)
		}
	}
	entry.lastUsed = time.Now()
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
//
// Concurrent cold-cache callers for the same id are single-flighted via
// loadOrInit so only one plugin is constructed. The handler is invoked under
// a refcount bump on the entry so evictIdle cannot close the plugin
// underneath an in-flight call, even if the call runs longer than idleTTL.
func (r *PluginRunner) withPlugin(
	ctx context.Context,
	instanceID string,
	pluginName string,
	req map[string]interface{},
	handler func(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error),
) (string, error) {
	if entry, ok := r.get(instanceID); ok {
		return r.runHandler(ctx, entry, req, handler)
	}

	cfg, _ := req["config"].(map[string]interface{})
	if cfg == nil {
		return "", fmt.Errorf("instance %s not cached and request carries no config to re-init", instanceID)
	}
	entry, err := r.loadOrInit(ctx, instanceID, pluginName, cfg)
	if err != nil {
		return "", err
	}
	return r.runHandler(ctx, entry, req, handler)
}

// runHandler invokes handler while holding a reference on entry, then bumps
// lastUsed on release so the next eviction check sees fresh activity even if
// the call took a long time. evictIdle skips entries with refs > 0, which is
// what keeps a long-running handler safe.
func (r *PluginRunner) runHandler(
	ctx context.Context,
	entry *pluginEntry,
	req map[string]interface{},
	handler func(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error),
) (string, error) {
	entry.refs.Add(1)
	defer func() {
		entry.refs.Add(-1)
		r.mu.Lock()
		entry.lastUsed = time.Now()
		r.mu.Unlock()
	}()
	return handler(ctx, entry.db, req)
}

// loadOrInit single-flights cold-cache loads for instanceID. Callers race for
// the per-id load mutex; the first to acquire it does the Initialize and puts
// the result in the cache, subsequent acquirers re-check the cache and find
// the freshly-loaded entry.
func (r *PluginRunner) loadOrInit(ctx context.Context, instanceID, pluginName string, cfg map[string]interface{}) (*pluginEntry, error) {
	// Find or create a load lock for this id without holding it across
	// Initialize (which can be slow — Initialize opens a DB connection).
	r.mu.Lock()
	if entry, ok := r.plugins[instanceID]; ok {
		r.mu.Unlock()
		return entry, nil
	}
	loadMu, ok := r.loading[instanceID]
	if !ok {
		loadMu = &sync.Mutex{}
		r.loading[instanceID] = loadMu
	}
	r.mu.Unlock()

	loadMu.Lock()
	defer loadMu.Unlock()

	// Double-check: another caller may have populated the cache while we were
	// queued on loadMu.
	if entry, ok := r.get(instanceID); ok {
		return entry, nil
	}

	plugin, err := loadPlugin(pluginName)
	if err != nil {
		return nil, err
	}
	if _, err := plugin.Initialize(ctx, dbplugin.InitializeRequest{
		Config:           cfg,
		VerifyConnection: false,
	}); err != nil {
		_ = plugin.Close()
		// Leave the load mutex in r.loading on failure. If we deleted it,
		// a concurrent fresh entrant would create a new mutex and the
		// retrying waiter (which still holds a reference to the old one)
		// would race with them — the very duplicate-Initialize bug the
		// single-flight design exists to prevent. Better to let every
		// retrying caller serialize through the same mutex; the leak is
		// bounded by the number of distinct instance ids ever served by
		// this runner (same bound the cache itself has).
		return nil, fmt.Errorf("lazy initialize: %w", err)
	}
	entry := &pluginEntry{pluginName: pluginName, db: plugin}
	r.put(instanceID, entry)
	// Done loading. Future cold-misses for this id will create a fresh mutex
	// (after the entry is later removed via Close). Without this delete the
	// loading map would grow once per distinct id the spoke ever saw.
	r.mu.Lock()
	delete(r.loading, instanceID)
	r.mu.Unlock()
	return entry, nil
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
