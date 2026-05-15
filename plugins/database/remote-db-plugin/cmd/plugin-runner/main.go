// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/openbao/openbao/plugins/database/remote-db-plugin/spoke-agent-v2/runner"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: plugin-runner <json-request>\n")
		os.Exit(1)
	}

	r := runner.NewPluginRunner()

	requestJSON := os.Args[1]
	response, err := r.ExecuteRequest(requestJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(response)
}
