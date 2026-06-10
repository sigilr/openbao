<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Weaviate Database Plugin — Design

## Scope

`weaviate-database-plugin` is a **static-credentials-only** OpenBao v5
database plugin for Weaviate self-hosted. Weaviate's API keys are loaded
from the `AUTHENTICATION_APIKEY_ALLOWED_KEYS` environment variable at
startup — there's no runtime key-management API — so the plugin's
surface is intentionally limited:

- `Initialize` parses config and (with `VerifyConnection=true`) calls
  `/v1/.well-known/ready` with the configured API key as a Bearer token.
- `NewUser` returns an explicit error pointing operators at static roles
  or external configuration management.
- `UpdateUser` is a no-op against the server but returns success on
  password updates so OpenBao static-role rotation works as a
  coordination mechanism.
- `DeleteUser` is a no-op.

Same pattern as the Memcached and Qdrant plugins in this repo.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `url` | yes | `http(s)://host:8080` |
| `api_key` | no | API key for VerifyConnection |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |

## Tests

Always-on tests cover Type/Version, the `NewUser` rejection, `UpdateUser`
validation + no-op, `DeleteUser`, healthcheck against `httptest.Server`,
and a 401 failure path.
