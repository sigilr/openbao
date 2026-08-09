// Copyright © 2019, Oracle and/or its affiliates.
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	uuid "github.com/hashicorp/go-uuid"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/openbao/openbao/sdk/v2/physical"
)

// Verify Backend satisfies the correct interfaces
var _ physical.Backend = (*Backend)(nil)

const (
	// MaxNumberOfPermits limits maximum outstanding requests
	MaxNumberOfPermits = 256
)

var (
	metricDelete     = []string{"oci", "delete"}
	metricGet        = []string{"oci", "get"}
	metricList       = []string{"oci", "list"}
	metricPut        = []string{"oci", "put"}
	metricDeleteFull = []string{"oci", "deleteFull"}
	metricGetFull    = []string{"oci", "getFull"}
	metricListFull   = []string{"oci", "listFull"}
	metricPutFull    = []string{"oci", "putFull"}

	metricDeleteHa = []string{"oci", "deleteHa"}
	metricGetHa    = []string{"oci", "getHa"}
	metricPutHa    = []string{"oci", "putHa"}

	metricDeleteAcquirePool = []string{"oci", "deleteAcquirePool"}
	metricGetAcquirePool    = []string{"oci", "getAcquirePool"}
	metricListAcquirePool   = []string{"oci", "listAcquirePool"}
	metricPutAcquirePool    = []string{"oci", "putAcquirePool"}

	metricDeleteFailed         = []string{"oci", "deleteFailed"}
	metricGetFailed            = []string{"oci", "getFailed"}
	metricListFailed           = []string{"oci", "listFailed"}
	metricPutFailed            = []string{"oci", "putFailed"}
	metricHaWatchLockRetriable = []string{"oci", "haWatchLockRetriable"}
	metricPermitsUsed          = []string{"oci", "permitsUsed"}

	metric5xx = []string{"oci", "5xx"}
)

type Backend struct {
	client         *objectstorage.ObjectStorageClient
	bucketName     string
	logger         log.Logger
	permitPool     *physical.PermitPool
	namespaceName  string
	haEnabled      bool
	lockBucketName string
}

func NewBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	bucketName := conf["bucket_name"]
	if bucketName == "" {
		return nil, errors.New("missing bucket name")
	}

	namespaceName := conf["namespace_name"]
	if namespaceName == "" {
		return nil, errors.New("missing namespace name")
	}

	lockBucketName := ""
	haEnabled := false
	var err error
	haEnabledStr := conf["ha_enabled"]
	if haEnabledStr != "" {
		haEnabled, err = strconv.ParseBool(haEnabledStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse HA enabled: %w", err)
		}

		if haEnabled {
			lockBucketName = conf["lock_bucket_name"]
			if lockBucketName == "" {
				return nil, errors.New("missing lock bucket name")
			}
		}
	}

	authTypeAPIKeyBool := false
	authTypeAPIKeyStr := conf["auth_type_api_key"]
	if authTypeAPIKeyStr != "" {
		authTypeAPIKeyBool, err = strconv.ParseBool(authTypeAPIKeyStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing auth_type_api_key parameter: %w", err)
		}
	}

	var cp common.ConfigurationProvider
	if authTypeAPIKeyBool {
		cp = common.DefaultConfigProvider()
	} else {
		cp, err = auth.InstancePrincipalConfigurationProvider()
		if err != nil {
			return nil, fmt.Errorf("failed creating InstancePrincipalConfigurationProvider: %w", err)
		}
	}

	objectStorageClient, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(cp)
	if err != nil {
		return nil, fmt.Errorf("failed creating NewObjectStorageClientWithConfigurationProvider: %w", err)
	}

	region := conf["region"]
	if region != "" {
		objectStorageClient.SetRegion(region)
	}

	logger.Debug(
		"configuration",
		"bucket_name", bucketName,
		"region", region,
		"namespace_name", namespaceName,
		"ha_enabled", haEnabled,
		"lock_bucket_name", lockBucketName,
	)

	return &Backend{
		client:         &objectStorageClient,
		bucketName:     bucketName,
		logger:         logger,
		permitPool:     physical.NewPermitPool(MaxNumberOfPermits),
		namespaceName:  namespaceName,
		haEnabled:      haEnabled,
		lockBucketName: lockBucketName,
	}, nil
}

func (o *Backend) Put(ctx context.Context, entry *physical.Entry) error {
	o.logger.Debug("PUT started")
	defer metrics.MeasureSince(metricPutFull, time.Now())
	startAcquirePool := time.Now()
	metrics.SetGauge(metricPermitsUsed, float32(o.permitPool.CurrentPermits()))
	o.permitPool.Acquire()
	defer o.permitPool.Release()
	metrics.MeasureSince(metricPutAcquirePool, startAcquirePool)

	defer metrics.MeasureSince(metricPut, time.Now())
	size := int64(len(entry.Value))
	opcClientRequestID, err := uuid.GenerateUUID()
	if err != nil {
		metrics.IncrCounter(metricPutFailed, 1)
		o.logger.Error("failed to generate UUID")
		return fmt.Errorf("failed to generate UUID: %w", err)
	}

	o.logger.Debug("PUT", "opc-client-request-id", opcClientRequestID)
	request := objectstorage.PutObjectRequest{
		NamespaceName:      &o.namespaceName,
		BucketName:         &o.bucketName,
		ObjectName:         &entry.Key,
		ContentLength:      &size,
		PutObjectBody:      io.NopCloser(bytes.NewReader(entry.Value)),
		OpcMeta:            nil,
		OpcClientRequestId: &opcClientRequestID,
	}

	resp, err := o.client.PutObject(ctx, request)
	if resp.RawResponse != nil && resp.RawResponse.Body != nil {
		defer resp.RawResponse.Body.Close() //nolint:errcheck
	}

	if err != nil {
		metrics.IncrCounter(metricPutFailed, 1)
		return fmt.Errorf("failed to put data: %w", err)
	}

	o.logRequest("PUT", resp.RawResponse, resp.OpcClientRequestId, resp.OpcRequestId, err)
	o.logger.Debug("PUT completed")

	return nil
}

func (o *Backend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	o.logger.Debug("GET started")
	defer metrics.MeasureSince(metricGetFull, time.Now())
	metrics.SetGauge(metricPermitsUsed, float32(o.permitPool.CurrentPermits()))
	startAcquirePool := time.Now()
	o.permitPool.Acquire()
	defer o.permitPool.Release()
	metrics.MeasureSince(metricGetAcquirePool, startAcquirePool)

	defer metrics.MeasureSince(metricGet, time.Now())
	opcClientRequestID, err := uuid.GenerateUUID()
	if err != nil {
		o.logger.Error("failed to generate UUID")
		return nil, fmt.Errorf("failed to generate UUID: %w", err)
	}
	o.logger.Debug("GET", "opc-client-request-id", opcClientRequestID)
	request := objectstorage.GetObjectRequest{
		NamespaceName:      &o.namespaceName,
		BucketName:         &o.bucketName,
		ObjectName:         &key,
		OpcClientRequestId: &opcClientRequestID,
	}

	resp, err := o.client.GetObject(ctx, request)
	if resp.RawResponse != nil && resp.RawResponse.Body != nil {
		defer resp.RawResponse.Body.Close() //nolint:errcheck
	}
	o.logRequest("GET", resp.RawResponse, resp.OpcClientRequestId, resp.OpcRequestId, err)

	if err != nil {
		if resp.RawResponse != nil && resp.RawResponse.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		metrics.IncrCounter(metricGetFailed, 1)
		return nil, fmt.Errorf("failed to read value: %w", err)
	}

	body, err := io.ReadAll(resp.Content)
	if err != nil {
		metrics.IncrCounter(metricGetFailed, 1)
		return nil, fmt.Errorf("failed to decode value into bytes: %w", err)
	}

	o.logger.Debug("GET completed")

	return &physical.Entry{
		Key:   key,
		Value: body,
	}, nil
}

func (o *Backend) Delete(ctx context.Context, key string) error {
	o.logger.Debug("DELETE started")
	defer metrics.MeasureSince(metricDeleteFull, time.Now())
	metrics.SetGauge(metricPermitsUsed, float32(o.permitPool.CurrentPermits()))
	startAcquirePool := time.Now()
	o.permitPool.Acquire()
	defer o.permitPool.Release()
	metrics.MeasureSince(metricDeleteAcquirePool, startAcquirePool)

	defer metrics.MeasureSince(metricDelete, time.Now())
	opcClientRequestID, err := uuid.GenerateUUID()
	if err != nil {
		o.logger.Error("Delete: error generating UUID")
		return fmt.Errorf("failed to generate UUID: %w", err)
	}
	o.logger.Debug("Delete", "opc-client-request-id", opcClientRequestID)
	request := objectstorage.DeleteObjectRequest{
		NamespaceName:      &o.namespaceName,
		BucketName:         &o.bucketName,
		ObjectName:         &key,
		OpcClientRequestId: &opcClientRequestID,
	}

	resp, err := o.client.DeleteObject(ctx, request)
	if resp.RawResponse != nil && resp.RawResponse.Body != nil {
		defer resp.RawResponse.Body.Close() //nolint:errcheck
	}

	o.logRequest("DELETE", resp.RawResponse, resp.OpcClientRequestId, resp.OpcRequestId, err)

	if err != nil {
		if resp.RawResponse != nil && resp.RawResponse.StatusCode == http.StatusNotFound {
			return nil
		}
		metrics.IncrCounter(metricDeleteFailed, 1)
		return fmt.Errorf("failed to delete key: %w", err)
	}
	o.logger.Debug("DELETE completed")

	return nil
}

// listAll lists the sorted, deduplicated set of "directory" entries and
// object keys directly under prefix.
func (o *Backend) listAll(ctx context.Context, prefix string) ([]string, error) {
	o.logger.Debug("LIST started")
	defer metrics.MeasureSince(metricListFull, time.Now())
	metrics.SetGauge(metricPermitsUsed, float32(o.permitPool.CurrentPermits()))
	startAcquirePool := time.Now()
	o.permitPool.Acquire()
	defer o.permitPool.Release()

	metrics.MeasureSince(metricListAcquirePool, startAcquirePool)
	defer metrics.MeasureSince(metricList, time.Now())
	var keys []string
	delimiter := "/"
	var start *string

	for {
		opcClientRequestID, err := uuid.GenerateUUID()
		if err != nil {
			o.logger.Error("List: error generating UUID")
			return nil, fmt.Errorf("failed to generate UUID %w", err)
		}
		o.logger.Debug("LIST", "opc-client-request-id", opcClientRequestID)
		request := objectstorage.ListObjectsRequest{
			NamespaceName:      &o.namespaceName,
			BucketName:         &o.bucketName,
			Prefix:             &prefix,
			Delimiter:          &delimiter,
			Start:              start,
			OpcClientRequestId: &opcClientRequestID,
		}

		resp, err := o.client.ListObjects(ctx, request)
		o.logRequest("LIST", resp.RawResponse, resp.OpcClientRequestId, resp.OpcRequestId, err)

		if err != nil {
			metrics.IncrCounter(metricListFailed, 1)
			return nil, fmt.Errorf("failed to list using prefix: %w", err)
		}

		for _, commonPrefix := range resp.Prefixes {
			commonPrefix := strings.TrimPrefix(commonPrefix, prefix)
			keys = append(keys, commonPrefix)
		}

		for _, object := range resp.Objects {
			key := strings.TrimPrefix(*object.Name, prefix)
			keys = append(keys, key)
		}

		// Duplicate keys are not expected
		keys = strutil.RemoveDuplicates(keys, false)

		if resp.NextStartWith == nil {
			resp.RawResponse.Body.Close() //nolint:errcheck
			break
		}

		start = resp.NextStartWith
		resp.RawResponse.Body.Close() //nolint:errcheck
	}

	sort.Strings(keys)
	o.logger.Debug("LIST completed")
	return keys, nil
}

func (o *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	return o.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, in sorted
// order, starting after the given key.
func (o *Backend) ListPage(ctx context.Context, prefix string, after string, limit int) ([]string, error) {
	keys, err := o.listAll(ctx, prefix)
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

func (o *Backend) logRequest(operation string, response *http.Response, clientOpcRequestIDPtr *string, opcRequestIDPtr *string, err error) {
	statusCode := 0
	clientOpcRequestID := " "
	opcRequestID := " "

	if response != nil {
		statusCode = response.StatusCode
		if statusCode/100 == 5 {
			metrics.IncrCounter(metric5xx, 1)
		}
	}

	if clientOpcRequestIDPtr != nil {
		clientOpcRequestID = *clientOpcRequestIDPtr
	}

	if opcRequestIDPtr != nil {
		opcRequestID = *opcRequestIDPtr
	}

	statusCodeStr := "No response"
	if statusCode != 0 {
		statusCodeStr = strconv.Itoa(statusCode)
	}

	logLine := fmt.Sprintf("%s client:opc-request-id %s opc-request-id: %s status-code: %s",
		operation, clientOpcRequestID, opcRequestID, statusCodeStr)
	if err != nil && statusCode/100 == 5 {
		o.logger.Error(logLine, "error", err)
	}
}
