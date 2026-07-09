/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import { Response } from 'miragejs';
import { click, fillIn, visit, settled } from '@ember/test-helpers';
import authPage from 'vault/tests/pages/auth';
import logout from 'vault/tests/pages/logout';

const SPOKES_RESPONSE = {
  data: {
    spokes: [
      {
        name: 'spoke-east',
        connected_at_unix: 1717900000,
        last_seen_unix: 1717900060,
        last_seen_seconds: 5,
        healthy: true,
      },
      {
        name: 'spoke-west',
        connected_at_unix: 1717900100,
        last_seen_unix: 1717900110,
        last_seen_seconds: 120,
        healthy: false,
      },
    ],
    connected_count: 2,
    healthy_count: 1,
    listener_port: 8201,
    stale_after_seconds: 60,
  },
};

const CA_INFO_RESPONSE = {
  data: {
    ca_cert_pem: '-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n',
    ca_cert_hash: 'sha256:deadbeef',
    ca_subject: 'CN=openbao-spoke-ca',
    ca_not_after: 2027900000,
    hub_endpoint: 'hub.example.com:8201',
    hub_cert_subject: 'CN=openbao-hub',
    hub_cert_not_after: 1827900000,
    hub_dns_sans: ['hub.example.com'],
    hub_ip_sans: ['10.0.0.1'],
    created_unix: 1717900000,
    listener_port: 8201,
  },
};

module('Acceptance | relay', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(async function () {
    this.server.get('/sys/internal/ui/resultant-acl', () =>
      this.server.create('configuration', { data: { root: true } })
    );
    await authPage.login();
  });

  hooks.afterEach(async function () {
    await logout.visit();
    await settled();
  });

  test('it renders connected spokes', async function (assert) {
    this.server.get('/relay/spokes', () => SPOKES_RESPONSE);

    await visit('/vault/relay');
    assert.dom('[data-test-relay-header]').hasText('Relay');
    assert.dom('[data-test-spoke-row]').exists({ count: 2 }, 'renders a row per spoke');
    assert.dom('[data-test-spokes-summary]').includesText('2 connected, 1 healthy');
  });

  test('it renders an empty state when spokes cannot be listed', async function (assert) {
    this.server.get('/relay/spokes', () => new Response(400, {}, { errors: ['CA not initialized'] }));

    await visit('/vault/relay');
    assert.dom('[data-test-empty-state-title]').hasText('Unable to list spokes');
  });

  test('it lists and revokes bootstrap tokens', async function (assert) {
    assert.expect(4);

    let revoked = false;
    this.server.get('/relay/bootstrap-tokens', () => {
      if (revoked) {
        return new Response(404, {}, { errors: [] });
      }
      return {
        data: {
          keys: ['abc123'],
          key_info: {
            abc123: {
              description: 'east coast spokes',
              allowed_spoke_name: 'spoke-east',
              created_unix: 1717900000,
              expiration_unix: 0,
              expired: false,
              usages: ['signing', 'authentication'],
            },
          },
        },
      };
    });
    this.server.delete('/relay/bootstrap-tokens/abc123', () => {
      revoked = true;
      assert.true(true, 'revoke request made');
      return {};
    });

    await visit('/vault/relay/tokens');
    assert.dom('[data-test-token-row="abc123"]').exists('token row renders');
    assert.dom('[data-test-token-row="abc123"]').includesText('east coast spokes');

    await click('[data-test-confirm-action-trigger]');
    await click('[data-test-confirm-button]');
    assert.dom('[data-test-empty-state-title]').hasText('No bootstrap tokens');
  });

  test('it renders an unreadable token record', async function (assert) {
    assert.expect(3);

    this.server.get('/relay/bootstrap-tokens', () => ({
      data: {
        keys: ['good', 'badrec'],
        key_info: {
          good: { description: 'fine', expiration_unix: 0, expired: false, usages: ['signing'] },
          badrec: { error: 'unreadable' },
        },
      },
    }));

    await visit('/vault/relay/tokens');
    assert.dom('[data-test-token-row="good"]').includesText('fine', 'readable token renders normally');
    assert.dom('[data-test-token-row="badrec"]').exists('unreadable record still renders (revocable)');
    assert
      .dom('[data-test-token-row="badrec"] [data-test-token-unreadable]')
      .exists('flags the unreadable record');
  });

  test('it creates a bootstrap token and shows it once', async function (assert) {
    assert.expect(3);

    this.server.post('/relay/bootstrap-tokens', (schema, req) => {
      const body = JSON.parse(req.requestBody);
      assert.strictEqual(body.allowed_spoke_name, 'spoke-east', 'sends allowed spoke name');
      return {
        data: {
          id: 'abc123',
          token: 'abc123.supersecretvalue',
          expiration_unix: 1827900000,
          allowed_spoke_name: 'spoke-east',
          usages: ['signing', 'authentication'],
        },
      };
    });

    await visit('/vault/relay/tokens/create');
    await fillIn('[data-test-token-spoke-name]', 'spoke-east');
    await fillIn('[data-test-token-description]', 'east coast spokes');
    await click('[data-test-token-save]');

    assert.dom('[data-test-token-created]').exists('shows the created token panel');
    assert.dom('[data-test-token-value]').hasText('abc123.supersecretvalue');
  });

  test('it renders CA info and rotates the hub cert', async function (assert) {
    assert.expect(3);

    this.server.get('/relay/ca/info', () => CA_INFO_RESPONSE);
    this.server.post('/relay/ca/rotate', (schema, req) => {
      const body = JSON.parse(req.requestBody);
      assert.notOk(body.full, 'hub rotation does not set full');
      return { data: { rotated: 'hub', ca_cert_hash: 'sha256:deadbeef' } };
    });

    await visit('/vault/relay/ca');
    assert.dom('[data-test-ca-info]').includesText('hub.example.com:8201');

    await click('[data-test-ca-rotate-hub] [data-test-confirm-action-trigger]');
    await click('[data-test-confirm-button]');
    assert.dom('[data-test-ca-info]').exists('returns to info view after rotation');
  });

  test('it renders an empty state when the CA is not initialized', async function (assert) {
    this.server.get(
      '/relay/ca/info',
      () => new Response(400, {}, { errors: ['CA not initialized; run `bao relay init`'] })
    );

    await visit('/vault/relay/ca');
    assert.dom('[data-test-empty-state-title]').hasText('Certificate authority not available');
  });
});
