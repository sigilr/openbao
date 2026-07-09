/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { action } from '@ember/object';

export default class RelayIndexController extends Controller {
  @action
  refreshList() {
    this.send('doRefresh');
  }
}
