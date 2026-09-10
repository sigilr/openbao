// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package milvus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type fakeMilvusServer struct {
	milvuspb.UnimplementedMilvusServiceServer

	connectRequests   []*milvuspb.ConnectRequest
	createRequests    []*milvuspb.CreateCredentialRequest
	roleRequests      []*milvuspb.OperateUserRoleRequest
	updateRequests    []*milvuspb.UpdateCredentialRequest
	deleteRequests    []*milvuspb.DeleteCredentialRequest
	createRoleReqs    []*milvuspb.CreateRoleRequest
	operatePrivReqs   []*milvuspb.OperatePrivilegeRequest
	createStatus      *commonpb.Status
	roleStatus        *commonpb.Status
	updateStatus      *commonpb.Status
	deleteStatus      *commonpb.Status
	createRoleStatus  *commonpb.Status
	operatePrivStatus *commonpb.Status
}

func successStatus() *commonpb.Status {
	return &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success}
}

func (s *fakeMilvusServer) Connect(_ context.Context, req *milvuspb.ConnectRequest) (*milvuspb.ConnectResponse, error) {
	s.connectRequests = append(s.connectRequests, req)
	return &milvuspb.ConnectResponse{Status: successStatus(), Identifier: 1}, nil
}

func (s *fakeMilvusServer) GetVersion(context.Context, *milvuspb.GetVersionRequest) (*milvuspb.GetVersionResponse, error) {
	return &milvuspb.GetVersionResponse{Status: successStatus(), Version: "2.4.0"}, nil
}

func (s *fakeMilvusServer) CreateCredential(_ context.Context, req *milvuspb.CreateCredentialRequest) (*commonpb.Status, error) {
	s.createRequests = append(s.createRequests, req)
	if s.createStatus != nil {
		return s.createStatus, nil
	}
	return successStatus(), nil
}

func (s *fakeMilvusServer) OperateUserRole(_ context.Context, req *milvuspb.OperateUserRoleRequest) (*commonpb.Status, error) {
	s.roleRequests = append(s.roleRequests, req)
	if s.roleStatus != nil {
		return s.roleStatus, nil
	}
	return successStatus(), nil
}

func (s *fakeMilvusServer) UpdateCredential(_ context.Context, req *milvuspb.UpdateCredentialRequest) (*commonpb.Status, error) {
	s.updateRequests = append(s.updateRequests, req)
	if s.updateStatus != nil {
		return s.updateStatus, nil
	}
	return successStatus(), nil
}

func (s *fakeMilvusServer) DeleteCredential(_ context.Context, req *milvuspb.DeleteCredentialRequest) (*commonpb.Status, error) {
	s.deleteRequests = append(s.deleteRequests, req)
	if s.deleteStatus != nil {
		return s.deleteStatus, nil
	}
	return successStatus(), nil
}

func (s *fakeMilvusServer) CreateRole(_ context.Context, req *milvuspb.CreateRoleRequest) (*commonpb.Status, error) {
	s.createRoleReqs = append(s.createRoleReqs, req)
	if s.createRoleStatus != nil {
		return s.createRoleStatus, nil
	}
	return successStatus(), nil
}

func (s *fakeMilvusServer) OperatePrivilege(_ context.Context, req *milvuspb.OperatePrivilegeRequest) (*commonpb.Status, error) {
	s.operatePrivReqs = append(s.operatePrivReqs, req)
	if s.operatePrivStatus != nil {
		return s.operatePrivStatus, nil
	}
	return successStatus(), nil
}

func startFakeMilvusServer(t *testing.T, server *fakeMilvusServer) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	milvuspb.RegisterMilvusServiceServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	return listener.Addr().String()
}

func initializeTestMilvus(t *testing.T, server *fakeMilvusServer) *Milvus {
	t.Helper()

	db := newMilvus()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":      startFakeMilvusServer(t, server),
			"username": "root",
			"password": "Milvus123",
		},
		VerifyConnection: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return db
}

func decodePassword(t *testing.T, password string) string {
	t.Helper()

	decoded, err := base64.StdEncoding.DecodeString(password)
	require.NoError(t, err)
	return string(decoded)
}

func TestMilvus_TypeAndVersion(t *testing.T) {
	db := newMilvus()
	typ, err := db.Type()
	require.NoError(t, err)
	require.Equal(t, milvusTypeName, typ)
	require.Equal(t, ReportedVersion, db.PluginVersion().Version)
}

func TestMilvus_StatementParsing(t *testing.T) {
	t.Run("basic roles", func(t *testing.T) {
		raw := `{"roles":["admin","reader"]}`
		var s milvusStatement
		require.NoError(t, json.Unmarshal([]byte(raw), &s))
		require.Equal(t, []string{"admin", "reader"}, s.Roles)
	})

	t.Run("structured with custom roles and aliases", func(t *testing.T) {
		raw := `{
			"role": "public",
			"customRoles": [
				{
					"name": "collection_reader",
					"permissions": [
						{
							"collection": "Products",
							"action": "Search",
							"dbName": "default"
						}
					]
				}
			]
		}`
		var s milvusStatement
		require.NoError(t, json.Unmarshal([]byte(raw), &s))
		require.Equal(t, []string{"public"}, s.Roles)
		require.Len(t, s.CustomRoles, 1)
		require.Equal(t, "collection_reader", s.CustomRoles[0].Name)
		require.Len(t, s.CustomRoles[0].Privileges, 1)
		p := s.CustomRoles[0].Privileges[0]
		require.Equal(t, "Products", p.ObjectName)
		require.Equal(t, "Search", p.Privilege)
		require.Equal(t, "default", p.DBName)
	})
}

func TestMilvus_CredentialLifecycle(t *testing.T) {
	server := &fakeMilvusServer{}
	db := initializeTestMilvus(t, server)

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{`{"roles":["public"]}`}},
		Password:       "BaoMilvusPass123",
		Expiration:     time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)

	// Password rotation is unsupported because Milvus requires the old password.
	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "BaoMilvusPass456"},
	})
	require.ErrorContains(t, err, "milvus does not support updating user credentials or static roles")

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: resp.Username})
	require.NoError(t, err)

	require.Len(t, server.createRequests, 1)
	require.Equal(t, resp.Username, server.createRequests[0].GetUsername())
	require.Equal(t, "BaoMilvusPass123", decodePassword(t, server.createRequests[0].GetPassword()))

	require.Len(t, server.roleRequests, 1)
	require.Equal(t, resp.Username, server.roleRequests[0].GetUsername())
	require.Equal(t, "public", server.roleRequests[0].GetRoleName())
	require.Equal(t, milvuspb.OperateUserRoleType_AddUserToRole, server.roleRequests[0].GetType())

	// UpdateUser rejected the rotation without changing the credential.
	require.Empty(t, server.updateRequests)

	require.Len(t, server.deleteRequests, 1)
	require.Equal(t, resp.Username, server.deleteRequests[0].GetUsername())
}

func TestMilvus_CreateCredentialError(t *testing.T) {
	server := &fakeMilvusServer{
		createStatus: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_CreateCredentialFailure,
			Reason:    "user already exists",
		},
	}
	db := initializeTestMilvus(t, server)

	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{`{"roles":["public"]}`}},
		Password:       "BaoMilvusPass123",
	})
	require.ErrorContains(t, err, "user already exists")
	require.Empty(t, server.roleRequests)
	require.Empty(t, server.deleteRequests)
}

func TestMilvus_RoleGrantFailureDeletesCredential(t *testing.T) {
	server := &fakeMilvusServer{
		roleStatus: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_OperateUserRoleFailure,
			Reason:    "role does not exist",
		},
	}
	db := initializeTestMilvus(t, server)

	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{`{"roles":["missing"]}`}},
		Password:       "BaoMilvusPass123",
	})
	require.ErrorContains(t, err, "role does not exist")
	require.Len(t, server.createRequests, 1)
	require.Len(t, server.roleRequests, 1)
	require.Len(t, server.deleteRequests, 1)
	require.Equal(t, server.createRequests[0].GetUsername(), server.deleteRequests[0].GetUsername())
}

func TestMilvus_NewUser_StructuredRoles(t *testing.T) {
	server := &fakeMilvusServer{}
	db := initializeTestMilvus(t, server)

	stmt := `{
		"roles": ["public"],
		"custom_roles": [
			{
				"name": "custom_reader",
				"privileges": [
					{
						"object_type": "Collection",
						"object_name": "Products",
						"privilege": "Search",
						"db_name": "default"
					}
				]
			}
		]
	}`

	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{stmt}},
		Password:       "Pass123!",
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)

	// Role creation
	require.Len(t, server.createRoleReqs, 1)
	require.Equal(t, "custom_reader", server.createRoleReqs[0].GetEntity().GetName())

	// Privilege grant
	require.Len(t, server.operatePrivReqs, 1)
	require.Equal(t, milvuspb.OperatePrivilegeType_Grant, server.operatePrivReqs[0].GetType())
	require.Equal(t, "custom_reader", server.operatePrivReqs[0].GetEntity().GetRole().GetName())
	require.Equal(t, "Products", server.operatePrivReqs[0].GetEntity().GetObjectName())
	require.Equal(t, "Collection", server.operatePrivReqs[0].GetEntity().GetObject().GetName())
	require.Equal(t, "Search", server.operatePrivReqs[0].GetEntity().GetGrantor().GetPrivilege().GetName())
	require.Equal(t, "default", server.operatePrivReqs[0].GetEntity().GetDbName())

	// User role assignments: both "public" and "custom_reader"
	require.Len(t, server.roleRequests, 2)
	require.Equal(t, "public", server.roleRequests[0].GetRoleName())
	require.Equal(t, "custom_reader", server.roleRequests[1].GetRoleName())
}

func TestMilvus_NewUser_CustomRoles(t *testing.T) {
	t.Run("single custom role JSON", func(t *testing.T) {
		server := &fakeMilvusServer{}
		db := initializeTestMilvus(t, server)

		stmt := `{
			"name": "writer_role",
			"privileges": [
				{
					"object_type": "Collection",
					"object_name": "Logs",
					"privilege": "Insert"
				}
			]
		}`

		resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
			Statements:     dbplugin.Statements{Commands: []string{stmt}},
			Password:       "Pass123!",
		})
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)

		require.Len(t, server.createRoleReqs, 1)
		require.Equal(t, "writer_role", server.createRoleReqs[0].GetEntity().GetName())
		require.Len(t, server.operatePrivReqs, 1)
		require.Equal(t, "writer_role", server.operatePrivReqs[0].GetEntity().GetRole().GetName())
		require.Equal(t, "Logs", server.operatePrivReqs[0].GetEntity().GetObjectName())
		require.Equal(t, "Insert", server.operatePrivReqs[0].GetEntity().GetGrantor().GetPrivilege().GetName())

		require.Len(t, server.roleRequests, 1)
		require.Equal(t, "writer_role", server.roleRequests[0].GetRoleName())
	})

	t.Run("array of custom roles", func(t *testing.T) {
		server := &fakeMilvusServer{}
		db := initializeTestMilvus(t, server)

		stmt := `[
			{
				"name": "global_admin",
				"privileges": [
					{
						"object_type": "Global",
						"privilege": "All"
					}
				]
			}
		]`

		resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
			Statements:     dbplugin.Statements{Commands: []string{stmt}},
			Password:       "Pass123!",
		})
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)

		require.Len(t, server.createRoleReqs, 1)
		require.Equal(t, "global_admin", server.createRoleReqs[0].GetEntity().GetName())
		require.Len(t, server.operatePrivReqs, 1)
		require.Equal(t, "*", server.operatePrivReqs[0].GetEntity().GetObjectName())
		require.Equal(t, "Global", server.operatePrivReqs[0].GetEntity().GetObject().GetName())
		require.Equal(t, "All", server.operatePrivReqs[0].GetEntity().GetGrantor().GetPrivilege().GetName())
	})

	t.Run("string array and plain string", func(t *testing.T) {
		server := &fakeMilvusServer{}
		db := initializeTestMilvus(t, server)

		resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
			UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
			Statements:     dbplugin.Statements{Commands: []string{`["roleA", "roleB"]`, "roleC, roleD"}},
			Password:       "Pass123!",
		})
		require.NoError(t, err)
		require.NotEmpty(t, resp.Username)
		require.Len(t, server.roleRequests, 4)
	})
}

func TestMilvus_NewUser_RoleAlreadyExists(t *testing.T) {
	server := &fakeMilvusServer{
		createRoleStatus: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    "role already exists",
		},
	}
	db := initializeTestMilvus(t, server)

	stmt := `{"custom_roles": [{"name": "existing_role", "privileges": [{"privilege": "Search", "collection": "Col"}]}]}`
	resp, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
		Statements:     dbplugin.Statements{Commands: []string{stmt}},
		Password:       "Pass123!",
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Username)
	require.Len(t, server.createRoleReqs, 1)
	require.Len(t, server.operatePrivReqs, 1)
	require.Len(t, server.roleRequests, 1)
}

func TestMilvus_NewUser_InvalidStatements(t *testing.T) {
	server := &fakeMilvusServer{}
	db := initializeTestMilvus(t, server)

	tests := []struct {
		name    string
		stmt    string
		wantErr string
	}{
		{
			name:    "malformed json object",
			stmt:    `{"roles":`,
			wantErr: "failed to parse role statement JSON",
		},
		{
			name:    "malformed json array",
			stmt:    `["roles"`,
			wantErr: "failed to parse role statement JSON array",
		},
		{
			name:    "custom role missing name",
			stmt:    `{"custom_roles": [{"privileges": []}]}`,
			wantErr: "custom role definition missing name",
		},
		{
			name:    "privilege missing action name",
			stmt:    `{"custom_roles": [{"name": "r1", "privileges": [{"object_name": "col"}]}]}`,
			wantErr: "missing privilege/action name",
		},
		{
			name:    "unknown object type",
			stmt:    `{"custom_roles": [{"name": "r1", "privileges": [{"object_type": "invalid", "privilege": "Search"}]}]}`,
			wantErr: "unknown privilege object type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
				UsernameConfig: dbplugin.UsernameMetadata{DisplayName: "t", RoleName: "t"},
				Statements:     dbplugin.Statements{Commands: []string{tc.stmt}},
				Password:       "Pass123!",
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestMilvus_NotInitialized(t *testing.T) {
	db := newMilvus()

	_, err := db.NewUser(context.Background(), dbplugin.NewUserRequest{
		Statements: dbplugin.Statements{Commands: []string{`{"roles":["public"]}`}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: "u",
		Password: &dbplugin.ChangePassword{NewPassword: "p"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "milvus does not support updating user credentials or static roles")

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database not initialized")
}

func TestMilvus_UpdateUser_Validation(t *testing.T) {
	db := newMilvus()

	_, err := db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{Username: "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no changes requested")

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username:   "u",
		Expiration: &dbplugin.ChangeExpiration{NewExpiration: time.Now().Add(time.Hour)},
	})
	require.NoError(t, err)
}

func TestMilvus_InitializeWithoutVerifyConnection(t *testing.T) {
	server := &fakeMilvusServer{}
	db := newMilvus()
	_, err := db.Initialize(context.Background(), dbplugin.InitializeRequest{
		Config: map[string]any{
			"url":      startFakeMilvusServer(t, server),
			"username": "root",
			"password": "Milvus123",
		},
		VerifyConnection: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	require.Empty(t, server.connectRequests)
}

func TestMilvus_DeleteUser_Validation(t *testing.T) {
	db := newMilvus()

	_, err := db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing username")
}

func TestMilvus_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("MILVUS_URL") == "" {
		t.Skip("set BAO_ACC=1 and MILVUS_URL to run Milvus acceptance tests")
	}
}
