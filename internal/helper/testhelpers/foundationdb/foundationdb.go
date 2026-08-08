// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package foundationdb provides a disposable, single-node FoundationDB
// cluster for tests. Unlike the other storage backend test helpers in this
// tree, it does not link against the native FDB client library itself:
// setup and readiness checks are done by exec'ing the fdbcli binary inside
// the container, so this package builds without the "foundationdb" Go
// build tag. Only the caller (internal/physical/foundationdb, built with
// -tags foundationdb) needs the native client library to actually talk to
// the cluster this helper stands up.
//
// FoundationDB does not publish a ready-to-run server image, so this helper
// builds one on the fly from the official *.deb release assets for the
// requested version (packaging/docker in the upstream apple/foundationdb
// repository notes that its own sample compose/k8s files are unmaintained,
// hence building from the client/server debs directly here).
package foundationdb

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-uuid"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// DefaultVersion is the FoundationDB release used to build the test
// container when none is requested explicitly.
const DefaultVersion = "7.3.77"

// MinAPIVersion is the minimum FDB API version supported by the backend
// (see internal/physical/foundationdb.minAPIVersion); tests default to it.
const MinAPIVersion = "520"

const containerfile = `
FROM debian:bookworm-slim

ARG FDB_VERSION
ARG FDB_ARCH

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates wget python3 && \
    wget -q -O /tmp/fdb-clients.deb "https://github.com/apple/foundationdb/releases/download/${FDB_VERSION}/foundationdb-clients_${FDB_VERSION}-1_${FDB_ARCH}.deb" && \
    wget -q -O /tmp/fdb-server.deb "https://github.com/apple/foundationdb/releases/download/${FDB_VERSION}/foundationdb-server_${FDB_VERSION}-1_${FDB_ARCH}.deb" && \
    (dpkg -i /tmp/fdb-clients.deb || apt-get install -f -y) && \
    (dpkg -i /tmp/fdb-server.deb || apt-get install -f -y) && \
    rm -f /tmp/*.deb && \
    rm -rf /var/lib/apt/lists/*

EXPOSE 4500
CMD ["/bin/bash", "-c", "/etc/init.d/foundationdb start && exec tail -f /dev/null"]
`

// debArch maps a Go GOARCH to the architecture suffix used in
// apple/foundationdb's release asset filenames.
func debArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("foundationdb: no known release asset for GOARCH %q", runtime.GOARCH)
	}
}

// rewriteClusterFile rewrites the ip:port suffix of a cluster file's
// connection string to point at the given host/port, preserving the
// server-generated description:id prefix.
func rewriteClusterFile(contents, hostPort string) (string, error) {
	contents = strings.TrimSpace(contents)
	idx := strings.LastIndex(contents, "@")
	if idx < 0 {
		return "", fmt.Errorf("foundationdb: unexpected cluster file contents: %q", contents)
	}

	return contents[:idx+1] + hostPort + "\n", nil
}

// PrepareTestContainer starts a disposable, single-node FoundationDB
// cluster in Docker and returns the path to a locally-written cluster file
// pointing at it, plus a cleanup function. It fails the test if Docker is
// unavailable or the cluster does not become healthy within a couple of
// minutes.
func PrepareTestContainer(t *testing.T) (func(), string) {
	t.Helper()

	arch, err := debArch()
	if err != nil {
		t.Fatalf("foundationdb: %s", err)
	}

	suffix, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatalf("foundationdb: could not generate unique suffix: %s", err)
	}
	imageName := "openbao_foundationdb_test"
	imageTag := suffix

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ImageRepo:     imageName,
		ImageTag:      imageTag,
		ContainerName: "foundationdb",
		Ports:         []string{"4500/tcp"},
		LogConsumer: func(s string) {
			if t.Failed() {
				t.Logf("foundationdb container logs: %s", s)
			}
		},
	})
	if err != nil {
		t.Fatalf("foundationdb: could not provision docker service runner: %s", err)
	}

	ctx := context.Background()
	bCtx := docker.NewBuildContext()
	fdbVersion, fdbArch := DefaultVersion, arch
	if _, err := runner.BuildImage(
		ctx, containerfile, bCtx,
		docker.BuildRemove(true), docker.BuildForceRemove(true),
		docker.BuildArgs(map[string]*string{
			"FDB_VERSION": &fdbVersion,
			"FDB_ARCH":    &fdbArch,
		}),
		docker.BuildTags([]string{imageName + ":" + imageTag}),
	); err != nil {
		t.Fatalf("foundationdb: could not build container image: %s", err)
	}

	var clusterFileContents string
	svc, err := runner.StartService(ctx, func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
		return docker.NewServiceHostPort(host, port), nil
	})
	if err != nil {
		t.Fatalf("foundationdb: could not start container: %s", err)
	}

	// Poll readiness by exec'ing fdbcli inside the container; the database
	// is pre-configured (single-process, memory storage engine) as part of
	// the client/server .deb installation during the image build above.
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		stdout, _, code, err := runner.RunCmdWithOutput(ctx, svc.Container.ID, []string{"fdbcli", "--exec", "status minimal"})
		if err == nil && code == 0 && strings.Contains(string(stdout), "The database is available") {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("status: %v, output: %s, err: %w", code, stdout, err)
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		svc.Cleanup()
		t.Fatalf("foundationdb: cluster did not become healthy in time: %s", lastErr)
	}

	stdout, _, _, err := runner.RunCmdWithOutput(ctx, svc.Container.ID, []string{"cat", "/etc/foundationdb/fdb.cluster"})
	if err != nil {
		svc.Cleanup()
		t.Fatalf("foundationdb: could not read cluster file from container: %s", err)
	}

	rewritten, err := rewriteClusterFile(string(stdout), svc.Config.Address())
	if err != nil {
		svc.Cleanup()
		t.Fatalf("foundationdb: %s", err)
	}
	clusterFileContents = rewritten

	tmpFile, err := os.CreateTemp("", "fdb-test-cluster-file")
	if err != nil {
		svc.Cleanup()
		t.Fatalf("foundationdb: could not create local cluster file: %s", err)
	}
	if _, err := tmpFile.WriteString(clusterFileContents); err != nil {
		tmpFile.Close()
		svc.Cleanup()
		t.Fatalf("foundationdb: could not write local cluster file: %s", err)
	}
	tmpFile.Close()

	cleanup := func() {
		svc.Cleanup()
		os.Remove(tmpFile.Name())
	}

	return cleanup, tmpFile.Name()
}
