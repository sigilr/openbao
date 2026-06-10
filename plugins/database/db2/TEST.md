<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# IBM Db2 Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/db2/...
```

Covers Type/Version, the documented `NewUser` rejection, `UpdateUser`
validation + no-op success, `DeleteUser` no-op, healthcheck against an
`httptest.Server`, and the empty-URL skip path.

## Acceptance / manual

Gated on `BAO_ACC=1` + `DB2_URL`.

Static credentials and root rotation work the same way as for the
Memcached, Qdrant, and Weaviate plugins in this repo. Refer to those
TEST.md files for the same shape of end-to-end run book, substituting:

- `plugin_name=db2-database-plugin`
- `url=` the Db2 REST endpoint (or omit to skip VerifyConnection)
- Rotation actually applied to Db2 happens out of band via the Db2 REST
  API or `db2` CLI.

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Wrong credentials w/ `verify_connection=true` | Init fails: `db2 host_status failed: 401 …` |
| Unreachable URL | Init fails with the wrapped net error |
| `url` empty + `verify_connection=true` | No-op success (healthcheck skipped) |
| `bao read database/creds/...` | error: dynamic credentials are not supported … |
