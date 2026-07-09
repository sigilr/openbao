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
    try {
      const resp = await this.store.adapterFor('relay').caInfo();
      return { ...resp.data, error: null };
    } catch (e) {
      return { error: errorMessage(e) };
    }
  },

  actions: {
    doRefresh() {
      this.refresh();
    },
  },
});
