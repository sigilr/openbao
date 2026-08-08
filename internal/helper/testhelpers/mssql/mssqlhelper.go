// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

const (
	// Password satisfies the SQL Server "strong password" policy: upper +
	// lower case, a digit, a symbol, and 8+ characters.
	Password = "yourStrong(!)Password"
	Username = "sa"
)

// This constant is used when retrying the mssql container restart, since
// the container intermittently starts before mssql within it is actually
// reachable.
const numRetries = 3

// PrepareTestContainer creates a Microsoft SQL Server docker container. If
// the environment variables MSSQL_HOST and MSSQL_PORT are set, the tests
// are executed against the specified server instead, and no container is
// launched.
//
// The upstream mcr.microsoft.com/mssql/server image is proprietary and
// EULA-licensed (accepted here via ACCEPT_EULA=Y), unlike the open source
// database engines used by the other physical backends' test fixtures.
// That licensing applies only to this test container, not to the client
// driver (github.com/microsoft/go-mssqldb, MIT) or to the backend code
// itself.
func PrepareTestContainer(t *testing.T) (cleanup func(), host string, port int) {
	t.Helper()

	if h := os.Getenv("MSSQL_HOST"); h != "" {
		p, err := strconv.Atoi(os.Getenv("MSSQL_PORT"))
		if err != nil {
			t.Fatalf("invalid MSSQL_PORT: %s", err)
		}
		return func() {}, h, p
	}

	var lastErr error
	for range numRetries {
		runner, err := docker.NewServiceRunner(docker.RunOptions{
			ContainerName: "sqlserver",
			ImageRepo:     "mcr.microsoft.com/mssql/server",
			ImageTag:      "2022-latest",
			Env:           []string{"ACCEPT_EULA=Y", "SA_PASSWORD=" + Password},
			Ports:         []string{"1433/tcp"},
			LogConsumer: func(s string) {
				if t.Failed() {
					t.Logf("container logs: %s", s)
				}
			},
		})
		if err != nil {
			t.Fatalf("Could not start docker MSSQL: %s", err)
		}

		var gotHost string
		var gotPort int
		svc, err := runner.StartService(context.Background(), func(ctx context.Context, h string, p int) (docker.ServiceConfig, error) {
			cfg, err := connectMSSQL(ctx, h, p)
			if err != nil {
				return nil, err
			}
			gotHost, gotPort = h, p
			return cfg, nil
		})
		if err == nil {
			return svc.Cleanup, gotHost, gotPort
		}
		lastErr = err
	}

	t.Fatalf("Could not start docker MSSQL: %s", lastErr)
	return nil, "", 0
}

func connectMSSQL(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
	connectionString := fmt.Sprintf("server=%s;port=%d;user id=%s;password=%s;connection timeout=30", host, port, Username, Password)

	db, err := sql.Open("mssql", connectionString)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	return docker.NewServiceHostPort(host, port), nil
}
