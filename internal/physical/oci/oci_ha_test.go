// Copyright © 2019, Oracle and/or its affiliates.
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"os"
	"testing"

	uuid "github.com/hashicorp/go-uuid"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/openbao/openbao/sdk/v2/physical"
)

// TestOCIHABackend is an acceptance test gated on BAO_ACC and real OCI
// credentials; see TestOCIBackend.
func TestOCIHABackend(t *testing.T) {
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

	backend := createBackend(bucketName, namespaceName, "true", bucketName, t)
	ha, ok := backend.(physical.HABackend)
	if !ok {
		t.Fatalf("does not implement physical.HABackend")
	}

	physical.ExerciseHABackend(t, ha, ha)
}
