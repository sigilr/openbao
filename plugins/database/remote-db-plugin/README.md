# Remote DB Plugin

Generic remote database plugin framework for OpenBao that supports PostgreSQL and MySQL through spoke-agent architecture.

## Architecture

```
Hub (OpenBao)                    Spoke Cluster
┌─────────────────┐             ┌──────────────────┐
│ remote-postgres │             │  spoke-agent pod │
│ remote-mysql    │────gRPC────▶│  executes:       │
│   plugins       │   :50052    │  - psql          │
│                 │             │  - mysql         │
└─────────────────┘             └──────────────────┘
```

## Components

- **base.go**: Core RemoteDB implementation with Dialect pattern
- **dialects.go**: PostgreSQL and MySQL dialect definitions
- **internal/agentserver**: Embedded gRPC server (singleton)
- **proto/**: Protobuf definitions for spoke communication
- **spoke-agent/**: Lightweight agent binary for spoke clusters
- **cmd/remote-postgres-plugin/**: PostgreSQL plugin binary
- **cmd/remote-mysql-plugin/**: MySQL plugin binary

## Build

```bash
# Build spoke-agent (for spoke cluster)
cd /home/rudro25/go/src/github.com/openbao/openbao
GOOS=linux GOARCH=amd64 go build -o /tmp/spoke-agent ./plugins/database/remote-db-plugin/spoke-agent/

# Build PostgreSQL plugin
go build -o /tmp/remote-postgres-plugin ./plugins/database/remote-db-plugin/cmd/remote-postgres-plugin/

# Build MySQL plugin
go build -o /tmp/remote-mysql-plugin ./plugins/database/remote-db-plugin/cmd/remote-mysql-plugin/
```

## Usage - PostgreSQL

### 1. Deploy spoke-agent in spoke cluster

```bash
kubectl run spoke-agent-pod -n demo --image=debian:stable-slim --restart=Never --command -- sleep infinity
kubectl wait pod/spoke-agent-pod -n demo --for=condition=Ready --timeout=60s
kubectl cp /tmp/spoke-agent demo/spoke-agent-pod:/tmp/spoke-agent
kubectl exec -n demo spoke-agent-pod -- chmod +x /tmp/spoke-agent
kubectl exec -n demo spoke-agent-pod -- apt-get update -qq && kubectl exec -n demo spoke-agent-pod -- apt-get install -y postgresql-client
```

### 2. Start spoke-agent (keep terminal open)

```bash
kubectl exec -it -n demo spoke-agent-pod -- /tmp/spoke-agent --server=<HUB_IP>:50052 --name=spoke-cluster-1
```

### 3. Configure OpenBao (on hub)

```bash
# Register plugin
bao plugin register -sha256=$(sha256sum /tmp/remote-postgres-plugin | cut -d' ' -f1) database remote-postgres-plugin

# Configure database
bao write database/config/spoke-pg \
  plugin_name=remote-postgres-plugin \
  connection_url="postgresql://postgres:<PASSWORD>@<CLUSTERIP>:5432/postgres" \
  spoke_name=spoke-cluster-1 \
  allowed_roles="*"

# Create role
bao write database/roles/myrole \
  db_name=spoke-pg \
  creation_statements="CREATE ROLE \"{{name}}\" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}';" \
  default_ttl=1h \
  max_ttl=24h

# Get credentials
bao read database/creds/myrole
```

## Usage - MySQL

### 1. Deploy spoke-agent (same as PostgreSQL)

### 2. Install MySQL client in spoke pod

```bash
kubectl exec -n demo spoke-agent-pod -- apt-get install -y mysql-client
```

### 3. Configure OpenBao

```bash
# Register plugin
bao plugin register -sha256=$(sha256sum /tmp/remote-mysql-plugin | cut -d' ' -f1) database remote-mysql-plugin

# Configure database
bao write database/config/spoke-mysql \
  plugin_name=remote-mysql-plugin \
  connection_url="root:<PASSWORD>@tcp(<CLUSTERIP>:3306)/mysql" \
  spoke_name=spoke-cluster-1 \
  allowed_roles="*"

# Create role
bao write database/roles/mysql-role \
  db_name=spoke-mysql \
  creation_statements="CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT SELECT ON *.* TO '{{name}}'@'%';" \
  default_ttl=1h \
  max_ttl=24h

# Get credentials
bao read database/creds/mysql-role
```

## Configuration Options

- **plugin_name**: `remote-postgres-plugin` or `remote-mysql-plugin`
- **spoke_name**: Unique identifier for spoke cluster (must match `--name` in spoke-agent)
- **connection_url**: Database connection string (accessible from spoke cluster)
- **agent_port**: gRPC port (default: 50052)
- **allowed_roles**: Roles allowed to use this connection

## Adding New Databases

To add support for a new database:

1. Define a new Dialect in `dialects.go`:
```go
var RedisDialect = Dialect{
    TypeName: "remote-redis",
    BuildCmd: func(connURL, stmt string) string {
        return fmt.Sprintf("redis-cli -u %s %s", shellQuote(connURL), stmt)
    },
    BuildVerifyCmd: func(connURL string) string {
        return fmt.Sprintf("redis-cli -u %s PING", shellQuote(connURL))
    },
    DefaultNewUserStmts: []string{"ACL SETUSER {{username}} ON >{{password}}"},
    // ...
}
```

2. Create plugin binary in `cmd/remote-redis-plugin/main.go`:
```go
func run() error {
    dbplugin.ServeMultiplex(remotedb.New(remotedb.RedisDialect))
    return nil
}
```

3. Install required CLI tool in spoke-agent pod (e.g., `redis-cli`)

## Testing

```bash
# Test PostgreSQL
cd /home/rudro25/go/src/github.com/openbao/openbao
go test ./plugins/database/remote-db-plugin/...

# Integration test (requires running spoke cluster)
# Follow steps in Usage section above
```
