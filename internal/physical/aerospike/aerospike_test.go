// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package aerospike

import (
	"math/bits"
	"testing"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/v2/internal/helper/testhelpers/aerospike"
)

func TestAerospikeBackend(t *testing.T) {
	if bits.UintSize == 32 {
		t.Skip("Aerospike storage is only supported on 64-bit architectures")
	}
	cleanup, config := aerospike.PrepareTestContainer(t)
	defer cleanup()

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewAerospikeBackend(map[string]string{
		"hostname":  config.Hostname,
		"port":      config.Port,
		"namespace": config.Namespace,
		"set":       config.Set,
	}, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}
