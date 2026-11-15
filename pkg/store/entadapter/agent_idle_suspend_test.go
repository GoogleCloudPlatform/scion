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
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAgentStore_ListIdleSuspendCandidates_Activities verifies across every
// Activity value (plus empty) that only idle, completed, and harness_error
// are returned when running, past the grace cutoff, and with a recent
// heartbeat.
func TestAgentStore_ListIdleSuspendCandidates_Activities(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	cutoff := now.Add(-15 * time.Minute)
	heartbeatRecency := now.Add(-2 * time.Minute)

	wantIDs := map[string]string{}
	for i, act := range append(state.Activities(), "") {
		slug := fmt.Sprintf("act-%d", i)
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(act)
		a.LastSeen = now.Add(-30 * time.Second)
		require.NoError(t, s.CreateAgent(ctx, a))

		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetActivitySince(now.Add(-20 * time.Minute)).
			Save(ctx)
		require.NoError(t, err)

		if act.IsSuspendEligible() {
			wantIDs[a.ID] = string(act)
		}
	}
	require.Len(t, wantIDs, 3, "expected idle, completed, and harness_error to be eligible")

	got, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency)
	require.NoError(t, err)

	gotIDs := map[string]string{}
	for _, a := range got {
		gotIDs[a.ID] = a.Activity
	}
	assert.Equal(t, wantIDs, gotIDs)
}

// TestAgentStore_ListIdleSuspendCandidates_Predicates verifies the phase,
// activity_since (including NULL and boundary), heartbeat recency (offline /
// stale / NULL last_seen), and soft-delete filters.
func TestAgentStore_ListIdleSuspendCandidates_Predicates(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	cutoff := now.Add(-15 * time.Minute)
	heartbeatRecency := now.Add(-2 * time.Minute)

	makeIdle := func(slug string, lastSeen time.Time) *store.Agent {
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.LastSeen = lastSeen
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	setSince := func(id string, ts *time.Time) {
		upd := s.client.Agent.UpdateOneID(uuid.MustParse(id))
		if ts == nil {
			upd.ClearActivitySince()
		} else {
			upd.SetActivitySince(*ts)
		}
		_, err := upd.Save(ctx)
		require.NoError(t, err)
	}

	past := now.Add(-20 * time.Minute)
	recent := now.Add(-5 * time.Minute)

	// 1. Eligible baseline.
	eligible := makeIdle("eligible", now.Add(-30*time.Second))
	setSince(eligible.ID, &past)

	// 2. Inside grace window (activity_since >= cutoff) -> excluded.
	withinGrace := makeIdle("within-grace", now.Add(-30*time.Second))
	setSince(withinGrace.ID, &recent)

	// 3. Exact cutoff (activity_since == cutoff) -> excluded (strict < cutoff).
	atCutoff := makeIdle("at-cutoff", now.Add(-30*time.Second))
	setSince(atCutoff.ID, &cutoff)

	// 4. NULL activity_since (pre-migration row) -> never eligible.
	nullSince := makeIdle("null-since", now.Add(-30*time.Second))
	setSince(nullSince.ID, nil)

	// 5. Stale heartbeat (last_seen < heartbeatRecency) -> offline/stale excluded.
	staleSeen := makeIdle("stale-seen", now.Add(-10*time.Minute))
	setSince(staleSeen.ID, &past)

	// 6. NULL last_seen -> excluded.
	nullSeen := makeIdle("null-seen", time.Time{})
	_, err := s.client.Agent.UpdateOneID(uuid.MustParse(nullSeen.ID)).
		ClearLastSeen().
		SetActivitySince(past).
		Save(ctx)
	require.NoError(t, err)

	// 7. Non-running phase -> excluded even if columns somehow held idle/since.
	notRunning := makeIdle("not-running", now.Add(-30*time.Second))
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(notRunning.ID)).
		SetPhase(string(state.PhaseSuspended)).
		SetActivitySince(past).
		Save(ctx)
	require.NoError(t, err)

	// 8. Soft-deleted row -> excluded.
	softDeleted := makeIdle("soft-deleted", now.Add(-30*time.Second))
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(softDeleted.ID)).
		SetActivitySince(past).
		SetDeletedAt(now).
		Save(ctx)
	require.NoError(t, err)

	got, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, eligible.ID, got[0].ID)
}
