<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Milvus Database Plugin — Design

## Scope

`milvus-database-plugin` implements the OpenBao v5 database plugin against
Milvus 2.x using its HTTP RESTful API v2 (`/v2/vectordb/users/...` and
`/v2/vectordb/users/grant_role`). Dynamic credentials become native
Milvus users; `creation_statements` is a JSON role doc listing
pre-existing roles to grant.

Built-in and remote variants are both registered.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `url` | yes | Milvus HTTP URL (e.g. `http://milvus:19530`) |
| `username` / `password` | one of | Root credentials |
| `token` | one of | Zilliz Cloud-style API token (sent as `Authorization: Bearer`) |
| `db_name` | no | Sent as `dbName` request header |
| `ca_cert` / `ca_path` / `client_cert` / `client_key` / `insecure` | no | TLS plumbing |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Creation statement

```json
{"roles": ["admin"]}
```

Roles must already exist on the cluster.

## Lifecycle

### NewUser

```http
POST /v2/vectordb/users/create     {"userName":"<name>","password":"<pw>"}
POST /v2/vectordb/users/grant_role {"userName":"<name>","roleName":"<role>"}
```

If grant_role fails, the plugin sends `users/drop` to clean up.

### UpdateUser

```http
POST /v2/vectordb/users/update_password {"userName":"<name>","newPassword":"<pw>"}
```

### DeleteUser

```http
POST /v2/vectordb/users/drop {"userName":"<name>"}
```

## API-level errors

Milvus returns HTTP 200 with an envelope `{"code": N, "message": "..."}`
even for failures. The plugin parses the envelope and treats `code != 0`
as an error so we never silently succeed.

## Tests

Always-on: Type/Version, JSON parsing, full request-flow against
`httptest.Server`, and a dedicated test for the API-error envelope
(`code:1`) translating into a returned error.

Acceptance tests are gated on `BAO_ACC=1` + `MILVUS_URL`.
