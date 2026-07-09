/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import ApplicationAdapter from './application';

// Talks to the relay/ logical backend (hub-and-spoke remote database plugins).
const BASE = '/v1/relay';

export default ApplicationAdapter.extend({
  spokes() {
    return this.ajax(`${BASE}/spokes`, 'GET');
  },

  caInfo() {
    return this.ajax(`${BASE}/ca/info`, 'GET');
  },

  rotateCa(data) {
    return this.ajax(`${BASE}/ca/rotate`, 'POST', { data });
  },

  updateEndpoint(data) {
    return this.ajax(`${BASE}/ca/update-endpoint`, 'POST', { data });
  },

  listTokens() {
    return this.ajax(`${BASE}/bootstrap-tokens`, 'GET', { data: { list: true } });
  },

  createToken(data) {
    return this.ajax(`${BASE}/bootstrap-tokens`, 'POST', { data });
  },

  revokeToken(id) {
    return this.ajax(`${BASE}/bootstrap-tokens/${encodeURIComponent(id)}`, 'DELETE');
  },
});
