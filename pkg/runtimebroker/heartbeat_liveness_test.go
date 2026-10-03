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

package runtimebroker

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// blockingListManager is a named manager whose List blocks, without
// honouring its context, while a release channel is set, as a container
// runtime CLI stalled under host load does.
type blockingListManager struct {
	namedHeartbeatManager
	mu      sync.Mutex
	release chan struct{}
	calls   atomic.Int32
}

func (m *blockingListManager) block() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.release = make(chan struct{})
}

func (m *blockingListManager) unblock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.release != nil {
		close(m.release)
		m.release = nil
	}
}

func (m *blockingListManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.calls.Add(1)
	m.mu.Lock()
	ch := m.release
	m.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return m.agents, m.err
}

// newLivenessService returns a heartbeat service with a short interval and
// listing deadline over a blocking default manager and one auxiliary target.
func newLivenessService(client *mockRuntimeBrokerService, interval, deadline time.Duration) (*HeartbeatService, *blockingListManager) {
	defaultMgr := &blockingListManager{namedHeartbeatManager: namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "d1", ProjectID: "p1", Phase: "running"},
		}},
		name: "docker",
	}}
	auxMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "a1", ProjectID: "p1", Phase: "running"},
		}},
		name:     "kubernetes",
		targetID: k8sTargetB,
	}
	svc := NewHeartbeatService(client, "b1", interval, defaultMgr, nil, slog.Default())
	// Below MinHeartbeatInterval, which NewHeartbeatService enforces.
	svc.interval = interval
	svc.listingDeadline = deadline
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }
	return svc, defaultMgr
}

// waitForHeartbeats waits until at least n heartbeats were sent.
func waitForHeartbeats(t *testing.T, client *mockRuntimeBrokerService, n int, timeout time.Duration) []mockHeartbeatCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		calls := client.getHeartbeatCalls()
		if len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d heartbeats within %v, want at least %d", len(calls), timeout, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var incompleteTargets = []hubclient.InventoryTarget{
	{ID: "docker", Runtime: "docker", Complete: false},
	{ID: k8sTargetB, Runtime: "kubernetes", Complete: false},
}

// assertLivenessOnly checks that a heartbeat keeps the broker online but
// carries no agent data and claims no target as complete, so the Hub keeps
// every agent's recorded state.
func assertLivenessOnly(t *testing.T, hb *hubclient.BrokerHeartbeat) {
	t.Helper()
	if hb.Status != "online" {
		t.Errorf("status = %q, want online", hb.Status)
	}
	if len(hb.Projects) != 0 {
		t.Errorf("projects = %+v, want none", hb.Projects)
	}
	if hb.Inventory == nil || !reflect.DeepEqual(hb.Inventory.Targets, incompleteTargets) {
		t.Errorf("inventory = %+v, want every target incomplete %+v", hb.Inventory, incompleteTargets)
	}
	if hb.Capabilities == nil {
		t.Error("capabilities missing from heartbeat")
	}
}

// A listing that hangs does not hold back the heartbeat: heartbeats keep
// going out every interval, each one online with no agent data and every
// target incomplete, and no second listing is started while the first hangs.
func TestHeartbeatLiveness_SlowListingStillSendsHeartbeat(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	const interval = 150 * time.Millisecond
	svc, mgr := newLivenessService(client, interval, 30*time.Millisecond)
	mgr.block()
	defer mgr.unblock()

	start := time.Now()
	svc.Start(context.Background())
	defer svc.Stop()

	calls := waitForHeartbeats(t, client, 3, 3*time.Second)
	if first := calls[0].Time.Sub(start); first >= interval {
		t.Errorf("first heartbeat after %v, want within one interval (%v)", first, interval)
	}
	for i := 1; i < len(calls); i++ {
		if gap := calls[i].Time.Sub(calls[i-1].Time); gap > 2*interval {
			t.Errorf("gap between heartbeats %d and %d = %v, want about %v", i-1, i, gap, interval)
		}
	}
	for _, c := range calls {
		assertLivenessOnly(t, c.Heartbeat)
	}
	if got := mgr.calls.Load(); got != 1 {
		t.Errorf("default runtime listed %d times while the first listing hung, want 1", got)
	}
}

// Once the listing is fast again, the next heartbeat carries a fresh,
// complete listing.
func TestHeartbeatLiveness_ListingRecovers(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, mgr := newLivenessService(client, 100*time.Millisecond, 30*time.Millisecond)
	mgr.block()
	defer mgr.unblock()

	svc.Start(context.Background())
	defer svc.Stop()

	calls := waitForHeartbeats(t, client, 1, 3*time.Second)
	assertLivenessOnly(t, calls[0].Heartbeat)

	mgr.unblock()
	listedBefore := mgr.calls.Load()

	deadline := time.Now().Add(3 * time.Second)
	for {
		calls = client.getHeartbeatCalls()
		hb := calls[len(calls)-1].Heartbeat
		if len(hb.Projects) > 0 {
			want := []hubclient.InventoryTarget{
				{ID: "docker", Runtime: "docker", Complete: true},
				{ID: k8sTargetB, Runtime: "kubernetes", Complete: true},
			}
			if !reflect.DeepEqual(hb.Inventory.Targets, want) {
				t.Errorf("targets = %+v, want %+v", hb.Inventory.Targets, want)
			}
			if got, want := heartbeatAgentTargets(hb), map[string]string{"d1": "docker", "a1": k8sTargetB}; !reflect.DeepEqual(got, want) {
				t.Errorf("agent targets = %v, want %v", got, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat with a listing after the runtime recovered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if mgr.calls.Load() <= listedBefore {
		t.Error("the recovered heartbeat did not come from a new listing")
	}
}

// Stop returns promptly while a listing hangs, even when the listing
// deadline is long, and cancels the listing's context.
func TestHeartbeatLiveness_StopDoesNotWaitForListing(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, mgr := newLivenessService(client, time.Hour, time.Hour)

	var listCtxErr atomic.Value
	listing := make(chan struct{})
	ctxMgr := &ctxAwareListManager{namedHeartbeatManager: mgr.namedHeartbeatManager, started: listing, ctxErr: &listCtxErr}
	svc.SwapManager(ctxMgr)

	svc.Start(context.Background())
	select {
	case <-listing:
	case <-time.After(3 * time.Second):
		t.Fatal("initial listing did not start")
	}

	stopped := make(chan struct{})
	go func() {
		svc.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return while the listing was in progress")
	}

	deadline := time.Now().Add(2 * time.Second)
	for listCtxErr.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the listing context was not cancelled by Stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if svc.IsRunning() {
		t.Error("service still running after Stop")
	}
}

// ctxAwareListManager blocks in List until its context is done and records
// the context error.
type ctxAwareListManager struct {
	namedHeartbeatManager
	started chan struct{}
	once    sync.Once
	ctxErr  *atomic.Value
}

func (m *ctxAwareListManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	m.ctxErr.Store(ctx.Err())
	return nil, ctx.Err()
}
