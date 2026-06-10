<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# HanaDB Database Plugin — Design

## Scope

`hana-database-plugin` implements the OpenBao database secrets-engine v5
contract (`Initialize`, `NewUser`, `UpdateUser`, `DeleteUser`, `Close`,
`Type`) against **SAP HANA**. It supports dynamic credentials, static-role
password rotation, and root credential rotation. The plugin is an OpenBao
port of the pre-BUSL HashiCorp Vault `plugins/database/hana` plugin (commit
`9aa7fee682`, MPL-2.0), with imports rewritten to `openbao/openbao/sdk/v2`
and SQL injection in the default revoke path fixed via
`dbutil.QuoteIdentifier`.

The plugin is registered in `helper/builtinplugins/registry.go` and also
exposed remotely as `remote-hana-plugin` through the hub-and-spoke
[remote-db-plugin](../remote-db-plugin/DESIGN.md).

## Provenance and license

The plugin source is licensed under MPL-2.0 and carries dual copyright
notices for the original HashiCorp contribution and OpenBao modifications,
per the conventions used by other ported plugins in this tree.

## Architecture

```
                +------------------+
                |  OpenBao Core    |
                |  (database mount)|
                +---------+--------+
                          | v5 gRPC
                          v
                +------------------+
                |  hana-database-  |
                |  plugin (go-plugin
                |  subprocess)     |
                +---------+--------+
                          | go-hdb driver / SAP HDB protocol
                          v
                +------------------+
                |   SAP HANA       |
                +------------------+
```

`HANA` embeds `connutil.SQLConnectionProducer` so the standard
`connection_url`, `max_open_connections`, `max_idle_connections`, root
rotation, namespace-scoped mount lifecycle, and the v5 error sanitizer
middleware all behave exactly like every other SQL-backed OpenBao DB
plugin.

## Connection

- `connection_url` template: `hdb://{{username}}:{{password}}@host:port`
  (HDB protocol port, typically `3<NN>15`).
- `username` / `password`: bootstrapped root credentials. These are the
  values OpenBao uses to log into HANA before any dynamic users exist; root
  rotation re-issues a fresh password into the same DB user.
- `password_authentication`: unused for HANA (HANA hashing is server-side);
  the v5 interface still accepts it for symmetry with Postgres.

## Identifiers and SQL contract

HANA identifiers are **case-folded to uppercase** and **reject hyphens**.
The default username template emits an uppercase, underscore-separated
identifier. After template rendering, `NewUser` additionally:

1. replaces `-` with `_`, and
2. uppercases the result,

so a custom template that returns mixed case still produces a HANA-legal
identifier. Callers needing length-bounded names should add `truncate` to
their template.

The plugin exposes the standard templated placeholders to creation /
revocation / rotation statements:

- `{{name}}` — generated username (uppercased, underscore-separated)
- `{{username}}` — alias of `{{name}}`, used by update statements
- `{{password}}` — generated password
- `{{expiration}}` — `2006-01-02 15:04:05` UTC

## Lifecycle

### NewUser

1. Acquire the producer mutex (so two concurrent requests can't race the
   shared `*sql.DB` initialization).
2. Generate a username via the configured template, then HANA-clean it.
3. Open a transaction, run every semicolon-separated statement from the
   role's `creation_statements`, commit.
4. Return the username.

If `creation_statements` is empty, `NewUser` returns
`dbutil.ErrEmptyCreationStatement`.

### UpdateUser

If both `Password` and `Expiration` are nil, returns no-op.

Default password statement when none is provided:

```sql
ALTER USER {{username}} PASSWORD "{{password}}"
```

Default expiration statement:

```sql
ALTER USER {{username}} VALID UNTIL '{{expiration}}'
```

### DeleteUser (default revoke)

When `revocation_statements` is empty:

```sql
ALTER USER <quoted-username> DEACTIVATE USER NOW
DROP USER <quoted-username> RESTRICT
```

`RESTRICT` fails if the user still owns objects — operators who want a hard
drop must provide `revocation_statements` containing `DROP USER {{name}}
CASCADE;`. The identifier is quoted via `dbutil.QuoteIdentifier`, which
prevents the SQL-injection class flagged in
[HCSEC-2025-12 / VAULT-43691](https://discuss.hashicorp.com/c/security/52)
on the upstream Vault plugin.

### DeleteUser (custom revoke)

Each statement is split on `;` and run inside a single transaction with the
`{{name}}` placeholder bound to the request's username.

## Namespace support

Every OpenBao namespace mounts its own database secrets engine; the plugin
runs identically per-namespace because the `Backend` lifecycle is owned by
core, not the plugin. Configuration, roles, and dynamic credentials all
inherit the mounting namespace. No plugin-side changes are needed.

For details on the namespace model, see
[OpenBao namespaces documentation](https://openbao.org/docs/concepts/namespaces/).

## Static credentials and root rotation

Static-role password rotation goes through `UpdateUser` with `Password`
set. Root rotation rewrites the embedded `connutil.SQLConnectionProducer`'s
stored password via the framework — no plugin code is involved.

## Remote variant

`remote-hana-plugin` is a one-line registration:

- `helper/builtinplugins/registry.go`: `"remote-hana-plugin": {Factory:
  dbRemote.New("hana-database-plugin")}`
- `plugins/database/remote-db-plugin/runner/runner.go:loadPlugin`: a
  `case "hana-database-plugin":` that returns `dbHana.New`.

On the hub, requests against `remote-hana-plugin` are proxied to the spoke
identified by `spoke_name` over the existing mTLS gRPC stream. On the
spoke, the in-process `hana-database-plugin` does the actual work against
a HANA instance reachable from the spoke. See
[remote-db-plugin/DESIGN.md](../remote-db-plugin/DESIGN.md) for the wire
protocol, trust bootstrap, and request lifecycle.

## Failure modes

- **Driver not loaded**: a blank import of `github.com/SAP/go-hdb/driver`
  registers the `hdb` SQL driver. If the binary is built with `-tags
  nohdb` (none currently exist), `sql.Open("hdb", ...)` will error.
- **HANA `DEACTIVATE` requires SYSTEM-privileged credentials**: the
  configured root user must have `USER ADMIN` or equivalent. Lower-privilege
  configurations should provide explicit `revocation_statements` that drop
  the user with the right grants.
- **Hyphenated DisplayName**: the producer template uses `truncate 32` on
  `DisplayName` and then upper-cases the result. If two display names
  differ only by a character outside `[A-Z0-9_]` they may collide — choose
  a longer `(random N)` suffix in the template for high-churn workloads.

## Tests

`hana_test.go` runs two always-on unit tests (`Type`, username producer)
and a suite of acceptance tests gated on `BAO_ACC=1` + `HANA_URL=...`.
See [TEST.md](TEST.md) for the manual plan and CI setup notes.
