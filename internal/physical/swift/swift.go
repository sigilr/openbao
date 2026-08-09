// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package swift

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	log "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/ncw/swift/v2"

	"github.com/openbao/openbao/sdk/v2/physical"
)

// Verify SwiftBackend satisfies the correct interfaces
var _ physical.Backend = (*SwiftBackend)(nil)

// SwiftBackend is a physical backend that stores data
// within an OpenStack Swift container.
type SwiftBackend struct {
	container  string
	client     *swift.Connection
	logger     log.Logger
	permitPool *physical.PermitPool
}

// NewSwiftBackend constructs a Swift backend using a pre-existing
// container. Credentials can be provided to the backend, sourced
// from the environment.
func NewSwiftBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	var ok bool

	username := os.Getenv("OS_USERNAME")
	if username == "" {
		username = conf["username"]
		if username == "" {
			return nil, fmt.Errorf("missing username")
		}
	}
	password := os.Getenv("OS_PASSWORD")
	if password == "" {
		password = conf["password"]
		if password == "" {
			return nil, fmt.Errorf("missing password")
		}
	}
	authURL := os.Getenv("OS_AUTH_URL")
	if authURL == "" {
		authURL = conf["auth_url"]
		if authURL == "" {
			return nil, fmt.Errorf("missing auth_url")
		}
	}
	container := os.Getenv("OS_CONTAINER")
	if container == "" {
		container = conf["container"]
		if container == "" {
			return nil, fmt.Errorf("missing container")
		}
	}
	project := os.Getenv("OS_PROJECT_NAME")
	if project == "" {
		if project, ok = conf["project"]; !ok {
			// Check for KeyStone naming prior to V3
			project = os.Getenv("OS_TENANT_NAME")
			if project == "" {
				project = conf["tenant"]
			}
		}
	}

	domain := os.Getenv("OS_USER_DOMAIN_NAME")
	if domain == "" {
		domain = conf["domain"]
	}
	projectDomain := os.Getenv("OS_PROJECT_DOMAIN_NAME")
	if projectDomain == "" {
		projectDomain = conf["project-domain"]
	}

	region := os.Getenv("OS_REGION_NAME")
	if region == "" {
		region = conf["region"]
	}
	tenantID := os.Getenv("OS_TENANT_ID")
	if tenantID == "" {
		tenantID = conf["tenant_id"]
	}
	trustID := os.Getenv("OS_TRUST_ID")
	if trustID == "" {
		trustID = conf["trust_id"]
	}
	storageURL := os.Getenv("OS_STORAGE_URL")
	if storageURL == "" {
		storageURL = conf["storage_url"]
	}
	authToken := os.Getenv("OS_AUTH_TOKEN")
	if authToken == "" {
		authToken = conf["auth_token"]
	}

	c := swift.Connection{
		Domain:       domain,
		UserName:     username,
		ApiKey:       password,
		AuthUrl:      authURL,
		Tenant:       project,
		TenantDomain: projectDomain,
		Region:       region,
		TenantId:     tenantID,
		TrustId:      trustID,
		StorageUrl:   storageURL,
		AuthToken:    authToken,
		Transport:    cleanhttp.DefaultPooledTransport(),
	}

	ctx := context.Background()

	if err := c.Authenticate(ctx); err != nil {
		return nil, err
	}

	if _, _, err := c.Container(ctx, container); err != nil {
		return nil, fmt.Errorf("unable to access container %q: %w", container, err)
	}

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	if ok {
		maxPar, err := strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		maxParInt = maxPar
		logger.Debug("max_parallel set", "max_parallel", maxParInt)
	}

	s := &SwiftBackend{
		client:     &c,
		container:  container,
		logger:     logger,
		permitPool: physical.NewPermitPool(maxParInt),
	}
	return s, nil
}

// Put is used to insert or update an entry
func (s *SwiftBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"swift", "put"}, time.Now())

	s.permitPool.Acquire()
	defer s.permitPool.Release()

	return s.client.ObjectPutBytes(ctx, s.container, entry.Key, entry.Value, "")
}

// Get is used to fetch an entry
func (s *SwiftBackend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"swift", "get"}, time.Now())

	s.permitPool.Acquire()
	defer s.permitPool.Release()

	// Do a list of names with the key first since eventual consistency means
	// it might be deleted, but a node might return a read of bytes which fails
	// the physical test
	list, err := s.client.ObjectNames(ctx, s.container, &swift.ObjectsOpts{Prefix: key})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}

	data, err := s.client.ObjectGetBytes(ctx, s.container, key)
	if errors.Is(err, swift.ObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &physical.Entry{
		Key:   key,
		Value: data,
	}, nil
}

// Delete is used to permanently delete an entry
func (s *SwiftBackend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"swift", "delete"}, time.Now())

	s.permitPool.Acquire()
	defer s.permitPool.Release()

	err := s.client.ObjectDelete(ctx, s.container, key)
	if err != nil && !errors.Is(err, swift.ObjectNotFound) {
		return err
	}

	return nil
}

// listAll returns the sorted, deduplicated set of "directory" entries
// directly under prefix.
func (s *SwiftBackend) listAll(ctx context.Context, prefix string) ([]string, error) {
	list, err := s.client.ObjectNamesAll(ctx, s.container, &swift.ObjectsOpts{Prefix: prefix})
	if err != nil {
		return nil, err
	}

	keys := []string{}
	for _, key := range list {
		key := strings.TrimPrefix(key, prefix)

		if i := strings.Index(key, "/"); i == -1 {
			// Add objects only from the current 'folder'
			keys = append(keys, key)
		} else {
			// Add truncated 'folder' paths
			keys = strutil.AppendIfMissing(keys, key[:i+1])
		}
	}

	sort.Strings(keys)

	return keys, nil
}

// List is used to list all the keys under a given
// prefix, up to the next prefix.
func (s *SwiftBackend) List(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"swift", "list"}, time.Now())

	return s.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, in sorted
// order, starting after the given key.
func (s *SwiftBackend) ListPage(ctx context.Context, prefix string, after string, limit int) ([]string, error) {
	defer metrics.MeasureSince([]string{"swift", "list_page"}, time.Now())

	keys, err := s.listAll(ctx, prefix)
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
