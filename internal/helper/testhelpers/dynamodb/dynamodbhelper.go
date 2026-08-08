// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package dynamodb

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// Config is a docker.ServiceConfig for a DynamoDB Local instance, plus the
// (fake, for DynamoDB Local) static credentials to reach it.
type Config struct {
	docker.ServiceURL
	AccessKey string
	SecretKey string
}

var _ docker.ServiceConfig = &Config{}

// PrepareTestContainer creates a DynamoDB Local docker container. If the
// environment variable AWS_DYNAMODB_ENDPOINT is set, the tests are executed
// against the specified endpoint instead (with credentials taken from the
// standard AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY environment variables),
// and no container is launched.
func PrepareTestContainer(t *testing.T) (func(), *Config) {
	t.Helper()

	if endpoint := os.Getenv("AWS_DYNAMODB_ENDPOINT"); endpoint != "" {
		s, err := docker.NewServiceURLParse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		return func() {}, &Config{
			ServiceURL: *s,
			AccessKey:  os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretKey:  os.Getenv("AWS_SECRET_ACCESS_KEY"),
		}
	}

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ImageRepo:     "docker.mirror.hashicorp.services/cnadiminti/dynamodb-local",
		ImageTag:      "latest",
		ContainerName: "dynamodb",
		Ports:         []string{"8000/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start local DynamoDB: %s", err)
	}

	svc, err := runner.StartService(context.Background(), connectDynamoDB)
	if err != nil {
		t.Fatalf("Could not start local DynamoDB: %s", err)
	}

	return svc.Cleanup, svc.Config.(*Config)
}

func connectDynamoDB(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
	u := url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", host, port),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// DynamoDB Local responds 400 to an unauthenticated, bodyless GET; that's
	// enough to know the server is up and accepting connections.
	if resp.StatusCode != http.StatusBadRequest {
		return nil, fmt.Errorf("unexpected status from DynamoDB Local: %s", resp.Status)
	}

	return &Config{
		ServiceURL: *docker.NewServiceURL(u),
		AccessKey:  "fake",
		SecretKey:  "fake",
	}, nil
}
