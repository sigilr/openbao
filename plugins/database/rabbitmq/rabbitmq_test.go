// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package rabbitmq

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	rabbithole "github.com/michaelklishin/rabbit-hole/v3"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestRMQ_TypeAndVersion(t *testing.T) {
	db := newRabbitMQ()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, rabbitmqTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestRMQ_ParseTags(t *testing.T) {
	cases := map[string]rabbithole.UserTags{
		"":                           nil,
		"administrator":              {"administrator"},
		"administrator,monitoring":   {"administrator", "monitoring"},
		" management , policymaker ": {"management", "policymaker"},
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got := parseTags(in)
			require.True(t, reflect.DeepEqual(got, want), "got %#v want %#v", got, want)
		})
	}
}

func TestRMQ_StatementParsing(t *testing.T) {
	raw := `{"tags":"administrator","vhosts":{"/":{"configure":".*","write":".*","read":".*"}}}`
	var s rmqStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, "administrator", s.Tags)
	require.Equal(t, ".*", s.VHosts["/"].Configure)
}

func TestRMQ_UpdateUser_Validation(t *testing.T) {
	db := newRabbitMQ()
	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")
}

func TestRMQ_NewUser_EmptyStatements(t *testing.T) {
	db := newRabbitMQ()
	// Initialize a producer so NewUser reaches the statements check.
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]interface{}{
			"connection_uri": "http://localhost:15672",
			"username":       "guest",
			"password":       "guest",
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)

	_, err = db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
	})
	require.Error(t, err)
}

func TestRMQ_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("RABBITMQ_URL") == "" {
		t.Skip("set BAO_ACC=1 and RABBITMQ_URL (management URL, e.g. http://guest:guest@localhost:15672) to run RabbitMQ acceptance tests")
	}
	// Manual flow lives in TEST.md.
}
