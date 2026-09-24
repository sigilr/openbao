// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestEtcd_TypeAndVersion(t *testing.T) {
	db := newEtcd()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, etcdTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestEtcd_SanitizeEndpoints(t *testing.T) {
	require.Equal(
		t,
		[]string{"https://etcd1.example:2379", "https://etcd2.example:2379"},
		sanitizeEndpoints([]string{"https://etcd1.example:2379,https://etcd2.example:2379"}),
	)
	require.Equal(
		t,
		[]string{"etcd1:2379", "etcd2:2379"},
		sanitizeEndpoints([]string{" etcd1:2379 ", "etcd2:2379"}),
	)
	require.Nil(t, sanitizeEndpoints(nil))
}

func TestEtcd_IsUserNotFound(t *testing.T) {
	require.False(t, isUserNotFound(nil))
	require.False(t, isUserNotFound(context.DeadlineExceeded))
	require.True(t, isUserNotFound(&testErr{"etcdserver: user name not found"}))
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

func TestEtcd_StatementParsing(t *testing.T) {
	t.Run("basic roles", func(t *testing.T) {
		raw := `{"roles":["reader","editor"]}`
		var s etcdStatement
		require.NoError(t, json.Unmarshal([]byte(raw), &s))
		require.Equal(t, []string{"reader", "editor"}, s.Roles)
		require.Empty(t, s.CustomRoles)
	})

	t.Run("single role alias", func(t *testing.T) {
		raw := `{"role":"viewer"}`
		var s etcdStatement
		require.NoError(t, json.Unmarshal([]byte(raw), &s))
		require.Equal(t, []string{"viewer"}, s.Roles)
	})

	t.Run("custom roles with prefix and range_end", func(t *testing.T) {
		raw := `{
			"roles": ["reader"],
			"custom_roles": [
				{
					"name": "app_writer",
					"permissions": [
						{
							"permission": "readwrite",
							"key": "/app/",
							"prefix": true
						},
						{
							"permission": "read",
							"key": "/config/a",
							"range_end": "/config/z"
						}
					]
				}
			]
		}`
		var s etcdStatement
		require.NoError(t, json.Unmarshal([]byte(raw), &s))
		require.Equal(t, []string{"reader"}, s.Roles)
		require.Len(t, s.CustomRoles, 1)
		cr := s.CustomRoles[0]
		require.Equal(t, "app_writer", cr.Name)
		require.Len(t, cr.Permissions, 2)
		require.Equal(t, "readwrite", cr.Permissions[0].Permission)
		require.Equal(t, "/app/", cr.Permissions[0].Key)
		require.True(t, cr.Permissions[0].Prefix)
		require.Equal(t, "read", cr.Permissions[1].Permission)
		require.Equal(t, "/config/a", cr.Permissions[1].Key)
		require.Equal(t, "/config/z", cr.Permissions[1].RangeEnd)
		require.False(t, cr.Permissions[1].Prefix)
	})

	t.Run("aliases support", func(t *testing.T) {
		raw := `{
			"customRoles": [
				{
					"name": "app_admin",
					"privileges": [
						{
							"perm": "write",
							"path": "/data/",
							"isPrefix": true
						},
						{
							"type": "read",
							"path": "/keys/1",
							"rangeEnd": "/keys/9"
						}
					]
				}
			]
		}`
		var s etcdStatement
		require.NoError(t, json.Unmarshal([]byte(raw), &s))
		require.Len(t, s.CustomRoles, 1)
		cr := s.CustomRoles[0]
		require.Equal(t, "app_admin", cr.Name)
		require.Len(t, cr.Permissions, 2)
		require.Equal(t, "write", cr.Permissions[0].Permission)
		require.Equal(t, "/data/", cr.Permissions[0].Key)
		require.True(t, cr.Permissions[0].Prefix)
		require.Equal(t, "read", cr.Permissions[1].Permission)
		require.Equal(t, "/keys/1", cr.Permissions[1].Key)
		require.Equal(t, "/keys/9", cr.Permissions[1].RangeEnd)
	})
}

func TestEtcd_ParseEtcdPermissionType(t *testing.T) {
	tests := []struct {
		input    string
		expected clientv3.PermissionType
		err      bool
	}{
		{"read", clientv3.PermissionType(clientv3.PermRead), false},
		{"READ", clientv3.PermissionType(clientv3.PermRead), false},
		{"write", clientv3.PermissionType(clientv3.PermWrite), false},
		{"WRITE", clientv3.PermissionType(clientv3.PermWrite), false},
		{"readwrite", clientv3.PermissionType(clientv3.PermReadWrite), false},
		{"READWRITE", clientv3.PermissionType(clientv3.PermReadWrite), false},
		{"read_write", clientv3.PermissionType(clientv3.PermReadWrite), false},
		{"read-write", clientv3.PermissionType(clientv3.PermReadWrite), false},
		{"invalid", clientv3.PermissionType(-1), true},
		{"", clientv3.PermissionType(-1), true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			pt, err := parseEtcdPermissionType(tt.input)
			if tt.err {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected, pt)
			}
		})
	}
}

func TestEtcd_IsRoleAlreadyExists(t *testing.T) {
	require.False(t, isRoleAlreadyExists(nil))
	require.False(t, isRoleAlreadyExists(errors.New("connection refused")))
	require.True(t, isRoleAlreadyExists(rpctypes.ErrRoleAlreadyExist))
	require.True(t, isRoleAlreadyExists(rpctypes.ErrGRPCRoleAlreadyExist))
	require.True(t, isRoleAlreadyExists(fmt.Errorf("rpc error: %w", rpctypes.ErrRoleAlreadyExist)))
	require.True(t, isRoleAlreadyExists(errors.New("etcdserver: role name already exists")))
	require.True(t, isRoleAlreadyExists(errors.New("etcdserver: role already exists")))
}

func TestEtcd_DeduplicateStrings(t *testing.T) {
	require.Nil(t, deduplicateStrings(nil))
	require.Equal(t, []string{"a", "b"}, deduplicateStrings([]string{"a", "b", "a", " ", "", "b"}))
}

func TestEtcd_Initialize_RequiresEndpoints(t *testing.T) {
	db := newEtcd()
	_, err := db.Initialize(t.Context(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"username": "root",
			"password": "password",
		},
	})
	require.ErrorContains(t, err, "endpoints is required")
}

func TestEtcd_Initialize_InvalidDialTimeout(t *testing.T) {
	db := newEtcd()
	_, err := db.Initialize(t.Context(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"endpoints":    "etcd.example.com:2379",
			"username":     "root",
			"password":     "password",
			"dial_timeout": "not-a-duration",
		},
	})
	require.ErrorContains(t, err, "invalid dial_timeout")
}

func TestEtcd_Initialize_RejectsIncompleteClientIdentity(t *testing.T) {
	db := newEtcd()
	_, err := db.Initialize(t.Context(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"endpoints":       "etcd.example.com:2379",
			"username":        "root",
			"password":        "password",
			"tls_certificate": "certificate",
		},
	})
	require.ErrorContains(t, err, "both tls_certificate and tls_key are required")
}

func TestEtcd_UpdateUser_Validation(t *testing.T) {
	db := newEtcd()
	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")
}

func TestEtcd_DeleteUser_Validation(t *testing.T) {
	db := newEtcd()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")
}

func TestEtcd_NotInitialized(t *testing.T) {
	db := newEtcd()

	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		Statements: dbplugin.Statements{
			Commands: []string{`{"roles":["reader"]}`},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: "u",
		Password: &dbplugin.ChangePassword{NewPassword: "p"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{
		Username: "u",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")
}

func TestEtcd_NewUser_EmptyCreationStatement(t *testing.T) {
	db := newEtcd()
	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{})
	require.Error(t, err)
}

func TestEtcd_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("ETCD_ENDPOINTS") == "" {
		t.Skip("set BAO_ACC=1 and ETCD_ENDPOINTS to run etcd acceptance tests")
	}
}
