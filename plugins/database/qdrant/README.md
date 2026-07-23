<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Qdrant Database Plugin

Static-credentials-only OpenBao plugin for Qdrant. Qdrant has no runtime
key-management API; the API key is loaded from the
`QDRANT__SERVICE__API_KEY` environment variable at startup. This plugin
lets OpenBao be the source of truth for the key and its rotation
schedule, while the actual push to the server is left to configuration
management.

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/qdrant \
    plugin_name=qdrant-database-plugin \
    url=https://qdrant.example.com:6333 \
    api_key=topsecret \
    allowed_roles=app

$ bao write database/static-roles/app \
    db_name=qdrant \
    username=app \
    rotation_period=24h
```

## Limitations

- Dynamic credentials are not supported.
- `UpdateUser` and `DeleteUser` are no-ops against the server.
- Configuration management must apply rotated keys to the server.

## Building

```
$ make qdrant-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
