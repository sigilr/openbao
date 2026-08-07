<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Hazelcast Database Plugin

Static-credentials-only OpenBao plugin for Hazelcast IMDG / Platform.
Hazelcast OSS has no runtime user-management API; auth is configured in
member XML at startup. See [DESIGN.md](DESIGN.md).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/hazelcast \
    plugin_name=hazelcast-database-plugin \
    url=http://hazelcast.example.com:5701 \
    username=admin password=admin \
    allowed_roles=app

$ bao write database/static-roles/app \
    db_name=hazelcast \
    username=app \
    rotation_period=24h
```

## Limitations

- Dynamic credentials are not supported.
- `UpdateUser` and `DeleteUser` are no-ops against the server.
- Configuration management or a sidecar must apply rotated credentials
  to the Hazelcast cluster.

## Building

```
$ make hazelcast-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
