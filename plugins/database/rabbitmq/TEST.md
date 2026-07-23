<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# RabbitMQ Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/rabbitmq/...
```

Covers `Type`, `PluginVersion`, `parseTags`, JSON statement parsing,
`UpdateUser` validation, and the empty-statements rejection on `NewUser`.

## Acceptance / manual

Gated on `BAO_ACC=1` + `RABBITMQ_URL`. The manual run book follows.

### Local RabbitMQ via Docker (management plugin enabled)

```
$ docker run --rm -d --name rmq -p 5672:5672 -p 15672:15672 \
    rabbitmq:3.13-management
```

### End-to-end with `bao`

```bash
$ make rabbitmq-database-plugin
$ bao server -dev

$ bao secrets enable database

$ bao write database/config/rmq \
    plugin_name=rabbitmq-database-plugin \
    connection_uri=http://localhost:15672 \
    username=guest password=guest \
    allowed_roles='reader'

$ bao write database/roles/reader \
    db_name=rmq \
    creation_statements='{
      "tags":"management",
      "vhosts":{"/":{"configure":"","write":"","read":".*"}}
    }' \
    default_ttl=1h

$ bao read database/creds/reader

# Verify the user can log in to management UI / API:
$ curl -u '<USERNAME>:<PASSWORD>' http://localhost:15672/api/whoami

# Revoke:
$ bao lease revoke <LEASE_ID>
$ curl -u '<USERNAME>:<PASSWORD>' http://localhost:15672/api/whoami
# 401

# Root rotation
$ bao write -force database/rotate-root/rmq
```

### Failure modes to spot-check

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Statement missing both `tags` and `vhosts` | "creation_statements requires at least one of: vhosts, tags" |
| Permission set fails after `PutUser` | Plugin deletes the user before returning the error |
| Delete on already-revoked user | 404 → success (idempotent) |
| Bad TLS chain | Init fails with TLS verify error |
