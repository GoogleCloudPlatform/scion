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

package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagereservation"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TransferHook has the signature of the entadapter TransferBrokerReservation
// test hook. It is declared here, with the identical signature, so this
// library never imports entadapter (whose internal tests import storetest).
type TransferHook func(ctx context.Context, tx *ent.Tx, step string, req store.BrokerReservationTransfer) error

// transferFixture is one agent assigned to broker from, with an active
// broker reservation on from (unless created without one), and a second
// broker to.
type transferFixture struct {
	s     store.Store
	def   string
	from  string
	to    string
	agent *store.Agent
	res   *store.UsageReservation // nil when the agent holds none
}

// newTransferFixture creates an agent in phase, with run ID run-1, assigned
// to a fresh broker from, and, when held, an active reservation on from.
func newTransferFixture(t *testing.T, ctx context.Context, s store.Store, phase string, held bool) *transferFixture {
	t.Helper()
	def := seedReservationLimit(t, ctx, s)
	f := &transferFixture{s: s, def: def, from: uuid.NewString(), to: uuid.NewString()}
	f.agent = createTransferAgent(t, ctx, s, f.from, phase, time.Time{})
	if held {
		res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, f.from, f.agent.ID))
		require.NoError(t, err)
		f.res = res
	}
	return f
}

// createTransferAgent creates an agent on broker in phase with run ID
// run-1, soft-deleted at deletedAt when that is set.
func createTransferAgent(t *testing.T, ctx context.Context, s store.Store, broker, phase string, deletedAt time.Time) *store.Agent {
	t.Helper()
	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: "xfer-" + projectID[:8], Slug: "xfer-" + projectID[:8]}))
	agentID := uuid.NewString()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:              agentID,
		Slug:            "xfer-" + agentID[:8],
		Name:            "xfer",
		Template:        "default",
		ProjectID:       projectID,
		Phase:           phase,
		RuntimeBrokerID: broker,
		DeletedAt:       deletedAt,
	}))
	if deletedAt.IsZero() {
		_, err := s.SetAgentRunID(ctx, agentID, "run-1", nil)
		require.NoError(t, err)
	}
	a, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	return a
}

// req builds a transfer of the fixture's agent onto broker.
func (f *transferFixture) req(broker string) store.BrokerReservationTransfer {
	return store.BrokerReservationTransfer{LimitDefinitionID: f.def, AgentID: f.agent.ID, ToBrokerID: broker}
}

// held returns the agent's active reservation, or nil when it holds none.
func (f *transferFixture) held(t *testing.T, ctx context.Context) *store.UsageReservation {
	t.Helper()
	r, err := f.s.GetActiveReservation(ctx, f.def, f.agent.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	require.NoError(t, err)
	return r
}

// requireUnchanged asserts the agent still holds exactly the reservation
// the fixture created (same ID and scope), or still none.
func (f *transferFixture) requireUnchanged(t *testing.T, ctx context.Context) {
	t.Helper()
	got := f.held(t, ctx)
	if f.res == nil {
		require.Nil(t, got, "the agent must still hold no reservation")
		return
	}
	require.NotNil(t, got, "the agent must keep its reservation")
	assert.Equal(t, f.res.ID, got.ID, "same row")
	assert.Equal(t, f.res.ScopeID, got.ScopeID, "same scope")
	assert.Equal(t, store.QuotaScopeBroker, got.ScopeType)
}

// count returns the active broker-scoped reservations on broker.
func (f *transferFixture) count(t *testing.T, ctx context.Context, broker string) int64 {
	t.Helper()
	n, err := f.s.CountActiveReservations(ctx, f.def, broker, store.QuotaScopeBroker, broker)
	require.NoError(t, err)
	return n
}

// requireMoved asserts a TransferMoved result from the fixture's row onto
// broker, and that the agent now holds exactly one active row there, a new
// one.
func (f *transferFixture) requireMoved(t *testing.T, ctx context.Context, res store.BrokerReservationTransferResult, broker string) {
	t.Helper()
	require.Equal(t, store.TransferMoved, res.Outcome)
	assert.Equal(t, store.QuotaScopeBroker, res.FromScopeType)
	assert.Equal(t, f.res.ScopeID, res.FromScopeID)
	assert.Equal(t, f.res.ID, res.ReleasedID)
	got := f.held(t, ctx)
	require.NotNil(t, got)
	assert.Equal(t, res.ReservationID, got.ID)
	assert.NotEqual(t, f.res.ID, got.ID, "a moved reservation is a new row")
	assert.Equal(t, store.QuotaScopeBroker, got.ScopeType)
	assert.Equal(t, broker, got.ScopeID)
	assert.Equal(t, broker, got.SubjectID)
	assert.Equal(t, int64(1), got.Reserved)
	assert.Equal(t, int64(0), f.count(t, ctx, f.res.ScopeID), "the old row is released")
}

// TransferBrokerReservationConformance checks TransferBrokerReservation's
// outcomes, preconditions and cap check on whichever backend factory
// selects. The cases that need the in-transaction hook are in
// TransferBrokerReservationHookConformance.
func TransferBrokerReservationConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("transfer_broker_reservation", func(t *testing.T) {
		t.Run("create if absent with a from reservation ID is invalid before any read", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			r := f.req(f.to)
			r.CreateIfAbsent, r.FromReservationID = true, f.res.ID
			_, err := s.TransferBrokerReservation(ctx, r)
			assert.ErrorIs(t, err, store.ErrInvalidTransfer)
			f.requireUnchanged(t, ctx)
			assert.Equal(t, int64(0), f.count(t, ctx, f.to))

			// Nothing is read: an unparseable limit and a missing agent
			// would otherwise be reported first.
			_, err = s.TransferBrokerReservation(ctx, store.BrokerReservationTransfer{
				LimitDefinitionID: "not-a-uuid", AgentID: uuid.NewString(), ToBrokerID: f.to,
				CreateIfAbsent: true, FromReservationID: uuid.NewString(),
			})
			assert.ErrorIs(t, err, store.ErrInvalidTransfer)
		})

		t.Run("an unparseable limit definition ID is invalid input", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			r := f.req(f.to)
			r.LimitDefinitionID = "not-a-uuid"
			_, err := s.TransferBrokerReservation(ctx, r)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
			f.requireUnchanged(t, ctx)
		})

		t.Run("a missing agent is not found", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
				_, err := s.TransferBrokerReservation(ctx, store.BrokerReservationTransfer{
					LimitDefinitionID: def, AgentID: id, ToBrokerID: uuid.NewString(), CreateIfAbsent: true,
				})
				assert.ErrorIs(t, err, store.ErrNotFound, id)
				_, err = s.GetActiveReservation(ctx, def, id)
				assert.ErrorIs(t, err, store.ErrNotFound, "nothing created for %s", id)
			}
		})

		t.Run("a soft-deleted agent is not found", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			from := uuid.NewString()
			a := createTransferAgent(t, ctx, s, from, "stopped", time.Now().Add(-time.Minute))
			require.False(t, a.DeletedAt.IsZero())
			res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, from, a.ID))
			require.NoError(t, err)
			f := &transferFixture{s: s, def: def, from: from, to: uuid.NewString(), agent: a, res: res}

			_, err = s.TransferBrokerReservation(ctx, f.req(f.to))
			assert.ErrorIs(t, err, store.ErrNotFound)
			f.requireUnchanged(t, ctx)
		})

		t.Run("already on the destination writes nothing", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			before := f.held(t, ctx)
			require.NotNil(t, before)
			for _, r := range []store.BrokerReservationTransfer{
				f.req(f.from),
				{LimitDefinitionID: f.def, AgentID: f.agent.ID, ToBrokerID: f.from, CreateIfAbsent: true, RequireAgentOnTo: true, Cap: 1},
				{LimitDefinitionID: f.def, AgentID: f.agent.ID, ToBrokerID: f.from, FromReservationID: f.res.ID},
			} {
				res, err := s.TransferBrokerReservation(ctx, r)
				require.NoError(t, err)
				assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferAlreadyOn, ReservationID: f.res.ID}, res)
				f.requireUnchanged(t, ctx)
				got := f.held(t, ctx)
				assert.True(t, before.CreatedAt.Equal(got.CreatedAt), "the row is not rewritten")
				assert.Nil(t, got.ReleasedAt)
			}
		})

		t.Run("moves the held row onto the destination", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			res, err := s.TransferBrokerReservation(ctx, f.req(f.to))
			require.NoError(t, err)
			f.requireMoved(t, ctx, res, f.to)
			assert.Equal(t, int64(1), f.count(t, ctx, f.to))
		})

		t.Run("moves a row held in another scope", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", false)
			held, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
				LimitDefinitionID: f.def, SubjectID: "proj", ScopeType: store.QuotaScopeProject, ScopeID: "proj",
				ResourceID: f.agent.ID, Reserved: 1,
			})
			require.NoError(t, err)
			res, err := s.TransferBrokerReservation(ctx, f.req(f.from))
			require.NoError(t, err)
			require.Equal(t, store.TransferMoved, res.Outcome)
			assert.Equal(t, store.QuotaScopeProject, res.FromScopeType)
			assert.Equal(t, "proj", res.FromScopeID)
			assert.Equal(t, held.ID, res.ReleasedID)
			got := f.held(t, ctx)
			require.NotNil(t, got)
			assert.Equal(t, f.from, got.ScopeID)
			assert.Equal(t, store.QuotaScopeBroker, got.ScopeType)
		})

		t.Run("creates when none is held and create if absent is set", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", false)
			r := f.req(f.from)
			r.CreateIfAbsent = true
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			require.Equal(t, store.TransferCreated, res.Outcome)
			assert.Empty(t, res.ReleasedID)
			assert.Empty(t, res.FromScopeID)
			got := f.held(t, ctx)
			require.NotNil(t, got)
			assert.Equal(t, res.ReservationID, got.ID)
			assert.Equal(t, f.from, got.ScopeID)
			assert.Equal(t, f.from, got.SubjectID)
		})

		t.Run("absent when none is held writes nothing", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", false)
			res, err := s.TransferBrokerReservation(ctx, f.req(f.to))
			require.NoError(t, err)
			assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferAbsent}, res)
			f.requireUnchanged(t, ctx)
			assert.Equal(t, int64(0), f.count(t, ctx, f.to))
		})

		t.Run("require agent on destination", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			r := f.req(f.to)
			r.RequireAgentOnTo = true
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferAgentElsewhere}, res)
			f.requireUnchanged(t, ctx)
			assert.Equal(t, int64(0), f.count(t, ctx, f.to))

			// The agent row's broker is the destination: the row held on
			// another broker follows it.
			g := newTransferFixture(t, ctx, s, "running", false)
			stray, err := s.CreateUsageReservation(ctx, newBrokerReservation(g.def, g.to, g.agent.ID))
			require.NoError(t, err)
			g.res = stray
			r = g.req(g.from)
			r.RequireAgentOnTo = true
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			g.requireMoved(t, ctx, res, g.from)
		})

		t.Run("stale when the from reservation ID does not match", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
				r := f.req(f.to)
				r.FromReservationID = id
				res, err := s.TransferBrokerReservation(ctx, r)
				require.NoError(t, err, id)
				assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferStale}, res, id)
				f.requireUnchanged(t, ctx)
			}
			r := f.req(f.to)
			r.FromReservationID = f.res.ID
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			f.requireMoved(t, ctx, res, f.to)

			// The row it named is released now: the same request is stale.
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			assert.Equal(t, store.TransferStale, res.Outcome)
		})

		t.Run("stale when a from reservation ID is set and none is held", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", false)
			r := f.req(f.from)
			r.FromReservationID = uuid.NewString()
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferStale}, res)
			f.requireUnchanged(t, ctx)
		})

		t.Run("cond run ID", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			for _, run := range []string{"run-0", ""} {
				r := f.req(f.to)
				r.Cond = &store.TransferAgentCond{CheckRunID: true, ExpectedRunID: run}
				res, err := s.TransferBrokerReservation(ctx, r)
				require.NoError(t, err)
				assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferStale}, res, "expected run %q", run)
				f.requireUnchanged(t, ctx)
			}
			r := f.req(f.to)
			r.Cond = &store.TransferAgentCond{CheckRunID: true, ExpectedRunID: "run-1"}
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			f.requireMoved(t, ctx, res, f.to)
		})

		t.Run("cond no claim", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "stopped", true)
			claim, err := s.ClaimAgentStart(ctx, f.agent.ID, "owner-1", store.StartClaimUser, "", time.Minute)
			require.NoError(t, err)
			r := f.req(f.to)
			r.Cond = &store.TransferAgentCond{NoClaim: true}
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferStale}, res)
			f.requireUnchanged(t, ctx)

			released, err := s.ReleaseAgentStart(ctx, f.agent.ID, claim.ID, "owner-1")
			require.NoError(t, err)
			require.True(t, released)
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			f.requireMoved(t, ctx, res, f.to)
		})

		t.Run("cond holds capacity", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "stopped", true)
			r := f.req(f.to)
			r.Cond = &store.TransferAgentCond{HoldsCapacity: true}
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferStale}, res)
			f.requireUnchanged(t, ctx)

			g := newTransferFixture(t, ctx, s, "running", true)
			r = g.req(g.to)
			r.Cond = &store.TransferAgentCond{HoldsCapacity: true}
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			g.requireMoved(t, ctx, res, g.to)
		})

		t.Run("cond not in flight", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			_, err := s.ClaimAgentReincarnation(ctx, f.agent.ID, f.agent.StateVersion, time.Now())
			require.NoError(t, err)
			r := f.req(f.to)
			r.Cond = &store.TransferAgentCond{NotInFlight: true}
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			assert.Equal(t, store.BrokerReservationTransferResult{Outcome: store.TransferStale}, res)
			f.requireUnchanged(t, ctx)

			g := newTransferFixture(t, ctx, s, "running", true)
			r = g.req(g.to)
			r.Cond = &store.TransferAgentCond{NotInFlight: true}
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			g.requireMoved(t, ctx, res, g.to)
		})

		t.Run("cap refuses a full destination and writes nothing", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			for i := 0; i < 2; i++ {
				_, err := s.CreateUsageReservation(ctx, newBrokerReservation(f.def, f.to, uuid.NewString()))
				require.NoError(t, err)
			}
			for _, limit := range []int64{1, 2} {
				r := f.req(f.to)
				r.Cap = limit
				_, err := s.TransferBrokerReservation(ctx, r)
				assert.ErrorIs(t, err, store.ErrQuotaExceeded, "cap %d", limit)
				f.requireUnchanged(t, ctx)
				assert.Equal(t, int64(2), f.count(t, ctx, f.to))
			}
			r := f.req(f.to)
			r.Cap = 3
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			f.requireMoved(t, ctx, res, f.to)
			assert.Equal(t, int64(3), f.count(t, ctx, f.to))
		})

		t.Run("cap refuses a create on a full destination", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", false)
			_, err := s.CreateUsageReservation(ctx, newBrokerReservation(f.def, f.from, uuid.NewString()))
			require.NoError(t, err)
			r := f.req(f.from)
			r.CreateIfAbsent, r.Cap = true, 1
			_, err = s.TransferBrokerReservation(ctx, r)
			assert.ErrorIs(t, err, store.ErrQuotaExceeded)
			f.requireUnchanged(t, ctx)
		})

		t.Run("cap zero or below is unchecked", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			_, err := s.CreateUsageReservation(ctx, newBrokerReservation(f.def, f.to, uuid.NewString()))
			require.NoError(t, err)
			r := f.req(f.to)
			r.Cap = -1
			res, err := s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			f.requireMoved(t, ctx, res, f.to)
			assert.Equal(t, int64(2), f.count(t, ctx, f.to))
		})
	})
}

// TransferBrokerReservationHookConformance checks, through the
// in-transaction test hook, that TransferBrokerReservation is atomic: a
// failure at any step leaves the agent with exactly the reservation it had,
// and a zero-row release fails closed. setHook installs the hook and
// returns its restore (entadapter.SetTransferBrokerReservationTestHook,
// adapted). The hook is process-global, so these cases never run in
// parallel. Hook bodies use only tx, which is safe on SQLite's single
// connection.
func TransferBrokerReservationHookConformance(t *testing.T, factory Factory, setHook func(fn TransferHook) (restore func())) {
	t.Helper()
	ctx := context.Background()

	// recordSteps installs a hook that records the steps of agentID's
	// transfers and returns failErr at failStep.
	recordSteps := func(t *testing.T, agentID, failStep string, failErr error) *[]string {
		t.Helper()
		var steps []string
		t.Cleanup(setHook(func(_ context.Context, _ *ent.Tx, step string, req store.BrokerReservationTransfer) error {
			if req.AgentID != agentID {
				return nil
			}
			steps = append(steps, step)
			if step == failStep {
				return failErr
			}
			return nil
		}))
		return &steps
	}

	t.Run("transfer_broker_reservation_hook", func(t *testing.T) {
		t.Run("hook steps per outcome", func(t *testing.T) {
			s := factory(t)
			moved := newTransferFixture(t, ctx, s, "running", true)
			steps := recordSteps(t, moved.agent.ID, "", nil)
			res, err := s.TransferBrokerReservation(ctx, moved.req(moved.to))
			require.NoError(t, err)
			moved.requireMoved(t, ctx, res, moved.to)
			assert.Equal(t, []string{"locked", "released", "before_insert"}, *steps, "moved")

			created := newTransferFixture(t, ctx, s, "running", false)
			steps = recordSteps(t, created.agent.ID, "", nil)
			r := created.req(created.from)
			r.CreateIfAbsent = true
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			require.Equal(t, store.TransferCreated, res.Outcome)
			assert.Equal(t, []string{"locked", "before_insert"}, *steps, "created: released does not fire")

			on := newTransferFixture(t, ctx, s, "running", true)
			steps = recordSteps(t, on.agent.ID, "", nil)
			res, err = s.TransferBrokerReservation(ctx, on.req(on.from))
			require.NoError(t, err)
			require.Equal(t, store.TransferAlreadyOn, res.Outcome)
			assert.Equal(t, []string{"locked"}, *steps, "already on")

			stale := newTransferFixture(t, ctx, s, "running", true)
			steps = recordSteps(t, stale.agent.ID, "", nil)
			r = stale.req(stale.to)
			r.Cond = &store.TransferAgentCond{CheckRunID: true, ExpectedRunID: "run-0"}
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			require.Equal(t, store.TransferStale, res.Outcome)
			r = stale.req(stale.to)
			r.FromReservationID = uuid.NewString()
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			require.Equal(t, store.TransferStale, res.Outcome)
			r = stale.req(stale.to)
			r.RequireAgentOnTo = true
			res, err = s.TransferBrokerReservation(ctx, r)
			require.NoError(t, err)
			require.Equal(t, store.TransferAgentElsewhere, res.Outcome)
			assert.Empty(t, *steps, "the step-2 and step-3 exits never reach locked")
		})

		for _, step := range []string{"locked", "released", "before_insert"} {
			t.Run("a failure at "+step+" rolls the move back", func(t *testing.T) {
				s := factory(t)
				f := newTransferFixture(t, ctx, s, "running", true)
				injected := errors.New("injected at " + step)
				steps := recordSteps(t, f.agent.ID, step, injected)
				_, err := s.TransferBrokerReservation(ctx, f.req(f.to))
				require.ErrorIs(t, err, injected)
				require.Contains(t, *steps, step, "the hook ran")
				f.requireUnchanged(t, ctx)
				assert.Equal(t, int64(1), f.count(t, ctx, f.from))
				assert.Equal(t, int64(0), f.count(t, ctx, f.to))
			})
		}

		t.Run("a failure before the insert of a create writes nothing", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", false)
			injected := errors.New("injected at before_insert")
			steps := recordSteps(t, f.agent.ID, "before_insert", injected)
			r := f.req(f.from)
			r.CreateIfAbsent = true
			_, err := s.TransferBrokerReservation(ctx, r)
			require.ErrorIs(t, err, injected)
			require.Contains(t, *steps, "before_insert", "the hook ran")
			f.requireUnchanged(t, ctx)
		})

		t.Run("a zero-row release fails closed with transfer raced", func(t *testing.T) {
			s := factory(t)
			f := newTransferFixture(t, ctx, s, "running", true)
			resUID, err := uuid.Parse(f.res.ID)
			require.NoError(t, err)
			ran := false
			t.Cleanup(setHook(func(ctx context.Context, tx *ent.Tx, step string, req store.BrokerReservationTransfer) error {
				if req.AgentID != f.agent.ID || step != "locked" {
					return nil
				}
				ran = true
				// Release the held row inside the transfer's own
				// transaction, so step 7 matches no row.
				_, err := tx.UsageReservation.Update().
					Where(usagereservation.IDEQ(resUID), usagereservation.ReleasedAtIsNil()).
					SetReleasedAt(time.Now()).
					Save(ctx)
				return err
			}))
			_, err = s.TransferBrokerReservation(ctx, f.req(f.to))
			require.ErrorIs(t, err, store.ErrTransferRaced)
			require.True(t, ran, "the hook ran")
			// The whole transaction, the injected release included, rolled
			// back, and nothing was inserted.
			f.requireUnchanged(t, ctx)
			assert.Equal(t, int64(0), f.count(t, ctx, f.to))
		})
	})
}
