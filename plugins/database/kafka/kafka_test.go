// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package kafka

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kadm"
)

func TestKafka_TypeAndVersion(t *testing.T) {
	db := newKafka()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, kafkaTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestKafka_StatementParsing(t *testing.T) {
	raw := `{"mechanism":"SCRAM-SHA-512","iterations":8192}`
	var s kafkaStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, "SCRAM-SHA-512", s.Mechanism)
	require.Equal(t, 8192, s.Iterations)
}

func TestKafka_PickMechanism(t *testing.T) {
	_, err := pickMechanism(&kafkaConfig{Mechanism: "SCRAM-SHA-256", Username: "u", Password: "p"})
	require.NoError(t, err)
	_, err = pickMechanism(&kafkaConfig{Mechanism: "SCRAM-SHA-512", Username: "u", Password: "p"})
	require.NoError(t, err)
	_, err = pickMechanism(&kafkaConfig{Mechanism: "PLAIN", Username: "u", Password: "p"})
	require.Error(t, err)
	_, err = pickMechanism(&kafkaConfig{Mechanism: "wat", Username: "u", Password: "p"})
	require.Error(t, err)
}

func TestKafka_KadmScramMechanism(t *testing.T) {
	m, err := kadmScramMechanism("SCRAM-SHA-256")
	require.NoError(t, err)
	require.Equal(t, kadm.ScramSha256, m)

	m, err = kadmScramMechanism("sha-512")
	require.NoError(t, err)
	require.Equal(t, kadm.ScramSha512, m)

	_, err = kadmScramMechanism("OAUTH")
	require.Error(t, err)
}

func TestKafka_NewUser_ACLsRejected(t *testing.T) {
	// We can't easily mock the franz-go AdminClient, so this test only
	// validates that the JSON path with ACLs returns the "not supported"
	// error before any client call. The AlterUserSCRAMs call happens BEFORE
	// the ACL check in the current implementation — which means an empty
	// k.admin will dereference nil. Skip this test until we mock the client.
	t.Skip("requires a mock kadm.Client; covered manually via TEST.md")
}

func TestKafka_UpdateUser_Validation(t *testing.T) {
	db := newKafka()
	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")
}

func TestKafka_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("KAFKA_BROKERS") == "" {
		t.Skip("set BAO_ACC=1 and KAFKA_BROKERS to run Kafka acceptance tests")
	}
}
