/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import errorMessage from 'vault/utils/error-message';

export default class RelayTokensIndexController extends Controller {
  @service store;
  @service flashMessages;

  @action
  refreshList() {
    this.send('doRefresh');
  }

  @action
  async revokeToken(id) {
    try {
      await this.store.adapterFor('relay').revokeToken(id);
      this.flashMessages.success(`Revoked bootstrap token ${id}.`);
    } catch (e) {
      this.flashMessages.danger(errorMessage(e));
    }
    this.send('doRefresh');
  }
}
