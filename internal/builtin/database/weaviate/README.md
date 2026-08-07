<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Weaviate Database Plugin

Static-credentials-only OpenBao plugin for Weaviate self-hosted.
Weaviate has no runtime key-management API; API keys are loaded from
`AUTHENTICATION_APIKEY_ALLOWED_KEYS` at startup. This plugin lets
OpenBao be the source of truth for the API key and its rotation
schedule, while the actual push to the server is left to configuration
management.

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/weaviate \
    plugin_name=weaviate-database-plugin \
    url=https://weaviate.example.com:8080 \
    api_key=topsecret \
    allowed_roles=app

$ bao write database/static-roles/app \
    db_name=weaviate \
    username=app \
    rotation_period=24h
```

## Limitations

- Dynamic credentials are not supported.
- `UpdateUser` and `DeleteUser` are no-ops against the server.
- Configuration management must apply rotated keys to the server.

## Building

```
$ make weaviate-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
