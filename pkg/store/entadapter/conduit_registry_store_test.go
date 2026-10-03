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

package entadapter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// The TestConduitRegistry_ suite drives registry.Registry over the real
// Ent-backed ConduitRegistryStore. It runs on SQLite by default and on
// Postgres under -tags integration with SCION_TEST_POSTGRES_URL set (it is
// part of `make test-launch-store-postgres`'s -run regex), which is the
// Postgres/SQLite parity proof for design conduit v2.1 §3.4.

type conduitFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *conduitFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *conduitFakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *conduitFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var conduitT0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

type conduitFixture struct {
	t     *testing.T
	ctx   context.Context
	clock *conduitFakeClock
	store *ConduitRegistryStore
	reg   *registry.Registry
}

func newConduitFixture(t *testing.T) *conduitFixture {
	t.Helper()
	clock := &conduitFakeClock{now: conduitT0}
	st := NewConduitRegistryStore(enttest.NewClient(t))
	return &conduitFixture{
		t:     t,
		ctx:   context.Background(),
		clock: clock,
		store: st,
		reg:   registry.New(st, registry.Config{Clock: clock}),
	}
}

func (f *conduitFixture) registerRelay(id string) int64 {
	f.t.Helper()
	gen, err := f.reg.RegisterRelay(f.ctx, registry.RelayInstance{InstanceID: id, InternalEndpoint: "http://" + id + ":9811"})
	require.NoError(f.t, err)
	return gen
}

func agentSession(id, relay string, gen int64, incarnation string) registry.SessionRecord {
	return registry.SessionRecord{
		SessionID:           id,
		PrincipalKind:       registry.PrincipalAgent,
		PrincipalID:         "agent-1",
		ProjectID:           "proj-1",
		RelayInstanceID:     relay,
		RelayGeneration:     gen,
		Transport:           registry.TransportWS,
		EndpointIncarnation: incarnation,
		Capabilities: registry.Capabilities{
			StreamKinds:         []string{"tcp", "pty"},
			RPC:                 []string{"wake"},
			EndpointIncarnation: incarnation,
			TransportLimits:     registry.TransportLimits{MaxFrame: 65536, IdleTimeoutS: 60},
		},
	}
}

func brokerSession(id, relay string, gen int64, incarnation, execScope string) registry.SessionRecord {
	return registry.SessionRecord{
		SessionID:           id,
		PrincipalKind:       registry.PrincipalBroker,
		PrincipalID:         "broker-1",
		RelayInstanceID:     relay,
		RelayGeneration:     gen,
		Transport:           registry.TransportWS,
		EndpointIncarnation: incarnation,
		ExecScope:           execScope,
		Capabilities:        registry.Capabilities{StreamKinds: []string{"pty"}, EndpointIncarnation: incarnation, ExecScope: execScope},
	}
}

func (f *conduitFixture) insert(rec registry.SessionRecord) int64 {
	f.t.Helper()
	epoch, err := f.reg.InsertSessionWithNextEpoch(f.ctx, rec)
	require.NoError(f.t, err)
	return epoch
}

func (f *conduitFixture) eligibleIDs(kind, id string, w registry.Want) []string {
	f.t.Helper()
	recs, err := f.reg.Eligible(f.ctx, kind, id, w, f.clock.Now())
	require.NoError(f.t, err)
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.SessionID)
	}
	return ids
}

func (f *conduitFixture) admission(sessionID string, w registry.Want) registry.Decision {
	f.t.Helper()
	d, err := f.reg.Admission(f.ctx, sessionID, w)
	require.NoError(f.t, err)
	return d
}

var agentWant = registry.Want{ProjectID: "proj-1", Incarnation: "inc-A", Capability: "pty"}

func TestConduitRegistry_RegisterRelay_GenerationStrictlyIncreasing(t *testing.T) {
	f := newConduitFixture(t)
	var last int64
	for i := 0; i < 5; i++ {
		// The clock goes BACKWARDS on every restart: generations must still
		// increase because they come from the database, not wall time.
		f.clock.Set(conduitT0.Add(-time.Duration(i) * time.Hour))
		gen := f.registerRelay("relay-1")
		assert.Greater(t, gen, last, "restart %d", i)
		last = gen
	}
	assert.Equal(t, int64(1), f.registerRelay("relay-2"), "a new instance starts at generation 1")
}

func TestConduitRegistry_C3_StaleGenerationCannotDeleteFreshRow(t *testing.T) {
	f := newConduitFixture(t)
	gen1 := f.registerRelay("relay-1")
	f.insert(agentSession("s-old", "relay-1", gen1, "inc-A"))

	// The relay restarts (same instance id, new generation) and the agent
	// reconnects to it; the new incarnation sweeps its predecessor's rows.
	gen2 := f.registerRelay("relay-1")
	require.Greater(t, gen2, gen1)
	n, err := f.reg.SweepOwnOlderGenerations(f.ctx, "relay-1", gen2)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	f.insert(agentSession("s-new", "relay-1", gen2, "inc-A"))

	// A late shutdown of the stale generation tries to delete the fresh row
	// (both with its own old generation and with the fresh row's id).
	deleted, err := f.reg.DeleteSessionCAS(f.ctx, "s-new", "relay-1", gen1)
	require.NoError(t, err)
	assert.False(t, deleted, "stale generation must not delete the fresh row")
	deleted, err = f.reg.DeleteSessionCAS(f.ctx, "s-new", "relay-other", gen2)
	require.NoError(t, err)
	assert.False(t, deleted, "another relay must not delete the row")

	assert.Equal(t, []string{"s-new"}, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))

	// The owning generation can delete it.
	deleted, err = f.reg.DeleteSessionCAS(f.ctx, "s-new", "relay-1", gen2)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func TestConduitRegistry_C3_StartupSweepRemovesOnlyOwnOlderGenerations(t *testing.T) {
	f := newConduitFixture(t)
	g1 := f.registerRelay("relay-1")
	other := f.registerRelay("relay-2")
	f.insert(agentSession("s-1a", "relay-1", g1, "inc-A"))
	rec := brokerSession("s-1b", "relay-1", g1, "binc", "")
	f.insert(rec)
	f.insert(brokerSession("s-2", "relay-2", other, "binc", ""))

	g2 := f.registerRelay("relay-1")
	// Rows of the superseded generation are fenced immediately, even before
	// the sweep runs.
	assert.Equal(t, registry.ReasonRelaySuperseded, f.admission("s-1a", agentWant).Reason)

	n, err := f.reg.SweepOwnOlderGenerations(f.ctx, "relay-1", g2)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, registry.ReasonNotFound, f.admission("s-1a", agentWant).Reason)
	assert.Equal(t, []string{"s-2"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", registry.Want{Incarnation: "binc"}))

	// Idempotent.
	n, err = f.reg.SweepOwnOlderGenerations(f.ctx, "relay-1", g2)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestConduitRegistry_SupersededRelayIsFenced(t *testing.T) {
	f := newConduitFixture(t)
	g1 := f.registerRelay("relay-1")
	g2 := f.registerRelay("relay-1")

	// A zombie of generation 1 can neither insert, heartbeat nor drain.
	_, err := f.reg.InsertSessionWithNextEpoch(f.ctx, agentSession("s-z", "relay-1", g1, "inc-A"))
	assert.ErrorIs(t, err, registry.ErrRelaySuperseded)
	assert.ErrorIs(t, f.reg.HeartbeatRelay(f.ctx, "relay-1", g1), registry.ErrRelaySuperseded)
	assert.ErrorIs(t, f.reg.SetRelayDraining(f.ctx, "relay-1", g1, true), registry.ErrRelaySuperseded)
	_, err = f.reg.InsertSessionWithNextEpoch(f.ctx, agentSession("s-u", "relay-unknown", 1, "inc-A"))
	assert.ErrorIs(t, err, registry.ErrRelaySuperseded)

	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-1", g2))
	require.NoError(t, f.reg.SetRelayDraining(f.ctx, "relay-1", g2, true))

	// The rejected insert did not consume a session row; the epoch counter
	// may have advanced inside the rolled-back transaction — it must not.
	epoch := f.insert(agentSession("s-ok", "relay-1", g2, "inc-A"))
	assert.Equal(t, int64(1), epoch, "rolled-back inserts must not advance the epoch")
}

func TestConduitRegistry_EpochMonotonicUnderConcurrentInserts(t *testing.T) {
	f := newConduitFixture(t)
	gen := f.registerRelay("relay-1")
	const n = 16
	epochs := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			epochs[i], errs[i] = f.reg.InsertSessionWithNextEpoch(f.ctx, agentSession(fmt.Sprintf("s-%02d", i), "relay-1", gen, "inc-A"))
		}(i)
	}
	close(start)
	wg.Wait()
	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.False(t, seen[epochs[i]], "duplicate epoch %d", epochs[i])
		seen[epochs[i]] = true
	}
	for e := int64(1); e <= n; e++ {
		assert.True(t, seen[e], "epoch %d missing: epochs must be exactly 1..%d", e, n)
	}
	// Exactly one session (the holder of the counter value) is current.
	ids := f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant)
	require.Len(t, ids, 1)
	for i := 0; i < n; i++ {
		if epochs[i] == n {
			assert.Equal(t, fmt.Sprintf("s-%02d", i), ids[0])
		}
	}
}

func TestConduitRegistry_EpochNeverRegressesWhenSessionsAreReaped(t *testing.T) {
	f := newConduitFixture(t)
	gen := f.registerRelay("relay-1")
	assert.Equal(t, int64(1), f.insert(agentSession("s-1", "relay-1", gen, "inc-A")))
	assert.Equal(t, int64(2), f.insert(agentSession("s-2", "relay-1", gen, "inc-A")))

	// Every session row disappears (CAS delete + reaper)...
	_, err := f.reg.DeleteSessionCAS(f.ctx, "s-1", "relay-1", gen)
	require.NoError(t, err)
	f.clock.Advance(2 * time.Minute)
	n, err := f.reg.ReapStaleRelays(f.ctx, time.Minute, f.clock.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// ...and the next connection still gets a strictly newer epoch.
	gen = f.registerRelay("relay-1")
	assert.Equal(t, int64(3), f.insert(agentSession("s-3", "relay-1", gen, "inc-A")))
	// Other principals have independent counters.
	assert.Equal(t, int64(1), f.insert(brokerSession("b-1", "relay-1", gen, "binc", "")))
}

func TestConduitRegistry_ForgetPrincipalEpoch_AgentOnly(t *testing.T) {
	f := newConduitFixture(t)
	gen := f.registerRelay("relay-1")
	f.insert(brokerSession("b-1", "relay-1", gen, "binc", ""))
	assert.ErrorIs(t, f.reg.ForgetPrincipalEpoch(f.ctx, registry.PrincipalBroker, "broker-1"), registry.ErrInvalidInput)
	assert.ErrorIs(t, f.reg.ForgetPrincipalEpoch(f.ctx, registry.PrincipalUser, "u-1"), registry.ErrInvalidInput)

	f.insert(agentSession("a-1", "relay-1", gen, "inc-A"))
	_, err := f.reg.DeleteSessionCAS(f.ctx, "a-1", "relay-1", gen)
	require.NoError(t, err)
	require.NoError(t, f.reg.ForgetPrincipalEpoch(f.ctx, registry.PrincipalAgent, "agent-1"))
	ps, err := f.store.ListPrincipalSessions(f.ctx, registry.PrincipalAgent, "agent-1")
	require.NoError(t, err)
	assert.Zero(t, ps.CurrentEpoch)
	ps, err = f.store.ListPrincipalSessions(f.ctx, registry.PrincipalBroker, "broker-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), ps.CurrentEpoch, "broker epoch rows are kept")
}

func TestConduitRegistry_C17_AgentReincarnation_OnlyCurrentEligibleAndAdmissible(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	f.insert(agentSession("s-old", "relay-1", r1, "inc-A"))
	// The agent is re-incarnated and its new container connects to another
	// relay while the old connection is still physically alive.
	f.insert(agentSession("s-new", "relay-2", r2, "inc-B"))

	wantB := registry.Want{ProjectID: "proj-1", Incarnation: "inc-B", Capability: "pty"}
	assert.Equal(t, []string{"s-new"}, f.eligibleIDs(registry.PrincipalAgent, "agent-1", wantB))
	assert.True(t, f.admission("s-new", wantB).Admissible)
	d := f.admission("s-old", wantB)
	assert.False(t, d.Admissible)
	assert.Equal(t, registry.ReasonIncarnationMismatch, d.Reason)
	// Even a caller holding the obsolete incarnation cannot use s-old: its
	// epoch has been superseded.
	assert.Equal(t, registry.ReasonEpochObsolete, f.admission("s-old", agentWant).Reason)
	assert.Empty(t, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))
}

func TestConduitRegistry_C17_StaleOwnerThroughPartitionNotAdmissible(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	f.insert(agentSession("s-r1", "relay-1", r1, "inc-A"))
	// Partition: same container reconnects via relay-2. relay-1 stays
	// healthy (keeps heartbeating) and still holds s-r1.
	f.clock.Advance(10 * time.Second)
	f.insert(agentSession("s-r2", "relay-2", r2, "inc-A"))
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-1", r1))
	require.NoError(t, f.reg.TouchSession(f.ctx, "s-r1"))

	assert.Equal(t, registry.ReasonEpochObsolete, f.admission("s-r1", agentWant).Reason)
	assert.True(t, f.admission("s-r2", agentWant).Admissible)
	assert.Equal(t, []string{"s-r2"}, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))
}

func TestConduitRegistry_C17_OldDisconnectAfterReplacementKeepsNewRow(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	f.insert(agentSession("s-old", "relay-1", r1, "inc-A"))
	f.insert(agentSession("s-new", "relay-2", r2, "inc-A"))

	// The old connection's disconnect handling arrives late. It may only
	// delete its own row; it must never reach the successor's row.
	deleted, err := f.reg.DeleteSessionCAS(f.ctx, "s-new", "relay-1", r1)
	require.NoError(t, err)
	assert.False(t, deleted)
	deleted, err = f.reg.DeleteSessionCAS(f.ctx, "s-old", "relay-1", r1)
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Equal(t, []string{"s-new"}, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))
	assert.True(t, f.admission("s-new", agentWant).Admissible)
}

func TestConduitRegistry_C17_ExecScopeNeverInterchangeable(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	// Two processes share a broker id but run in different exec scopes.
	f.insert(brokerSession("b-east", "relay-1", r1, "binc", "cluster-east"))
	f.insert(brokerSession("b-west", "relay-2", r2, "binc", "cluster-west"))

	east := registry.Want{ExecScope: "cluster-east", Incarnation: "binc", Capability: "pty"}
	west := registry.Want{ExecScope: "cluster-west", Incarnation: "binc", Capability: "pty"}
	assert.Equal(t, []string{"b-east"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", east))
	assert.Equal(t, []string{"b-west"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", west))
	assert.Equal(t, registry.ReasonExecScopeMismatch, f.admission("b-west", east).Reason)
	assert.Equal(t, registry.ReasonExecScopeMismatch, f.admission("b-east", west).Reason)
	// An unscoped request matches neither.
	assert.Empty(t, f.eligibleIDs(registry.PrincipalBroker, "broker-1", registry.Want{Incarnation: "binc"}))
}

func TestConduitRegistry_C17_EquivalentBrokerSessionsRemainUsable(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	w := registry.Want{ExecScope: "cl", Incarnation: "binc", Capability: "pty"}

	// One broker process connected to two replicas (G9).
	f.insert(brokerSession("b-r1", "relay-1", r1, "binc", "cl"))
	f.clock.Advance(time.Second)
	f.insert(brokerSession("b-r2", "relay-2", r2, "binc", "cl"))
	assert.Equal(t, []string{"b-r2", "b-r1"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", w), "both usable, freshest first")
	assert.True(t, f.admission("b-r1", w).Admissible)
	assert.True(t, f.admission("b-r2", w).Admissible)

	// Touching b-r1 makes it the freshest.
	f.clock.Advance(time.Second)
	require.NoError(t, f.reg.TouchSession(f.ctx, "b-r1"))
	assert.Equal(t, []string{"b-r1", "b-r2"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", w))

	// A reconnect to relay-1 supersedes its predecessor on relay-1 only.
	f.insert(brokerSession("b-r1-new", "relay-1", r1, "binc", "cl"))
	assert.Equal(t, registry.ReasonEpochObsolete, f.admission("b-r1", w).Reason)
	assert.True(t, f.admission("b-r2", w).Admissible)
	assert.ElementsMatch(t, []string{"b-r1-new", "b-r2"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", w))

	// A re-incarnated broker process is fenced by incarnation.
	assert.Equal(t, registry.ReasonIncarnationMismatch,
		f.admission("b-r2", registry.Want{ExecScope: "cl", Incarnation: "binc-2", Capability: "pty"}).Reason)
}

func TestConduitRegistry_UserSessions_ConcurrentBothAdmissible_NeverEligible(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	user := func(id string) registry.SessionRecord {
		return registry.SessionRecord{
			SessionID: id, PrincipalKind: registry.PrincipalUser, PrincipalID: "user-1",
			RelayInstanceID: "relay-1", RelayGeneration: r1, Transport: registry.TransportWS,
		}
	}
	e1 := f.insert(user("u-1"))
	e2 := f.insert(user("u-2"))
	assert.Greater(t, e2, e1, "epoch still recorded (audit)")

	// Two concurrent CLI invocations of one user never fence each other.
	assert.True(t, f.admission("u-1", registry.Want{}).Admissible)
	assert.True(t, f.admission("u-2", registry.Want{}).Admissible)

	// Users are never resolved by principal.
	_, err := f.reg.Eligible(f.ctx, registry.PrincipalUser, "user-1", registry.Want{Incarnation: "x"}, f.clock.Now())
	assert.ErrorIs(t, err, registry.ErrNotRoutable)

	// Draining still applies.
	require.NoError(t, f.reg.SetSessionDraining(f.ctx, "u-1"))
	assert.Equal(t, registry.ReasonDraining, f.admission("u-1", registry.Want{}).Reason)
}

func TestConduitRegistry_RelayPeerNeverGetsRows(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	_, err := f.reg.InsertSessionWithNextEpoch(f.ctx, registry.SessionRecord{
		SessionID: "p-1", PrincipalKind: registry.PrincipalRelayPeer, PrincipalID: "relay-2",
		RelayInstanceID: "relay-1", RelayGeneration: r1, Transport: registry.TransportWS, EndpointIncarnation: "x",
	})
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
	ps, err := f.store.ListPrincipalSessions(f.ctx, registry.PrincipalRelayPeer, "relay-2")
	require.NoError(t, err)
	assert.Empty(t, ps.Sessions)
	assert.Zero(t, ps.CurrentEpoch, "no epoch allocated for relay-peer")
	_, err = f.reg.Eligible(f.ctx, registry.PrincipalRelayPeer, "relay-2", registry.Want{Incarnation: "x"}, f.clock.Now())
	assert.ErrorIs(t, err, registry.ErrNotRoutable)
}

func TestConduitRegistry_Eligible_FiltersBeforeRanking(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	// The freshest connection is the current one (s-cur); every earlier
	// one is obsolete by epoch, so ranking never even sees them.
	f.insert(agentSession("s-a", "relay-1", r1, "inc-A"))
	f.clock.Advance(time.Second)
	f.insert(agentSession("s-cur", "relay-2", r2, "inc-A"))
	f.clock.Advance(time.Second)
	require.NoError(t, f.reg.TouchSession(f.ctx, "s-a")) // fresher, but obsolete
	assert.Equal(t, []string{"s-cur"}, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))

	cases := []struct {
		name string
		want registry.Want
		why  registry.Reason
	}{
		{"project mismatch", registry.Want{ProjectID: "proj-2", Incarnation: "inc-A"}, registry.ReasonProjectMismatch},
		{"exec scope mismatch", registry.Want{ProjectID: "proj-1", ExecScope: "x", Incarnation: "inc-A"}, registry.ReasonExecScopeMismatch},
		{"incarnation mismatch", registry.Want{ProjectID: "proj-1", Incarnation: "inc-Z"}, registry.ReasonIncarnationMismatch},
		{"capability missing", registry.Want{ProjectID: "proj-1", Incarnation: "inc-A", Capability: "ssh"}, registry.ReasonCapabilityMissing},
		{"rpc capability ok", registry.Want{ProjectID: "proj-1", Incarnation: "inc-A", Capability: "wake"}, registry.ReasonOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.why, f.admission("s-cur", tc.want).Reason)
			ids := f.eligibleIDs(registry.PrincipalAgent, "agent-1", tc.want)
			if tc.why == registry.ReasonOK {
				assert.Equal(t, []string{"s-cur"}, ids)
			} else {
				assert.Empty(t, ids)
			}
		})
	}

	_, err := f.reg.Eligible(f.ctx, registry.PrincipalAgent, "agent-1", registry.Want{ProjectID: "proj-1"}, f.clock.Now())
	assert.ErrorIs(t, err, registry.ErrIncompleteWant)
	ok, err := f.reg.IsAdmissible(f.ctx, "s-cur", registry.Want{ProjectID: "proj-1"})
	assert.False(t, ok)
	assert.ErrorIs(t, err, registry.ErrIncompleteWant)
	// An agent lookup without a project fails closed loudly rather than
	// silently mismatching.
	noProject := registry.Want{Incarnation: "inc-A"}
	_, err = f.reg.Eligible(f.ctx, registry.PrincipalAgent, "agent-1", noProject, f.clock.Now())
	assert.ErrorIs(t, err, registry.ErrIncompleteWant)
	ok, err = f.reg.IsAdmissible(f.ctx, "s-cur", noProject)
	assert.False(t, ok)
	assert.ErrorIs(t, err, registry.ErrIncompleteWant)

	// Draining removes the session.
	require.NoError(t, f.reg.SetSessionDraining(f.ctx, "s-cur"))
	assert.Equal(t, registry.ReasonDraining, f.admission("s-cur", agentWant).Reason)
	assert.Empty(t, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))
}

func TestConduitRegistry_Liveness(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	f.insert(agentSession("s-1", "relay-1", r1, "inc-A"))

	// Session not touched for >90s while the relay keeps heartbeating.
	f.clock.Advance(91 * time.Second)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-1", r1))
	assert.Equal(t, registry.ReasonSessionStale, f.admission("s-1", agentWant).Reason)
	require.NoError(t, f.reg.TouchSession(f.ctx, "s-1"))
	assert.True(t, f.admission("s-1", agentWant).Admissible)

	// Relay silent for >60s.
	f.clock.Advance(61 * time.Second)
	require.NoError(t, f.reg.TouchSession(f.ctx, "s-1"))
	assert.Equal(t, registry.ReasonRelayStale, f.admission("s-1", agentWant).Reason)
	assert.Empty(t, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))
}

func TestConduitRegistry_ReapStaleRelays_CascadesToSessionsKeepsRelayRows(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	f.insert(agentSession("s-1", "relay-1", r1, "inc-A"))
	f.insert(brokerSession("b-1", "relay-1", r1, "binc", ""))
	f.insert(brokerSession("b-2", "relay-2", r2, "binc", ""))

	f.clock.Advance(45 * time.Second)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-2", r2))
	f.clock.Advance(20 * time.Second) // relay-1 last seen 65s ago, relay-2 20s ago

	n, err := f.reg.ReapStaleRelays(f.ctx, 60*time.Second, f.clock.Now())
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	assert.ErrorIs(t, f.reg.TouchSession(f.ctx, "s-1"), registry.ErrSessionNotFound)
	assert.ErrorIs(t, f.reg.TouchSession(f.ctx, "b-1"), registry.ErrSessionNotFound)
	require.NoError(t, f.reg.TouchSession(f.ctx, "b-2"))

	// The relay row survives so a restart still gets a newer generation.
	assert.Greater(t, f.registerRelay("relay-1"), r1)

	// Nothing more to reap.
	n, err = f.reg.ReapStaleRelays(f.ctx, 60*time.Second, f.clock.Now())
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestConduitRegistry_FailsClosedOnReadError(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	f.insert(agentSession("s-1", "relay-1", r1, "inc-A"))

	boom := errors.New("injected: database unreachable")
	fs := &registry.FaultStore{Inner: f.store, Fault: func(_ context.Context, op string) error {
		if op == registry.OpListPrincipalSessionsBySession || op == registry.OpListPrincipalSessions {
			return boom
		}
		return nil
	}}
	reg := registry.New(fs, registry.Config{Clock: f.clock})
	ok, err := reg.IsAdmissible(f.ctx, "s-1", agentWant)
	assert.False(t, ok, "admission must fail closed")
	assert.ErrorIs(t, err, boom)
	d, err := reg.Admission(f.ctx, "s-1", agentWant)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, registry.Decision{Reason: registry.ReasonReadError}, d)
	recs, err := reg.Eligible(f.ctx, registry.PrincipalAgent, "agent-1", agentWant, f.clock.Now())
	assert.ErrorIs(t, err, boom)
	assert.Nil(t, recs)
}

func TestConduitRegistry_CapabilitiesRoundTrip(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	in := agentSession("s-1", "relay-1", r1, "inc-A")
	in.ExecScope = "opaque-scope-1"
	f.insert(in)
	ps, found, err := f.store.ListPrincipalSessionsBySession(f.ctx, "s-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, ps.Sessions, 1)
	got := ps.Sessions[0]
	assert.Equal(t, in.Capabilities, got.Session.Capabilities)
	assert.Equal(t, "proj-1", got.Session.ProjectID)
	assert.Equal(t, "opaque-scope-1", got.Session.ExecScope)
	assert.Equal(t, int64(1), got.Session.ConnectionEpoch)
	assert.True(t, got.Session.ConnectedAt.Equal(conduitT0))
	require.NotNil(t, got.Relay)
	assert.Equal(t, "http://relay-1:9811", got.Relay.InternalEndpoint)

	_, found, err = f.store.ListPrincipalSessionsBySession(f.ctx, "missing")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestConduitRegistry_RegisterRelay_ConcurrentSameInstance(t *testing.T) {
	f := newConduitFixture(t)
	const n = 8
	gens := make([]int64, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			g, err := f.reg.RegisterRelay(f.ctx, registry.RelayInstance{InstanceID: "relay-1", InternalEndpoint: "http://relay-1:9811"})
			assert.NoError(t, err)
			gens[i] = g
		}(i)
	}
	close(start)
	wg.Wait()

	// Every racer got a distinct generation, exactly 1..n.
	sorted := append([]int64(nil), gens...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i, g := range sorted {
		assert.Equal(t, int64(i+1), g)
	}
	// Only the winner (highest generation) can act; every loser is fenced.
	for _, g := range gens {
		hbErr := f.reg.HeartbeatRelay(f.ctx, "relay-1", g)
		_, insErr := f.reg.InsertSessionWithNextEpoch(f.ctx, brokerSession(fmt.Sprintf("b-%d", g), "relay-1", g, "binc", ""))
		if g == n {
			assert.NoError(t, hbErr)
			assert.NoError(t, insErr)
		} else {
			assert.ErrorIs(t, hbErr, registry.ErrRelaySuperseded)
			assert.ErrorIs(t, insErr, registry.ErrRelaySuperseded)
		}
	}
}

func TestConduitRegistry_ExecScope_AnyExecScopeIsExplicitOptOut(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	r3 := f.registerRelay("relay-3")
	f.insert(brokerSession("b-x", "relay-1", r1, "binc", "scope-x"))
	f.insert(brokerSession("b-y", "relay-2", r2, "binc", "scope-y"))
	f.insert(brokerSession("b-none", "relay-3", r3, "binc", ""))

	stateful := func(scope string) registry.Want { return registry.Want{ExecScope: scope, Incarnation: "binc"} }
	// Stateful lookups match their exact scope only.
	assert.Equal(t, []string{"b-x"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", stateful("scope-x")))
	assert.Equal(t, []string{"b-y"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", stateful("scope-y")))
	// A caller that forgets ExecScope matches only the unscoped session,
	// never a scoped one by accident.
	assert.Equal(t, []string{"b-none"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", stateful("")))
	assert.Equal(t, registry.ReasonExecScopeMismatch, f.admission("b-x", stateful("")).Reason)

	// Stateless callers opt out explicitly and see every equivalent session.
	anyScope := registry.Want{AnyExecScope: true, Incarnation: "binc"}
	assert.ElementsMatch(t, []string{"b-x", "b-y", "b-none"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", anyScope))
	assert.True(t, f.admission("b-y", anyScope).Admissible)
	// Opting out does not relax the other fences.
	assert.Empty(t, f.eligibleIDs(registry.PrincipalBroker, "broker-1", registry.Want{AnyExecScope: true, Incarnation: "other"}))

	// ExecScope plus AnyExecScope is contradictory.
	bad := registry.Want{ExecScope: "scope-x", AnyExecScope: true, Incarnation: "binc"}
	_, err := f.reg.Eligible(f.ctx, registry.PrincipalBroker, "broker-1", bad, f.clock.Now())
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
	ok, err := f.reg.IsAdmissible(f.ctx, "b-x", bad)
	assert.False(t, ok)
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestConduitRegistry_DrainingRelay_EligibleButRankedLast(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	r2 := f.registerRelay("relay-2")
	f.insert(brokerSession("b-1", "relay-1", r1, "binc", ""))
	f.clock.Advance(time.Second)
	f.insert(brokerSession("b-2", "relay-2", r2, "binc", ""))
	want := registry.Want{Incarnation: "binc"}
	assert.Equal(t, []string{"b-2", "b-1"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", want))

	// Draining relay-2 keeps b-2 usable but ranks it after b-1, even though
	// b-2 is fresher.
	require.NoError(t, f.reg.SetRelayDraining(f.ctx, "relay-2", r2, true))
	assert.Equal(t, []string{"b-1", "b-2"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", want))
	assert.True(t, f.admission("b-2", want).Admissible)

	// A principal whose only session is on a draining relay stays reachable.
	f.insert(agentSession("s-1", "relay-2", r2, "inc-A"))
	assert.Equal(t, []string{"s-1"}, f.eligibleIDs(registry.PrincipalAgent, "agent-1", agentWant))

	// Exclusion during a drain is per session: the relay marks the session
	// draining when it sends GoAway.
	require.NoError(t, f.reg.SetSessionDraining(f.ctx, "b-2"))
	assert.Equal(t, []string{"b-1"}, f.eligibleIDs(registry.PrincipalBroker, "broker-1", want))
	assert.Equal(t, registry.ReasonDraining, f.admission("b-2", want).Reason)
}

func TestConduitRegistry_PruneRelayInstances_OnlyIdleAndStale(t *testing.T) {
	f := newConduitFixture(t)
	idle := f.registerRelay("relay-idle")   // stale, no sessions: pruned
	busy := f.registerRelay("relay-busy")   // stale, has a session: kept
	fresh := f.registerRelay("relay-fresh") // no sessions, heartbeating: kept
	f.insert(brokerSession("b-1", "relay-busy", busy, "binc", ""))

	f.clock.Advance(registry.DefaultRelayPruneAfter + time.Hour)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-fresh", fresh))

	n, err := f.reg.PruneRelayInstances(f.ctx, 0, f.clock.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// The pruned relay is fenced until it re-registers.
	assert.ErrorIs(t, f.reg.HeartbeatRelay(f.ctx, "relay-idle", idle), registry.ErrRelaySuperseded)
	// The busy relay's session survived (no cascade from pruning).
	require.NoError(t, f.reg.TouchSession(f.ctx, "b-1"))
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-busy", busy))

	// Once the reaper has removed a stale relay's sessions, prune removes it.
	f.clock.Advance(registry.DefaultRelayPruneAfter + time.Hour)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-fresh", fresh))
	_, err = f.reg.ReapStaleRelays(f.ctx, 0, f.clock.Now())
	require.NoError(t, err)
	n, err = f.reg.PruneRelayInstances(f.ctx, 0, f.clock.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.ErrorIs(t, f.reg.TouchSession(f.ctx, "b-1"), registry.ErrSessionNotFound)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-fresh", fresh))
}

func TestConduitRegistry_CapabilitiesSQLDefault(t *testing.T) {
	// A writer that omits capabilities gets the SQL-level '{}' default.
	f := newConduitFixture(t)
	gen := f.registerRelay("relay-1")
	drv, ok := f.store.client.Driver().(*entsql.Driver)
	require.True(t, ok)
	q := fmt.Sprintf(`INSERT INTO conduit_sessions (session_id, principal_kind, principal_id, relay_instance_id,
  relay_generation, transport, endpoint_incarnation, connection_epoch, connected_at, last_seen)
VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		f.store.ph(1), f.store.ph(2), f.store.ph(3), f.store.ph(4), f.store.ph(5),
		f.store.ph(6), f.store.ph(7), f.store.ph(8), f.store.ph(9), f.store.ph(10))
	now := f.clock.Now()
	_, err := drv.DB().ExecContext(f.ctx, q, "raw-1", registry.PrincipalBroker, "broker-1", "relay-1",
		gen, registry.TransportWS, "binc", int64(1), now, now)
	require.NoError(t, err)
	ps, err := f.store.ListPrincipalSessions(f.ctx, registry.PrincipalBroker, "broker-1")
	require.NoError(t, err)
	require.Len(t, ps.Sessions, 1)
	assert.Equal(t, registry.Capabilities{}, ps.Sessions[0].Session.Capabilities)
}

func TestConduitRegistry_ProjectRuleRejectedWithoutConsumingEpoch(t *testing.T) {
	f := newConduitFixture(t)
	gen := f.registerRelay("relay-1")
	noProject := agentSession("s-1", "relay-1", gen, "inc-A")
	noProject.ProjectID = ""
	_, err := f.reg.InsertSessionWithNextEpoch(f.ctx, noProject)
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
	withProject := brokerSession("b-1", "relay-1", gen, "binc", "")
	withProject.ProjectID = "proj-1"
	_, err = f.reg.InsertSessionWithNextEpoch(f.ctx, withProject)
	assert.ErrorIs(t, err, registry.ErrInvalidInput)

	for _, k := range []struct{ kind, id string }{{registry.PrincipalAgent, "agent-1"}, {registry.PrincipalBroker, "broker-1"}} {
		ps, err := f.store.ListPrincipalSessions(f.ctx, k.kind, k.id)
		require.NoError(t, err)
		assert.Empty(t, ps.Sessions)
		assert.Zero(t, ps.CurrentEpoch, "%s: a rejected insert must not consume an epoch", k.kind)
	}
	// The first valid insert still gets epoch 1.
	assert.Equal(t, int64(1), f.insert(agentSession("s-2", "relay-1", gen, "inc-A")))
}

func TestConduitRegistry_ReapStaleSessions_OnlyOldRowsOnLiveRelays(t *testing.T) {
	f := newConduitFixture(t)
	r1 := f.registerRelay("relay-1")
	f.insert(agentSession("s-leaked", "relay-1", r1, "inc-A")) // its delete "failed"
	f.insert(brokerSession("b-live", "relay-1", r1, "binc", ""))
	f.insert(brokerSession("b-hiccup", "relay-1", r1, "binc2", ""))

	// 11 minutes pass. The relay heartbeats throughout; b-live pongs
	// normally; b-hiccup's last pong was 2 minutes ago (stale for
	// routing, but well inside the reap horizon).
	f.clock.Advance(9 * time.Minute)
	require.NoError(t, f.reg.TouchSession(f.ctx, "b-hiccup"))
	f.clock.Advance(2 * time.Minute)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-1", r1))
	require.NoError(t, f.reg.TouchSession(f.ctx, "b-live"))

	// The relay reaper does nothing: the relay is alive.
	n, err := f.reg.ReapStaleRelays(f.ctx, 0, f.clock.Now())
	require.NoError(t, err)
	assert.Zero(t, n)

	n, err = f.reg.ReapStaleSessions(f.ctx, 0, f.clock.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.ErrorIs(t, f.reg.TouchSession(f.ctx, "s-leaked"), registry.ErrSessionNotFound)
	require.NoError(t, f.reg.TouchSession(f.ctx, "b-live"))
	require.NoError(t, f.reg.TouchSession(f.ctx, "b-hiccup"))

	// The epoch table is untouched: the agent's next session is epoch 2.
	ps, err := f.store.ListPrincipalSessions(f.ctx, registry.PrincipalAgent, "agent-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), ps.CurrentEpoch)
	assert.Equal(t, int64(2), f.insert(agentSession("s-next", "relay-1", r1, "inc-A")))

	// A horizon inside SessionStaleAfter is refused.
	_, err = f.reg.ReapStaleSessions(f.ctx, registry.DefaultSessionStaleAfter, f.clock.Now())
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}
