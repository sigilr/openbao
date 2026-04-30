# Remote DB Plugin - PostgreSQL Test Commands

## Prerequisites
- OpenBao running on hub
- PostgreSQL running in spoke cluster (e.g., via KubeDB)
- kubectl access to spoke cluster

## Build Binaries

```bash
cd /home/rudro25/go/src/github.com/openbao/openbao

# Build OpenBao
go build -o /tmp/bao .

# Build spoke-agent (linux binary for pod)
GOOS=linux GOARCH=amd64 go build -o /tmp/spoke-agent ./plugins/database/remote-db-plugin/spoke-agent/

# Build PostgreSQL plugin
go build -o /tmp/remote-postgres-plugin ./plugins/database/remote-db-plugin/cmd/remote-postgres-plugin/
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
SHA256=$(sha256sum /tmp/remote-postgres-plugin | cut -d' ' -f1)
/tmp/bao plugin register -sha256=$SHA256 database remote-postgres-plugin
```

## Setup Spoke Agent (Terminal 2)

```bash
# Create pod in spoke cluster
kubectl run spoke-agent-pod -n demo --image=debian:stable-slim --restart=Never --command -- sleep infinity

# Wait for pod
kubectl wait pod/spoke-agent-pod -n demo --for=condition=Ready --timeout=60s

# Copy spoke-agent binary
kubectl cp /tmp/spoke-agent demo/spoke-agent-pod:/tmp/spoke-agent

# Make executable
kubectl exec -n demo spoke-agent-pod -- chmod +x /tmp/spoke-agent

# Install PostgreSQL client
kubectl exec -n demo spoke-agent-pod -- apt-get update -qq
kubectl exec -n demo spoke-agent-pod -- apt-get install -y postgresql-client

# Get hub IP (OpenBao server IP)
HUB_IP=$(hostname -I | awk '{print $1}')
echo "Hub IP: $HUB_IP"

# Start spoke-agent (keep this terminal open)
kubectl exec -it -n demo spoke-agent-pod -- /tmp/spoke-agent --server=$HUB_IP:50052 --name=spoke-cluster-1
```

## Configure Database (Terminal 1)

```bash
# Get PostgreSQL ClusterIP
PG_IP=$(kubectl get svc -n demo postgres-quickstart -o jsonpath='{.spec.clusterIP}')
echo "PostgreSQL IP: $PG_IP"

# Get PostgreSQL password
PG_PASS=$(kubectl get secret postgres-quickstart-auth -n demo -o jsonpath='{.data.password}' | base64 -d)
echo "PostgreSQL Password: $PG_PASS"

# Configure database connection
/tmp/bao write database/config/spoke-pg \
  plugin_name=remote-postgres-plugin \
  connection_url="postgresql://postgres:$PG_PASS@$PG_IP:5432/postgres" \
  spoke_name=spoke-cluster-1 \
  allowed_roles="*" \
  verify_connection=true

# Create role
/tmp/bao write database/roles/myrole \
  db_name=spoke-pg \
  creation_statements="CREATE ROLE \"{{name}}\" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}';" \
  default_ttl=1h \
  max_ttl=24h

# Generate credentials
/tmp/bao read database/creds/myrole
```

## Verify Credentials

```bash
# Save credentials from previous command
USERNAME="<username-from-output>"
PASSWORD="<password-from-output>"

# Test connection from spoke pod
kubectl exec -n demo spoke-agent-pod -- psql "postgresql://$USERNAME:$PASSWORD@$PG_IP:5432/postgres" -c "SELECT current_user, now();"
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
lease_id           database/creds/myrole/xxxxx
lease_duration     1h
lease_renewable    true
password           A1a-xxxxxxxxxx
username           v-root-myrole-xxxxxxxxxx
```

### When testing credentials:
```
 current_user              |              now
---------------------------+-------------------------------
 v-root-myrole-xxxxxxxxxx  | 2024-04-29 17:00:00.000000+00
```

## Cleanup

```bash
# Revoke lease
/tmp/bao lease revoke database/creds/myrole/<lease-id>

# Delete pod
kubectl delete pod spoke-agent-pod -n demo

# Stop OpenBao
pkill bao
```
