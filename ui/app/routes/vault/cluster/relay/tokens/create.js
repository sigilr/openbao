/**
 * Copyright (c) AppsCode Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

export default Route.extend({
  resetController(controller, isExiting) {
    if (isExiting) {
      controller.setProperties({
        ttl: '24h',
        allowedSpokeName: '',
        description: '',
        createdToken: null,
      });
    }
  },
});
