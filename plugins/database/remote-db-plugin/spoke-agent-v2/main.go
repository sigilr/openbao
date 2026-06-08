// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

// spoke-agent-v2 connects to the hub's proxy gRPC server using mTLS, then
// executes plugin-runner to run built-in OpenBao database plugins locally in
// the spoke cluster.
//
// The credentials directory must contain:
//
//	cert.pem  client cert issued by `bao agent join`
//	key.pem   matching private key
//	ca.pem    spoke-CA root used to verify the hub
//
// The certificate's Common Name is the spoke's authoritative identity; the
// hub reads it off the verified peer cert.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"google.golang.org/grpc/credentials"
)

func main() {
	serverAddr := flag.String("server", "localhost:50053", "Hub gRPC address (host:port)")
	credsDir := flag.String("credentials-dir", "/etc/openbao-spoke", "Directory containing cert.pem, key.pem, ca.pem")
	serverName := flag.String("server-name", "", "Override the SNI/expected hub CN (defaults to the host part of -server)")
	flag.Parse()

	pluginRunnerPath, err := findPluginRunner()
	if err != nil {
		log.Fatalf("plugin-runner: %v", err)
	}
	log.Printf("using plugin-runner at: %s", pluginRunnerPath)

	tlsCfg, err := loadClientTLS(*credsDir, *serverName, *serverAddr)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	spokeName := tlsCfg.Certificates[0].Leaf.Subject.CommonName
	log.Printf("connecting to hub as spoke %q", spokeName)

	conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := proto.NewAgentServiceClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		log.Fatalf("open stream: %v", err)
	}

	// Hub uses the verified client cert CN as identity; we still send a
	// ClientName so old log lines stay informative, but it is NOT load-bearing.
	if err := stream.Send(&proto.AgentMessage{ClientName: spokeName, IsResponse: false}); err != nil {
		log.Fatalf("register: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		log.Fatalf("recv ack: %v", err)
	}
	log.Printf("registered: %s", ack.Output)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			log.Println("hub disconnected")
			return
		}
		if err != nil {
			log.Printf("stream error: %v", err)
			return
		}
		if msg.Command == "" || msg.IsResponse {
			continue
		}

		output, execErr := runRequest(pluginRunnerPath, msg.Command)
		if execErr != nil {
			output = fmt.Sprintf("Error: %v\n%s", execErr, output)
		}
		if err := stream.Send(&proto.AgentMessage{
			ClientName: spokeName,
			Output:     output,
			IsResponse: true,
		}); err != nil {
			log.Printf("send response: %v", err)
			return
		}
	}
}

// runRequest executes a plugin-runner command. The previous "bash -lc"
// fallback has been removed: with mTLS authenticating the hub, there is no
// legitimate reason for the hub to send arbitrary shell, and accepting it
// turned the hub's gRPC port into a per-spoke RCE primitive.
func runRequest(pluginRunner, command string) (string, error) {
	if !strings.HasPrefix(command, "plugin-runner ") {
		return "", fmt.Errorf("rejected non-plugin-runner command: %q", command)
	}
	jsonRequest := strings.TrimPrefix(command, "plugin-runner ")
	cmd := exec.Command(pluginRunner, jsonRequest)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func findPluginRunner() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(exePath), "plugin-runner")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("plugin-runner not found at %s: %w", p, err)
	}
	return p, nil
}

// loadClientTLS reads cert/key/ca from credsDir and returns a tls.Config
// suitable for grpc.NewClient. The leaf cert is parsed so the CN is available
// as the spoke identity without a second open of the PEM file.
func loadClientTLS(credsDir, serverName, serverAddr string) (*tls.Config, error) {
	certPath := filepath.Join(credsDir, "cert.pem")
	keyPath := filepath.Join(credsDir, "key.pem")
	caPath := filepath.Join(credsDir, "ca.pem")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key from %s: %w", credsDir, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse client cert: %w", err)
	}
	cert.Leaf = leaf

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s did not yield any CA certs", caPath)
	}

	if serverName == "" {
		host, _, _ := strings.Cut(serverAddr, ":")
		serverName = host
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
