// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package fakegcsserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// PrepareTestContainer creates a fake-gcs-server docker container (the
// open source Google Cloud Storage emulator) and returns its endpoint URL.
// Set the standard STORAGE_EMULATOR_HOST environment variable to this
// URL's host before constructing a storage.Client (or this backend) to
// have the official GCS client library talk to the emulator instead of
// real GCS.
func PrepareTestContainer(t *testing.T, version string) (func(), docker.ServiceConfig) {
	t.Helper()

	if version == "" {
		version = "latest"
	}
	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ContainerName: "fake-gcs-server",
		ImageRepo:     "docker.mirror.hashicorp.services/fsouza/fake-gcs-server",
		ImageTag:      version,
		// -backend memory: the default "filesystem" backend models each
		// object as a file on disk inside the container and can spuriously
		// fail (e.g. "mkdir .../foo: not a directory") the moment a bucket
		// mixes leaf and prefix keys sharing a path segment, which physical
		// backend tests do routinely. The in-memory backend doesn't have
		// this problem and is faster besides.
		//
		// No -public-host/-external-url: Docker assigns this container's
		// host port randomly (PublishAllPorts), unknowable before the
		// container starts, so it can't be pinned up front. Leaving both
		// unset lets fake-gcs-server derive the external URL used in
		// resumable-upload Location headers from each request's own Host
		// header instead, which already reflects whatever mapped host:port
		// the client actually connected through.
		Cmd:   []string{"-scheme", "http", "-backend", "memory"},
		Ports: []string{"4443/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start docker fake-gcs-server: %s", err)
	}

	svc, err := runner.StartService(context.Background(), connectGCS)
	if err != nil {
		t.Fatalf("Could not start docker fake-gcs-server: %s", err)
	}

	return svc.Cleanup, svc.Config
}

func connectGCS(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
	u := url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", host, port),
		Path:   "storage/v1/b",
	}
	transCfg := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // ignore expired SSL certificates from the emulator
	}
	httpClient := &http.Client{Transport: transCfg}
	client, err := storage.NewClient(ctx, option.WithEndpoint(u.String()), option.WithHTTPClient(httpClient), option.WithoutAuthentication())
	if err != nil {
		return nil, err
	}
	defer client.Close()

	it := client.Buckets(ctx, "test")
	for {
		if _, err := it.Next(); err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
	}

	return docker.NewServiceURL(u), nil
}
