<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache Ignite Plugin — Test Plan

## Always-on unit tests

```
$ go test ./internal/builtin/database/ignite/...
```

Covers Type/Version, identifier validation, password validation, the
template renderer, and host/port normalization (including
backwards-compatible `url` parsing).

## Acceptance / manual

Gated on `BAO_ACC=1` + `IGNITE_ADDR` (host:port of the thin client
listener), plus optional `IGNITE_CA_CERT` / `IGNITE_INSECURE`.

```
$ IGNITE_ADDR=localhost:10800 \
  IGNITE_USER=ignite IGNITE_PASSWORD=ignite \
  BAO_ACC=1 go test ./internal/builtin/database/ignite/ -run TestIgnite_Acceptance -v
```

### Local Ignite via Docker

```
$ docker run --rm -d --name ignite \
    -e IGNITE_CONFIGURATION=/opt/ignite/config/ignite-auth.xml \
    -p 10800:10800 apacheignite/ignite:2.16.0
```

Operators must turn on `authenticationEnabled=true` and enable
persistence in the cluster config; refer to the Ignite docs for the
minimal XML.

### End-to-end with `bao`

```bash
$ make ignite-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/ignite \
    plugin_name=ignite-database-plugin \
    url=tcp://localhost:10800 \
    username=ignite password=ignite \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=ignite \
    creation_statements='CREATE USER "{{name}}" WITH PASSWORD '"'"'{{password}}'"'"';' \
    default_ttl=1h

$ bao read database/creds/reader

# Verify via sqlline (thin client):
$ docker exec -it ignite /opt/ignite/apache-ignite/bin/sqlline.sh \
    -u "jdbc:ignite:thin://localhost:10800" --show-time

> SELECT * FROM SYS.USERS;

# Revoke:
$ bao lease revoke <LEASE_ID>
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Username with `"` or `'` | "identifier contains forbidden character" |
| Password with `'` | "password contains a single quote …" |
| Cluster not in active state | Ignite error propagated from the thin client response |
| Authentication not enabled on cluster | `CREATE USER` rejected by Ignite; surfaced verbatim |
| TLS mismatch | handshake failure surfaced with host:port context |
