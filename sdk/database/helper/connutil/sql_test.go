// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package connutil

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

type noopConnector struct{}

func (noopConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, assert.AnError
}

func (noopConnector) Driver() driver.Driver { return noopDriver{} }

type noopDriver struct{}

func (noopDriver) Open(string) (driver.Conn, error) { return nil, assert.AnError }

func TestSQLConnectionProducerOpenDB(t *testing.T) {
	t.Parallel()

	producer := &SQLConnectionProducer{Type: "hdb"}
	var gotDriver, gotDSN string
	producer.OpenDB = func(driverName, dataSourceName string) (*sql.DB, error) {
		gotDriver, gotDSN = driverName, dataSourceName
		return sql.OpenDB(noopConnector{}), nil
	}

	_, err := producer.Init(t.Context(), map[string]any{
		"connection_url": "hdb://user:password@database.example:39041",
	}, false)
	assert.NoError(t, err)

	_, err = producer.Connection(t.Context())
	assert.NoError(t, err)
	assert.Equal(t, "hdb", gotDriver)
	assert.Equal(t, "hdb://user:password@database.example:39041", gotDSN)
}

func TestSQLPasswordChars(t *testing.T) {
	testCases := []struct {
		Username string
		Password string
	}{
		{"postgres", "password{0}"},
		{"postgres", "pass:word"},
		{"postgres", "pass/word"},
		{"postgres", "p@ssword"},
		{"postgres", "pass\"word\""},
	}
	for _, tc := range testCases {
		t.Logf("username %q password %q", tc.Username, tc.Password)

		sql := &SQLConnectionProducer{}
		ctx := t.Context()
		conf := map[string]any{
			"connection_url":   "postgres://{{username}}:{{password}}@localhost:5432/mydb",
			"username":         tc.Username,
			"password":         tc.Password,
			"disable_escaping": false,
		}
		_, err := sql.Init(ctx, conf, false)
		if err != nil {
			t.Errorf("Init error on %q %q: %+v", tc.Username, tc.Password, err)
		} else {
			// This jumps down a few layers...
			// Connection() uses sql.Open uses lib/pq uses net/url.Parse
			u, err := url.Parse(sql.ConnectionURL)
			if err != nil {
				t.Errorf("URL parse error on %q %q: %+v", tc.Username, tc.Password, err)
			} else {
				username := u.User.Username()
				password, pPresent := u.User.Password()
				if username != tc.Username {
					t.Errorf("Parsed username %q != original username %q", username, tc.Username)
				}
				if !pPresent {
					t.Errorf("Password %q not present", tc.Password)
				} else if password != tc.Password {
					t.Errorf("Parsed password %q != original password %q", password, tc.Password)
				}
			}
		}
	}
}

func TestSQLDisableEscaping(t *testing.T) {
	testCases := []struct {
		Username        string
		Password        string
		DisableEscaping bool
	}{
		{"mssql{0}", "password{0}", true},
		{"mssql{0}", "password{0}", false},
		{"ms\"sql\"", "pass\"word\"", true},
		{"ms\"sql\"", "pass\"word\"", false},
		{"ms'sq;l", "pass'wor;d", true},
		{"ms'sq;l", "pass'wor;d", false},
	}
	for _, tc := range testCases {
		t.Logf("username %q password %q disable_escaling %t", tc.Username, tc.Password, tc.DisableEscaping)

		sql := &SQLConnectionProducer{}
		ctx := t.Context()
		conf := map[string]any{
			"connection_url":   "server=localhost;port=1433;user id={{username}};password={{password}};database=mydb;",
			"username":         tc.Username,
			"password":         tc.Password,
			"disable_escaping": tc.DisableEscaping,
		}
		_, err := sql.Init(ctx, conf, false)
		if err != nil {
			t.Errorf("Init error on %q %q: %+v", tc.Username, tc.Password, err)
		} else {
			if tc.DisableEscaping {
				if !strings.Contains(sql.ConnectionURL, tc.Username) || !strings.Contains(sql.ConnectionURL, tc.Password) {
					t.Error("Raw username and/or password missing from ConnectionURL")
				}
			} else {
				if strings.Contains(sql.ConnectionURL, tc.Username) || strings.Contains(sql.ConnectionURL, tc.Password) {
					t.Error("Raw username and/or password was present in ConnectionURL")
				}
			}
		}
	}
}

func TestSQLDisallowTemplates(t *testing.T) {
	testCases := []struct {
		Username string
		Password string
	}{
		{"{{username}}", "pass"},
		{"{{password}}", "pass"},
		{"user", "{{username}}"},
		{"user", "{{password}}"},
		{"{{username}}", "{{password}}"},
		{"abc{username}xyz", "123{password}789"},
		{"abc{{username}}xyz", "123{{password}}789"},
		{"abc{{{username}}}xyz", "123{{{password}}}789"},
	}
	for _, disableEscaping := range []bool{true, false} {
		for _, tc := range testCases {
			t.Logf("username %q password %q disable_escaping %t", tc.Username, tc.Password, disableEscaping)

			sql := &SQLConnectionProducer{}
			ctx := t.Context()
			conf := map[string]any{
				"connection_url":   "server=localhost;port=1433;user id={{username}};password={{password}};database=mydb;",
				"username":         tc.Username,
				"password":         tc.Password,
				"disable_escaping": disableEscaping,
			}
			_, err := sql.Init(ctx, conf, false)
			if disableEscaping {
				if err != nil {
					if !assert.EqualError(t, err, "username and/or password cannot contain the template variables") {
						t.Errorf("Init error on %q %q: %+v", tc.Username, tc.Password, err)
					}
				} else {
					assert.Equal(t, sql.ConnectionURL, "server=localhost;port=1433;user id=abc{username}xyz;password=123{password}789;database=mydb;")
				}
			}
		}
	}
}
