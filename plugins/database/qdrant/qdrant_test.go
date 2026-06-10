// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package qdrant

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

func TestQdrant_TypeAndVersion(t *testing.T) {
	db := newQdrant()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, qdrantTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestQdrant_NewUserUnsupported(t *testing.T) {
	db := newQdrant()
	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dynamic credentials are not supported")
}

func TestQdrant_UpdateUser_Validation(t *testing.T) {
	db := newQdrant()
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
	require.NoError(t, err) // no-op success
}

func TestQdrant_DeleteUser_NoOp(t *testing.T) {
	db := newQdrant()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.NoError(t, err)
}

func TestQdrant_Healthcheck(t *testing.T) {
	var seen string
	var apiKey string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = r.URL.Path
		apiKey = r.Header.Get("api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close() //nolint:errcheck

	db := newQdrant()
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
	require.Equal(t, "/readyz", seen)
	require.Equal(t, "topsecret", apiKey)
}

func TestQdrant_Healthcheck_Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close() //nolint:errcheck

	db := newQdrant()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"url": srv.URL},
		VerifyConnection: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to verify connection")
}

func TestQdrant_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("QDRANT_URL") == "" {
		t.Skip("set BAO_ACC=1 and QDRANT_URL to run Qdrant acceptance tests")
	}
}
