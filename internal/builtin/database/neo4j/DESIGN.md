<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Neo4j Database Plugin — Design

## Scope

`neo4j-database-plugin` implements the OpenBao v5 database plugin
contract against Neo4j 4+ (and 5/6) using the official
`neo4j-go-driver/v5`. Dynamic credentials become native users created via
Cypher's `CREATE USER` syntax against the `system` database; permissions
come from pre-existing roles named in `creation_statements`.

Also exposed remotely as `remote-neo4j-plugin`.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `uri` | yes | `bolt://`, `neo4j://`, `bolt+s://`, etc. |
| `username` / `password` | yes | Root credentials |
| `database` | no | Database to run user-management against; defaults to `system` (Neo4j 4+ convention) |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Creation statement

JSON:

```json
{"roles": ["reader", "editor"]}
```

Roles must already exist on the cluster. Bare strings — no DB scoping,
because Neo4j's role model is global.

## Lifecycle

### NewUser

```cypher
CREATE USER $name SET PASSWORD $password CHANGE NOT REQUIRED;
GRANT ROLE `<role>` TO $name;  -- once per role in the statement
```

`name` and `password` are parameterized through the driver. Role names
can't be parameterized in Cypher, so they're back-tick quoted; role
names that contain back-ticks are rejected up front.

On any per-role grant failure, the plugin runs `DROP USER $name` before
returning so we don't leak half-configured users.

### UpdateUser

```cypher
ALTER USER $name SET PASSWORD $password CHANGE NOT REQUIRED;
```

Expiration is a no-op (Neo4j has no native VALID UNTIL on users).

### DeleteUser

```cypher
DROP USER $name IF EXISTS;
```

`IF EXISTS` makes the revoke idempotent.

## Namespace support

Per-namespace mounts work without plugin-side changes.

## Failure modes

| Scenario | Behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Role name contains back-tick | Rejected: "role name %q contains a backtick" |
| Role does not exist | GRANT fails; plugin DROPs the user and returns the error |
| `uri` missing | "uri is required" |
| Wrong scheme for TLS | Driver returns a clear connection error |

## Tests

Always-on:

- `Type` / `PluginVersion`
- Statement parsing
- `containsBacktick` helper
- `UpdateUser` validation

Acceptance tests are gated on `BAO_ACC=1` + `NEO4J_URI`.
