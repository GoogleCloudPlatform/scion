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

//go:build !no_sqlite

package hub

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcileFixture is a hub with one online broker, one project and helpers
// to drive heartbeats that carry an inventory.
type reconcileFixture struct {
	t         *testing.T
	srv       *Server
	s         store.Store
	brokerID  string
	projectID string
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("rc-broker"),
		Name:    "RC Broker",
		Slug:    "rc-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(ctx, broker.ID, store.BrokerStatusOnline))

	project := &store.Project{
		ID:      tid("rc-project"),
		Slug:    "rc-project",
		Name:    "RC Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	return &reconcileFixture{t: t, srv: srv, s: s, brokerID: broker.ID, projectID: project.ID}
}

// addAgent creates an agent of the fixture's broker, last seen long ago.
func (f *reconcileFixture) addAgent(slug, phase, activity string, mutate ...func(a *store.Agent)) *store.Agent {
	f.t.Helper()
	a := &store.Agent{
		ID:              tid("rc-" + slug),
		Slug:            slug,
		Name:            slug,
		Template:        "default",
		ProjectID:       f.projectID,
		RuntimeBrokerID: f.brokerID,
		Runtime:         "docker",
		AppliedConfig:   &store.AgentAppliedConfig{RuntimeTarget: "docker"},
		Phase:           phase,
		Activity:        activity,
		LastSeen:        time.Now().Add(-time.Hour),
		Labels:          map[string]string{},
	}
	for _, m := range mutate {
		m(a)
	}
	require.NoError(f.t, f.s.CreateAgent(context.Background(), a))
	return a
}

// completeInventory reports the given targets (default: "docker") as
// completely listed.
func completeInventory(targets ...string) *brokerInventory {
	if len(targets) == 0 {
		targets = []string{"docker"}
	}
	inv := &brokerInventory{}
	for _, id := range targets {
		inv.Targets = append(inv.Targets, brokerInventoryTarget{ID: id, Complete: true})
	}
	return inv
}

// heartbeat sends an online heartbeat that reports the given slugs as
// running agents of the fixture's project.
func (f *reconcileFixture) heartbeat(inv *brokerInventory, slugs ...string) {
	f.t.Helper()
	agents := make([]brokerAgentHeartbeat, 0, len(slugs))
	for _, slug := range slugs {
		agents = append(agents, brokerAgentHeartbeat{Slug: slug, Phase: "running", Activity: "working", RuntimeTarget: "docker"})
	}
	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: inv,
		Projects:  []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: agents}},
	})
}

func (f *reconcileFixture) send(hb brokerHeartbeatRequest) {
	f.t.Helper()
	rec := doRequest(f.t, f.srv, http.MethodPost, "/api/v1/runtime-brokers/"+f.brokerID+"/heartbeat", hb)
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
}

// expireClock moves an agent's first-missing time past the grace period, as
// if it had been absent from complete inventories for that long.
func (f *reconcileFixture) expireClock(agentID string) {
	f.t.Helper()
	tr := &f.srv.missingAgents
	tr.mu.Lock()
	defer tr.mu.Unlock()
	m := tr.since[f.brokerID]
	require.Contains(f.t, m, agentID, "agent has no missing clock")
	m[agentID] = time.Now().Add(-2 * f.srv.missingAgentGrace())
}

func (f *reconcileFixture) hasClock(agentID string) bool {
	tr := &f.srv.missingAgents
	tr.mu.Lock()
	defer tr.mu.Unlock()
	_, ok := tr.since[f.brokerID][agentID]
	return ok
}

func (f *reconcileFixture) get(id string) *store.Agent {
	f.t.Helper()
	a, err := f.s.GetAgent(context.Background(), id)
	require.NoError(f.t, err)
	return a
}

func (f *reconcileFixture) assertReconciled(id string) {
	f.t.Helper()
	a := f.get(id)
	assert.Equal(f.t, string(state.PhaseError), a.Phase)
	assert.Equal(f.t, string(state.ExitReasonContainerMissing), a.ExitReason)
	assert.Equal(f.t, "", a.Activity)
	assert.NotEmpty(f.t, a.Message)
}

func (f *reconcileFixture) assertUntouched(id, phase string) {
	f.t.Helper()
	a := f.get(id)
	assert.Equal(f.t, phase, a.Phase)
	assert.Empty(f.t, a.ExitReason)
}

// TestReconcileMissing_IssueScenario: two running agents, one of them blocked;
// the heartbeat omits the blocked one. Within the grace period nothing
// happens; after it, the omitted agent is terminal with exit reason
// container_missing and the reported agent is unchanged.
func TestReconcileMissing_IssueScenario(t *testing.T) {
	f := newReconcileFixture(t)
	reported := f.addAgent("reported", "running", "working")
	omitted := f.addAgent("omitted", "running", "blocked")

	f.heartbeat(completeInventory(), reported.Slug)
	f.assertUntouched(omitted.ID, "running")
	assert.Equal(t, "blocked", f.get(omitted.ID).Activity, "within the grace period the agent is left alone")
	require.True(t, f.hasClock(omitted.ID))
	assert.False(t, f.hasClock(reported.ID))

	f.expireClock(omitted.ID)
	f.heartbeat(completeInventory(), reported.Slug)

	f.assertReconciled(omitted.ID)
	assert.Equal(t, "missing", f.get(omitted.ID).ContainerStatus)
	got := f.get(reported.ID)
	assert.Equal(t, "running", got.Phase)
	assert.Equal(t, "working", got.Activity)
	assert.Empty(t, got.ExitReason)
	assert.False(t, f.hasClock(omitted.ID), "clock is dropped after the reconcile")
}

// TestReconcileMissing_PeriodicWritesDoNotDelay: the hub's periodic sweeps
// write the agent row (and bump its updated timestamp) during the grace
// period; the agent is still reconciled.
func TestReconcileMissing_PeriodicWritesDoNotDelay(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	omitted := f.addAgent("omitted", "running", "working", func(a *store.Agent) {
		a.LastActivityEvent = time.Now().Add(-time.Hour)
	})

	f.heartbeat(completeInventory())
	before := f.get(omitted.ID).Updated

	time.Sleep(5 * time.Millisecond)
	_, err := f.s.MarkStaleAgentsOffline(ctx, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.s.UpdateAgent(ctx, func() *store.Agent {
		a := f.get(omitted.ID)
		a.TaskSummary = "periodic write"
		return a
	}()))
	mid := f.get(omitted.ID)
	require.True(t, mid.Updated.After(before), "periodic writes bump updated")
	assert.Equal(t, "offline", mid.Activity)

	f.expireClock(omitted.ID)
	f.heartbeat(completeInventory())
	f.assertReconciled(omitted.ID)
}

// TestReconcileMissing_RecentLastSeenNotReconciled: an agent whose last_seen
// is recent (for example its own status reports still arrive) is left alone
// even when its missing clock has expired.
func TestReconcileMissing_RecentLastSeenNotReconciled(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("recent", "running", "working", func(a *store.Agent) { a.LastSeen = time.Now() })

	f.heartbeat(completeInventory())
	f.expireClock(a.ID)
	f.heartbeat(completeInventory())
	f.assertUntouched(a.ID, "running")
}

// TestReconcileMissing_NonRunningPhasesNotReconciled covers dispatch phases
// (container may not exist yet) and phases whose container is intentionally
// absent.
func TestReconcileMissing_NonRunningPhasesNotReconciled(t *testing.T) {
	phases := []string{"created", "provisioning", "cloning", "starting", "suspended", "stopping", "stopped"}
	f := newReconcileFixture(t)
	ids := map[string]string{}
	for _, p := range phases {
		ids[p] = f.addAgent("phase-"+p, p, "").ID
	}

	f.heartbeat(completeInventory())
	for _, p := range phases {
		assert.False(t, f.hasClock(ids[p]), "phase %s must not start a missing clock", p)
	}
	f.heartbeat(completeInventory())
	for _, p := range phases {
		f.assertUntouched(ids[p], p)
	}
}

// reconcileBlockedCase runs a scenario in which the agent's clock would have
// expired, then asserts the agent is still running.
func reconcileBlockedCase(t *testing.T, f *reconcileFixture, a *store.Agent, second func()) {
	t.Helper()
	// First heartbeat: a normal complete inventory starts the clock.
	f.heartbeat(completeInventory())
	require.True(t, f.hasClock(a.ID))
	f.expireClock(a.ID)
	second()
	f.assertUntouched(a.ID, "running")
}

func TestReconcileMissing_GateBlocks(t *testing.T) {
	t.Run("stale broker", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			ctx := context.Background()
			b, err := f.s.GetRuntimeBroker(ctx, f.brokerID)
			require.NoError(t, err)
			b.LastHeartbeat = time.Now().Add(-time.Hour)
			require.NoError(t, f.s.UpdateRuntimeBroker(ctx, b))
			f.heartbeat(completeInventory())
		})
		assert.False(t, f.hasClock(a.ID), "a returning broker clears the clocks")
		// The next heartbeat starts counting again from zero.
		f.heartbeat(completeInventory())
		assert.True(t, f.hasClock(a.ID))
		f.assertUntouched(a.ID, "running")
	})

	t.Run("broker was offline", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			require.NoError(t, f.s.UpdateRuntimeBrokerHeartbeat(context.Background(), f.brokerID, store.BrokerStatusOffline))
			f.heartbeat(completeInventory())
		})
	})

	t.Run("heartbeat not online", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			f.send(brokerHeartbeatRequest{Status: store.BrokerStatusDegraded, Inventory: completeInventory()})
		})
		assert.False(t, f.hasClock(a.ID))
	})

	t.Run("incomplete inventory", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			f.heartbeat(&brokerInventory{Targets: []brokerInventoryTarget{{ID: "docker", Complete: false}}})
		})
		assert.False(t, f.hasClock(a.ID), "an incomplete inventory resets the clocks")
	})

	t.Run("filtered heartbeat claims no target", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			f.heartbeat(&brokerInventory{})
		})
	})

	t.Run("no inventory (older broker)", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			f.heartbeat(nil)
		})
	})
}

func TestReconcileMissing_Exclusions(t *testing.T) {
	t.Run("lifecycle operation in flight", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			end := f.srv.beginLifecycleOp(a.ID)
			defer end()
			f.heartbeat(completeInventory())
		})
	})

	t.Run("pending lifecycle dispatch", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			require.NoError(t, f.s.InsertBrokerDispatch(context.Background(), &store.BrokerDispatch{
				ID:        tid("rc-dispatch"),
				BrokerID:  f.brokerID,
				AgentID:   a.ID,
				AgentSlug: a.Slug,
				ProjectID: f.projectID,
				Op:        "restart",
				State:     "pending",
			}))
			f.heartbeat(completeInventory())
		})
	})

	t.Run("reincarnation in flight", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			cur := f.get(a.ID)
			cur.ReincarnationState = store.ReincarnationStateStarting
			require.NoError(t, f.s.UpdateAgent(context.Background(), cur))
			f.heartbeat(completeInventory())
		})
	})

	t.Run("recorded target absent from the heartbeat", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working", withRuntimeTarget(k8sTargetB))
		f.heartbeat(completeInventory("docker"))
		assert.False(t, f.hasClock(a.ID))
		f.assertUntouched(a.ID, "running")
	})

	t.Run("no recorded target", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working", withRuntimeTarget(""))
		f.heartbeat(completeInventory("docker"))
		assert.False(t, f.hasClock(a.ID))
		f.assertUntouched(a.ID, "running")
	})

	t.Run("no applied config", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working", func(a *store.Agent) { a.AppliedConfig = nil })
		f.heartbeat(completeInventory("docker"))
		assert.False(t, f.hasClock(a.ID))
	})

	t.Run("unresolved report entry with the same slug", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("omitted", "running", "working")
		reconcileBlockedCase(t, f, a, func() {
			f.send(brokerHeartbeatRequest{
				Status:    store.BrokerStatusOnline,
				Inventory: completeInventory(),
				Projects: []brokerProjectHeartbeat{{
					ProjectID: tid("rc-unknown-project"),
					Agents:    []brokerAgentHeartbeat{{Slug: a.Slug, Phase: "running"}},
				}},
			})
		})
	})
}

const (
	k8sTargetA = "kubernetes|context=hybval|namespace=default"
	k8sTargetB = "kubernetes|context=hybval|namespace=scion-agents"
)

func withRuntimeTarget(target string) func(a *store.Agent) {
	return func(a *store.Agent) {
		a.AppliedConfig = &store.AgentAppliedConfig{RuntimeTarget: target}
	}
}

// TestReconcileMissing_PerTargetCompleteness: the broker has two Kubernetes
// targets; listing one is forbidden. An agent on the forbidden target is
// never concluded, while an agent on the listed target is reconciled.
func TestReconcileMissing_PerTargetCompleteness(t *testing.T) {
	f := newReconcileFixture(t)
	onForbidden := f.addAgent("on-forbidden", "running", "working", withRuntimeTarget(k8sTargetA))
	onListed := f.addAgent("on-listed", "running", "working", withRuntimeTarget(k8sTargetB))
	onDocker := f.addAgent("on-docker", "running", "working")

	inv := &brokerInventory{Targets: []brokerInventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: true},
		{ID: k8sTargetA, Runtime: "kubernetes", Complete: false},
		{ID: k8sTargetB, Runtime: "kubernetes", Complete: true},
	}}
	f.heartbeat(inv, onDocker.Slug)
	assert.False(t, f.hasClock(onForbidden.ID), "an agent on an incomplete target gets no clock")
	require.True(t, f.hasClock(onListed.ID))
	f.expireClock(onListed.ID)
	f.heartbeat(inv, onDocker.Slug)

	f.assertReconciled(onListed.ID)
	f.assertUntouched(onForbidden.ID, "running")
	f.assertUntouched(onDocker.ID, "running")
}

// TestReconcileMissing_TargetReportedTwice: a target reported both complete
// and incomplete counts as incomplete.
func TestReconcileMissing_TargetReportedTwice(t *testing.T) {
	hb := &brokerHeartbeatRequest{Inventory: &brokerInventory{Targets: []brokerInventoryTarget{
		{ID: k8sTargetB, Complete: true},
		{ID: k8sTargetB, Complete: false},
		{ID: "docker", Complete: true},
		{ID: "", Complete: true},
	}}}
	assert.Equal(t, map[string]bool{"docker": true}, hb.completeTargets())
}

// TestHeartbeat_RecordsRuntimeTargetOnlyOnChange: the hub records the target
// that listed an agent, writes the agent row only when that target changes,
// and leaves it alone on steady heartbeats.
func TestHeartbeat_RecordsRuntimeTargetOnlyOnChange(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("listed", "running", "working", withRuntimeTarget(""))
	report := func(target string) {
		f.send(brokerHeartbeatRequest{
			Status:    store.BrokerStatusOnline,
			Inventory: completeInventory(k8sTargetB),
			Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
				{Slug: a.Slug, Phase: "running", Activity: "working", RuntimeTarget: target},
			}}},
		})
	}

	v0 := f.get(a.ID).StateVersion
	report(k8sTargetB)
	got := f.get(a.ID)
	require.NotNil(t, got.AppliedConfig)
	assert.Equal(t, k8sTargetB, got.AppliedConfig.RuntimeTarget, "target backfilled")
	v1 := got.StateVersion
	assert.Equal(t, v0+1, v1, "one row write for the backfill")

	for i := 0; i < 3; i++ {
		report(k8sTargetB)
	}
	assert.Equal(t, v1, f.get(a.ID).StateVersion, "no row write while the target is unchanged")

	report(k8sTargetA)
	got = f.get(a.ID)
	assert.Equal(t, k8sTargetA, got.AppliedConfig.RuntimeTarget, "target updated on change")
	assert.Equal(t, v1+1, got.StateVersion)

	report("")
	got = f.get(a.ID)
	assert.Equal(t, k8sTargetA, got.AppliedConfig.RuntimeTarget, "a report without a target leaves the record alone")
	assert.Equal(t, v1+1, got.StateVersion)
}

// TestReconcileMissing_OtherBrokerUntouched: an agent of another broker is
// never a candidate.
func TestReconcileMissing_OtherBrokerUntouched(t *testing.T) {
	f := newReconcileFixture(t)
	other := f.addAgent("elsewhere", "running", "working", func(a *store.Agent) { a.RuntimeBrokerID = tid("rc-other-broker") })
	f.heartbeat(completeInventory())
	assert.False(t, f.hasClock(other.ID))
	f.assertUntouched(other.ID, "running")
}

// TestReconcileMissing_ReappearingAgentClearsClock: an agent reported again
// before the grace period ends loses its clock.
func TestReconcileMissing_ReappearingAgentClearsClock(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("flaky", "running", "working")
	f.heartbeat(completeInventory())
	require.True(t, f.hasClock(a.ID))
	f.heartbeat(completeInventory(), a.Slug)
	assert.False(t, f.hasClock(a.ID))
	f.assertUntouched(a.ID, "running")
}

// TestReconcileMissing_MessageFailsVisibly: a message to a reconciled agent
// is rejected instead of being reported as delivered.
func TestReconcileMissing_MessageFailsVisibly(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("omitted", "running", "working")
	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)

	f.heartbeat(completeInventory())
	f.expireClock(a.ID)
	f.heartbeat(completeInventory())
	f.assertReconciled(a.ID)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/message", map[string]interface{}{
		"message":   "are you there",
		"interrupt": false,
	})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeAgentNotRunning)
	assert.Empty(t, disp.getCalls(), "nothing is dispatched to the broker")
}

func TestInventoryAllowsReconcile(t *testing.T) {
	now := time.Now()
	grace := 3 * time.Minute
	online := &store.RuntimeBroker{Status: store.BrokerStatusOnline, LastHeartbeat: now.Add(-30 * time.Second)}
	hb := &brokerHeartbeatRequest{Status: store.BrokerStatusOnline, Inventory: completeInventory()}

	assert.True(t, inventoryAllowsReconcile(online, hb, now, grace))
	assert.False(t, inventoryAllowsReconcile(nil, hb, now, grace), "unknown previous state")
	assert.False(t, inventoryAllowsReconcile(&store.RuntimeBroker{Status: store.BrokerStatusOnline, LastHeartbeat: now.Add(-grace)}, hb, now, grace), "stale")
	assert.False(t, inventoryAllowsReconcile(&store.RuntimeBroker{Status: store.BrokerStatusOnline}, hb, now, grace), "never heard from")
	assert.False(t, inventoryAllowsReconcile(&store.RuntimeBroker{Status: store.BrokerStatusOffline, LastHeartbeat: now}, hb, now, grace), "was offline")
	assert.False(t, inventoryAllowsReconcile(online, &brokerHeartbeatRequest{Status: store.BrokerStatusOnline}, now, grace), "no inventory")
	assert.False(t, inventoryAllowsReconcile(online, &brokerHeartbeatRequest{Status: store.BrokerStatusOnline, Inventory: &brokerInventory{}}, now, grace), "no targets")
	assert.False(t, inventoryAllowsReconcile(online, &brokerHeartbeatRequest{Status: store.BrokerStatusOnline, Inventory: &brokerInventory{Targets: []brokerInventoryTarget{{ID: "docker"}}}}, now, grace), "no complete target")
	assert.False(t, inventoryAllowsReconcile(online, &brokerHeartbeatRequest{Status: store.BrokerStatusDegraded, Inventory: completeInventory()}, now, grace), "not online")
}

func TestMissingAgentGraceFloor(t *testing.T) {
	s := &Server{}
	assert.Equal(t, DefaultMissingAgentGrace, s.missingAgentGrace())
	s.config.MissingAgentGrace = 10 * time.Second
	assert.Equal(t, DefaultMissingAgentGrace, s.missingAgentGrace())
	s.config.MissingAgentGrace = 5 * time.Minute
	assert.Equal(t, 5*time.Minute, s.missingAgentGrace())
}

func TestLifecycleOpTracker(t *testing.T) {
	var tr lifecycleOpTracker
	end1 := tr.begin("a")
	end2 := tr.begin("a")
	assert.True(t, tr.active("a"))
	end1()
	end1() // idempotent
	assert.True(t, tr.active("a"), "second operation still in flight")
	end2()
	assert.False(t, tr.active("a"))
	tr.begin("")() // no-op
}
