<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# MongoDB Database Plugin

OpenBao plugin that issues dynamic database credentials against **MongoDB**.
Implements the v5 database plugin contract; exposed both as the in-process
built-in `mongodb-database-plugin` and, via the
[remote-db-plugin](../remote-db-plugin/README.md), as `remote-mongodb-plugin`.

See [DESIGN.md](DESIGN.md) for the architecture and
[TEST.md](TEST.md) for the manual test plan.

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/my-mongo \
    plugin_name=mongodb-database-plugin \
    connection_url='mongodb://{{username}}:{{password}}@mongo.example.com:27017/admin' \
    username='root' \
    password='secret' \
    allowed_roles='readonly,readwrite'

$ bao write database/roles/readonly \
    db_name=my-mongo \
    creation_statements='{"db":"admin","roles":[{"role":"read","db":"app"}]}' \
    default_ttl=1h \
    max_ttl=24h

$ bao read database/creds/readonly
Key                Value
---                -----
lease_id           database/creds/readonly/abc123…
lease_duration     1h
password           Bao-Mongo-1234567890
username           v-token-readonly-3pfw6t8rvmc8mbkz8zef-1717000000
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `mongodb-database-plugin` or `remote-mongodb-plugin` |
| `connection_url` | yes | Mongo URI; supports `{{username}}` / `{{password}}` placeholders |
| `username` / `password` | yes | Root credentials |
| `allowed_roles` | yes | Comma-separated list, or `*` |
| `write_concern` | no | JSON or base64-of-JSON; defaults to `{"wmode":"majority"}` |
| `tls_ca` | no | PEM bundle |
| `tls_certificate_key` | no | PEM cert+key (enables `MONGODB-X509`) |
| `socket_timeout` | no | Default `1m` |
| `connect_timeout` | no | Default `1m` |
| `server_selection_timeout` | no | Default driver behavior |
| `username_template` | no | Override the dynamic-username template |
| `spoke_name` | yes (remote) | Name of the spoke that will execute the requests |

## Creation statements

A single JSON document:

```json
{
  "db": "admin",
  "roles": [
    { "role": "read", "db": "reports" },
    { "role": "readWrite", "db": "billing" }
  ]
}
```

- `db` (optional, default `"admin"`) is the authentication database.
- `roles` (required) is an array of MongoDB role documents.

Bare role names (no `db`) are flattened to strings; db-qualified ones stay
as objects, matching `createUser`'s expectation.

## Static roles

```bash
$ bao write database/static-roles/svc \
    db_name=my-mongo \
    username=svc \
    rotation_period=24h
```

Password rotation is handled in `UpdateUser`. MongoDB doesn't have native
`VALID UNTIL`, so dynamic-lease expiry relies on OpenBao + `DeleteUser`.

## Root rotation

```bash
$ bao write -force database/rotate-root/my-mongo
```

The configured root password is replaced with a freshly generated one. The
old password is no longer recoverable.

## Write concern

`write_concern` accepts JSON like `{"wmode":"majority","wtimeout":1000,"j":true}`.
For CI systems that can't pass literal braces, base64-encode the same value;
the plugin tries base64 first and falls back to raw JSON.

## TLS

Pass `tls_ca` (PEM bundle) and optionally `tls_certificate_key` (PEM cert
+ key concatenated). When `tls_certificate_key` is set, the plugin
authenticates with `MONGODB-X509` using the cert's subject.

## Namespaces

Per-namespace mounts work without plugin-side changes:

```bash
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/my-mongo ...
```

See [OpenBao namespaces](https://openbao.org/docs/concepts/namespaces/).

## Remote variant

```bash
$ bao write database/config/spoke-mongo \
    plugin_name=remote-mongodb-plugin \
    spoke_name=spoke-1 \
    connection_url='mongodb://{{username}}:{{password}}@mongo:27017/admin' \
    username=root password=secret \
    allowed_roles='*'
```

See [../remote-db-plugin/README.md](../remote-db-plugin/README.md) for the
trust bootstrap and operations.

## Building

```
$ make mongodb-database-plugin
```

Output: `bin/mongodb-database-plugin`. The same code is also linked into
the `bao` binary as a built-in.

## License

Copyright &copy; HashiCorp, Inc. and AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
