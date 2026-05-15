// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// spoke-agent-v2 connects to hub's agentserver and executes plugin-runner
// to run built-in OpenBao database plugins locally in the spoke cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	proto "github.com/openbao/openbao/plugins/database/remote-db-plugin/proto/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	serverAddr := flag.String("server", "localhost:50052", "OpenBao spoke server address")
	spokeName := flag.String("name", "spoke-1", "Unique name for this spoke cluster")
	flag.Parse()

	// Find plugin-runner binary (should be in same directory as spoke-agent)
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("failed to get executable path: %v", err)
	}
	pluginRunnerPath := filepath.Join(filepath.Dir(exePath), "plugin-runner")

	// Verify plugin-runner exists
	if _, err := os.Stat(pluginRunnerPath); os.IsNotExist(err) {
		log.Fatalf("plugin-runner not found at %s", pluginRunnerPath)
	}

	log.Printf("using plugin-runner at: %s", pluginRunnerPath)

	conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	client := proto.NewAgentServiceClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		log.Fatalf("failed to open stream: %v", err)
	}

	// Register with hub
	if err := stream.Send(&proto.AgentMessage{
		ClientName: *spokeName,
		IsResponse: false,
	}); err != nil {
		log.Fatalf("failed to register: %v", err)
	}

	ack, err := stream.Recv()
	if err != nil {
		log.Fatalf("failed to receive ack: %v", err)
	}
	log.Printf("registered: %s", ack.Output)

	// Process commands
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			log.Println("server disconnected")
			return
		}
		if err != nil {
			log.Printf("stream error: %v", err)
			return
		}

		if msg.Command == "" || msg.IsResponse {
			continue
		}

		log.Printf("executing: %s", msg.Command)

		var output string
		var execErr error

		// Check if this is a plugin-runner command
		if strings.HasPrefix(msg.Command, "plugin-runner ") {
			// Extract JSON request (everything after "plugin-runner ")
			jsonRequest := strings.TrimPrefix(msg.Command, "plugin-runner ")

			// Execute plugin-runner with full path
			cmd := exec.Command(pluginRunnerPath, jsonRequest)
			out, err := cmd.CombinedOutput()
			output = strings.TrimSpace(string(out))
			execErr = err
		} else {
			// Fall back to bash execution for backward compatibility
			cmd := exec.Command("bash", "-lc", msg.Command)
			out, err := cmd.CombinedOutput()
			output = strings.TrimSpace(string(out))
			execErr = err
		}

		if execErr != nil {
			output = fmt.Sprintf("Error: %v\n%s", execErr, output)
		}

		if err := stream.Send(&proto.AgentMessage{
			ClientName: *spokeName,
			Output:     output,
			IsResponse: true,
		}); err != nil {
			log.Printf("failed to send response: %v", err)
			return
		}
	}
}
