// Copyright (c) HashiCorp, Inc.
// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package hana

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	dbtesting "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5/testing"
	"github.com/stretchr/testify/require"
)

// hanaACC is the env-var gate for HANA acceptance tests. Set BAO_ACC=1 and
// HANA_URL=hdb://SYSTEM:<pass>@host:port to exercise these tests against a
// real HANA database. Tests that don't need a database run unconditionally.
func hanaACC(t *testing.T) string {
	t.Helper()
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("HANA_URL") == "" {
		t.Skip("set BAO_ACC=1 and HANA_URL to run HANA acceptance tests")
	}
	return os.Getenv("HANA_URL")
}

// TestHANA_TypeAndVersion confirms the plugin metadata surface without
// reaching out to a database. This is the always-on unit test that ensures
// `go test ./plugins/database/hana/...` does something useful on CI.
func TestHANA_TypeAndVersion(t *testing.T) {
	db := newHANA()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, hanaTypeName, typ)

	pv := db.PluginVersion()
	require.Equal(t, ReportedVersion, pv.Version)
}

// TestHANA_UsernameTemplate validates the default template produces the
// HANA-safe identifier shape (uppercased, underscores, length-capped).
func TestHANA_UsernameTemplate(t *testing.T) {
	db := newHANA()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			// connection_url is required by Init even when VerifyConnection=false.
			"connection_url": "hdb://user:pass@localhost:39041",
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)

	name, err := db.usernameProducer.Generate(dbplugin.UsernameMetadata{
		DisplayName: "displayname",
		RoleName:    "rolename",
	})
	require.NoError(t, err)
	require.Regexp(t, `^V_DISPLAYNAME_ROLENAME_[A-Z0-9]{20}_[0-9]{10}$`, name)
}

func TestHANA_Initialize(t *testing.T) {
	connURL := hanaACC(t)

	connectionDetails := map[string]interface{}{
		"connection_url": connURL,
	}

	expectedConfig := copyConfig(connectionDetails)

	initReq := dbplugin.InitializeRequest{
		Config:           connectionDetails,
		VerifyConnection: true,
	}

	db := newHANA()
	initResp := dbtesting.AssertInitialize(t, db, initReq)
	defer dbtesting.AssertClose(t, db)

	if !reflect.DeepEqual(initResp.Config, expectedConfig) {
		t.Fatalf("Actual config: %#v\nExpected config: %#v", initResp.Config, expectedConfig)
	}
}

func TestHANA_NewUser(t *testing.T) {
	connURL := hanaACC(t)

	type testCase struct {
		commands   []string
		expectErr  bool
		assertUser func(t testing.TB, connURL, username, password string)
	}

	tests := map[string]testCase{
		"no creation statements": {
			commands:   []string{},
			expectErr:  true,
			assertUser: assertCredsDoNotExist,
		},
		"with creation statements": {
			commands:   []string{testHANARole},
			expectErr:  false,
			assertUser: assertCredsExist,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			db := newHANA()
			dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
				Config:           map[string]interface{}{"connection_url": connURL},
				VerifyConnection: true,
			})
			defer dbtesting.AssertClose(t, db)

			req := dbplugin.NewUserRequest{
				UsernameConfig: dbplugin.UsernameMetadata{
					DisplayName: "test-test",
					RoleName:    "test-test",
				},
				Statements: dbplugin.Statements{Commands: test.commands},
				Password:   "AG4qagho_dsvZ",
				Expiration: time.Now().Add(1 * time.Second),
			}

			createResp, err := db.NewUser(context.Background(), req)
			if test.expectErr && err == nil {
				t.Fatalf("err expected, received nil")
			}
			if !test.expectErr && err != nil {
				t.Fatalf("no error expected, got: %s", err)
			}

			test.assertUser(t, connURL, createResp.Username, req.Password)
		})
	}
}

func TestHANA_UpdateUser(t *testing.T) {
	connURL := hanaACC(t)

	type testCase struct {
		commands         []string
		expectErrOnLogin bool
		expectedErrMsg   string
	}

	tests := map[string]testCase{
		"no update statements": {
			commands:         []string{},
			expectErrOnLogin: true,
			expectedErrMsg:   "user is forced to change password",
		},
		"with custom update statements": {
			commands:         []string{testHANAUpdate},
			expectErrOnLogin: false,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			db := newHANA()
			dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
				Config:           map[string]interface{}{"connection_url": connURL},
				VerifyConnection: true,
			})
			defer dbtesting.AssertClose(t, db)

			password := "this_is_Thirty_2_characters_wow_"
			userResp := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
				UsernameConfig: dbplugin.UsernameMetadata{
					DisplayName: "test-test",
					RoleName:    "test-test",
				},
				Password:   password,
				Statements: dbplugin.Statements{Commands: []string{testHANARole}},
				Expiration: time.Now().Add(time.Hour),
			})
			assertCredsExist(t, connURL, userResp.Username, password)

			req := dbplugin.UpdateUserRequest{
				Username: userResp.Username,
				Password: &dbplugin.ChangePassword{
					NewPassword: "this_is_ALSO_Thirty_2_characters_",
					Statements:  dbplugin.Statements{Commands: test.commands},
				},
			}

			dbtesting.AssertUpdateUser(t, db, req)
			err := testCredsExist(t, connURL, userResp.Username, req.Password.NewPassword)
			if test.expectErrOnLogin {
				if err == nil {
					t.Fatalf("Able to login with new creds when expecting an issue")
				} else if test.expectedErrMsg != "" && !strings.Contains(err.Error(), test.expectedErrMsg) {
					t.Fatalf("Expected error message to contain %q, received: %s", test.expectedErrMsg, err)
				}
			}
			if !test.expectErrOnLogin && err != nil {
				t.Fatalf("Unable to login: %s", err)
			}
		})
	}
}

func TestHANA_DeleteUser(t *testing.T) {
	connURL := hanaACC(t)

	tests := map[string]struct{ commands []string }{
		"default soft drop":           {commands: []string{}},
		"with custom drop statements": {commands: []string{testHANADrop}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			db := newHANA()
			dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
				Config:           map[string]interface{}{"connection_url": connURL},
				VerifyConnection: true,
			})
			defer dbtesting.AssertClose(t, db)

			password := "this_is_Thirty_2_characters_wow_"

			userResp := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
				UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "test-test", RoleName: "test-test"},
				Password:       password,
				Statements:     dbplugin.Statements{Commands: []string{testHANARole}},
				Expiration:     time.Now().Add(time.Hour),
			})
			assertCredsExist(t, connURL, userResp.Username, password)

			dbtesting.AssertDeleteUser(t, db, dbplugin.DeleteUserRequest{
				Username:   userResp.Username,
				Statements: dbplugin.Statements{Commands: tc.commands},
			})
			assertCredsDoNotExist(t, connURL, userResp.Username, password)
		})
	}
}

func TestHANA_CustomUsernameTemplate(t *testing.T) {
	connURL := hanaACC(t)

	db := newHANA()
	dbtesting.AssertInitialize(t, db, dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"connection_url":    connURL,
			"username_template": "{{.DisplayName}}_{{random 10}}",
		},
		VerifyConnection: true,
	})

	const password = "SuperSecurePa55w0rd!"
	resp := dbtesting.AssertNewUser(t, db, dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "test", RoleName: "test"},
		Password:       password,
		Statements:     dbplugin.Statements{Commands: []string{testHANARole}},
		Expiration:     time.Now().Add(5 * time.Minute),
	})

	require.NotEmpty(t, resp.Username)
	require.Regexp(t, regexp.MustCompile(`^TEST_[A-Z0-9]{10}$`), resp.Username)
	defer dbtesting.AssertClose(t, db)
}

func testCredsExist(t testing.TB, connURL, username, password string) error {
	parts := strings.Split(connURL, "@")
	connURL = fmt.Sprintf("hdb://%s:%s@%s", username, password, parts[1])
	db, err := sql.Open("hdb", connURL)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	return db.Ping()
}

func assertCredsExist(t testing.TB, connURL, username, password string) {
	t.Helper()
	if err := testCredsExist(t, connURL, username, password); err != nil {
		t.Fatalf("Unable to log in as %q: %s", username, err)
	}
}

func assertCredsDoNotExist(t testing.TB, connURL, username, password string) {
	t.Helper()
	if err := testCredsExist(t, connURL, username, password); err == nil {
		t.Fatalf("Able to log in when we should not be able to")
	}
}

func copyConfig(config map[string]interface{}) map[string]interface{} {
	newConfig := map[string]interface{}{}
	for k, v := range config {
		newConfig[k] = v
	}
	return newConfig
}

const testHANARole = `
CREATE USER {{name}} PASSWORD "{{password}}" NO FORCE_FIRST_PASSWORD_CHANGE VALID UNTIL '{{expiration}}';`

const testHANADrop = `
DROP USER {{name}} CASCADE;`

const testHANAUpdate = `
ALTER USER {{name}} PASSWORD "{{password}}" NO FORCE_FIRST_PASSWORD_CHANGE;`
