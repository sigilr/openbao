<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# IBM Db2 Database Plugin — Design

## Scope

`db2-database-plugin` is a **static-credentials-only** OpenBao v5
database plugin for IBM Db2. The design is constrained by two facts:

1. There is no pure-Go Db2 driver. The canonical client
   (`github.com/ibmdb/go_ibm_db`) requires CGO + the IBM CLI Driver on
   the build host, which conflicts with OpenBao's CGO_ENABLED=0 build
   target.
2. Native Db2 user management (`CREATE USER` / `ALTER USER` /
   `DROP USER`) needs the `AUTH_NATIVE` security plugin. Production Db2
   deployments overwhelmingly delegate to OS users or LDAP.

Implementing dynamic credentials would be misleading for the typical
deployment. Instead the plugin:

- Initialize parses config and (optionally) pings the Db2 REST API
  (`/dbapi/v4/host_status`) with Basic Auth to confirm reachability.
- NewUser returns an explicit "not supported" error.
- UpdateUser is a no-op against the server; returns success on password
  updates so OpenBao static-role rotation tracks the new value and
  emits audit events.
- DeleteUser is a no-op.

Operators who need dynamic Db2 credentials should pair this plugin
with a sidecar that uses the Db2 REST API (or the `db2` CLI) to apply
OpenBao's rotated value to the server's auth plugin.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `url` | no | Db2 REST endpoint (e.g. `http://db2-dbapi:50000`). When empty, VerifyConnection is a no-op |
| `username` / `password` | no | REST credentials (Basic Auth for healthcheck) |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |

## Tests

Always-on tests cover Type/Version, the documented `NewUser` rejection,
`UpdateUser` validation + no-op, `DeleteUser`, healthcheck against
`httptest.Server`, and the empty-URL skip path.
