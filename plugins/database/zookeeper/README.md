<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Apache ZooKeeper Database Plugin

Static-credentials-only OpenBao plugin for Apache ZooKeeper. See
[DESIGN.md](DESIGN.md) for the constraint (no runtime user-management
API for SASL/digest principals).

## Quick start

```bash
$ bao secrets enable database

$ bao write database/config/zk \
    plugin_name=zookeeper-database-plugin \
    address=zookeeper.example.com:2181 \
    allowed_roles=app

$ bao write database/static-roles/app \
    db_name=zk \
    username=app \
    rotation_period=24h
```

## Limitations

- Dynamic credentials are not supported.
- `UpdateUser` and `DeleteUser` are no-ops against the server.
- Configuration management or a sidecar must apply rotated credentials
  to the ZooKeeper ensemble (`jaas.conf` reload, rolling restart).

## Building

```
$ make zookeeper-database-plugin
```

## License

Copyright &copy; AppsCode Inc.

Licensed under the [Mozilla Public License, v. 2.0](https://www.mozilla.org/en-US/MPL/2.0/).
