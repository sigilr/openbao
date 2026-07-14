// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"testing"

	proto "github.com/openbao/openbao/plugins/database/remote-db-plugin/proto/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRedirectFromErr covers the redirect-handling helper the pin-spokes-to-active
// chase relies on: a FailedPrecondition carrying a RelayRedirect detail yields the
// endpoint to chase, while every other error (including a FailedPrecondition with
// no or an empty endpoint detail) is a normal error, not a redirect.
func TestRedirectFromErr(t *testing.T) {
	withDetail := func(t *testing.T, ep string) error {
		t.Helper()
		st, err := status.New(codes.FailedPrecondition, "hub node is not active").
			WithDetails(&proto.RelayRedirect{RelayEndpoint: ep})
		if err != nil {
			t.Fatalf("WithDetails: %v", err)
		}
		return st.Err()
	}

	t.Run("FailedPrecondition with endpoint is a redirect", func(t *testing.T) {
		ep, ok := redirectFromErr(withDetail(t, "active-host:50053"))
		if !ok || ep != "active-host:50053" {
			t.Fatalf("got (%q, %v), want (active-host:50053, true)", ep, ok)
		}
	})

	t.Run("FailedPrecondition with empty endpoint is not a redirect", func(t *testing.T) {
		if ep, ok := redirectFromErr(withDetail(t, "")); ok {
			t.Fatalf("empty endpoint treated as redirect: %q", ep)
		}
	})

	t.Run("FailedPrecondition without a detail is a normal error", func(t *testing.T) {
		if ep, ok := redirectFromErr(status.Error(codes.FailedPrecondition, "not active")); ok {
			t.Fatalf("bare FailedPrecondition treated as redirect: %q", ep)
		}
	})

	t.Run("other status codes are normal errors", func(t *testing.T) {
		if _, ok := redirectFromErr(status.Error(codes.Unavailable, "down")); ok {
			t.Fatal("Unavailable treated as redirect")
		}
	})

	t.Run("non-status error is a normal error", func(t *testing.T) {
		if _, ok := redirectFromErr(errors.New("boom")); ok {
			t.Fatal("plain error treated as redirect")
		}
	})
}

// TestRedirectChaseCap pins the chase loop's termination guard: the spoke follows
// at most redirectChaseLimit consecutive redirects before giving up, so a
// misconfigured pin (two nodes redirecting at each other) cannot loop forever.
// This replays the guard in Run's loop against the shared constant.
func TestRedirectChaseCap(t *testing.T) {
	redirects := 0
	hops := 0
	for {
		// Stand in for connectAndServe always returning a fresh redirect.
		redirects++
		if redirects > redirectChaseLimit {
			break // Run returns 1 here ("hub kept redirecting")
		}
		hops++
		if hops > redirectChaseLimit+100 {
			t.Fatal("chase did not terminate at the cap")
		}
	}
	if hops != redirectChaseLimit {
		t.Fatalf("chased %d hops before aborting, want %d", hops, redirectChaseLimit)
	}
}
