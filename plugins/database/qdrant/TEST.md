<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Qdrant Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/qdrant/...
```

Covers Type/Version, the documented `NewUser` rejection, `UpdateUser`
validation + no-op success, `DeleteUser` no-op, and `Healthcheck`
against an `httptest.Server` (200 and 401).

## Acceptance / manual

Gated on `BAO_ACC=1` + `QDRANT_URL`.

### Local Qdrant via Docker

```
$ docker run --rm -d --name qdrant -p 6333:6333 \
    -e QDRANT__SERVICE__API_KEY=topsecret \
    qdrant/qdrant:v1.13.0
```

### End-to-end with `bao`

```bash
$ make qdrant-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/qdrant \
    plugin_name=qdrant-database-plugin \
    url=http://localhost:6333 \
    api_key=topsecret \
    allowed_roles=app

# Dynamic credentials are not supported — this should fail with the
# documented error:
$ bao write database/roles/app db_name=qdrant creation_statements=''
$ bao read database/creds/app
# error: dynamic credentials are not supported by Qdrant ...

# Static roles work as a credential-tracking mechanism:
$ bao write database/static-roles/app \
    db_name=qdrant username=app rotation_period=24h
$ bao read database/static-creds/app
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Wrong `api_key` with `verify_connection=true` | Init fails: `qdrant /readyz failed: 401 …` |
| Unreachable URL | Init fails with the wrapped net error |
| `bao read database/creds/...` | error: dynamic credentials are not supported … |
