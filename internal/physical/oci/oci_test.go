// Copyright © 2019, Oracle and/or its affiliates.
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"context"
	"os"
	"testing"

	log "github.com/hashicorp/go-hclog"
	uuid "github.com/hashicorp/go-uuid"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// TestOCIBackend is an acceptance test gated on BAO_ACC and real OCI
// credentials; there is no local OCI Object Storage emulator this could
// run against instead. See
// https://pkg.go.dev/github.com/oracle/oci-go-sdk/v65/common#DefaultConfigProvider
// for how to set up OCI credentials.
func TestOCIBackend(t *testing.T) {
	if os.Getenv("BAO_ACC") == "" {
		t.Skip("skipping as this test requires BAO_ACC and real OCI credentials")
	}

	if !hasOCICredentials() {
		t.Skip("skipping because OCI credentials could not be resolved")
	}

	bucketName, _ := uuid.GenerateUUID()
	configProvider := common.DefaultConfigProvider()
	objectStorageClient, _ := objectstorage.NewObjectStorageClientWithConfigurationProvider(configProvider)
	namespaceName := getNamespaceName(objectStorageClient, t)

	createBucket(bucketName, getTenancyOCID(configProvider, t), namespaceName, objectStorageClient, t)
	defer deleteBucket(namespaceName, bucketName, objectStorageClient, t)

	backend := createBackend(bucketName, namespaceName, "false", "", t)
	physical.ExerciseBackend(t, backend)
	physical.ExerciseBackend_ListPrefix(t, backend)
}

func createBucket(bucketName string, tenancyOCID string, namespaceName string, objectStorageClient objectstorage.ObjectStorageClient, t *testing.T) {
	createBucketRequest := objectstorage.CreateBucketRequest{
		NamespaceName: &namespaceName,
	}
	createBucketRequest.CompartmentId = &tenancyOCID
	createBucketRequest.Name = &bucketName
	createBucketRequest.Metadata = make(map[string]string)
	createBucketRequest.PublicAccessType = objectstorage.CreateBucketDetailsPublicAccessTypeNopublicaccess
	if _, err := objectStorageClient.CreateBucket(context.Background(), createBucketRequest); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}
}

func deleteBucket(namespaceName string, bucketName string, objectStorageClient objectstorage.ObjectStorageClient, t *testing.T) {
	request := objectstorage.DeleteBucketRequest{
		NamespaceName: &namespaceName,
		BucketName:    &bucketName,
	}
	if _, err := objectStorageClient.DeleteBucket(context.Background(), request); err != nil {
		t.Fatalf("Failed to delete bucket: %v", err)
	}
}

func getTenancyOCID(configProvider common.ConfigurationProvider, t *testing.T) string {
	tenancyOCID, err := configProvider.TenancyOCID()
	if err != nil {
		t.Fatalf("Failed to get tenancy ocid: %v", err)
	}
	return tenancyOCID
}

func createBackend(bucketName string, namespaceName string, haEnabledStr string, lockBucketName string, t *testing.T) physical.Backend {
	backend, err := NewBackend(map[string]string{
		"auth_type_api_key": "true",
		"bucket_name":       bucketName,
		"namespace_name":    namespaceName,
		"ha_enabled":        haEnabledStr,
		"lock_bucket_name":  lockBucketName,
	}, logging.NewVaultLogger(log.Trace))
	if err != nil {
		t.Fatalf("Failed to create new backend: %v", err)
	}
	return backend
}

func getNamespaceName(objectStorageClient objectstorage.ObjectStorageClient, t *testing.T) string {
	response, err := objectStorageClient.GetNamespace(context.Background(), objectstorage.GetNamespaceRequest{})
	if err != nil {
		t.Fatalf("Failed to get namespaceName: %v", err)
	}
	return *response.Value
}

func hasOCICredentials() bool {
	configProvider := common.DefaultConfigProvider()

	_, err := configProvider.KeyID()
	return err == nil
}
