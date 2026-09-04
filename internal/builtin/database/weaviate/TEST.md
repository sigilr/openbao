<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Weaviate Plugin — Test Plan

## Always-on unit tests

```
$ go test ./internal/builtin/database/weaviate/...
```

Covers Type/Version, `NewUser` dynamic user creation + role assignment,
`UpdateUser` key rotation, `DeleteUser`, and `Healthcheck` against an
`httptest.Server` (200 and 401).

## Acceptance / manual

Gated on `BAO_ACC=1` + `WEAVIATE_URL`.

### Local Weaviate via Docker

```bash
$ docker run --rm -d --name weaviate -p 8080:8080 \
    -e AUTHENTICATION_APIKEY_ENABLED=true \
    -e AUTHENTICATION_APIKEY_ALLOWED_KEYS=admin-key \
    -e AUTHENTICATION_DB_USERS_ENABLED=true \
    -e AUTHORIZATION_ENABLE_RBAC=true \
    -e AUTHORIZATION_RBAC_ROOT_USERS=admin-user \
    cr.weaviate.io/semitechnologies/weaviate:1.30.0
```

### End-to-end with `bao`

```bash
$ make weaviate-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/weaviate \
    plugin_name=weaviate-database-plugin \
    url=http://localhost:8080 \
    api_key=admin-key \
    allowed_roles=app

# Dynamic credentials via Weaviate RBAC API:
$ bao write database/roles/app \
    db_name=weaviate \
    creation_statements='{"roles": ["viewer"]}' \
    default_ttl=1h \
    max_ttl=24h

$ bao read database/creds/app
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Wrong `api_key` with `verify_connection=true` | Init fails: `weaviate /ready failed: 401 …` |
| Unreachable URL | Init fails with the wrapped net error |
