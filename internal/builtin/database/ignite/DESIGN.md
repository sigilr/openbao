<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache Ignite Database Plugin — Design

## Scope

`ignite-database-plugin` implements the OpenBao v5 database plugin
against Apache Ignite using the REST API. Dynamic credentials become
native SQL users via `CREATE USER` / `ALTER USER` / `DROP USER` DDL,
which Ignite 2.5+ supports when persistence is enabled and
`authenticationEnabled=true` is set on the cluster.

Built-in and remote variants are both registered.

## Why REST instead of a Go driver?

Apache Ignite has no official Go driver. The community
`amsokol/ignite-go-client` is unmaintained. The REST API ships with the
core distribution and supports running SQL via `cmd=qryfldexe`, which
covers everything we need.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `url` | yes | Ignite REST base URL (e.g. `http://ignite:8080`) |
| `username` / `password` | yes | Root credentials (sent as `ignite.login` / `ignite.password` query params) |
| `cache_name` | no | Default `PUBLIC` |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Identifier and password safety

Ignite DDL doesn't take parameters, so the plugin builds SQL strings
directly. Before doing so it validates both sides:

- **`safeIdentifier`** rejects any identifier containing `"`, `'`, `;`,
  or `` ` ``.
- **`safePassword`** rejects passwords containing a single quote (which
  would terminate the string literal).

The username producer additionally uppercases and underscore-converts the
generated name, so the result is always inside the safe character set.

## Creation statement

Templated SQL — same conventions as the SQL plugins:

```sql
CREATE USER "{{name}}" WITH PASSWORD '{{password}}';
```

Operators can supply multiple statements separated by `;`. Each is run
sequentially through the REST API. Per-statement failures are surfaced
verbatim.

## Lifecycle

- **NewUser** — render and execute each creation statement.
- **UpdateUser** — `ALTER USER "<name>" WITH PASSWORD '<pw>'`.
- **DeleteUser** — `DROP USER "<name>"`, or custom revocation statements.
- **Expiration** — no-op (Ignite has no `VALID UNTIL`).

## REST envelope

Ignite responses look like `{"successStatus": N, "error": "..."}` even
when the HTTP status is 200. The plugin checks `successStatus != 0` and
returns the embedded error. Verified by `TestIgnite_RestError`.

## Tests

Always-on tests cover Type/Version, identifier validation, password
validation, the template renderer, the full request flow against an
`httptest.Server`, and the `successStatus != 0` error path.

Acceptance tests are gated on `BAO_ACC=1` + `IGNITE_URL`.
