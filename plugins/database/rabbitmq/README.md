<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# RabbitMQ Database Plugin

Issues dynamic RabbitMQ users (internal realm) with per-vhost permissions
via the management HTTP API. Counterpart to the existing `rabbitmq/`
secrets engine — same semantics, exposed through the unified database
engine.

Available as `rabbitmq-database-plugin` (built-in) and
`remote-rabbitmq-plugin` (proxied through a spoke).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/rmq \
    plugin_name=rabbitmq-database-plugin \
    connection_uri=http://rabbitmq.example.com:15672 \
    username=guest password=guest \
    allowed_roles='reader'

$ bao write database/roles/reader \
    db_name=rmq \
    creation_statements='{
      "tags":"management",
      "vhosts":{"/":{"configure":"","write":"","read":".*"}}
    }' \
    default_ttl=1h max_ttl=24h

$ bao read database/creds/reader
```

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `plugin_name` | yes | `rabbitmq-database-plugin` or `remote-rabbitmq-plugin` |
| `connection_uri` | yes | Management API URL (e.g. `http://host:15672`) |
| `username` / `password` | yes | API credentials |
| `tls_ca` | no | PEM CA bundle |
| `tls_certificate` / `tls_key` | no | mTLS PEM client identity |
| `insecure` | no | Skip TLS verify (dev only) |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that will execute requests |

## Creation statement

```json
{
  "tags": "administrator,management",
  "vhosts": {
    "/": {"configure":".*", "write":".*", "read":".*"}
  },
  "vhost_topics": {
    "/": {"amq.topic": {"write":".*", "read":".*"}}
  }
}
```

At least one of `tags` or `vhosts` is required.

## Remote variant

```bash
$ bao write database/config/spoke-rmq \
    plugin_name=remote-rabbitmq-plugin \
    spoke_name=spoke-1 \
    connection_uri=http://rabbitmq:15672 \
    username=guest password=guest \
    allowed_roles='*'
```

## Building

```
$ make rabbitmq-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
