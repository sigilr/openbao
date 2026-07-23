<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Hazelcast Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/hazelcast/...
```

Covers Type/Version, the documented `NewUser` rejection, `UpdateUser`
validation + no-op success, `DeleteUser` no-op, and `Healthcheck`
against an `httptest.Server`.

## Acceptance / manual

Gated on `BAO_ACC=1` + `HAZELCAST_URL`.

Static credentials and root rotation work the same way as for the
Memcached, Qdrant, Weaviate, and Db2 plugins in this repo. Refer to
those TEST.md files for the same shape of end-to-end run book,
substituting:

- `plugin_name=hazelcast-database-plugin`
- `url=http://hazelcast:5701` (or wherever the REST endpoint lives)
- Rotation actually applied to Hazelcast happens out of band via
  member XML reconfiguration + rolling restart, or (Enterprise)
  the ClusterPermissionsConfig API.

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Wrong credentials w/ `verify_connection=true` | Init fails: `hazelcast /health/ready failed: 401 …` |
| Unreachable URL | Init fails with the wrapped net error |
| `bao read database/creds/...` | error: dynamic credentials are not supported … |
