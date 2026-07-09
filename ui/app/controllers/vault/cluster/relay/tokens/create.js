/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import errorMessage from 'vault/utils/error-message';

export default class RelayTokensCreateController extends Controller {
  @service store;
  @service flashMessages;
  @service router;

  @tracked ttl = '24h';
  @tracked allowedSpokeName = '';
  @tracked description = '';
  @tracked createdToken = null;
  @tracked isSaving = false;

  @action
  async createToken(evt) {
    evt.preventDefault();
    this.isSaving = true;
    const data = {};
    if (this.ttl) {
      data.ttl = this.ttl;
    }
    if (this.allowedSpokeName) {
      data.allowed_spoke_name = this.allowedSpokeName;
    }
    if (this.description) {
      data.description = this.description;
    }
    try {
      const resp = await this.store.adapterFor('relay').createToken(data);
      this.createdToken = resp.data;
    } catch (e) {
      this.flashMessages.danger(errorMessage(e));
    } finally {
      this.isSaving = false;
    }
  }

  @action
  done() {
    this.router.transitionTo('vault.cluster.relay.tokens');
  }
}
