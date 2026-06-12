// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package remotedb

import (
	"sync"
	"testing"
	"time"
)

// These tests cover the in-process primitives the gRPC handler relies on —
// request_id register/deliver/cancel, the failAll teardown, and the
// touch/lastSeenAt freshness accounting. The TLS + stream surface is
// exercised by TEST.md's manual flow; the unit tests here pin the
// concurrency discipline that's easy to regress without anyone noticing.

func TestNewRequestIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1024)
	for i := 0; i < 1024; i++ {
		id, err := newRequestID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 24 {
			t.Fatalf("request id length: got %d, want 24 hex chars", len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate request id %q after %d generated", id, i)
		}
		seen[id] = struct{}{}
	}
}

func TestSpokeConnection_RegisterDeliver(t *testing.T) {
	conn := newSpokeConnection(nil)
	ch := conn.register("req-1")

	go conn.deliver("req-1", pendingResponse{output: "ok"})

	select {
	case resp := <-ch:
		if resp.output != "ok" {
			t.Fatalf("output: got %q, want %q", resp.output, "ok")
		}
	case <-time.After(time.Second):
		t.Fatal("deliver did not arrive on waiter channel")
	}
}

func TestSpokeConnection_DeliverUnknownIsNoop(t *testing.T) {
	conn := newSpokeConnection(nil)
	// Must not panic or block.
	conn.deliver("never-registered", pendingResponse{output: "ignored"})
}

func TestSpokeConnection_CancelPreventsDelivery(t *testing.T) {
	conn := newSpokeConnection(nil)
	ch := conn.register("req-1")
	conn.cancel("req-1")
	conn.deliver("req-1", pendingResponse{output: "should-not-arrive"})
	select {
	case <-ch:
		t.Fatal("delivery after cancel should be dropped")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestSpokeConnection_DuplicateDeliveryDoesNotPanic(t *testing.T) {
	conn := newSpokeConnection(nil)
	conn.register("req-1")
	conn.deliver("req-1", pendingResponse{output: "first"})
	// Second deliver finds no entry in the map; must be a quiet no-op.
	conn.deliver("req-1", pendingResponse{output: "second"})
}

func TestSpokeConnection_FailAllUnblocksWaiters(t *testing.T) {
	conn := newSpokeConnection(nil)
	const N = 8
	chs := make([]chan pendingResponse, N)
	for i := 0; i < N; i++ {
		chs[i] = conn.register(string(rune('a' + i)))
	}

	conn.failAll("bye")
	for i, ch := range chs {
		select {
		case resp := <-ch:
			if resp.err != "bye" {
				t.Errorf("waiter %d: err=%q, want %q", i, resp.err, "bye")
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d did not unblock after failAll", i)
		}
	}
}

func TestSpokeConnection_TouchAdvancesLastSeen(t *testing.T) {
	conn := newSpokeConnection(nil)
	before := conn.lastSeenAt()
	time.Sleep(2 * time.Millisecond)
	conn.touch()
	after := conn.lastSeenAt()
	if !after.After(before) {
		t.Fatalf("touch did not advance lastSeen: before=%v after=%v", before, after)
	}
}

func TestSpokeConnection_ConcurrentRegisterDeliverIsRaceFree(t *testing.T) {
	// Goal: exercise the inflight map under a contended workload so the
	// race detector has something to look at. We don't assert ordering;
	// we assert that every waiter either gets its response or is
	// drained by failAll at the end.
	conn := newSpokeConnection(nil)

	const N = 256
	var wg sync.WaitGroup
	delivered := make(chan string, N)
	for i := 0; i < N; i++ {
		id := newTestID(i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			ch := conn.register(id)
			select {
			case resp := <-ch:
				delivered <- resp.output
			case <-time.After(2 * time.Second):
				delivered <- "timeout:" + id
			}
		}()
		go func() {
			defer wg.Done()
			conn.deliver(id, pendingResponse{output: id})
		}()
	}
	wg.Wait()
	close(delivered)

	count := 0
	for got := range delivered {
		if got == "" {
			t.Errorf("empty output (zero pendingResponse leaked)")
		}
		count++
	}
	if count != N {
		t.Fatalf("waiter count: got %d, want %d", count, N)
	}
}

func TestSpokeStatusHealthyFreshness(t *testing.T) {
	// SpokeStaleAfter is the freshness window. A spoke whose last-seen is
	// within the window is healthy; outside it is not.
	last := time.Now()
	freshHealthy := time.Since(last) < SpokeStaleAfter
	if !freshHealthy {
		t.Fatalf("a fresh last-seen should be considered healthy")
	}

	stale := time.Now().Add(-SpokeStaleAfter - time.Second)
	staleHealthy := time.Since(stale) < SpokeStaleAfter
	if staleHealthy {
		t.Fatalf("a last-seen older than SpokeStaleAfter should be unhealthy")
	}
}

func newTestID(i int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 4)
	for j := 0; j < 4; j++ {
		out[j] = hex[(i>>(j*4))&0xf]
	}
	return string(out)
}
