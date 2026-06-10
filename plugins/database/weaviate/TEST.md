<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Weaviate Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/weaviate/...
```

Covers Type/Version, the documented `NewUser` rejection, `UpdateUser`
validation + no-op success, `DeleteUser` no-op, and `Healthcheck`
against an `httptest.Server` (200 with Bearer auth, and 401).

## Acceptance / manual

Gated on `BAO_ACC=1` + `WEAVIATE_URL`.

### Local Weaviate via Docker

```
$ docker run --rm -d --name weaviate -p 8080:8080 \
    -e AUTHENTICATION_APIKEY_ENABLED=true \
    -e AUTHENTICATION_APIKEY_ALLOWED_KEYS=topsecret \
    -e AUTHENTICATION_APIKEY_USERS=admin \
    -e AUTHORIZATION_ADMINLIST_ENABLED=true \
    -e AUTHORIZATION_ADMINLIST_USERS=admin \
    semitechnologies/weaviate:1.27.0
```

### End-to-end with `bao`

```bash
$ make weaviate-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/weaviate \
    plugin_name=weaviate-database-plugin \
    url=http://localhost:8080 \
    api_key=topsecret \
    allowed_roles=app

# Dynamic credentials are not supported:
$ bao read database/creds/app
# error: dynamic credentials are not supported by Weaviate self-hosted ...

# Static roles work as a credential-tracking mechanism:
$ bao write database/static-roles/app \
    db_name=weaviate username=app rotation_period=24h
$ bao read database/static-creds/app
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Wrong `api_key` with `verify_connection=true` | Init fails: `weaviate /v1/.well-known/ready failed: 401 …` |
| Unreachable URL | Init fails with the wrapped net error |
| `bao read database/creds/...` | error: dynamic credentials are not supported … |
