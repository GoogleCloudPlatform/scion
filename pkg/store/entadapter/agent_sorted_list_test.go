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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCountAgents_MatchesListAgentsTotal pins that CountAgents applies the
// exact predicate ListAgents' own COUNT does.
func TestCountAgents_MatchesListAgentsTotal(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	for i := 0; i < 7; i++ {
		a := makeAgent(projectID, fmt.Sprintf("count-%d", i))
		if i%2 == 0 {
			a.Labels = map[string]string{"team": "a"}
		} else {
			a.Labels = map[string]string{"team": "b"}
		}
		require.NoError(t, s.CreateAgent(ctx, a))
	}

	n, err := s.CountAgents(ctx, store.AgentFilter{ProjectID: projectID})
	require.NoError(t, err)
	assert.Equal(t, 7, n)

	n, err = s.CountAgents(ctx, store.AgentFilter{ProjectID: projectID, Labels: map[string]string{"team": "a"}})
	require.NoError(t, err)
	assert.Equal(t, 4, n)

	result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{Limit: 500, SkipTotalCount: false})
	require.NoError(t, err)
	assert.Equal(t, result.TotalCount, 7, "CountAgents must agree with ListAgents' own total")
}

// createAgentWithTimestamps inserts an agent directly through the raw ent
// client (bypassing store.CreateAgent, which always stamps Created/Updated
// to time.Now()) so tests can control the exact timestamps sorted-mode
// ordering depends on, including sub-second trailing-zero fractions and a
// NULL last_activity_event. created is required (the schema's Created field
// is NOT NULL); updated defaults to created when zero; lastActivity is left
// NULL (unset) when zero, matching agent_store.go's "a zero LastActivityEvent
// is stored as NULL" rule.
func createAgentWithTimestamps(t *testing.T, s *AgentStore, projectID, slug string, created, updated, lastActivity time.Time) *store.Agent {
	t.Helper()
	ctx := context.Background()
	if updated.IsZero() {
		updated = created
	}
	a := makeAgent(projectID, slug)
	projectUID := uuid.MustParse(projectID)
	id := uuid.New()
	create := s.client.Agent.Create().
		SetID(id).
		SetSlug(a.Slug).
		SetName(a.Name).
		SetTemplate(a.Template).
		SetProjectID(projectUID).
		SetPhase(a.Phase).
		SetActivity(a.Activity).
		SetMessageMode(agent.MessageModeNone).
		SetCreated(created).
		SetUpdated(updated).
		SetStateVersion(1).
		SetGeneration(1)
	if !lastActivity.IsZero() {
		create.SetLastActivityEvent(lastActivity)
	}
	if a.Labels != nil {
		create.SetLabels(a.Labels)
	}
	row, err := create.Save(ctx)
	require.NoError(t, err)
	return entAgentToStore(row)
}

// TestListAgentMembers_OrderMatchesAgentsortReference is the store-layer
// order-parity check: pages/positions derived from ListAgentMembers must
// equal the agentsort package's own total order over the same rows, for
// both sort keys and both directions, including SQLite trailing-zero-fraction
// and NULL last_activity_event cases.
//
// The non-UTC time.Time case is deliberately NOT exercised by writing a
// non-UTC time.Time through this store's SQLite backend: doing so hits a
// pre-existing, unrelated ent/database-sql scan limitation ("unsupported
// Scan ... storing driver.Value type string into type *time.Time") that
// affects any time column write through this adapter, not something P1b
// introduces. Non-UTC correctness is instead proven at the pure-function
// level, where it matters (agentsort.Compare/Less never read a time.Time's
// zone, only its instant): see
// pkg/store/agentsort/agentsort_test.go:TestSortRows_NonUTC. Every
// time.Time this package actually feeds into agentsort (store reads,
// decoded cursors) is UTC in practice.
func TestListAgentMembers_OrderMatchesAgentsortReference(t *testing.T) {
	s, projectID := newTestAgentStore(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	slugByID := make(map[string]string)
	seed := func(slug string, created, updated, lastActivity time.Time) {
		a := createAgentWithTimestamps(t, s, projectID, slug, created, updated, lastActivity)
		slugByID[a.ID] = slug
	}

	// a, b: equal "updated", so sort=updated ties them, broken by created
	// desc (b was created after a).
	seed("a", base, base.Add(1*time.Hour), time.Time{})
	seed("b", base.Add(1*time.Minute), base.Add(1*time.Hour), time.Time{})
	// c: LastActivityEvent set, non-zero -> K uses it, not Updated (COALESCE).
	seed("c", base.Add(2*time.Minute), base.Add(2*time.Hour), base.Add(3*time.Hour))
	// d, e: sub-second trailing-zero fractions; d < e chronologically even
	// though "…05.1Z" vs "…05.12Z" would misorder under localeCompare.
	seed("d", base.Add(3*time.Minute), base.Add(5*time.Second+100*time.Millisecond), time.Time{})
	seed("e", base.Add(4*time.Minute), base.Add(5*time.Second+120*time.Millisecond), time.Time{})

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			got, err := s.ListAgentMembers(context.Background(), store.AgentFilter{ProjectID: projectID}, sortKey, dir, 100)
			require.NoError(t, err)
			require.Len(t, got, 5)
			assertMembersMatchRowOrder(t, sortKey, dir, got)
		}
	}

	// Pin the concrete expected order for sort=updated desc, since that is
	// the one P1b actually serves. Descending by K (updated, or
	// LastActivityEvent when set): c (K=+3h) > b,a (K=+1h, tied; b sorts
	// first because it was created after a) > e > d (K=+5.12s, +5.1s: the
	// true-time, not string, order).
	got, err := s.ListAgentMembers(context.Background(), store.AgentFilter{ProjectID: projectID}, agentsort.Updated, agentsort.Desc, 100)
	require.NoError(t, err)
	slugs := make([]string, len(got))
	for i, m := range got {
		slugs[i] = slugByID[m.ID]
	}
	assert.Equal(t, []string{"c", "b", "a", "e", "d"}, slugs, "sort=updated desc order")
}

// assertMembersMatchRowOrder asserts ListAgentMembers' own output is already
// in the agentsort total order (not merely "looks sorted" by coincidence).
func assertMembersMatchRowOrder(t *testing.T, sortKey, dir string, got []store.AgentMember) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		prev := agentsort.KeyFor(sortKey, got[i-1].ID, got[i-1].Created, got[i-1].Updated, got[i-1].LastActivityEvent)
		cur := agentsort.KeyFor(sortKey, got[i].ID, got[i].Created, got[i].Updated, got[i].LastActivityEvent)
		if agentsort.Less(dir, cur, prev) {
			t.Fatalf("sort=%s dir=%s: row %d (%s) sorts before row %d (%s), but ListAgentMembers returned them in the opposite order",
				sortKey, dir, i, got[i].ID, i-1, got[i-1].ID)
		}
	}
}

// TestListAgentMembers_ProjectionEqualsFullRow is the store-layer half of the
// non-waivable member/full equality gate: every field ListAgentMembers
// copies into AgentMember must equal the same field on the full row
// GetAgentsByIDs returns for the same agent.
func TestListAgentMembers_ProjectionEqualsFullRow(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "proj-eq")
	a.Labels = map[string]string{"k": "v", "team": "x"}
	require.NoError(t, s.CreateAgent(ctx, a))

	members, err := s.ListAgentMembers(ctx, store.AgentFilter{ProjectID: projectID}, agentsort.Updated, agentsort.Desc, 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	m := members[0]

	full, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	assert.Equal(t, full.ID, m.ID)
	assert.Equal(t, full.OwnerID, m.OwnerID)
	assert.Equal(t, full.ProjectID, m.ProjectID)
	assert.Equal(t, full.Labels, m.Labels)
	assert.Equal(t, full.Ancestry, m.Ancestry)
	assert.Equal(t, full.Phase, m.Phase)
	assert.True(t, full.Created.Equal(m.Created))
	assert.True(t, full.Updated.Equal(m.Updated))
	assert.True(t, full.LastActivityEvent.Equal(m.LastActivityEvent))

	// ToAgent reconstructs a store.Agent whose fields agree with the full
	// row on every field agentResource reads.
	reconstructed := m.ToAgent()
	assert.Equal(t, full.ID, reconstructed.ID)
	assert.Equal(t, full.OwnerID, reconstructed.OwnerID)
	assert.Equal(t, full.ProjectID, reconstructed.ProjectID)
	assert.Equal(t, full.Labels, reconstructed.Labels)
	assert.Equal(t, full.Ancestry, reconstructed.Ancestry)
}

// TestListAgentMembers_MaxBoundsCandidatePool asserts the sorted-mode
// candidate ceiling's read-side bound: a candidate pool
// larger than max returns exactly max rows, never more, so a pool that grew
// between the caller's COUNT and this read is still detected by comparing
// len(result) against the ceiling.
func TestListAgentMembers_MaxBoundsCandidatePool(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	const n = 12
	for i := 0; i < n; i++ {
		require.NoError(t, s.CreateAgent(ctx, makeAgent(projectID, fmt.Sprintf("ceil-%d", i))))
	}

	members, err := s.ListAgentMembers(ctx, store.AgentFilter{ProjectID: projectID}, agentsort.Updated, agentsort.Desc, 5)
	require.NoError(t, err)
	assert.Len(t, members, 5, "ListAgentMembers must not return more than max rows")

	members, err = s.ListAgentMembers(ctx, store.AgentFilter{ProjectID: projectID}, agentsort.Updated, agentsort.Desc, 100)
	require.NoError(t, err)
	assert.Len(t, members, n, "ListAgentMembers must return every candidate up to max")
}

// TestListAgentMembers_RespectsFilter confirms ListAgentMembers applies the
// same AgentFilter predicates ListAgents/CountAgents do (label, phase),
// since it shares agentFilterPredicates.
func TestListAgentMembers_RespectsFilter(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	running := makeAgent(projectID, "running-1")
	running.Phase = "running"
	require.NoError(t, s.CreateAgent(ctx, running))

	stopped := makeAgent(projectID, "stopped-1")
	stopped.Phase = "stopped"
	require.NoError(t, s.CreateAgent(ctx, stopped))

	members, err := s.ListAgentMembers(ctx, store.AgentFilter{ProjectID: projectID, Phase: "running"}, agentsort.Updated, agentsort.Desc, 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, "running", members[0].Phase)
}
