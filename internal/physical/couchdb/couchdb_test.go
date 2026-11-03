// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package couchdb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
)

func TestCouchDBBackend(t *testing.T) {
	cleanup, config := prepareCouchDBTestContainer(t)
	defer cleanup()

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewCouchDBBackend(map[string]string{
		"endpoint": config.URL().String(),
		"username": config.username,
		"password": config.password,
	}, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}

type couchDB struct {
	baseURL  url.URL
	dbname   string
	username string
	password string
}

func (c couchDB) Address() string {
	return c.baseURL.Host
}

func (c couchDB) URL() *url.URL {
	u := c.baseURL
	u.Path = c.dbname
	return &u
}

var _ docker.ServiceConfig = &couchDB{}

func prepareCouchDBTestContainer(t *testing.T) (func(), *couchDB) {
	t.Helper()

	// If the environment variable is set, assume the caller wants to target
	// a real CouchDB.
	if os.Getenv("COUCHDB_ENDPOINT") != "" {
		return func() {}, &couchDB{
			baseURL:  url.URL{Host: os.Getenv("COUCHDB_ENDPOINT")},
			username: os.Getenv("COUCHDB_USERNAME"),
			password: os.Getenv("COUCHDB_PASSWORD"),
		}
	}

	const (
		username = "admin"
		password = "admin"
	)

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ContainerName: "couchdb",
		ImageRepo:     "docker.mirror.hashicorp.services/library/couchdb",
		ImageTag:      "3.5",
		Env: []string{
			"COUCHDB_USER=" + username,
			"COUCHDB_PASSWORD=" + password,
		},
		Ports: []string{"5984/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start local CouchDB: %s", err)
	}

	svc, err := runner.StartService(context.Background(), func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
		return setupCouchDB(ctx, host, port, username, password)
	})
	if err != nil {
		t.Fatalf("Could not start local CouchDB: %s", err)
	}

	return svc.Cleanup, svc.Config.(*couchDB)
}

func setupCouchDB(ctx context.Context, host string, port int, username, password string) (docker.ServiceConfig, error) {
	c := &couchDB{
		baseURL:  url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", host, port)},
		dbname:   fmt.Sprintf("vault-test-%d", time.Now().Unix()),
		username: username,
		password: password,
	}

	{
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("could not create readiness request: %w", err)
		}
		req.SetBasicAuth(username, password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("expected couchdb to return status code 200, got (%s) instead", resp.Status)
		}
	}

	{
		u := c.baseURL
		u.Path = c.dbname
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("could not create create-database request: %w", err)
		}
		req.SetBasicAuth(username, password)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("could not create database: %w", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusCreated {
			bs, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("failed to create database: %s %s", resp.Status, string(bs))
		}
	}

	return c, nil
}
