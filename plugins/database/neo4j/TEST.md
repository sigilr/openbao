<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Neo4j Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/neo4j/...
```

Covers Type/Version, JSON statement parsing, `containsBacktick` helper,
and `UpdateUser` validation.

## Acceptance / manual

Gated on `BAO_ACC=1` + `NEO4J_URI`. Run book follows.

### Local Neo4j via Docker

```
$ docker run --rm -d --name neo4j -p 7474:7474 -p 7687:7687 \
    -e NEO4J_AUTH=neo4j/neo4j-bao \
    neo4j:5
```

### End-to-end with `bao`

```bash
$ make neo4j-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/neo4j \
    plugin_name=neo4j-database-plugin \
    uri=bolt://localhost:7687 \
    username=neo4j password=neo4j-bao \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=neo4j \
    creation_statements='{"roles":["reader"]}' \
    default_ttl=1h

$ bao read database/creds/reader

# Verify:
$ cypher-shell -a bolt://localhost:7687 \
    -u <USERNAME> -p <PASSWORD> 'SHOW CURRENT USER'

# Revoke:
$ bao lease revoke <LEASE_ID>
$ cypher-shell -a bolt://localhost:7687 \
    -u neo4j -p neo4j-bao 'SHOW USERS WHERE name = "<USERNAME>"'
# 0 rows

# Root rotation
$ bao write -force database/rotate-root/neo4j
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Role name with backtick | "role name … contains a backtick" |
| Granting a non-existent role | DROP USER runs, error propagates |
| DROP USER on a missing user | `IF EXISTS` makes it a no-op |
| Wrong URI scheme (e.g. http://) | Driver returns a clear error during Initialize |
