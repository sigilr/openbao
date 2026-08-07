<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Hazelcast Database Plugin — Design

## Scope

`hazelcast-database-plugin` is a **static-credentials-only** OpenBao v5
database plugin for Hazelcast IMDG / Platform.

Hazelcast OSS has no runtime user-management API — authentication is
configured in the member XML (`<security>`) at startup. Hazelcast
Enterprise's `Permissions` API can be reconfigured at runtime via
ClusterPermissionsConfig, but the plugin keeps a uniform surface across
editions and treats both as static.

The plugin:

- Initialize parses config and (with VerifyConnection=true) pings the
  REST endpoint (`/hazelcast/health/ready`) with Basic Auth.
- NewUser returns an explicit "not supported" error.
- UpdateUser is a no-op against the server; returns success on password
  updates so OpenBao static-role rotation tracks the new value.
- DeleteUser is a no-op.

Same pattern as the Memcached, Qdrant, Weaviate, and Db2 plugins in
this repo.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `url` | yes | `http(s)://host:5701` |
| `username` / `password` | no | REST credentials (Basic Auth for healthcheck) |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |

## Tests

Always-on tests cover Type/Version, the documented `NewUser` rejection,
`UpdateUser` validation + no-op, `DeleteUser`, and healthcheck against
`httptest.Server`.
