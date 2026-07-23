// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package weaviate

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

func TestWeaviate_TypeAndVersion(t *testing.T) {
	db := newWeaviate()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, weaviateTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestWeaviate_NewUserUnsupported(t *testing.T) {
	db := newWeaviate()
	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dynamic credentials are not supported")
}

func TestWeaviate_UpdateUser_Validation(t *testing.T) {
	db := newWeaviate()
	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: "u",
		Password: &dbplugin.ChangePassword{NewPassword: "n"},
	})
	require.NoError(t, err)
}

func TestWeaviate_DeleteUser_NoOp(t *testing.T) {
	db := newWeaviate()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.NoError(t, err)
}

func TestWeaviate_Healthcheck(t *testing.T) {
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

	db := newWeaviate()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":     srv.URL,
			"api_key": "topsecret",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "/v1/.well-known/ready", seen)
	require.Equal(t, "Bearer topsecret", auth)
}

func TestWeaviate_Healthcheck_Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close() //nolint:errcheck

	db := newWeaviate()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"url": srv.URL},
		VerifyConnection: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to verify connection")
}

func TestWeaviate_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("WEAVIATE_URL") == "" {
		t.Skip("set BAO_ACC=1 and WEAVIATE_URL to run Weaviate acceptance tests")
	}
}
