// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// spoke-agent is the lightweight spoke-side binary that connects to OpenBao's
// embedded spoke server and executes commands locally inside the spoke cluster.
//
// This replaces "grpc-agent join". No go-plugin or local_exec_plugin needed —
// commands are executed directly via bash.
//
// Usage (run inside a pod in the spoke cluster):
//
//	spoke-agent --server=<openbao-hub-ip>:50052 --name=spoke-cluster-1
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os/exec"
	"strings"

	agentproto "github.com/openbao/openbao/plugins/database/remote-postgres/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	serverAddr := flag.String("server", "localhost:50052", "OpenBao spoke server address (hub IP:port)")
	spokeName := flag.String("name", "spoke-1", "Unique name for this spoke cluster")
	flag.Parse()

	conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect to spoke server at %s: %v", *serverAddr, err)
	}
	defer conn.Close()

	client := agentproto.NewAgentServiceClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		log.Fatalf("failed to open stream: %v", err)
	}

	// Phase 1: register this spoke
	if err := stream.Send(&agentproto.AgentMessage{
		ClientName: *spokeName,
		IsResponse: false,
	}); err != nil {
		log.Fatalf("failed to register: %v", err)
	}

	ack, err := stream.Recv()
	if err != nil {
		log.Fatalf("failed to receive ack: %v", err)
	}
	log.Printf("registered with OpenBao spoke server: %s", ack.Output)

	// Phase 2: receive commands and execute them locally
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

		// Run the command locally — inside the spoke pod, ClusterIP services are reachable
		out, execErr := exec.Command("bash", "-lc", msg.Command).CombinedOutput()
		output := strings.TrimSpace(string(out))
		if execErr != nil {
			output = "Error: " + execErr.Error() + "\n" + output
		}

		if err := stream.Send(&agentproto.AgentMessage{
			ClientName: *spokeName,
			Output:     output,
			IsResponse: true,
		}); err != nil {
			log.Printf("failed to send response: %v", err)
			return
		}
	}
}
