<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Weaviate Database Plugin

OpenBao database plugin for Weaviate. For Weaviate instances running with
database-backed users and RBAC enabled (`AUTHENTICATION_DB_USERS_ENABLED=true` and
`AUTHORIZATION_ENABLE_RBAC=true`), this plugin creates dynamic users, assigns
custom roles, rotates keys, and deletes users upon lease expiry.

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/weaviate \
    plugin_name=weaviate-database-plugin \
    url=https://weaviate.example.com:8080 \
    api_key=admin-key \
    allowed_roles=app

$ bao write database/roles/app \
    db_name=weaviate \
    creation_statements='{"roles": ["viewer"]}' \
    default_ttl=1h \
    max_ttl=24h

$ bao read database/creds/app
```

## Building

```
$ make weaviate-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
