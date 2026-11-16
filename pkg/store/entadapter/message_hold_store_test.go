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
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

type holdStoreFixture struct {
	client    *ent.Client
	cs        *CompositeStore
	projectID string
	agentID   string
	nowMu     sync.Mutex
	now       time.Time
}

func newHoldStoreFixture(t *testing.T) *holdStoreFixture {
	t.Helper()
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	baseNow := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	f := &holdStoreFixture{
		client:    client,
		cs:        cs,
		projectID: uuid.NewString(),
		agentID:   uuid.NewString(),
		now:       baseNow,
	}
	cs.SetMessageHoldClockForTest(func() time.Time {
		f.nowMu.Lock()
		defer f.nowMu.Unlock()
		return f.now
	})

	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   f.projectID,
		Name: "Hold Test Project",
		Slug: "hold-test-" + f.projectID[:8],
	}))
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:        f.agentID,
		Slug:      "cloudrun-agent",
		Name:      "Cloud Run Agent",
		ProjectID: f.projectID,
		Phase:     "starting",
		Runtime:   "cloudrun",
	}))
	_, err := cs.SetAgentRunID(ctx, f.agentID, "run-initial", nil)
	require.NoError(t, err)
	return f
}

func (f *holdStoreFixture) advance(d time.Duration) time.Time {
	f.nowMu.Lock()
	defer f.nowMu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

func (f *holdStoreFixture) currentTime() time.Time {
	f.nowMu.Lock()
	defer f.nowMu.Unlock()
	return f.now
}

func (f *holdStoreFixture) createDispatchedMsg(t *testing.T, body string, createdOffset time.Duration) *store.Message {
	t.Helper()
	m := newTestMessage(uuid.NewString(), f.agentID)
	m.ProjectID = f.projectID
	m.Recipient = "agent:cloudrun-agent"
	m.Msg = body
	m.DispatchState = store.MessageDispatchDispatched
	created := f.currentTime().Add(createdOffset)
	m.CreatedAt = created
	m.DispatchedAt = &created
	require.NoError(t, f.cs.CreateMessage(context.Background(), m))
	return m
}

func (f *holdStoreFixture) stampNoSession(t *testing.T, msgIDs ...string) {
	t.Helper()
	ctx := context.Background()
	deliveryID := "deliv-" + uuid.NewString()
	res, err := f.cs.BindMessageDelivery(ctx, f.agentID, deliveryID, msgIDs, f.currentTime())
	require.NoError(t, err)
	require.Equal(t, store.DeliveryBound, res.Result)
	require.NoError(t, f.cs.UnbindMessageDeliveryWithProof(ctx, deliveryID, msgIDs, store.MessageNotReachedNoSession))
}

func TestMessageHoldStore_ProofStampAndHoldCAS(t *testing.T) {
	ctx := context.Background()

	t.Run("holds only fresh no_session proof and creates marker atomically", func(t *testing.T) {
		f := newHoldStoreFixture(t)
		m := f.createDispatchedMsg(t, "secret boot prompt", 0)

		prep, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m.ID, 100)
		require.NoError(t, err)
		assert.False(t, prep.Held)
		assert.Equal(t, "run-initial", prep.HoldLaunchID)

		// Unstamped dispatched row (e.g. exec path) cannot be held.
		held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   m.ID,
			AgentID:     f.agentID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, err)
		assert.False(t, held, "unstamped dispatched row must never be held")

		// Plain unbind (not_advertised) leaves no proof stamp -> still rejected.
		res, err := f.cs.BindMessageDelivery(ctx, f.agentID, "d-unadv", []string{m.ID}, f.currentTime())
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)
		require.NoError(t, f.cs.UnbindMessageDelivery(ctx, "d-unadv", []string{m.ID}))

		held, err = f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   m.ID,
			AgentID:     f.agentID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, err)
		assert.False(t, held, "not_advertised unbind must not allow hold")

		// Hub-stamped no_session proof -> CAS holds row and creates marker.
		f.stampNoSession(t, m.ID)
		held, err = f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   m.ID,
			AgentID:     f.agentID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, err)
		assert.True(t, held)

		got, err := f.cs.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState)
		assert.Equal(t, "run-initial", got.HoldLaunchID)
		assert.Empty(t, got.NotReached, "single-use no_session proof must be consumed on HoldMessageAwaitingSession")
		assert.Nil(t, got.NotReachedAt)
		require.NotNil(t, got.HoldUntil)
		assert.Equal(t, f.currentTime().Add(10*time.Minute), *got.HoldUntil)

		// Internal hold/proof fields are json:"-" and never serialize.
		rawJSON, err := json.Marshal(got)
		require.NoError(t, err)
		assert.NotContains(t, string(rawJSON), "holdUntil")
		assert.NotContains(t, string(rawJSON), "holdLaunchId")
		assert.NotContains(t, string(rawJSON), "notReached")

		hasHold, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
		require.NoError(t, err)
		assert.True(t, hasHold)

		// M-03 & M-04: awaiting_session is neither bindable nor failed by FailUndeliveredMessage.
		bindRes, err := f.cs.BindMessageDelivery(ctx, f.agentID, "d-stale", []string{m.ID}, f.currentTime())
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryConflict, bindRes.Result)

		won, err := f.cs.FailUndeliveredMessage(ctx, m.ID, "late broker failure")
		require.NoError(t, err)
		assert.False(t, won)

		got, err = f.cs.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState)
	})

	t.Run("rejects stale proof, delivering/partial rows, and ineligible agent states", func(t *testing.T) {
		f := newHoldStoreFixture(t)

		// Stale proof (> 2m old).
		stale := f.createDispatchedMsg(t, "stale proof", 0)
		f.stampNoSession(t, stale.ID)
		f.advance(2*time.Minute + time.Second)

		held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   stale.ID,
			AgentID:     f.agentID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, err)
		assert.False(t, held, "proof older than MaxProofAge must be rejected")

		// Later bind clears a fresh proof stamp, and delivering/partial are rejected.
		ambig := f.createDispatchedMsg(t, "ambiguous", 0)
		f.stampNoSession(t, ambig.ID)
		res, err := f.cs.BindMessageDelivery(ctx, f.agentID, "d-live", []string{ambig.ID}, f.currentTime())
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		gotAmbig, err := f.cs.GetMessage(ctx, ambig.ID)
		require.NoError(t, err)
		assert.Empty(t, gotAmbig.NotReached, "BindMessageDelivery must clear not_reached stamp")
		assert.Nil(t, gotAmbig.NotReachedAt)

		held, err = f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   ambig.ID,
			AgentID:     f.agentID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, err)
		assert.False(t, held, "delivering row must never be held")

		_, err = f.cs.FinishMessageDelivery(ctx, "d-live", []string{ambig.ID}, store.MessageDispatchPartial, store.DeliveryOutcomeUnknownPartial)
		require.NoError(t, err)
		held, err = f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   ambig.ID,
			AgentID:     f.agentID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, err)
		assert.False(t, held, "partial row must never be held")

		// Non-awaiting_session failure clears proof stamp (R1).
		r1Msg := f.createDispatchedMsg(t, "exec fail", 0)
		f.stampNoSession(t, r1Msg.ID)
		won, err := f.cs.FailUndeliveredMessage(ctx, r1Msg.ID, "exec failed")
		require.NoError(t, err)
		assert.True(t, won)
		gotR1, err := f.cs.GetMessage(ctx, r1Msg.ID)
		require.NoError(t, err)
		assert.Empty(t, gotR1.NotReached)
		assert.Nil(t, gotR1.NotReachedAt)

		// Active AgentHold blocks HoldMessageAwaitingSession.
		heldTargetMsg := f.createDispatchedMsg(t, "held target", 0)
		f.stampNoSession(t, heldTargetMsg.ID)
		_, err = f.cs.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID:           f.agentID,
			ProjectID:         f.projectID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: "user",
			RootPrincipalID:   uuid.NewString(),
			Trigger:           store.MembershipLossTriggerMemberRemove,
			ActorKind:         "system",
			ActorID:           "test",
		}})
		require.NoError(t, err)

		held, err = f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   heldTargetMsg.ID,
			AgentID:     f.agentID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, err)
		assert.False(t, held, "agent under active AgentHold must not enter awaiting_session")
	})
}

func TestMessageHoldStore_PrepareOrHoldAndCap(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	// Message 1 enters hold via broker report; cap = 3.
	m1 := f.createDispatchedMsg(t, "m1", 0)
	_, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m1.ID, 3)
	require.NoError(t, err)
	f.stampNoSession(t, m1.ID)
	held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m1.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     3,
	})
	require.NoError(t, err)
	require.True(t, held)

	f.advance(10 * time.Second)

	// M-01: Messages 2 and 3 arrive while marker exists -> held pre-dispatch with marker's hold_until.
	m2 := f.createDispatchedMsg(t, "m2", 0)
	res2, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m2.ID, 3)
	require.NoError(t, err)
	assert.True(t, res2.Held)

	m3 := f.createDispatchedMsg(t, "m3", time.Millisecond)
	res3, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m3.ID, 3)
	require.NoError(t, err)
	assert.True(t, res3.Held)

	// AC A2-6: 4th message exceeds MaxHeld=3 -> PrepareOrHoldMessage stamps launch_id and returns Held=false;
	// HoldMessageAwaitingSession also refuses over cap.
	m4 := f.createDispatchedMsg(t, "m4-over-cap", 2*time.Millisecond)
	res4, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m4.ID, 3)
	require.NoError(t, err)
	assert.False(t, res4.Held)
	assert.Equal(t, "run-initial", res4.HoldLaunchID)

	f.stampNoSession(t, m4.ID)
	held4, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m4.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     3,
	})
	require.NoError(t, err)
	assert.False(t, held4, "HoldMessageAwaitingSession must enforce per-agent cap")

	// Claim all 3 held rows: returned in creation order, marker deleted in same tx.
	claimed, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 64)
	require.NoError(t, err)
	require.Len(t, claimed, 3)
	assert.Equal(t, []string{m1.ID, m2.ID, m3.ID}, []string{claimed[0].ID, claimed[1].ID, claimed[2].ID})

	hasMarker, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.False(t, hasMarker, "marker must be deleted when final held batch is claimed")
}

func TestMessageHoldStore_COALESCENeverExtendsHoldUntil(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	start := f.currentTime()
	wantHoldUntil := start.Add(10 * time.Minute)

	m := f.createDispatchedMsg(t, "flapping session", 0)
	_, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m.ID, 100)
	require.NoError(t, err)
	f.stampNoSession(t, m.ID)

	held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, held)

	// 4 minutes later: claim row, hit transient authz lookup error -> ReholdClaimedMessage (M-11 / R3).
	f.advance(4 * time.Minute)
	claimed, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 64)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NotNil(t, claimed[0].HoldUntil)
	assert.Equal(t, wantHoldUntil, *claimed[0].HoldUntil)

	reheld, err := f.cs.ReholdClaimedMessage(ctx, f.agentID, m.ID, 100)
	require.NoError(t, err)
	assert.True(t, reheld)

	hasMarker, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.True(t, hasMarker, "ReholdClaimedMessage must restore marker")

	// 3 minutes later (T+7m): claim again, dispatch, session drops -> new no_session proof and HoldMessageAwaitingSession.
	f.advance(3 * time.Minute)
	claimed, err = f.cs.ClaimHeldMessages(ctx, f.agentID, 64)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	f.stampNoSession(t, m.ID)
	held, err = f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, held)

	got, err := f.cs.GetMessage(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, got.HoldUntil)
	assert.Equal(t, wantHoldUntil, *got.HoldUntil, "re-hold must preserve original hold_until via COALESCE")

	// Advance past original 10m bound (T+10m+1s): claim returns nothing, re-hold refused, sweep expires it.
	f.advance(3*time.Minute + time.Second)
	claimedAfterDeadline, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 64)
	require.NoError(t, err)
	assert.Empty(t, claimedAfterDeadline)

	expired, err := f.cs.ExpireHeldMessagesPastDeadline(ctx, "agent session did not connect", 100)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, m.ID, expired[0].ID)

	hasMarker, err = f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.False(t, hasMarker)
}

// TestMessageHoldStore_ConcurrentClaimWithReadWriteInterceptor verifies M-08 /
// AC A2-4 / A2-S7: three concurrent ClaimHeldMessages callers (two admission
// triggers plus the 30s sweep) paused by an Ent query interceptor right after
// reading the same candidate rows and released simultaneously claim each row
// at most once.
func TestMessageHoldStore_ConcurrentClaimWithReadWriteInterceptor(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	const totalRows = 20
	wantIDs := make([]string, totalRows)
	for i := 0; i < totalRows; i++ {
		m := f.createDispatchedMsg(t, "msg", time.Duration(i)*time.Millisecond)
		wantIDs[i] = m.ID
		if i == 0 {
			_, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m.ID, 100)
			require.NoError(t, err)
			f.stampNoSession(t, m.ID)
			held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
				MessageID:   m.ID,
				AgentID:     f.agentID,
				MaxProofAge: 2 * time.Minute,
				HoldTTL:     10 * time.Minute,
				MaxHeld:     100,
			})
			require.NoError(t, err)
			require.True(t, held)
		} else {
			res, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m.ID, 100)
			require.NoError(t, err)
			require.True(t, res.Held)
		}
	}

	type pauseFlagKey struct{}
	const callers = 3
	var arrived atomic.Int32
	allRead := make(chan struct{})
	release := make(chan struct{})

	f.client.Message.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(qCtx context.Context, query ent.Query) (ent.Value, error) {
			val, err := next.Query(qCtx, query)
			if flag, ok := qCtx.Value(pauseFlagKey{}).(*atomic.Bool); ok && flag.CompareAndSwap(false, true) {
				if arrived.Add(1) == callers {
					close(allRead)
				}
				<-release
			}
			return val, err
		})
	}))

	var wg sync.WaitGroup
	results := make([][]store.Message, callers)
	errs := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var once atomic.Bool
			pCtx := context.WithValue(ctx, pauseFlagKey{}, &once)
			results[idx], errs[idx] = f.cs.ClaimHeldMessages(pCtx, f.agentID, 64)
		}(i)
	}

	<-allRead
	close(release)
	wg.Wait()

	seen := make(map[string]int, totalRows)
	totalClaimed := 0
	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		for _, m := range results[i] {
			seen[m.ID]++
			totalClaimed++
		}
	}
	assert.Equal(t, totalRows, totalClaimed, "every held row must be claimed exactly once across all concurrent callers")
	for _, id := range wantIDs {
		assert.Equal(t, 1, seen[id], "row %s claimed %d times", id, seen[id])
	}

	hasMarker, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.False(t, hasMarker)
}

// TestMessageHoldStore_InterleavingPrepareAndClaim exercises M-02: new sends
// calling PrepareOrHoldMessage interleaved with ClaimHeldMessages batches.
func TestMessageHoldStore_InterleavingPrepareAndClaim(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	seed := f.createDispatchedMsg(t, "seed", 0)
	_, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, seed.ID, 100)
	require.NoError(t, err)
	f.stampNoSession(t, seed.ID)
	held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   seed.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, held)

	const dynamicSends = 25
	directDispatched := make(map[string]bool)
	var directMu sync.Mutex
	var claimedIDs []string
	var claimedMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < dynamicSends; i++ {
			m := f.createDispatchedMsg(t, "dyn", time.Duration(i+1)*time.Millisecond)
			res, pErr := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m.ID, 100)
			require.NoError(t, pErr)
			if !res.Held {
				directMu.Lock()
				directDispatched[m.ID] = true
				directMu.Unlock()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < dynamicSends; i++ {
			batch, cErr := f.cs.ClaimHeldMessages(ctx, f.agentID, 5)
			require.NoError(t, cErr)
			claimedMu.Lock()
			for _, m := range batch {
				claimedIDs = append(claimedIDs, m.ID)
			}
			claimedMu.Unlock()
		}
	}()
	wg.Wait()

	// Drain any remaining held rows after writers finish.
	rest, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 100)
	require.NoError(t, err)
	for _, m := range rest {
		claimedIDs = append(claimedIDs, m.ID)
	}

	seen := make(map[string]int)
	for _, id := range claimedIDs {
		seen[id]++
		assert.False(t, directDispatched[id], "row %s was both held-claimed and directly dispatched", id)
	}
	for id := range directDispatched {
		seen[id]++
	}
	assert.Equal(t, 1+dynamicSends, len(seen), "every message must be accounted for exactly once")
	for id, count := range seen {
		assert.Equal(t, 1, count, "row %s accounted %d times", id, count)
	}

	hasMarker, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.False(t, hasMarker)
}

// TestMessageHoldStore_ExpiryAndSweepsIgnoreAwaitingSession covers M-06, M-10,
// M-13, AC A2-5, AC A2-9, and verifies partial index usage on SQLite.
func TestMessageHoldStore_ExpiryAndSweepsIgnoreAwaitingSession(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	mHeld := f.createDispatchedMsg(t, "held", 0)
	_, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, mHeld.ID, 100)
	require.NoError(t, err)
	f.stampNoSession(t, mHeld.ID)
	ok, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   mHeld.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, ok)

	mDelivering := f.createDispatchedMsg(t, "delivering", time.Millisecond)
	bindRes, err := f.cs.BindMessageDelivery(ctx, f.agentID, "d-keep", []string{mDelivering.ID}, f.currentTime())
	require.NoError(t, err)
	require.Equal(t, store.DeliveryBound, bindRes.Result)

	// M-13 / AC A2-9: stuck-pending sweep, stale-delivery sweep, and ListPendingMessages ignore awaiting_session.
	farFuture := f.currentTime().Add(48 * time.Hour)
	cnt, err := f.cs.CountStuckPendingMessages(ctx, farFuture)
	require.NoError(t, err)
	assert.Zero(t, cnt)

	expPending, err := f.cs.ExpireAndListStuckPendingMessages(ctx, farFuture, "expired pending")
	require.NoError(t, err)
	assert.Empty(t, expPending)

	sweptDelivering, err := f.cs.SweepStaleDeliveries(ctx, f.currentTime().Add(-time.Minute), store.DeliveryOutcomeUnknownPartial)
	require.NoError(t, err)
	assert.Zero(t, sweptDelivering)

	// Verify partial indexes exist with expected key columns on both SQLite and Postgres,
	// and check dialect-specific partial predicate / query plan output.
	assert.Equal(t, []string{"recipient_id", "created", "id"},
		messageIndexColumns(t, f.client, "message_awaiting_session_recipient_created_id"))
	assert.Equal(t, []string{"created", "id"},
		messageIndexColumns(t, f.client, "message_awaiting_session_created_id"))
	if f.client.Driver().Dialect() == dialect.Postgres {
		var rows entsql.Rows
		require.NoError(t, f.client.Driver().Query(ctx,
			`SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'messages' AND indexname IN ('message_awaiting_session_recipient_created_id', 'message_awaiting_session_created_id') ORDER BY indexname`,
			[]any{}, &rows))
		var defs []string
		for rows.Next() {
			var def string
			require.NoError(t, rows.Scan(&def))
			defs = append(defs, def)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()
		require.Len(t, defs, 2)
		for _, def := range defs {
			assert.Contains(t, def, "awaiting_session", "expected partial WHERE predicate on Postgres index, got: %s", def)
		}

		var explainRows entsql.Rows
		require.NoError(t, f.client.Driver().Query(ctx,
			"EXPLAIN SELECT id FROM messages WHERE dispatch_state = 'awaiting_session' AND recipient_id = $1 ORDER BY created, id LIMIT 64",
			[]any{f.agentID}, &explainRows))
		_ = explainRows.Close()
	} else {
		var rows entsql.Rows
		require.NoError(t, f.client.Driver().Query(ctx,
			"EXPLAIN QUERY PLAN SELECT id FROM messages WHERE dispatch_state = 'awaiting_session' AND recipient_id = ? ORDER BY created, id LIMIT 64",
			[]any{f.agentID}, &rows))
		var plans []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
			plans = append(plans, detail)
		}
		_ = rows.Close()
		assert.True(t, strings.Contains(strings.Join(plans, " "), "message_awaiting_session"),
			"expected partial index in query plan, got: %v", plans)
	}

	// M-10: ExpireHeldMessagesForAgent fails held rows, deletes marker, leaves delivering row untouched.
	expired, err := f.cs.ExpireHeldMessagesForAgent(ctx, f.agentID, "agent stopped")
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, mHeld.ID, expired[0].ID)

	gotHeld, err := f.cs.GetMessage(ctx, mHeld.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotHeld.DispatchState)
	require.NotNil(t, gotHeld.DispatchFailureReason)
	assert.Equal(t, "agent stopped", *gotHeld.DispatchFailureReason)

	gotDeliv, err := f.cs.GetMessage(ctx, mDelivering.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDelivering, gotDeliv.DispatchState)

	hasMarker, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.False(t, hasMarker)
}

func TestMessageHoldStore_RedriveLeaseExclusivityAndExpiry(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	// No marker -> AcquireAgentRedriveLease returns false.
	ok, err := f.cs.AcquireAgentRedriveLease(ctx, f.agentID, "replica-a", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)

	m1 := f.createDispatchedMsg(t, "lease-1", 0)
	m2 := f.createDispatchedMsg(t, "lease-2", time.Millisecond)
	_, err = f.cs.PrepareOrHoldMessage(ctx, f.agentID, m1.ID, 100)
	require.NoError(t, err)
	f.stampNoSession(t, m1.ID)
	held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID: m1.ID, AgentID: f.agentID, MaxProofAge: 2 * time.Minute, HoldTTL: 10 * time.Minute, MaxHeld: 100,
	})
	require.NoError(t, err)
	require.True(t, held)
	res2, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m2.ID, 100)
	require.NoError(t, err)
	require.True(t, res2.Held)

	// Replica A acquires the lease; Replica B is refused at both Acquire and Claim.
	ok, err = f.cs.AcquireAgentRedriveLease(ctx, f.agentID, "replica-a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	okB, err := f.cs.AcquireAgentRedriveLease(ctx, f.agentID, "replica-b", time.Minute)
	require.NoError(t, err)
	assert.False(t, okB, "competing replica must not acquire unexpired lease")

	stolen, err := f.cs.ClaimHeldMessages(store.WithRedriveLeaseOwner(ctx, "replica-b"), f.agentID, 64)
	require.NoError(t, err)
	assert.Empty(t, stolen, "non-owner must not claim rows while lease is active")

	unleased, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 64)
	require.NoError(t, err)
	assert.Empty(t, unleased, "unleased caller must not claim rows while lease is active")

	// Replica A claims batch 1 (1 row), renews lease, then claims final row.
	ctxA := store.WithRedriveLeaseOwner(ctx, "replica-a")
	batch1, err := f.cs.ClaimHeldMessages(ctxA, f.agentID, 1)
	require.NoError(t, err)
	require.Len(t, batch1, 1)
	assert.Equal(t, m1.ID, batch1[0].ID)

	f.advance(30 * time.Second)
	renewed, err := f.cs.RenewAgentRedriveLease(ctx, f.agentID, "replica-a", time.Minute)
	require.NoError(t, err)
	require.True(t, renewed)

	// Past original 1m TTL (now T+70s), renewed lease keeps blocking Replica B.
	f.advance(40 * time.Second)
	okB, err = f.cs.AcquireAgentRedriveLease(ctx, f.agentID, "replica-b", time.Minute)
	require.NoError(t, err)
	assert.False(t, okB)

	batch2, err := f.cs.ClaimHeldMessages(ctxA, f.agentID, 64)
	require.NoError(t, err)
	require.Len(t, batch2, 1)
	assert.Equal(t, m2.ID, batch2[0].ID)

	// Marker stays present while Replica A still holds the lease (so live sends
	// arriving mid-drain queue behind batch 2 instead of overtaking it), and is
	// deleted once ReleaseAgentRedriveLease runs with zero awaiting_session rows.
	hasMarker, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.True(t, hasMarker, "leased marker stays alive until drain releases lease")

	require.NoError(t, f.cs.ReleaseAgentRedriveLease(ctx, f.agentID, "replica-a"))
	hasMarker, err = f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.False(t, hasMarker, "releasing lease with 0 held rows must delete marker")

	// Lease expiry takeover: if Replica A stalls past TTL, Replica B can take over.
	m3 := f.createDispatchedMsg(t, "lease-3", 2*time.Millisecond)
	_, err = f.cs.PrepareOrHoldMessage(ctx, f.agentID, m3.ID, 100)
	require.NoError(t, err)
	f.stampNoSession(t, m3.ID)
	held, err = f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID: m3.ID, AgentID: f.agentID, MaxProofAge: 2 * time.Minute, HoldTTL: 10 * time.Minute, MaxHeld: 100,
	})
	require.NoError(t, err)
	require.True(t, held)

	ok, err = f.cs.AcquireAgentRedriveLease(ctx, f.agentID, "replica-a", 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok)

	f.advance(31 * time.Second)
	okB, err = f.cs.AcquireAgentRedriveLease(ctx, f.agentID, "replica-b", time.Minute)
	require.NoError(t, err)
	require.True(t, okB, "expired lease must allow replica B takeover")

	renewStaleA, err := f.cs.RenewAgentRedriveLease(ctx, f.agentID, "replica-a", time.Minute)
	require.NoError(t, err)
	assert.False(t, renewStaleA, "superseded owner must not renew lease")

	staleClaimA, err := f.cs.ClaimHeldMessages(ctxA, f.agentID, 64)
	require.NoError(t, err)
	assert.Empty(t, staleClaimA, "superseded owner must not claim rows")

	claimedB, err := f.cs.ClaimHeldMessages(store.WithRedriveLeaseOwner(ctx, "replica-b"), f.agentID, 64)
	require.NoError(t, err)
	require.Len(t, claimedB, 1)
	assert.Equal(t, m3.ID, claimedB[0].ID)
	require.NoError(t, f.cs.ReleaseAgentRedriveLease(ctx, f.agentID, "replica-b"))
}

func TestMessageHoldStore_ConcurrentCapNeverExceedsMaxHeld(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)
	const maxHeld = 10
	const totalSenders = 32

	msgs := make([]*store.Message, totalSenders)
	for i := 0; i < totalSenders; i++ {
		msgs[i] = f.createDispatchedMsg(t, "cap-race", time.Duration(i)*time.Millisecond)
		if i%2 == 0 {
			_, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, msgs[i].ID, maxHeld)
			require.NoError(t, err)
			f.stampNoSession(t, msgs[i].ID)
		}
	}

	// Seed the first held row so both PrepareOrHoldMessage and HoldMessageAwaitingSession
	// race concurrently against maxHeld.
	held0, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID: msgs[0].ID, AgentID: f.agentID, MaxProofAge: 2 * time.Minute, HoldTTL: 10 * time.Minute, MaxHeld: maxHeld,
	})
	require.NoError(t, err)
	require.True(t, held0)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var admitted atomic.Int32
	admitted.Store(1)
	for i := 1; i < totalSenders; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				ok, hErr := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
					MessageID: msgs[i].ID, AgentID: f.agentID, MaxProofAge: 2 * time.Minute, HoldTTL: 10 * time.Minute, MaxHeld: maxHeld,
				})
				assert.NoError(t, hErr)
				if ok {
					admitted.Add(1)
				}
			} else {
				res, pErr := f.cs.PrepareOrHoldMessage(ctx, f.agentID, msgs[i].ID, maxHeld)
				assert.NoError(t, pErr)
				if res.Held {
					admitted.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(maxHeld), admitted.Load(), "concurrent callers must admit exactly maxHeld rows")
	claimed, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 100)
	require.NoError(t, err)
	assert.Len(t, claimed, maxHeld)
}

func TestMessageHoldStore_ScheduledEventIDRoundTripAndJSONOmission(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	evtID := uuid.NewString()
	m := &store.Message{
		ID:               uuid.NewString(),
		ProjectID:        f.projectID,
		Sender:           "scheduler",
		SenderID:         "SCHEDULER",
		Recipient:        "agent:target",
		RecipientID:      f.agentID,
		AgentID:          f.agentID,
		Msg:              "scheduled body",
		Type:             "system",
		DispatchState:    store.MessageDispatchDispatched,
		ScheduledEventID: evtID,
		CreatedAt:        f.currentTime(),
	}
	require.NoError(t, f.cs.CreateMessage(ctx, m))

	got, err := f.cs.GetMessage(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, evtID, got.ScheduledEventID)

	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), evtID, "ScheduledEventID must be omitted from JSON")
}

// TestMessageHoldStore_LaterEpisodeExtendsMarkerHoldUntil verifies RV3-O1:
// when a second hold episode holds (or re-holds) a fresh row with a later
// hold_until while an earlier marker still exists, the marker's hold_until
// advances to the max across remaining awaiting_session rows so new sends keep
// queueing behind unexpired held rows even after the first episode's bound
// passes, while individual rows keep their own COALESCE(hold_until, now+10m).
func TestMessageHoldStore_LaterEpisodeExtendsMarkerHoldUntil(t *testing.T) {
	ctx := context.Background()
	f := newHoldStoreFixture(t)

	start := f.currentTime()
	ep1Bound := start.Add(10 * time.Minute)

	// Episode 1 at T=0: m1 held until T+10m; m2 created before marker (HoldUntil nil)
	// so it represents an in-flight/cap-bypassed message from a later drop at T+6m.
	m2 := f.createDispatchedMsg(t, "ep2-fresh", 0)
	m1 := f.createDispatchedMsg(t, "ep1", time.Millisecond)
	f.stampNoSession(t, m1.ID)
	held1, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m1.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, held1)

	// Episode 2 at T+6m while marker from Episode 1 still exists: m2 gets a
	// fresh no_session proof and holds until T+16m.
	f.advance(6 * time.Minute)
	ep2Bound := f.currentTime().Add(10 * time.Minute)
	f.stampNoSession(t, m2.ID)
	held2, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m2.ID,
		AgentID:     f.agentID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, held2)

	// Individual row bounds: m1 keeps T+10m (COALESCE), m2 gets T+16m.
	got1, err := f.cs.GetMessage(ctx, m1.ID)
	require.NoError(t, err)
	require.NotNil(t, got1.HoldUntil)
	assert.True(t, got1.HoldUntil.Equal(ep1Bound), "m1 must keep its original T+10m bound")

	got2, err := f.cs.GetMessage(ctx, m2.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.HoldUntil)
	assert.True(t, got2.HoldUntil.Equal(ep2Bound), "m2 must get T+16m bound")

	// Advance to T+11m (past Episode 1's T+10m bound, still within Episode 2's T+16m bound).
	f.advance(5 * time.Minute)

	// New send m3 at T+11m MUST still queue behind m2 because marker.HoldUntil extended to T+16m!
	m3 := f.createDispatchedMsg(t, "ep2-queued-after-ep1-deadline", 2*time.Millisecond)
	res3, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m3.ID, 100)
	require.NoError(t, err)
	require.True(t, res3.Held, "new send must keep queueing behind m2 after Episode 1 deadline passed")
	require.NotNil(t, res3.HoldUntil)
	assert.True(t, res3.HoldUntil.Equal(ep2Bound), "queued row must inherit the extended Episode 2 marker bound")

	// ExpireHeldMessagesPastDeadlineForAgent expires ONLY m1 (past T+10m) and keeps m2 + m3 + marker.
	expired, err := f.cs.ExpireHeldMessagesPastDeadlineForAgent(ctx, f.agentID, "agent session did not connect")
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, m1.ID, expired[0].ID)

	hasMarker, err := f.cs.HasAgentDeliveryHold(ctx, f.agentID)
	require.NoError(t, err)
	assert.True(t, hasMarker, "marker must remain for unexpired Episode 2 rows")

	claimed, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 64)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.Equal(t, []string{m2.ID, m3.ID}, []string{claimed[0].ID, claimed[1].ID})
}

func TestMessageHoldStore_HoldTimestampsNormalizedToUTC(t *testing.T) {
	// 1. Direct mapper check: pgx scans PostgreSQL timestamptz columns in
	// time.Local (non-UTC Location). Verify entMessageToStore normalizes
	// HoldUntil and NotReachedAt to time.UTC even when running on SQLite.
	nonUTC := time.FixedZone("pgx-local", -7*3600)
	localHold := time.Date(2026, 11, 15, 0, 10, 0, 0, time.UTC).In(nonUTC)
	localProof := time.Date(2026, 11, 15, 0, 0, 30, 0, time.UTC).In(nonUTC)
	mapped := entMessageToStore(&ent.Message{
		ID:           uuid.New(),
		ProjectID:    uuid.New(),
		HoldUntil:    &localHold,
		NotReachedAt: &localProof,
	})
	require.NotNil(t, mapped.HoldUntil)
	require.NotNil(t, mapped.NotReachedAt)
	assert.Equal(t, time.UTC, mapped.HoldUntil.Location(), "HoldUntil must normalize to time.UTC")
	assert.Equal(t, time.Date(2026, 11, 15, 0, 10, 0, 0, time.UTC), *mapped.HoldUntil)
	assert.Equal(t, time.UTC, mapped.NotReachedAt.Location(), "NotReachedAt must normalize to time.UTC")
	assert.Equal(t, time.Date(2026, 11, 15, 0, 0, 30, 0, time.UTC), *mapped.NotReachedAt)

	// 2. Round-trip store check across HoldMessageAwaitingSession,
	// PrepareOrHoldMessage, GetMessage, and ClaimHeldMessages.
	ctx := context.Background()
	f := newHoldStoreFixture(t)
	wantHoldUntil := f.currentTime().Add(10 * time.Minute)

	m1 := f.createDispatchedMsg(t, "utc-1", 0)
	_, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m1.ID, 100)
	require.NoError(t, err)
	f.stampNoSession(t, m1.ID)

	gotProof, err := f.cs.GetMessage(ctx, m1.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProof.NotReachedAt)
	assert.Equal(t, time.UTC, gotProof.NotReachedAt.Location())

	held, err := f.cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID: m1.ID, AgentID: f.agentID, MaxProofAge: 2 * time.Minute, HoldTTL: 10 * time.Minute, MaxHeld: 100,
	})
	require.NoError(t, err)
	require.True(t, held)

	m2 := f.createDispatchedMsg(t, "utc-2", time.Millisecond)
	prep2, err := f.cs.PrepareOrHoldMessage(ctx, f.agentID, m2.ID, 100)
	require.NoError(t, err)
	require.True(t, prep2.Held)
	require.NotNil(t, prep2.HoldUntil)
	assert.Equal(t, time.UTC, prep2.HoldUntil.Location())
	assert.Equal(t, wantHoldUntil, *prep2.HoldUntil)

	claimed, err := f.cs.ClaimHeldMessages(ctx, f.agentID, 64)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	for _, cm := range claimed {
		require.NotNil(t, cm.HoldUntil)
		assert.Equal(t, time.UTC, cm.HoldUntil.Location())
		assert.Equal(t, wantHoldUntil, *cm.HoldUntil)
	}
}
