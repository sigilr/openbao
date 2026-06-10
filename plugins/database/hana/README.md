<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# SAP HANA Database Plugin

OpenBao plugin that issues dynamic database credentials against **SAP
HANA**. Implements the v5 database plugin contract and is exposed both as
the in-process built-in `hana-database-plugin` and, via the hub-and-spoke
[remote-db-plugin](../remote-db-plugin/README.md), as `remote-hana-plugin`.

See [DESIGN.md](DESIGN.md) for the architecture and
[TEST.md](TEST.md) for the manual test plan.

## Quick start

Mount the database secrets engine and configure a HANA connection:

```bash
$ bao secrets enable database

$ bao write database/config/my-hana \
    plugin_name=hana-database-plugin \
    connection_url='hdb://{{username}}:{{password}}@hana.example.com:39041' \
    allowed_roles='readonly,readwrite' \
    username='SYSTEM' \
    password='your-system-password'
```

Define a role with creation statements:

```bash
$ bao write database/roles/readonly \
    db_name=my-hana \
    creation_statements='CREATE USER {{name}} PASSWORD "{{password}}" NO FORCE_FIRST_PASSWORD_CHANGE VALID UNTIL '"'"'{{expiration}}'"'"';
                         GRANT SELECT ON SCHEMA SYSTEM TO {{name}};' \
    default_ttl=1h \
    max_ttl=24h
```

Request a credential:

```bash
$ bao read database/creds/readonly
Key                Value
---                -----
lease_id           database/creds/readonly/abc123…
lease_duration     1h
lease_renewable    true
password           AbCdEf1234567890QwRtYi
username           V_TOKEN_READONLY_3PFW6T8RVMC8MBKZ8ZEF_1717000000
```

The username is uppercased and underscore-separated because HANA
identifiers are case-folded and reject hyphens.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `hana-database-plugin` (built-in) or `remote-hana-plugin` (proxied through a spoke) |
| `connection_url` | yes | `hdb://{{username}}:{{password}}@host:port` |
| `username` / `password` | yes | Root credentials used to issue / revoke users |
| `allowed_roles` | yes | Comma-separated list of role names allowed on this mount |
| `max_open_connections` | no | Default `4` |
| `max_idle_connections` | no | Default `0` |
| `username_template` | no | Override the dynamic-username template |
| `verify_connection` | no | Default `true` |
| `spoke_name` | yes (remote variant) | Name of the spoke that will execute the requests |

## Role statements

Templated placeholders:

- `{{name}}` — generated username (uppercased, underscores)
- `{{username}}` — alias of `{{name}}`, used by update statements
- `{{password}}` — generated password
- `{{expiration}}` — UTC, format `2006-01-02 15:04:05`

Recommended creation statement:

```sql
CREATE USER {{name}} PASSWORD "{{password}}" NO FORCE_FIRST_PASSWORD_CHANGE VALID UNTIL '{{expiration}}';
GRANT <required-privileges> TO {{name}};
```

Default revocation (when `revocation_statements` is empty):

```sql
ALTER USER "<username>" DEACTIVATE USER NOW;
DROP USER "<username>" RESTRICT;
```

To force a hard drop:

```bash
$ bao write database/roles/readonly \
    revocation_statements='DROP USER {{name}} CASCADE;'
```

Default password change (when `update_statements.password` is empty):

```sql
ALTER USER "{{username}}" PASSWORD "{{password}}";
```

## Static roles

```bash
$ bao write database/static-roles/app-svc \
    db_name=my-hana \
    username=APPSVC \
    rotation_period=24h
```

Plus a rotation statement if your environment needs custom SQL:

```bash
$ bao write database/static-roles/app-svc \
    db_name=my-hana \
    username=APPSVC \
    rotation_period=24h \
    rotation_statements='ALTER USER {{username}} PASSWORD "{{password}}" NO FORCE_FIRST_PASSWORD_CHANGE;'
```

## Root rotation

```bash
$ bao write -force database/rotate-root/my-hana
```

OpenBao rewrites the configured password to a randomly generated one and
discards the old value. The original password is no longer recoverable
afterward — make sure no other system depends on it.

## Namespaces

The plugin runs identically per-namespace. Mount the database engine
inside a namespace and all the commands above apply with the namespace
prefix:

```bash
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/my-hana ...
```

See [OpenBao namespaces](https://openbao.org/docs/concepts/namespaces/) for
the full model.

## Remote variant

For deployments where OpenBao runs in a hub cluster but HANA is reachable
only from a spoke cluster, use `remote-hana-plugin`. Operators install
`bao relay` on the spoke, run `bao relay join` once to obtain an mTLS
client cert, and then `bao relay run` exposes the spoke to the hub. Mount
configuration on the hub is identical, except `plugin_name=remote-hana-plugin`
and an additional `spoke_name=...`:

```bash
$ bao write database/config/spoke-hana \
    plugin_name=remote-hana-plugin \
    spoke_name=spoke-1 \
    connection_url='hdb://{{username}}:{{password}}@hana:39041' \
    username='SYSTEM' \
    password='your-system-password' \
    allowed_roles='*'
```

See [../remote-db-plugin/README.md](../remote-db-plugin/README.md) for the
trust bootstrap and operations.

## Building

```
$ make hana-database-plugin
```

Output: `bin/hana-database-plugin`. The same code is also linked into the
`bao` binary as a built-in, so no separate registration is needed when
running against OpenBao.

## License

Copyright &copy; HashiCorp, Inc. and AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
