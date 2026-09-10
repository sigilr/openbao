// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package weaviate

import (
	"context"
	"encoding/json"
	"errors"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestWeaviate_TypeAndVersion(t *testing.T) {
	db := newWeaviate()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, weaviateTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestWeaviate_NotInitialized(t *testing.T) {
	db := newWeaviate()
	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")
}

func TestWeaviate_NewUser_And_DeleteUser(t *testing.T) {
	var mu sync.Mutex
	var createdUser string
	var assignedRoles []string
	var deletedUser string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		auth := r.Header.Get("Authorization")
		if auth != "Bearer admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/.well-known/ready":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/users/db/"):
			createdUser = strings.TrimPrefix(r.URL.Path, "/v1/users/db/")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"apikey": "weaviate-secret-key-xyz"})

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/authz/users/") && strings.HasSuffix(r.URL.Path, "/assign"):
			var body struct {
				Roles    []string `json:"roles"`
				UserType string   `json:"userType"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			assignedRoles = body.Roles
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/users/db/"):
			deletedUser = strings.TrimPrefix(r.URL.Path, "/v1/users/db/")
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	db := newWeaviate()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":     srv.URL,
			"api_key": "admin-token",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	// Test NewUser
	req := dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{
			DisplayName: "testuser",
			RoleName:    "reader",
		},
		Statements: dbplugin.Statements{
			Commands: []string{"viewer", "customRole"},
		},
	}
	resp, err := db.NewUser(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)
	require.Equal(t, "weaviate-secret-key-xyz", resp.Password)

	mu.Lock()
	require.Equal(t, resp.Username, createdUser)
	require.Equal(t, []string{"viewer", "customRole"}, assignedRoles)
	mu.Unlock()

	// Test UpdateUser rejects password updates / static credentials
	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "new"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "weaviate does not support updating user credentials or static roles")

	// Test UpdateUser allows expiration update (lease renewal)
	upResp, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username:   resp.Username,
		Expiration: &dbplugin.ChangeExpiration{NewExpiration: time.Now().Add(time.Hour)},
	})
	require.NoError(t, err)
	require.NotNil(t, upResp)

	// Test DeleteUser
	delResp, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{
		Username: resp.Username,
	})
	require.NoError(t, err)
	require.NotNil(t, delResp)

	mu.Lock()
	require.Equal(t, resp.Username, deletedUser)
	mu.Unlock()
}

func TestWeaviate_NewUser_CustomRoles(t *testing.T) {
	var mu sync.Mutex
	var createdRoles []string
	var assignedRoles []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/.well-known/ready":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && r.URL.Path == "/v1/authz/roles":
			var role struct {
				Name string `json:"name"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &role)
			createdRoles = append(createdRoles, role.Name)
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/users/db/"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"apikey": "test-key"})

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/authz/users/") && strings.HasSuffix(r.URL.Path, "/assign"):
			var body struct {
				Roles []string `json:"roles"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			assignedRoles = body.Roles
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	db := newWeaviate()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":     srv.URL,
			"api_key": "test",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	role1JSON := `{
		"name": "customrole",
		"permissions": [
			{"action": "read_data", "collections": {"collection": "Products"}},
			{"action": "create_data", "collections": {"collection": "Products"}}
		]
	}`
	role2JSON := `{
		"name": "clusterAdmin",
		"permissions": [
			{"action": "read_cluster"}
		]
	}`
	req := dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{
			DisplayName: "testuser",
			RoleName:    "reader",
		},
		Statements: dbplugin.Statements{
			Commands: []string{"viewer", role1JSON, role2JSON},
		},
	}
	resp, err := db.NewUser(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)

	mu.Lock()
	require.Equal(t, []string{"customrole", "clusterAdmin"}, createdRoles)
	require.Equal(t, []string{"viewer", "customrole", "clusterAdmin"}, assignedRoles)
	mu.Unlock()
}

func TestWeaviate_EnsureRole_AddPermissionsFailures(t *testing.T) {
	transportErr := errors.New("connection reset")
	tests := []struct {
		name        string
		response    *http.Response
		err         error
		expectedErr string
	}{
		{
			name:        "transport error",
			err:         transportErr,
			expectedErr: "connection reset",
		},
		{
			name: "non-success response",
			response: &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Body:       io.NopCloser(strings.NewReader(`{"error":[{"message":"permission denied"}]}`)),
				Header:     make(http.Header),
			},
			expectedErr: "403 Forbidden: permission denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			db := newWeaviate()
			db.config = &weaviateConfig{URL: "http://weaviate.example"}
			db.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{
						StatusCode: http.StatusConflict,
						Status:     "409 Conflict",
						Body:       http.NoBody,
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}
				if tt.response != nil {
					tt.response.Request = req
				}
				return tt.response, tt.err
			})}

			err := db.ensureRole(context.Background(), weaviateRoleDef{
				Name:        "analyst",
				Permissions: []map[string]string{{"action": "read_data"}},
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), `failed to add permissions to role "analyst"`)
			require.Contains(t, err.Error(), tt.expectedErr)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
			}
			require.Equal(t, 2, calls)
		})
	}
}

func TestWeaviate_NewUser_StructuredRoles(t *testing.T) {
	var mu sync.Mutex
	var createdRoles []string
	var assignedRoles []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/.well-known/ready":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && r.URL.Path == "/v1/authz/roles":
			var role struct {
				Name string `json:"name"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &role)
			createdRoles = append(createdRoles, role.Name)
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/users/db/"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"apikey": "test-key"})

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/authz/users/") && strings.HasSuffix(r.URL.Path, "/assign"):
			var body struct {
				Roles []string `json:"roles"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			assignedRoles = body.Roles
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	db := newWeaviate()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":     srv.URL,
			"api_key": "test",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	t.Run("roles and custom_roles in single JSON object", func(t *testing.T) {
		mu.Lock()
		createdRoles = nil
		assignedRoles = nil
		mu.Unlock()

		stmt := `{
			"roles": ["viewer"],
			"custom_roles": [
				{
					"name": "customrole",
					"permissions": [
						{"action": "read_data", "collections": {"collection": "Products"}},
						{"action": "create_data", "collections": {"collection": "Products"}}
					]
				}
			]
		}`
		req := dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{RoleName: "app"},
			Statements:     dbplugin.Statements{Commands: []string{stmt}},
		}
		resp, err := db.NewUser(context.Background(), req)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)

		mu.Lock()
		require.Equal(t, []string{"customrole"}, createdRoles)
		require.Equal(t, []string{"viewer", "customrole"}, assignedRoles)
		mu.Unlock()
	})

	t.Run("roles only JSON object", func(t *testing.T) {
		mu.Lock()
		createdRoles = nil
		assignedRoles = nil
		mu.Unlock()

		stmt := `{"roles": ["viewer", "editor"]}`
		req := dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{RoleName: "app"},
			Statements:     dbplugin.Statements{Commands: []string{stmt}},
		}
		resp, err := db.NewUser(context.Background(), req)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)

		mu.Lock()
		require.Empty(t, createdRoles)
		require.Equal(t, []string{"viewer", "editor"}, assignedRoles)
		mu.Unlock()
	})

	t.Run("custom_roles only JSON object", func(t *testing.T) {
		mu.Lock()
		createdRoles = nil
		assignedRoles = nil
		mu.Unlock()

		stmt := `{"custom_roles": [{"name": "writer", "permissions": [{"action": "create_data"}]}]}`
		req := dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{RoleName: "app"},
			Statements:     dbplugin.Statements{Commands: []string{stmt}},
		}
		resp, err := db.NewUser(context.Background(), req)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)

		mu.Lock()
		require.Equal(t, []string{"writer"}, createdRoles)
		require.Equal(t, []string{"writer"}, assignedRoles)
		mu.Unlock()
	})

	t.Run("JSON array of string role names", func(t *testing.T) {
		mu.Lock()
		createdRoles = nil
		assignedRoles = nil
		mu.Unlock()

		stmt := `["viewer", "admin"]`
		req := dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{RoleName: "app"},
			Statements:     dbplugin.Statements{Commands: []string{stmt}},
		}
		resp, err := db.NewUser(context.Background(), req)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)

		mu.Lock()
		require.Empty(t, createdRoles)
		require.Equal(t, []string{"viewer", "admin"}, assignedRoles)
		mu.Unlock()
	})

	t.Run("camelCase customRoles and role aliases", func(t *testing.T) {
		mu.Lock()
		createdRoles = nil
		assignedRoles = nil
		mu.Unlock()

		stmt := `{"role": "viewer", "customRoles": [{"name": "analyst"}]}`
		req := dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{RoleName: "app"},
			Statements:     dbplugin.Statements{Commands: []string{stmt}},
		}
		resp, err := db.NewUser(context.Background(), req)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)

		mu.Lock()
		require.Equal(t, []string{"analyst"}, createdRoles)
		require.Equal(t, []string{"viewer", "analyst"}, assignedRoles)
		mu.Unlock()
	})
}

func TestWeaviate_NewUser_InvalidStatements(t *testing.T) {
	var mu sync.Mutex
	var createdUsers []string
	var deletedUsers []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/.well-known/ready":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/users/db/"):
			user := strings.TrimPrefix(r.URL.Path, "/v1/users/db/")
			createdUsers = append(createdUsers, user)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"apikey": "test-key"})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/users/db/"):
			user := strings.TrimPrefix(r.URL.Path, "/v1/users/db/")
			deletedUsers = append(deletedUsers, user)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	db := newWeaviate()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":     srv.URL,
			"api_key": "test",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	testCases := []struct {
		name        string
		command     string
		errContains string
	}{
		{
			name:        "invalid JSON object schema",
			command:     `{"unrecognized": "value"}`,
			errContains: "failed to parse role statement JSON",
		},
		{
			name:        "malformed JSON object",
			command:     `{not valid json`,
			errContains: "failed to parse role statement JSON",
		},
		{
			name:        "malformed JSON array",
			command:     `[not valid json`,
			errContains: "failed to parse role statement JSON array",
		},
		{
			name:        "custom role missing name in custom_roles",
			command:     `{"custom_roles": [{"permissions": [{"action": "read_data"}]}]}`,
			errContains: "custom role definition missing name",
		},
		{
			name:        "custom role missing name in array",
			command:     `[{"permissions": [{"action": "read_data"}]}]`,
			errContains: "custom role definition missing name",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			createdUsers = nil
			deletedUsers = nil
			mu.Unlock()

			req := dbplugin.NewUserRequest{
				UsernameConfig: dbplugin.UsernameMetadata{RoleName: "app"},
				Statements:     dbplugin.Statements{Commands: []string{tc.command}},
			}
			_, err := db.NewUser(context.Background(), req)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errContains)

			mu.Lock()
			require.Len(t, createdUsers, 1)
			require.Equal(t, createdUsers, deletedUsers, "created user must be cleaned up on error")
			mu.Unlock()
		})
	}
}

func TestWeaviate_UsernameTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/.well-known/ready":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/users/db/"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"apikey": "key"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	db := newWeaviate()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":               srv.URL,
			"username_template": `weaviate-{{ .RoleName }}-{{ random 5 }}`,
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{
			RoleName: "custom",
		},
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(resp.Username, "weaviate-custom-"))
	require.Equal(t, "key", resp.Password)
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
		Password: &dbplugin.ChangePassword{NewPassword: "p"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "weaviate does not support updating user credentials or static roles")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username:   "u",
		Expiration: &dbplugin.ChangeExpiration{NewExpiration: time.Now().Add(time.Hour)},
	})
	require.NoError(t, err)
}

func TestWeaviate_DeleteUser_Validation(t *testing.T) {
	db := newWeaviate()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")
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
		Config:           map[string]any{"url": srv.URL},
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
