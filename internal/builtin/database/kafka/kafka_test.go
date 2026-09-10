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
	"github.com/twmb/franz-go/pkg/kerr"
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

func TestKafka_SanitizeBrokers(t *testing.T) {
	cases := []struct {
		input    []string
		expected []string
	}{
		{
			input:    []string{"127.0.0.1:9092"},
			expected: []string{"127.0.0.1:9092"},
		},
		{
			input:    []string{"tcp://127.0.0.1:9092"},
			expected: []string{"127.0.0.1:9092"},
		},
		{
			input:    []string{"kafka://broker.default.svc:9092/"},
			expected: []string{"broker.default.svc:9092"},
		},
		{
			input:    []string{"//broker.default.svc:9092"},
			expected: []string{"broker.default.svc:9092"},
		},
		{
			input:    []string{"tcp://b1:9092, tcp://b2:9092, b3:9092"},
			expected: []string{"b1:9092", "b2:9092", "b3:9092"},
		},
		{
			input:    []string{"", "   "},
			expected: nil,
		},
	}
	for _, tc := range cases {
		actual := sanitizeBrokers(tc.input)
		require.Equal(t, tc.expected, actual)
	}
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

func TestKafka_DeleteUser_Validation(t *testing.T) {
	db := newKafka()
	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")
}

func TestKafka_NotInitialized(t *testing.T) {
	db := newKafka()

	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		Statements: dbplugin.Statements{
			Commands: []string{`{"mechanism":"SCRAM-SHA-256"}`},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: "u",
		Password: &dbplugin.ChangePassword{NewPassword: "p"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{
		Username: "u",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")
}

func TestKafka_CheckAlteredSCRAMs(t *testing.T) {
	// Success case
	altered := kadm.AlteredUserSCRAMs{
		"user1": kadm.AlteredUserSCRAM{User: "user1", Err: nil},
	}
	require.NoError(t, checkAlteredSCRAMs(altered, false))

	// Broker error case
	msg := "broker authorization error"
	alteredErr := kadm.AlteredUserSCRAMs{
		"user1": kadm.AlteredUserSCRAM{
			User:       "user1",
			Err:        kerr.ClusterAuthorizationFailed,
			ErrMessage: msg,
		},
	}
	err := checkAlteredSCRAMs(alteredErr, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), msg)

	// ResourceNotFound ignored when ignoreNotFound=true
	alteredNotFound := kadm.AlteredUserSCRAMs{
		"user1": kadm.AlteredUserSCRAM{
			User: "user1",
			Err:  kerr.ResourceNotFound,
		},
	}
	require.NoError(t, checkAlteredSCRAMs(alteredNotFound, true))
	require.Error(t, checkAlteredSCRAMs(alteredNotFound, false))
}

func TestKafka_CheckDeleteACLResults(t *testing.T) {
	// Empty / no match case
	require.NoError(t, checkDeleteACLResults(nil))
	require.NoError(t, checkDeleteACLResults(kadm.DeleteACLsResults{}))

	// Filter level error
	resErr := kadm.DeleteACLsResults{
		{
			Err:        kerr.ClusterAuthorizationFailed,
			ErrMessage: "unauthorized to delete ACLs",
		},
	}
	err := checkDeleteACLResults(resErr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized to delete ACLs")

	// Matching ACL level error
	resMatchingErr := kadm.DeleteACLsResults{
		{
			Deleted: []kadm.DeletedACL{
				{
					Err:        kerr.SecurityDisabled,
					ErrMessage: "security is disabled",
				},
			},
		},
	}
	err = checkDeleteACLResults(resMatchingErr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "security is disabled")
}

func TestKafka_ScramUpsertsForUser(t *testing.T) {
	// Case 1: User with existing SCRAM-SHA-256
	desc256 := kadm.DescribedUserSCRAM{
		User: "u1",
		CredInfos: []kadm.CredInfo{
			{Mechanism: kadm.ScramSha256, Iterations: 4096},
		},
	}
	upserts, err := scramUpsertsForUser("u1", "p1", desc256, kadm.ScramSha256)
	require.NoError(t, err)
	require.Len(t, upserts, 1)
	require.Equal(t, kadm.ScramSha256, upserts[0].Mechanism)
	require.Equal(t, int32(4096), upserts[0].Iterations)
	require.Equal(t, "p1", upserts[0].Password)

	// Case 2: User with existing SCRAM-SHA-512 and custom iterations
	desc512 := kadm.DescribedUserSCRAM{
		User: "u2",
		CredInfos: []kadm.CredInfo{
			{Mechanism: kadm.ScramSha512, Iterations: 8192},
		},
	}
	upserts, err = scramUpsertsForUser("u2", "p2", desc512, kadm.ScramSha256)
	require.NoError(t, err)
	require.Len(t, upserts, 1)
	require.Equal(t, kadm.ScramSha512, upserts[0].Mechanism)
	require.Equal(t, int32(8192), upserts[0].Iterations)
	require.Equal(t, "p2", upserts[0].Password)

	// Case 3: User with multiple mechanisms (both 256 and 512)
	descMulti := kadm.DescribedUserSCRAM{
		User: "u3",
		CredInfos: []kadm.CredInfo{
			{Mechanism: kadm.ScramSha256, Iterations: 4096},
			{Mechanism: kadm.ScramSha512, Iterations: 10000},
		},
	}
	upserts, err = scramUpsertsForUser("u3", "p3", descMulti, kadm.ScramSha256)
	require.NoError(t, err)
	require.Len(t, upserts, 2)
	require.Equal(t, kadm.ScramSha256, upserts[0].Mechanism)
	require.Equal(t, int32(4096), upserts[0].Iterations)
	require.Equal(t, kadm.ScramSha512, upserts[1].Mechanism)
	require.Equal(t, int32(10000), upserts[1].Iterations)

	// Case 4: User not found (ResourceNotFound) -> fallback
	descNotFound := kadm.DescribedUserSCRAM{
		User: "u4",
		Err:  kerr.ResourceNotFound,
	}
	upserts, err = scramUpsertsForUser("u4", "p4", descNotFound, kadm.ScramSha512)
	require.NoError(t, err)
	require.Len(t, upserts, 1)
	require.Equal(t, kadm.ScramSha512, upserts[0].Mechanism)
	require.Equal(t, int32(4096), upserts[0].Iterations)
	require.Equal(t, "p4", upserts[0].Password)

	// Case 5: Empty description (not in map) -> fallback
	var descEmpty kadm.DescribedUserSCRAM
	upserts, err = scramUpsertsForUser("u5", "p5", descEmpty, kadm.ScramSha256)
	require.NoError(t, err)
	require.Len(t, upserts, 1)
	require.Equal(t, kadm.ScramSha256, upserts[0].Mechanism)
	require.Equal(t, int32(4096), upserts[0].Iterations)

	// Case 6: Broker error
	descErr := kadm.DescribedUserSCRAM{
		User:       "u6",
		Err:        kerr.ClusterAuthorizationFailed,
		ErrMessage: "not authorized",
	}
	_, err = scramUpsertsForUser("u6", "p6", descErr, kadm.ScramSha256)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not authorized")
}

func TestKafka_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("KAFKA_BROKERS") == "" {
		t.Skip("set BAO_ACC=1 and KAFKA_BROKERS to run Kafka acceptance tests")
	}
}
