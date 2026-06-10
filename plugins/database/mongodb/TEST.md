<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# MongoDB Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/mongodb/...
```

Runs on every CI build. Covers, without touching the network:

- `Type()` returns `mongodb`.
- `PluginVersion()` returns `ReportedVersion`.
- The default username template renders into the expected shape.
- `mongoDBStatement` JSON parsing (bare role / db-qualified role).
- `mongodbRoles.toStandardRolesArray` flattening.
- `getWriteConcern` for raw JSON, base64-of-JSON, empty, and garbage.
- `loadConfig` rejects empty `connection_url` and negative timeouts.

## Acceptance tests

Gated on `BAO_ACC=1` plus `MONGODB_URL=mongodb://user:pass@host:port`.

```
$ export BAO_ACC=1
$ export MONGODB_URL='mongodb://root:secret@localhost:27017/admin'
$ go test -v -count=1 -timeout=10m ./plugins/database/mongodb/
```

### Local MongoDB via Docker

```
$ docker run --rm -d --name mongo -p 27017:27017 \
    -e MONGO_INITDB_ROOT_USERNAME=root \
    -e MONGO_INITDB_ROOT_PASSWORD=secret \
    mongo:7
```

Then point `MONGODB_URL=mongodb://root:secret@localhost:27017/admin`.

### Coverage

- `TestMongoDB_Initialize` — Init + VerifyConnection round-trip.
- `TestMongoDB_NewUser` — createUser with the canonical role document.
- `TestMongoDB_UpdateUser_Password` — password rotation.
- `TestMongoDB_DeleteUser` — drop + idempotent second drop.

## Manual end-to-end with `bao`

### Built-in plugin

```bash
$ make mongodb-database-plugin
$ bao server -dev

# In another terminal:
$ export BAO_ADDR='http://127.0.0.1:8200'
$ bao login root

$ bao secrets enable database

$ bao write database/config/mongo \
    plugin_name=mongodb-database-plugin \
    connection_url='mongodb://{{username}}:{{password}}@localhost:27017/admin' \
    allowed_roles='readonly' \
    username=root password=secret

$ bao write database/roles/readonly \
    db_name=mongo \
    creation_statements='{"db":"admin","roles":[{"role":"read","db":"app"}]}' \
    default_ttl=1h max_ttl=24h

$ bao read database/creds/readonly

# Verify the issued user works:
$ mongosh "mongodb://<USERNAME>:<PASSWORD>@localhost:27017/admin?authSource=admin" \
    --eval 'db.runCommand({ping: 1})'

# Revoke and verify the user is dropped:
$ bao lease revoke <LEASE_ID>
$ mongosh "mongodb://root:secret@localhost:27017/admin" \
    --eval 'db.getSiblingDB("admin").getUsers()'  # <USERNAME> should be gone

# Root rotation
$ bao write -force database/rotate-root/mongo
$ bao read database/creds/readonly  # still works → rotation succeeded
```

### Remote plugin

Prerequisites: a hub `bao` running with `bao relay init` completed and a
spoke `bao relay run` connected (see `bao relay list`).

```bash
$ bao write database/config/spoke-mongo \
    plugin_name=remote-mongodb-plugin \
    spoke_name=spoke-1 \
    connection_url='mongodb://{{username}}:{{password}}@mongo:27017/admin' \
    username=root password=secret \
    allowed_roles='readonly'

$ bao write database/roles/readonly \
    db_name=spoke-mongo \
    creation_statements='{"db":"admin","roles":[{"role":"read","db":"app"}]}' \
    default_ttl=1h

$ bao read database/creds/readonly
```

### Namespace isolation

```bash
$ bao namespace create billing
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/mongo ...
# Identical commands with -namespace=billing; credentials don't leak into root.
```

### Failure modes to spot-check

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| `roles: []` in statement | "roles array is required in creation statement" |
| Two revocation statements | "expected 0 or 1 revocation statements, got 2" |
| User already deleted, lease revoke | Logged as WARN, response OK |
| Mongo cluster restart mid-request | First attempt EOFs, transparent retry succeeds |
| TLS misconfig (bad CA PEM) | Init fails with `failed to append CA to client options` |
| `write_concern` is invalid JSON | Init fails with `error unmarshalling write_concern` |
| `write_concern` is base64 of valid JSON | Init succeeds |
