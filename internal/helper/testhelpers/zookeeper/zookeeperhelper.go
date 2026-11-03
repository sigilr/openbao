// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package zookeeper

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-zookeeper/zk"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// PrepareTestContainer creates a ZooKeeper docker container. If the
// environment variable ZOOKEEPER_ADDR is set, the tests are executed
// against the specified address instead, and no container is launched.
func PrepareTestContainer(t *testing.T) (func(), string) {
	t.Helper()

	if addr := os.Getenv("ZOOKEEPER_ADDR"); addr != "" {
		return func() {}, addr
	}

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ContainerName: "zookeeper",
		ImageRepo:     "zookeeper",
		ImageTag:      "3.9",
		Ports:         []string{"2181/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start docker zookeeper: %s", err)
	}

	svc, err := runner.StartService(context.Background(), func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
		address := fmt.Sprintf("%s:%d", host, port)
		cfg := docker.NewServiceHostPort(host, port)

		client, _, err := zk.Connect([]string{address}, 2*time.Second)
		if err != nil {
			return nil, fmt.Errorf("could not connect to zookeeper container: %w", err)
		}
		defer client.Close()

		if _, _, err := client.Exists("/"); err != nil {
			return nil, fmt.Errorf("zookeeper container not ready: %w", err)
		}

		return cfg, nil
	})
	if err != nil {
		t.Fatalf("Could not start docker zookeeper: %s", err)
	}

	return svc.Cleanup, svc.Config.Address()
}
