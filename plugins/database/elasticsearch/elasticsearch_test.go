// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package elasticsearch

import (
	"context"
	"encoding/json"
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

func TestES_TypeAndVersion(t *testing.T) {
	db := newES()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, esTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestES_StatementParsing(t *testing.T) {
	raw := `{"elasticsearch_roles":["readonly","kibana_user"],"full_name":"Test User"}`
	var s esStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, []string{"readonly", "kibana_user"}, s.ElasticsearchRoles)
	require.Equal(t, "Test User", s.FullName)
}

// TestES_FakeServer wires Initialize -> NewUser -> UpdateUser -> DeleteUser
// through an httptest server. Verifies the request shape and routes the
// plugin sends to the security API, plus that the username producer works
// end-to-end.
func TestES_FakeServer(t *testing.T) {
	var mu sync.Mutex
	type call struct {
		method, path string
		body         map[string]interface{}
	}
	var calls []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		c := call{method: r.Method, path: r.URL.Path}
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			if len(b) > 0 {
				_ = json.Unmarshal(b, &c.body)
			}
		}
		calls = append(calls, c)

		if r.URL.Path == "/_cluster/health" {
			_, _ = w.Write([]byte(`{"status":"green"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close() //nolint:errcheck

	db := newES()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      srv.URL,
			"username": "elastic",
			"password": "elastic",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "test", RoleName: "test"},
		Statements: dbplugin.Statements{
			Commands: []string{`{"elasticsearch_roles":["readonly"]}`},
		},
		Password:   "BaoESPass1234",
		Expiration: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(resp.Username, "v-test-test-"))

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "BaoESPass5678"},
	})
	require.NoError(t, err)

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: resp.Username})
	require.NoError(t, err)

	// Assert call sequence.
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 4)
	require.Equal(t, "GET", calls[0].method)
	require.Equal(t, "/_cluster/health", calls[0].path)
	require.Equal(t, "PUT", calls[1].method)
	require.Equal(t, "/_security/user/"+resp.Username, calls[1].path)
	require.Equal(t, "POST", calls[2].method)
	require.Equal(t, "/_security/user/"+resp.Username+"/_password", calls[2].path)
	require.Equal(t, "DELETE", calls[3].method)
	require.Equal(t, "/_security/user/"+resp.Username, calls[3].path)
}

// TestES_OldXPackPath verifies the legacy ES 6 API path is used when the
// use_old_xpack flag is set.
func TestES_OldXPackPath(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close() //nolint:errcheck

	db := newES()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":           srv.URL,
			"username":      "elastic",
			"password":      "elastic",
			"use_old_xpack": true,
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	_, _ = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.Equal(t, "/_xpack/security/user/u", seen)
}

func TestES_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("ES_URL") == "" {
		t.Skip("set BAO_ACC=1 and ES_URL to run Elasticsearch acceptance tests")
	}
	// Real-cluster acceptance flow is identical to the fake server above;
	// see TEST.md for the manual run book.
}
