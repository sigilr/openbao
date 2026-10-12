// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	pkgPath "path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/cenkalti/backoff/v5"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	log "github.com/hashicorp/go-hclog"
	uuid "github.com/hashicorp/go-uuid"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	// DefaultDynamoDBRegion is used when no region is configured
	// explicitly.
	DefaultDynamoDBRegion = "us-east-1"
	// DefaultDynamoDBTableName is used when no table name
	// is configured explicitly.
	DefaultDynamoDBTableName = "openbao-dynamodb-backend"

	// DefaultDynamoDBReadCapacity is the default read capacity
	// that is used when none is configured explicitly.
	DefaultDynamoDBReadCapacity = 5
	// DefaultDynamoDBWriteCapacity is the default write capacity
	// that is used when none is configured explicitly.
	DefaultDynamoDBWriteCapacity = 5

	// DynamoDBEmptyPath is the string that is used instead of
	// empty strings when stored in DynamoDB.
	DynamoDBEmptyPath = " "
	// DynamoDBLockPrefix is the prefix used to mark DynamoDB records
	// as locks. This prefix causes them not to be returned by
	// List operations.
	DynamoDBLockPrefix = "_"

	// DynamoDBLockTTL matches the default that Consul's API uses, 15 seconds.
	DynamoDBLockTTL = 15 * time.Second

	// DynamoDBLockRenewInterval is the amount of time to wait between lock
	// renewals.
	DynamoDBLockRenewInterval = 5 * time.Second

	// DynamoDBLockRetryInterval is the amount of time to wait
	// if a lock fails before trying again.
	DynamoDBLockRetryInterval = time.Second
	// DynamoDBWatchRetryMax is the number of times to re-try a
	// failed watch before signaling that leadership is lost.
	DynamoDBWatchRetryMax = 5
	// DynamoDBWatchRetryInterval is the amount of time to wait
	// if a watch fails before trying again.
	DynamoDBWatchRetryInterval = 5 * time.Second
)

// Verify DynamoDBBackend satisfies the correct interfaces
var (
	_ physical.Backend   = (*DynamoDBBackend)(nil)
	_ physical.HABackend = (*DynamoDBBackend)(nil)
	_ physical.Lock      = (*DynamoDBLock)(nil)
)

// DynamoDBBackend is a physical backend that stores data in
// a DynamoDB table. It can be run in high-availability mode
// as DynamoDB has locking capabilities.
type DynamoDBBackend struct {
	table      string
	client     *dynamodb.Client
	logger     log.Logger
	haEnabled  bool
	permitPool *physical.PermitPool
}

// DynamoDBRecord is the representation of an OpenBao entry in
// DynamoDB. The OpenBao key is split up into two components
// (Path and Key) in order to allow more efficient listings.
type DynamoDBRecord struct {
	Path  string
	Key   string
	Value []byte
}

// DynamoDBLock implements a lock using a DynamoDB client.
type DynamoDBLock struct {
	backend    *DynamoDBBackend
	value, key string
	identity   string
	held       bool
	lock       sync.Mutex
	// Allow modifying the Lock durations for ease of unit testing.
	renewInterval      time.Duration
	ttl                time.Duration
	watchRetryInterval time.Duration
}

type DynamoDBLockRecord struct {
	Path     string
	Key      string
	Value    []byte
	Identity []byte
	Expires  int64
}

// NewDynamoDBBackend constructs a DynamoDB backend. If the
// configured DynamoDB table does not exist, it creates it.
func NewDynamoDBBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	table := os.Getenv("AWS_DYNAMODB_TABLE")
	if table == "" {
		table = conf["table"]
		if table == "" {
			table = DefaultDynamoDBTableName
		}
	}
	readCapacityString := os.Getenv("AWS_DYNAMODB_READ_CAPACITY")
	if readCapacityString == "" {
		readCapacityString = conf["read_capacity"]
		if readCapacityString == "" {
			readCapacityString = "0"
		}
	}
	readCapacity, err := strconv.Atoi(readCapacityString)
	if err != nil {
		return nil, fmt.Errorf("invalid read capacity: %q", readCapacityString)
	}
	if readCapacity == 0 {
		readCapacity = DefaultDynamoDBReadCapacity
	}

	writeCapacityString := os.Getenv("AWS_DYNAMODB_WRITE_CAPACITY")
	if writeCapacityString == "" {
		writeCapacityString = conf["write_capacity"]
		if writeCapacityString == "" {
			writeCapacityString = "0"
		}
	}
	writeCapacity, err := strconv.Atoi(writeCapacityString)
	if err != nil {
		return nil, fmt.Errorf("invalid write capacity: %q", writeCapacityString)
	}
	if writeCapacity == 0 {
		writeCapacity = DefaultDynamoDBWriteCapacity
	}

	endpoint := os.Getenv("AWS_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		endpoint = conf["endpoint"]
	}
	region := os.Getenv("AWS_DYNAMODB_REGION")
	if region == "" {
		region = os.Getenv("AWS_REGION")
		if region == "" {
			region = os.Getenv("AWS_DEFAULT_REGION")
			if region == "" {
				region = conf["region"]
				if region == "" {
					region = DefaultDynamoDBRegion
				}
			}
		}
	}

	dynamodbMaxRetryString := os.Getenv("AWS_DYNAMODB_MAX_RETRIES")
	if dynamodbMaxRetryString == "" {
		dynamodbMaxRetryString = conf["dynamodb_max_retries"]
	}
	var dynamodbMaxRetry int
	if dynamodbMaxRetryString != "" {
		dynamodbMaxRetry, err = strconv.Atoi(dynamodbMaxRetryString)
		if err != nil {
			return nil, fmt.Errorf("invalid max retry: %q", dynamodbMaxRetryString)
		}
	}

	ctx := context.Background()

	pooledTransport := cleanhttp.DefaultPooledTransport()
	pooledTransport.MaxIdleConnsPerHost = consts.ExpirationRestoreWorkerCount

	loadOpts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithHTTPClient(&http.Client{Transport: pooledTransport}),
	}
	if accessKey := conf["access_key"]; accessKey != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, conf["secret_key"], conf["session_token"]),
		))
	}
	if dynamodbMaxRetry > 0 {
		loadOpts = append(loadOpts, config.WithRetryMaxAttempts(dynamodbMaxRetry))
	}

	awsConf, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("could not establish AWS session: %w", err)
	}

	client := dynamodb.NewFromConfig(awsConf, func(o *dynamodb.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})

	if err := ensureTableExists(ctx, client, table, readCapacity, writeCapacity); err != nil {
		return nil, err
	}

	haEnabled := os.Getenv("DYNAMODB_HA_ENABLED")
	if haEnabled == "" {
		haEnabled = conf["ha_enabled"]
	}
	haEnabledBool, _ := strconv.ParseBool(haEnabled)

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		logger.Debug("max_parallel set", "max_parallel", maxParInt)
	}

	return &DynamoDBBackend{
		table:      table,
		client:     client,
		permitPool: physical.NewPermitPool(maxParInt),
		haEnabled:  haEnabledBool,
		logger:     logger,
	}, nil
}

// Put is used to insert or update an entry
func (d *DynamoDBBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"dynamodb", "put"}, time.Now())

	record := DynamoDBRecord{
		Path:  recordPathForKey(entry.Key),
		Key:   recordKeyForKey(entry.Key),
		Value: entry.Value,
	}
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("could not convert prefix record to DynamoDB item: %w", err)
	}
	requests := []types.WriteRequest{{
		PutRequest: &types.PutRequest{
			Item: item,
		},
	}}

	for _, prefix := range physical.Prefixes(entry.Key) {
		record = DynamoDBRecord{
			Path: recordPathForKey(prefix),
			Key:  fmt.Sprintf("%s/", recordKeyForKey(prefix)),
		}
		item, err := attributevalue.MarshalMap(record)
		if err != nil {
			return fmt.Errorf("could not convert prefix record to DynamoDB item: %w", err)
		}
		requests = append(requests, types.WriteRequest{
			PutRequest: &types.PutRequest{
				Item: item,
			},
		})
	}

	return d.batchWriteRequests(ctx, requests)
}

// Get is used to fetch an entry
func (d *DynamoDBBackend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"dynamodb", "get"}, time.Now())

	d.permitPool.Acquire()
	defer d.permitPool.Release()

	resp, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(d.table),
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			"Path": &types.AttributeValueMemberS{Value: recordPathForKey(key)},
			"Key":  &types.AttributeValueMemberS{Value: recordKeyForKey(key)},
		},
	})
	if err != nil {
		return nil, err
	}
	if resp.Item == nil {
		return nil, nil
	}

	record := &DynamoDBRecord{}
	if err := attributevalue.UnmarshalMap(resp.Item, record); err != nil {
		return nil, err
	}

	return &physical.Entry{
		Key:   entryKey(record),
		Value: record.Value,
	}, nil
}

// Delete is used to permanently delete an entry
func (d *DynamoDBBackend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"dynamodb", "delete"}, time.Now())

	requests := []types.WriteRequest{{
		DeleteRequest: &types.DeleteRequest{
			Key: map[string]types.AttributeValue{
				"Path": &types.AttributeValueMemberS{Value: recordPathForKey(key)},
				"Key":  &types.AttributeValueMemberS{Value: recordKeyForKey(key)},
			},
		},
	}}

	// Clean up empty "folders" by looping through all levels of the path to the item being deleted looking for
	// children. Loop from deepest path to shallowest, and only consider items children if they are not going to be
	// deleted by our batch delete request. If a path has no valid children, then it should be considered an empty
	// "folder" and be deleted along with the original item in our batch job. Because we loop from deepest path to
	// shallowest, once we find a path level that contains valid children we can stop the cleanup operation.
	prefixes := physical.Prefixes(key)
	sort.Sort(sort.Reverse(sort.StringSlice(prefixes)))
	for index, prefix := range prefixes {
		// Because delete batches its requests, we need to pass keys we know are going to be deleted into
		// hasChildren so it can exclude those when it determines if there WILL be any children left after
		// the delete operations have completed.
		var excluded []string
		if index == 0 {
			// This is the value we know for sure is being deleted
			excluded = append(excluded, recordKeyForKey(key))
		} else {
			// The previous path doesn't count as a child, since if we're still looping, we've found no children
			excluded = append(excluded, recordKeyForKey(prefixes[index-1]))
		}

		hasChildren, err := d.hasChildren(ctx, prefix, excluded)
		if err != nil {
			return err
		}

		if !hasChildren {
			// If there are no children other than ones we know are being deleted then cleanup empty "folder" pointers
			requests = append(requests, types.WriteRequest{
				DeleteRequest: &types.DeleteRequest{
					Key: map[string]types.AttributeValue{
						"Path": &types.AttributeValueMemberS{Value: recordPathForKey(prefix)},
						"Key":  &types.AttributeValueMemberS{Value: fmt.Sprintf("%s/", recordKeyForKey(prefix))},
					},
				},
			})
		} else {
			// This loop starts at the deepest path and works backwards looking for children
			// once a deeper level of the path has been found to have children there is no
			// more cleanup that needs to happen, otherwise we might remove folder pointers
			// to that deeper path making it "undiscoverable" with the list operation
			break
		}
	}

	return d.batchWriteRequests(ctx, requests)
}

// listAll returns every non-lock record key stored directly under prefix
// (one path component, with a trailing slash for "folders"). DynamoDB
// Query results within a partition (a given Path) come back ordered by
// the "Key" range attribute, so the returned slice is already sorted;
// listAll is shared by List and ListPage.
func (d *DynamoDBBackend) listAll(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"dynamodb", "list"}, time.Now())

	prefix = strings.TrimSuffix(prefix, "/")
	prefix = escapeEmptyPath(prefix)

	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String(d.table),
		ConsistentRead:         aws.Bool(true),
		KeyConditionExpression: aws.String("#path = :path"),
		ExpressionAttributeNames: map[string]string{
			"#path": "Path",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":path": &types.AttributeValueMemberS{Value: prefix},
		},
	}

	d.permitPool.Acquire()
	defer d.permitPool.Release()

	keys := []string{}
	paginator := dynamodb.NewQueryPaginator(d.client, queryInput)
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		var record DynamoDBRecord
		for _, item := range out.Items {
			if err := attributevalue.UnmarshalMap(item, &record); err != nil {
				return nil, err
			}
			if !strings.HasPrefix(record.Key, DynamoDBLockPrefix) {
				keys = append(keys, record.Key)
			}
		}
	}

	return keys, nil
}

// List is used to list all the keys under a given prefix, up to the next
// prefix.
func (d *DynamoDBBackend) List(ctx context.Context, prefix string) ([]string, error) {
	return d.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, starting
// strictly after the "after" key, up to "limit" keys.
func (d *DynamoDBBackend) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	keys, err := d.listAll(ctx, prefix)
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

// hasChildren returns true if there exist items below a certain path prefix.
// To do so, the method fetches such items from DynamoDB. This method is primarily
// used by Delete. Because DynamoDB requests are batched this method is being called
// before any deletes take place. To account for that hasChildren accepts a slice of
// strings representing values we expect to find that should NOT be counted as children
// because they are going to be deleted.
func (d *DynamoDBBackend) hasChildren(ctx context.Context, prefix string, exclude []string) (bool, error) {
	prefix = strings.TrimSuffix(prefix, "/")
	prefix = escapeEmptyPath(prefix)

	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String(d.table),
		ConsistentRead:         aws.Bool(true),
		KeyConditionExpression: aws.String("#path = :path"),
		ExpressionAttributeNames: map[string]string{
			"#path": "Path",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":path": &types.AttributeValueMemberS{Value: prefix},
		},
		// Avoid fetching too many items from DynamoDB for performance reasons.
		// We want to know if there are any children we don't expect to see.
		// Answering that question requires fetching a minimum of one more item
		// than the number we expect. In most cases this value will be 2
		Limit: aws.Int32(int32(len(exclude) + 1)),
	}

	d.permitPool.Acquire()
	defer d.permitPool.Release()

	out, err := d.client.Query(ctx, queryInput)
	if err != nil {
		return false, err
	}
	var childrenExist bool
	for _, item := range out.Items {
		keyAttr, ok := item["Key"].(*types.AttributeValueMemberS)
		if !ok {
			continue
		}
		for _, excluded := range exclude {
			// Check if we've found an item we didn't expect to. Look for "folder" pointer keys (trailing slash)
			// and regular value keys (no trailing slash)
			if keyAttr.Value != excluded && keyAttr.Value != fmt.Sprintf("%s/", excluded) {
				childrenExist = true
				break
			}
		}
		if childrenExist {
			// We only need to find ONE child we didn't expect to.
			break
		}
	}

	return childrenExist, nil
}

// LockWith is used for mutual exclusion based on the given key.
func (d *DynamoDBBackend) LockWith(key, value string) (physical.Lock, error) {
	identity, err := uuid.GenerateUUID()
	if err != nil {
		return nil, err
	}
	return &DynamoDBLock{
		backend:            d,
		key:                pkgPath.Join(pkgPath.Dir(key), DynamoDBLockPrefix+pkgPath.Base(key)),
		value:              value,
		identity:           identity,
		renewInterval:      DynamoDBLockRenewInterval,
		ttl:                DynamoDBLockTTL,
		watchRetryInterval: DynamoDBWatchRetryInterval,
	}, nil
}

func (d *DynamoDBBackend) HAEnabled() bool {
	return d.haEnabled
}

// batchWriteRequests takes a list of write requests and executes them in
// batches, with a maximum size of 25 (the limit of BatchWriteItem
// requests).
func (d *DynamoDBBackend) batchWriteRequests(ctx context.Context, requests []types.WriteRequest) error {
	for len(requests) > 0 {
		batchSize := int(math.Min(float64(len(requests)), 25))
		batch := map[string][]types.WriteRequest{d.table: requests[:batchSize]}
		requests = requests[batchSize:]

		var err error

		d.permitPool.Acquire()

		boff := backoff.NewExponentialBackOff()
		deadline := time.Now().Add(600 * time.Second)

		for len(batch) > 0 {
			var output *dynamodb.BatchWriteItemOutput
			output, err = d.client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: batch,
			})
			if err != nil {
				break
			}

			if len(output.UnprocessedItems) == 0 {
				break
			}

			if time.Now().After(deadline) {
				err = errors.New("dynamodb: timeout handling UnprocessedItems")
				break
			}

			batch = output.UnprocessedItems
			select {
			case <-time.After(boff.NextBackOff()):
			case <-ctx.Done():
				err = ctx.Err()
			}
			if err != nil {
				break
			}
		}

		d.permitPool.Release()
		if err != nil {
			return err
		}
	}
	return nil
}

// Lock tries to acquire the lock by repeatedly trying to create
// a record in the DynamoDB table. It will block until either the
// stop channel is closed or the lock could be acquired successfully.
// The returned channel will be closed once the lock is deleted or
// changed in the DynamoDB table.
func (l *DynamoDBLock) Lock(stopCh <-chan struct{}) (doneCh <-chan struct{}, retErr error) {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.held {
		return nil, fmt.Errorf("lock already held")
	}

	done := make(chan struct{})
	// close done channel even in case of error
	defer func() {
		if retErr != nil {
			close(done)
		}
	}()

	var (
		stop    = make(chan struct{})
		success = make(chan struct{})
		errs    = make(chan error)
		leader  = make(chan struct{})
	)
	// try to acquire the lock asynchronously
	go l.tryToLock(stop, success, errs)

	select {
	case <-success:
		l.held = true
		// after acquiring it successfully, we must renew the lock periodically,
		// and watch the lock in order to close the leader channel
		// once it is lost.
		go l.periodicallyRenewLock(leader)
		go l.watch(leader)
	case retErr = <-errs:
		close(stop)
		return nil, retErr
	case <-stopCh:
		close(stop)
		return nil, nil
	}

	return leader, retErr
}

// Unlock releases the lock by deleting the lock record from the
// DynamoDB table.
func (l *DynamoDBLock) Unlock() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if !l.held {
		return nil
	}

	l.held = false

	// Conditionally delete after check that the key is actually this OpenBao
	// instance's and not been already claimed by another leader
	_, err := l.backend.client.DeleteItem(context.Background(), &dynamodb.DeleteItemInput{
		TableName:           &l.backend.table,
		ConditionExpression: aws.String("#identity = :identity"),
		Key: map[string]types.AttributeValue{
			"Path": &types.AttributeValueMemberS{Value: recordPathForKey(l.key)},
			"Key":  &types.AttributeValueMemberS{Value: recordKeyForKey(l.key)},
		},
		ExpressionAttributeNames: map[string]string{
			"#identity": "Identity",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":identity": &types.AttributeValueMemberB{Value: []byte(l.identity)},
		},
	})
	if isConditionCheckFailed(err) {
		err = nil
	}

	return err
}

// Value checks whether or not the lock is held by any instance of DynamoDBLock,
// including this one, and returns the current value.
func (l *DynamoDBLock) Value() (bool, string, error) {
	entry, err := l.backend.Get(context.Background(), l.key)
	if err != nil {
		return false, "", err
	}
	if entry == nil {
		return false, "", nil
	}

	return true, string(entry.Value), nil
}

// tryToLock tries to create a new item in DynamoDB
// every `DynamoDBLockRetryInterval`. As long as the item
// cannot be created (because it already exists), it will
// be retried. If the operation fails due to an error, it
// is sent to the errors channel.
// When the lock could be acquired successfully, the success
// channel is closed.
func (l *DynamoDBLock) tryToLock(stop, success chan struct{}, errs chan error) {
	ticker := time.NewTicker(DynamoDBLockRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			err := l.updateItem(true)
			if err != nil {
				// Don't report a condition check failure, this means that the lock
				// is already being held.
				if isConditionCheckFailed(err) {
					continue
				}
				errs <- err
				return
			}
			close(success)
			return
		}
	}
}

func (l *DynamoDBLock) periodicallyRenewLock(done chan struct{}) {
	ticker := time.NewTicker(l.renewInterval)
	for {
		select {
		case <-ticker.C:
			// This should not renew the lock if the lock was deleted from under you.
			err := l.updateItem(false)
			if err != nil {
				if !isConditionCheckFailed(err) {
					l.backend.logger.Error("error renewing leadership lock", "error", err)
				}
			}
		case <-done:
			ticker.Stop()
			return
		}
	}
}

// updateItem attempts to put/update the DynamoDB item using condition
// expressions to evaluate the TTL.
func (l *DynamoDBLock) updateItem(createIfMissing bool) error {
	now := time.Now()

	conditionExpression := ""
	if createIfMissing {
		conditionExpression += "attribute_not_exists(#path) or " +
			"attribute_not_exists(#key) or "
	} else {
		conditionExpression += "attribute_exists(#path) and " +
			"attribute_exists(#key) and "
	}

	// To work when upgrading from older versions that did not include the
	// Identity attribute, we first check if the attr doesn't exist, and if
	// it does, then we check if the identity is equal to our own.
	// We also write if the lock expired.
	conditionExpression += "(attribute_not_exists(#identity) or #identity = :identity or #expires <= :now)"

	_, err := l.backend.client.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName: aws.String(l.backend.table),
		Key: map[string]types.AttributeValue{
			"Path": &types.AttributeValueMemberS{Value: recordPathForKey(l.key)},
			"Key":  &types.AttributeValueMemberS{Value: recordKeyForKey(l.key)},
		},
		UpdateExpression: aws.String("SET #value=:value, #identity=:identity, #expires=:expires"),
		// If both key and path already exist, we can only write if
		// A. identity is equal to our identity (or the identity doesn't exist)
		// or
		// B. The ttl on the item is <= to the current time
		ConditionExpression: aws.String(conditionExpression),
		ExpressionAttributeNames: map[string]string{
			"#path":     "Path",
			"#key":      "Key",
			"#identity": "Identity",
			"#expires":  "Expires",
			"#value":    "Value",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":identity": &types.AttributeValueMemberB{Value: []byte(l.identity)},
			":value":    &types.AttributeValueMemberB{Value: []byte(l.value)},
			":now":      &types.AttributeValueMemberN{Value: strconv.FormatInt(now.UnixNano(), 10)},
			":expires":  &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(l.ttl).UnixNano(), 10)},
		},
	})

	return err
}

// watch checks whether the lock has changed in the
// DynamoDB table and closes the leader channel if so.
// The interval is set by `DynamoDBWatchRetryInterval`.
// If an error occurs during the check, watch will retry
// the operation for `DynamoDBWatchRetryMax` times and
// close the leader channel if it can't succeed.
func (l *DynamoDBLock) watch(lost chan struct{}) {
	retries := DynamoDBWatchRetryMax

	ticker := time.NewTicker(l.watchRetryInterval)
WatchLoop:
	for range ticker.C {
		resp, err := l.backend.client.GetItem(context.Background(), &dynamodb.GetItemInput{
			TableName:      aws.String(l.backend.table),
			ConsistentRead: aws.Bool(true),
			Key: map[string]types.AttributeValue{
				"Path": &types.AttributeValueMemberS{Value: recordPathForKey(l.key)},
				"Key":  &types.AttributeValueMemberS{Value: recordKeyForKey(l.key)},
			},
		})
		if err != nil {
			retries--
			if retries == 0 {
				break WatchLoop
			}
			continue
		}

		if resp == nil || resp.Item == nil {
			break WatchLoop
		}
		record := &DynamoDBLockRecord{}
		err = attributevalue.UnmarshalMap(resp.Item, record)
		if err != nil || string(record.Identity) != l.identity {
			break WatchLoop
		}
		retries = DynamoDBWatchRetryMax
	}

	close(lost)
}

// ensureTableExists creates a DynamoDB table with a given
// DynamoDB client. If the table already exists, it is not
// being reconfigured.
func ensureTableExists(ctx context.Context, client *dynamodb.Client, table string, readCapacity, writeCapacity int) error {
	_, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(table),
	})
	if err == nil {
		return nil
	}

	var notFound *types.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		return err
	}

	_, err = client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		ProvisionedThroughput: &types.ProvisionedThroughput{
			ReadCapacityUnits:  aws.Int64(int64(readCapacity)),
			WriteCapacityUnits: aws.Int64(int64(writeCapacity)),
		},
		KeySchema: []types.KeySchemaElement{{
			AttributeName: aws.String("Path"),
			KeyType:       types.KeyTypeHash,
		}, {
			AttributeName: aws.String("Key"),
			KeyType:       types.KeyTypeRange,
		}},
		AttributeDefinitions: []types.AttributeDefinition{{
			AttributeName: aws.String("Path"),
			AttributeType: types.ScalarAttributeTypeS,
		}, {
			AttributeName: aws.String("Key"),
			AttributeType: types.ScalarAttributeTypeS,
		}},
	})
	if err != nil {
		return err
	}

	waiter := dynamodb.NewTableExistsWaiter(client)
	return waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)}, 5*time.Minute)
}

// recordPathForKey transforms an OpenBao key into a value suitable for the
// `DynamoDBRecord`'s `Path` property. This path equals the key without its
// last component.
func recordPathForKey(key string) string {
	if strings.Contains(key, "/") {
		return pkgPath.Dir(key)
	}
	return DynamoDBEmptyPath
}

// recordKeyForKey transforms an OpenBao key into a value suitable for the
// `DynamoDBRecord`'s `Key` property. This equals the key's last component.
func recordKeyForKey(key string) string {
	return pkgPath.Base(key)
}

// entryKey returns the OpenBao key for a given record from the DynamoDB
// table. This is the combination of the record's Path and Key.
func entryKey(record *DynamoDBRecord) string {
	path := unescapeEmptyPath(record.Path)
	if path == "" {
		return record.Key
	}
	return pkgPath.Join(record.Path, record.Key)
}

// escapeEmptyPath is used to escape the root key's path
// with a value that can be stored in DynamoDB. DynamoDB
// does not allow values to be empty strings.
func escapeEmptyPath(s string) string {
	if s == "" {
		return DynamoDBEmptyPath
	}
	return s
}

// unescapeEmptyPath is the opposite of `escapeEmptyPath`.
func unescapeEmptyPath(s string) string {
	if s == DynamoDBEmptyPath {
		return ""
	}
	return s
}

// isConditionCheckFailed tests whether err is a ConditionalCheckFailedException
// from the AWS SDK.
func isConditionCheckFailed(err error) bool {
	var condErr *types.ConditionalCheckFailedException
	return errors.As(err, &condErr)
}
