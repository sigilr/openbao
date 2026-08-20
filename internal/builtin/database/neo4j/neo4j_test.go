// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package neo4j

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
)

func TestNeo4j_TypeAndVersion(t *testing.T) {
	db := newNeo4j()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, neo4jTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestNeo4j_InlineTLS(t *testing.T) {
	db := newNeo4j()
	_, err := db.Initialize(t.Context(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"uri":      "neo4j://database.example:7687",
			"username": "neo4j",
			"password": "password",
			"use_tls":  true,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "neo4j+s://database.example:7687", db.config.URI)
	require.NotNil(t, db.driver)
	require.NoError(t, db.Close())
}

func TestNeo4j_InlineTLSInsecureIsExplicit(t *testing.T) {
	scheme, err := secureScheme("bolt+ssc", false)
	require.NoError(t, err)
	require.Equal(t, "bolt+s", scheme)

	scheme, err = secureScheme("bolt", true)
	require.NoError(t, err)
	require.Equal(t, "bolt+ssc", scheme)
}

func TestNeo4j_InlineTLSRejectsIncompleteClientIdentity(t *testing.T) {
	db := newNeo4j()
	_, err := db.Initialize(t.Context(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"uri":             "neo4j://database.example:7687",
			"username":        "neo4j",
			"password":        "password",
			"tls_certificate": "certificate",
		},
	})
	require.ErrorContains(t, err, "both tls_certificate and tls_key are required")
}

func TestNeo4j_StatementParsing(t *testing.T) {
	raw := `{"roles":["reader","editor"]}`
	var s neo4jStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, []string{"reader", "editor"}, s.Roles)
}

func TestNeo4j_ContainsBacktick(t *testing.T) {
	require.False(t, containsBacktick(""))
	require.False(t, containsBacktick("reader"))
	require.True(t, containsBacktick("role`name"))
}

func TestNeo4j_UpdateUser_Validation(t *testing.T) {
	db := newNeo4j()
	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")
}

func TestNeo4j_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("NEO4J_URI") == "" {
		t.Skip("set BAO_ACC=1 and NEO4J_URI to run Neo4j acceptance tests")
	}
}
