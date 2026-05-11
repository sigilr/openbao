# Remote Database Plugin - Code Flow

## File Structure

```
plugins/database/remote-db-plugin/
├── proxy.go                          # Hub-side proxy plugin
├── proto/
│   ├── plugin_proxy.proto           # gRPC protocol definition
│   ├── agent.pb.go                  # Generated protobuf
│   └── agent_grpc.pb.go             # Generated gRPC
├── spoke-agent-v2/
│   ├── main.go                      # Spoke agent
│   └── runner/
│       └── runner.go                # Plugin executor
└── cmd/plugin-runner/
    └── main.go                      # Plugin runner binary

helper/builtinplugins/
└── registry.go                       # Plugin registration (lines 88-91)
```

---

## Code Flow: Credential Generation

### Step 1: User Request
```bash
bao read database/creds/my-role
```

### Step 2: Vault Core → Database Secrets Engine
**File**: OpenBao core (not in remote-db-plugin)
- Vault receives HTTP request
- Routes to database secrets engine
- Loads database config from storage
- Calls plugin Initialize() with config

### Step 3: Plugin Initialize
**File**: `proxy.go`
**Function**: `PluginProxy.Initialize()`
**Lines**: ~180-230

```go
func (p *PluginProxy) Initialize(ctx context.Context, req dbplugin.InitializeRequest) {
    // 1. Extract spoke_name from config
    spokeName, err := proxyGetConfigString(req.Config, "spoke_name")
    
    // 2. Store spoke_name and config
    p.spokeName = spokeName
    p.config = req.Config
    
    // 3. Auto-start gRPC server (once)
    proxyServerOnce.Do(func() {
        proxyServerStartErr = getProxyServer().Start(agentPort)
    })
    
    // 4. Filter proxy-specific fields
    pluginConfig := make(map[string]interface{})
    for k, v := range req.Config {
        if k != "spoke_name" && k != "agent_port" {
            pluginConfig[k] = v
        }
    }
    
    // 5. Forward to spoke-agent
    request := map[string]interface{}{
        "method":            "Initialize",
        "plugin_name":       p.pluginName,
        "config":            pluginConfig,
        "verify_connection": req.VerifyConnection,
    }
    response, err := p.callPlugin(ctx, request)
    
    // 6. Add back proxy fields to persist them
    initResp.Config["spoke_name"] = spokeName
    initResp.Config["agent_port"] = agentPort
    
    return dbplugin.InitializeResponse{Config: initResp.Config}, nil
}
```

**Flow**:
1. Extract `spoke_name` from config
2. Auto-start gRPC server on port 50053 (first time only)
3. Filter out `spoke_name` and `agent_port` before sending to built-in plugin
4. Forward Initialize request to spoke-agent
5. Add back `spoke_name` and `agent_port` to response so they persist in storage

### Step 4: Plugin NewUser
**File**: `proxy.go`
**Function**: `PluginProxy.NewUser()`
**Lines**: ~232-260

```go
func (p *PluginProxy) NewUser(ctx context.Context, req dbplugin.NewUserRequest) {
    // 1. Build request with config
    request := map[string]interface{}{
        "method":      "NewUser",
        "plugin_name": p.pluginName,
        "config":      p.getPluginConfig(),  // Filters out spoke_name
        "username_config": map[string]interface{}{
            "display_name": req.UsernameConfig.DisplayName,
            "role_name":    req.UsernameConfig.RoleName,
        },
        "password":   req.Password,
        "expiration": req.Expiration.Unix(),
        "statements": req.Statements.Commands,
    }
    
    // 2. Forward to spoke-agent
    response, err := p.callPlugin(ctx, request)
    
    // 3. Parse response
    var newUserResp struct {
        Username string `json:"username"`
    }
    json.Unmarshal([]byte(response), &newUserResp)
    
    return dbplugin.NewUserResponse{Username: newUserResp.Username}, nil
}
```

**Flow**:
1. Build JSON request with method="NewUser"
2. Include config (without proxy fields)
3. Call `callPlugin()` to forward to spoke-agent
4. Parse JSON response
5. Return username to Vault

### Step 5: Forward Request to Spoke
**File**: `proxy.go`
**Function**: `PluginProxy.callPlugin()`
**Lines**: ~310-330

```go
func (p *PluginProxy) callPlugin(ctx context.Context, request map[string]interface{}) (string, error) {
    // 1. Marshal request to JSON
    reqJSON, err := json.Marshal(request)
    
    // 2. Build command string
    command := fmt.Sprintf("plugin-runner %s", string(reqJSON))
    
    // 3. Send to spoke via gRPC
    output, err := getProxyServer().RunCommand(ctx, p.spokeName, command)
    
    // 4. Check for errors
    if strings.HasPrefix(output, "Error:") {
        return "", fmt.Errorf("spoke error: %s", output)
    }
    
    return output, nil
}
```

**Flow**:
1. Convert request to JSON
2. Build command: `plugin-runner {json}`
3. Call `proxyServer.RunCommand()` with spoke_name
4. Return response or error

### Step 6: gRPC Server Send Command
**File**: `proxy.go`
**Function**: `proxyServer.RunCommand()`
**Lines**: ~120-150

```go
func (s *proxyServer) RunCommand(ctx context.Context, spokeName, command string) (string, error) {
    // 1. Find spoke connection
    s.mu.RLock()
    conn, ok := s.spokes[spokeName]
    s.mu.RUnlock()
    if !ok {
        return "", fmt.Errorf("spoke %q not connected", spokeName)
    }
    
    // 2. Lock connection (serialize requests)
    conn.mu.Lock()
    defer conn.mu.Unlock()
    
    // 3. Send command via gRPC stream
    err := conn.stream.Send(&agentproto.AgentMessage{
        ClientName: "proxy",
        TargetName: spokeName,
        Command:    command,
        IsResponse: false,
    })
    
    // 4. Wait for response
    select {
    case output := <-conn.respCh:
        return output, nil
    case <-ctx.Done():
        return "", ctx.Err()
    }
}
```

**Flow**:
1. Look up spoke connection in map by `spokeName`
2. Lock connection (one request at a time per spoke)
3. Send command via gRPC bidirectional stream
4. Wait for response on channel
5. Return response

### Step 7: Spoke-Agent Receives Command
**File**: `spoke-agent-v2/main.go`
**Function**: `main()`
**Lines**: ~50-110

```go
func main() {
    // 1. Connect to hub gRPC server
    conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    client := proto.NewAgentServiceClient(conn)
    stream, err := client.Connect(context.Background())
    
    // 2. Register with hub
    stream.Send(&proto.AgentMessage{
        ClientName: *spokeName,
        IsResponse: false,
    })
    
    // 3. Receive commands in loop
    for {
        msg, err := stream.Recv()
        
        if msg.Command == "" || msg.IsResponse {
            continue
        }
        
        // 4. Check if plugin-runner command
        if strings.HasPrefix(msg.Command, "plugin-runner ") {
            // Extract JSON request
            jsonRequest := strings.TrimPrefix(msg.Command, "plugin-runner ")
            
            // 5. Execute plugin-runner
            cmd := exec.Command(pluginRunnerPath, jsonRequest)
            out, err := cmd.CombinedOutput()
            output = strings.TrimSpace(string(out))
        }
        
        // 6. Send response back
        stream.Send(&proto.AgentMessage{
            ClientName: *spokeName,
            Output:     output,
            IsResponse: true,
        })
    }
}
```

**Flow**:
1. Connect to hub gRPC server (10.2.0.88:32406)
2. Register with spoke name
3. Wait for commands in loop
4. Detect `plugin-runner` command
5. Execute plugin-runner binary with JSON argument
6. Send output back via gRPC stream

### Step 8: Plugin-Runner Executes
**File**: `cmd/plugin-runner/main.go`
**Function**: `main()`
**Lines**: ~10-30

```go
func main() {
    // 1. Get JSON request from args
    requestJSON := os.Args[1]
    
    // 2. Create runner
    r := runner.NewPluginRunner()
    
    // 3. Execute request
    response, err := r.ExecuteRequest(requestJSON)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        os.Exit(1)
    }
    
    // 4. Print response
    fmt.Println(response)
}
```

**Flow**:
1. Read JSON request from command line argument
2. Create PluginRunner instance
3. Call ExecuteRequest()
4. Print JSON response to stdout

### Step 9: Runner Executes Plugin
**File**: `spoke-agent-v2/runner/runner.go`
**Function**: `PluginRunner.ExecuteRequest()`
**Lines**: ~60-90

```go
func (r *PluginRunner) ExecuteRequest(requestJSON string) (string, error) {
    // 1. Parse JSON request
    var req map[string]interface{}
    json.Unmarshal([]byte(requestJSON), &req)
    
    pluginName := req["plugin_name"].(string)
    method := req["method"].(string)
    
    // 2. Load built-in plugin
    plugin, err := r.LoadPlugin(pluginName)
    
    // 3. Route to method handler
    switch method {
    case "Initialize":
        return r.handleInitialize(ctx, plugin, req)
    case "NewUser":
        return r.handleNewUser(ctx, plugin, req)
    case "UpdateUser":
        return r.handleUpdateUser(ctx, plugin, req)
    case "DeleteUser":
        return r.handleDeleteUser(ctx, plugin, req)
    }
}
```

**Flow**:
1. Parse JSON request
2. Load built-in plugin (PostgreSQL, MySQL, etc.)
3. Route to appropriate method handler
4. Return JSON response

### Step 10: Runner Handles NewUser
**File**: `spoke-agent-v2/runner/runner.go`
**Function**: `PluginRunner.handleNewUser()`
**Lines**: ~130-180

```go
func (r *PluginRunner) handleNewUser(ctx context.Context, plugin dbplugin.Database, req map[string]interface{}) (string, error) {
    // 1. Initialize plugin with config (one-shot process)
    if config, ok := req["config"].(map[string]interface{}); ok {
        initReq := dbplugin.InitializeRequest{
            Config:           config,
            VerifyConnection: false,
        }
        plugin.Initialize(ctx, initReq)
    }
    
    // 2. Build NewUser request
    newUserReq := dbplugin.NewUserRequest{
        UsernameConfig: dbplugin.UsernameMetadata{
            DisplayName: getString(usernameConfig, "display_name"),
            RoleName:    getString(usernameConfig, "role_name"),
        },
        Password:   password,
        Expiration: time.Unix(int64(expirationUnix), 0),
        Statements: dbplugin.Statements{
            Commands: stmtStrings,
        },
    }
    
    // 3. Call built-in plugin
    resp, err := plugin.NewUser(ctx, newUserReq)
    
    // 4. Build JSON response
    result := map[string]interface{}{
        "username": resp.Username,
    }
    resultJSON, _ := json.Marshal(result)
    
    return string(resultJSON), nil
}
```

**Flow**:
1. Initialize plugin with config (needed because plugin-runner is one-shot)
2. Build NewUserRequest from JSON
3. Call built-in plugin's NewUser() method
4. Convert response to JSON
5. Return JSON string

### Step 11: Built-in Plugin Creates User
**File**: OpenBao built-in plugin (e.g., `plugins/database/postgresql/postgresql.go`)
**Function**: `PostgreSQL.NewUser()`

```go
func (p *PostgreSQL) NewUser(ctx context.Context, req dbplugin.NewUserRequest) {
    // 1. Connect to database
    db, err := sql.Open("postgres", p.connectionURL)
    
    // 2. Generate username
    username := generateUsername(req.UsernameConfig)
    
    // 3. Execute SQL statements
    for _, stmt := range req.Statements.Commands {
        stmt = strings.Replace(stmt, "{{name}}", username, -1)
        stmt = strings.Replace(stmt, "{{password}}", req.Password, -1)
        stmt = strings.Replace(stmt, "{{expiration}}", req.Expiration.Format(time.RFC3339), -1)
        
        _, err = db.ExecContext(ctx, stmt)
    }
    
    return dbplugin.NewUserResponse{Username: username}, nil
}
```

**Flow**:
1. Connect to PostgreSQL database
2. Generate username (e.g., `v-root-readonly-xxx`)
3. Replace template variables in SQL statements
4. Execute SQL to create user
5. Return username

### Step 12: Response Flows Back
```
Built-in Plugin → Runner → Plugin-Runner stdout → Spoke-Agent → gRPC → Proxy → Vault → User
```

---

## Code Flow: Auto-Start gRPC Server

### Trigger: First Database Config Creation
```bash
bao write database/config/spoke-pg plugin_name=remote-postgres-proxy spoke_name=spoke-1 ...
```

### Step 1: Initialize Called
**File**: `proxy.go`
**Function**: `PluginProxy.Initialize()`
**Lines**: ~200-210

```go
// Auto-start gRPC server on first database config
proxyServerOnce.Do(func() {
    proxyServerStartErr = getProxyServer().Start(agentPort)
})
```

**Flow**:
1. `sync.Once` ensures this runs only once
2. Calls `proxyServer.Start(50053)`
3. Starts gRPC server on port 50053

### Step 2: Start gRPC Server
**File**: `proxy.go`
**Function**: `proxyServer.Start()`
**Lines**: ~50-70

```go
func (s *proxyServer) Start(port int) error {
    // 1. Create TCP listener
    lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
    
    // 2. Create gRPC server
    srv := grpc.NewServer()
    agentproto.RegisterAgentServiceServer(srv, s)
    
    // 3. Start serving in goroutine
    go func() {
        if err := srv.Serve(lis); err != nil {
            log.Printf("[proxy] gRPC server stopped: %v\", err)
        }
    }()
    
    log.Printf("[proxy] server listening on :%d", port)
    return nil
}
```

**Flow**:
1. Create TCP listener on port 50053
2. Create gRPC server
3. Register AgentService
4. Start serving in background goroutine
5. Log: `[proxy] server listening on :50053`

---

## Code Flow: Spoke-Agent Connection

### Step 1: Spoke-Agent Starts
**File**: `spoke-agent-v2/main.go`
**Function**: `main()`
**Lines**: ~30-60

```go
func main() {
    // 1. Parse flags
    serverAddr := flag.String("server", "localhost:50052", "OpenBao spoke server address")
    spokeName := flag.String("name", "spoke-1", "Unique name for this spoke cluster")
    flag.Parse()
    
    // 2. Find plugin-runner binary
    exePath, _ := os.Executable()
    pluginRunnerPath := filepath.Join(filepath.Dir(exePath), "plugin-runner")
    
    // 3. Connect to hub
    conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    client := proto.NewAgentServiceClient(conn)
    stream, err := client.Connect(context.Background())
    
    // 4. Register with hub
    stream.Send(&proto.AgentMessage{
        ClientName: *spokeName,
        IsResponse: false,
    })
    
    // 5. Wait for ack
    ack, err := stream.Recv()
    log.Printf("registered: %s", ack.Output)
}
```

**Flow**:
1. Parse command line flags (`-server`, `-name`)
2. Find plugin-runner binary in same directory
3. Connect to hub gRPC server
4. Send registration message with spoke name
5. Wait for acknowledgment

### Step 2: Hub Receives Connection
**File**: `proxy.go`
**Function**: `proxyServer.Connect()`
**Lines**: ~75-120

```go
func (s *proxyServer) Connect(stream agentproto.AgentService_ConnectServer) error {
    var spokeName string
    var conn *spokeConnection
    
    for {
        msg, err := stream.Recv()
        
        // 1. First message is registration
        if spokeName == "" && !msg.IsResponse {
            spokeName = msg.ClientName
            
            // 2. Create connection object
            conn = &spokeConnection{
                stream: stream,
                respCh: make(chan string, 1),
            }
            
            // 3. Store in map
            s.mu.Lock()
            s.spokes[spokeName] = conn
            s.mu.Unlock()
            
            // 4. Send ack
            stream.Send(&agentproto.AgentMessage{
                ClientName: spokeName,
                Output:     "Connected",
                IsResponse: true,
            })
            continue
        }
        
        // 5. Handle responses
        if msg.IsResponse && conn != nil {
            conn.respCh <- msg.Output
        }
    }
}
```

**Flow**:
1. Receive registration message from spoke-agent
2. Extract spoke name
3. Create spokeConnection object with stream and response channel
4. Store in `spokes` map: `map[string]*spokeConnection`
5. Send "Connected" acknowledgment
6. Continue loop to handle command responses

---

## Plugin Registration

**File**: `helper/builtinplugins/registry.go`
**Lines**: 88-91

```go
databasePlugins: map[string]databasePlugin{
    // ... other plugins ...
    "remote-postgres-proxy": {Factory: dbRemoteDB.NewProxy("postgresql-database-plugin")},
    "remote-mysql-proxy":    {Factory: dbRemoteDB.NewProxy("mysql-database-plugin")},
    "remote-redis-proxy":    {Factory: dbRemoteDB.NewProxy("redis-database-plugin")},
    "remote-valkey-proxy":   {Factory: dbRemoteDB.NewProxy("valkey-database-plugin")},
}
```

**Flow**:
1. Register 4 proxy plugins in built-in registry
2. Each proxy forwards to corresponding built-in plugin
3. Factory function: `dbRemoteDB.NewProxy(pluginName)`

---

## Key Data Structures

### PluginProxy
**File**: `proxy.go`
**Lines**: ~155-165

```go
type PluginProxy struct {
    pluginName    string                    // e.g., "postgresql-database-plugin"
    agentPort     int                       // 50053
    spokeName     string                    // e.g., "spoke-1"
    connectionURL string                    // Database connection URL
    config        map[string]interface{}    // Full config including spoke_name
}
```

### proxyServer
**File**: `proxy.go`
**Lines**: ~30-40

```go
type proxyServer struct {
    agentproto.UnimplementedAgentServiceServer
    mu     sync.RWMutex
    spokes map[string]*spokeConnection    // Map: spoke_name → connection
}

type spokeConnection struct {
    stream agentproto.AgentService_ConnectServer  // gRPC bidirectional stream
    respCh chan string                             // Response channel
    mu     sync.Mutex                              // Serialize requests
}
```

---

## Summary

### Request Flow
```
User → Vault → Proxy.NewUser() → callPlugin() → proxyServer.RunCommand() 
→ gRPC → Spoke-Agent → Plugin-Runner → Runner.handleNewUser() 
→ Built-in Plugin → PostgreSQL → Response back through chain
```

### Auto-Start Flow
```
First config creation → Proxy.Initialize() → proxyServerOnce.Do() 
→ proxyServer.Start() → gRPC server listening on :50053
```

### Connection Flow
```
Spoke-Agent starts → Connect to hub → Send registration 
→ Hub stores in spokes map → Send "Connected" ack → Ready for commands
```

### Key Files
- `proxy.go` - Hub proxy (180 lines)
- `spoke-agent-v2/main.go` - Spoke agent (110 lines)
- `spoke-agent-v2/runner/runner.go` - Plugin executor (280 lines)
- `cmd/plugin-runner/main.go` - Binary entry point (30 lines)
- `registry.go` - Plugin registration (4 lines modified)

**Total Code**: ~600 lines (excluding generated protobuf)
