// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"math"
	"sync"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/openbao/openbao/sdk/v2/helper/pluginutil"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/plugin/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	_ plugin.Plugin     = (*GRPCRemoteBackendPlugin)(nil)
	_ plugin.GRPCPlugin = (*GRPCRemoteBackendPlugin)(nil)
)

// GRPCRemoteBackendPlugin is the plugin.Plugin implementation for remote
// (network) backend plugins. Unlike GRPCBackendPlugin, this variant's server
// side connects back to the host over the network (via _host_address in the
// config) instead of using the go-plugin broker for localhost connections.
type GRPCRemoteBackendPlugin struct {
	Factory      logical.Factory
	MetadataMode bool
	Logger       log.Logger

	MultiplexingSupport bool

	plugin.NetRPCUnsupportedPlugin
}

func (b GRPCRemoteBackendPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	server := &backendGRPCRemotePluginServer{
		factory:   b.Factory,
		instances: make(map[string]backendInstance),
		logger:    b.Logger,
	}

	if b.MultiplexingSupport {
		pluginutil.RegisterPluginMultiplexingServer(s, pluginutil.PluginMultiplexingServerImpl{
			Supported: true,
		})
		server.multiplexingSupport = true
	}

	pb.RegisterBackendServer(s, server)
	logical.RegisterPluginVersionServer(s, server)
	return nil
}

func (b *GRPCRemoteBackendPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &backendGRPCPluginClient{
		client:        pb.NewBackendClient(c),
		versionClient: logical.NewPluginVersionClient(c),
		broker:        broker,
		cleanupCh:     make(chan struct{}),
		doneCtx:       ctx,
		metadataMode:  b.MetadataMode,
	}, nil
}

// backendGRPCRemotePluginServer is the server-side implementation for remote
// backend plugins. It overrides Setup() to dial the host's network address
// (from _host_address in config) instead of using the localhost broker.
type backendGRPCRemotePluginServer struct {
	pb.UnimplementedBackendServer
	logical.UnimplementedPluginVersionServer

	instances           map[string]backendInstance
	instancesLock       sync.RWMutex
	multiplexingSupport bool

	factory logical.Factory

	logger log.Logger
}

// getBackendAndBrokeredClientInternal returns the backend and client
// connection but does not hold a lock.
func (b *backendGRPCRemotePluginServer) getBackendAndBrokeredClientInternal(ctx context.Context) (logical.Backend, *grpc.ClientConn, error) {
	if b.multiplexingSupport {
		id, err := pluginutil.GetMultiplexIDFromContext(ctx)
		if err != nil {
			return nil, nil, err
		}
		if inst, ok := b.instances[id]; ok {
			return inst.backend, inst.brokeredClient, nil
		}
	}

	if singleImpl, ok := b.instances[singleImplementationID]; ok {
		return singleImpl.backend, singleImpl.brokeredClient, nil
	}

	return nil, nil, errors.New("no backend instance found")
}

// getBackendAndBrokeredClient holds a read lock and returns the backend and
// client connection.
func (b *backendGRPCRemotePluginServer) getBackendAndBrokeredClient(ctx context.Context) (logical.Backend, *grpc.ClientConn, error) {
	b.instancesLock.RLock()
	defer b.instancesLock.RUnlock()
	return b.getBackendAndBrokeredClientInternal(ctx)
}

// Setup dials the host's network address (from _host_address in config) to get
// Storage and SystemView, then instantiates the backend through its factory.
func (b *backendGRPCRemotePluginServer) Setup(ctx context.Context, args *pb.SetupArgs) (*pb.SetupReply, error) {
	var err error
	id := singleImplementationID
	if b.multiplexingSupport {
		id, err = pluginutil.GetMultiplexIDFromContext(ctx)
		if err != nil {
			return &pb.SetupReply{}, err
		}
	}

	// Get the host address from config
	hostAddr := args.Config["_host_address"]
	if hostAddr == "" {
		return &pb.SetupReply{
			Err: "remote plugin requires _host_address in config",
		}, nil
	}

	// Dial the host's gRPC server for Storage and SystemView
	conn, err := grpc.Dial(hostAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(math.MaxInt32),
			grpc.MaxCallSendMsgSize(math.MaxInt32),
		),
	)
	if err != nil {
		return &pb.SetupReply{
			Err: pb.ErrToString(err),
		}, nil
	}

	storage, err := newGRPCStorageClient(ctx, conn)
	if err != nil {
		conn.Close()
		return &pb.SetupReply{
			Err: pb.ErrToString(err),
		}, nil
	}

	sysView := newGRPCSystemView(conn)

	config := &logical.BackendConfig{
		StorageView: storage,
		Logger:      b.logger,
		System:      sysView,
		Config:      args.Config,
		BackendUUID: args.BackendUUID,
	}

	backend, err := b.factory(ctx, config)
	if err != nil {
		conn.Close()
		return &pb.SetupReply{
			Err: pb.ErrToString(err),
		}, nil
	}

	b.instancesLock.Lock()
	defer b.instancesLock.Unlock()
	b.instances[id] = backendInstance{
		brokeredClient: conn,
		backend:        backend,
	}

	return &pb.SetupReply{}, nil
}

func (b *backendGRPCRemotePluginServer) HandleRequest(ctx context.Context, args *pb.HandleRequestArgs) (*pb.HandleRequestReply, error) {
	backend, brokeredClient, err := b.getBackendAndBrokeredClient(ctx)
	if err != nil {
		return &pb.HandleRequestReply{}, err
	}

	if pluginutil.InMetadataMode() {
		return &pb.HandleRequestReply{}, ErrServerInMetadataMode
	}

	logicalReq, err := pb.ProtoRequestToLogicalRequest(args.Request)
	if err != nil {
		return &pb.HandleRequestReply{}, err
	}

	logicalReq.Storage, err = newGRPCStorageClient(ctx, brokeredClient)
	if err != nil {
		return &pb.HandleRequestReply{}, err
	}

	resp, respErr := backend.HandleRequest(ctx, logicalReq)

	pbResp, err := pb.LogicalResponseToProtoResponse(resp)
	if err != nil {
		return &pb.HandleRequestReply{}, err
	}

	return &pb.HandleRequestReply{
		Response: pbResp,
		Err:      pb.ErrToProtoErr(respErr),
	}, nil
}

func (b *backendGRPCRemotePluginServer) Initialize(ctx context.Context, _ *pb.InitializeArgs) (*pb.InitializeReply, error) {
	backend, brokeredClient, err := b.getBackendAndBrokeredClient(ctx)
	if err != nil {
		return &pb.InitializeReply{}, err
	}

	if pluginutil.InMetadataMode() {
		return &pb.InitializeReply{}, ErrServerInMetadataMode
	}

	storage, err := newGRPCStorageClient(ctx, brokeredClient)
	if err != nil {
		return &pb.InitializeReply{}, err
	}

	req := &logical.InitializationRequest{
		Storage: storage,
	}

	respErr := backend.Initialize(ctx, req)

	return &pb.InitializeReply{
		Err: pb.ErrToProtoErr(respErr),
	}, nil
}

func (b *backendGRPCRemotePluginServer) SpecialPaths(ctx context.Context, args *pb.Empty) (*pb.SpecialPathsReply, error) {
	backend, _, err := b.getBackendAndBrokeredClient(ctx)
	if err != nil {
		return &pb.SpecialPathsReply{}, err
	}

	paths := backend.SpecialPaths()
	if paths == nil {
		return &pb.SpecialPathsReply{
			Paths: nil,
		}, nil
	}

	return &pb.SpecialPathsReply{
		Paths: &pb.Paths{
			Root:                  paths.Root,
			Unauthenticated:       paths.Unauthenticated,
			LocalStorage:          paths.LocalStorage,
			SealWrapStorage:       paths.SealWrapStorage,
			WriteForwardedStorage: paths.WriteForwardedStorage,
		},
	}, nil
}

func (b *backendGRPCRemotePluginServer) HandleExistenceCheck(ctx context.Context, args *pb.HandleExistenceCheckArgs) (*pb.HandleExistenceCheckReply, error) {
	backend, brokeredClient, err := b.getBackendAndBrokeredClient(ctx)
	if err != nil {
		return &pb.HandleExistenceCheckReply{}, err
	}

	if pluginutil.InMetadataMode() {
		return &pb.HandleExistenceCheckReply{}, ErrServerInMetadataMode
	}

	logicalReq, err := pb.ProtoRequestToLogicalRequest(args.Request)
	if err != nil {
		return &pb.HandleExistenceCheckReply{}, err
	}

	logicalReq.Storage, err = newGRPCStorageClient(ctx, brokeredClient)
	if err != nil {
		return &pb.HandleExistenceCheckReply{}, err
	}

	checkFound, exists, err := backend.HandleExistenceCheck(ctx, logicalReq)
	return &pb.HandleExistenceCheckReply{
		CheckFound: checkFound,
		Exists:     exists,
		Err:        pb.ErrToProtoErr(err),
	}, nil
}

func (b *backendGRPCRemotePluginServer) Cleanup(ctx context.Context, _ *pb.Empty) (*pb.Empty, error) {
	b.instancesLock.Lock()
	defer b.instancesLock.Unlock()

	backend, brokeredClient, err := b.getBackendAndBrokeredClientInternal(ctx)
	if err != nil {
		return &pb.Empty{}, err
	}

	backend.Cleanup(ctx)

	brokeredClient.Close()

	if b.multiplexingSupport {
		id, err := pluginutil.GetMultiplexIDFromContext(ctx)
		if err != nil {
			return nil, err
		}
		delete(b.instances, id)
	} else {
		delete(b.instances, singleImplementationID)
	}

	return &pb.Empty{}, nil
}

func (b *backendGRPCRemotePluginServer) InvalidateKey(ctx context.Context, args *pb.InvalidateKeyArgs) (*pb.Empty, error) {
	backend, _, err := b.getBackendAndBrokeredClient(ctx)
	if err != nil {
		return &pb.Empty{}, err
	}

	if pluginutil.InMetadataMode() {
		return &pb.Empty{}, ErrServerInMetadataMode
	}

	backend.InvalidateKey(ctx, args.Key)
	return &pb.Empty{}, nil
}

func (b *backendGRPCRemotePluginServer) Type(ctx context.Context, _ *pb.Empty) (*pb.TypeReply, error) {
	backend, _, err := b.getBackendAndBrokeredClient(ctx)
	if err != nil {
		return &pb.TypeReply{}, err
	}

	return &pb.TypeReply{
		Type: uint32(backend.Type()),
	}, nil
}

func (b *backendGRPCRemotePluginServer) Version(ctx context.Context, _ *logical.Empty) (*logical.VersionReply, error) {
	backend, _, err := b.getBackendAndBrokeredClient(ctx)
	if err != nil {
		return &logical.VersionReply{}, err
	}

	if versioner, ok := backend.(logical.PluginVersioner); ok {
		return &logical.VersionReply{
			PluginVersion: versioner.PluginVersion().Version,
		}, nil
	}
	return &logical.VersionReply{
		PluginVersion: "",
	}, nil
}
