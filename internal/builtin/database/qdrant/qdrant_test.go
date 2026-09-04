// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package qdrant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
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

func TestQdrant_NewUser_JWT(t *testing.T) {
	adminKey := "test-admin-secret-key-12345"
	db := newQdrant()
	db.config = &qdrantConfig{
		URL:    "http://localhost:6333",
		APIKey: adminKey,
	}

	expTime := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	t.Run("global read access via string", func(t *testing.T) {
		resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{
				DisplayName: "testuser",
				RoleName:    "reader",
			},
			Statements: dbplugin.Statements{
				Commands: []string{"r"},
			},
			Expiration: expTime,
		})
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)
		require.NotEmpty(t, resp.Password)

		// Parse and validate JWT token
		claims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(resp.Password, claims, func(token *jwt.Token) (interface{}, error) {
			require.Equal(t, jwt.SigningMethodHS256, token.Method)
			return []byte(adminKey), nil
		})
		require.NoError(t, err)
		require.True(t, token.Valid)
		require.Equal(t, "r", claims["access"])
		require.Equal(t, resp.Username, claims["sub"])
		require.Equal(t, float64(expTime.Unix()), claims["exp"])
	})

	t.Run("granular access via native JSON statement", func(t *testing.T) {
		jsonStmt := `{
			"access": [
				{
					"collection": "Products",
					"access": "rw"
				},
				{
					"collection": "Users",
					"access": "r"
				}
			]
		}`
		resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
			Statements: dbplugin.Statements{
				Commands: []string{jsonStmt},
			},
			Expiration: expTime,
		})
		require.NoError(t, err)

		claims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(resp.Password, claims, func(token *jwt.Token) (interface{}, error) {
			return []byte(adminKey), nil
		})
		require.NoError(t, err)
		require.True(t, token.Valid)

		accessList, ok := claims["access"].([]interface{})
		require.True(t, ok)
		require.Len(t, accessList, 2)
	})

	t.Run("collection rules via multiple JSON statements", func(t *testing.T) {
		stmt1 := `{"collection": "Products", "access": "r"}`
		stmt2 := `{"collection": "Orders", "access": "rw"}`
		resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
			Statements: dbplugin.Statements{
				Commands: []string{stmt1, stmt2},
			},
		})
		require.NoError(t, err)

		claims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(resp.Password, claims, func(token *jwt.Token) (interface{}, error) {
			return []byte(adminKey), nil
		})
		require.NoError(t, err)
		require.True(t, token.Valid)

		accessList, ok := claims["access"].([]interface{})
		require.True(t, ok)
		require.Len(t, accessList, 2)
	})

	t.Run("missing api_key returns error", func(t *testing.T) {
		badDB := newQdrant()
		_, err := badDB.NewUser(context.Background(), dbplugin.NewUserRequest{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "api_key is required")
	})
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

func TestQdrant_ValueExists_Lifecycle(t *testing.T) {
	adminKey := "test-secret-key-999"
	var mu sync.Mutex
	var collectionsCreated []string
	var pointsInserted []map[string]any
	var pointsDeleted []map[string]any
	collectionExists := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		require.Equal(t, adminKey, r.Header.Get("api-key"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/readyz":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/collections/openbao_users":
			if !collectionExists {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"status":{"error":"Collection not found"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))

		case r.Method == http.MethodPut && r.URL.Path == "/collections/openbao_users":
			collectionExists = true
			collectionsCreated = append(collectionsCreated, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"result":true,"status":"ok"}`))

		case r.Method == http.MethodPut && r.URL.Path == "/collections/openbao_users/points":
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			pointsInserted = append(pointsInserted, payload)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"result":{"operation_id":1,"status":"completed"},"status":"ok"}`))

		case r.Method == http.MethodPost && r.URL.Path == "/collections/openbao_users/points/delete":
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			pointsDeleted = append(pointsDeleted, payload)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"result":{"operation_id":2,"status":"completed"},"status":"ok"}`))

		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	db := newQdrant()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":     srv.URL,
			"api_key": adminKey,
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close()

	// 1. Create User
	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{
			DisplayName: "testuser",
			RoleName:    "reader",
		},
		Statements: dbplugin.Statements{
			Commands: []string{"r"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)
	require.NotEmpty(t, resp.Password)

	// Verify collection was created and point was inserted
	mu.Lock()
	require.Len(t, collectionsCreated, 1)
	require.Len(t, pointsInserted, 1)
	pts := pointsInserted[0]["points"].([]any)
	pt := pts[0].(map[string]any)
	require.NotNil(t, pt["vector"])
	require.Equal(t, resp.Username, pt["payload"].(map[string]any)["user_id"])
	mu.Unlock()

	// Verify JWT contains value_exists claim
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(resp.Password, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(adminKey), nil
	})
	require.NoError(t, err)
	require.True(t, token.Valid)

	veClaim, ok := claims["value_exists"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "openbao_users", veClaim["collection"])
	matches := veClaim["matches"].([]any)
	match := matches[0].(map[string]any)
	require.Equal(t, "user_id", match["key"])
	require.Equal(t, resp.Username, match["value"])

	// 2. Delete User
	delResp, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{
		Username: resp.Username,
	})
	require.NoError(t, err)
	require.NotNil(t, delResp)

	// Verify points/delete was called with filter matching user_id
	mu.Lock()
	require.Len(t, pointsDeleted, 1)
	delFilter := pointsDeleted[0]["filter"].(map[string]any)
	mustList := delFilter["must"].([]any)
	mustClause := mustList[0].(map[string]any)
	require.Equal(t, "user_id", mustClause["key"])
	matchObj := mustClause["match"].(map[string]any)
	require.Equal(t, resp.Username, matchObj["value"])
	mu.Unlock()
}

func TestQdrant_DeleteUser_Validation(t *testing.T) {
	db := newQdrant()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")
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
		Config: map[string]any{
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
		Config:           map[string]any{"url": srv.URL},
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
