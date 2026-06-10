// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package zookeeper

import (
	"context"
	"net"
	"os"
	"testing"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestZK_TypeAndVersion(t *testing.T) {
	db := newZooKeeper()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, zookeeperTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestZK_NewUserUnsupported(t *testing.T) {
	db := newZooKeeper()
	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dynamic credentials are not supported")
}

func TestZK_UpdateUser_Validation(t *testing.T) {
	db := newZooKeeper()
	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: "u",
		Password: &dbplugin.ChangePassword{NewPassword: "n"},
	})
	require.NoError(t, err)
}

func TestZK_DeleteUser_NoOp(t *testing.T) {
	db := newZooKeeper()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.NoError(t, err)
}

// TestZK_Healthcheck_OK serves `imok` to a 4-letter `ruok` request.
func TestZK_Healthcheck_OK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close() //nolint:errcheck

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close() //nolint:errcheck
		buf := make([]byte, 4)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("imok"))
	}()

	db := newZooKeeper()
	_, err = db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"address": ln.Addr().String()},
		VerifyConnection: true,
	})
	require.NoError(t, err)
}

// TestZK_Healthcheck_Restricted simulates a cluster where ruok is not
// in the 4lw whitelist — the server closes without writing anything.
func TestZK_Healthcheck_Restricted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close() //nolint:errcheck

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close() //nolint:errcheck
		// Read the request and close immediately without writing.
		buf := make([]byte, 4)
		_, _ = c.Read(buf)
	}()

	db := newZooKeeper()
	_, err = db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"address": ln.Addr().String()},
		VerifyConnection: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to verify connection")
}

func TestZK_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("ZOOKEEPER_ADDRESS") == "" {
		t.Skip("set BAO_ACC=1 and ZOOKEEPER_ADDRESS to run ZooKeeper acceptance tests")
	}
}
