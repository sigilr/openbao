<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Kafka Plugin — Test Plan

## Always-on unit tests

```
$ go test ./plugins/database/kafka/...
```

Covers Type/Version, JSON statement parsing, mechanism mapping, and
UpdateUser validation. The full request flow is exercised manually below
(the franz-go AdminClient is awkward to mock cleanly).

## Acceptance / manual

Gated on `BAO_ACC=1` + `KAFKA_BROKERS`.

### Local Kafka via Docker (KRaft mode with SCRAM)

```
$ docker run --rm -d --name kafka -p 9092:9092 \
    -e KAFKA_NODE_ID=1 \
    -e KAFKA_PROCESS_ROLES=broker,controller \
    -e KAFKA_LISTENERS=SASL_PLAINTEXT://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093 \
    -e KAFKA_ADVERTISED_LISTENERS=SASL_PLAINTEXT://localhost:9092 \
    -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
    -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
    -e KAFKA_INTER_BROKER_LISTENER_NAME=SASL_PLAINTEXT \
    -e KAFKA_SASL_ENABLED_MECHANISMS=SCRAM-SHA-256 \
    -e KAFKA_SASL_MECHANISM_INTER_BROKER_PROTOCOL=SCRAM-SHA-256 \
    -e KAFKA_LISTENER_NAME_SASL_PLAINTEXT_SCRAM_SHA_256_SASL_JAAS_CONFIG='org.apache.kafka.common.security.scram.ScramLoginModule required username="admin" password="admin";' \
    confluentinc/cp-kafka:7.6.0
```

Provision the initial `admin` SCRAM credential before the plugin can
connect. Refer to the Confluent quickstart for the bootstrap commands.

### End-to-end with `bao`

```bash
$ make kafka-database-plugin
$ bao server -dev

$ bao secrets enable database
$ bao write database/config/kafka \
    plugin_name=kafka-database-plugin \
    brokers=localhost:9092 \
    username=admin password=admin \
    mechanism=SCRAM-SHA-256 \
    allowed_roles=producer

$ bao write database/roles/producer \
    db_name=kafka \
    creation_statements='{"mechanism":"SCRAM-SHA-256"}' \
    default_ttl=1h

$ bao read database/creds/producer

# Verify the credential works (use kcat / kafka-console-producer):
$ kafka-console-producer --bootstrap-server localhost:9092 \
    --producer.config <(cat <<EOF
security.protocol=SASL_PLAINTEXT
sasl.mechanism=SCRAM-SHA-256
sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username="<USERNAME>" password="<PASSWORD>";
EOF
) --topic test < /dev/null

# Revoke:
$ bao lease revoke <LEASE_ID>
$ kafka-configs.sh --bootstrap-server localhost:9092 \
    --describe --entity-type users --entity-name <USERNAME>
# returns nothing → credential was deleted.

# Root rotation
$ bao write -force database/rotate-root/kafka
```

### Failure modes

| Scenario | Expected behavior |
| --- | --- |
| Empty `creation_statements` | `dbutil.ErrEmptyCreationStatement` |
| `acls` non-empty in statement | "acls in creation_statements are not yet supported"; credential is created then deleted |
| Unknown mechanism in statement | "unsupported mechanism" |
| Mechanism in statement differs from cluster config | Cluster returns INVALID_PRINCIPAL_TYPE on first auth — operator must align mechanism with the listener |
| Connection broker dead | Init verify fails with the wrapped franz-go error |
