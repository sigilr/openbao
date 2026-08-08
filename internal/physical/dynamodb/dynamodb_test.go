// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package dynamodb

import (
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	dynamodbhelper "github.com/openbao/openbao/v2/internal/helper/testhelpers/dynamodb"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestDynamoDBBackend(t *testing.T) {
	cleanup, svccfg := dynamodbhelper.PrepareTestContainer(t)
	defer cleanup()

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-east-1"
	}

	table := testTableName(t)
	conn := testClient(t, svccfg, region)
	defer func() {
		conn.DeleteTable(t.Context(), &dynamodb.DeleteTableInput{TableName: awsv2.String(table)})
	}()

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewDynamoDBBackend(map[string]string{
		"access_key": svccfg.AccessKey,
		"secret_key": svccfg.SecretKey,
		"table":      table,
		"region":     region,
		"endpoint":   svccfg.URL().String(),
	}, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}

func TestDynamoDBHABackend(t *testing.T) {
	cleanup, svccfg := dynamodbhelper.PrepareTestContainer(t)
	defer cleanup()

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-east-1"
	}

	table := testTableName(t)
	conn := testClient(t, svccfg, region)
	defer func() {
		conn.DeleteTable(t.Context(), &dynamodb.DeleteTableInput{TableName: awsv2.String(table)})
	}()

	logger := logging.NewVaultLogger(log.Debug)
	config := map[string]string{
		"access_key": svccfg.AccessKey,
		"secret_key": svccfg.SecretKey,
		"table":      table,
		"region":     region,
		"endpoint":   svccfg.URL().String(),
	}

	b, err := NewDynamoDBBackend(config, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	b2, err := NewDynamoDBBackend(config, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseHABackend(t, b.(physical.HABackend), b2.(physical.HABackend))
	testDynamoDBLockTTL(t, b.(physical.HABackend))
	testDynamoDBLockRenewal(t, b.(physical.HABackend))
}

// Similar to ExerciseHABackend, but using internal implementation details to
// trigger the lock failure scenario by setting the lock renew period for one
// of the locks to a higher value than the lock TTL.
func testDynamoDBLockTTL(t *testing.T, ha physical.HABackend) {
	// Set much smaller lock times to speed up the test.
	lockTTL := time.Second * 3
	renewInterval := time.Second * 1
	watchInterval := time.Second * 1

	// Get the lock
	origLock, err := ha.LockWith("dynamodbttl", "bar")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// set the first lock renew period to double the expected TTL.
	lock := origLock.(*DynamoDBLock)
	lock.renewInterval = lockTTL * 2
	lock.ttl = lockTTL
	lock.watchRetryInterval = watchInterval

	// Attempt to lock
	leaderCh, err := lock.Lock(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if leaderCh == nil {
		t.Fatalf("failed to get leader ch")
	}

	// Check the value
	held, val, err := lock.Value()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !held {
		t.Fatalf("should be held")
	}
	if val != "bar" {
		t.Fatalf("bad value: %v", err)
	}

	// Second acquisition should succeed because the first lock should
	// not renew within the 3 sec TTL.
	origLock2, err := ha.LockWith("dynamodbttl", "baz")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	lock2 := origLock2.(*DynamoDBLock)
	lock2.renewInterval = renewInterval
	lock2.ttl = lockTTL
	lock2.watchRetryInterval = watchInterval

	// Cancel attempt eventually so as not to block unit tests forever
	stopCh := make(chan struct{})
	time.AfterFunc(lockTTL*10, func() {
		close(stopCh)
	})

	// Attempt to lock should work
	leaderCh2, err := lock2.Lock(stopCh)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if leaderCh2 == nil {
		t.Fatalf("should get leader ch")
	}

	// Check the value
	held, val, err = lock2.Value()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !held {
		t.Fatalf("should be held")
	}
	if val != "baz" {
		t.Fatalf("bad value: %v", err)
	}

	// The first lock should have lost the leader channel
	leaderChClosed := false
	blocking := make(chan struct{})
	// Attempt to read from the leader or the blocking channel, which ever one
	// happens first.
	go func() {
		select {
		case <-time.After(watchInterval * 3):
			return
		case <-leaderCh:
			leaderChClosed = true
			close(blocking)
		case <-blocking:
			return
		}
	}()

	<-blocking
	if !leaderChClosed {
		t.Fatalf("original lock did not have its leader channel closed.")
	}

	// Cleanup
	lock2.Unlock()
}

// Similar to ExerciseHABackend, but using internal implementation details to
// trigger a renewal before a "watch" check, which has been a source of
// race conditions.
func testDynamoDBLockRenewal(t *testing.T, ha physical.HABackend) {
	renewInterval := time.Second * 1
	watchInterval := time.Second * 5

	// Get the lock
	origLock, err := ha.LockWith("dynamodbrenewal", "bar")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// customize the renewal and watch intervals
	lock := origLock.(*DynamoDBLock)
	lock.renewInterval = renewInterval
	lock.watchRetryInterval = watchInterval

	// Attempt to lock
	leaderCh, err := lock.Lock(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if leaderCh == nil {
		t.Fatalf("failed to get leader ch")
	}

	// Check the value
	held, val, err := lock.Value()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !held {
		t.Fatalf("should be held")
	}
	if val != "bar" {
		t.Fatalf("bad value: %v", err)
	}

	// Release the lock, which will delete the stored item
	if err := lock.Unlock(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Wait longer than the renewal time, but less than the watch time
	time.Sleep(1500 * time.Millisecond)

	// Attempt to lock with new lock
	newLock, err := ha.LockWith("dynamodbrenewal", "baz")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Cancel attempt in 6 sec so as not to block unit tests forever
	stopCh := make(chan struct{})
	time.AfterFunc(6*time.Second, func() {
		close(stopCh)
	})

	// Attempt to lock should work
	leaderCh2, err := newLock.Lock(stopCh)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if leaderCh2 == nil {
		t.Fatalf("should get leader ch")
	}

	// Check the value
	held, val, err = newLock.Value()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !held {
		t.Fatalf("should be held")
	}
	if val != "baz" {
		t.Fatalf("bad value: %v", err)
	}

	// Cleanup
	newLock.Unlock()
}

func testTableName(t *testing.T) string {
	t.Helper()
	randInt := rand.New(rand.NewSource(time.Now().UnixNano())).Int()
	return fmt.Sprintf("openbao-dynamodb-testacc-%d", randInt)
}

func testClient(t *testing.T, svccfg *dynamodbhelper.Config, region string) *dynamodb.Client {
	t.Helper()

	cfg, err := config.LoadDefaultConfig(
		t.Context(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(svccfg.AccessKey, svccfg.SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = awsv2.String(svccfg.URL().String())
	})
}
