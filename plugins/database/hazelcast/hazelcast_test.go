// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package hazelcast

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

func TestHazelcast_TypeAndVersion(t *testing.T) {
	db := newHazelcast()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, hazelcastTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestHazelcast_NewUserUnsupported(t *testing.T) {
	db := newHazelcast()
	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dynamic credentials are not supported")
}

func TestHazelcast_UpdateUser_Validation(t *testing.T) {
	db := newHazelcast()
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

func TestHazelcast_DeleteUser_NoOp(t *testing.T) {
	db := newHazelcast()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.NoError(t, err)
}

func TestHazelcast_Healthcheck(t *testing.T) {
	var seen string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close() //nolint:errcheck

	db := newHazelcast()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"url": srv.URL},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "/hazelcast/health/ready", seen)
}

func TestHazelcast_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("HAZELCAST_URL") == "" {
		t.Skip("set BAO_ACC=1 and HAZELCAST_URL to run Hazelcast acceptance tests")
	}
}
