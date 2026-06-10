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
- `acls` is **not yet implemented**. If the array is non-empty the plugin
  returns an error and deletes the just-created credential — Kafka ACL
  translation needs careful per-resource handling and we don't want to
  ship a half-implementation that silently grants the wrong access.
  Operators who need ACLs should provision them out of band via
  `kafka-acls.sh` against the username this plugin returns.

## Lifecycle

### NewUser

1. Generate a username.
2. Parse the statement; default mechanism / iterations.
3. `AlterUserSCRAMs(ctx, nil, []UpsertSCRAM{...})` to create the credential.
4. If ACLs are requested → return an error and delete the credential.
5. Otherwise return the username.

### UpdateUser

Upsert the same SCRAM record with the new password (default iterations
4096). The mechanism is taken from the config-level `mechanism` field;
operators wanting per-credential variation should rotate via a custom
mechanism stored in OpenBao.

### DeleteUser

`AlterUserSCRAMs(ctx, []DeleteSCRAM{{User, ScramSha256}, {User, ScramSha512}}, nil)`
to drop both mechanisms in one call. Missing records are not an error.

## Failure modes

| Scenario | Behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Non-JSON `creation_statements` | "creation_statements must be a JSON role doc" |
| Unknown mechanism | "unsupported mechanism" |
| `acls` non-empty | Credential created then deleted; error returned |
| AdminClient connection broken | Surfaced as Init / NewUser error |

## Tests

Always-on tests cover Type/Version, JSON statement parsing, mechanism
mapping helpers, and `UpdateUser` validation. The `httptest`-style test
for the full flow is intentionally skipped because the franz-go
AdminClient can't be easily mocked; the integration is exercised via the
manual run book.

Acceptance tests are gated on `BAO_ACC=1` + `KAFKA_BROKERS`.
