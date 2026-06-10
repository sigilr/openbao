<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache Druid Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/druid/...
```

Covers Type/Version, JSON statement parsing, default authenticator/authorizer
fallback, and the full Initialize → NewUser → UpdateUser → DeleteUser
flow against `httptest.Server` (validates the BasicSecurity URL paths).

## Acceptance / manual

Gated on `BAO_ACC=1` + `DRUID_URL`.

### Local Druid via Docker

```
$ docker run --rm -d --name druid -p 8081:8081 \
    -e DRUID_BASIC_AUTH_BOOTSTRAP_USER=admin \
    apache/druid:30.0.0
```

(Real Druid security configuration is non-trivial; refer to Apache Druid
docs for the auth/authz extension configuration in `common.runtime.properties`.)

### End-to-end with `bao`

```bash
$ make druid-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/druid \
    plugin_name=druid-database-plugin \
    url=http://localhost:8081 \
    username=admin password=admin \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=druid \
    creation_statements='{"roles":["datasourceReadAccess"]}' \
    default_ttl=1h

$ bao read database/creds/reader

# Verify with the Druid Router (assumes the same Basic Auth chain):
$ curl -u '<USERNAME>:<PASSWORD>' http://localhost:8888/status

# Revoke:
$ bao lease revoke <LEASE_ID>
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Non-JSON `creation_statements` | "creation_statements must be a JSON role doc" |
| `assign role` returns 4xx/5xx | Plugin deletes the half-configured user |
| Authenticator/authorizer name wrong | Coordinator returns a clear 4xx body |
| Delete on missing user | 404 is swallowed (idempotent) |
