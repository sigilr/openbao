// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	dbtesting "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5/testing"
	_ "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"
)

func oracleACC(t *testing.T) string {
	t.Helper()
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("ORACLE_URL") == "" {
		t.Skip("set BAO_ACC=1 and ORACLE_URL to run Oracle acceptance tests")
	}
	return os.Getenv("ORACLE_URL")
}

func TestOracle_TypeAndVersion(t *testing.T) {
	db := newOracle()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, oracleTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestOracle_UsernameTemplate(t *testing.T) {
	db := newOracle()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"connection_url": "oracle://user:pass@localhost:1521/xe",
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)

	name, err := db.usernameProducer.Generate(dbplugin.UsernameMetadata{
		DisplayName: "display",
		RoleName:    "role",
	})
	require.NoError(t, err)
	// upper-case, underscore-separated, <= 30 chars
	require.LessOrEqual(t, len(name), 30)
	require.Regexp(t, `^V_[A-Z0-9_]+$`, name)
}

func TestOracle_UpdateUser_Validation(t *testing.T) {
	db := newOracle()
	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "U"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")
}

// --- Acceptance tests, gated on BAO_ACC=1 + ORACLE_URL ---------------------

func TestOracle_NewUser(t *testing.T) {
	connURL := oracleACC(t)

	db := newOracle()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": connURL},
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	pw := "BaoPass1234!"
	resp := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "test", RoleName: "test"},
		Statements: dbplugin.Statements{
			Commands: []string{
				`CREATE USER {{name}} IDENTIFIED BY "{{password}}"`,
				`GRANT CREATE SESSION TO {{name}}`,
			},
		},
		Password:   pw,
		Expiration: time.Now().Add(time.Hour),
	})
	require.NotEmpty(t, resp.Username)
	assertCredsExistOracle(t, connURL, resp.Username, pw)
	dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{Username: resp.Username})
}

func TestOracle_UpdateUser_Password(t *testing.T) {
	connURL := oracleACC(t)

	db := newOracle()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config:           map[string]interface{}{"connection_url": connURL},
		VerifyConnection: true,
	})
	defer dbtesting.AssertClose(t, db)

	created := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "upd", RoleName: "upd"},
		Statements: dbplugin.Statements{
			Commands: []string{
				`CREATE USER {{name}} IDENTIFIED BY "{{password}}"`,
				`GRANT CREATE SESSION TO {{name}}`,
			},
		},
		Password:   "BaoOld1234!",
		Expiration: time.Now().Add(time.Hour),
	})
	dbtesting.AssertUpdateUser(t, db, dbplugin.UpdateUserRequest{
		Username: created.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "BaoNew1234!"},
	})
	assertCredsExistOracle(t, connURL, created.Username, "BaoNew1234!")
	dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{Username: created.Username})
}

func assertCredsExistOracle(t testing.TB, connURL, username, password string) {
	t.Helper()
	scheme := "oracle://"
	rest := strings.TrimPrefix(connURL, scheme)
	at := strings.Index(rest, "@")
	require.GreaterOrEqual(t, at, 0)
	url := fmt.Sprintf("%s%s:%s@%s", scheme, username, password, rest[at+1:])
	db, err := sql.Open("oracle", url)
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck
	require.NoError(t, db.Ping())
}
