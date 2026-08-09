// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package spanneremulator provides a dockertest fixture wrapping the
// official, open source Cloud Spanner emulator
// (gcr.io/cloud-spanner-emulator/emulator), plus the instance/database/DDL
// bootstrapping every test needs before it can start reading and writing
// rows.
package spanneremulator

import (
	"context"
	"fmt"
	"os"
	"testing"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

const (
	projectID  = "test-project"
	instanceID = "test-instance"
	databaseID = "test-database"
)

// DatabaseName is the fully qualified name of the database the emulator is
// seeded with; pass this as the backend's "database" config value.
const DatabaseName = "projects/" + projectID + "/instances/" + instanceID + "/databases/" + databaseID

// PrepareTestContainer starts a Cloud Spanner emulator container, then
// creates an instance and a database seeded with the given DDL statements
// (typically one or more CREATE TABLE statements). If the environment
// variable SPANNER_EMULATOR_HOST is already set, no container is launched
// and the existing emulator is reused (with the instance/database created
// against it if they don't already exist).
func PrepareTestContainer(t *testing.T, ddl ...string) func() {
	t.Helper()

	cleanup := func() {}

	if os.Getenv("SPANNER_EMULATOR_HOST") == "" {
		runner, err := docker.NewServiceRunner(docker.RunOptions{
			ContainerName: "cloud-spanner-emulator",
			ImageRepo:     "gcr.io/cloud-spanner-emulator/emulator",
			ImageTag:      "latest",
			Ports:         []string{"9010/tcp"},
		})
		if err != nil {
			t.Fatalf("Could not start docker cloud-spanner-emulator: %s", err)
		}

		svc, err := runner.StartService(context.Background(), func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
			hp := docker.NewServiceHostPort(host, port)

			// The container's gRPC port accepts TCP connections before the
			// emulator has actually finished starting up; probe it with a
			// real admin call (any response, even a well-formed error like
			// NotFound, proves the server is actually serving) so
			// StartService's own retry/backoff can do its job.
			//
			// t.Setenv (rather than os.Setenv) so this is automatically
			// unset once this test finishes -- otherwise it would leak into
			// (and be misread as a pre-existing external emulator by) any
			// later test in the same process, pointing at a container that
			// no longer exists.
			t.Setenv("SPANNER_EMULATOR_HOST", hp.Address())
			instAdmin, err := instance.NewInstanceAdminClient(ctx)
			if err != nil {
				return nil, err
			}
			defer instAdmin.Close() //nolint:errcheck
			_, err = instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{
				Name: "projects/" + projectID + "/instances/readiness-probe",
			})
			if err != nil && status.Code(err) != codes.NotFound {
				return nil, fmt.Errorf("emulator not ready yet: %w", err)
			}

			return hp, nil
		})
		if err != nil {
			t.Fatalf("Could not start docker cloud-spanner-emulator: %s", err)
		}
		cleanup = svc.Cleanup
	}

	if err := setupInstanceAndDatabase(ddl); err != nil {
		cleanup()
		t.Fatalf("Could not set up cloud-spanner-emulator instance/database: %s", err)
	}

	return cleanup
}

func setupInstanceAndDatabase(ddl []string) error {
	ctx := context.Background()

	instAdmin, err := instance.NewInstanceAdminClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create instance admin client: %w", err)
	}
	defer instAdmin.Close() //nolint:errcheck

	instOp, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent:     "projects/" + projectID,
		InstanceId: instanceID,
		Instance: &instancepb.Instance{
			Config:      "projects/" + projectID + "/instanceConfigs/emulator-config",
			DisplayName: instanceID,
			NodeCount:   1,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
	}
	if _, err := instOp.Wait(ctx); err != nil {
		return fmt.Errorf("failed to wait for instance creation: %w", err)
	}

	dbAdmin, err := database.NewDatabaseAdminClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create database admin client: %w", err)
	}
	defer dbAdmin.Close() //nolint:errcheck

	dbOp, err := dbAdmin.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent:          "projects/" + projectID + "/instances/" + instanceID,
		CreateStatement: "CREATE DATABASE `" + databaseID + "`",
		ExtraStatements: ddl,
	})
	if err != nil {
		return fmt.Errorf("failed to create database: %w", err)
	}
	if _, err := dbOp.Wait(ctx); err != nil {
		return fmt.Errorf("failed to wait for database creation: %w", err)
	}

	return nil
}
