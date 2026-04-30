# Remote Postgres Plugin — End-to-End Test Commands

> Run step 13 in a **separate terminal** and keep it open. All other steps run in the main terminal.

| # | What | Where | Command |
|---|------|-------|---------|
| 1 | Start OpenBao dev server in background | local | `/tmp/bao server -dev -dev-root-token-id=root 2>&1 &` |
| 2 | Wait for server to start | local | `sleep 2` |
| 3 | Set env vars | local | `export BAO_ADDR=http://127.0.0.1:8200 && export BAO_TOKEN=root` |
| 4 | Enable database secrets engine | local | `/tmp/bao secrets enable database` |
| 5 | Create pod in demo namespace on spoke cluster | local | `kubectl run spoke-agent-pod -n demo --image=debian:stable-slim --restart=Never --command -- sleep infinity` |
| 6 | Wait for pod ready | local | `kubectl wait pod/spoke-agent-pod -n demo --for=condition=Ready --timeout=60s` |
| 7 | Build spoke-agent binary for linux/amd64 | local | `cd /home/rudro25/go/src/kubevault.dev/central-bao/openbao && GOOS=linux GOARCH=amd64 go build -o /tmp/spoke-agent ./plugins/database/remote-postgres/spoke-agent/` |
| 8 | Copy binary into spoke pod | local | `kubectl cp /tmp/spoke-agent demo/spoke-agent-pod:/tmp/spoke-agent` |
| 9 | Make binary executable | local | `kubectl exec -n demo spoke-agent-pod -- chmod +x /tmp/spoke-agent` |
| 10 | Install psql client in spoke pod | local | `kubectl exec -n demo spoke-agent-pod -- apt-get update -qq && kubectl exec -n demo spoke-agent-pod -- apt-get install -y postgresql-client` |
| 11 | Get postgres ClusterIP | local | `kubectl get svc -n demo postgres-quickstart -o jsonpath='{.spec.clusterIP}'` |
| 12 | Get postgres password | local | `kubectl get secret postgres-quickstart-auth -n demo -o jsonpath='{.data.password}' \| base64 -d` |
| 13 | Connect spoke-agent to hub (keep terminal open) | spoke | `kubectl exec -it -n demo spoke-agent-pod -- /tmp/spoke-agent --server=10.2.0.91:50052 --name=spoke-cluster-1` |
| 14 | Write database config with spoke details | local | `/tmp/bao write database/config/spoke-pg plugin_name=remote-postgres-database-plugin 'connection_url=postgresql://postgres:<PASSWORD>@<CLUSTERIP>:5432/postgres' spoke_name=spoke-cluster-1 allowed_roles="*" verify_connection=false` |
| 15 | Write database role | local | `/tmp/bao write database/roles/myrole db_name=spoke-pg "creation_statements=CREATE ROLE \"{{name}}\" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}';" default_ttl=1h max_ttl=24h` |
| 16 | Read credentials (triggers NewUser on spoke postgres) | local | `/tmp/bao read database/creds/myrole` |
| 17 | Verify creds actually work on spoke postgres | local | `kubectl exec -n demo spoke-agent-pod -- psql "postgresql://<USERNAME>:<PASSWORD>@<CLUSTERIP>:5432/postgres" -c "SELECT current_user, now();"` |
