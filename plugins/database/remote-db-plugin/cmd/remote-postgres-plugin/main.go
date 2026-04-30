// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log"
	"os"

	remotedb "github.com/openbao/openbao/plugins/database/remote-db-plugin"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
)

func main() {
	if err := run(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func run() error {
	dbplugin.ServeMultiplex(remotedb.New(remotedb.PostgresDialect))
	return nil
}
