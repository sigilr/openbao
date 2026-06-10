// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package zookeeper implements a **static-credentials-only** OpenBao v5
// database plugin for Apache ZooKeeper.
//
// ZooKeeper has no runtime user-management API for SASL/digest principals:
// usernames + passwords are loaded from a `jaas.conf` (server-side) or a
// `zoo.cfg` `digest:` entry at startup. ACLs *can* be reconfigured at
// runtime, but they are per-znode and tied to existing principals — not a
// dynamic-credential interface.
//
// The plugin therefore:
//
//   - Initialize verifies the configured ensemble is reachable by
//     connecting TCP and sending the ZooKeeper 4-letter `ruok` command.
//     `imok` confirms the node is healthy.
//   - NewUser returns an explicit "not supported" error.
//   - UpdateUser is a no-op against the server; returns success on
//     password updates so OpenBao static-role rotation tracks the new
//     credential and emits audit events.
//   - DeleteUser is a no-op.
package zookeeper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const zookeeperTypeName = "zookeeper"

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// ZooKeeper implements dbplugin.Database in static-only mode.
type ZooKeeper struct {
	mu     sync.Mutex
	config *zookeeperConfig
}

type zookeeperConfig struct {
	// Address is host:port for one node of the ensemble. We don't need a
	// connected client to verify reachability — the 4-letter command is a
	// single-shot TCP exchange.
	Address  string `mapstructure:"address"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
}

var (
	_ dbplugin.Database       = (*ZooKeeper)(nil)
	_ logical.PluginVersioner = (*ZooKeeper)(nil)
)

func New() (interface{}, error) {
	db := newZooKeeper()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newZooKeeper() *ZooKeeper {
	return &ZooKeeper{}
}

func (z *ZooKeeper) secretValues() map[string]string {
	if z.config == nil {
		return map[string]string{}
	}
	return map[string]string{z.config.Password: "[password]"}
}

func (z *ZooKeeper) Type() (string, error) {
	return zookeeperTypeName, nil
}

func (z *ZooKeeper) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (z *ZooKeeper) Close() error {
	z.mu.Lock()
	defer z.mu.Unlock()
	return nil
}

func (z *ZooKeeper) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	z.mu.Lock()
	defer z.mu.Unlock()

	cfg := &zookeeperConfig{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if cfg.Address == "" {
		return dbplugin.InitializeResponse{}, errors.New("address is required (host:port)")
	}

	z.config = cfg

	if req.VerifyConnection {
		if err := z.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser is unsupported — see the package comment.
func (z *ZooKeeper) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	return dbplugin.NewUserResponse{}, errors.New(
		"dynamic credentials are not supported by ZooKeeper (SASL/digest principals are loaded from jaas.conf/zoo.cfg at startup); " +
			"use static-roles to track a manually-provisioned credential, or run a sidecar that updates the server config on UpdateUser",
	)
}

// UpdateUser is a no-op against the server but returns success on
// password updates so static-role rotation tracks the new credential.
func (z *ZooKeeper) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser is a no-op.
func (z *ZooKeeper) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	return dbplugin.DeleteUserResponse{}, nil
}

// healthcheck sends ZooKeeper's 4-letter `ruok` command. A healthy node
// replies with `imok` (no terminator). Many production deployments
// restrict 4lw commands via `4lw.commands.whitelist`; if `ruok` is not
// allowed the connection succeeds but the response is empty, which we
// treat as a verification failure with a clear hint.
func (z *ZooKeeper) healthcheck(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", z.config.Address)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	if _, err := conn.Write([]byte("ruok")); err != nil {
		return fmt.Errorf("write ruok: %w", err)
	}
	buf := make([]byte, 4)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read ruok reply: %w", err)
	}
	if string(buf[:n]) != "imok" {
		return fmt.Errorf("ruok returned %q (expected %q) — is 4lw.commands.whitelist=ruok set?", string(buf[:n]), "imok")
	}
	return nil
}
