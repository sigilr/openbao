Implement Openbao database plugin for the following dbs, if one does not exist for openbao .Do this in checkout pr from remote-db-plugin branch in /Users/tamal/go/src/github.com/openbao/openbao. Use git worktree for eah db. Implement these plugins in batches of 2

- Add openbao namespace support. Read https://openbao.org/docs/concepts/namespaces/
- Use database plugin v5. Read https://openbao.org/docs/plugins/ , https://openbao.org/docs/plugins/plugin-architecture/ , https://openbao.org/docs/plugins/plugin-development/ , https://openbao.org/docs/secrets/databases/custom/ , https://openbao.org/docs/plugins/plugin-authors-guide/ . You can also find existing db plugins here: /Users/tamal/go/src/github.com/openbao/openbao/plugins/database

- In original hashicorp Vault, there was additional database plugins that were removed in Openbao. You can find them here /Users/tamal/go/src/github.com/openbao/vault/plugins/database . We can only use the last version before hashicorp changed license from MPL-2 to BUSL. These old plugins also probably not using openbao v5 plugin interface. So, update them as needed.

- Add remote-database-plugin support for this db as described in pr https://github.com/kubevault/openbao/pull/1 .Learn about it from https://github.com/kubevault/openbao/blob/remote-db-plugin/plugins/database/remote-db-plugin/README.md . Read the https://github.com/kubevault/openbao/blob/remote-db-plugin/plugins/database/remote-db-plugin/DESIGN.md to understand it better. The pr is https://github.com/kubevault/openbao/pull/1
- Implement the plugin in /Users/tamal/go/src/github.com/openbao/openbao/plugins/database/{db} folder.
- Add design doc in for the plugin in /Users/tamal/go/src/github.com/openbao/openbao/plugins/database/{db}/DESIGN.md
- Add user guide for the plugin in /Users/tamal/go/src/github.com/openbao/openbao/plugins/database/{db}/README.md
- Add test doc for the plugin in /Users/tamal/go/src/github.com/openbao/openbao/plugins/database/{db}/TEST.md
- update the build system to build/publish these plugins
- Add/update the openbao ui for this database plugin.
- Open prs for each db against https://github.com/sigilr/openbao

- DB2
- DocumentDB
- Druid
- Elasticsearch
- HanaDB
- Hazelcast
- Ignite
- Kafka
- Memcached
- Milvus
- MongoDB
- MSSQLServer
- Neo4j
- Oracle
- Qdrant
- RabbitMQ
- Solr
- Weaviate
- ZooKeeper

By DocumentDB , I am referring to https://github.com/documentdb/documentdb .

# Known limitations / honest notes:

- Kafka's ACL provisioning in creation_statements returns an explicit error and rolls back the credential creation; per-ACL translation needs careful per-resource-type work that
wasn't in scope. Operators provision ACLs out of band against the returned username.
- Static-only plugins (Memcached, Qdrant, Weaviate, DB2, Hazelcast, ZooKeeper) require configuration management / a sidecar to apply OpenBao's rotated value to the server.
- DB2 has no pure-Go driver; production deployments overwhelmingly delegate auth to OS/LDAP. The plugin documents this and stays static-only by design.
