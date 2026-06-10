// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package solr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestSolr_TypeAndVersion(t *testing.T) {
	db := newSolr()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, solrTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestSolr_StatementParsing(t *testing.T) {
	raw := `{"roles":["admin","reader"]}`
	var s solrStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, []string{"admin", "reader"}, s.Roles)
}

// TestSolr_FakeServer wires Initialize -> NewUser -> UpdateUser -> DeleteUser
// through an httptest server and asserts the path + JSON body of each call.
func TestSolr_FakeServer(t *testing.T) {
	type call struct {
		method, path string
		body         map[string]interface{}
	}
	var mu sync.Mutex
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
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close() //nolint:errcheck

	db := newSolr()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"url":      srv.URL + "/solr",
			"username": "solr",
			"password": "solrRocks",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{`{"roles":["admin"]}`}},
		Password:       "BaoSolr1234",
		Expiration:     time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "BaoSolr5678"},
	})
	require.NoError(t, err)

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: resp.Username})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 5)
	require.Equal(t, "/solr/admin/info/system", calls[0].path)    // ping
	require.Equal(t, "/solr/admin/authentication", calls[1].path) // set-user
	_, ok := calls[1].body["set-user"]
	require.True(t, ok)
	require.Equal(t, "/solr/admin/authorization", calls[2].path)  // set-user-role
	require.Equal(t, "/solr/admin/authentication", calls[3].path) // update password
	require.Equal(t, "/solr/admin/authentication", calls[4].path) // delete-user
}

func TestSolr_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("SOLR_URL") == "" {
		t.Skip("set BAO_ACC=1 and SOLR_URL to run Solr acceptance tests")
	}
}
