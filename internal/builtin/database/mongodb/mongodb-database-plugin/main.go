// Copyright (c) HashiCorp, Inc.
// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"log"
	"os"

	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/v2/internal/builtin/database/mongodb"
)

func main() {
	if err := Run(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

// Run instantiates a MongoDB object and runs the RPC server for the plugin.
func Run() error {
	dbplugin.ServeMultiplex(mongodb.New)
	return nil
}
