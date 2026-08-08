// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package gcs

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	log "github.com/hashicorp/go-hclog"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	fakegcsserver "github.com/openbao/openbao/v2/internal/helper/testhelpers/fakegcsserver"
)

// testClient constructs a storage.Client. If STORAGE_EMULATOR_HOST is set
// in the environment (which is what points the backend under test at the
// emulator too), it connects unauthenticated against the emulator instead
// of real GCS.
func testClient(t *testing.T, ctx context.Context) *storage.Client {
	t.Helper()

	var opts []option.ClientOption
	if os.Getenv("STORAGE_EMULATOR_HOST") != "" {
		opts = append(opts, option.WithoutAuthentication())
	}

	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testCleanup(t testing.TB, client *storage.Client, bucket string) {
	t.Helper()

	ctx := context.Background()
	b := client.Bucket(bucket)

	// Objects must be deleted before a bucket can be deleted.
	it := b.Objects(ctx, nil)
	for {
		attrs, err := it.Next()
		if err != nil {
			if !errors.Is(err, iterator.Done) {
				t.Logf("failed to list objects for cleanup: %v", err)
			}
			break
		}
		if err := b.Object(attrs.Name).Delete(ctx); err != nil {
			t.Logf("failed to delete object %q: %v", attrs.Name, err)
		}
	}

	if err := b.Delete(ctx); err != nil {
		t.Logf("failed to delete bucket: %v", err)
	}
}

// testFixture returns a backend under test and a cleanup function. It runs
// against a fake-gcs-server container unless GOOGLE_PROJECT_ID (a real GCP
// project) is set in the environment.
func testFixture(t *testing.T) (physical.Backend, func()) {
	t.Helper()

	r := rand.New(rand.NewSource(time.Now().UnixNano())).Int()
	bucket := fmt.Sprintf("openbao-gcs-testacc-%d", r)

	ctx := context.Background()
	projectID := os.Getenv("GOOGLE_PROJECT_ID")

	dockerCleanup := func() {}
	if projectID == "" {
		projectID = "test-project"
		cleanup, svcConfig := fakegcsserver.PrepareTestContainer(t, "")
		dockerCleanup = cleanup
		t.Setenv("STORAGE_EMULATOR_HOST", svcConfig.Address())
	}

	client := testClient(t, ctx)
	defer client.Close()

	testCleanup(t, client, bucket)
	if err := client.Bucket(bucket).Create(ctx, projectID, nil); err != nil {
		dockerCleanup()
		t.Fatal(err)
	}

	backend, err := NewBackend(map[string]string{
		"bucket":     bucket,
		"ha_enabled": "false",
	}, logging.NewVaultLogger(log.Trace))
	if err != nil {
		testCleanup(t, client, bucket)
		dockerCleanup()
		t.Fatal(err)
	}

	// Verify chunkSize is set correctly on the Backend
	be := backend.(*Backend)
	expectedChunkSize, err := strconv.Atoi(defaultChunkSize)
	if err != nil {
		t.Fatalf("failed to convert defaultChunkSize to int: %s", err)
	}
	expectedChunkSize *= 1024
	if be.chunkSize != expectedChunkSize {
		t.Fatalf("expected chunkSize to be %d. got=%d", expectedChunkSize, be.chunkSize)
	}

	return backend, func() {
		testCleanup(t, client, bucket)
		dockerCleanup()
	}
}

func TestBackend(t *testing.T) {
	backend, cleanup := testFixture(t)
	defer cleanup()

	physical.ExerciseBackend(t, backend)
	physical.ExerciseBackend_ListPrefix(t, backend)
}
