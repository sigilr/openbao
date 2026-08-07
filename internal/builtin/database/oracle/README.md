<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Oracle Database Plugin

OpenBao plugin that issues dynamic credentials against **Oracle Database**.
Built on the pure-Go [`sijms/go-ora`](https://github.com/sijms/go-ora)
driver — no Oracle Instant Client required. Available built-in as
`oracle-database-plugin` and proxied through the
[remote-db-plugin](../remote-db-plugin/README.md) as `remote-oracle-plugin`.

See [DESIGN.md](DESIGN.md) for architecture and [TEST.md](TEST.md) for the
test plan.

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/my-oracle \
    plugin_name=oracle-database-plugin \
    connection_url='oracle://{{username}}:{{password}}@oracle.example.com:1521/XEPDB1' \
    allowed_roles='readonly' \
    username='SYSTEM' password='oracle'

$ bao write database/roles/readonly \
    db_name=my-oracle \
    creation_statements='CREATE USER {{name}} IDENTIFIED BY "{{password}}";
                         GRANT CREATE SESSION TO {{name}};
                         GRANT SELECT ANY TABLE TO {{name}};' \
    default_ttl=1h max_ttl=24h

$ bao read database/creds/readonly
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `oracle-database-plugin` or `remote-oracle-plugin` |
| `connection_url` | yes | `oracle://{{username}}:{{password}}@host:port/service` |
| `username` / `password` | yes | Root credentials (often `SYSTEM`) |
| `allowed_roles` | yes | `*` or comma-separated list |
| `max_open_connections` | no | Default `4` |
| `max_idle_connections` | no | Default `0` |
| `max_connection_lifetime` | no | Duration; default unlimited |
| `username_template` | no | Override the dynamic-username template |
| `spoke_name` | yes (remote) | Spoke that will execute the requests |

## Role statements

Templated placeholders: `{{name}}`, `{{username}}`, `{{password}}`,
`{{expiration}}`.

### Recommended creation

```sql
CREATE USER {{name}} IDENTIFIED BY "{{password}}";
GRANT CREATE SESSION TO {{name}};
GRANT SELECT, INSERT, UPDATE, DELETE ON some.table TO {{name}};
```

### Default revoke

If `revocation_statements` is empty, the plugin:

1. Kills any active sessions for the user (via `v$session` + `ALTER SYSTEM KILL SESSION`).
2. Runs `DROP USER "<quoted-username>" CASCADE`.

To override, set `revocation_statements` on the role.

### Default password change

```sql
ALTER USER {{username}} IDENTIFIED BY "{{password}}";
```

## Static roles

```bash
$ bao write database/static-roles/svc \
    db_name=my-oracle username=SVC rotation_period=24h
```

## Root rotation

```bash
$ bao write -force database/rotate-root/my-oracle
```

The configured root password is replaced with a freshly generated one and
the original is no longer recoverable.

## Namespaces

Per-namespace mounts work without plugin-side changes:

```bash
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/my-oracle ...
```

See [OpenBao namespaces](https://openbao.org/docs/concepts/namespaces/).

## Remote variant

```bash
$ bao write database/config/spoke-oracle \
    plugin_name=remote-oracle-plugin \
    spoke_name=spoke-1 \
    connection_url='oracle://{{username}}:{{password}}@oracle:1521/XEPDB1' \
    username='SYSTEM' password='oracle' \
    allowed_roles='*'
```

## Building

```
$ make oracle-database-plugin
```

Output: `bin/oracle-database-plugin`. The same code is also linked into
the `bao` binary as a built-in.

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
