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

// Invariants of per-instance capacity on flat Runtime Brokers (design P3.3
// §4.6, extracted from closed GoogleCloudPlatform/scion#2421; tests only):
// each flat instance has its own Runtime Broker ID and so its own
// max_agents_per_broker limit, and two instances never share or bypass each
// other's accounting.

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFlatQuotaInstance creates an online flat Runtime Broker serving one
// runtime target of type targetType, and a project it provides.
func newFlatQuotaInstance(t *testing.T, s store.Store, suffix, targetType string) (*store.RuntimeBroker, *store.Project) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:            tid("flat-quota-" + suffix),
		Name:          "Flat Quota " + suffix,
		Slug:          "flat-quota-" + suffix,
		Status:        store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("flat-target-" + suffix), Type: targetType},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	require.True(t, got.IsFlat())
	project := &store.Project{ID: tid("flat-quota-proj-" + suffix), Name: "Flat Quota " + suffix, Slug: "flat-quota-proj-" + suffix}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: broker.Status,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	return got, project
}

// flatPinnedAgent creates an agent pinned to broker's runtime target.
func flatPinnedAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase) *store.Agent {
	t.Helper()
	return pinnedQuotaAgentWith(t, s, broker, project, name, phase, func(a *store.Agent) {
		a.PinnedRuntimeTargetID = broker.RuntimeTarget.ID
		a.PinnedRuntimeTargetType = broker.RuntimeTarget.Type
	})
}

// setInstanceMaxAgents sets broker's own maxAgents setting.
func setInstanceMaxAgents(t *testing.T, s store.Store, brokerID string, value int64) {
	t.Helper()
	ctx := context.Background()
	rev := int64(0)
	if rec, err := s.GetBrokerSettings(ctx, brokerID); err == nil {
		rev = rec.Revision
	}
	_, err := s.PutBrokerSettings(ctx, brokerID, store.BrokerSettings{MaxAgents: &value}, rev, "test")
	require.NoError(t, err)
}

func startCode(t *testing.T, srv *Server, agentID string) int {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/start", nil).Code
}

// Invariant 1: scopes are disjoint. Concurrent starts on a Docker and a
// Kubernetes instance on one host, with the Docker instance already full:
// filling one never changes the other's count or admission.
func TestFlatCapacity_DockerAndK8sCreatesIndependent(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	docker, dockerProject := newFlatQuotaInstance(t, s, "indep-docker", "docker")
	k8s, k8sProject := newFlatQuotaInstance(t, s, "indep-k8s", "kubernetes")
	setInstanceMaxAgents(t, s, docker.ID, 1)
	setInstanceMaxAgents(t, s, k8s.ID, 1)
	held := flatPinnedAgent(t, s, docker, dockerProject, "indep-held", state.PhaseRunning)
	seedSlot(t, s, docker.ID, held.ID)

	const n = 5
	ids := map[string][]string{}
	for i := 0; i < n; i++ {
		ids[docker.ID] = append(ids[docker.ID], flatPinnedAgent(t, s, docker, dockerProject, fmt.Sprintf("indep-d-%d", i), state.PhaseStopped).ID)
		ids[k8s.ID] = append(ids[k8s.ID], flatPinnedAgent(t, s, k8s, k8sProject, fmt.Sprintf("indep-k-%d", i), state.PhaseStopped).ID)
	}
	ok := map[string]*atomic.Int32{docker.ID: {}, k8s.ID: {}}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for brokerID, list := range ids {
		for _, id := range list {
			wg.Add(1)
			go func(brokerID, id string) {
				defer wg.Done()
				<-gate
				if startCode(t, srv, id) == http.StatusOK {
					ok[brokerID].Add(1)
				}
			}(brokerID, id)
		}
	}
	close(gate)
	wg.Wait()

	assert.Zero(t, ok[docker.ID].Load(), "the full Docker instance admits nothing")
	assert.EqualValues(t, 1, ok[k8s.ID].Load(), "the Kubernetes instance admits up to its own cap")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, docker.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, s, k8s.ID))
}

// The acceptance race extended to two flat instances: concurrent starts at
// cap-1 on one instance while the other instance is full; exactly one
// succeeds, and the full instance is untouched.
func TestFlatCapacity_ConcurrentStartsAtCapMinusOne_OtherInstanceFull(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	open, openProject := newFlatQuotaInstance(t, s, "cm1-open", "docker")
	full, fullProject := newFlatQuotaInstance(t, s, "cm1-full", "kubernetes")
	setInstanceMaxAgents(t, s, open.ID, 2)
	setInstanceMaxAgents(t, s, full.ID, 1)
	seedSlot(t, s, open.ID, flatPinnedAgent(t, s, open, openProject, "cm1-open-held", state.PhaseRunning).ID)
	seedSlot(t, s, full.ID, flatPinnedAgent(t, s, full, fullProject, "cm1-full-held", state.PhaseRunning).ID)
	a1 := flatPinnedAgent(t, s, open, openProject, "cm1-a1", state.PhaseStopped)
	a2 := flatPinnedAgent(t, s, open, openProject, "cm1-a2", state.PhaseStopped)

	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, id := range []string{a1.ID, a2.ID} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			codes[i] = startCode(t, srv, id)
		}(i, id)
	}
	wg.Wait()

	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusTooManyRequests}, codes)
	assert.EqualValues(t, 2, brokerReservationCount(t, s, open.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, s, full.ID))
}

// Invariant 2: the 429 names what was exhausted: the limit in the message
// and the Runtime Broker ID in the details (the name is omitted, so the
// error path makes no broker read).
func TestFlatCapacity_AtCapRefusalNamesLimitAndInstance(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	inst, project := newFlatQuotaInstance(t, s, "names", "docker")
	setInstanceMaxAgents(t, s, inst.ID, 1)
	seedSlot(t, s, inst.ID, flatPinnedAgent(t, s, inst, project, "names-held", state.PhaseRunning).ID)
	cand := flatPinnedAgent(t, s, inst, project, "names-cand", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/start", nil)
	assertBrokerQuotaExceeded(t, rec)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, inst.ID, resp.Error.Details["runtimeBrokerId"])
}

// Invariant 3: explicit zero (unlimited), nil (inherit the hub default) and
// a positive cap are distinct, per instance.
func TestFlatCapacity_ZeroNilCapPerInstance(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 1) // the hub default an instance inherits
	unlimited, up := newFlatQuotaInstance(t, s, "zn-zero", "docker")
	inherit, ip := newFlatQuotaInstance(t, s, "zn-nil", "kubernetes")
	capped, cp := newFlatQuotaInstance(t, s, "zn-cap", "docker")
	setInstanceMaxAgents(t, s, unlimited.ID, 0)
	setInstanceMaxAgents(t, s, capped.ID, 2)

	for i := 0; i < 3; i++ {
		a := flatPinnedAgent(t, s, unlimited, up, fmt.Sprintf("zn-zero-%d", i), state.PhaseStopped)
		assert.Equal(t, http.StatusOK, startCode(t, srv, a.ID), "0 is unlimited (#%d)", i)
	}
	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		a := flatPinnedAgent(t, s, inherit, ip, fmt.Sprintf("zn-nil-%d", i), state.PhaseStopped)
		assert.Equal(t, want, startCode(t, srv, a.ID), "nil inherits the default of 1 (#%d)", i)
	}
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		a := flatPinnedAgent(t, s, capped, cp, fmt.Sprintf("zn-cap-%d", i), state.PhaseStopped)
		assert.Equal(t, want, startCode(t, srv, a.ID), "a cap of 2 (#%d)", i)
	}
	// An unlimited limit records no reservation (QuotaService.Reserve,
	// "Unlimited — no enforcement"), so the maxAgents=0 instance holds none.
	assert.EqualValues(t, 0, brokerReservationCount(t, s, unlimited.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, s, inherit.ID))
	assert.EqualValues(t, 2, brokerReservationCount(t, s, capped.ID))
}

// Invariant 5: the accounting identity is the pinned Runtime Broker ID,
// which never changes. Instance agent defaults are settings-file only and
// cannot be edited from a hub test, so the hub-side equivalent is pinned:
// the instance's maxAgents setting and the hub default change, and the
// restarted agent's reservation stays on its pinned instance.
func TestFlatCapacity_ScopeStableAcrossDefaultsChange(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&hookedStartDispatcher{})
	inst, project := newFlatQuotaInstance(t, s, "stable", "kubernetes")
	setInstanceMaxAgents(t, s, inst.ID, 5)
	a := flatPinnedAgent(t, s, inst, project, "stable", state.PhaseRunning)
	before := seedSlot(t, s, inst.ID, a.ID)

	setInstanceMaxAgents(t, s, inst.ID, 3)
	setBrokerAgentCeiling(t, s, 7)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	held := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, held)
	assert.Equal(t, inst.ID, held.ScopeID, "same scope")
	assert.Equal(t, before.ID, held.ID, "the restart kept its own reservation")
	assert.Equal(t, inst.ID, getAgent(t, s, a.ID).PinnedRuntimeBrokerID)
}

// Invariant 6: suspend and delete release, each on its own instance only.
func TestFlatCapacity_SuspendDeleteRelease(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	docker, dp := newFlatQuotaInstance(t, s, "sd-docker", "docker")
	k8s, kp := newFlatQuotaInstance(t, s, "sd-k8s", "kubernetes")
	suspended := flatPinnedAgent(t, s, docker, dp, "sd-suspend", state.PhaseRunning)
	seedSlot(t, s, docker.ID, suspended.ID)
	deleted := flatPinnedAgent(t, s, k8s, kp, "sd-delete", state.PhaseRunning)
	seedSlot(t, s, k8s.ID, deleted.ID)
	otherDocker := flatPinnedAgent(t, s, docker, dp, "sd-docker-other", state.PhaseRunning)
	seedSlot(t, s, docker.ID, otherDocker.ID)
	otherK8s := flatPinnedAgent(t, s, k8s, kp, "sd-k8s-other", state.PhaseRunning)
	seedSlot(t, s, k8s.ID, otherK8s.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+suspended.ID+"/suspend", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, suspended.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, s, docker.ID))
	assert.EqualValues(t, 2, brokerReservationCount(t, s, k8s.ID), "the other instance is untouched")

	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+deleted.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, deleted.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, s, k8s.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, s, docker.ID))
}

// Invariant 8, for unpinned paths: the sweep never releases the reservation
// of an agent whose launch is in flight (its pre-running phase is counted),
// however old the reservation.
func TestSweep_SkipsInFlightLaunch(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "inflight")
	a := newQuotaTestAgent(t, s, broker, project, "inflight", state.PhaseCreated)
	reserveStaleBrokerSlot(t, s, broker, a.ID)
	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateActive, getAgent(t, s, a.ID).LaunchState)

	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
}

// Invariant 9: reincarnation keeps the accounting identity. A pinned agent
// never changes instance: the move is refused before anything is changed,
// so the quota move (moveBrokerQuota) is never reached and the reservation
// stays on the pinned instance. (The in-place reincarnation worker is not
// driven here; the refusal is the only path that could move the scope.)
func TestFlatCapacity_ReincarnationKeepsScope(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	inst, project := newFlatQuotaInstance(t, s, "reinc", "docker")
	target, _ := newQuotaTestBrokerAndProject(t, s, "reinc-target")
	require.NoError(t, s.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: target.ID, BrokerName: target.Name, Status: target.Status,
	}))
	a := pinnedQuotaAgentWith(t, s, inst, project, "reinc", state.PhaseRunning, func(a *store.Agent) {
		a.PinnedRuntimeTargetID = inst.RuntimeTarget.ID
		a.PinnedRuntimeTargetType = inst.RuntimeTarget.Type
		reincarnationEligible(a)
	})
	before := seedSlot(t, s, inst.ID, a.ID)

	rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{TargetBroker: target.ID})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRuntimeTargetMoveUnsupported)

	held := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, held)
	assert.Equal(t, before.ID, held.ID, "the reservation is untouched")
	assert.Equal(t, inst.ID, held.ScopeID)
	assert.Zero(t, brokerReservationCount(t, s, target.ID), "nothing moved to the target")
	assert.Equal(t, inst.ID, getAgent(t, s, a.ID).RuntimeBrokerID)
}

// errSettingsRead is returned by settingsFailingStore while armed.
var errSettingsRead = errors.New("injected broker settings read failure")

// settingsFailingStore fails every broker settings read while armed.
type settingsFailingStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *settingsFailingStore) GetBrokerSettings(ctx context.Context, brokerID string) (*store.BrokerSettingsRecord, error) {
	if f.fault.Active() {
		return nil, errSettingsRead
	}
	return f.Store.GetBrokerSettings(ctx, brokerID)
}

// Invariant 10: when an instance's settings cannot be read, the limit is
// unknown: no new admission (enforcement stays on), and existing
// reservations are untouched.
func TestFlatCapacity_SettingsLoadFailureConservative(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *settingsFailingStore {
		return &settingsFailingStore{Store: inner, fault: f}
	})
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	inst, project := newFlatQuotaInstance(t, s, "settings-fail", "docker")
	setInstanceMaxAgents(t, s, inst.ID, 5)
	held := flatPinnedAgent(t, s, inst, project, "settings-fail-held", state.PhaseRunning)
	before := seedSlot(t, s, inst.ID, held.ID)
	cand := flatPinnedAgent(t, s, inst, project, "settings-fail-cand", state.PhaseStopped)
	fault.Arm()

	code := startCode(t, srv, cand.ID)
	assert.NotEqual(t, http.StatusOK, code, "not admitted while the limit is unknown")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, cand.ID))

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	got := activeReservation(t, s, brokerLimitDefID(t, s), held.ID)
	require.NotNil(t, got)
	assert.Equal(t, before.ID, got.ID, "the existing reservation is untouched")
}

// Invariant 11: the admin usage view lists every scope's rows with that
// instance's own limit, flags a pinned agent's row on another instance
// (scopeConflict) and a row on a scope that is not a listed broker
// (scopeUnlisted); unlisted rows are not counted in totalActive.
func TestUsage_PerInstanceRowsAndFlags(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	docker, dp := newFlatQuotaInstance(t, s, "usage-docker", "docker")
	k8s, kp := newFlatQuotaInstance(t, s, "usage-k8s", "kubernetes")
	gone, gp := newQuotaTestBrokerAndProject(t, s, "usage-gone")
	setInstanceMaxAgents(t, s, docker.ID, 3)
	setInstanceMaxAgents(t, s, k8s.ID, 7)
	onDocker := flatPinnedAgent(t, s, docker, dp, "usage-on-docker", state.PhaseRunning)
	seedSlot(t, s, docker.ID, onDocker.ID)
	onK8s := flatPinnedAgent(t, s, k8s, kp, "usage-on-k8s", state.PhaseRunning)
	seedSlot(t, s, k8s.ID, onK8s.ID)
	conflicted := flatPinnedAgent(t, s, k8s, kp, "usage-conflicted", state.PhaseRunning)
	seedSlot(t, s, docker.ID, conflicted.ID)
	orphan := newQuotaTestAgent(t, s, gone, gp, "usage-orphan", state.PhaseRunning)
	seedSlot(t, s, gone.ID, orphan.ID)
	require.NoError(t, s.DeleteRuntimeBroker(ctx, gone.ID))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+brokerLimitDefID(t, s), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	rows := map[string]usageReservationView{}
	for _, v := range resp.Reservations {
		rows[v.ResourceID] = v
	}
	require.Len(t, rows, 4)
	assert.Equal(t, 3, resp.TotalActive, "the unlisted row is not counted")

	require.NotNil(t, rows[onDocker.ID].BrokerAgentLimit)
	assert.EqualValues(t, 3, *rows[onDocker.ID].BrokerAgentLimit, "the Docker instance's own limit")
	require.NotNil(t, rows[onK8s.ID].BrokerAgentLimit)
	assert.EqualValues(t, 7, *rows[onK8s.ID].BrokerAgentLimit, "the Kubernetes instance's own limit")
	assert.True(t, rows[conflicted.ID].ScopeConflict)
	assert.False(t, rows[conflicted.ID].ScopeUnlisted)
	assert.False(t, rows[onDocker.ID].ScopeConflict)
	assert.True(t, rows[orphan.ID].ScopeUnlisted)
	for _, id := range []string{onDocker.ID, onK8s.ID, conflicted.ID} {
		assert.False(t, rows[id].ScopeUnlisted, id)
	}

	var raw struct {
		Reservations []map[string]any `json:"reservations"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, row := range raw.Reservations {
		if row["resourceId"] == orphan.ID {
			assert.Equal(t, true, row["scopeUnlisted"], "wire name is scopeUnlisted")
		}
	}
}

// A direct-message wake of an agent that holds capacity on another Runtime
// Broker surfaces the 409 scope conflict, not a generic quota failure; at a
// full instance the 429 names the instance.
func TestWakeDM_ScopeConflictAndCapSurfaceTheirCodes(t *testing.T) {
	t.Run("scope conflict", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &wakeTrackingDispatcher{}
		srv.SetDispatcher(disp)
		other, _ := newQuotaTestBrokerAndProject(t, s, "wake-conflict-other")
		inst, project := newFlatQuotaInstance(t, s, "wake-conflict", "docker")
		target := flatPinnedAgent(t, s, inst, project, "wake-conflict", state.PhaseSuspended)
		seedSlot(t, s, other.ID, target.ID)

		_, dmErr := srv.wakeAgentForDM(context.Background(), target)
		require.NotNil(t, dmErr)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Equal(t, ErrCodeBrokerReservationScopeConflict, dmErr.Code)
		assert.Equal(t, fmt.Sprintf("agent %s already holds capacity on Runtime Broker %s; it cannot be admitted on %s",
			target.ID, other.ID, inst.ID), dmErr.Message)
		assert.Empty(t, disp.getStartCalls(), "no resume dispatch")
	})
	t.Run("cap", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &wakeTrackingDispatcher{}
		srv.SetDispatcher(disp)
		inst, project := newFlatQuotaInstance(t, s, "wake-cap", "docker")
		setInstanceMaxAgents(t, s, inst.ID, 1)
		seedSlot(t, s, inst.ID, flatPinnedAgent(t, s, inst, project, "wake-cap-held", state.PhaseRunning).ID)
		target := flatPinnedAgent(t, s, inst, project, "wake-cap", state.PhaseSuspended)

		_, dmErr := srv.wakeAgentForDM(context.Background(), target)
		require.NotNil(t, dmErr)
		assert.Equal(t, http.StatusTooManyRequests, dmErr.HTTPStatus)
		assert.Equal(t, ErrCodeQuotaExceeded, dmErr.Code)
		assert.Equal(t, inst.ID, dmErr.Details["runtimeBrokerId"])
	})
}
