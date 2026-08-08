// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// Config carries the endpoint and (fake, for LocalStack) static credentials
// needed to reach a test S3 instance.
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
}

// PrepareTestContainer creates a LocalStack docker container with the S3
// service enabled. If the environment variable AWS_S3_ENDPOINT is set, the
// tests are executed against the specified endpoint instead (with
// credentials taken from the standard AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY environment variables), and no container is
// launched.
func PrepareTestContainer(t *testing.T) (func(), *Config) {
	t.Helper()

	if endpoint := os.Getenv("AWS_S3_ENDPOINT"); endpoint != "" {
		return func() {}, &Config{
			Endpoint:  endpoint,
			AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		}
	}

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ContainerName: "localstack",
		ImageRepo:     "docker.mirror.hashicorp.services/localstack/localstack",
		ImageTag:      "3",
		Env:           []string{"SERVICES=s3"},
		Ports:         []string{"4566/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start local S3 (LocalStack): %s", err)
	}

	svc, err := runner.StartService(context.Background(), func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
		hp := docker.NewServiceHostPort(host, port)

		// LocalStack's edge proxy accepts TCP connections before its
		// internal services have actually finished starting up; poll the
		// health endpoint until S3 itself reports ready.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s/_localstack/health", hp.Address()), nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var health struct {
			Services map[string]string `json:"services"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			return nil, err
		}
		if status := health.Services["s3"]; status != "available" && status != "running" {
			return nil, fmt.Errorf("s3 service not ready yet (status: %q)", status)
		}

		return hp, nil
	})
	if err != nil {
		t.Fatalf("Could not start local S3 (LocalStack): %s", err)
	}

	hp := svc.Config.(*docker.ServiceHostPort)
	return svc.Cleanup, &Config{
		Endpoint:  fmt.Sprintf("http://%s", hp.Address()),
		AccessKey: "fake",
		SecretKey: "fake",
	}
}
