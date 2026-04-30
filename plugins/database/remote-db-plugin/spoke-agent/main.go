// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os/exec"
	"strings"

	agentproto "github.com/openbao/openbao/plugins/database/remote-db-plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	serverAddr := flag.String("server", "localhost:50052", "OpenBao spoke server address")
	spokeName := flag.String("name", "spoke-1", "Unique name for this spoke cluster")
	flag.Parse()

	conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	client := agentproto.NewAgentServiceClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		log.Fatalf("failed to open stream: %v", err)
	}

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
	log.Printf("registered: %s", ack.Output)

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
