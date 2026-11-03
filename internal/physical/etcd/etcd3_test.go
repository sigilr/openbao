// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package etcd

import (
	"fmt"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	etcdhelper "github.com/openbao/openbao/v2/internal/helper/testhelpers/etcd"
)

func TestEtcd3Backend(t *testing.T) {
	cleanup, config := etcdhelper.PrepareTestContainer(t, "v3.5.0")
	defer cleanup()

	logger := logging.NewVaultLogger(log.Debug)
	configMap := map[string]string{
		"address":  config.URL().String(),
		"path":     fmt.Sprintf("/vault-%d", time.Now().Unix()),
		"etcd_api": "3",
		"username": "root",
		"password": "insecure",

		// Syncing advertised client urls should be disabled since docker port mapping confuses the client.
		"sync": "false",
	}

	b, err := NewEtcdBackend(configMap, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	b2, err := NewEtcdBackend(configMap, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
	physical.ExerciseHABackend(t, b.(physical.HABackend), b2.(physical.HABackend))
}
