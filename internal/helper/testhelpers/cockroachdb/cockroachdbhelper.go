// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cockroachdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// PrepareTestContainer creates a CockroachDB docker container (running
// insecure, i.e. without TLS or authentication -- suitable for tests only)
// and a fresh database within it. If the environment variable CR_URL is
// set, the tests are executed against the specified connection URL
// instead, and no container is launched.
func PrepareTestContainer(t *testing.T) (func(), string) {
	t.Helper()

	if retURL := os.Getenv("CR_URL"); retURL != "" {
		return func() {}, retURL
	}

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ImageRepo:     "docker.mirror.hashicorp.services/cockroachdb/cockroach",
		ImageTag:      "latest-v24.1",
		ContainerName: "cockroachdb",
		Cmd:           []string{"start-single-node", "--insecure"},
		Ports:         []string{"26257/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start docker CockroachDB: %s", err)
	}

	svc, err := runner.StartService(context.Background(), connectCockroachDB)
	if err != nil {
		t.Fatalf("Could not start docker CockroachDB: %s", err)
	}

	return svc.Cleanup, svc.Config.URL().String()
}

func connectCockroachDB(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword("root", ""),
		Host:     fmt.Sprintf("%s:%d", host, port),
		Path:     "/openbao",
		RawQuery: "sslmode=disable",
	}

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS openbao"); err != nil {
		return nil, err
	}

	return docker.NewServiceURL(u), nil
}
