<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# DocumentDB Database Plugin

Issues dynamic credentials against the open-source DocumentDB engine
([documentdb/documentdb](https://github.com/documentdb/documentdb)) via
its MongoDB-compatible gateway. Available as
`documentdb-database-plugin` and `remote-documentdb-plugin`. See
[DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

DocumentDB is a PostgreSQL extension that exposes a MongoDB wire
protocol via a gateway process — the same engine that powers Azure
Cosmos DB for MongoDB vCore. This plugin is **not** for AWS
DocumentDB; the AWS-managed service has different defaults (retryable
writes forbidden, mandatory TLS against the AWS RDS CA).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/docdb \
    plugin_name=documentdb-database-plugin \
    connection_url='mongodb://{{username}}:{{password}}@docdb.example.com:10260/?tls=true' \
    username=documentdb password=secret \
    tls_ca_path=/etc/openbao/documentdb-ca.pem \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=docdb \
    creation_statements='{"db":"admin","roles":[{"role":"read","db":"app"}]}' \
    default_ttl=1h

$ bao read database/creds/reader
```

For the upstream docker quickstart (which uses a self-signed gateway
cert), use `insecure=true` instead of pointing `tls_ca_path` at a
bundle:

```bash
$ bao write database/config/docdb \
    plugin_name=documentdb-database-plugin \
    connection_url='mongodb://{{username}}:{{password}}@localhost:10260/?tls=true' \
    username=documentdb password='Documentdb_!' \
    insecure=true \
    allowed_roles=reader
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `documentdb-database-plugin` or `remote-documentdb-plugin` |
| `connection_url` | yes | Mongo URI; gateway port is typically `10260` |
| `username` / `password` | yes | Root credentials |
| `tls_ca` | no | PEM contents for a custom CA |
| `tls_ca_path` | no | Path to a custom CA bundle |
| `insecure` | no | Skip TLS verification (dev only; pairs with the docker quickstart's self-signed cert) |
| `connect_timeout` / `socket_timeout` / `server_selection_timeout` | no | Driver timeouts |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes requests |

## Creation statement

Same JSON shape as the MongoDB plugin:

```json
{"db":"admin","roles":[{"role":"readWrite","db":"app"}]}
```

The exact roles available depend on the documentdb version. See the
upstream docs for the supported RBAC catalog.

## Building

```
$ make documentdb-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
