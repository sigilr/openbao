<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Milvus Database Plugin

Issues dynamic credentials against Milvus 2.x via its HTTP RESTful API
v2. Available as `milvus-database-plugin` (built-in) and
`remote-milvus-plugin` (proxied through a spoke).

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/milvus \
    plugin_name=milvus-database-plugin \
    url=http://milvus.example.com:19530 \
    username=root password=Milvus123 \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=milvus \
    creation_statements='{"roles":["public"]}' \
    default_ttl=1h
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `milvus-database-plugin` or `remote-milvus-plugin` |
| `url` | yes | Milvus HTTP URL |
| `username` / `password` | one of | Root credentials |
| `token` | one of | Bearer token (Zilliz Cloud style) |
| `db_name` | no | Default `dbName` header per request |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Building

```
$ make milvus-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
