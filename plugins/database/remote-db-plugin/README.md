<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: LicenseRef-AppsCode-Free-Trial-1.0.0
-->

# Remote Database Plugin

A production-ready OpenBao plugin that enables a hub vault instance to manage database credentials in remote spoke clusters through a proxy architecture.

## 🚀 Quick Start

```bash
# 1. Build binaries and images
make dev
docker build -t rudro25/openbao:hub-v2 .
docker build -f Dockerfile.spoke -t rudro25/spoke-agent:v1 .

# 2. Deploy hub vault
kubectl apply -f yaml/01-vaultserverversion.yaml
kubectl apply -f yaml/02-vaultserver-hub.yaml

# 3. Deploy spoke-agent
kubectl apply -f yaml/03-spoke-agent-deployment.yaml

# 4. Configure database
export VAULT_ADDR="http://hub-ip:30820"
export VAULT_TOKEN="<root-token>"
bao secrets enable database
bao write database/config/spoke-pg \
    plugin_name=remote-postgres-proxy \
    spoke_name=spoke-1 \
    connection_url="postgresql://{{username}}:{{password}}@postgres:5432/postgres" \
    username="postgres" \
    password="password" \
    allowed_roles="*"

# 5. Generate credentials
bao write database/roles/readonly \
    db_name=spoke-pg \
    creation_statements="CREATE ROLE \"{{name}}\" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}';" \
    default_ttl="1h"
bao read database/creds/readonly
```

## 📚 Documentation

- **[PRODUCTION_DEPLOYMENT.md](PRODUCTION_DEPLOYMENT.md)** - Complete production deployment guide with architecture diagrams and troubleshooting
- **[QUICK_REFERENCE.md](QUICK_REFERENCE.md)** - Quick reference for common commands and operations
- **[COMPLETE_WORKFLOW.md](COMPLETE_WORKFLOW.md)** - Detailed workflow and design decisions

## 🏗️ Architecture

```
Hub Cluster                          Spoke Cluster
┌─────────────────┐                 ┌─────────────────┐
│  OpenBao Vault  │                 │  Spoke-Agent    │
│  ┌───────────┐  │                 │  ┌───────────┐  │
│  │  Proxy    │  │  gRPC (50053)   │  │  Plugin   │  │
│  │  Plugin   │──┼─────────────────┼─→│  Runner   │  │
│  └───────────┘  │                 │  └───────────┘  │
│  Auto-starts    │                 │  Executes       │
│  on first       │                 │  built-in       │
│  config         │                 │  plugins        │
└─────────────────┘                 └─────────────────┘
                                             │
                                             ↓
                                    ┌─────────────────┐
                                    │  PostgreSQL/    │
                                    │  MySQL Database │
                                    └─────────────────┘
```

## ✨ Key Features

- ✅ **Auto-Start**: gRPC server automatically starts when first database config is created
- ✅ **Auto-Connect**: Spoke-agent automatically connects to hub when deployed
- ✅ **Parameter Persistence**: `spoke_name` and `agent_port` automatically saved and persist across restarts
- ✅ **Multi-Spoke Support**: Hub can manage multiple spoke clusters simultaneously
- ✅ **Built-in Plugin Reuse**: Reuses OpenBao's built-in PostgreSQL, MySQL, Redis, and Valkey plugins
- ✅ **Production-Ready**: Kubernetes-native with proper error handling and reconnection logic

## 📁 File Structure

```
remote-db-plugin/
├── proxy.go                          # Hub proxy plugin (main logic)
├── proto/                            # gRPC protocol definitions
│   ├── plugin_proxy.proto           # Protocol buffer definition
│   ├── agent.pb.go                  # Generated protobuf code
│   └── agent_grpc.pb.go             # Generated gRPC code
├── spoke-agent-v2/                   # Spoke-side agent
│   ├── main.go                      # Agent main (connects to hub)
│   └── runner/
│       └── runner.go                # Plugin execution logic
├── cmd/
│   └── plugin-runner/               # Plugin runner binary
│       └── main.go                  # Executes built-in plugins
├── yaml/                            # Kubernetes manifests
│   ├── 01-vaultserverversion.yaml  # Custom VaultServerVersion
│   ├── 02-vaultserver-hub.yaml     # Hub vault deployment
│   ├── 03-spoke-agent-deployment.yaml  # Spoke-1 agent
│   └── 04-spoke-agent-spoke2.yaml  # Spoke-2 agent (multi-spoke)
├── Dockerfile.spoke                 # Spoke-agent Docker image
├── PRODUCTION_DEPLOYMENT.md         # Full deployment guide
├── QUICK_REFERENCE.md               # Quick reference
├── COMPLETE_WORKFLOW.md             # Detailed workflow
└── README.md                        # This file
```

## 🔌 Supported Databases

| Plugin Name | Database | Status |
|-------------|----------|--------|
| `remote-postgres-proxy` | PostgreSQL | ✅ Tested |
| `remote-mysql-proxy` | MySQL | ✅ Tested |
| `remote-redis-proxy` | Redis | ✅ Ready |
| `remote-valkey-proxy` | Valkey | ✅ Ready |

## 🛠️ Build Requirements

- Go 1.21+
- Docker
- Kubernetes cluster
- KubeVault operator (for hub cluster)

## 📦 Binaries

| Binary | Size | Purpose | Location |
|--------|------|---------|----------|
| `bao` | ~171M | OpenBao with proxy plugin | Hub cluster |
| `spoke-agent-v2` | ~15M | Connects to hub and executes plugin-runner | Spoke cluster |
| `plugin-runner` | ~27M | Executes built-in database plugins | Spoke cluster (called by agent) |

## 🔐 Security Considerations

- Use TLS for gRPC connections in production
- Implement network policies to restrict access
- Use Kubernetes RBAC for pod access control
- Store database passwords in Kubernetes secrets
- Enable audit logging on vault

## 🚦 Production Checklist

- [ ] Use semantic versioning for Docker images
- [ ] Enable TLS for gRPC
- [ ] Use LoadBalancer instead of NodePort
- [ ] Deploy vault with 3 replicas for HA
- [ ] Deploy spoke-agent with 2 replicas for redundancy
- [ ] Configure network policies
- [ ] Set up monitoring and alerts
- [ ] Configure backup for vault data
- [ ] Test disaster recovery procedures

## 🐛 Troubleshooting

### "spoke not connected" error
Check spoke-agent logs:
```bash
kubectl logs -n demo deployment/spoke-agent
```

### "connection refused" when starting spoke-agent
This is normal! The gRPC server auto-starts when you create the first database config.

### "Endpoint ignored these unrecognized parameters" warning
This is cosmetic and can be ignored. Parameters are saved in `connection_details`.

See [PRODUCTION_DEPLOYMENT.md](PRODUCTION_DEPLOYMENT.md) for detailed troubleshooting.

## 📝 Example Usage

### PostgreSQL
```bash
bao write database/config/my-postgres \
    plugin_name=remote-postgres-proxy \
    spoke_name=spoke-1 \
    connection_url="postgresql://{{username}}:{{password}}@postgres:5432/mydb" \
    username="admin" \
    password="secret" \
    allowed_roles="*"

bao write database/roles/app-user \
    db_name=my-postgres \
    creation_statements="CREATE ROLE \"{{name}}\" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}';" \
    default_ttl="1h"

bao read database/creds/app-user
```

### MySQL
```bash
bao write database/config/my-mysql \
    plugin_name=remote-mysql-proxy \
    spoke_name=spoke-1 \
    connection_url="{{username}}:{{password}}@tcp(mysql:3306)/" \
    username="root" \
    password="secret" \
    allowed_roles="*"

bao write database/roles/app-user \
    db_name=my-mysql \
    creation_statements="CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT SELECT ON *.* TO '{{name}}'@'%';" \
    default_ttl="1h"

bao read database/creds/app-user
```

## 🤝 Contributing

This plugin is part of the OpenBao project. Contributions are welcome!

## 📄 License

Apache-2.0

## 🙏 Acknowledgments

- OpenBao community
- KubeVault project
- HashiCorp Vault (original inspiration)

---

**Status**: ✅ Production-Ready

**Last Updated**: 2026-05-11

**Tested With**:
- OpenBao 2.4.3
- Kubernetes 1.34.6
- PostgreSQL 15
- MySQL 8.0
