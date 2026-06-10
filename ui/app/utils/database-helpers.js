/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

export const AVAILABLE_PLUGIN_TYPES = [
  {
    value: 'mysql-aurora-database-plugin',
    displayName: 'MySQL (Aurora)',
    fields: [
      { attr: 'plugin_name' },
      { attr: 'name' },
      { attr: 'verify_connection', show: false },
      { attr: 'password_policy' },
      { attr: 'connection_url', group: 'pluginConfig' },
      { attr: 'username', group: 'pluginConfig', show: false },
      { attr: 'password', group: 'pluginConfig', show: false },
      { attr: 'max_open_connections', group: 'pluginConfig' },
      { attr: 'max_idle_connections', group: 'pluginConfig' },
      { attr: 'max_connection_lifetime', group: 'pluginConfig' },
      { attr: 'username_template', group: 'pluginConfig' },
      { attr: 'tls', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_ca', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'root_rotation_statements', group: 'statements' },
    ],
  },
  {
    value: 'mysql-legacy-database-plugin',
    displayName: 'MySQL (Legacy)',
    fields: [
      { attr: 'plugin_name' },
      { attr: 'name' },
      { attr: 'verify_connection', show: false },
      { attr: 'password_policy' },
      { attr: 'connection_url', group: 'pluginConfig' },
      { attr: 'username', group: 'pluginConfig', show: false },
      { attr: 'password', group: 'pluginConfig', show: false },
      { attr: 'max_open_connections', group: 'pluginConfig' },
      { attr: 'max_idle_connections', group: 'pluginConfig' },
      { attr: 'max_connection_lifetime', group: 'pluginConfig' },
      { attr: 'username_template', group: 'pluginConfig' },
      { attr: 'tls', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_ca', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'root_rotation_statements', group: 'statements' },
    ],
  },
  {
    value: 'mysql-database-plugin',
    displayName: 'MySQL/MariaDB',
    fields: [
      { attr: 'plugin_name' },
      { attr: 'name' },
      { attr: 'verify_connection', show: false },
      { attr: 'password_policy' },
      { attr: 'connection_url', group: 'pluginConfig' },
      { attr: 'username', group: 'pluginConfig', show: false },
      { attr: 'password', group: 'pluginConfig', show: false },
      { attr: 'max_open_connections', group: 'pluginConfig' },
      { attr: 'max_idle_connections', group: 'pluginConfig' },
      { attr: 'max_connection_lifetime', group: 'pluginConfig' },
      { attr: 'username_template', group: 'pluginConfig' },
      { attr: 'tls', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_ca', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'root_rotation_statements', group: 'statements' },
    ],
  },
  {
    value: 'mysql-rds-database-plugin',
    displayName: 'MySQL (RDS)',
    fields: [
      { attr: 'plugin_name' },
      { attr: 'name' },
      { attr: 'verify_connection', show: false },
      { attr: 'password_policy' },
      { attr: 'connection_url', group: 'pluginConfig' },
      { attr: 'username', group: 'pluginConfig', show: false },
      { attr: 'password', group: 'pluginConfig', show: false },
      { attr: 'max_open_connections', group: 'pluginConfig' },
      { attr: 'max_idle_connections', group: 'pluginConfig' },
      { attr: 'max_connection_lifetime', group: 'pluginConfig' },
      { attr: 'username_template', group: 'pluginConfig' },
      { attr: 'tls', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_ca', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'root_rotation_statements', group: 'statements' },
    ],
  },
  {
    value: 'postgresql-database-plugin',
    displayName: 'PostgreSQL',
    fields: [
      { attr: 'plugin_name' },
      { attr: 'name' },
      { attr: 'verify_connection', show: false },
      { attr: 'password_policy' },
      { attr: 'connection_url', group: 'pluginConfig' },
      { attr: 'username', group: 'pluginConfig', show: false },
      { attr: 'password', group: 'pluginConfig', show: false },
      { attr: 'max_open_connections', group: 'pluginConfig' },
      { attr: 'max_idle_connections', group: 'pluginConfig' },
      { attr: 'max_connection_lifetime', group: 'pluginConfig' },
      { attr: 'username_template', group: 'pluginConfig' },
      { attr: 'root_rotation_statements', group: 'statements' },
    ],
  },
  {
    value: 'kafka-database-plugin',
    displayName: 'Apache Kafka',
    fields: [
      { attr: 'plugin_name' },
      { attr: 'name' },
      { attr: 'verify_connection', show: false },
      { attr: 'password_policy' },
      { attr: 'brokers', group: 'pluginConfig' },
      { attr: 'username', group: 'pluginConfig', show: false },
      { attr: 'password', group: 'pluginConfig', show: false },
      { attr: 'mechanism', group: 'pluginConfig' },
      { attr: 'use_tls', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_ca', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_certificate', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_key', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'insecure', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'username_template', group: 'pluginConfig' },
      { attr: 'root_rotation_statements', group: 'statements' },
    ],
  },
  {
    value: 'remote-kafka-plugin',
    displayName: 'Apache Kafka (Remote)',
    fields: [
      { attr: 'plugin_name' },
      { attr: 'name' },
      { attr: 'verify_connection', show: false },
      { attr: 'password_policy' },
      { attr: 'spoke_name', group: 'pluginConfig' },
      { attr: 'brokers', group: 'pluginConfig' },
      { attr: 'username', group: 'pluginConfig', show: false },
      { attr: 'password', group: 'pluginConfig', show: false },
      { attr: 'mechanism', group: 'pluginConfig' },
      { attr: 'use_tls', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_ca', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_certificate', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'tls_key', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'insecure', group: 'pluginConfig', subgroup: 'TLS options' },
      { attr: 'username_template', group: 'pluginConfig' },
      { attr: 'root_rotation_statements', group: 'statements' },
    ],
  },
];

export const ROLE_FIELDS = {
  static: ['username', 'rotation_period'],
  dynamic: ['default_ttl', 'max_ttl'],
};

export const STATEMENT_FIELDS = {
  static: {
    default: ['rotation_statements'],
    'mysql-database-plugin': [],
    'mysql-aurora-database-plugin': [],
    'mysql-legacy-database-plugin': [],
    'mysql-rds-database-plugin': [],
    'postgresql-database-plugin': [],
    'kafka-database-plugin': [],
    'remote-kafka-plugin': [],
  },
  dynamic: {
    default: ['creation_statements', 'revocation_statements', 'rollback_statements', 'renew_statements'],
    'mysql-database-plugin': ['creation_statements', 'revocation_statements'],
    'mysql-aurora-database-plugin': ['creation_statements', 'revocation_statements'],
    'mysql-legacy-database-plugin': ['creation_statements', 'revocation_statements'],
    'mysql-rds-database-plugin': ['creation_statements', 'revocation_statements'],
    'postgresql-database-plugin': [
      'creation_statements',
      'revocation_statements',
      'rollback_statements',
      'renew_statements',
    ],
    'kafka-database-plugin': ['creation_statements'],
    'remote-kafka-plugin': ['creation_statements'],
  },
};

export function getStatementFields(type, plugin) {
  if (!type) return null;
  let dbValidFields = STATEMENT_FIELDS[type].default;
  if (STATEMENT_FIELDS[type][plugin]) {
    dbValidFields = STATEMENT_FIELDS[type][plugin];
  }
  return dbValidFields;
}

export function getRoleFields(type) {
  if (!type) return null;
  return ROLE_FIELDS[type];
}
