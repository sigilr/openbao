# Remote DB Plugin - MySQL Test Commands

## Prerequisites
- OpenBao running on hub
- MySQL running in spoke cluster (e.g., via KubeDB)
- kubectl access to spoke cluster

## Build Binaries

```bash
cd /home/rudro25/go/src/github.com/openbao/openbao

# Build OpenBao
go build -o /tmp/bao .

# Build spoke-agent (linux binary for pod)
GOOS=linux GOARCH=amd64 go build -o /tmp/spoke-agent ./plugins/database/remote-db-plugin/spoke-agent/

# Build MySQL plugin
go build -o /tmp/remote-mysql-plugin ./plugins/database/remote-db-plugin/cmd/remote-mysql-plugin/
```

## Setup OpenBao (Terminal 1)

```bash
# Start OpenBao dev server
/tmp/bao server -dev -dev-root-token-id=root -dev-plugin-dir=/tmp &
sleep 2

# Set environment
export BAO_ADDR=http://127.0.0.1:8200
export BAO_TOKEN=root

# Enable database secrets engine
/tmp/bao secrets enable database

# Register plugin
SHA256=$(sha256sum /tmp/remote-mysql-plugin | cut -d' ' -f1)
/tmp/bao plugin register -sha256=$SHA256 database remote-mysql-plugin
```

## Setup Spoke Agent (Terminal 2)

```bash
# Create pod in spoke cluster (reuse if exists)
kubectl run spoke-agent-pod -n demo --image=debian:stable-slim --restart=Never --command -- sleep infinity

# Wait for pod
kubectl wait pod/spoke-agent-pod -n demo --for=condition=Ready --timeout=60s

# Copy spoke-agent binary
kubectl cp /tmp/spoke-agent demo/spoke-agent-pod:/tmp/spoke-agent

# Make executable
kubectl exec -n demo spoke-agent-pod -- chmod +x /tmp/spoke-agent

# Install MySQL client
kubectl exec -n demo spoke-agent-pod -- apt-get update -qq
kubectl exec -n demo spoke-agent-pod -- apt-get install -y default-mysql-client

# Get hub IP
HUB_IP=$(hostname -I | awk '{print $1}')
echo "Hub IP: $HUB_IP"

# Start spoke-agent (keep this terminal open)
kubectl exec -it -n demo spoke-agent-pod -- /tmp/spoke-agent --server=$HUB_IP:50052 --name=spoke-cluster-1
```

## Configure Database (Terminal 1)

```bash
# Get MySQL ClusterIP
MYSQL_IP=$(kubectl get svc -n demo mysql-quickstart -o jsonpath='{.spec.clusterIP}')
echo "MySQL IP: $MYSQL_IP"

# Get MySQL password
MYSQL_PASS=$(kubectl get secret mysql-quickstart-auth -n demo -o jsonpath='{.data.password}' | base64 -d)
echo "MySQL Password: $MYSQL_PASS"

# Configure database connection
/tmp/bao write database/config/spoke-mysql \
  plugin_name=remote-mysql-plugin \
  connection_url="root:$MYSQL_PASS@tcp($MYSQL_IP:3306)/mysql" \
  spoke_name=spoke-cluster-1 \
  allowed_roles="*" \
  verify_connection=true

# Create role
/tmp/bao write database/roles/mysql-role \
  db_name=spoke-mysql \
  creation_statements="CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT SELECT, INSERT, UPDATE, DELETE ON *.* TO '{{name}}'@'%';" \
  default_ttl=1h \
  max_ttl=24h

# Generate credentials
/tmp/bao read database/creds/mysql-role
```

## Verify Credentials

```bash
# Save credentials from previous command
USERNAME="<username-from-output>"
PASSWORD="<password-from-output>"

# Test connection from spoke pod
kubectl exec -n demo spoke-agent-pod -- mysql -u$USERNAME -p$PASSWORD -h$MYSQL_IP -e "SELECT USER(), NOW();"
```

## Expected Output

### When spoke-agent connects:
```
[agentserver] spoke "spoke-cluster-1" registered
```

### When credentials are generated:
```
Key                Value
---                -----
lease_id           database/creds/mysql-role/xxxxx
lease_duration     1h
lease_renewable    true
password           A1a-xxxxxxxxxx
username           v-root-mysql-role-xxxxxxxxxx
```

### When testing credentials:
```
+----------------------------------+---------------------+
| USER()                           | NOW()               |
+----------------------------------+---------------------+
| v-root-mysql-role-xxxxxxxxxx@... | 2024-04-29 17:00:00 |
+----------------------------------+---------------------+
```

## Cleanup

```bash
# Revoke lease
/tmp/bao lease revoke database/creds/mysql-role/<lease-id>

# Delete pod
kubectl delete pod spoke-agent-pod -n demo

# Stop OpenBao
pkill bao
```

## Notes

### MySQL Connection URL Format

The connection URL for MySQL follows this format:
```
user:password@tcp(host:port)/database
```

Examples:
- `root:mypass@tcp(10.96.0.1:3306)/mysql`
- `admin:secret@tcp(mysql-svc.demo.svc:3306)/mydb`

### Common Issues

1. **MySQL client not found**: Install with `apt-get install -y default-mysql-client`
2. **Connection refused**: Verify MySQL ClusterIP is accessible from spoke pod
3. **Authentication failed**: Check MySQL password and user permissions
