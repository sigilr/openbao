<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Neo4j Database Plugin

Issues dynamic credentials against Neo4j 4+ via the official Bolt-protocol
Go driver. Available as `neo4j-database-plugin` (built-in) and
`remote-neo4j-plugin` (proxied through a spoke).

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/neo4j \
    plugin_name=neo4j-database-plugin \
    uri=bolt://neo4j.example.com:7687 \
    username=neo4j password=password \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=neo4j \
    creation_statements='{"roles":["reader"]}' \
    default_ttl=1h
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `neo4j-database-plugin` or `remote-neo4j-plugin` |
| `uri` | yes | `bolt://`, `neo4j://`, `bolt+s://`, etc. |
| `username` / `password` | yes | Root credentials |
| `database` | no | Default `system` |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Creation statement

```json
{"roles": ["reader", "editor"]}
```

Roles must already exist on the cluster.

## Building

```
$ make neo4j-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
