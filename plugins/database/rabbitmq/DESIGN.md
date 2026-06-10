<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# RabbitMQ Database Plugin — Design

## Scope

`rabbitmq-database-plugin` is a v5 database-plugin counterpart of the
existing `rabbitmq/` secrets engine. Same user-management semantics
(internal-realm users + per-vhost permissions + topic permissions), but
exposed via the unified database engine so roles, leases, and the
remote-db-plugin hub/spoke flow all behave the same as for SQL plugins.

Uses [`rabbit-hole/v3`](https://github.com/michaelklishin/rabbit-hole) (the
canonical Go client for the RabbitMQ HTTP management API) — the same
library the secrets engine uses.

Also exposed remotely as `remote-rabbitmq-plugin`.

## Architecture

```
+------------------+   gRPC   +-------------------+   HTTP   +---------------+
|  OpenBao Core    |--------->| rabbitmq-database |--------->| RabbitMQ      |
|  database mount  |          | -plugin           |          | management API|
+------------------+          +-------------------+          +---------------+
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `connection_uri` | yes | `http(s)://rabbitmq.example.com:15672` |
| `username` / `password` | yes | Management API credentials |
| `tls_ca` | no | PEM CA bundle |
| `tls_certificate` / `tls_key` | no | mTLS client identity PEM |
| `insecure` | no | Skip TLS verify (dev only) |
| `username_template` | no | Override the dynamic-username template |

## Creation statement

```json
{
  "tags": "administrator",
  "vhosts": {
    "/": {"configure":".*", "write":".*", "read":".*"}
  },
  "vhost_topics": {
    "/": {
      "amq.topic": {"write":".*", "read":".*"}
    }
  }
}
```

- `tags`: comma-separated list (e.g. `administrator,management`).
- `vhosts`: map of vhost → `{configure, write, read}` regexes (RabbitMQ
  semantics).
- `vhost_topics`: map of vhost → map of exchange → `{write, read}` regexes.
- At least one of `tags` or `vhosts` is required.

## Lifecycle

### NewUser
1. Generate username via the template.
2. `PUT /api/users/<name>` with password and tags.
3. For each vhost, `PUT /api/permissions/<vhost>/<name>`.
4. For each topic permission, `PUT /api/topic-permissions/<vhost>/<name>`.

If any step after `PutUser` fails, the plugin deletes the half-configured
user before returning so we don't leak entries on partial failure.

### UpdateUser
- Password change: re-PUT the user with the new password, preserving the
  existing tags (fetched via `GetUser` first).
- Expiration: no-op (RabbitMQ has no native VALID UNTIL).

### DeleteUser
`DELETE /api/users/<name>`. 404 is treated as success (idempotent revoke).

## Remote variant

`remote-rabbitmq-plugin` is registered in
`helper/builtinplugins/registry.go` and the runner's `loadPlugin` switch.

## Failure modes

| Scenario | Behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Missing `tags` and `vhosts` | "creation_statements requires at least one of: vhosts, tags" |
| `connection_uri` missing | Init fails with "connection_uri is required" |
| Permission set fails mid-flight | Plugin deletes the user before returning the error |
| `Delete` on missing user | 404 → success |

## Tests

`rabbitmq_test.go` runs always-on:

- `Type` / `PluginVersion`
- `parseTags` (empty, single, list, whitespace handling)
- JSON parsing of `rmqStatement`
- `UpdateUser` validation (missing username; no changes)
- `NewUser` rejects empty `creation_statements`

Acceptance flow is documented in [TEST.md](TEST.md).
