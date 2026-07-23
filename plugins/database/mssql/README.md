<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Microsoft SQL Server Database Plugin

OpenBao plugin that issues dynamic credentials against **SQL Server**.
Available built-in as `mssql-database-plugin` and proxied through the
[remote-db-plugin](../remote-db-plugin/README.md) as `remote-mssql-plugin`.

See [DESIGN.md](DESIGN.md) for architecture and [TEST.md](TEST.md) for
the test plan.

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/mssql \
    plugin_name=mssql-database-plugin \
    connection_url='sqlserver://{{username}}:{{password}}@mssql.example.com:1433' \
    allowed_roles='readonly' \
    username='sa' password='Pass-1234'

$ bao write database/roles/readonly \
    db_name=mssql \
    creation_statements="CREATE LOGIN [{{name}}] WITH PASSWORD = '{{password}}';
                         CREATE USER [{{name}}] FOR LOGIN [{{name}}];
                         GRANT SELECT, INSERT, UPDATE, DELETE TO [{{name}}];" \
    default_ttl=1h max_ttl=24h

$ bao read database/creds/readonly
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `mssql-database-plugin` or `remote-mssql-plugin` |
| `connection_url` | yes | `sqlserver://{{username}}:{{password}}@host:port?database=...` |
| `username` / `password` | yes | Root credentials |
| `allowed_roles` | yes | `*` or comma-separated list |
| `contained_db` | no | `true` for contained DBs (changes the default revoke path) |
| `max_open_connections` | no | Default `4` |
| `max_idle_connections` | no | Default `0` |
| `max_connection_lifetime` | no | Duration; default unlimited |
| `username_template` | no | Override the dynamic-username template |
| `spoke_name` | yes (remote) | Spoke that will execute the requests |

## Role statements

Templated placeholders: `{{name}}`, `{{username}}`, `{{password}}`,
`{{expiration}}`.

### Recommended creation statement (server login + DB user)

```sql
CREATE LOGIN [{{name}}] WITH PASSWORD = '{{password}}';
CREATE USER [{{name}}] FOR LOGIN [{{name}}];
GRANT SELECT, INSERT, UPDATE, DELETE TO [{{name}}];
```

### Contained-DB creation statement

```sql
CREATE USER [{{name}}] WITH PASSWORD = '{{password}}';
GRANT SELECT TO [{{name}}];
```

Set `contained_db=true` on the config so the default revoke path runs the
corresponding contained `DROP USER`.

### Default revoke (non-contained)

1. `ALTER LOGIN [{{name}}] DISABLE`
2. `KILL` for every active session belonging to the login
3. `DROP USER` per database where the login is mapped
4. `DROP LOGIN [{{name}}]`

To override, set `revocation_statements` on the role.

### Default password change

```sql
ALTER LOGIN [{{username}}] WITH PASSWORD = '{{password}}'
```

Contained DBs have no default — supply `password_policy` /
`rotation_statements` instead.

## Static roles & root rotation

```bash
$ bao write database/static-roles/svc \
    db_name=mssql username=svc rotation_period=24h

$ bao write -force database/rotate-root/mssql
```

## Namespaces

Per-namespace mounts work without plugin-side changes:

```bash
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/mssql ...
```

## Remote variant

```bash
$ bao write database/config/spoke-mssql \
    plugin_name=remote-mssql-plugin \
    spoke_name=spoke-1 \
    connection_url='sqlserver://{{username}}:{{password}}@mssql:1433' \
    username='sa' password='Pass-1234' \
    allowed_roles='*'
```

## Building

```
$ make mssql-database-plugin
```

Output: `bin/mssql-database-plugin`. The same code is also linked into
the `bao` binary as a built-in.

## License

Copyright &copy; HashiCorp, Inc. and AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
