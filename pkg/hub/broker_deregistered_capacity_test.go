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

// Holds of a deregistered Runtime Broker (design P3.3-b §4.4 conservative
// case, §4.5 G4): the start-claim reaper releases an unconfirmed claim once
// the broker row is confirmed gone, the sweep releases reservations only on
// a scope confirmed deleted by a point lookup and a conditional write, and a
// broker registered again under the same ID gets its running agents'
// reservations back at its registration (the join, the co-located flat and
// embedded provider registrations; erratum E-45-1).

package hub

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errBrokerReadFault is returned by brokerHookStore while its fault is on.
var errBrokerReadFault = errors.New("injected broker read fault")

// brokerHookStore intercepts the broker reads of the reaper and the sweep
// while armed: GetRuntimeBroker of failID fails; GetRuntimeBroker of
// recreateID, when it is not found, re-creates that broker row before it
// returns the NotFound (a re-registration between the sweep's point lookup
// and its conditional release); afterList runs once after the first broker
// list.
type brokerHookStore struct {
	store.Store
	fault      *storeFaultSwitch
	failID     string
	recreateID string
	recreated  bool
	afterList  func()
	listOnce   sync.Once
}

func (b *brokerHookStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if b.fault.Active() && id != "" && id == b.failID {
		return nil, errBrokerReadFault
	}
	got, err := b.Store.GetRuntimeBroker(ctx, id)
	if b.fault.Active() && id == b.recreateID && !b.recreated && errors.Is(err, store.ErrNotFound) {
		b.recreated = true
		if cerr := b.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: id, Name: "re-registered", Slug: "re-registered-" + id[:8], Status: store.BrokerStatusOnline,
		}); cerr != nil {
			return nil, cerr
		}
	}
	return got, err
}

func (b *brokerHookStore) ListRuntimeBrokers(ctx context.Context, filter store.RuntimeBrokerFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	res, err := b.Store.ListRuntimeBrokers(ctx, filter, opts)
	if b.fault.Active() && b.afterList != nil {
		b.listOnce.Do(b.afterList)
	}
	return res, err
}

func newBrokerHookServer(t *testing.T) (*Server, store.Store, *brokerHookStore, *storeFaultSwitch) {
	t.Helper()
	return testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *brokerHookStore {
		return &brokerHookStore{Store: inner, fault: f}
	})
}

// unconfirmedClaimOn gives a an unconfirmed start claim of kind.
func unconfirmedClaimOn(t *testing.T, s store.Store, a *store.Agent, kind store.StartClaimKind) store.StartClaim {
	t.Helper()
	ctx := context.Background()
	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-x", kind, "", time.Minute)
	require.NoError(t, err)
	held, err := s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-x", time.Hour)
	require.NoError(t, err)
	require.True(t, held)
	return c
}

// The reaper releases an unconfirmed claim of an agent whose Runtime Broker
// is confirmed deregistered, with an audit event; the slot is freed by the
// sweep's deregistered-scope pass, also audited and counted. A live claim on
// the deleted broker is not released (it is demoted once its lease
// expires), and a transient broker read error keeps the claim.
func TestReaper_DeregisteredBroker_ReleasesUnconfirmedClaim_Audited(t *testing.T) {
	t.Run("unconfirmed", func(t *testing.T) {
		srv, s := testServer(t)
		_, deregistered := capacityCounters(t, srv)
		ctx := context.Background()
		broker, project := newQuotaTestBrokerAndProject(t, s, "dereg-claim")
		a := newQuotaTestAgent(t, s, broker, project, "dereg-claim", state.PhaseRunning)
		seedSlot(t, s, broker.ID, a.ID)
		claim := unconfirmedClaimOn(t, s, a, store.StartClaimUser)
		require.NoError(t, s.DeleteRuntimeBroker(ctx, broker.ID))

		srv.reapStartClaims(ctx)

		assert.Empty(t, getAgent(t, s, a.ID).StartClaimID, "released")
		audits := capacityAudits(t, s, auditStartClaimBrokerDeregistered, a.ID)
		require.Len(t, audits, 1)
		assert.Contains(t, audits[0].AfterSummary, broker.ID)
		assert.Contains(t, audits[0].AfterSummary, claim.ID)
		assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the slot is the sweep's to free")

		srv.ReconcileStaleBrokerQuotaReservations(ctx)
		assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "freed by the deregistered-scope pass")
		assert.EqualValues(t, 1, deregistered())
		require.Len(t, capacityAudits(t, s, auditQuotaDeregisteredScopeRelease, a.ID), 1)
	})

	t.Run("live claim kept", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		broker, project := newQuotaTestBrokerAndProject(t, s, "dereg-live")
		a := newQuotaTestAgent(t, s, broker, project, "dereg-live", state.PhaseRunning)
		_, err := s.ClaimAgentStart(ctx, a.ID, "hub-x", store.StartClaimUser, "", time.Millisecond)
		require.NoError(t, err)
		require.NoError(t, s.DeleteRuntimeBroker(ctx, broker.ID))
		time.Sleep(5 * time.Millisecond)

		srv.reapStartClaims(ctx)
		got := getAgent(t, s, a.ID)
		require.NotEmpty(t, got.StartClaimID, "a live claim is not released by the deregistered case")
		assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "only demoted once its lease expired")
		assert.Empty(t, capacityAudits(t, s, auditStartClaimBrokerDeregistered, a.ID))

		srv.reapStartClaims(ctx)
		assert.Empty(t, getAgent(t, s, a.ID).StartClaimID, "released on a later tick, once unconfirmed")
	})

	t.Run("live claim with an unexpired lease untouched", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		broker, project := newQuotaTestBrokerAndProject(t, s, "dereg-lease")
		a := newQuotaTestAgent(t, s, broker, project, "dereg-lease", state.PhaseRunning)
		_, err := s.ClaimAgentStart(ctx, a.ID, "hub-x", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		require.NoError(t, s.DeleteRuntimeBroker(ctx, broker.ID))

		srv.reapStartClaims(ctx)
		got := getAgent(t, s, a.ID)
		assert.NotEmpty(t, got.StartClaimID)
		assert.Equal(t, store.StartClaimLive, got.StartClaimState)
	})

	t.Run("transient read error keeps the claim", func(t *testing.T) {
		srv, s, hook, fault := newBrokerHookServer(t)
		ctx := context.Background()
		broker, project := newQuotaTestBrokerAndProject(t, s, "dereg-transient")
		a := newQuotaTestAgent(t, s, broker, project, "dereg-transient", state.PhaseRunning)
		unconfirmedClaimOn(t, s, a, store.StartClaimUser)
		hook.failID = broker.ID
		fault.Arm()

		srv.reapStartClaims(ctx)
		assert.NotEmpty(t, getAgent(t, s, a.ID).StartClaimID, "kept this round")
		assert.Empty(t, capacityAudits(t, s, auditStartClaimBrokerDeregistered, a.ID))
	})
}

// The sweep releases a reservation on a scope a point lookup confirms
// deleted, with an audit event and the counter, whether or not the agent is
// pinned (§4.5 precedence over the §4.2 scope-conflict rule).
func TestSweep_DeregisteredBrokerScope_ReleasesReservation_Audited(t *testing.T) {
	srv, s := testServer(t)
	_, deregistered := capacityCounters(t, srv)
	ctx := context.Background()
	gone, project := newQuotaTestBrokerAndProject(t, s, "dereg-sweep-gone")
	home, homeProject := newFlatQuotaInstance(t, s, "dereg-sweep-home", "kubernetes")
	unpinned := newQuotaTestAgent(t, s, gone, project, "dereg-sweep-unpinned", state.PhaseRunning)
	seedSlot(t, s, gone.ID, unpinned.ID)
	pinned := flatPinnedAgent(t, s, home, homeProject, "dereg-sweep-pinned", state.PhaseRunning)
	seedSlot(t, s, gone.ID, pinned.ID)
	require.NoError(t, s.DeleteRuntimeBroker(ctx, gone.ID))

	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, unpinned.ID))
	held := activeReservation(t, s, brokerLimitDefID(t, s), pinned.ID)
	require.NotNil(t, held, "the running pinned agent is backfilled on its real broker in the same pass")
	assert.Equal(t, home.ID, held.ScopeID)
	assert.EqualValues(t, 2, deregistered())
	for _, a := range []*store.Agent{unpinned, pinned} {
		audits := capacityAudits(t, s, auditQuotaDeregisteredScopeRelease, a.ID)
		require.Len(t, audits, 1, a.Slug)
		assert.Contains(t, audits[0].AfterSummary, gone.ID)
	}
}

// A broker registered, with its first reservation, between the sweep's
// broker list and its reservation snapshot is unlisted but live: untouched.
func TestSweep_BrokerRegisteredAfterList_Untouched(t *testing.T) {
	srv, s, hook, fault := newBrokerHookServer(t)
	_, deregistered := capacityCounters(t, srv)
	ctx := context.Background()
	var late *store.RuntimeBroker
	var agent *store.Agent
	hook.afterList = func() {
		var project *store.Project
		late, project = newQuotaTestBrokerAndProject(t, s, "dereg-late")
		agent = newQuotaTestAgent(t, s, late, project, "dereg-late", state.PhaseRunning)
		seedSlot(t, s, late.ID, agent.ID)
	}
	fault.Arm()

	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	require.NotNil(t, agent, "the broker registered mid-pass")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agent.ID))
	assert.Zero(t, deregistered())
}

// Brokers beyond the list cap are unlisted but live: untouched.
func TestSweep_BrokerBeyondListLimit_Untouched(t *testing.T) {
	srv, s := testServer(t)
	_, deregistered := capacityCounters(t, srv)
	old := reconcileBrokerListLimit
	reconcileBrokerListLimit = 1
	t.Cleanup(func() { reconcileBrokerListLimit = old })
	ctx := context.Background()
	older, project := newQuotaTestBrokerAndProject(t, s, "dereg-older")
	a := newQuotaTestAgent(t, s, older, project, "dereg-older", state.PhaseRunning)
	seedSlot(t, s, older.ID, a.ID)
	time.Sleep(5 * time.Millisecond)
	newQuotaTestBrokerAndProject(t, s, "dereg-newer")

	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.Zero(t, deregistered())
}

// A registered legacy broker whose agents await the drain keeps its slots,
// listed or not.
func TestSweep_RegisteredLegacyScope_Untouched(t *testing.T) {
	srv, s := testServer(t)
	_, deregistered := capacityCounters(t, srv)
	old := reconcileBrokerListLimit
	reconcileBrokerListLimit = 1
	t.Cleanup(func() { reconcileBrokerListLimit = old })
	ctx := context.Background()
	legacy, project := newQuotaTestBrokerAndProject(t, s, "dereg-legacy")
	a := newQuotaTestAgent(t, s, legacy, project, "dereg-legacy", state.PhaseRunning)
	seedSlot(t, s, legacy.ID, a.ID)
	time.Sleep(5 * time.Millisecond)
	newQuotaTestBrokerAndProject(t, s, "dereg-flat-newer") // takes the one listed place

	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	got, err := s.GetRuntimeBroker(ctx, legacy.ID)
	require.NoError(t, err)
	assert.False(t, got.IsFlat())
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.Zero(t, deregistered())
}

// Any point lookup error other than NotFound skips the scope this pass.
func TestSweep_PointLookupError_SkipsScope(t *testing.T) {
	srv, s, hook, fault := newBrokerHookServer(t)
	ctx := context.Background()
	gone, project := newQuotaTestBrokerAndProject(t, s, "dereg-lookup")
	a := newQuotaTestAgent(t, s, gone, project, "dereg-lookup", state.PhaseRunning)
	seedSlot(t, s, gone.ID, a.ID)
	require.NoError(t, s.DeleteRuntimeBroker(ctx, gone.ID))
	hook.failID = gone.ID
	fault.Arm()

	srv.ReconcileStaleBrokerQuotaReservations(ctx)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "skipped this pass")

	hook.failID = ""
	srv.ReconcileStaleBrokerQuotaReservations(ctx)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "released once the lookup confirms it")
}

// A broker that registers again with the same ID between the point lookup
// and the conditional release keeps its row.
func TestSweep_ReRegisteredBetweenLookupAndRelease_Untouched(t *testing.T) {
	srv, s, hook, fault := newBrokerHookServer(t)
	_, deregistered := capacityCounters(t, srv)
	ctx := context.Background()
	gone, project := newQuotaTestBrokerAndProject(t, s, "dereg-rereg")
	a := newQuotaTestAgent(t, s, gone, project, "dereg-rereg", state.PhaseRunning)
	seedSlot(t, s, gone.ID, a.ID)
	require.NoError(t, s.DeleteRuntimeBroker(ctx, gone.ID))
	hook.recreateID = gone.ID
	fault.Arm()

	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	require.True(t, hook.recreated, "the broker registered again right after the lookup")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the conditional write matched nothing")
	assert.Zero(t, deregistered())
	assert.Empty(t, capacityAudits(t, s, auditQuotaDeregisteredScopeRelease, a.ID))
}

// While a registered broker is offline (no fresh complete inventory), its
// claims and slots stay held: a queued or unobserved stop is not evidence
// that the workload stopped.
func TestOfflineRegisteredBroker_HoldContinues(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "offline-hold")
	b, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	b.Status = store.BrokerStatusOffline
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
	a := newQuotaTestAgent(t, s, broker, project, "offline-hold", state.PhaseRunning)
	reserveStaleBrokerSlot(t, s, broker, a.ID)
	unconfirmedClaimOn(t, s, a, store.StartClaimUser)

	srv.reapStartClaims(ctx)
	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	assert.NotEmpty(t, getAgent(t, s, a.ID).StartClaimID, "the claim is held")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the slot is held")
}

// The documented conservative case: an agent whose exit was observed while
// a start claim was held, on a registered broker that is offline, keeps the
// claim and the slot, even past the sweep's grace, until the broker reports
// again or is deregistered.
func TestOfflineBroker_ClaimHeld_SlotHeld(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "offline-claim")
	b, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	b.Status = store.BrokerStatusOffline
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
	a := newQuotaTestAgent(t, s, broker, project, "offline-claim", state.PhaseStopped)
	reserveStaleBrokerSlot(t, s, broker, a.ID)
	unconfirmedClaimOn(t, s, a, store.StartClaimUser)

	srv.reapStartClaims(ctx)
	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	assert.NotEmpty(t, getAgent(t, s, a.ID).StartClaimID)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "a start-kind claim blocks the sweep's release")
}

// registerBrokerWithID registers a Runtime Broker under id through the
// registration API and completes its join, as a re-registering instance
// does.
func registerBrokerWithID(t *testing.T, srv *Server, id, name string) {
	t.Helper()
	rec := doRequestLoopback(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{BrokerID: id, Name: name})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	reg := decodeRegistration(t, rec)
	require.Equal(t, id, reg.BrokerID)
	join := joinWithToken(t, srv, id, reg.JoinToken)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
}

// runningHeartbeat posts an online heartbeat of brokerID reporting a
// running.
func runningHeartbeat(t *testing.T, srv *Server, brokerID string, a *store.Agent) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", brokerHeartbeatRequest{
		Status: store.BrokerStatusOnline,
		Projects: []brokerProjectHeartbeat{{ProjectID: a.ProjectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: string(state.PhaseRunning), Activity: "working"},
		}}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A broker deregistered (its reservations released by the sweep) and then
// registered again under the same ID gets its still-running agents'
// reservations back at its join, before any heartbeat or sweep. Driven
// through the real register, join and heartbeat endpoints.
func TestReRegistered_BackfillRestoresRunningAgents(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s) // the heartbeat and DELETE endpoints check runtime_broker permissions
	ctx := context.Background()
	id := uuid.NewString()
	registerBrokerWithID(t, srv, id, "rereg-instance")
	project := &store.Project{ID: tid("proj-rereg"), Name: "Rereg", Slug: "rereg"}
	require.NoError(t, s.CreateProject(ctx, project))
	a := &store.Agent{ID: tid("agent-rereg"), Slug: "rereg-agent", Name: "rereg-agent", ProjectID: project.ID, RuntimeBrokerID: id, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, a))
	seedSlot(t, s, id, a.ID)
	runningHeartbeat(t, srv, id, a)
	first, err := s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/runtime-brokers/"+id, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	srv.ReconcileStaleBrokerQuotaReservations(ctx)
	require.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "released on the deregistered scope")

	time.Sleep(1100 * time.Millisecond) // a new row identity, whatever the timestamp precision
	registerBrokerWithID(t, srv, id, "rereg-instance")
	second, err := s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	require.False(t, second.Created.Equal(first.Created), "the re-registered row has a new Created")
	require.False(t, second.LastHeartbeat.IsZero(), "join set LastHeartbeat, so a LastHeartbeat.IsZero trigger would never fire")

	held := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, held, "restored at the join, before any heartbeat or sweep")
	assert.Equal(t, id, held.ScopeID)
	runningHeartbeat(t, srv, id, a)
	again := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, again)
	assert.Equal(t, held.ID, again.ID, "the heartbeat writes nothing more")
}

// errReservationRead is returned by reservationReadFaultStore while armed.
var errReservationRead = errors.New("injected reservation read fault")

// errReservationWrite is returned by reservationWriteFaultStore while armed.
var errReservationWrite = errors.New("injected reservation write fault")

// reservationReadFaultStore fails GetActiveReservation of agentID while armed.
type reservationReadFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
}

func (r *reservationReadFaultStore) GetActiveReservation(ctx context.Context, limitDefID, resourceID string) (*store.UsageReservation, error) {
	if r.fault.Active() && resourceID == r.agentID {
		return nil, errReservationRead
	}
	return r.Store.GetActiveReservation(ctx, limitDefID, resourceID)
}

// backfillPending reports whether brokerID's join-time backfill is pending
// on srv.
func backfillPending(srv *Server, brokerID string) bool {
	_, ok := srv.quotaService.brokerBackfillPending.Load(brokerID)
	return ok
}

// A reservation read that fails during a broker's join-time backfill keeps
// the broker pending on that replica: the next heartbeat retries, and the
// agent is counted then, not at the hourly pass.
func TestJoinBackfill_ReadError_RetriedNextHeartbeat(t *testing.T) {
	f := newReconcileFixture(t)
	hook, fault := installStoreFault(t, f.srv, func(inner store.Store, fs *storeFaultSwitch) *reservationReadFaultStore {
		return &reservationReadFaultStore{Store: inner, fault: fs}
	})
	a := f.addAgent("bf-retry", "running", "working")
	hook.agentID = a.ID
	fault.Arm()

	f.srv.BackfillBrokerOnRegistration(context.Background(), f.brokerID)
	require.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the read failed: not backfilled")
	require.True(t, backfillPending(f.srv, f.brokerID), "the broker stays pending")

	hook.agentID = ""
	f.heartbeat(completeInventory(), a.Slug)
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "retried on the next heartbeat")
	assert.False(t, backfillPending(f.srv, f.brokerID), "cleared once the backfill ran to the end")
}

// reservationWriteFaultStore fails CreateUsageReservation for agentID while
// armed.
type reservationWriteFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
}

func (r *reservationWriteFaultStore) CreateUsageReservation(ctx context.Context, res *store.UsageReservation) (*store.UsageReservation, error) {
	if r.fault.Active() && res.ResourceID == r.agentID {
		return nil, errReservationWrite
	}
	return r.Store.CreateUsageReservation(ctx, res)
}

// A reservation write that fails during a broker's join-time backfill keeps
// the broker pending too: the next heartbeat retries. The retry is
// idempotent: an agent already reserved at the join is not written again.
func TestJoinBackfill_WriteError_RetriedNextHeartbeat(t *testing.T) {
	f := newReconcileFixture(t)
	hook, fault := installStoreFault(t, f.srv, func(inner store.Store, fs *storeFaultSwitch) *reservationWriteFaultStore {
		return &reservationWriteFaultStore{Store: inner, fault: fs}
	})
	ok := f.addAgent("bf-write-ok", "running", "working")
	a := f.addAgent("bf-write-retry", "running", "working")
	hook.agentID = a.ID
	fault.Arm()

	f.srv.BackfillBrokerOnRegistration(context.Background(), f.brokerID)
	require.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the write failed: not backfilled")
	first := activeReservation(t, f.s, brokerLimitDefID(t, f.s), ok.ID)
	require.NotNil(t, first, "the other agent was backfilled at the join")
	require.True(t, backfillPending(f.srv, f.brokerID))

	hook.agentID = ""
	f.heartbeat(completeInventory(), ok.Slug, a.Slug)
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "retried on the next heartbeat")
	again := activeReservation(t, f.s, brokerLimitDefID(t, f.s), ok.ID)
	require.NotNil(t, again)
	assert.Equal(t, first.ID, again.ID, "the retry does not write the other agent's reservation again")
	assert.EqualValues(t, 2, brokerReservationCount(t, f.s, f.brokerID), "no double count")
	assert.False(t, backfillPending(f.srv, f.brokerID))
}

// A heartbeat for a broker whose join-time backfill is not pending on this
// replica backfills nothing: an agent missing its reservation is restored
// by a join or by the hourly sweep, not by every heartbeat.
func TestHeartbeat_NotPending_DoesNotBackfill(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("bf-not-pending", "running", "working")
	require.False(t, backfillPending(f.srv, f.brokerID))

	f.heartbeat(completeInventory(), a.Slug)
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "no join, no pending backfill: nothing written")
}

// Deregistering a broker clears its pending join-time backfill on this
// replica.
func TestJoinBackfill_DeleteClearsPending(t *testing.T) {
	f := newReconcileFixture(t) // grants the dev user runtime broker access
	hook, fault := installStoreFault(t, f.srv, func(inner store.Store, fs *storeFaultSwitch) *reservationReadFaultStore {
		return &reservationReadFaultStore{Store: inner, fault: fs}
	})
	a := f.addAgent("bf-delete", "running", "working")
	hook.agentID = a.ID
	fault.Arm()
	f.srv.BackfillBrokerOnRegistration(context.Background(), f.brokerID)
	require.True(t, backfillPending(f.srv, f.brokerID))

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/runtime-brokers/"+f.brokerID, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.False(t, backfillPending(f.srv, f.brokerID), "the delete cleared the pending backfill")
}

// Accepted gap: a pending join-time backfill lives on one replica. If that
// replica goes away (here: another Server on the same store, with nothing
// pending), heartbeats elsewhere do not retry it, and the hourly sweep's
// backfill restores the reservation.
func TestJoinBackfill_PendingLost_SweepRestores(t *testing.T) {
	f := newReconcileFixture(t)
	hook, fault := installStoreFault(t, f.srv, func(inner store.Store, fs *storeFaultSwitch) *reservationReadFaultStore {
		return &reservationReadFaultStore{Store: inner, fault: fs}
	})
	a := f.addAgent("bf-lost", "running", "working")
	hook.agentID = a.ID
	fault.Arm()
	f.srv.BackfillBrokerOnRegistration(context.Background(), f.brokerID)
	require.True(t, backfillPending(f.srv, f.brokerID))
	require.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID))

	other, _ := testServerWithStore(t, f.s)
	require.False(t, backfillPending(other, f.brokerID), "the other replica holds nothing pending")
	runningHeartbeat(t, other, f.brokerID, a)
	require.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "a heartbeat on the other replica does not retry it")
	other.ReconcileStaleBrokerQuotaReservations(context.Background())
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the hourly sweep restored it")
}

// A broker that re-registers through one replica gets its agents'
// reservations at the join; a heartbeat handled by another replica (nothing
// pending there) keeps them and writes nothing.
func TestReRegistered_JoinOnOneReplica_HeartbeatOnAnother(t *testing.T) {
	srvA, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	id := uuid.NewString()
	registerBrokerWithID(t, srvA, id, "rereg-xr")
	project := &store.Project{ID: tid("proj-rereg-xr"), Name: "Rereg XR", Slug: "rereg-xr"}
	require.NoError(t, s.CreateProject(ctx, project))
	a := &store.Agent{ID: tid("agent-rereg-xr"), Slug: "rereg-xr-agent", Name: "rereg-xr-agent", ProjectID: project.ID, RuntimeBrokerID: id, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, a))

	rec := doRequest(t, srvA, http.MethodDelete, "/api/v1/runtime-brokers/"+id, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	registerBrokerWithID(t, srvA, id, "rereg-xr")
	held := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, held, "restored at the join on replica A")

	srvB, _ := testServerWithStore(t, s)
	require.False(t, backfillPending(srvB, id))
	runningHeartbeat(t, srvB, id, a)
	again := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, again, "kept across replicas")
	assert.Equal(t, held.ID, again.ID)
}

// The co-located flat registration (registerEmbeddedFlat) re-creating a
// deregistered broker's row gets its still-running agents' reservations back
// before it returns, with no heartbeat or sweep (erratum E-45-1: every
// registration path).
func TestReRegistered_EmbeddedFlat_RestoresRunningAgents(t *testing.T) {
	srv, s := testServer(t)
	setFlatExperiment(t, srv, true)
	ctx := context.Background()
	id := uuid.NewString()
	target := &api.RuntimeTargetDescriptor{ID: tid("rt-flat-rereg"), Type: "docker", DisplayName: "Docker"}
	_, err := registerEmbeddedFlatForTest(t, srv, id, "flat-rereg", target)
	require.NoError(t, err)
	project := &store.Project{ID: tid("proj-flat-rereg"), Name: "Flat Rereg", Slug: "flat-rereg"}
	require.NoError(t, s.CreateProject(ctx, project))
	a := &store.Agent{ID: tid("agent-flat-rereg"), Slug: "flat-rereg-agent", Name: "flat-rereg-agent", ProjectID: project.ID, RuntimeBrokerID: id, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, a))
	require.NoError(t, s.DeleteRuntimeBroker(ctx, id))
	require.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))

	_, err = registerEmbeddedFlatForTest(t, srv, id, "flat-rereg", target)
	require.NoError(t, err)
	held := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, held, "restored at the embedded flat registration, before any heartbeat or sweep")
	assert.Equal(t, id, held.ScopeID)
}

// The embedded provider registration (POST /api/v1/projects/register with a
// broker) re-creating a deregistered broker's row under its ID gets its
// still-running agents' reservations back before it answers.
func TestReRegistered_EmbeddedProvider_RestoresRunningAgents(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	user := newHubMemberUser(t, s, "rereg-provider-user")
	id := uuid.NewString()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: id, Name: "provider-rereg", Slug: "provider-rereg", Status: store.BrokerStatusOnline, CreatedBy: user.ID}))
	project := &store.Project{ID: tid("proj-provider-rereg"), Name: "Provider Rereg", Slug: "provider-rereg-agents"}
	require.NoError(t, s.CreateProject(ctx, project))
	a := &store.Agent{ID: tid("agent-provider-rereg"), Slug: "provider-rereg-agent", Name: "provider-rereg-agent", ProjectID: project.ID, RuntimeBrokerID: id, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, a))
	require.NoError(t, s.DeleteRuntimeBroker(ctx, id))

	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:   "provider-rereg-project",
		Broker: &RegisterProjectBrokerInfo{ID: id, Name: "provider-rereg", Version: "1.0.0"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	created, err := s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err, "the registration re-created the row under its ID")
	require.Equal(t, id, created.ID)
	held := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, held, "restored at the embedded provider registration, before any heartbeat or sweep")
	assert.Equal(t, id, held.ScopeID)
}
