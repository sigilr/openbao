// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package etcd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

type Config struct {
	docker.ServiceURL
}

// PrepareTestContainer creates an etcd docker container. If the environment
// variable ETCD_ADDR is set, the tests are executed against the specified
// address instead, and no container is launched.
func PrepareTestContainer(t *testing.T, version string) (func(), *Config) {
	t.Helper()

	if addr := os.Getenv("ETCD_ADDR"); addr != "" {
		u, err := docker.NewServiceURLParse(addr)
		if err != nil {
			t.Fatal(err)
		}
		return func() {}, &Config{ServiceURL: *u}
	}

	// Check https://github.com/etcd-io/etcd/releases for latest releases.
	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ContainerName: "etcd",
		ImageRepo:     "gcr.io/etcd-development/etcd",
		ImageTag:      version,
		Cmd: []string{
			"/usr/local/bin/etcd",
			"--name", "s1",
			"--listen-client-urls", "http://0.0.0.0:2379",
			"--advertise-client-urls", "http://0.0.0.0:2379",
			"--listen-peer-urls", "http://0.0.0.0:2380",
			"--initial-advertise-peer-urls", "http://0.0.0.0:2380",
			"--initial-cluster", "s1=http://0.0.0.0:2380",
			"--initial-cluster-token", "tkn",
			"--initial-cluster-state", "new",
			"--log-level", "info",
			"--logger", "zap",
			"--log-outputs", "stderr",
		},
		Ports: []string{"2379/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start docker etcd container: %s", err)
	}

	svc, err := runner.StartService(context.Background(), func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
		address := fmt.Sprintf("%s:%d", host, port)
		s := docker.NewServiceURL(url.URL{
			Scheme: "http",
			Host:   address,
		})

		client, err := clientv3.New(clientv3.Config{
			Endpoints:   []string{address},
			DialTimeout: 2 * time.Minute,
		})
		if err != nil {
			return nil, fmt.Errorf("could not connect to etcd container: %w", err)
		}
		defer client.Close()

		// Enable authentication for the tests.
		if _, err := client.RoleAdd(ctx, "root"); err != nil {
			return nil, fmt.Errorf("could not add etcd root role: %w", err)
		}
		if _, err := client.UserAdd(ctx, "root", "insecure"); err != nil {
			return nil, fmt.Errorf("could not add etcd root user: %w", err)
		}
		if _, err := client.UserGrantRole(ctx, "root", "root"); err != nil {
			return nil, fmt.Errorf("could not grant etcd root role: %w", err)
		}
		if _, err := client.AuthEnable(ctx); err != nil {
			return nil, fmt.Errorf("could not enable etcd auth: %w", err)
		}

		return &Config{
			ServiceURL: *s,
		}, nil
	})
	if err != nil {
		t.Fatalf("Could not start docker etcd container: %s", err)
	}

	return svc.Cleanup, svc.Config.(*Config)
}
