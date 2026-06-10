<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache Ignite Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/ignite/...
```

Covers Type/Version, identifier validation, password validation, the
template renderer, the full request flow against `httptest.Server`, and
the REST `successStatus != 0` translation.

## Acceptance / manual

Gated on `BAO_ACC=1` + `IGNITE_URL`.

### Local Ignite via Docker

```
$ docker run --rm -d --name ignite \
    -e IGNITE_CONFIGURATION=/opt/ignite/config/ignite-auth.xml \
    -p 8080:8080 -p 10800:10800 apacheignite/ignite:2.16.0
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
    url=http://localhost:8080 \
    username=ignite password=ignite \
    allowed_roles=reader

$ bao write database/roles/reader \
    db_name=ignite \
    creation_statements='CREATE USER "{{name}}" WITH PASSWORD '"'"'{{password}}'"'"';' \
    default_ttl=1h

$ bao read database/creds/reader

# Verify via the REST API:
$ curl 'http://localhost:8080/ignite?cmd=qryfldexe&cacheName=PUBLIC&pageSize=1&qry=SELECT+1&ignite.login=<USERNAME>&ignite.password=<PASSWORD>'

# Revoke:
$ bao lease revoke <LEASE_ID>
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| Username with `"` or `'` | "identifier contains forbidden character" |
| Password with `'` | "password contains a single quote …" |
| Cluster not in active state | REST `successStatus != 0` propagated |
| Authentication not enabled on cluster | `CREATE USER` rejected by Ignite; surfaced via REST error envelope |
