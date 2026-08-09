// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package azurite

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// well-known Azurite development credentials; see
// https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azurite
const (
	accountName = "testaccount"
	accountKey  = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

type Config struct {
	Endpoint    string
	AccountName string
	AccountKey  string
}

func (c Config) Address() string {
	return c.Endpoint
}

func (c Config) URL() *url.URL {
	return &url.URL{Scheme: "http", Host: c.Endpoint, Path: "/" + accountName}
}

var _ docker.ServiceConfig = &Config{}

// PrepareTestContainer creates an Azurite docker container (the official
// open source Azure Storage emulator) and a fresh blob container within
// it.
func PrepareTestContainer(t *testing.T, version string) (func(), *Config) {
	t.Helper()

	if version == "" {
		version = "latest"
	}
	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ContainerName: "azurite",
		ImageRepo:     "mcr.microsoft.com/azure-storage/azurite",
		ImageTag:      version,
		// --skipApiVersionCheck: the SDK client sends whatever storage API
		// version it was built against, which is routinely newer than
		// what's baked into the Azurite image; without this flag Azurite
		// rejects the request instead of just handling it.
		Cmd:   []string{"azurite-blob", "--blobHost", "0.0.0.0", "--blobPort", "10000", "--skipApiVersionCheck", "-d", "/dev/stderr"},
		Ports: []string{"10000/tcp"},
		Env:   []string{fmt.Sprintf("AZURITE_ACCOUNTS=%s:%s", accountName, accountKey)},
	})
	if err != nil {
		t.Fatalf("Could not start docker Azurite: %s", err)
	}

	svc, err := runner.StartService(context.Background(), connectAzurite)
	if err != nil {
		t.Fatalf("Could not start docker Azurite: %s", err)
	}

	return svc.Cleanup, svc.Config.(*Config)
}

func connectAzurite(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
	cfg := &Config{
		Endpoint:    fmt.Sprintf("%s:%d", host, port),
		AccountName: accountName,
		AccountKey:  accountKey,
	}

	cred, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return nil, err
	}

	containerURL := fmt.Sprintf("http://%s/%s/testcontainer", cfg.Endpoint, accountName)
	client, err := container.NewClientWithSharedKeyCredential(containerURL, cred, nil)
	if err != nil {
		return nil, err
	}

	if _, err := client.Create(ctx, nil); err != nil {
		return nil, err
	}

	return cfg, nil
}
