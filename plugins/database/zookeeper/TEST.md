<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache ZooKeeper Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/zookeeper/...
```

Covers Type/Version, the documented `NewUser` rejection, `UpdateUser`
validation + no-op success, `DeleteUser` no-op, the `ruok→imok`
healthcheck against a local TCP listener, and the
`ruok-not-whitelisted` failure path.

## Acceptance / manual

Gated on `BAO_ACC=1` + `ZOOKEEPER_ADDRESS`.

### Local ZooKeeper via Docker

```
$ docker run --rm -d --name zk -p 2181:2181 \
    -e ZOO_4LW_COMMANDS_WHITELIST=ruok,stat \
    zookeeper:3.9
```

### End-to-end with `bao`

```bash
$ make zookeeper-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/zk \
    plugin_name=zookeeper-database-plugin \
    address=localhost:2181 \
    allowed_roles=app

# Dynamic credentials are not supported:
$ bao read database/creds/app
# error: dynamic credentials are not supported by ZooKeeper ...

# Static roles work as a credential-tracking mechanism:
$ bao write database/static-roles/app \
    db_name=zk username=app rotation_period=24h
$ bao read database/static-creds/app
# returns the tracked password; operator must apply it to the
# jaas.conf or zoo.cfg out of band.
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| `address` empty | "address is required (host:port)" |
| `ruok` not in whitelist | `ruok returned "" (expected "imok") — is 4lw.commands.whitelist=ruok set?` |
| Unreachable port | Init fails with the wrapped net error |
| `bao read database/creds/...` | error: dynamic credentials are not supported … |
