// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"math"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/openbao/openbao/sdk/v2/helper/pluginutil"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// ServeRemoteOpts configures a remote plugin server. Remote plugins run as
// standalone gRPC servers on the network rather than as local subprocesses.
type ServeRemoteOpts struct {
	// Addr is the listen address for the remote plugin server (e.g. ":50051").
	Addr string

	// BackendFactoryFunc is the logical.Factory for the backend plugin.
	BackendFactoryFunc logical.Factory

	// MultiplexingSupport enables multiplexing for this plugin.
	MultiplexingSupport bool

	Logger log.Logger
}

// ServeRemote serves a backend plugin as a remote gRPC server on the network.
// This should be called from a standalone plugin binary's main function.
// The plugin binary listens on the given address and the host connects to it.
//
// Unlike the local Serve/ServeMultiplex which use subprocess communication,
// ServeRemote creates a network listener and serves gRPC directly.
func ServeRemote(opts *ServeRemoteOpts) error {
	logger := opts.Logger
	if logger == nil {
		logger = log.New(&log.LoggerOptions{
			Level:      log.Info,
			Output:     os.Stderr,
			JSONFormat: true,
		})
	}

	err := pluginutil.OptionallyEnableMlock()
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return err
	}

	logger.Info("remote plugin server listening", "addr", lis.Addr().String())

	grpcServer := plugin.NewGRPCRemoteServer(
		grpc.MaxRecvMsgSize(math.MaxInt32),
		grpc.MaxSendMsgSize(math.MaxInt32),
	)

	// Register health check
	healthCheck := health.NewServer()
	healthCheck.SetServingStatus(
		plugin.GRPCServiceName,
		grpc_health_v1.HealthCheckResponse_SERVING,
	)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthCheck)

	// Register the remote backend plugin. This plugin variant connects to the
	// host's Storage and SystemView over the network (via _host_address in
	// config) instead of using the localhost broker.
	remotePlugin := &GRPCRemoteBackendPlugin{
		Factory:             opts.BackendFactoryFunc,
		MultiplexingSupport: opts.MultiplexingSupport,
		Logger:              logger,
	}
	if err := remotePlugin.GRPCServer(nil, grpcServer); err != nil {
		return err
	}

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("shutting down remote plugin server")
		grpcServer.GracefulStop()
	}()

	logger.Info("remote plugin server ready", "addr", lis.Addr().String())

	if err := grpcServer.Serve(lis); err != nil {
		return err
	}

	return nil
}
