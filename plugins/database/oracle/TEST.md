<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Oracle Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/oracle/...
```

Covers without touching a database:

- `Type` / `PluginVersion`
- Default username template renders an upper-case identifier ≤ 30 chars
- `UpdateUser` validation: missing username; no changes requested

## Acceptance tests

Gated on `BAO_ACC=1` + `ORACLE_URL=oracle://SYSTEM:pw@host:1521/service`.

```
$ export BAO_ACC=1
$ export ORACLE_URL='oracle://SYSTEM:oracle@localhost:1521/XEPDB1'
$ go test -v -count=1 -timeout=10m ./plugins/database/oracle/
```

### Local Oracle via Docker (Express Edition)

```
$ docker run --rm -d --name oracle-xe \
    -p 1521:1521 \
    -e ORACLE_PWD=oracle \
    container-registry.oracle.com/database/express:21.3.0-xe
```

Wait for `DATABASE IS READY TO USE!` in the logs (~5 min on first run).

### Coverage

- `TestOracle_NewUser` — create user with custom statements, verify login,
  then DeleteUser.
- `TestOracle_UpdateUser_Password` — rotate password, verify new password
  works.

## Manual end-to-end with `bao`

### Built-in plugin

```bash
$ make oracle-database-plugin
$ bao server -dev

$ export BAO_ADDR='http://127.0.0.1:8200'
$ bao login root
$ bao secrets enable database

$ bao write database/config/oracle \
    plugin_name=oracle-database-plugin \
    connection_url='oracle://{{username}}:{{password}}@localhost:1521/XEPDB1' \
    allowed_roles='readonly' \
    username='SYSTEM' password='oracle'

$ bao write database/roles/readonly \
    db_name=oracle \
    creation_statements="CREATE USER {{name}} IDENTIFIED BY \"{{password}}\";
                         GRANT CREATE SESSION TO {{name}};" \
    default_ttl=1h max_ttl=24h

$ bao read database/creds/readonly

# Verify the user can log in (sqlplus or sqlcl):
$ sqlplus <USERNAME>/<PASSWORD>@localhost:1521/XEPDB1

# Revoke and verify the user is gone:
$ bao lease revoke <LEASE_ID>
$ sqlplus SYSTEM/oracle@localhost:1521/XEPDB1 <<'EOF'
SELECT username FROM dba_users WHERE username = '<USERNAME>';
EOF
# returns 0 rows.

# Root rotation
$ bao write -force database/rotate-root/oracle
$ bao read database/creds/readonly  # still works
```

### Remote plugin

```bash
$ bao write database/config/spoke-oracle \
    plugin_name=remote-oracle-plugin \
    spoke_name=spoke-1 \
    connection_url='oracle://{{username}}:{{password}}@oracle:1521/XEPDB1' \
    username='SYSTEM' password='oracle' \
    allowed_roles='readonly'
```

### Namespace isolation

```bash
$ bao namespace create billing
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/oracle ...
```

### Failure modes to spot-check

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Root lacks `ALTER SYSTEM` | Default revoke can't kill sessions; if user has an active session, DROP USER fails with ORA-01940. Either grant the role or supply `revocation_statements` that disconnects sessions another way. |
| Username contains `"` | Default revoke errors at Oracle DDL layer |
| `connection_url` missing | Initialize fails with `connection_url cannot be empty` |
| Statement with unknown placeholder | Plugin renders empty for unknown keys, Oracle returns the syntax error |
