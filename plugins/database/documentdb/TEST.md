<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# DocumentDB Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/documentdb/...
```

Covers Type/Version, username template, JSON statement parsing, and
the TLS-PEM-rejection error path. The real wire-level interactions are
identical to the MongoDB plugin (same role docs, same wire), so deep
coverage of `createUser` / `updateUser` / `dropUser` lives there.

## Acceptance / manual

Gated on `BAO_ACC=1` + `DOCDB_URL`.

### Local DocumentDB via Docker (upstream quickstart)

The upstream project ships a docker image at
`ghcr.io/microsoft/documentdb/documentdb-local`. The README shows:

```
$ docker pull ghcr.io/microsoft/documentdb/documentdb-local:latest
$ docker run --rm -d --name documentdb \
    -p 10260:10260 \
    -e USERNAME=documentdb \
    -e PASSWORD='Documentdb_!' \
    ghcr.io/microsoft/documentdb/documentdb-local:latest
```

The gateway listens on `10260` and presents a self-signed TLS cert.
Pair `insecure=true` with `?tls=true` in `connection_url` to reach it.

### End-to-end with `bao`

```bash
$ make documentdb-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/docdb \
    plugin_name=documentdb-database-plugin \
    connection_url='mongodb://{{username}}:{{password}}@localhost:10260/?tls=true' \
    username=documentdb password='Documentdb_!' \
    insecure=true \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=docdb \
    creation_statements='{"db":"admin","roles":[{"role":"read","db":"app"}]}' \
    default_ttl=1h

$ bao read database/creds/reader

# Verify (must accept the self-signed cert):
$ mongosh "mongodb://<USERNAME>:<PASSWORD>@localhost:10260/?tls=true" \
    --tlsAllowInvalidCertificates --eval 'db.runCommand({ping:1})'

# Revoke
$ bao lease revoke <LEASE_ID>

# Root rotation
$ bao write -force database/rotate-root/docdb
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| `connection_url` without `tls=true` against a TLS-enabled gateway | Gateway rejects the connection during Initialize |
| Missing or wrong CA bundle | TLS verify fails during Initialize |
| `UserNotFound` on revoke | Logged WARN, treated as success |
| Two revocation statements | "expected 0 or 1 revocation statements, got 2" |
| Older gateway that doesn't accept `retryWrites=true` | Set `?retryWrites=false` in `connection_url`; the plugin no longer forces this |
