// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package azure

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	azlegacy "github.com/Azure/go-autorest/autorest/azure"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/strutil"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	// MaxBlobSize at this time
	MaxBlobSize = 1024 * 1024 * 4
	// MaxListResults is the current default value, setting explicitly
	MaxListResults = 5000
)

// AzureBackend is a physical backend that stores data
// within an Azure blob container.
type AzureBackend struct {
	container  *container.Client
	logger     log.Logger
	permitPool *physical.PermitPool
}

// Verify AzureBackend satisfies the correct interfaces
var _ physical.Backend = (*AzureBackend)(nil)

// NewAzureBackend constructs an Azure backend using a pre-existing
// container. Credentials can be provided to the backend, sourced
// from the environment, via HCL, or by using managed identities.
func NewAzureBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	name := os.Getenv("AZURE_BLOB_CONTAINER")
	useMSI := false

	if name == "" {
		name = conf["container"]
		if name == "" {
			return nil, fmt.Errorf("'container' must be set")
		}
	}

	accountName := os.Getenv("AZURE_ACCOUNT_NAME")
	if accountName == "" {
		accountName = conf["accountName"]
		if accountName == "" {
			return nil, fmt.Errorf("'accountName' must be set")
		}
	}

	accountKey := os.Getenv("AZURE_ACCOUNT_KEY")
	if accountKey == "" {
		accountKey = conf["accountKey"]
		if accountKey == "" {
			logger.Info("accountKey not set, using managed identity auth")
			useMSI = true
		}
	}

	environmentName := os.Getenv("AZURE_ENVIRONMENT")
	if environmentName == "" {
		environmentName = conf["environment"]
		if environmentName == "" {
			environmentName = "AzurePublicCloud"
		}
	}

	environmentURL := os.Getenv("AZURE_ARM_ENDPOINT")
	if environmentURL == "" {
		environmentURL = conf["arm_endpoint"]
	}

	var storageEndpointSuffix string
	var containerURL string

	testHost := conf["testHost"]
	switch {
	case testHost != "":
		containerURL = fmt.Sprintf("http://%s/%s/%s", testHost, accountName, name)
	default:
		var environment azlegacy.Environment
		var err error
		if environmentURL != "" {
			environment, err = azlegacy.EnvironmentFromURL(environmentURL)
			if err != nil {
				return nil, fmt.Errorf("failed to look up Azure environment descriptor for URL %q: %w", environmentURL, err)
			}
		} else {
			environment, err = azlegacy.EnvironmentFromName(environmentName)
			if err != nil {
				return nil, fmt.Errorf("failed to look up Azure environment descriptor for name %q: %w", environmentName, err)
			}
		}
		storageEndpointSuffix = environment.StorageEndpointSuffix
		containerURL = fmt.Sprintf("https://%s.blob.%s/%s", accountName, storageEndpointSuffix, name)
	}

	var client *container.Client
	var err error
	if useMSI {
		cred, credErr := azidentity.NewManagedIdentityCredential(nil)
		if credErr != nil {
			return nil, fmt.Errorf("failed to obtain managed identity credential: %w", credErr)
		}
		client, err = container.NewClient(containerURL, cred, nil)
	} else {
		var cred *azblob.SharedKeyCredential
		cred, err = azblob.NewSharedKeyCredential(accountName, accountKey)
		if err == nil {
			client, err = container.NewClientWithSharedKeyCredential(containerURL, cred, nil)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.GetProperties(ctx, nil); err != nil {
		if bloberror.HasCode(err, bloberror.ContainerNotFound) {
			if _, err := client.Create(ctx, nil); err != nil {
				return nil, fmt.Errorf("failed to create %q container: %w", name, err)
			}
		} else {
			return nil, fmt.Errorf("failed to get properties for container %q: %w", name, err)
		}
	}

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		if logger.IsDebug() {
			logger.Debug("max_parallel set", "max_parallel", maxParInt)
		}
	}

	a := &AzureBackend{
		container:  client,
		logger:     logger,
		permitPool: physical.NewPermitPool(maxParInt),
	}
	return a, nil
}

// Put is used to insert or update an entry
func (a *AzureBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"azure", "put"}, time.Now())

	if len(entry.Value) >= MaxBlobSize {
		return fmt.Errorf("value is bigger than the current supported limit of 4MBytes")
	}

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	blobClient := a.container.NewBlockBlobClient(entry.Key)
	_, err := blobClient.UploadBuffer(ctx, entry.Value, nil)

	return err
}

// Get is used to fetch an entry
func (a *AzureBackend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"azure", "get"}, time.Now())

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	blobClient := a.container.NewBlockBlobClient(key)

	res, err := blobClient.DownloadStream(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to download blob %q: %w", key, err)
	}

	body := res.NewRetryReader(ctx, nil)
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}

	return &physical.Entry{
		Key:   key,
		Value: data,
	}, nil
}

// Delete is used to permanently delete an entry
func (a *AzureBackend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"azure", "delete"}, time.Now())

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	blobClient := a.container.NewBlockBlobClient(key)
	includeSnapshots := blob.DeleteSnapshotsOptionTypeInclude
	_, err := blobClient.Delete(ctx, &blob.DeleteOptions{
		DeleteSnapshots: &includeSnapshots,
	})
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil
		}
		return fmt.Errorf("failed to delete blob %q: %w", key, err)
	}

	return nil
}

// listAll returns the sorted set of keys under a given prefix, up to the
// next prefix; blob names are returned by Azure in lexicographic order,
// and folding "subdirectories" here preserves that order. Shared by List
// and ListPage.
func (a *AzureBackend) listAll(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"azure", "list"}, time.Now())

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	var keys []string
	maxResults := int32(MaxListResults)
	pager := a.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     &prefix,
		MaxResults: &maxResults,
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, blobInfo := range page.Segment.BlobItems {
			if blobInfo.Name == nil {
				continue
			}
			key := strings.TrimPrefix(*blobInfo.Name, prefix)
			if i := strings.Index(key, "/"); i == -1 {
				// file
				keys = append(keys, key)
			} else {
				// subdirectory
				keys = strutil.AppendIfMissing(keys, key[:i+1])
			}
		}
	}

	sort.Strings(keys)
	return keys, nil
}

// List is used to list all the keys under a given prefix, up to the next
// prefix.
func (a *AzureBackend) List(ctx context.Context, prefix string) ([]string, error) {
	return a.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, starting
// strictly after the "after" key, up to "limit" keys.
func (a *AzureBackend) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	keys, err := a.listAll(ctx, prefix)
	if err != nil {
		return nil, err
	}

	start := sort.SearchStrings(keys, after)
	for start < len(keys) && keys[start] <= after {
		start++
	}

	end := len(keys)
	if limit > 0 && start+limit < end {
		end = start + limit
	}

	return keys[start:end], nil
}
