// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package documentdb

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestDocDB_TypeAndVersion(t *testing.T) {
	db := newDocumentDB()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, docDBTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestDocDB_UsernameTemplate(t *testing.T) {
	db := newDocumentDB()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": "mongodb://user:pass@host:27017"},
		VerifyConnection: false,
	})
	require.NoError(t, err)
	name, err := db.usernameProducer.Generate(dbplugin.UsernameMetadata{
		DisplayName: "display", RoleName: "role",
	})
	require.NoError(t, err)
	require.Regexp(t, `^v-display-role-[A-Za-z0-9]{20}-[0-9]{10}$`, name)
}

func TestDocDB_StatementParsing(t *testing.T) {
	in := `{"db":"admin","roles":[{"role":"readWrite"},{"role":"readWrite","db":"app"}]}`
	var s docDBStatement
	require.NoError(t, json.Unmarshal([]byte(in), &s))
	require.Equal(t, "admin", s.DB)
	require.Len(t, s.Roles, 2)
	out := s.Roles.toStandardRolesArray()
	require.Equal(t, "readWrite", out[0])
	require.True(t, reflect.DeepEqual(docDBRole{Role: "readWrite", DB: "app"}, out[1]))
}

func TestDocDB_LoadConfig_BadTLS(t *testing.T) {
	cfg := &docDBConnectionProducer{
		ConnectionURL: "mongodb://docdb.local:27017",
		TLSCAData:     []byte("not pem"),
	}
	_, err := cfg.makeClientOpts()
	require.Error(t, err)
}

func TestDocDB_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("DOCDB_URL") == "" {
		t.Skip("set BAO_ACC=1 and DOCDB_URL to run DocumentDB acceptance tests")
	}
}
