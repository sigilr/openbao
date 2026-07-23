// Copyright (c) HashiCorp, Inc.
// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	dbtesting "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5/testing"
	"github.com/stretchr/testify/require"
)

func mssqlACC(t *testing.T) string {
	t.Helper()
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("MSSQL_URL") == "" {
		t.Skip("set BAO_ACC=1 and MSSQL_URL to run MSSQL acceptance tests")
	}
	return os.Getenv("MSSQL_URL")
}

func TestMSSQL_TypeAndVersion(t *testing.T) {
	db := newMSSQL()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, msSQLTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestMSSQL_UsernameTemplate(t *testing.T) {
	db := newMSSQL()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"connection_url": "sqlserver://sa:Pass@localhost:1433?database=master",
		},
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

func TestMSSQL_ContainedDB_Parse(t *testing.T) {
	cases := map[string]struct {
		v       interface{}
		want    bool
		wantErr bool
	}{
		"true":    {v: true, want: true},
		"strTrue": {v: "true", want: true},
		"false":   {v: false, want: false},
		"garbage": {v: "nope", wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := newMSSQL()
			_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
				Config: map[string]interface{}{
					"connection_url": "sqlserver://sa:Pass@localhost:1433?database=master",
					"contained_db":   tc.v,
				},
				VerifyConnection: false,
			})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, db.containedDB)
		})
	}
}

// --- Acceptance tests, gated on BAO_ACC=1 + MSSQL_URL ---------------------

func TestMSSQL_NewUser(t *testing.T) {
	connURL := mssqlACC(t)

	db := newMSSQL()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": connURL},
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	resp := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "test", RoleName: "test"},
		Statements: dbplugin.Statements{
			Commands: []string{`
				CREATE LOGIN [{{name}}] WITH PASSWORD = '{{password}}';
				CREATE USER [{{name}}] FOR LOGIN [{{name}}];
				GRANT SELECT, INSERT, UPDATE, DELETE TO [{{name}}];`},
		},
		Password:   "TestPass-MSSQL-Bao-1234567",
		Expiration: time.Now().Add(time.Hour),
	})
	require.NotEmpty(t, resp.Username)

	assertCredsExistMSSQL(t, connURL, resp.Username, "TestPass-MSSQL-Bao-1234567")

	dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{Username: resp.Username})
}

func TestMSSQL_UpdateUser(t *testing.T) {
	connURL := mssqlACC(t)

	db := newMSSQL()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": connURL},
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	pw1 := "TestPass-MSSQL-Bao-old-1"
	created := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "upd", RoleName: "upd"},
		Statements: dbplugin.Statements{
			Commands: []string{`CREATE LOGIN [{{name}}] WITH PASSWORD = '{{password}}';`},
		},
		Password:   pw1,
		Expiration: time.Now().Add(time.Hour),
	})

	pw2 := "TestPass-MSSQL-Bao-new-2"
	dbtesting.AssertUpdateUser(t, db, dbplugin.UpdateUserRequest{
		Username: created.Username,
		Password: &dbplugin.ChangePassword{NewPassword: pw2},
	})
	assertCredsExistMSSQL(t, connURL, created.Username, pw2)

	dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{Username: created.Username})
}

func assertCredsExistMSSQL(t testing.TB, connURL, username, password string) {
	t.Helper()
	// connURL is sqlserver://oldUser:oldPass@host:port?...
	scheme := "sqlserver://"
	rest := strings.TrimPrefix(connURL, scheme)
	at := strings.Index(rest, "@")
	require.GreaterOrEqual(t, at, 0)
	hostPart := rest[at+1:]
	url := fmt.Sprintf("%s%s:%s@%s", scheme, username, password, hostPart)

	db, err := sql.Open("sqlserver", url)
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck
	require.NoError(t, db.Ping())
}
