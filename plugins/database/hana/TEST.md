<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# HanaDB Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/hana/...
```

Runs unconditionally on every CI build. Covers:

- `Type()` returns `hdb`
- `PluginVersion()` returns `ReportedVersion`
- The default username template renders into a HANA-legal identifier of
  the expected shape (`^V_DISPLAYNAME_ROLENAME_[A-Z0-9]{20}_[0-9]{10}$`)

These tests don't open a SQL connection, so they run without external
dependencies.

## Acceptance tests

Acceptance tests require a real SAP HANA instance and are gated on
`BAO_ACC=1` plus a `HANA_URL` of the form
`hdb://SYSTEM:<password>@host:port`. They will `t.Skip` otherwise.

```
$ export BAO_ACC=1
$ export HANA_URL='hdb://SYSTEM:Hxe-1234@hana.local:39041'
$ go test -v -count=1 -timeout=10m ./plugins/database/hana/
```

### Local HANA via Docker (HANA Express)

```
$ docker run --name hxe -p 39013:39013 -p 39017:39017 -p 39041-39045:39041-39045 \
    -p 1128-1129:1128-1129 -p 59013-59014:59013-59014 \
    -v hxe-data:/hana/mounts --ulimit nofile=1048576:1048576 \
    -e "AGREE_TO_SAP_LICENSE=yes" \
    --sysctl kernel.shmmni=4096 --sysctl kernel.shmmax=1073741824 \
    store/saplabs/hanaexpress:2.00.061.00.20220519.1 \
    --master-password Hxe-1234
```

Wait until `XS_SERVER (hxehost): Available` appears, then point `HANA_URL`
at `hdb://SYSTEM:Hxe-1234@localhost:39041`.

### What's covered

- `TestHANA_Initialize` — Init + VerifyConnection round-trip.
- `TestHANA_NewUser` — creation with and without statements; error path
  when no statements provided.
- `TestHANA_UpdateUser` — password change with default and custom
  statements; verifies the new password can log in.
- `TestHANA_DeleteUser` — default soft-drop revoke and custom drop
  statements; verifies revoked creds fail to log in.
- `TestHANA_CustomUsernameTemplate` — confirms a user-provided template
  flows through.

## Manual end-to-end with `bao`

The unit + acceptance tests exercise the plugin in isolation. The manual
plan below validates the integration in a running `bao` server.

### Built-in plugin

```bash
# 1. Build and start the dev server.
$ make hana-database-plugin
$ bao server -dev

# In another terminal:
$ export BAO_ADDR='http://127.0.0.1:8200'
$ bao login root

# 2. Mount the database engine.
$ bao secrets enable database

# 3. Configure the HANA connection.
$ bao write database/config/hana \
    plugin_name=hana-database-plugin \
    connection_url='hdb://{{username}}:{{password}}@localhost:39041' \
    allowed_roles='readonly' \
    username=SYSTEM password=Hxe-1234

# 4. Define a role.
$ bao write database/roles/readonly \
    db_name=hana \
    creation_statements='CREATE USER {{name}} PASSWORD "{{password}}" NO FORCE_FIRST_PASSWORD_CHANGE VALID UNTIL '"'"'{{expiration}}'"'"';' \
    default_ttl=1h max_ttl=24h

# 5. Read a credential.
$ bao read database/creds/readonly

# 6. Verify the issued user can log into HANA.
$ hdbsql -i 90 -d HXE -u "<USERNAME_FROM_STEP_5>" -p '<PASSWORD>' \
    "SELECT 1 FROM DUMMY"

# 7. Revoke the lease and verify the user is dropped.
$ bao lease revoke <LEASE_ID>
$ hdbsql -i 90 -d HXE -u SYSTEM -p Hxe-1234 \
    "SELECT USER_NAME FROM USERS WHERE USER_NAME = '<USERNAME_FROM_STEP_5>'"
# returns 0 rows.

# 8. Root rotation.
$ bao write -force database/rotate-root/hana
$ bao read database/creds/readonly  # still works -> rotation succeeded
```

### Remote plugin

Prerequisites: a hub `bao` running with `bao relay init` already
completed and a spoke `bao relay run` connected. Verify with:

```bash
$ bao relay list
```

Then:

```bash
$ bao write database/config/spoke-hana \
    plugin_name=remote-hana-plugin \
    spoke_name=spoke-1 \
    connection_url='hdb://{{username}}:{{password}}@hana:39041' \
    username=SYSTEM password=Hxe-1234 \
    allowed_roles='readonly'

$ bao write database/roles/readonly \
    db_name=spoke-hana \
    creation_statements='CREATE USER {{name}} PASSWORD "{{password}}" NO FORCE_FIRST_PASSWORD_CHANGE VALID UNTIL '"'"'{{expiration}}'"'"';' \
    default_ttl=1h

$ bao read database/creds/readonly
# A username/password pair appears identical to the built-in flow.
```

While the request is in flight, `bao relay list` on the hub shows the
spoke as healthy (last-seen < 45s).

### Namespace isolation

```bash
$ bao namespace create billing
$ bao secrets enable -namespace=billing database
$ bao write -namespace=billing database/config/hana ...
# Identical commands work with the namespace prefix; credentials are
# scoped to the billing namespace and do not appear in the root namespace
# credential list.
$ bao list -namespace=billing database/creds
$ bao list database/creds  # empty (or unrelated entries)
```

### Failure modes to spot-check

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `NewUser` returns `dbutil.ErrEmptyCreationStatement` |
| Username containing `'` | Default revoke quotes the identifier; statement runs cleanly |
| `RESTRICT` drop with owned objects | Drop fails; OpenBao surfaces HANA's error. Switch to `revocation_statements='DROP USER {{name}} CASCADE;'` |
| Spoke disconnected mid-request | Hub returns a `proxy: spoke not connected` style error; retry succeeds once spoke reconnects |
| Idle-evict | After the spoke's `runner.DefaultIdleTTL` (24h), the cached plugin instance is closed and re-Initialized transparently on the next request |
