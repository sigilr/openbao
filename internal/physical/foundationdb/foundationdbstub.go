// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !foundationdb

package foundationdb

import (
	"fmt"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/physical"
)

// NewFDBBackend returns an error unless this binary was built with the
// "foundationdb" build tag and linked against the native libfdb_c client
// library (see FDB_ENABLED=1 in the Makefile, and this package's README.md).
func NewFDBBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	return nil, fmt.Errorf("FoundationDB backend not available in this OpenBao build")
}
