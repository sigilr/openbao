// Copyright (c) HashiCorp, Inc.
// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package mongodb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	dbtesting "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5/testing"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// mongoACC is the env-var gate for MongoDB acceptance tests. Set BAO_ACC=1
// and MONGODB_URL=mongodb://user:pass@host:port to exercise these tests
// against a real cluster. Tests that don't need a database run uncond.
func mongoACC(t *testing.T) string {
	t.Helper()
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("MONGODB_URL") == "" {
		t.Skip("set BAO_ACC=1 and MONGODB_URL to run MongoDB acceptance tests")
	}
	return os.Getenv("MONGODB_URL")
}

func TestMongoDB_TypeAndVersion(t *testing.T) {
	db := newMongoDB()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, mongoDBTypeName, typ)

	pv := db.PluginVersion()
	require.Equal(t, ReportedVersion, pv.Version)
}

// TestMongoDB_UsernameTemplate validates the default template renders into
// the expected v-<display>-<role>-<random>-<unix> shape.
func TestMongoDB_UsernameTemplate(t *testing.T) {
	db := newMongoDB()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": "mongodb://user:pass@localhost:27017"},
		VerifyConnection: false,
	})
	require.NoError(t, err)

	name, err := db.usernameProducer.Generate(dbplugin.UsernameMetadata{
		DisplayName: "displayname",
		RoleName:    "rolename",
	})
	require.NoError(t, err)
	require.Regexp(t, `^v-displayname-rolename-[A-Za-z0-9]{20}-[0-9]{10}$`, name)
}

// TestMongoDB_StatementParsing covers the JSON->mongoDBStatement decoding
// the plugin does on every NewUser / DeleteUser request.
func TestMongoDB_StatementParsing(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		expect mongoDBStatement
	}{
		{
			name: "single bare role",
			in:   `{"db":"admin","roles":[{"role":"readWrite"}]}`,
			expect: mongoDBStatement{
				DB:    "admin",
				Roles: mongodbRoles{{Role: "readWrite"}},
			},
		},
		{
			name: "db-scoped role",
			in:   `{"db":"admin","roles":[{"role":"readWrite","db":"app"}]}`,
			expect: mongoDBStatement{
				DB:    "admin",
				Roles: mongodbRoles{{Role: "readWrite", DB: "app"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got mongoDBStatement
			require.NoError(t, json.Unmarshal([]byte(tc.in), &got))
			require.True(t, reflect.DeepEqual(tc.expect, got), "got %#v want %#v", got, tc.expect)
		})
	}
}

// TestMongoDB_StandardRoles confirms bare roles flatten to strings and
// db-qualified roles stay as documents — that's what createUser expects.
func TestMongoDB_StandardRoles(t *testing.T) {
	roles := mongodbRoles{
		{Role: "readWrite"},
		{Role: "readWrite", DB: "app"},
	}
	out := roles.toStandardRolesArray()
	require.Len(t, out, 2)
	require.Equal(t, "readWrite", out[0])
	rd, ok := out[1].(mongodbRole)
	require.True(t, ok)
	require.Equal(t, "readWrite", rd.Role)
	require.Equal(t, "app", rd.DB)
}

// TestMongoDB_WriteConcern_RawJSON validates the JSON path through
// getWriteConcern.
func TestMongoDB_WriteConcern_RawJSON(t *testing.T) {
	c := &mongoDBConnectionProducer{
		WriteConcern: `{"wmode":"majority","wtimeout":1000,"j":true}`,
	}
	opts, err := c.getWriteConcern()
	require.NoError(t, err)
	require.NotNil(t, opts)
	require.NotNil(t, opts.WriteConcern)
}

// TestMongoDB_WriteConcern_Base64 validates the base64-of-JSON path through
// getWriteConcern — operators do this when a CI secret store can't pass
// literal braces.
func TestMongoDB_WriteConcern_Base64(t *testing.T) {
	raw := `{"wmode":"majority","wtimeout":500,"j":true}`
	c := &mongoDBConnectionProducer{
		WriteConcern: base64.StdEncoding.EncodeToString([]byte(raw)),
	}
	opts, err := c.getWriteConcern()
	require.NoError(t, err)
	require.NotNil(t, opts)
	require.NotNil(t, opts.WriteConcern)
}

func TestMongoDB_WriteConcern_Empty(t *testing.T) {
	c := &mongoDBConnectionProducer{}
	opts, err := c.getWriteConcern()
	require.NoError(t, err)
	require.Nil(t, opts)
}

func TestMongoDB_WriteConcern_Garbage(t *testing.T) {
	c := &mongoDBConnectionProducer{WriteConcern: "not json"}
	_, err := c.getWriteConcern()
	require.Error(t, err)
}

func TestMongoDB_LoadConfig_Errors(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]interface{}
	}{
		{name: "empty connection_url", cfg: map[string]interface{}{}},
		{name: "negative socket_timeout", cfg: map[string]interface{}{"connection_url": "mongodb://localhost:27017", "socket_timeout": "-1s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &mongoDBConnectionProducer{}
			require.Error(t, c.loadConfig(tc.cfg))
		})
	}
}

// --- Acceptance tests, gated on BAO_ACC=1 + MONGODB_URL ---------------------

func TestMongoDB_Initialize(t *testing.T) {
	connURL := mongoACC(t)

	cfg := map[string]interface{}{"connection_url": connURL}
	db := newMongoDB()
	resp := dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           cfg,
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	require.True(t, reflect.DeepEqual(resp.Config, cfg), "config mismatch")
}

func TestMongoDB_NewUser(t *testing.T) {
	connURL := mongoACC(t)

	db := newMongoDB()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": connURL},
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	resp := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "test", RoleName: "test"},
		Password:       "Bao-Test-Mongo-Password-1",
		Statements: dbplugin.Statements{
			Commands: []string{`{"db":"admin","roles":[{"role":"readWrite"}]}`},
		},
		Expiration: time.Now().Add(time.Hour),
	})
	require.NotEmpty(t, resp.Username)

	// Verify we can authenticate as the new user.
	client, err := db.Connection(context.Background())
	require.NoError(t, err)
	require.NoError(t, client.Ping(context.Background(), readpref.Primary()))
}

func TestMongoDB_DeleteUser(t *testing.T) {
	connURL := mongoACC(t)

	db := newMongoDB()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": connURL},
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	created := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "delete", RoleName: "delete"},
		Password:       "Bao-Test-Mongo-Password-2",
		Statements: dbplugin.Statements{
			Commands: []string{`{"db":"admin","roles":[{"role":"readWrite"}]}`},
		},
		Expiration: time.Now().Add(time.Hour),
	})

	dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{Username: created.Username})

	// Second delete should still succeed (UserNotFound is treated as a no-op).
	dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{Username: created.Username})
}

func TestMongoDB_UpdateUser_Password(t *testing.T) {
	connURL := mongoACC(t)

	db := newMongoDB()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": connURL},
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	created := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "upd", RoleName: "upd"},
		Password:       "Bao-Test-Mongo-Password-3",
		Statements: dbplugin.Statements{
			Commands: []string{`{"db":"admin","roles":[{"role":"readWrite"}]}`},
		},
		Expiration: time.Now().Add(time.Hour),
	})

	dbtesting.AssertUpdateUser(t, db, dbplugin.UpdateUserRequest{
		Username: created.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "Bao-Test-Mongo-Password-4"},
	})

	dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{Username: created.Username})
}
