// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package db2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestDB2_TypeAndVersion(t *testing.T) {
	db := newDB2()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, db2TypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestDB2_NewUserUnsupported(t *testing.T) {
	db := newDB2()
	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dynamic credentials are not supported")
}

func TestDB2_UpdateUser_Validation(t *testing.T) {
	db := newDB2()
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

func TestDB2_DeleteUser_NoOp(t *testing.T) {
	db := newDB2()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.NoError(t, err)
}

func TestDB2_Healthcheck(t *testing.T) {
	var seen string
	var auth string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = r.URL.Path
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close() //nolint:errcheck

	db := newDB2()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      srv.URL,
			"username": "admin",
			"password": "admin",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "/dbapi/v4/host_status", seen)
	require.NotEmpty(t, auth)
}

func TestDB2_Healthcheck_SkippedWhenURLEmpty(t *testing.T) {
	db := newDB2()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{},
		VerifyConnection: true,
	})
	require.NoError(t, err)
}

func TestDB2_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("DB2_URL") == "" {
		t.Skip("set BAO_ACC=1 and DB2_URL to run Db2 acceptance tests")
	}
}
