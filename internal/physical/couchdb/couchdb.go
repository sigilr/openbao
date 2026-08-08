// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package couchdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	log "github.com/hashicorp/go-hclog"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// CouchDBBackend allows the management of couchdb users
type CouchDBBackend struct {
	logger     log.Logger
	client     *couchDBClient
	permitPool *physical.PermitPool
}

// Verify CouchDBBackend satisfies the correct interfaces
var _ physical.Backend = (*CouchDBBackend)(nil)

type couchDBClient struct {
	endpoint string
	username string
	password string
	*http.Client
}

type couchDBListItem struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Value struct {
		Revision string
	} `json:"value"`
}

type couchDBList struct {
	TotalRows int               `json:"total_rows"`
	Offset    int               `json:"offset"`
	Rows      []couchDBListItem `json:"rows"`
}

func (m *couchDBClient) rev(ctx context.Context, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, fmt.Sprintf("%s/%s", m.endpoint, key), nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(m.username, m.password)

	resp, err := m.Client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil
	}
	etag := resp.Header.Get("Etag")
	if len(etag) < 2 {
		return "", nil
	}
	return etag[1 : len(etag)-1], nil
}

func (m *couchDBClient) put(ctx context.Context, e couchDBEntry) error {
	bs, err := json.Marshal(e)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/%s", m.endpoint, e.ID), bytes.NewReader(bs))
	if err != nil {
		return err
	}
	req.SetBasicAuth(m.username, m.password)
	resp, err := m.Client.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	return err
}

func (m *couchDBClient) get(ctx context.Context, key string) (*physical.Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/%s", m.endpoint, url.PathEscape(key)), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(m.username, m.password)
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	} else if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned %q", resp.Status)
	}
	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	entry := couchDBEntry{}
	if err := json.Unmarshal(bs, &entry); err != nil {
		return nil, err
	}
	return entry.Entry, nil
}

func (m *couchDBClient) list(ctx context.Context, prefix string) ([]couchDBListItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/_all_docs", m.endpoint), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(m.username, m.password)
	values := req.URL.Query()
	values.Set("skip", "0")
	values.Set("include_docs", "false")
	if prefix != "" {
		values.Set("startkey", fmt.Sprintf("%q", prefix))
		values.Set("endkey", fmt.Sprintf("%q", prefix+"{}"))
	}
	req.URL.RawQuery = values.Encode()

	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	results := couchDBList{}
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, err
	}

	return results.Rows, nil
}

func buildCouchDBBackend(conf map[string]string, logger log.Logger) (*CouchDBBackend, error) {
	endpoint := os.Getenv("COUCHDB_ENDPOINT")
	if endpoint == "" {
		endpoint = conf["endpoint"]
	}
	if endpoint == "" {
		return nil, fmt.Errorf("missing endpoint")
	}

	username := os.Getenv("COUCHDB_USERNAME")
	if username == "" {
		username = conf["username"]
	}

	password := os.Getenv("COUCHDB_PASSWORD")
	if password == "" {
		password = conf["password"]
	}

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	var err error
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		if logger.IsDebug() {
			logger.Debug("max_parallel set", "max_parallel", maxParInt)
		}
	}

	return &CouchDBBackend{
		client: &couchDBClient{
			endpoint: endpoint,
			username: username,
			password: password,
			Client:   cleanhttp.DefaultPooledClient(),
		},
		logger:     logger,
		permitPool: physical.NewPermitPool(maxParInt),
	}, nil
}

// NewCouchDBBackend constructs a CouchDB backend using the given API client
// and endpoint.
//
// CouchDB has no native multi-document transaction or lock primitive, so
// unlike postgresql/mysql this backend implements only physical.Backend:
// no physical.TransactionalBackend (there is nothing to commit or roll
// back across documents) and no physical.HABackend.
func NewCouchDBBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	return buildCouchDBBackend(conf, logger)
}

type couchDBEntry struct {
	Entry   *physical.Entry `json:"entry"`
	Rev     string          `json:"_rev,omitempty"`
	ID      string          `json:"_id"`
	Deleted *bool           `json:"_deleted,omitempty"`
}

// Put is used to insert or update an entry
func (m *CouchDBBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"couchdb", "put"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	revision, _ := m.client.rev(ctx, url.PathEscape(entry.Key))

	return m.client.put(ctx, couchDBEntry{
		Entry: entry,
		Rev:   revision,
		ID:    url.PathEscape(entry.Key),
	})
}

// Get is used to fetch an entry
func (m *CouchDBBackend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"couchdb", "get"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	return m.client.get(ctx, key)
}

// Delete is used to permanently delete an entry
func (m *CouchDBBackend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"couchdb", "delete"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	revision, _ := m.client.rev(ctx, url.PathEscape(key))
	deleted := true
	return m.client.put(ctx, couchDBEntry{
		ID:      url.PathEscape(key),
		Rev:     revision,
		Deleted: &deleted,
	})
}

// listAll returns every key stored directly under prefix (one path
// component, with a trailing slash for "folders"). CouchDB's _all_docs
// view returns rows sorted by document ID, so the folded result is already
// sorted; listAll is shared by List and ListPage.
func (m *CouchDBBackend) listAll(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"couchdb", "list"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	items, err := m.client.list(ctx, prefix)
	if err != nil {
		return nil, err
	}

	var out []string
	seen := make(map[string]struct{})
	for _, result := range items {
		trimmed := strings.TrimPrefix(result.ID, prefix)
		sep := strings.Index(trimmed, "/")
		if sep == -1 {
			out = append(out, trimmed)
			continue
		}
		trimmed = trimmed[:sep+1]
		if _, ok := seen[trimmed]; !ok {
			out = append(out, trimmed)
			seen[trimmed] = struct{}{}
		}
	}
	return out, nil
}

// List is used to list all the keys under a given prefix
func (m *CouchDBBackend) List(ctx context.Context, prefix string) ([]string, error) {
	return m.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, starting
// strictly after the "after" key, up to "limit" keys.
func (m *CouchDBBackend) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	keys, err := m.listAll(ctx, prefix)
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
