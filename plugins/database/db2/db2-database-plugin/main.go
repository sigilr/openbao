// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"log"
	"os"

	"github.com/openbao/openbao/v2/plugins/database/db2"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
)

func main() {
	if err := Run(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func Run() error {
	dbplugin.ServeMultiplex(db2.New)
	return nil
}
