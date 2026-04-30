// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// Package agentserver implements a hub-side gRPC server that spoke agents
// connect to. It is a singleton embedded inside the OpenBao process — no
// separate grpc-agent binary needed on the hub.
//
// Spoke agents (spoke-agent binary) connect by calling Connect() and send
// their cluster name as the first message. The server then routes commands
// from RunCommand() to the right spoke and waits for the response.
package agentserver

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	agentproto "github.com/openbao/openbao/plugins/database/remote-postgres/proto"
	"google.golang.org/grpc"
)

// spokeConn holds the state for one connected spoke agent.
type spokeConn struct {
	stream agentproto.AgentService_ConnectServer
	respCh chan string // receives output from spoke after command execution
	mu     sync.Mutex  // serializes concurrent requests to this spoke
}

// Server is the embedded gRPC server that spoke agents connect to.
// It mirrors the role of "grpc-agent init" but runs inside OpenBao.
type Server struct {
	agentproto.UnimplementedAgentServiceServer
	mu     sync.RWMutex
	spokes map[string]*spokeConn
}

var (
	instance *Server
	once     sync.Once
)

// Instance returns the singleton Server, creating it if needed.
func Instance() *Server {
	once.Do(func() {
		instance = &Server{
			spokes: make(map[string]*spokeConn),
		}
	})
	return instance
}

// Start begins listening for spoke agent connections on the given port.
// Safe to call multiple times — subsequent calls are no-ops.
func (s *Server) Start(port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("spoke server failed to listen on port %d: %w", port, err)
	}
	srv := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(srv, s)
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("[agentserver] gRPC server stopped: %v", err)
		}
	}()
	log.Printf("[agentserver] spoke server listening on :%d", port)
	return nil
}

// Connect implements AgentService.Connect. It is called by each spoke agent.
// Phase 1: spoke sends {client_name: "spoke-cluster-1"} → server registers it.
// Phase 2: server sends commands, spoke sends back responses via IsResponse=true.
func (s *Server) Connect(stream agentproto.AgentService_ConnectServer) error {
	var spokeName string
	var conn *spokeConn

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			if spokeName != "" {
				s.mu.Lock()
				delete(s.spokes, spokeName)
				s.mu.Unlock()
				log.Printf("[agentserver] spoke %q disconnected", spokeName)
			}
			return nil
		}
		if err != nil {
			if spokeName != "" {
				s.mu.Lock()
				delete(s.spokes, spokeName)
				s.mu.Unlock()
			}
			return err
		}

		// First message from spoke — register it
		if spokeName == "" && !msg.IsResponse {
			spokeName = msg.ClientName
			conn = &spokeConn{
				stream: stream,
				respCh: make(chan string, 1),
			}
			s.mu.Lock()
			s.spokes[spokeName] = conn
			s.mu.Unlock()
			log.Printf("[agentserver] spoke %q registered", spokeName)

			if err := stream.Send(&agentproto.AgentMessage{
				ClientName: spokeName,
				Output:     "Connected to OpenBao spoke server",
				IsResponse: true,
			}); err != nil {
				return err
			}
			continue
		}

		// Response from spoke after executing a command
		if msg.IsResponse && conn != nil {
			select {
			case conn.respCh <- msg.Output:
			default:
				// No one waiting — discard (e.g. timed-out caller)
			}
		}
	}
}

// RunCommand sends command to the named spoke and waits for the output.
// Requests to the same spoke are serialized via spokeConn.mu.
func (s *Server) RunCommand(ctx context.Context, spokeName, command string) (string, error) {
	s.mu.RLock()
	conn, ok := s.spokes[spokeName]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("spoke %q is not connected — ensure spoke-agent is running in the spoke cluster", spokeName)
	}

	// One request at a time per spoke
	conn.mu.Lock()
	defer conn.mu.Unlock()

	// Drain any stale response from previous call
	select {
	case <-conn.respCh:
	default:
	}

	if err := conn.stream.Send(&agentproto.AgentMessage{
		ClientName: "remote-postgres-plugin",
		TargetName: spokeName,
		Command:    command,
		IsResponse: false,
	}); err != nil {
		return "", fmt.Errorf("failed to send command to spoke %q: %w", spokeName, err)
	}

	select {
	case output := <-conn.respCh:
		return output, nil
	case <-ctx.Done():
		return "", fmt.Errorf("timed out waiting for response from spoke %q: %w", spokeName, ctx.Err())
	}
}

// ConnectedSpokes returns names of all currently connected spokes.
func (s *Server) ConnectedSpokes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.spokes))
	for name := range s.spokes {
		names = append(names, name)
	}
	return names
}
