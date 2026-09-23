<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# etcd Database Plugin

Issues dynamic credentials against an etcd cluster's built-in v3 Auth API via
the official Go client. Available as `etcd-database-plugin` (built-in) and
`remote-etcd-plugin` (proxied through a spoke).

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/etcd \
    plugin_name=etcd-database-plugin \
    endpoints=https://etcd.example.com:2379 \
    username=root password=password \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=etcd \
    creation_statements='{"roles":["reader"]}' \
    default_ttl=1h
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `etcd-database-plugin` or `remote-etcd-plugin` |
| `endpoints` | yes | Comma-separated etcd client URLs |
| `username` / `password` | yes if auth is enabled | Root credentials |
| `dial_timeout` | no | Default `5s` |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Creation statement

Accepts existing roles, custom inline roles, or a combination:

### Pre-existing roles
```json
{"roles": ["reader", "editor"]}
```

### Custom roles with inline permissions
```json
{
  "roles": ["reader"],
  "custom_roles": [
    {
      "name": "app_writer",
      "permissions": [
        {
          "permission": "readwrite",
          "key": "/app/",
          "prefix": true
        },
        {
          "permission": "read",
          "key": "/config/sample"
        }
      ]
    }
  ]
}
```

- `permission`: `"read"`, `"write"`, or `"readwrite"` (case-insensitive).
- `key`: key path or range start.
- `prefix`: boolean; if `true`, automatically computes prefix range end.
- `range_end`: optional explicit range end key.
- Custom roles are created idempotently (`RoleAdd` ignores "already exists", `RoleGrantPermission` updates permissions).
- Ephemeral user revocation deletes the user and preserves custom roles.

## Building

```
$ make etcd-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
