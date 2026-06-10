<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Memcached Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/memcached/...
```

Covers:

- Type/Version
- `NewUser` returns the documented "not supported" error
- `UpdateUser` validation + no-op success path
- `DeleteUser` no-op
- Healthcheck against a local TCP listener
- Healthcheck failure against an unreachable port

## Acceptance / manual

Gated on `BAO_ACC=1` + `MEMCACHED_URL`.

### Local Memcached via Docker

```
$ docker run --rm -d --name memcached -p 11211:11211 memcached:1.6
```

### End-to-end with `bao`

```bash
$ make memcached-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/memcached \
    plugin_name=memcached-database-plugin \
    address=localhost:11211 \
    allowed_roles=app

# Dynamic credentials are not supported — this should fail with the
# documented error:
$ bao write database/roles/app db_name=memcached creation_statements=''
$ bao read database/creds/app
# error: dynamic credentials are not supported by Memcached ...

# Static roles work as a credential-tracking mechanism:
$ bao write database/static-roles/app \
    db_name=memcached \
    username=app \
    rotation_period=24h

$ bao read database/static-creds/app
# returns the current tracked password; operator must apply it to the
# SASL auth file out of band.

# Root rotation
$ bao write -force database/rotate-root/memcached
# OpenBao stores the new password; operator must rewrite the auth file.
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| `bao read database/creds/...` | error: dynamic credentials are not supported … |
| `bao read database/static-creds/...` | returns the tracked password (does not contact the server) |
| Unreachable `address` | `Initialize` with `VerifyConnection=true` fails |
| Bad TLS config | `Initialize` fails with TLS handshake error |
