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

	createRequests []*milvuspb.CreateCredentialRequest
	roleRequests   []*milvuspb.OperateUserRoleRequest
	updateRequests []*milvuspb.UpdateCredentialRequest
	deleteRequests []*milvuspb.DeleteCredentialRequest
	createStatus   *commonpb.Status
	roleStatus     *commonpb.Status
	updateStatus   *commonpb.Status
	deleteStatus   *commonpb.Status
}

func successStatus() *commonpb.Status {
	return &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success}
}

func (s *fakeMilvusServer) Connect(context.Context, *milvuspb.ConnectRequest) (*milvuspb.ConnectResponse, error) {
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
	raw := `{"roles":["admin","reader"]}`
	var s milvusStatement
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, []string{"admin", "reader"}, s.Roles)
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

	_, err = db.UpdateUser(context.Background(), dbplugin.UpdateUserRequest{
		Username: resp.Username,
		Password: &dbplugin.ChangePassword{NewPassword: "BaoMilvusPass456"},
	})
	require.NoError(t, err)

	_, err = db.DeleteUser(context.Background(), dbplugin.DeleteUserRequest{Username: resp.Username})
	require.NoError(t, err)

	require.Len(t, server.createRequests, 1)
	require.Equal(t, resp.Username, server.createRequests[0].GetUsername())
	require.Equal(t, "BaoMilvusPass123", decodePassword(t, server.createRequests[0].GetPassword()))

	require.Len(t, server.roleRequests, 1)
	require.Equal(t, resp.Username, server.roleRequests[0].GetUsername())
	require.Equal(t, "public", server.roleRequests[0].GetRoleName())
	require.Equal(t, milvuspb.OperateUserRoleType_AddUserToRole, server.roleRequests[0].GetType())

	require.Len(t, server.updateRequests, 1)
	require.Equal(t, resp.Username, server.updateRequests[0].GetUsername())
	require.Empty(t, decodePassword(t, server.updateRequests[0].GetOldPassword()))
	require.Equal(t, "BaoMilvusPass456", decodePassword(t, server.updateRequests[0].GetNewPassword()))

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

func TestMilvus_Acceptance(t *testing.T) {
	if os.Getenv("BAO_ACC") != "1" || os.Getenv("MILVUS_URL") == "" {
		t.Skip("set BAO_ACC=1 and MILVUS_URL to run Milvus acceptance tests")
	}
}
