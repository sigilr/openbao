<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# IBM Db2 Database Plugin

Static-credentials-only OpenBao plugin for IBM Db2. See [DESIGN.md](DESIGN.md)
for the constraints (no pure-Go driver + most prod deployments delegate
auth to OS or LDAP).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/db2 \
    plugin_name=db2-database-plugin \
    url=http://db2-dbapi.example.com:50000 \
    username=admin password=admin \
    allowed_roles=app

$ bao write database/static-roles/app \
    db_name=db2 \
    username=APP \
    rotation_period=24h
```

## Limitations

- Dynamic credentials are not supported.
- `UpdateUser` and `DeleteUser` are no-ops against the server.
- Configuration management or a sidecar must apply rotated credentials
  to the Db2 auth plugin.

## Building

```
$ make db2-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
