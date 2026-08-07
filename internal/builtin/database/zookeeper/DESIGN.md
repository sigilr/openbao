<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache ZooKeeper Database Plugin — Design

## Scope

`zookeeper-database-plugin` is a **static-credentials-only** OpenBao v5
database plugin for Apache ZooKeeper.

ZooKeeper has no runtime user-management API for SASL/digest principals
— usernames and passwords are loaded from a server-side `jaas.conf` (or
`zoo.cfg` `digest:` entry) at startup. ACLs *can* be reconfigured at
runtime, but they're per-znode and tied to existing principals; they
don't add a dynamic-credential surface.

The plugin:

- Initialize verifies the configured ensemble node is reachable by
  opening a TCP connection and sending the ZooKeeper 4-letter `ruok`
  command. A healthy node replies with `imok`. If the cluster restricts
  4lw commands via `4lw.commands.whitelist`, the plugin surfaces the
  empty reply with an actionable hint.
- NewUser returns an explicit "not supported" error.
- UpdateUser is a no-op against the server; returns success on password
  updates so OpenBao static-role rotation tracks the new value.
- DeleteUser is a no-op.

Same pattern as the Memcached, Qdrant, Weaviate, Db2, and Hazelcast
plugins in this repo.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `address` | yes | `host:port` for one ensemble node |
| `username` / `password` | no | Carried for static-role tracking; not sent during the 4lw ping |

## Tests

Always-on tests cover Type/Version, the documented `NewUser` rejection,
`UpdateUser` validation + no-op, `DeleteUser`, the `ruok→imok`
healthcheck against a local TCP listener, and the `ruok-not-whitelisted`
failure path.
