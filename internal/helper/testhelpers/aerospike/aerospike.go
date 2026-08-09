// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package aerospike

import (
	"context"
	"testing"
	"time"

	aero "github.com/aerospike/aerospike-client-go/v8"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

type Config struct {
	Hostname  string
	Port      string
	Namespace string
	Set       string
}

// PrepareTestContainer starts a disposable, single-node Aerospike
// Community Edition container and returns a connection config for it plus
// a cleanup function.
func PrepareTestContainer(t *testing.T) (func(), *Config) {
	t.Helper()

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ImageRepo:     "docker.mirror.hashicorp.services/aerospike/aerospike-server",
		ContainerName: "aerospikedb",
		ImageTag:      "8.1.2.4",
		Ports:         []string{"3000/tcp", "3001/tcp", "3002/tcp", "3003/tcp"},
	})
	if err != nil {
		t.Fatalf("aerospike: could not provision docker service runner: %s", err)
	}

	svc, err := runner.StartService(
		context.Background(),
		func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
			cfg := docker.NewServiceHostPort(host, port)

			time.Sleep(time.Second)
			client, aeroErr := aero.NewClient(host, port)
			if aeroErr != nil {
				return nil, aeroErr
			}
			defer client.Close()

			node, aeroErr := client.Cluster().GetRandomNode()
			if aeroErr != nil {
				return nil, aeroErr
			}

			if _, aeroErr := node.RequestInfo(aero.NewInfoPolicy(), "namespaces"); aeroErr != nil {
				return nil, aeroErr
			}

			return cfg, nil
		},
	)
	if err != nil {
		t.Fatalf("aerospike: could not start container: %s", err)
	}

	return svc.Cleanup, &Config{
		Hostname:  svc.Config.URL().Hostname(),
		Port:      svc.Config.URL().Port(),
		Namespace: "test",
		Set:       "vault",
	}
}
