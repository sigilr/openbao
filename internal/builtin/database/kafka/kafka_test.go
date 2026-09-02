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

func TestKafka_ParseACLOperation(t *testing.T) {
	cases := []struct {
		input    string
		expected kadm.ACLOperation
		err      bool
	}{
		{"READ", kadm.OpRead, false},
		{"Write", kadm.OpWrite, false},
		{"CREATE", kadm.OpCreate, false},
		{"DELETE", kadm.OpDelete, false},
		{"ALTER", kadm.OpAlter, false},
		{"DESCRIBE", kadm.OpDescribe, false},
		{"CLUSTER_ACTION", kadm.OpClusterAction, false},
		{"DESCRIBE_CONFIGS", kadm.OpDescribeConfigs, false},
		{"ALTER_CONFIGS", kadm.OpAlterConfigs, false},
		{"IDEMPOTENT_WRITE", kadm.OpIdempotentWrite, false},
		{"ALL", kadm.OpAll, false},
		{"UNKNOWN_OP", 0, true},
	}
	for _, tc := range cases {
		op, err := parseACLOperation(tc.input)
		if tc.err {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.expected, op)
		}
	}
}

func TestKafka_ParseACLPattern(t *testing.T) {
	p, err := parseACLPattern("LITERAL")
	require.NoError(t, err)
	require.Equal(t, kadm.ACLPatternLiteral, p)

	p, err = parseACLPattern("prefixed")
	require.NoError(t, err)
	require.Equal(t, kadm.ACLPatternPrefixed, p)

	p, err = parseACLPattern("")
	require.NoError(t, err)
	require.Equal(t, kadm.ACLPatternLiteral, p)

	_, err = parseACLPattern("INVALID")
	require.Error(t, err)
}

func TestKafka_StatementParsingWithACLs(t *testing.T) {
	raw := `{"mechanism":"SCRAM-SHA-256","iterations":4096,"acls":[{"resource_type":"TOPIC","resource_name":"my-topic","pattern_type":"LITERAL","operation":"WRITE","permission":"ALLOW"},{"resource_type":"GROUP","resource_name":"my-group","operation":"READ","permission":"ALLOW"}]}`
	var s kafkaStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, "SCRAM-SHA-256", s.Mechanism)
	require.Equal(t, 4096, s.Iterations)
	require.Len(t, s.ACLs, 2)
	require.Equal(t, "TOPIC", s.ACLs[0].ResourceType)
	require.Equal(t, "my-topic", s.ACLs[0].ResourceName)
	require.Equal(t, "WRITE", s.ACLs[0].Operation)
	require.Equal(t, "ALLOW", s.ACLs[0].Permission)
	require.Equal(t, "GROUP", s.ACLs[1].ResourceType)
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
