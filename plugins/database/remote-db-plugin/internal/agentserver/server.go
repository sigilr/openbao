// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package agentserver

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	agentproto "github.com/openbao/openbao/plugins/database/remote-db-plugin/proto"
	"google.golang.org/grpc"
)

type spokeConn struct {
	stream agentproto.AgentService_ConnectServer
	respCh chan string
	mu     sync.Mutex
}

type Server struct {
	agentproto.UnimplementedAgentServiceServer
	mu     sync.RWMutex
	spokes map[string]*spokeConn
}

var (
	instance *Server
	once     sync.Once
)

func Instance() *Server {
	once.Do(func() {
		instance = &Server{
			spokes: make(map[string]*spokeConn),
		}
	})
	return instance
}

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

		if msg.IsResponse && conn != nil {
			select {
			case conn.respCh <- msg.Output:
			default:
			}
		}
	}
}

func (s *Server) RunCommand(ctx context.Context, spokeName, command string) (string, error) {
	s.mu.RLock()
	conn, ok := s.spokes[spokeName]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("spoke %q is not connected", spokeName)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()

	select {
	case <-conn.respCh:
	default:
	}

	if err := conn.stream.Send(&agentproto.AgentMessage{
		ClientName: "remote-db-plugin",
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
