// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package autoexpose

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// countingHubClient counts ListPorts calls on a mockHubClient and can fail
// the next ones.
type countingHubClient struct {
	*mockHubClient
	lists    atomic.Int32
	failNext atomic.Int32
}

func (c *countingHubClient) ListPorts(ctx context.Context) ([]scionhub.ExposedPort, error) {
	c.lists.Add(1)
	if c.failNext.Load() > 0 {
		c.failNext.Add(-1)
		return nil, errors.New("dial tcp: connection refused")
	}
	return c.mockHubClient.ListPorts(ctx)
}

func newCountingReconciler(t *testing.T, ports ...int) (*Reconciler, *countingHubClient) {
	t.Helper()
	client := &countingHubClient{mockHubClient: newMockHubClient()}
	r := NewReconciler(client, defaultTestConfig())
	r.scan = listening(ports...)
	return r, client
}

// ptone/scion#5243: idle passes never list the registered ports; there is
// no periodic refresh.
func TestReconcileOnce_IdlePassesDoNotListPorts(t *testing.T) {
	r, client := newCountingReconciler(t, 3000)
	ctx := context.Background()

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// First pass: one fetch for the empty cache, one after the register.
	if got := client.lists.Load(); got != 2 {
		t.Fatalf("ListPorts calls after the first pass = %d, want 2", got)
	}
	for i := 0; i < 50; i++ {
		if err := r.reconcileOnce(ctx); err != nil {
			t.Fatalf("idle pass %d: %v", i, err)
		}
		r.nextDelay(nil)
	}
	if got := client.lists.Load(); got != 2 {
		t.Fatalf("ListPorts calls after 50 idle passes = %d, want 2", got)
	}
}

// ptone/scion#5243: one ListPorts after the agent's own change (register or
// delete), none on the pass after it.
func TestReconcileOnce_OwnChangeListsOnce(t *testing.T) {
	r, client := newCountingReconciler(t, 3000)
	ctx := context.Background()
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	for _, step := range []struct {
		name  string
		ports []int
	}{
		{"register", []int{3000, 4000}},
		{"delete", []int{3000}},
	} {
		r.scan = listening(step.ports...)
		before := client.lists.Load()
		if err := r.reconcileOnce(ctx); err != nil {
			t.Fatalf("%s pass: %v", step.name, err)
		}
		if got := client.lists.Load() - before; got != 1 {
			t.Fatalf("%s: ListPorts calls = %d, want 1", step.name, got)
		}
		before = client.lists.Load()
		if err := r.reconcileOnce(ctx); err != nil {
			t.Fatalf("pass after %s: %v", step.name, err)
		}
		if got := client.lists.Load() - before; got != 0 {
			t.Fatalf("pass after %s: ListPorts calls = %d, want 0", step.name, got)
		}
	}
}

// ptone/scion#5243: a failed pass makes the next pass list the ports once;
// the pass after that uses the cache again.
func TestReconcileOnce_FailedListRelistsOnce(t *testing.T) {
	r, client := newCountingReconciler(t, 3000)
	ctx := context.Background()
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	r.cacheStale = true // as after any failed pass (nextDelay)
	client.failNext.Store(1)
	err := r.reconcileOnce(ctx)
	if err == nil {
		t.Fatal("pass with a failing ListPorts should fail")
	}
	r.nextDelay(err)

	before := client.lists.Load()
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("pass after the failure: %v", err)
	}
	if got := client.lists.Load() - before; got != 1 {
		t.Fatalf("ListPorts calls after a failure = %d, want 1", got)
	}
	before = client.lists.Load()
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("second pass after the failure: %v", err)
	}
	if got := client.lists.Load() - before; got != 0 {
		t.Fatalf("ListPorts calls on the second pass after a failure = %d, want 0", got)
	}
}

// ptone/scion#5243: a port the agent exposed and the user removed from the
// registry out of band stays un-exposed while it still listens, also after
// a later fetch (here, the one after another port's register).
func TestReconcileOnce_OutOfBandRemovalIsNotReexposed(t *testing.T) {
	r, client := newCountingReconciler(t, 3000)
	ctx := context.Background()
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !r.autoExposed[3000] {
		t.Fatal("port 3000 should be auto-exposed after the first pass")
	}

	// The user un-exposes 3000 while it still listens.
	if err := client.DeletePort(ctx, 3000); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := r.reconcileOnce(ctx); err != nil {
			t.Fatalf("idle pass %d: %v", i, err)
		}
	}
	if got := client.registeredPorts(); len(got) != 0 {
		t.Fatalf("registered ports after idle passes = %v, want none", got)
	}

	// Another port's register refreshes the cache, which no longer lists
	// 3000: it must still not be exposed again.
	r.scan = listening(3000, 4000)
	for i := 0; i < 3; i++ {
		if err := r.reconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d after the second port: %v", i, err)
		}
	}
	if got := client.registeredPorts(); len(got) != 1 || got[0] != 4000 {
		t.Fatalf("registered ports = %v, want [4000]", got)
	}
	if !r.withdrawn[3000] {
		t.Fatal("port 3000 should be withdrawn")
	}

	// When 3000 stops listening it is dropped (the delete answers 404 or
	// succeeds; either is done) and stays withdrawn: listening again does
	// not expose it.
	r.scan = listening(4000)
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("pass after 3000 stopped: %v", err)
	}
	if r.autoExposed[3000] {
		t.Fatal("port 3000 should no longer be tracked once it stopped listening")
	}
	r.scan = listening(3000, 4000)
	for i := 0; i < 3; i++ {
		if err := r.reconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d after 3000 listened again: %v", i, err)
		}
	}
	if got := client.registeredPorts(); len(got) != 1 || got[0] != 4000 {
		t.Fatalf("registered ports = %v, want [4000]", got)
	}
}
