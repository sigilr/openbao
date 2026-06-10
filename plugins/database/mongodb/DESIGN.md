<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# MongoDB Database Plugin — Design

## Scope

`mongodb-database-plugin` implements the OpenBao database secrets-engine v5
contract against a **MongoDB cluster** using the official
[mongo-driver](https://pkg.go.dev/go.mongodb.org/mongo-driver) Go driver.
It supports dynamic credentials, static-role password rotation, and root
credential rotation. Roles are MongoDB role documents (JSON), not SQL.

Also exposed remotely as `remote-mongodb-plugin` through the
[remote-db-plugin](../remote-db-plugin/DESIGN.md) hub-and-spoke runner.

## Provenance and license

Ported from the pre-BUSL HashiCorp Vault `plugins/database/mongodb`
plugin (commit `53cbcd3f34`, last commit under MPL-2.0). Imports rewritten
to `openbao/openbao/sdk/v2`. The plugin keeps its dual copyright (HashiCorp
+ OpenBao) and the MPL-2.0 license header.

## Architecture

```
                +------------------+
                |  OpenBao Core    |
                |  (database mount)|
                +---------+--------+
                          | v5 gRPC
                          v
                +------------------+
                |  mongodb-        |
                |  database-plugin |
                |  (go-plugin sub) |
                +---------+--------+
                          | mongo-driver / MongoDB wire protocol
                          v
                +------------------+
                |   MongoDB        |
                +------------------+
```

`MongoDB` does **not** use `connutil.SQLConnectionProducer` (MongoDB isn't
a SQL connection). It carries its own `mongoDBConnectionProducer` that
manages a long-lived `*mongo.Client`, with locking, lazy connection,
ping-on-reuse, and TLS / write-concern / timeout configuration.

## Connection

- `connection_url` template: `mongodb://{{username}}:{{password}}@host:port`
  or any valid Mongo connection string (SRV, replica set, options).
- `username` / `password`: bootstrapped root credentials.
- `write_concern`: optional JSON or base64-encoded JSON. Base64 is a
  convenience for CI systems that can't pass literal braces.
- `tls_ca`, `tls_certificate_key`: TLS options. When a client cert is set,
  the plugin switches to `MONGODB-X509` SASL auth.
- `socket_timeout`, `connect_timeout`, `server_selection_timeout`: durations
  with sensible 1-minute defaults.

## Creation statement schema

A creation statement is a single JSON document:

```json
{
  "db": "admin",
  "roles": [
    { "role": "readWrite" },
    { "role": "readWrite", "db": "app" }
  ]
}
```

- `db` (optional, default `"admin"`) — authentication database.
- `roles` (required) — array of role documents. Bare roles flatten to a
  string, db-qualified roles stay as objects (matches MongoDB's `createUser`
  expectation).

## Lifecycle

### NewUser

1. Generate a username (default template emits 100-char hyphenated names,
   safe for Mongo identifiers).
2. Parse `Commands[0]` as a `mongoDBStatement`.
3. Run `createUser` against the `db` from the statement (or `"admin"`).

### UpdateUser

- Only `Password` is supported. MongoDB has no native VALID UNTIL; lease
  expiration is enforced by OpenBao + DeleteUser.
- The plugin picks the database from the configured `connection_url`'s
  default db, falling back to `"admin"`. For root rotation (when the
  target username equals the configured root), always targets `"admin"`.

### DeleteUser

- If `Statements.Commands` is empty: drop with default write concern
  (majority) against `"admin"`.
- If one statement is supplied: parse as `{"db": ..., ...}` to override
  the authentication database.
- Treats `UserNotFound` as success — OpenBao may race with manual
  cleanup and we don't want a retry storm.

## Retry on EOF

Long-lived Mongo connections sometimes drop and the driver surfaces this as
an `io.EOF` on the first attempt. `runCommandWithRetry` reconnects via
`Connection(ctx)` (which itself does a Ping and reconnects on failure) and
retries once.

## Namespace support

Per-namespace database mounts work without plugin-side changes: the
`Backend` lifecycle is owned by core, and the plugin only sees the request
+ the mount config.

See [OpenBao namespaces](https://openbao.org/docs/concepts/namespaces/).

## Remote variant

`remote-mongodb-plugin` is a one-line registration:

- `helper/builtinplugins/registry.go`: `"remote-mongodb-plugin":
  {Factory: dbRemote.New("mongodb-database-plugin")}`
- `plugins/database/remote-db-plugin/runner/runner.go:loadPlugin`: a
  `case "mongodb-database-plugin":` that returns `dbMongo.New`.

On the hub, requests against `remote-mongodb-plugin` are proxied to the
spoke identified by `spoke_name` over the mTLS gRPC stream. On the spoke,
the in-process `mongodb-database-plugin` does the actual work.

## Failure modes

- **Empty `creation_statements`** → `dbutil.ErrEmptyCreationStatement`.
- **More than one revocation statement** → explicit error; the format
  doesn't compose.
- **TLS misconfiguration** → `failed to append CA to client options` or
  `unable to load tls_certificate_key_data`; surfaced from Init.
- **`UserNotFound` on revoke** → logged at WARN, treated as success.
- **Connection drop mid-request** → single transparent retry; second
  failure propagates.

## Tests

`mongodb_test.go` runs always-on unit tests for:

- Type and PluginVersion
- Username template shape
- JSON parsing of `mongoDBStatement`
- `toStandardRolesArray` flattening
- `getWriteConcern` for raw JSON, base64, empty, and garbage inputs
- `loadConfig` error paths

Acceptance tests are gated on `BAO_ACC=1` + `MONGODB_URL`. See
[TEST.md](TEST.md) for the manual plan.
