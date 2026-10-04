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
// listing deadline over a blocking default target ("docker", agent d1) and
// a blocking auxiliary target (k8sTargetB, agent a1). Neither blocks until
// block is called on it.
func newLivenessService(client *mockRuntimeBrokerService, interval, deadline time.Duration) (*HeartbeatService, *blockingListManager, *blockingListManager) {
	defaultMgr := &blockingListManager{namedHeartbeatManager: namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "d1", ProjectID: "p1", Phase: "running"},
		}},
		name: "docker",
	}}
	auxMgr := &blockingListManager{namedHeartbeatManager: namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "a1", ProjectID: "p1", Phase: "running"},
		}},
		name:     "kubernetes",
		targetID: k8sTargetB,
	}}
	svc := NewHeartbeatService(client, "b1", interval, defaultMgr, nil, slog.Default())
	// Below MinHeartbeatInterval, which NewHeartbeatService enforces.
	svc.interval = interval
	svc.listingDeadline = deadline
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }
	return svc, defaultMgr, auxMgr
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

// assertHeartbeat checks that a heartbeat is online, reports the wanted
// inventory and exactly the wanted agents (slug -> runtime target).
func assertHeartbeat(t *testing.T, hb *hubclient.BrokerHeartbeat, wantTargets []hubclient.InventoryTarget, wantAgents map[string]string) {
	t.Helper()
	if hb.Status != "online" {
		t.Errorf("status = %q, want online", hb.Status)
	}
	if hb.Capabilities == nil {
		t.Error("capabilities missing from heartbeat")
	}
	if hb.Inventory == nil || !reflect.DeepEqual(hb.Inventory.Targets, wantTargets) {
		t.Errorf("inventory = %+v, want targets %+v", hb.Inventory, wantTargets)
	}
	if got := heartbeatAgentTargets(hb); !reflect.DeepEqual(got, wantAgents) {
		t.Errorf("agents = %v, want %v", got, wantAgents)
	}
}

var (
	allComplete = []hubclient.InventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: true},
		{ID: k8sTargetB, Runtime: "kubernetes", Complete: true},
	}
	defaultPending = []hubclient.InventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: false},
		{ID: k8sTargetB, Runtime: "kubernetes", Complete: true},
	}
	allPending = []hubclient.InventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: false},
		{ID: k8sTargetB, Runtime: "kubernetes", Complete: false},
	}
	allAgents = map[string]string{"d1": "docker", "a1": k8sTargetB}
)

// A default-runtime listing that hangs does not hold back the heartbeat:
// heartbeats keep going out every interval, online, with the default target
// incomplete and the auxiliary target's agents still reported complete. The
// hung listing is not started again while it hangs.
func TestHeartbeatLiveness_SlowListingStillSendsHeartbeat(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	const interval = 300 * time.Millisecond
	svc, defaultMgr, _ := newLivenessService(client, interval, 30*time.Millisecond)
	defaultMgr.block()

	start := time.Now()
	svc.Start(context.Background())
	// Deferred calls run in reverse: release the listing before Stop, so a
	// failure here cannot leave Stop waiting on a hung listing.
	defer svc.Stop()
	defer defaultMgr.unblock()

	calls := waitForHeartbeats(t, client, 3, 5*time.Second)
	if first := calls[0].Time.Sub(start); first >= interval {
		t.Errorf("first heartbeat after %v, want within one interval (%v)", first, interval)
	}
	for i := 1; i < len(calls); i++ {
		if gap := calls[i].Time.Sub(calls[i-1].Time); gap > 3*interval {
			t.Errorf("gap between heartbeats %d and %d = %v, want about %v", i-1, i, gap, interval)
		}
	}
	for _, c := range calls {
		assertHeartbeat(t, c.Heartbeat, defaultPending, map[string]string{"a1": k8sTargetB})
	}
	if got := defaultMgr.calls.Load(); got != 1 {
		t.Errorf("default runtime listed %d times while the first listing hung, want 1", got)
	}
}

// When every target hangs, the heartbeat still goes out online, with no
// agent data and every target incomplete, so the Hub keeps every agent's
// recorded state.
func TestHeartbeatLiveness_AllTargetsHang(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, defaultMgr, auxMgr := newLivenessService(client, 300*time.Millisecond, 30*time.Millisecond)
	defaultMgr.block()
	auxMgr.block()

	svc.Start(context.Background())
	defer svc.Stop()
	defer defaultMgr.unblock()
	defer auxMgr.unblock()

	calls := waitForHeartbeats(t, client, 2, 5*time.Second)
	for _, c := range calls {
		assertHeartbeat(t, c.Heartbeat, allPending, map[string]string{})
		if len(c.Heartbeat.Projects) != 0 {
			t.Errorf("projects = %+v, want none", c.Heartbeat.Projects)
		}
	}
}

// Once the listing is fast again, the next heartbeat carries a fresh,
// complete listing of every target.
func TestHeartbeatLiveness_ListingRecovers(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, defaultMgr, _ := newLivenessService(client, 200*time.Millisecond, 30*time.Millisecond)
	defaultMgr.block()

	svc.Start(context.Background())
	defer svc.Stop()
	defer defaultMgr.unblock()

	calls := waitForHeartbeats(t, client, 1, 5*time.Second)
	assertHeartbeat(t, calls[0].Heartbeat, defaultPending, map[string]string{"a1": k8sTargetB})

	defaultMgr.unblock()
	listedBefore := defaultMgr.calls.Load()

	deadline := time.Now().Add(5 * time.Second)
	for {
		calls = client.getHeartbeatCalls()
		hb := calls[len(calls)-1].Heartbeat
		if _, ok := heartbeatAgentTargets(hb)["d1"]; ok {
			assertHeartbeat(t, hb, allComplete, allAgents)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat with the default target's agents after the runtime recovered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if defaultMgr.calls.Load() <= listedBefore {
		t.Error("the recovered heartbeat did not come from a new listing")
	}
}

// A heartbeat that starts while another heartbeat's listing is still within
// its deadline waits for that listing and reports its result, instead of
// going out without agent data or listing the runtime a second time.
func TestHeartbeatLiveness_ConcurrentHeartbeatJoinsListing(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, defaultMgr, _ := newLivenessService(client, time.Hour, 5*time.Second)
	joined := watchJoins(svc)
	defaultMgr.block()
	defer defaultMgr.unblock()

	errs := make(chan error, 2)
	go func() { errs <- svc.ForceHeartbeat(context.Background()) }()
	waitFor(t, 3*time.Second, "the first listing to start", func() bool { return defaultMgr.calls.Load() == 1 })
	go func() { errs <- svc.ForceHeartbeat(context.Background()) }()
	// The second heartbeat must join the listing in progress, not list again.
	waitForJoin(t, joined, "docker")
	defaultMgr.unblock()

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("ForceHeartbeat: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("ForceHeartbeat did not return")
		}
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 2 {
		t.Fatalf("got %d heartbeats, want 2", len(calls))
	}
	for _, c := range calls {
		assertHeartbeat(t, c.Heartbeat, allComplete, allAgents)
	}
	if got := defaultMgr.calls.Load(); got != 1 {
		t.Errorf("default runtime listed %d times, want 1 shared listing", got)
	}
}

// Stop returns promptly while a listing hangs, even when the listing
// deadline is long, and cancels the listing's context.
func TestHeartbeatLiveness_StopDoesNotWaitForListing(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, defaultMgr, _ := newLivenessService(client, time.Hour, time.Hour)

	var listCtxErr atomic.Value
	listing := make(chan struct{})
	ctxMgr := &ctxAwareListManager{namedHeartbeatManager: defaultMgr.namedHeartbeatManager, started: listing, ctxErr: &listCtxErr}
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
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return while the listing was in progress")
	}

	waitFor(t, 3*time.Second, "the listing context to be cancelled by Stop", func() bool { return listCtxErr.Load() != nil })
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

func TestDefaultListingDeadline(t *testing.T) {
	for _, tc := range []struct {
		interval, want time.Duration
	}{
		{5 * time.Second, 3750 * time.Millisecond}, // 3/4 of the interval
		{10 * time.Second, 7500 * time.Millisecond},
		{15 * time.Second, 10 * time.Second}, // the floor
		{20 * time.Second, 10 * time.Second},
		{30 * time.Second, 15 * time.Second}, // half the interval
		{time.Minute, 30 * time.Second},
	} {
		if got := defaultListingDeadline(tc.interval); got != tc.want {
			t.Errorf("defaultListingDeadline(%v) = %v, want %v", tc.interval, got, tc.want)
		}
		if got := defaultListingDeadline(tc.interval); got >= tc.interval {
			t.Errorf("defaultListingDeadline(%v) = %v, not below the interval", tc.interval, got)
		}
	}
}

// watchJoins reports each target key for which a heartbeat joins a listing
// already in progress.
func watchJoins(svc *HeartbeatService) <-chan string {
	joined := make(chan string, 16)
	svc.mu.Lock()
	svc.joinedListing = func(key string) { joined <- key }
	svc.mu.Unlock()
	return joined
}

func waitForJoin(t *testing.T, joined <-chan string, key string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-joined:
			if got == key {
				return
			}
		case <-timeout:
			t.Fatalf("no heartbeat joined the %s listing within 5s", key)
		}
	}
}

// A heartbeat never writes into a listing's result, which concurrent
// heartbeats may share: merging the auxiliary agents must not use spare
// capacity in the default target's slice.
func TestHeartbeatLiveness_SharedListingNotModified(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, defaultMgr, _ := newLivenessService(client, time.Hour, 5*time.Second)
	listed := make([]api.AgentInfo, 1, 8)
	listed[0] = api.AgentInfo{Name: "d1", ProjectID: "p1", Phase: "running"}
	defaultMgr.agents = listed

	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d heartbeats, want 1", len(calls))
	}
	assertHeartbeat(t, calls[0].Heartbeat, allComplete, allAgents)
	if spare := listed[:2][1]; spare.Name != "" {
		t.Errorf("heartbeat wrote agent %q into the listing's spare capacity", spare.Name)
	}
}

// A heartbeat that joined a listing before its deadline, and whose own
// deadline is later, does not report that listing's result when it arrives
// after the listing's deadline.
func TestHeartbeatLiveness_LateResultNotReported(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc, defaultMgr, _ := newLivenessService(client, time.Hour, time.Second)
	joined := watchJoins(svc)
	defaultMgr.block()
	defer defaultMgr.unblock()

	errs := make(chan error, 2)
	go func() { errs <- svc.ForceHeartbeat(context.Background()) }()
	waitFor(t, 3*time.Second, "the first listing to start", func() bool { return defaultMgr.calls.Load() == 1 })
	svc.mu.Lock()
	svc.listingDeadline = 10 * time.Second
	svc.mu.Unlock()
	// Joins the listing (deadline 1s) and waits up to 10s for it.
	go func() { errs <- svc.ForceHeartbeat(context.Background()) }()
	waitForJoin(t, joined, "docker")
	// The first heartbeat goes out once the listing's deadline has passed.
	waitForHeartbeats(t, client, 1, 5*time.Second)
	defaultMgr.unblock() // the listing returns late

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("ForceHeartbeat: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("ForceHeartbeat did not return")
		}
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 2 {
		t.Fatalf("got %d heartbeats, want 2", len(calls))
	}
	for _, c := range calls {
		assertHeartbeat(t, c.Heartbeat, defaultPending, map[string]string{"a1": k8sTargetB})
	}
	if got := defaultMgr.calls.Load(); got != 1 {
		t.Errorf("default runtime listed %d times, want 1", got)
	}
}
