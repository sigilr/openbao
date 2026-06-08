// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// Package remotedb provides a proxy plugin that forwards database plugin
// requests to spoke-agent, which then executes the actual built-in plugins.
package remotedb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/openbao/openbao/plugins/database/remote-db-plugin/bootstrap"
	agentproto "github.com/openbao/openbao/plugins/database/remote-db-plugin/proto/gen"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

const (
	proxyAgentPort = 50053 // Different port to avoid conflict with base.go
)

var (
	proxyServerOnce     sync.Once
	proxyServerStartErr error
	proxyServerInstance *proxyServer
)

//func init() {
//	// Start gRPC server when plugin loads
//	go func() {
//		getProxyServer().Start(proxyAgentPort)
//	}()
//}

// proxyServer is a simple gRPC server for spoke-agent connections
type proxyServer struct {
	agentproto.UnimplementedAgentServiceServer
	mu     sync.RWMutex
	spokes map[string]*spokeConnection
}

type spokeConnection struct {
	stream agentproto.AgentService_ConnectServer
	respCh chan string
	mu     sync.Mutex
}

func getProxyServer() *proxyServer {
	if proxyServerInstance == nil {
		proxyServerInstance = &proxyServer{
			spokes: make(map[string]*spokeConnection),
		}
	}
	return proxyServerInstance
}

func (s *proxyServer) Start(port int) error {
	if !bootstrap.Global().Ready() {
		return fmt.Errorf("hub identity not initialized; run `bao agent init` first")
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	creds := credentials.NewTLS(bootstrap.Global().TLSConfig())
	srv := grpc.NewServer(grpc.Creds(creds))
	agentproto.RegisterAgentServiceServer(srv, s)
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("[proxy] gRPC server stopped: %v", err)
		}
	}()
	log.Printf("[proxy] mTLS server listening on :%d", port)
	return nil
}

func (s *proxyServer) Connect(stream agentproto.AgentService_ConnectServer) error {
	// Identity comes from the verified client cert, NOT from msg.ClientName.
	// This is the load-bearing security check now that bootstrap tokens have
	// been exchanged for client certs — the wire-level claim is spoofable, the
	// CN is not.
	spokeName, err := spokeNameFromPeer(stream.Context())
	if err != nil {
		return err
	}

	conn := &spokeConnection{
		stream: stream,
		respCh: make(chan string, 1),
	}
	s.mu.Lock()
	if old, ok := s.spokes[spokeName]; ok {
		// A reconnection from the same spoke: drop the stale entry so the new
		// stream is the one we forward requests on. The old stream is
		// abandoned and will error out on its next Recv/Send.
		_ = old
	}
	s.spokes[spokeName] = conn
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if cur, ok := s.spokes[spokeName]; ok && cur == conn {
			delete(s.spokes, spokeName)
		}
		s.mu.Unlock()
	}()

	if err := stream.Send(&agentproto.AgentMessage{
		ClientName: spokeName,
		Output:     "Connected",
		IsResponse: true,
	}); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if msg.IsResponse {
			select {
			case conn.respCh <- msg.Output:
			default:
			}
		}
	}
}

// spokeNameFromPeer extracts the spoke identity from the verified client cert.
// Requires the gRPC server to be configured with mTLS (RequireAndVerifyClientCert).
func spokeNameFromPeer(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", fmt.Errorf("no peer info on incoming stream")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", fmt.Errorf("connection is not TLS")
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", fmt.Errorf("no verified client cert chain")
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]
	if leaf.Subject.CommonName == "" {
		return "", fmt.Errorf("client cert has no Common Name")
	}
	return leaf.Subject.CommonName, nil
}

func (s *proxyServer) RunCommand(ctx context.Context, spokeName, command string) (string, error) {
	s.mu.RLock()
	conn, ok := s.spokes[spokeName]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("spoke %q not connected", spokeName)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()

	select {
	case <-conn.respCh:
	default:
	}

	if err := conn.stream.Send(&agentproto.AgentMessage{
		ClientName: "proxy",
		TargetName: spokeName,
		Command:    command,
		IsResponse: false,
	}); err != nil {
		return "", err
	}

	select {
	case output := <-conn.respCh:
		return output, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// PluginProxy forwards all database plugin operations to spoke-agent
type PluginProxy struct {
	pluginName    string
	agentPort     int
	spokeName     string
	connectionURL string
	config        map[string]interface{}
}

var _ dbplugin.Database = (*PluginProxy)(nil)

func NewProxy(pluginName string) func() (interface{}, error) {
	return func() (interface{}, error) {
		db := &PluginProxy{
			pluginName: pluginName,
		}
		return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
	}
}

func (p *PluginProxy) secretValues() map[string]string {
	return map[string]string{
		p.connectionURL: "[connection_url]",
	}
}

func (p *PluginProxy) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	spokeName, err := proxyGetConfigString(req.Config, "spoke_name")
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	agentPort := proxyAgentPort
	if v, ok := req.Config["agent_port"]; ok {
		if port, ok := v.(int); ok && port > 0 {
			agentPort = port
		}
	}

	p.spokeName = spokeName
	p.agentPort = agentPort
	p.config = req.Config

	if connURL, ok := req.Config["connection_url"].(string); ok {
		p.connectionURL = connURL
	}

	// Auto-start gRPC server on first database config
	proxyServerOnce.Do(func() {
		proxyServerStartErr = getProxyServer().Start(agentPort)
	})
	if proxyServerStartErr != nil {
		return dbplugin.InitializeResponse{}, proxyServerStartErr
	}

	// Filter out proxy-specific config fields before sending to actual plugin
	pluginConfig := make(map[string]interface{})
	for k, v := range req.Config {
		if k != "spoke_name" && k != "agent_port" {
			pluginConfig[k] = v
		}
	}

	request := map[string]interface{}{
		"method":            "Initialize",
		"plugin_name":       p.pluginName,
		"config":            pluginConfig,
		"verify_connection": req.VerifyConnection,
	}

	response, err := p.callPlugin(ctx, request)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	var initResp struct {
		Config map[string]interface{} `json:"config"`
	}
	if err := json.Unmarshal([]byte(response), &initResp); err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("parse response failed: %w", err)
	}

	// Add back proxy-specific fields to persist them
	if initResp.Config == nil {
		initResp.Config = make(map[string]interface{})
	}
	initResp.Config["spoke_name"] = spokeName
	initResp.Config["agent_port"] = agentPort

	return dbplugin.InitializeResponse{Config: initResp.Config}, nil
}

func (p *PluginProxy) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	request := map[string]interface{}{
		"method":      "NewUser",
		"plugin_name": p.pluginName,
		"config":      p.getPluginConfig(),
		"username_config": map[string]interface{}{
			"display_name": req.UsernameConfig.DisplayName,
			"role_name":    req.UsernameConfig.RoleName,
		},
		"password":   req.Password,
		"expiration": req.Expiration.Unix(),
		"statements": req.Statements.Commands,
	}

	response, err := p.callPlugin(ctx, request)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	var newUserResp struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal([]byte(response), &newUserResp); err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("parse response failed: %w", err)
	}

	return dbplugin.NewUserResponse{Username: newUserResp.Username}, nil
}

func (p *PluginProxy) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	request := map[string]interface{}{
		"method":      "UpdateUser",
		"plugin_name": p.pluginName,
		"config":      p.getPluginConfig(),
		"username":    req.Username,
	}

	if req.Password != nil {
		request["password"] = map[string]interface{}{
			"new_password": req.Password.NewPassword,
			"statements":   req.Password.Statements.Commands,
		}
	}

	if req.Expiration != nil {
		request["expiration"] = map[string]interface{}{
			"new_expiration": req.Expiration.NewExpiration.Unix(),
			"statements":     req.Expiration.Statements.Commands,
		}
	}

	_, err := p.callPlugin(ctx, request)
	return dbplugin.UpdateUserResponse{}, err
}

func (p *PluginProxy) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	request := map[string]interface{}{
		"method":      "DeleteUser",
		"plugin_name": p.pluginName,
		"config":      p.getPluginConfig(),
		"username":    req.Username,
		"statements":  req.Statements.Commands,
	}

	_, err := p.callPlugin(ctx, request)
	return dbplugin.DeleteUserResponse{}, err
}

func (p *PluginProxy) Type() (string, error) {
	return p.pluginName, nil
}

func (p *PluginProxy) Close() error {
	return nil
}

func (p *PluginProxy) callPlugin(ctx context.Context, request map[string]interface{}) (string, error) {
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return "", err
	}

	command := fmt.Sprintf("plugin-runner %s", string(reqJSON))
	output, err := getProxyServer().RunCommand(ctx, p.spokeName, command)
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(output, "Error:") {
		return "", fmt.Errorf("spoke error: %s", output)
	}

	return output, nil
}

func proxyGetConfigString(config map[string]interface{}, key string) (string, error) {
	v, ok := config[key]
	if !ok {
		return "", fmt.Errorf("missing %q", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%q must be non-empty string", key)
	}
	return s, nil
}

func (p *PluginProxy) getPluginConfig() map[string]interface{} {
	// Return config without proxy-specific fields
	pluginConfig := make(map[string]interface{})
	for k, v := range p.config {
		if k != "spoke_name" && k != "agent_port" {
			pluginConfig[k] = v
		}
	}
	return pluginConfig
}
