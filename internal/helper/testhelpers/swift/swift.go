// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package swift

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ncw/swift/v2"

	"github.com/openbao/openbao/sdk/v2/helper/docker"
)

// Well-known TempAuth credentials baked into the openstackswift/saio image.
const (
	Username = "test:tester"
	Password = "testing"
)

// Config describes how to reach a test Swift cluster.
type Config struct {
	AuthURL  string
	Username string
	Password string
}

// PrepareTestContainer starts a disposable, single-node OpenStack Swift
// "SAIO" (Swift All In One) container and returns a connection config for
// it, plus a cleanup function.
func PrepareTestContainer(t *testing.T) (func(), *Config) {
	t.Helper()

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ImageRepo:     "openstackswift/saio",
		ContainerName: "swift",
		ImageTag:      "latest",
		Ports:         []string{"8080/tcp"},
	})
	if err != nil {
		t.Fatalf("swift: could not provision docker service runner: %s", err)
	}

	// The image's default proxy-server pipeline loads the s3api middleware,
	// which opens an AF_ALG (Linux kernel crypto API) socket at import
	// time; that address family isn't available in every container
	// runtime (notably Docker Desktop's VM), which crash-loops the proxy
	// server before it ever binds its listening socket. We don't need
	// S3-compatibility for these tests, so drop it from the pipeline and
	// restart the proxy before the health-check/connect loop below runs.
	runner.RunOptions.PostStart = func(containerID, _ string) error {
		ctx := context.Background()

		patch := []string{"sed", "-i", "s/ s3api / /", "/etc/swift/proxy-server.conf"}
		if _, stderr, code, err := runner.RunCmdWithOutput(ctx, containerID, patch); err != nil {
			return fmt.Errorf("failed to patch proxy-server.conf: %w", err)
		} else if code != 0 {
			return fmt.Errorf("failed to patch proxy-server.conf: exit %d: %s", code, stderr)
		}

		// The image's boot sequence takes a while (observed ~70s under
		// emulation) to get around to registering swift-proxy with
		// s6-svscan at all, so wait for that before trying to signal it.
		const svcDir = "/var/run/s6/services/swift-proxy"
		waitCmd := []string{"test", "-d", svcDir}
		var found bool
		for range 150 {
			if _, _, code, err := runner.RunCmdWithOutput(ctx, containerID, waitCmd); err == nil && code == 0 {
				found = true
				break
			}
			time.Sleep(time.Second)
		}
		if !found {
			return fmt.Errorf("timed out waiting for %s to be registered with s6-svscan", svcDir)
		}

		// s6-svc has no -r; -t (SIGTERM) is enough since s6-supervise
		// automatically respawns the service, picking up the edited config.
		restart := []string{"s6-svc", "-t", svcDir}
		if _, stderr, code, err := runner.RunCmdWithOutput(ctx, containerID, restart); err != nil {
			return fmt.Errorf("failed to restart swift-proxy: %w", err)
		} else if code != 0 {
			return fmt.Errorf("failed to restart swift-proxy: exit %d: %s", code, stderr)
		}

		return nil
	}

	svc, err := runner.StartService(
		context.Background(),
		func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
			authURL := fmt.Sprintf("http://%s:%d/auth/v1.0", host, port)

			c := swift.Connection{
				UserName: Username,
				ApiKey:   Password,
				AuthUrl:  authURL,
			}
			if err := c.Authenticate(ctx); err != nil {
				return nil, err
			}

			return docker.NewServiceURLParse(authURL)
		},
	)
	if err != nil {
		t.Fatalf("swift: could not start container: %s", err)
	}

	return svc.Cleanup, &Config{
		AuthURL:  svc.Config.URL().String(),
		Username: Username,
		Password: Password,
	}
}
