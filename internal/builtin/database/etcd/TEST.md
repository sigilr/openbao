<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# etcd Plugin — Test Plan

## Always-on unit tests

```
$ go test ./internal/builtin/database/etcd/...
```

Covers Type/Version, JSON statement parsing, the `sanitizeEndpoints` /
`hostFromEndpoint` / `isUserNotFound` helpers, `Initialize` validation, and
`UpdateUser`/`DeleteUser` validation.

## Acceptance / manual

Gated on `BAO_ACC=1` + `ETCD_ENDPOINTS`. Run book follows.

### Local etcd via Docker

```
$ docker run --rm -d --name etcd -p 2379:2379 \
    quay.io/coreos/etcd:v3.5.17 \
    /usr/local/bin/etcd \
    --advertise-client-urls http://0.0.0.0:2379 \
    --listen-client-urls http://0.0.0.0:2379

$ etcdctl --endpoints=http://localhost:2379 user add root:root-bao
$ etcdctl --endpoints=http://localhost:2379 role add reader
$ etcdctl --endpoints=http://localhost:2379 role grant-permission reader read '' --prefix
$ etcdctl --endpoints=http://localhost:2379 user grant-role root root
$ etcdctl --endpoints=http://localhost:2379 auth enable
```

### End-to-end with `bao`

```bash
$ make etcd-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/etcd \
    plugin_name=etcd-database-plugin \
    endpoints=http://localhost:2379 \
    username=root password=root-bao \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=etcd \
    creation_statements='{"roles":["reader"]}' \
    default_ttl=1h

$ bao read database/creds/reader

# Verify:
$ etcdctl --endpoints=http://localhost:2379 \
    --user <USERNAME>:<PASSWORD> get foo

# Revoke:
$ bao lease revoke <LEASE_ID>
$ etcdctl --endpoints=http://localhost:2379 \
    --user root:root-bao user get <USERNAME>
# Error: user name not found

# Root rotation
$ bao write -force database/rotate-root/etcd
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Granting a non-existent role | `UserDelete` runs, error propagates |
| `UserDelete` on a missing user | Treated as success (idempotent) |
| Wrong `endpoints` | `Initialize` returns a clear connection error when `verify_connection=true` |
