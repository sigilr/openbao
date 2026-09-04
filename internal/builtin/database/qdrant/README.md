<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Qdrant Database Plugin

OpenBao database plugin for Qdrant. It generates dynamic credentials using
Qdrant's Granular Access API Keys (HS256-signed JWTs) and provides stateful token
revocation through Qdrant's `value_exists` validation mechanism.

See [DESIGN.md](DESIGN.md) and [TEST.md](TEST.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/qdrant \
    plugin_name=qdrant-database-plugin \
    url=https://qdrant.example.com:6333 \
    api_key=topsecret \
    allowed_roles=app

$ bao write database/roles/app \
    db_name=qdrant \
    creation_statements='{"access": [{"collection": "test_collection", "access": "rw"}]}' \
    default_ttl=1h \
    max_ttl=24h

$ bao read database/creds/app
```

## Building

```
$ make qdrant-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
