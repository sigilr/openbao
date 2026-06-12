We want to implement Kubernetes operator support for these new database plugins. You have to make changes in 3 places:

api: /Users/tamal/go/src/kubevault.dev/apimachinery checkout from agent branch
operator: /Users/tamal/go/src/kubevault.dev/operator checkout from agent branch
docs: /Users/tamal/go/src/kubevault.dev/docs checkout from master branch

We also support the hub-spoke model for the database plugins. read Design doc here: /Users/tamal/go/src/kubevault.dev/operator/design/ocm-agent-placement-design.md

As an example for Postgres:

api:
- /Users/tamal/go/src/kubevault.dev/apimachinery/apis/engine/v1alpha1/postgres_types.go
- /Users/tamal/go/src/kubevault.dev/apimachinery/apis/engine/v1alpha1/postgres_helpers.go

operator:
- /Users/tamal/go/src/kubevault.dev/operator/pkg/controller/postgres_role.go
- /Users/tamal/go/src/kubevault.dev/operator/pkg/controller/postgres_role_test.go

docs:
/Users/tamal/go/src/kubevault.dev/kubevault/docs/guides/secret-engines/postgres

Implement operator support for the following database plugin for the following dbs, if one does not exist in apis. Use git worktree for eah db. Implement these plugins in batches of 2

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
