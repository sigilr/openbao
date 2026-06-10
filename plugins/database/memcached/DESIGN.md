<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Memcached Database Plugin — Design

## Scope

`memcached-database-plugin` is a **static-credentials-only** OpenBao v5
database plugin for Memcached. Memcached has **no runtime
user-management API**: SASL credentials live in an auth file loaded at
startup. This plugin therefore intentionally implements a limited surface:

- `Initialize` parses config and optionally pings the server (TCP and
  optional TLS handshake) to confirm reachability.
- `NewUser` returns an explicit error pointing operators at static roles
  or external configuration management.
- `UpdateUser` is a no-op against the server — but returns success on
  password updates so OpenBao static-role rotation flows still work as a
  coordination mechanism: OpenBao tracks the new credential and emits
  audit events; an out-of-band system updates the SASL auth file.
- `DeleteUser` is a no-op.

Operators who want dynamic credentials for Memcached should pair this
plugin with a sidecar that watches OpenBao lease events and rewrites
the SASL auth file. That's out of scope for this plugin itself.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `address` | yes | `host:port` |
| `username` / `password` | no | Used for VerifyConnection auth (not currently exercised by the ping path) |
| `use_tls` / `tls_ca` / `tls_ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing for the ping |

## Tests

Always-on tests cover:

- `Type` / `PluginVersion`
- `NewUser` returns the documented "not supported" error
- `UpdateUser` validation (missing username; no changes) and the no-op
  password path
- `DeleteUser` no-op
- `Healthcheck_Connect` against a local TCP listener
- `Healthcheck_BadAddr` against an unreachable port
