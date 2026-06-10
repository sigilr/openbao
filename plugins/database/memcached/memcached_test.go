// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package memcached

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestMemcached_TypeAndVersion(t *testing.T) {
	db := newMemcached()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, memcachedTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestMemcached_NewUserUnsupported(t *testing.T) {
	db := newMemcached()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"address": "localhost:11211"},
		VerifyConnection: false,
	})
	require.NoError(t, err)

	_, err = db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dynamic credentials are not supported")
}

func TestMemcached_UpdateUser_NoOp(t *testing.T) {
	db := newMemcached()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"address": "localhost:11211"},
		VerifyConnection: false,
	})
	require.NoError(t, err)

	// Validation paths
	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")

	// Successful no-op
	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: "u",
		Password: &dbplugin.ChangePassword{NewPassword: "new"},
	})
	require.NoError(t, err)
}

func TestMemcached_DeleteUser_NoOp(t *testing.T) {
	db := newMemcached()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.NoError(t, err)
}

// TestMemcached_Healthcheck_Connect runs against a local TCP listener that
// drops the connection after a single read, verifying the ping path is
// exercised. It doesn't pretend to speak the memcached protocol.
func TestMemcached_Healthcheck_Connect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close() //nolint:errcheck
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close() //nolint:errcheck
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("STAT pid 1\r\nEND\r\n"))
	}()

	db := newMemcached()
	_, err = db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"address": ln.Addr().String()},
		VerifyConnection: true,
	})
	require.NoError(t, err)
}

func TestMemcached_Healthcheck_BadAddr(t *testing.T) {
	db := newMemcached()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"address": "127.0.0.1:1"},
		VerifyConnection: true,
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "failed to verify connection"))
}

func TestMemcached_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("MEMCACHED_URL") == "" {
		t.Skip("set BAO_ACC=1 and MEMCACHED_URL to run Memcached acceptance tests")
	}
}
