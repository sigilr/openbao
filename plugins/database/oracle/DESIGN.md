<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Oracle Database Plugin — Design

## Scope

`oracle-database-plugin` implements the OpenBao database secrets-engine v5
contract against **Oracle Database**, including 19c, 21c, and 23c. Uses
the pure-Go driver `github.com/sijms/go-ora/v2`, so the binary does not
need the Oracle Instant Client and builds with `CGO_ENABLED=0`. Supports
dynamic credentials, static-role password rotation, and root credential
rotation. Also exposed remotely as `remote-oracle-plugin` via the
[remote-db-plugin](../remote-db-plugin/DESIGN.md) hub-and-spoke runner.

## Provenance and license

Greenfield implementation by the OpenBao project, modelled on the existing
`postgresql-database-plugin` shape. HashiCorp's Oracle plugin is an
enterprise component and is not the source for this one. Licensed under
MPL-2.0.

## Architecture

```
                +------------------+
                |  OpenBao Core    |
                |  (database mount)|
                +---------+--------+
                          | v5 gRPC
                          v
                +------------------+
                |  oracle-         |
                |  database-plugin |
                +---------+--------+
                          | go-ora / Oracle TNS
                          v
                +------------------+
                |   Oracle DB      |
                +------------------+
```

`Oracle` embeds `connutil.SQLConnectionProducer` for shared
connection_url plumbing, max-conns, root rotation, and per-namespace
mount lifecycle.

## Connection

- `connection_url`: `oracle://{{username}}:{{password}}@host:port/service`
  or any URL accepted by `go-ora`. Common shapes: `oracle://SYSTEM:PW@host:1521/XEPDB1`.
- `username` / `password`: bootstrap root credentials.
- `username_template`: override the default template (default emits a
  30-character upper-case identifier of the form `V_<DISPLAY>_<ROLE>_<RANDOM>_<UNIX>`).

## Identifier rules

Oracle identifiers are case-folded to upper-case when unquoted. The default
username producer uppercases and strips hyphens so the rendered name is
safe to use unquoted. Custom templates that return mixed case still get
this normalization applied post-generation.

The default revoke path quotes identifiers via `dbutil.QuoteIdentifier`
before interpolating into `DROP USER %s CASCADE` — a username containing
`"` would be rejected by Oracle rather than allowing SQL injection.

## Lifecycle

### NewUser

1. Generate a username (upper, underscore-separated, ≤ 30 chars).
2. Run each `creation_statements` via `dbtxn.ExecuteDBQueryDirect` against
   the connection. Oracle does not let DDL share a transaction with DML
   meaningfully, so the runs are not wrapped; each statement is its own
   auto-commit unit.
3. On first failure: return the username + the multierror so the caller
   (and the lease revoke) can clean up the partially-created user.

If `creation_statements` is empty, returns `dbutil.ErrEmptyCreationStatement`.

### UpdateUser

- Password change only. Default statement:
  `ALTER USER {{username}} IDENTIFIED BY "{{password}}";`
- Expiration is intentionally a no-op; Oracle has no native `VALID UNTIL`
  on users (`PROFILE`-based password lifetimes are managed out of band).

### DeleteUser (default revoke)

1. Query `v$session` for active sessions on the username; emit
   `ALTER SYSTEM KILL SESSION '<sid>,<serial>'` for each. Failures here
   are swallowed (e.g. when the configured root lacks `SELECT` on
   `v$session`) and we fall through to the DROP.
2. `DROP USER <quoted> CASCADE` — drops the schema and any owned objects.

### DeleteUser (custom)

`revocation_statements` run sequentially with `{{name}}`/`{{username}}`
bound to the request's username. Per-statement failures aggregate into a
multierror; the call returns all of them at the end.

## Namespace support

Per-namespace database mounts work without plugin-side changes. See
[OpenBao namespaces](https://openbao.org/docs/concepts/namespaces/).

## Remote variant

`remote-oracle-plugin` is a one-line registration in
`helper/builtinplugins/registry.go` plus a `case` in
`plugins/database/remote-db-plugin/runner/runner.go:loadPlugin`. See
[remote-db-plugin/DESIGN.md](../remote-db-plugin/DESIGN.md) for the
hub/spoke wire protocol and trust bootstrap.

## Failure modes

| Scenario | Behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Root lacks `SELECT` on `v$session` | KILL SESSION loop quietly skipped, DROP USER attempted |
| User has an active session and root cannot kill it | DROP USER fails with `ORA-01940`; the lease revoke will retry |
| Username with `"` | Rejected by Oracle at the DDL layer (identifier quote is `"`) |
| `UPDATE` with neither password nor expiration | `no changes requested` |
| `connection_url` missing | `connutil.Init` reports the error during Initialize |

## Tests

`oracle_test.go` runs always-on unit tests:

- `Type` / `PluginVersion`
- Username template produces an upper-case identifier ≤ 30 chars
- `UpdateUser` validation (missing username, no changes)

Acceptance tests are gated on `BAO_ACC=1` + `ORACLE_URL`. See
[TEST.md](TEST.md) for the manual plan.
