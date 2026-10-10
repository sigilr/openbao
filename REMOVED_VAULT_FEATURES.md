# Vault OSS Features Removed in OpenBao

OpenBao forked from HashiCorp Vault 1.14.1 (the last MPL 2.0 release) and
trimmed a fair amount of Vault OSS's surface area at and after the fork
point. This is tracked here for reference when deciding what (if anything)
to restore in this fork, following the same pattern already used for the
[storage backend restorations](internal/builtin/database/remote-db-plugin/DESIGN.md).

Verified directly against `internal/helper/builtinplugins/registry.go`,
`CHANGELOG.md`, and `website/content/community/deprecation/index.mdx` in
`openbao/openbao`, not just secondhand claims.

## Auth methods no longer built in

Present in Vault OSS 1.14, absent from OpenBao's `credentialBackends`
registry:

- **AWS**, **Azure**, **GCP**, **Alicloud**, **OCI**, **GitHub**, **Okta**

Centrify and Cloud Foundry auth were also on the original removal
proposal, but those had already been dropped from Vault itself before the
1.14 fork point, so they're not really "removed by OpenBao."

Kept: `approle`, `cert`, `jwt`/`oidc`, `kubernetes`, `userpass`, and —
still shipping today but **officially deprecated, slated for removal in
v2.7.0** — `kerberos`, `ldap`, `radius`.

## Secrets engines no longer built in

- **AWS**, **Azure**, **GCP**, **GCP KMS**, **Alicloud**, **Active
  Directory (ad)**, **Consul**, **Nomad**, **MongoDB Atlas**,
  **Terraform Cloud**

Kept: `kv`, `pki`, `ssh`, `totp`, `transit`, `rabbitmq`, `kubernetes`,
`ldap`/`openldap`, `database`.

## Database secrets plugins no longer built in

Vault OSS ships MongoDB, MongoDB Atlas, MSSQL, Elasticsearch, HanaDB,
Couchbase, Redshift, and Snowflake connectors; OpenBao only ships MySQL
(+ Aurora/RDS/legacy variants), PostgreSQL, Cassandra, InfluxDB, and Redis
(reimplemented against Valkey rather than actually removed).

## Storage backends

The 17+ backends restored in this fork this session — MySQL, Cassandra,
etcd, ZooKeeper, CouchDB, MSSQL, DynamoDB, S3, CockroachDB, Azure, GCS,
Spanner, FoundationDB, Aerospike, Swift, OCI, Alicloud OSS — plus Consul
(deliberately skipped) and Manta, which nobody's restored yet.

## Seal / auto-unseal and HSM — in progress, not gone yet

As of the current release (2.6.1), these are **deprecated with removal
targeted for v2.7.0**, not yet actually removed:

- Vendor-specific auto-unseal (`awskms`, `azurekeyvault`, `gcpckms`,
  `alicloudkms`, `ocikms`) — moving to external KMS-provider plugins.
- The separate HSM distribution / built-in PKCS#11 — same story, replaced
  by a PKCS#11 KMS plugin.
- Built-in `kerberos`/`ldap`/`radius` plugins mentioned above are on the
  same v2.7.0 removal train, moving to the `openbao-plugins` org.

## Other core/CLI/UI features already removed

- **mlock support** — dropped entirely in 2.0 (`disable_mlock` is now a
  no-op).
- **Legacy unauthenticated lease endpoints** — `sys/revoke`,
  `sys/renew`, `sys/revoke-prefix`, `sys/revoke-force` removed outright
  for a cross-namespace lease CVE (GHSA-v8v8-cm84-m686).
- **`stored_shares`** init/rekey parameter — removed, now ignored.
- **`jsonx` audit log format** — removed, use `json`.
- **Undocumented `aead` seal mechanism** — removed.
- **`FeatureFlags`/license SDK plumbing** — removed (this was Vault's
  enterprise-licensing hook, not really an OSS-user-facing feature).
- **UI client-count menu** — removed.
- **Illumos and Solaris platform builds** — dropped (broken builds / no
  Docker support).
- **`vault transform` CLI subcommands** — removed, though Transform
  itself was Enterprise-only in Vault OSS to begin with, so this doesn't
  cost OSS users anything real.
- **API-driven audit device creation** — disabled by default (not
  removed, but off unless you set `unsafe_allow_api_audit_creation`);
  declarative, config-file-based audit devices are the replacement.
- **PostgreSQL < 9.5 support** — dropped in the storage backend.

## Rationale

The overarching rationale the maintainers gave (in the GitHub discussion
where this was originally proposed) was preferring OSI-licensed
integrations over proprietary cloud SDKs and cutting binary size/
maintenance burden, with the expectation that anything cut would
resurface as an external plugin via `openbao-plugins` — which is exactly
the pattern this fork's storage-backend restorations, and now the
LDAP/Kerberos/RADIUS/KMS moves, are following.

## Sources

- <https://github.com/orgs/openbao/discussions/64>
- `CHANGELOG.md` and `website/content/community/deprecation/index.mdx`
  in `openbao/openbao`
- `internal/helper/builtinplugins/registry.go` in `openbao/openbao`
