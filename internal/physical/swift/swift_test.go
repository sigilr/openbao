// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package swift

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	log "github.com/hashicorp/go-hclog"
	"github.com/ncw/swift/v2"

	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/physical"
	testhelpersswift "github.com/openbao/openbao/v2/internal/helper/testhelpers/swift"
)

func TestSwiftBackend(t *testing.T) {
	username := os.Getenv("OS_USERNAME")
	password := os.Getenv("OS_PASSWORD")
	authURL := os.Getenv("OS_AUTH_URL")
	project := os.Getenv("OS_PROJECT_NAME")
	domain := os.Getenv("OS_USER_DOMAIN_NAME")
	projectDomain := os.Getenv("OS_PROJECT_DOMAIN_NAME")
	region := os.Getenv("OS_REGION_NAME")
	tenantID := os.Getenv("OS_TENANT_ID")

	if username == "" || password == "" || authURL == "" {
		cleanup, cfg := testhelpersswift.PrepareTestContainer(t)
		defer cleanup()

		username = cfg.Username
		password = cfg.Password
		authURL = cfg.AuthURL
	}

	ctx := context.Background()
	ts := time.Now().UnixNano()
	container := fmt.Sprintf("openbao-test-%d", ts)

	cleaner := swift.Connection{
		Domain:       domain,
		UserName:     username,
		ApiKey:       password,
		AuthUrl:      authURL,
		Tenant:       project,
		TenantDomain: projectDomain,
		Region:       region,
		TenantId:     tenantID,
		Transport:    cleanhttp.DefaultPooledTransport(),
	}

	if err := cleaner.Authenticate(ctx); err != nil {
		t.Fatalf("err: %s", err)
	}

	if err := cleaner.ContainerCreate(ctx, container, nil); err != nil {
		t.Fatalf("unable to create test container %q: %v", container, err)
	}
	defer func() {
		newObjects, err := cleaner.ObjectNamesAll(ctx, container, nil)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		for _, o := range newObjects {
			if err := cleaner.ObjectDelete(ctx, container, o); err != nil {
				t.Fatalf("err: %s", err)
			}
		}
		if err := cleaner.ContainerDelete(ctx, container); err != nil {
			t.Fatalf("err: %s", err)
		}
	}()

	logger := logging.NewVaultLogger(log.Debug)

	b, err := NewSwiftBackend(map[string]string{
		"username":       username,
		"password":       password,
		"container":      container,
		"auth_url":       authURL,
		"project":        project,
		"domain":         domain,
		"project-domain": projectDomain,
		"tenant_id":      tenantID,
		"region":         region,
	}, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	physical.ExerciseBackend(t, b)
	physical.ExerciseBackend_ListPrefix(t, b)
}
