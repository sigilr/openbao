<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Memcached Database Plugin

Static-credentials-only OpenBao plugin for Memcached. Memcached has no
runtime user-management API; SASL credentials must be provisioned in the
auth file out of band. This plugin lets OpenBao be the source of truth
for the credential value and its rotation schedule, while the actual
push to the server is left to configuration management.

See [DESIGN.md](DESIGN.md) for the full rationale.

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/memcached \
    plugin_name=memcached-database-plugin \
    address=memcached.example.com:11211 \
    allowed_roles=app

$ bao write database/static-roles/app \
    db_name=memcached \
    username=app \
    rotation_period=24h
```

`bao read database/static-creds/app` returns the current password.
Update your SASL auth file out of band (e.g., from your sidecar or
configuration tooling) to keep Memcached in sync.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `memcached-database-plugin` or `remote-memcached-plugin` |
| `address` | yes | `host:port` |
| `use_tls` / `tls_ca` / `tls_ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS for the ping |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Limitations

- `NewUser` (dynamic credentials) is **not supported** and returns an
  explicit error.
- `UpdateUser` and `DeleteUser` are no-ops against the server.
- The plugin doesn't push changes to Memcached.

## Building

```
$ make memcached-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
