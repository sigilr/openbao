<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Qdrant Database Plugin — Design

## Scope

`qdrant-database-plugin` is a **static-credentials-only** OpenBao v5
database plugin for Qdrant. Qdrant's API key is loaded from the
`QDRANT__SERVICE__API_KEY` environment variable at startup — there's no
runtime user/key-management API — so the plugin's surface is intentionally
limited:

- `Initialize` parses config and (with `VerifyConnection=true`) calls
  the `/readyz` endpoint with the configured API key as the `api-key`
  header.
- `NewUser` returns an explicit error pointing operators at static roles
  or external configuration management.
- `UpdateUser` is a no-op against the server but returns success on
  password updates so OpenBao static-role rotation tracks the new
  credential and emits audit events.
- `DeleteUser` is a no-op.

The same pattern as the Memcached plugin in this repo; see that plugin
for additional context.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `url` | yes | `http(s)://host:port` |
| `api_key` | no | API key for VerifyConnection |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |

## Tests

Always-on tests cover Type/Version, the `NewUser` rejection, `UpdateUser`
validation + no-op, `DeleteUser`, healthcheck against `httptest.Server`,
and a failure path with 401 from the upstream.
