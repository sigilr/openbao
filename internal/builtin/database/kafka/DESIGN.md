<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Kafka Database Plugin — Design

## Scope

`kafka-database-plugin` implements the OpenBao v5 database plugin against
Apache Kafka using the AdminClient API (franz-go's `kadm` package).
Dynamic credentials are SCRAM-SHA-256 (default) or SCRAM-SHA-512 user
records, written and deleted through `AlterUserSCRAMs`. Available
built-in and via the remote-db-plugin runner.

## Why franz-go?

`github.com/twmb/franz-go` is the most actively maintained pure-Go Kafka
client and supports both the SCRAM AdminClient flow and TLS dial config.
`kadm` packs the AlterUserSCRAMs RPC behind a typed helper.

## Configuration

| Field | Required | Description |
| --- | --- | --- |
| `brokers` | yes | Bootstrap brokers (comma list or string slice) |
| `username` / `password` | yes | Root credentials |
| `mechanism` | no | `SCRAM-SHA-256` (default) or `SCRAM-SHA-512` |
| `use_tls` | no | Enable TLS dial |
| `tls_ca` / `tls_ca_path` | no | CA PEM (string or file) |
| `tls_certificate` / `tls_key` | no | mTLS client identity |
| `insecure` | no | Skip TLS verify (dev only) |
| `username_template` | no | Override default template |
| `spoke_name` | yes (remote) | Spoke that executes the requests |

## Creation statement

```json
{
  "mechanism":  "SCRAM-SHA-256",
  "iterations": 4096,
  "acls":       []
}
```

- `mechanism` and `iterations` default to `SCRAM-SHA-256` / 4096 when omitted.
- `acls` is an array of ACL objects (`resource_type`, `resource_name`, `pattern_type`, `operation`, `permission`). When specified, ACLs are provisioned for the generated user upon creation and dropped upon revocation.

## Lifecycle

### NewUser

1. Generate a username.
2. Parse the statement; default mechanism / iterations.
3. `AlterUserSCRAMs(ctx, nil, []UpsertSCRAM{...})` to create the credential.
4. If ACLs are requested → create ACLs using `kadm.CreateACLs`. If ACL creation fails, roll back by deleting ACLs and the SCRAM credential.
5. Return the username.

### UpdateUser

Upsert the same SCRAM record with the new password (default iterations
4096). The mechanism is taken from the config-level `mechanism` field;
operators wanting per-credential variation should rotate via a custom
mechanism stored in OpenBao.

### DeleteUser

1. `DeleteACLs(ctx, delACL)` to remove all ACLs associated with `User:<username>`.
2. `AlterUserSCRAMs` to delete SCRAM credentials for the user.

## Failure modes

| Scenario | Behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Non-JSON `creation_statements` | "creation_statements must be a JSON role doc" |
| Unknown mechanism | "unsupported mechanism" |
| Invalid ACL / ACL creation failure | Credential rolled back and error returned |
| AdminClient connection broken | Surfaced as Init / NewUser error |

## Tests

Always-on tests cover Type/Version, JSON statement parsing, mechanism
mapping helpers, and `UpdateUser` validation. The `httptest`-style test
for the full flow is intentionally skipped because the franz-go
AdminClient can't be easily mocked; the integration is exercised via the
manual run book.

Acceptance tests are gated on `BAO_ACC=1` + `KAFKA_BROKERS`.
