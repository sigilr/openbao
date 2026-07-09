/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import errorMessage from 'vault/utils/error-message';

export default Route.extend({
  store: service(),

  async model() {
    const adapter = this.store.adapterFor('relay');
    try {
      const list = await adapter.listTokens();
      // key_info covers every displayable token id — corrupt records carry an
      // `error` marker instead of being dropped — so iterate it directly.
      const keyInfo = list.data?.key_info || {};
      const tokens = Object.entries(keyInfo).map(([id, meta]) => ({ id, ...meta }));
      return { tokens, error: null };
    } catch (e) {
      // an empty list returns 404
      if (e.httpStatus === 404) {
        return { tokens: [], error: null };
      }
      return { tokens: [], error: errorMessage(e) };
    }
  },

  actions: {
    doRefresh() {
      this.refresh();
    },
  },
});
