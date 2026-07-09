/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import errorMessage from 'vault/utils/error-message';

const splitList = (value) =>
  value
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);

export default class RelayCaController extends Controller {
  @service store;
  @service flashMessages;

  @tracked showEndpointForm = false;
  @tracked endpointValue = '';
  @tracked dnsSansValue = '';
  @tracked ipSansValue = '';
  @tracked isSaving = false;

  get dnsSansDisplay() {
    return (this.model.hub_dns_sans || []).join(', ');
  }

  get ipSansDisplay() {
    return (this.model.hub_ip_sans || []).join(', ');
  }

  @action
  refreshList() {
    this.send('doRefresh');
  }

  @action
  async rotateHub() {
    try {
      await this.store.adapterFor('relay').rotateCa({});
      this.flashMessages.success('Rotated the hub TLS certificate.');
    } catch (e) {
      this.flashMessages.danger(errorMessage(e));
    }
    this.send('doRefresh');
  }

  @action
  async rotateFull() {
    try {
      await this.store.adapterFor('relay').rotateCa({ full: true });
      this.flashMessages.success('Rotated the spoke CA and hub TLS certificate.');
    } catch (e) {
      this.flashMessages.danger(errorMessage(e));
    }
    this.send('doRefresh');
  }

  @action
  toggleEndpointForm() {
    if (!this.showEndpointForm) {
      this.endpointValue = this.model.hub_endpoint || '';
      this.dnsSansValue = this.dnsSansDisplay;
      this.ipSansValue = this.ipSansDisplay;
    }
    this.showEndpointForm = !this.showEndpointForm;
  }

  @action
  async updateEndpoint(evt) {
    evt.preventDefault();
    this.isSaving = true;
    const data = {
      hub_endpoint: this.endpointValue,
      hub_dns_sans: splitList(this.dnsSansValue),
      hub_ip_sans: splitList(this.ipSansValue),
    };
    try {
      await this.store.adapterFor('relay').updateEndpoint(data);
      this.flashMessages.success('Updated the hub endpoint.');
      this.showEndpointForm = false;
      this.send('doRefresh');
    } catch (e) {
      this.flashMessages.danger(errorMessage(e));
    } finally {
      this.isSaving = false;
    }
  }
}
