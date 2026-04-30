// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// hub-server is a standalone test binary that starts the embedded agentserver
// and lets you send commands to connected spokes interactively.
//
// This is the equivalent of "grpc-agent init" + "grpc-agent exec remote".
//
// Usage:
//
//	# hub
//	./hub-server --port 50051
//	# waits for spoke to connect, then accepts stdin commands:
//	# <spoke-name> <command>
//	# example: spoke-cluster-1 hostname
//
//	# spoke (in another terminal / pod)
//	./spoke-agent --server <HUB_IP>:50051 --name spoke-cluster-1
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/openbao/openbao/plugins/database/remote-postgres/internal/agentserver"
)

func main() {
	port := flag.Int("port", 50052, "Port to listen for spoke agents")
	flag.Parse()

	if err := agentserver.Instance().Start(*port); err != nil {
		log.Fatalf("failed to start spoke server: %v", err)
	}
	log.Printf("hub spoke server listening on :%d", *port)
	log.Println("waiting for spokes to connect...")
	log.Println()
	log.Println("type commands once a spoke is connected:")
	log.Println("  <spoke-name> <command>")
	log.Println("  example: spoke-cluster-1 hostname")
	log.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			log.Println("usage: <spoke-name> <command>")
			continue
		}
		spokeName := parts[0]
		command := parts[1]

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, err := agentserver.Instance().RunCommand(ctx, spokeName, command)
		cancel()

		if err != nil {
			log.Printf("error: %v", err)
			continue
		}
		fmt.Println(output)
	}
}
