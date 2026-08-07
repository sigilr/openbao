<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache Ignite Database Plugin

Issues dynamic credentials against Apache Ignite via its REST API and
native SQL `CREATE USER` DDL. Available as `ignite-database-plugin`
(built-in) and `remote-ignite-plugin` (proxied through a spoke).

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

Requires Ignite 2.5+, persistence enabled, and
`authenticationEnabled=true` on the cluster's IgniteConfiguration.

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/ignite \
    plugin_name=ignite-database-plugin \
    url=http://ignite.example.com:8080 \
    username=ignite password=ignite \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=ignite \
    creation_statements='CREATE USER "{{name}}" WITH PASSWORD '"'"'{{password}}'"'"';' \
    default_ttl=1h
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `ignite-database-plugin` or `remote-ignite-plugin` |
| `url` | yes | Ignite REST base URL |
| `username` / `password` | yes | Root credentials |
| `cache_name` | no | Default `PUBLIC` |
| `ca_cert` / `ca_path` | no | Custom CA PEM (string or path) |
| `client_cert` / `client_key` | no | mTLS PEM client identity |
| `insecure` | no | Skip TLS verify (dev only) |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Identifier / password constraints

- Usernames are uppercased and underscore-separated by the producer.
  Custom templates that emit forbidden characters (`"`, `'`, `;`, `` ` ``)
  are rejected at runtime.
- Passwords containing single quotes are rejected up front — Ignite DDL
  has no way to escape them safely.

## Building

```
$ make ignite-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
