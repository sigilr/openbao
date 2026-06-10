// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package druid

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

func TestDruid_TypeAndVersion(t *testing.T) {
	db := newDruid()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, druidTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestDruid_StatementParsing(t *testing.T) {
	raw := `{"roles":["admin","datasourceReadAccess"]}`
	var s druidStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, []string{"admin", "datasourceReadAccess"}, s.Roles)
}

func TestDruid_DefaultAuthenticatorAndAuthorizer(t *testing.T) {
	db := newDruid()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      "http://druid:8081",
			"username": "admin",
			"password": "admin",
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)
	require.Equal(t, defaultAuthenticator, db.config.Authenticator)
	require.Equal(t, defaultAuthorizer, db.config.Authorizer)
}

func TestDruid_FakeServer(t *testing.T) {
	type call struct{ method, path string }
	var mu sync.Mutex
	var calls []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{method: r.Method, path: r.URL.Path})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close() //nolint:errcheck

	db := newDruid()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":           srv.URL,
			"username":      "admin",
			"password":      "admin",
			"authenticator": "auth1",
			"authorizer":    "auth2",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{`{"roles":["roleA"]}`}},
		Password:       "BaoDruidPass1234",
		Expiration:     time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "BaoDruidPass5678"},
	})
	require.NoError(t, err)

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: resp.Username})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	// 1 status, then NewUser issues 4 (authn user, authn creds, authz user,
	// authz role), then 1 for password update, then 2 for delete = 8.
	require.Len(t, calls, 8)
	require.Equal(t, "GET", calls[0].method)
	require.Contains(t, calls[0].path, "/status")
	require.Equal(t, "POST", calls[1].method)
	require.Contains(t, calls[1].path, "/authentication/db/auth1/users/")
	require.Equal(t, "POST", calls[2].method)
	require.Contains(t, calls[2].path, "/credentials")
	require.Equal(t, "POST", calls[3].method)
	require.Contains(t, calls[3].path, "/authorization/db/auth2/users/")
	require.Equal(t, "POST", calls[4].method)
	require.Contains(t, calls[4].path, "/roles/roleA")
	require.Equal(t, "POST", calls[5].method)
	require.Contains(t, calls[5].path, "/credentials") // update
	require.Equal(t, "DELETE", calls[6].method)
	require.Contains(t, calls[6].path, "/authorization/db/auth2/users/")
	require.Equal(t, "DELETE", calls[7].method)
	require.Contains(t, calls[7].path, "/authentication/db/auth1/users/")
}

func TestDruid_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("DRUID_URL") == "" {
		t.Skip("set BAO_ACC=1 and DRUID_URL to run Druid acceptance tests")
	}
}
