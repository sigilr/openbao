<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Milvus Plugin — Test Plan

## Always-on unit tests

```
$ go test ./internal/builtin/database/milvus/...
```

Covers Type/Version, JSON statement parsing, full credential lifecycle
against an in-memory gRPC server implementing `milvuspb.MilvusServiceServer`,
role-grant failure cleanup, and error handling.

## Acceptance / manual

Gated on `BAO_ACC=1` + `MILVUS_URL`.

### Local Milvus via Docker

```
$ docker run --rm -d --name milvus -p 19530:19530 \
    -e ETCD_USE_EMBED=true -e ETCD_DATA_DIR=/var/lib/milvus/etcd \
    -e ETCD_CONFIG_PATH=/milvus/configs/embedEtcd.yaml \
    -e COMMON_STORAGETYPE=local \
    milvusdb/milvus:v2.4.1 milvus run standalone
```

Wait for `Milvus Proxy successfully started` in the logs.

### End-to-end with `bao`

```bash
$ make milvus-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/milvus \
    plugin_name=milvus-database-plugin \
    url=localhost:19530 \
    username=root password=Milvus \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=milvus \
    creation_statements='{"roles":["public"]}' \
    default_ttl=1h

$ bao read database/creds/reader

# Revoke:
$ bao lease revoke <LEASE_ID>
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Non-JSON `creation_statements` | "creation_statements must be a JSON role doc" |
| Username > 32 chars | Server rejects; tune `username_template` |
| Grant role fails after user create | Plugin drops the user before returning |
| Wrong URL or auth | Connection verification (`GetVersion`) fails |
