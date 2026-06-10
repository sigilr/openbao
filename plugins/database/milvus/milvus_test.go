// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package milvus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestMilvus_TypeAndVersion(t *testing.T) {
	db := newMilvus()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, milvusTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestMilvus_StatementParsing(t *testing.T) {
	raw := `{"roles":["admin","reader"]}`
	var s milvusStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, []string{"admin", "reader"}, s.Roles)
}

func TestMilvus_FakeServer(t *testing.T) {
	type call struct {
		path string
		body map[string]interface{}
	}
	var mu sync.Mutex
	var calls []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		c := call{path: r.URL.Path}
		var b map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.body = b
		calls = append(calls, c)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close() //nolint:errcheck

	db := newMilvus()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      srv.URL,
			"username": "root",
			"password": "Milvus123",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{`{"roles":["public"]}`}},
		Password:       "BaoMilvusPass123",
		Expiration:     time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "BaoMilvusPass456"},
	})
	require.NoError(t, err)

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: resp.Username})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 5)
	require.Equal(t, "/v2/vectordb/users/list", calls[0].path) // ping
	require.Equal(t, "/v2/vectordb/users/create", calls[1].path)
	require.Equal(t, "/v2/vectordb/users/grant_role", calls[2].path)
	require.Equal(t, "/v2/vectordb/users/update_password", calls[3].path)
	require.Equal(t, "/v2/vectordb/users/drop", calls[4].path)
}

// TestMilvus_APIError verifies the plugin treats `{"code": non-zero}` as a
// failure even though the HTTP status is 200.
func TestMilvus_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1, "message":"user already exists"}`))
	}))
	defer srv.Close() //nolint:errcheck

	db := newMilvus()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      srv.URL,
			"username": "root",
			"password": "Milvus123",
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	_, err = db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{`{"roles":["public"]}`}},
		Password:       "BaoMilvusPass123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "code=1")
	require.Contains(t, err.Error(), "user already exists")
}

func TestMilvus_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("MILVUS_URL") == "" {
		t.Skip("set BAO_ACC=1 and MILVUS_URL to run Milvus acceptance tests")
	}
}
