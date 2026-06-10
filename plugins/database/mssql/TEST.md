<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# MSSQL Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/mssql/...
```

Covers:

- `Type` / `PluginVersion`.
- Default username template renders into `v-<display>-<role>-<random>-<unix>`.
- `contained_db` parsing for bool, "true" string, false, garbage.

## Acceptance tests

Gated on `BAO_ACC=1` + `MSSQL_URL=sqlserver://sa:Pass@host:port`.

```
$ export BAO_ACC=1
$ export MSSQL_URL='sqlserver://sa:Pass-1234@localhost:1433?database=master'
$ go test -v -count=1 -timeout=10m ./plugins/database/mssql/
```

### Local SQL Server via Docker

```
$ docker run --rm -d --name mssql \
    -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD=Pass-1234 \
    -p 1433:1433 mcr.microsoft.com/mssql/server:2022-latest
```

Wait until `SQL Server is now ready` appears in the logs.

### Coverage

- `TestMSSQL_NewUser` — server login + DB user creation + grants; verifies
  the user can authenticate, then cleans up via DeleteUser.
- `TestMSSQL_UpdateUser` — password rotation; verifies the new password
  works.

## Manual end-to-end with `bao`

### Built-in plugin

```bash
$ make mssql-database-plugin
$ bao server -dev

$ export BAO_ADDR='http://127.0.0.1:8200'
$ bao login root
$ bao secrets enable database

$ bao write database/config/mssql \
    plugin_name=mssql-database-plugin \
    connection_url='sqlserver://{{username}}:{{password}}@localhost:1433' \
    allowed_roles='readonly' \
    username='sa' password='Pass-1234'

$ bao write database/roles/readonly \
    db_name=mssql \
    creation_statements="CREATE LOGIN [{{name}}] WITH PASSWORD = '{{password}}';
                         CREATE USER [{{name}}] FOR LOGIN [{{name}}];
                         GRANT SELECT TO [{{name}}];" \
    default_ttl=1h max_ttl=24h

$ bao read database/creds/readonly

# Verify the user logs in:
$ sqlcmd -S localhost -U '<USERNAME>' -P '<PASSWORD>' \
    -Q "SELECT @@VERSION"

# Revoke and check the login is dropped:
$ bao lease revoke <LEASE_ID>
$ sqlcmd -S localhost -U sa -P 'Pass-1234' \
    -Q "SELECT name FROM sys.server_principals WHERE name = '<USERNAME>'"
# returns 0 rows.

# Root rotation
$ bao write -force database/rotate-root/mssql
$ bao read database/creds/readonly  # still works
```

### Contained DB

```bash
$ bao write database/config/mssql-contained \
    plugin_name=mssql-database-plugin \
    connection_url='sqlserver://{{username}}:{{password}}@localhost:1433?database=AppDB' \
    contained_db=true \
    allowed_roles='svc' \
    username='sa' password='Pass-1234'

$ bao write database/roles/svc \
    db_name=mssql-contained \
    creation_statements="CREATE USER [{{name}}] WITH PASSWORD = '{{password}}'; GRANT SELECT TO [{{name}}];" \
    default_ttl=1h
```

The default revoke runs `DROP USER IF EXISTS` against the contained
database; no `ALTER LOGIN`, no `sp_msloginmappings`.

### Remote plugin

```bash
$ bao write database/config/spoke-mssql \
    plugin_name=remote-mssql-plugin \
    spoke_name=spoke-1 \
    connection_url='sqlserver://{{username}}:{{password}}@mssql:1433' \
    username='sa' password='Pass-1234' \
    allowed_roles='readonly'
```

### Namespace isolation

```bash
$ bao namespace create billing
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/mssql ...
```

### Failure modes to spot-check

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Root lacks `VIEW SERVER STATE` | Default revoke fails to enumerate sessions; partial revoke + multierror |
| Contained DB without `contained_db=true` | Default revoke runs `ALTER LOGIN`, which fails (no server login exists) |
| `contained_db="nope"` | Init fails with `invalid value for "contained_db"` |
| `update` with neither password nor expiration | "no changes requested" |
