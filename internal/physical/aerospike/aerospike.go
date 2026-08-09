// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package aerospike

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	aero "github.com/aerospike/aerospike-client-go/v8"
	aerotypes "github.com/aerospike/aerospike-client-go/v8/types"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/strutil"

	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	keyBin   = "keyBin"
	valueBin = "valueBin"

	defaultNamespace = "test"

	defaultHostname = "127.0.0.1"
	defaultPort     = 3000
)

// AerospikeBackend is a physical backend that stores data in Aerospike.
type AerospikeBackend struct {
	client    *aero.Client
	namespace string
	set       string
	logger    log.Logger
}

// Verify AerospikeBackend satisfies the correct interface.
var _ physical.Backend = (*AerospikeBackend)(nil)

// NewAerospikeBackend constructs an AerospikeBackend backend.
func NewAerospikeBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	namespace, ok := conf["namespace"]
	if !ok {
		namespace = defaultNamespace
	}
	set := conf["set"]

	policy, err := buildClientPolicy(conf)
	if err != nil {
		return nil, err
	}

	client, err := buildAerospikeClient(conf, policy)
	if err != nil {
		return nil, err
	}

	return &AerospikeBackend{
		client:    client,
		namespace: namespace,
		set:       set,
		logger:    logger,
	}, nil
}

func buildAerospikeClient(conf map[string]string, policy *aero.ClientPolicy) (*aero.Client, error) {
	hostListString, ok := conf["hostlist"]
	if !ok || hostListString == "" {
		hostname, ok := conf["hostname"]
		if !ok || hostname == "" {
			hostname = defaultHostname
		}

		portString, ok := conf["port"]
		if !ok || portString == "" {
			portString = strconv.Itoa(defaultPort)
		}

		port, err := strconv.Atoi(portString)
		if err != nil {
			return nil, err
		}

		client, aeroErr := aero.NewClientWithPolicy(policy, hostname, port)
		if aeroErr != nil {
			return nil, aeroErr
		}
		return client, nil
	}

	hostList, err := parseHostList(hostListString)
	if err != nil {
		return nil, err
	}

	client, aeroErr := aero.NewClientWithPolicyAndHost(policy, hostList...)
	if aeroErr != nil {
		return nil, aeroErr
	}
	return client, nil
}

func buildClientPolicy(conf map[string]string) (*aero.ClientPolicy, error) {
	policy := aero.NewClientPolicy()

	policy.User = conf["username"]
	policy.Password = conf["password"]

	authMode := aero.AuthModeInternal
	if mode, ok := conf["auth_mode"]; ok {
		switch strings.ToUpper(mode) {
		case "EXTERNAL":
			authMode = aero.AuthModeExternal
		case "INTERNAL":
			authMode = aero.AuthModeInternal
		default:
			return nil, fmt.Errorf("'auth_mode' must be one of {INTERNAL, EXTERNAL}")
		}
	}
	policy.AuthMode = authMode
	policy.ClusterName = conf["cluster_name"]

	if timeoutString, ok := conf["timeout"]; ok {
		timeout, err := strconv.Atoi(timeoutString)
		if err != nil {
			return nil, err
		}
		policy.Timeout = time.Duration(timeout) * time.Millisecond
	}

	if idleTimeoutString, ok := conf["idle_timeout"]; ok {
		idleTimeout, err := strconv.Atoi(idleTimeoutString)
		if err != nil {
			return nil, err
		}
		policy.IdleTimeout = time.Duration(idleTimeout) * time.Millisecond
	}

	return policy, nil
}

func (a *AerospikeBackend) key(userKey string) (*aero.Key, error) {
	key, err := aero.NewKey(a.namespace, a.set, objectKeyDigest(userKey))
	if err != nil {
		return nil, err
	}
	return key, nil
}

// Put is used to insert or update an entry.
func (a *AerospikeBackend) Put(_ context.Context, entry *physical.Entry) error {
	aeroKey, err := a.key(entry.Key)
	if err != nil {
		return err
	}

	// replace the Aerospike record if exists
	writePolicy := aero.NewWritePolicy(0, 0)
	writePolicy.RecordExistsAction = aero.REPLACE

	binMap := make(aero.BinMap, 2)
	binMap[keyBin] = entry.Key
	binMap[valueBin] = entry.Value

	if err := a.client.Put(writePolicy, aeroKey, binMap); err != nil {
		return err
	}
	return nil
}

// Get is used to fetch an entry.
func (a *AerospikeBackend) Get(_ context.Context, key string) (*physical.Entry, error) {
	aeroKey, err := a.key(key)
	if err != nil {
		return nil, err
	}

	record, aeroErr := a.client.Get(nil, aeroKey)
	if aeroErr != nil {
		if aeroErr.Matches(aerotypes.KEY_NOT_FOUND_ERROR) {
			return nil, nil
		}
		return nil, aeroErr
	}

	value, ok := record.Bins[valueBin]
	if !ok {
		return nil, fmt.Errorf("value bin was not found in the record")
	}

	return &physical.Entry{
		Key:   key,
		Value: value.([]byte),
	}, nil
}

// Delete is used to permanently delete an entry.
func (a *AerospikeBackend) Delete(_ context.Context, key string) error {
	aeroKey, err := a.key(key)
	if err != nil {
		return err
	}

	if _, err := a.client.Delete(nil, aeroKey); err != nil {
		return err
	}
	return nil
}

// listAll scans the whole set (Aerospike has no notion of an ordered key
// range we could otherwise restrict the scan to) and returns the sorted,
// deduplicated set of "directory" entries directly under prefix.
func (a *AerospikeBackend) listAll(prefix string) ([]string, error) {
	recordSet, err := a.client.ScanAll(nil, a.namespace, a.set)
	if err != nil {
		return nil, err
	}

	var keyList []string
	for res := range recordSet.Results() {
		if res.Err != nil {
			return nil, res.Err
		}
		recordKey := res.Record.Bins[keyBin].(string)
		if trimPrefix, ok := strings.CutPrefix(recordKey, prefix); ok {
			keys := strings.Split(trimPrefix, "/")
			if len(keys) == 1 {
				keyList = append(keyList, keys[0])
			} else {
				withSlash := keys[0] + "/"
				if !strutil.StrListContains(keyList, withSlash) {
					keyList = append(keyList, withSlash)
				}
			}
		}
	}

	sort.Strings(keyList)

	return keyList, nil
}

// List is used to list all the keys under a given
// prefix, up to the next prefix.
func (a *AerospikeBackend) List(_ context.Context, prefix string) ([]string, error) {
	return a.listAll(prefix)
}

// ListPage is used to list a page of keys under a given prefix, in sorted
// order, starting after the given key.
func (a *AerospikeBackend) ListPage(_ context.Context, prefix string, after string, limit int) ([]string, error) {
	keys, err := a.listAll(prefix)
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

func parseHostList(list string) ([]*aero.Host, error) {
	hosts := strings.Split(list, ",")
	var hostList []*aero.Host
	for _, host := range hosts {
		if host == "" {
			continue
		}
		split := strings.Split(host, ":")
		switch len(split) {
		case 1:
			hostList = append(hostList, aero.NewHost(split[0], defaultPort))
		case 2:
			port, err := strconv.Atoi(split[1])
			if err != nil {
				return nil, err
			}
			hostList = append(hostList, aero.NewHost(split[0], port))
		default:
			return nil, fmt.Errorf("invalid 'hostlist' configuration")
		}
	}
	return hostList, nil
}

// objectKeyDigest derives a fixed-length Aerospike record key from an
// arbitrary-length OpenBao storage key. This is not a password or
// credential hash -- it's a deterministic identifier transform, since
// Aerospike doesn't accept arbitrary-length strings as record keys.
func objectKeyDigest(s string) string {
	digest := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", digest[:])
}
