<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Milvus Database Plugin — Design

## Scope

`milvus-database-plugin` implements the OpenBao v5 database plugin against
Milvus 2.x using the official Milvus Go SDK (`github.com/milvus-io/milvus-sdk-go/v2/client`)
over gRPC. Dynamic credentials become native Milvus users; `creation_statements`
is a JSON role doc listing pre-existing roles to grant.

Built-in and remote variants are both registered.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `url` | yes | Milvus gRPC server address (e.g. `milvus:19530`) |
| `username` / `password` | one of | Root credentials |
| `token` | one of | API key / token (e.g. Zilliz Cloud) |
| `db_name` | no | Milvus database name |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Creation statement

```json
{"roles": ["public"]}
```

Roles must already exist on the cluster.

## Lifecycle

### NewUser

- Calls `client.CreateCredential(ctx, username, password)`
- For each role in `creation_statements`, calls `client.AddUserRole(ctx, username, role)`
- If `AddUserRole` fails, the plugin calls `client.DeleteCredential(ctx, username)` to clean up partially created user.

### UpdateUser

- Calls `client.UpdateCredential(ctx, username, "", newPassword)`

### DeleteUser

- Calls `client.DeleteCredential(ctx, username)`

## Tests

Always-on unit tests run against an in-memory gRPC server implementing `milvuspb.MilvusServiceServer` (`fakeMilvusServer`).
Tests cover:
- Type and version reporting
- Statement JSON parsing
- Full credential lifecycle (`CreateCredential`, `AddUserRole`, `UpdateCredential`, `DeleteCredential`)
- Role grant failure and automatic cleanup of partially created credentials
- Error handling

Acceptance tests are gated on `BAO_ACC=1` + `MILVUS_URL`.
