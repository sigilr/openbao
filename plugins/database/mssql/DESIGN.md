<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# MSSQL Database Plugin — Design

## Scope

`mssql-database-plugin` implements the OpenBao database secrets-engine v5
contract against **Microsoft SQL Server** using
`github.com/microsoft/go-mssqldb`. Supports dynamic credentials, static-role
password rotation, and root rotation. Also exposed remotely as
`remote-mssql-plugin` via the
[remote-db-plugin](../remote-db-plugin/DESIGN.md) hub-and-spoke runner.

## Provenance and license

Ported from the pre-BUSL HashiCorp Vault `plugins/database/mssql` plugin
(commit `90e314d5da`, last commit under MPL-2.0). Imports rewritten to
`openbao/openbao/sdk/v2`. Dual copyright (HashiCorp + OpenBao) under MPL-2.0.

## Architecture

```
                +------------------+
                |  OpenBao Core    |
                |  (database mount)|
                +---------+--------+
                          | v5 gRPC
                          v
                +------------------+
                |  mssql-          |
                |  database-plugin |
                +---------+--------+
                          | go-mssqldb / TDS
                          v
                +------------------+
                |   SQL Server     |
                +------------------+
```

`MSSQL` embeds `connutil.SQLConnectionProducer` for shared connection_url
plumbing, max-conns, root rotation, and namespace-scoped lifecycle.

## Configuration

- `connection_url`: `sqlserver://{{username}}:{{password}}@host:port?database=...`
  or any URL accepted by `go-mssqldb`.
- `username` / `password`: bootstrap root credentials.
- `username_template`: override the default name template.
- `contained_db` (default `false`): set `true` if the target is a
  contained database. The revoke path changes to a DB-user-only DROP
  instead of disabling the server login and enumerating DB users via
  `sp_msloginmappings`.

## Lifecycle

### NewUser

Run all `creation_statements` in one transaction. Each statement is split
on `;`, templated with `{{name}}`, `{{password}}`, `{{expiration}}`, and
executed via `dbtxn.ExecuteTxQueryDirect`. Expiration is informational
only — MSSQL has no `VALID UNTIL`; OpenBao + DeleteUser bound it.

### UpdateUser

- Password change only. Default statement (non-contained DB): `ALTER LOGIN
  [{{username}}] WITH PASSWORD = '{{password}}'`. For contained DBs, the
  caller must supply a custom `password` statement.
- `Expiration` updates are a no-op (intentional).

### DeleteUser (default revoke, non-contained DB)

1. `ALTER LOGIN ... DISABLE` (parameterized via `sql.Named` + `QuoteName`).
2. Enumerate active sessions via `sys.dm_exec_sessions` and emit `KILL`
   for each.
3. Enumerate per-database users via `sp_msloginmappings` and emit
   `DROP USER` per database.
4. Run all collected statements best-effort (keep going on individual
   failures so we revoke as much access as possible).
5. `DROP LOGIN ...` (parameterized via `sql.Named` + `QuoteName`).

### DeleteUser (default revoke, contained DB)

`DROP USER IF EXISTS @username` against the current database, with
`@username` bound through `sql.Named` + `QuoteName` so identifier
quoting happens server-side.

### DeleteUser (custom)

Statements run with `{{name}}` bound to the request username. Each
statement runs against the connection (not a transaction) so a partial
failure leaves the cleanup at "best-effort"; multierror captures all of
them.

## Namespace support

Per-namespace mounts work without plugin-side changes. See
[OpenBao namespaces](https://openbao.org/docs/concepts/namespaces/).

## Remote variant

`remote-mssql-plugin` is a one-line registration in
`helper/builtinplugins/registry.go` plus a `case` in
`plugins/database/remote-db-plugin/runner/runner.go:loadPlugin`.

## Failure modes

- Empty `creation_statements` → `dbutil.ErrEmptyCreationStatement`.
- Default revoke against a non-contained DB requires the configured root
  to be able to query `sys.dm_exec_sessions` and run `sp_msloginmappings`
  (typically `sysadmin` or a role with `VIEW SERVER STATE`).
- Custom revoke statements run against the connection, not a transaction,
  so partial failures leave the user partially revoked. multierror
  aggregates them.

## Tests

`mssql_test.go` runs always-on unit tests:

- `Type` / `PluginVersion`
- Username template shape
- `contained_db` parsing (bool, "true" string, false, garbage)

Acceptance tests are gated on `BAO_ACC=1` + `MSSQL_URL`. See
[TEST.md](TEST.md) for the manual plan.
