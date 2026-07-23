<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# DocumentDB Database Plugin — Design

## Scope

`documentdb-database-plugin` issues dynamic credentials against the
**open-source DocumentDB engine** —
[github.com/documentdb/documentdb](https://github.com/documentdb/documentdb) —
a PostgreSQL extension that exposes a MongoDB-compatible wire protocol
via a gateway process. The same engine powers Azure Cosmos DB for
MongoDB vCore.

Because the gateway speaks the mongo wire protocol, the plugin uses
the official `mongo-driver`. The role-document creation-statement shape
(`{"db":"admin","roles":[…]}`) and the idempotent revoke + EOF
retry-once behaviour mirror OpenBao's MongoDB plugin.

Exposed both built-in (`documentdb-database-plugin`) and via the
remote-db hub/spoke runner (`remote-documentdb-plugin`).

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `connection_url` | yes | Mongo URI pointing at the documentdb gateway, e.g. `mongodb://{{username}}:{{password}}@host:10260/?tls=true` |
| `username` / `password` | yes | Bootstrap root credentials |
| `tls_ca` | no | Custom CA PEM (for the upstream docker quickstart, which issues a self-signed cert) |
| `tls_ca_path` | no | Path to a custom CA PEM |
| `insecure` | no | Skip TLS verification — pairs with the docker quickstart's self-signed gateway cert; **dev only** |
| `connect_timeout` / `socket_timeout` / `server_selection_timeout` | no | Mongo driver timeouts (default 1 minute each) |
| `username_template` | no | Override the default username template |
| `spoke_name` | yes (remote) | Spoke that will execute the requests |

## Differences from AWS DocumentDB

- Retryable writes are **not** forced off. The OSS engine supports them
  because PostgreSQL's transaction layer absorbs the retry semantics.
  Operators who run an older gateway and hit issues can disable retries
  via `?retryWrites=false` in `connection_url`.
- TLS is **not** mandatory. The upstream docker quickstart enables it
  with a self-signed cert; `tls_ca` / `tls_ca_path` / `insecure` cover
  that case. Production clusters typically front the gateway with a
  publicly-trusted cert, in which case no TLS config is needed.

## Creation statement

```json
{
  "db": "admin",
  "roles": [{"role": "readWrite", "db": "app"}]
}
```

Same shape as the MongoDB plugin. The exact roles available depend on
the documentdb version — refer to the upstream docs for the supported
RBAC catalog.

## Lifecycle

- **NewUser** — parses the statement, runs `createUser` against the
  named auth database (default `"admin"`).
- **UpdateUser** — password change via `updateUser`. Expiration is a
  no-op (the gateway has no native VALID UNTIL).
- **DeleteUser** — `dropUser`. `UserNotFound` is logged at WARN and
  treated as success.

## Failure modes

| Scenario | Behavior |
| --- | --- |
| URL missing `tls=true` against a TLS-enabled gateway | Connection rejected by the gateway; surfaced during Initialize |
| Bad CA PEM | `failed to parse tls_ca PEM` |
| `roles: []` in statement | "roles array is required in creation statement" |
| `UserNotFound` on revoke | Logged WARN, treated as success |

## Tests

- `Type` / `PluginVersion`
- Username template renders into the expected shape
- JSON statement parsing
- Bad TLS PEM is rejected

Acceptance tests are gated on `BAO_ACC=1` + `DOCDB_URL`.
