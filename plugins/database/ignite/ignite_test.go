// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package ignite

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestIgnite_TypeAndVersion(t *testing.T) {
	db := newIgnite()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, igniteTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestIgnite_SafeIdentifier(t *testing.T) {
	require.NoError(t, safeIdentifier("V_TEST_TEST"))
	require.Error(t, safeIdentifier(""))
	require.Error(t, safeIdentifier(`bad"quote`))
	require.Error(t, safeIdentifier("bad'quote"))
	require.Error(t, safeIdentifier("bad;semi"))
}

func TestIgnite_SafePassword(t *testing.T) {
	require.NoError(t, safePassword("BaoIgnite-1234"))
	require.Error(t, safePassword(""))
	require.Error(t, safePassword("bad'quote"))
}

func TestIgnite_RenderTemplate(t *testing.T) {
	out := renderTemplate(`CREATE USER "{{name}}" WITH PASSWORD '{{password}}'`,
		map[string]string{"name": "V_T", "password": "pw"})
	require.Equal(t, `CREATE USER "V_T" WITH PASSWORD 'pw'`, out)
}

// TestIgnite_FakeServer exercises the REST API call shape end-to-end.
func TestIgnite_FakeServer(t *testing.T) {
	type call struct{ qry, cmd string }
	var mu sync.Mutex
	var calls []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{qry: r.URL.Query().Get("qry"), cmd: r.URL.Query().Get("cmd")})
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successStatus":0,"response":[]}`))
	}))
	defer srv.Close() //nolint:errcheck

	db := newIgnite()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      srv.URL,
			"username": "ignite",
			"password": "ignite",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements: dbplugin.Statements{
			Commands: []string{`CREATE USER "{{name}}" WITH PASSWORD '{{password}}';`},
		},
		Password:   "BaoIgnitePass1234",
		Expiration: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)
	require.Regexp(t, `^V_[A-Z0-9_]+$`, resp.Username)

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "NewPass-1234"},
	})
	require.NoError(t, err)

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: resp.Username})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "version", calls[0].cmd) // ping
	require.Contains(t, calls[1].qry, "CREATE USER")
	require.Contains(t, calls[2].qry, "ALTER USER")
	require.Contains(t, calls[3].qry, "DROP USER")
}

// TestIgnite_RestError verifies the REST envelope's successStatus is
// honoured even when the HTTP status is 200.
func TestIgnite_RestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successStatus":4,"error":"USER_EXISTS"}`))
	}))
	defer srv.Close() //nolint:errcheck

	db := newIgnite()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      srv.URL,
			"username": "ignite",
			"password": "ignite",
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	_, err = db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements: dbplugin.Statements{
			Commands: []string{`CREATE USER "{{name}}" WITH PASSWORD '{{password}}';`},
		},
		Password: "Pass-1234",
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "USER_EXISTS"))
}

func TestIgnite_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("IGNITE_URL") == "" {
		t.Skip("set BAO_ACC=1 and IGNITE_URL to run Ignite acceptance tests")
	}
}
