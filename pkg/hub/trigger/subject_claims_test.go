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

package trigger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type fakeAgentLookup struct {
	agents map[string]*store.Agent
}

func (f *fakeAgentLookup) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	if ag, ok := f.agents[id]; ok {
		return ag, nil
	}
	return nil, store.ErrNotFound
}

type stubClaimStore struct {
	store.TriggerStore
	active   map[string]*store.TriggerSubjectClaim
	released []string
}

func (s *stubClaimStore) key(triggerID, subject string) string {
	return triggerID + "\x00" + subject
}

func (s *stubClaimStore) ClaimTriggerSubject(_ context.Context, triggerID, subject, eventID, fireID string, now, abandonedBefore time.Time) (*store.TriggerSubjectClaim, *store.TriggerSubjectClaim, error) {
	k := s.key(triggerID, subject)
	if cur, ok := s.active[k]; ok {
		if cur.State == store.TriggerSubjectClaimStateActive &&
			(cur.AgentID != "" || !cur.ClaimedAt.Before(abandonedBefore)) {
			cp := *cur
			return nil, &cp, nil
		}
		cur.State = store.TriggerSubjectClaimStateActive
		cur.AgentID = ""
		cur.EventID = eventID
		cur.FireID = fireID
		cur.ClaimedAt = now
		cur.BoundAt = nil
		cur.ReleasedAt = nil
		cp := *cur
		return &cp, nil, nil
	}
	c := &store.TriggerSubjectClaim{
		ID:        uuid.NewString(),
		TriggerID: triggerID,
		Subject:   subject,
		State:     store.TriggerSubjectClaimStateActive,
		EventID:   eventID,
		FireID:    fireID,
		ClaimedAt: now,
	}
	s.active[k] = c
	cp := *c
	return &cp, nil, nil
}

func (s *stubClaimStore) BindTriggerSubjectClaim(_ context.Context, claimID, fireID, agentID string, boundAt time.Time) (bool, error) {
	for _, c := range s.active {
		if c.ID == claimID && c.FireID == fireID && c.State == store.TriggerSubjectClaimStateActive {
			c.AgentID = agentID
			bt := boundAt
			c.BoundAt = &bt
			return true, nil
		}
	}
	return false, nil
}

func (s *stubClaimStore) ReleaseTriggerSubjectClaim(_ context.Context, claimID, fireID string, releasedAt time.Time) (bool, error) {
	for _, c := range s.active {
		if c.ID == claimID && c.FireID == fireID && c.State == store.TriggerSubjectClaimStateActive {
			c.State = store.TriggerSubjectClaimStateReleased
			rt := releasedAt
			c.ReleasedAt = &rt
			s.released = append(s.released, claimID)
			return true, nil
		}
	}
	return false, nil
}

func (s *stubClaimStore) GetActiveTriggerSubjectClaim(_ context.Context, triggerID, subject string) (*store.TriggerSubjectClaim, error) {
	if c, ok := s.active[s.key(triggerID, subject)]; ok && c.State == store.TriggerSubjectClaimStateActive {
		cp := *c
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func TestSubjectClaims_LiveLazilyReleasesTerminalOrDeletedAndKeepsSuspended(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ts := &stubClaimStore{active: map[string]*store.TriggerSubjectClaim{}}
	agents := &fakeAgentLookup{agents: map[string]*store.Agent{}}
	claims := trigger.NewSubjectClaims(ts, agents, func() time.Time { return now })

	trigID := uuid.NewString()

	// Unbound active claim: Live returns nil without releasing.
	cUnbound, ex, err := claims.Claim(ctx, trigID, "sub-unbound", uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, ex)
	live, err := claims.Live(ctx, trigID, "sub-unbound")
	require.NoError(t, err)
	assert.Nil(t, live)
	assert.Empty(t, ts.released, "unbound in-flight claim must not be released by Live")

	// Running and suspended agents count as live (§5.6).
	for _, ph := range []state.Phase{state.PhaseRunning, state.PhaseSuspended, state.PhaseCreated, state.PhaseStarting} {
		agID := uuid.NewString()
		agents.agents[agID] = &store.Agent{ID: agID, Phase: string(ph)}
		sub := "sub-" + string(ph)
		c, ex, err := claims.Claim(ctx, trigID, sub, uuid.NewString(), uuid.NewString())
		require.NoError(t, err)
		require.Nil(t, ex)
		require.NoError(t, claims.Bind(ctx, c, agID))
		live, err := claims.Live(ctx, trigID, sub)
		require.NoError(t, err)
		require.NotNil(t, live, "phase %s must be live", ph)
		assert.Equal(t, agID, live.AgentID)
	}

	// Terminal phases (stopped, error, halted), soft-deleted, and missing agents are lazily released by Live.
	deadCases := []struct {
		name string
		ag   *store.Agent
	}{
		{"stopped", &store.Agent{ID: uuid.NewString(), Phase: string(state.PhaseStopped)}},
		{"error", &store.Agent{ID: uuid.NewString(), Phase: string(state.PhaseError)}},
		{"halted", &store.Agent{ID: uuid.NewString(), Phase: string(state.PhaseHalted)}},
		{"soft_deleted", &store.Agent{ID: uuid.NewString(), Phase: string(state.PhaseRunning), DeletedAt: now}},
		{"missing", nil},
	}
	for _, tc := range deadCases {
		t.Run(tc.name, func(t *testing.T) {
			agID := uuid.NewString()
			if tc.ag != nil {
				agID = tc.ag.ID
				agents.agents[agID] = tc.ag
			}
			sub := "sub-" + tc.name
			c, ex, err := claims.Claim(ctx, trigID, sub, uuid.NewString(), uuid.NewString())
			require.NoError(t, err)
			require.Nil(t, ex)
			require.NoError(t, claims.Bind(ctx, c, agID))

			live, err := claims.Live(ctx, trigID, sub)
			require.NoError(t, err)
			assert.Nil(t, live, "%s agent must not be live", tc.name)

			// Re-claiming immediately succeeds because Live released the stale claim.
			c2, ex2, err := claims.Claim(ctx, trigID, sub, uuid.NewString(), uuid.NewString())
			require.NoError(t, err)
			assert.Nil(t, ex2)
			assert.Equal(t, c.ID, c2.ID)
		})
	}

	// Abandoned unbound claim is taken over after 5 minutes and Bind by old holder returns ErrClaimNotHeld.
	now = now.Add(trigger.AbandonedClaimTTL + time.Second)
	cTaken, ex, err := claims.Claim(ctx, trigID, "sub-unbound", uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, ex)
	assert.Equal(t, cUnbound.ID, cTaken.ID)
	assert.ErrorIs(t, claims.Bind(ctx, cUnbound, uuid.NewString()), trigger.ErrClaimNotHeld)
}
