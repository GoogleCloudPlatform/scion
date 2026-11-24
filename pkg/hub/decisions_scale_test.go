//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func rebindScaleSQL(d, q string) string {
	if d != dialect.Postgres {
		return q
	}
	var b strings.Builder
	idx := 1
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			_, _ = fmt.Fprintf(&b, "$%d", idx)
			idx++
		} else {
			b.WriteByte(q[i])
		}
	}
	return b.String()
}

func measureHubColdAndWarmStats(t *testing.T, warmRuns int, fn func()) (cold, warmMedian, tailMedian, tailMax time.Duration) {
	t.Helper()
	t0 := time.Now()
	fn()
	cold = time.Since(t0)
	samples := make([]time.Duration, warmRuns)
	for i := 0; i < warmRuns; i++ {
		t1 := time.Now()
		fn()
		samples[i] = time.Since(t1)
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	warmMedian = sorted[len(sorted)/2]

	// Tail calls (calls 7..N, 1-indexed including cold = warm indices 5..N-1)
	// capture Postgres pgx prepared-statement generic-plan execution after the
	// 5th execution on one pool.
	tailStart := 5
	if tailStart >= len(samples) {
		tailStart = 0
	}
	tail := append([]time.Duration(nil), samples[tailStart:]...)
	sort.Slice(tail, func(i, j int) bool { return tail[i] < tail[j] })
	tailMedian = tail[len(tail)/2]
	tailMax = tail[len(tail)-1]
	return cold, warmMedian, tailMedian, tailMax
}

func measureHubColdAndWarmMedian(t *testing.T, warmRuns int, fn func()) (cold, warmMedian time.Duration) {
	t.Helper()
	cold, warmMedian, _, _ = measureHubColdAndWarmStats(t, warmRuns, fn)
	return cold, warmMedian
}

func seedSettledAndOpenHistoryForQueryCount(
	t *testing.T,
	f *decisionsFixture,
	cs *entadapter.CompositeStore,
	numProjects int,
	settledPerProject int,
	openProjects int,
) {
	t.Helper()
	ctx := context.Background()
	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)

	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	for p := 0; p < numProjects; p++ {
		proj := &store.Project{
			ID:      api.NewUUID(),
			Name:    fmt.Sprintf("qc-proj-%03d", p),
			Slug:    fmt.Sprintf("qc-proj-%03d", p),
			Created: base,
		}
		require.NoError(t, f.store.CreateProject(ctx, proj))
		_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRole.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      f.bob.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          proj.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
		tpID := createTestDecisionTopic(t, f.wcs, proj.ID, "general", f.bob.ID)
		botID := api.NewUUID()

		// Bulk-insert settled & facet-indexed history directly into SQL for fast fixture setup.
		var latestTS time.Time
		for j := 0; j < settledPerProject; j++ {
			decID := uuid.New()
			msgID := uuid.New()
			ts := base.Add(time.Duration(p*10000+j) * time.Second)
			latestTS = ts
			key := fmt.Sprintf("h-%03d-%05d", p, j)
			_, err := cs.DB().ExecContext(ctx,
				`INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, facet_indexed, created_at, withdrawn_at)
				 VALUES (?, ?, ?, ?, 0, ?, 'bot', ?, ?, 'agent', 1, 'History?', '[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]', false, 0, 'withdrawn', true, true, ?, ?)`,
				decID.String(), proj.ID, tpID, msgID.String(), botID, key, key, ts, ts,
			)
			require.NoError(t, err)
		}
		if settledPerProject > 0 {
			_, err := cs.DB().ExecContext(ctx,
				`INSERT INTO decision_facet_entries (id, project_id, chat_key, kind, actor_id, name, email, latest_at, ref_count)
				 VALUES (?, ?, ?, 'asker', ?, 'bot', '', ?, ?)`,
				uuid.NewString(), proj.ID, tpID, botID, latestTS, settledPerProject,
			)
			require.NoError(t, err)
		}

		if p < openProjects {
			_, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
				Message: &store.Message{
					ID: api.NewUUID(), ProjectID: proj.ID, Sender: "agent:bot", SenderID: botID,
					Recipient: "chat", ThreadID: tpID, Channel: "web",
					CreatedAt: base.Add(time.Hour),
				},
				AskerAgentID:   botID,
				AskerAgentName: "bot",
				Decisions: []messages.Decision{
					{Key: fmt.Sprintf("open-%03d", p), Question: "Open?", Choices: choices},
				},
			})
			require.NoError(t, err)
		}
	}
}

// TestDecisionsQueryCount_LimitZeroAndWaitingOnYouTotal proves that both
// GET /api/v1/users/me/decisions?tab=pending&limit=0 and waitingOnYouTotal
// issue a constant number of SQL queries with respect to project count
// (5 vs 40) and history depth (10 vs 300) (design-scale §7 item 4).
func TestDecisionsQueryCount_LimitZeroAndWaitingOnYouTotal(t *testing.T) {
	measure := func(t *testing.T, numProjects, settledPerProject, openProjects int) (limitZeroCold, limitZeroWarm, bellQueries int64) {
		t.Helper()
		f, cs, counter := newCountingDecisionsFixture(t)
		seedSettledAndOpenHistoryForQueryCount(t, f, cs, numProjects, settledPerProject, openProjects)
		ctx := context.Background()

		// First HTTP call on a fresh server includes 3 extra queries from the
		// throttled auth-middleware users.last_seen update (SELECT user,
		// UPDATE users SET last_seen, post-update SELECT). Subsequent calls
		// within the throttle window run the 10 steady-state queries matching
		// waitingOnYouTotal and Scale S warm measurements.
		counter.Reset(true)
		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
		resp := decodeProfilePendingDecisions(t, rr)
		limitZeroCold = counter.Count()
		counter.Reset(false)
		assert.Equal(t, openProjects, resp.Counts.WaitingOnYou)
		assert.Empty(t, resp.Prompts)

		counter.Reset(true)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
		resp = decodeProfilePendingDecisions(t, rr)
		limitZeroWarm = counter.Count()
		counter.Reset(false)
		assert.Equal(t, openProjects, resp.Counts.WaitingOnYou)
		assert.Empty(t, resp.Prompts)

		counter.Reset(true)
		total, projs, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
		bellQueries = counter.Count()
		counter.Reset(false)
		require.NoError(t, err)
		assert.Equal(t, openProjects, total)
		assert.Equal(t, openProjects, projs)

		t.Logf("projects=%d history=%d open=%d -> limit=0 cold=%d warm=%d, waitingOnYouTotal=%d",
			numProjects, settledPerProject, openProjects, limitZeroCold, limitZeroWarm, bellQueries)
		return limitZeroCold, limitZeroWarm, bellQueries
	}

	t.Run("quiet_user_constant_in_projects_and_history", func(t *testing.T) {
		lzC5p10h, lzW5p10h, bell5p10h := measure(t, 5, 10, 0)
		lzC40p10h, lzW40p10h, bell40p10h := measure(t, 40, 10, 0)
		lzC5p300h, lzW5p300h, bell5p300h := measure(t, 5, 300, 0)

		assert.Equal(t, lzC5p10h, lzC40p10h, "limit=0 (cold) query count must be constant across 5 vs 40 projects")
		assert.Equal(t, lzC5p10h, lzC5p300h, "limit=0 (cold) query count must be constant across 10 vs 300 history rows/project")
		assert.Equal(t, lzW5p10h, lzW40p10h, "limit=0 (warm) query count must be constant across 5 vs 40 projects")
		assert.Equal(t, lzW5p10h, lzW5p300h, "limit=0 (warm) query count must be constant across 10 vs 300 history rows/project")
		assert.Equal(t, bell5p10h, bell40p10h, "waitingOnYouTotal query count must be constant across 5 vs 40 projects")
		assert.Equal(t, bell5p10h, bell5p300h, "waitingOnYouTotal query count must be constant across 10 vs 300 history rows/project")
		// Owner exit criterion (design-scale §1): quiet-user limit=0 probe
		// (steady-state warm) and bell waitingOnYouTotal stay <= 12 queries.
		// The very first HTTP request on a fresh server adds 3 throttled
		// users.last_seen update queries (<= 15).
		assert.LessOrEqual(t, bell40p10h, int64(12), "quiet-user waitingOnYouTotal must stay <= 12 queries (design-scale §1)")
		assert.LessOrEqual(t, lzW40p10h, int64(12), "quiet-user HTTP limit=0 (warm) must stay <= 12 queries (design-scale §1)")
		assert.LessOrEqual(t, lzC40p10h, int64(15), "quiet-user HTTP limit=0 (cold first request with throttled last_seen) must stay <= 15 queries")
	})

	t.Run("one_open_project_constant_in_total_projects_and_history", func(t *testing.T) {
		lzC5p10h, lzW5p10h, bell5p10h := measure(t, 5, 10, 1)
		lzC40p10h, lzW40p10h, bell40p10h := measure(t, 40, 10, 1)
		lzC5p300h, lzW5p300h, bell5p300h := measure(t, 5, 300, 1)

		assert.Equal(t, lzC5p10h, lzC40p10h, "limit=0 (cold) with 1 open project must be constant across 5 vs 40 projects")
		assert.Equal(t, lzC5p10h, lzC5p300h, "limit=0 (cold) with 1 open project must be constant across 10 vs 300 history rows/project")
		assert.Equal(t, lzW5p10h, lzW40p10h, "limit=0 (warm) with 1 open project must be constant across 5 vs 40 projects")
		assert.Equal(t, lzW5p10h, lzW5p300h, "limit=0 (warm) with 1 open project must be constant across 10 vs 300 history rows/project")
		assert.Equal(t, bell5p10h, bell40p10h, "waitingOnYouTotal with 1 open project must be constant across 5 vs 40 projects")
		assert.Equal(t, bell5p10h, bell5p300h, "waitingOnYouTotal with 1 open project must be constant across 10 vs 300 history rows/project")
	})
}

// TestDecisionsQueryCount_ProfileReadsConstantInHistory proves that profile and
// single-project Decisions reads (tab=pending, tab=made&sort=recent,
// tab=made&sort=project, and GET /projects/{id}/decisions) issue a constant
// number of SQL queries as history depth grows (10 vs 300 rows/project)
// (design-scale §7 item 4).
func TestDecisionsQueryCount_ProfileReadsConstantInHistory(t *testing.T) {
	measure := func(t *testing.T, settledPerProject int) (pendingQ, madeRecentQ, madeProjectQ, singleProjQ int64) {
		t.Helper()
		f, cs, counter := newCountingDecisionsFixture(t)
		seedSettledAndOpenHistoryForQueryCount(t, f, cs, 5, settledPerProject, 1)

		counter.Reset(true)
		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		pendingQ = counter.Count()
		counter.Reset(false)

		counter.Reset(true)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=recent", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		madeRecentQ = counter.Count()
		counter.Reset(false)

		counter.Reset(true)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=project", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		madeProjectQ = counter.Count()
		counter.Reset(false)

		counter.Reset(true)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/decisions?tab=pending", f.project.ID), nil)
		require.Equal(t, http.StatusOK, rr.Code)
		singleProjQ = counter.Count()
		counter.Reset(false)

		t.Logf("history=%d/proj -> pending=%d, made-recent=%d, made-project=%d, single-project=%d queries",
			settledPerProject, pendingQ, madeRecentQ, madeProjectQ, singleProjQ)
		return pendingQ, madeRecentQ, madeProjectQ, singleProjQ
	}

	p10, mr10, mp10, sp10 := measure(t, 10)
	p300, mr300, mp300, sp300 := measure(t, 300)

	assert.Equal(t, p10, p300, "tab=pending query count must be constant in history depth")
	assert.Equal(t, mr10, mr300, "tab=made&sort=recent query count must be constant in history depth")
	assert.Equal(t, mp10, mp300, "tab=made&sort=project query count must be constant in history depth")
	assert.Equal(t, sp10, sp300, "single-project decisions query count must be constant in history depth")
}

// TestSelectProfileDecisionProjects_UnsettledOnlyRespectsEffectiveCap pins that
// selectProfileDecisionProjects(..., unsettledOnly=true) compares candidate
// count against profileDecisionProjectLimit() (the effective cap) rather than
// the package constant maxProfileDecisionProjects, so lowered caps preserve
// truncated=true whether or not any project has unsettled decisions, and both
// the limit=0 HTTP probe and waitingOnYouSummary/waitingOnYouTotal agree.
func TestSelectProfileDecisionProjects_UnsettledOnlyRespectsEffectiveCap(t *testing.T) {
	cases := []struct {
		name         string
		openProjects int
	}{
		{name: "zero_unsettled", openProjects: 0},
		{name: "partial_unsettled", openProjects: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, cs, _ := newCountingDecisionsFixture(t)
			seedSettledAndOpenHistoryForQueryCount(t, f, cs, 3, 2, tc.openProjects)
			ctx := context.Background()
			viewer := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))

			// Effective cap (2) < candidate count (3): must fall back to the
			// exact select-then-filter path and report truncated=true.
			f.srv.maxProfileDecisionProjectsForTest = 2

			selected, truncated, err := f.srv.selectProfileDecisionProjects(ctx, viewer, nil, "", true)
			require.NoError(t, err)
			assert.Len(t, selected, tc.openProjects)
			assert.True(t, truncated, "truncated must be true when readable candidates exceed effective cap")

			rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
			resp := decodeProfilePendingDecisions(t, rr)
			assert.True(t, resp.TruncatedProjects, "limit=0 probe must report truncatedProjects=true")
			assert.Equal(t, tc.openProjects, resp.Counts.WaitingOnYou)
			assert.Empty(t, resp.Prompts)

			summary, err := f.srv.waitingOnYouSummary(withAuthzInputMemo(ctx), viewer, nil, 0)
			require.NoError(t, err)
			assert.True(t, summary.truncatedProjects, "waitingOnYouSummary must report truncatedProjects=true")
			assert.Equal(t, tc.openProjects, summary.total)
			assert.Equal(t, tc.openProjects, summary.projects)

			wTotal, wProjects, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.openProjects, wTotal)
			assert.Equal(t, tc.openProjects, wProjects)

			// Effective cap (3) == candidate count (3): pre-filter path is safe
			// and truncated must be false.
			f.srv.maxProfileDecisionProjectsForTest = 3

			selected, truncated, err = f.srv.selectProfileDecisionProjects(ctx, viewer, nil, "", true)
			require.NoError(t, err)
			assert.Len(t, selected, tc.openProjects)
			assert.False(t, truncated, "truncated must be false when candidates do not exceed effective cap")

			rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
			resp = decodeProfilePendingDecisions(t, rr)
			assert.False(t, resp.TruncatedProjects, "limit=0 probe must report truncatedProjects=false")
			assert.Equal(t, tc.openProjects, resp.Counts.WaitingOnYou)

			summary, err = f.srv.waitingOnYouSummary(withAuthzInputMemo(ctx), viewer, nil, 0)
			require.NoError(t, err)
			assert.False(t, summary.truncatedProjects, "waitingOnYouSummary must report truncatedProjects=false")
			assert.Equal(t, tc.openProjects, summary.total)
			assert.Equal(t, tc.openProjects, summary.projects)

			wTotal, wProjects, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.openProjects, wTotal)
			assert.Equal(t, tc.openProjects, wProjects)
		})
	}
}

// seedDecidedAndOpenHistoryForProfileQueryCount seeds numProjects projects with
// historyPerProject settled decided+answer rows each plus openProjects open
// prompts, so profile tab=pending, tab=made&sort=recent, and
// tab=made&sort=project all return non-empty results.
func seedDecidedAndOpenHistoryForProfileQueryCount(
	t *testing.T,
	f *decisionsFixture,
	cs *entadapter.CompositeStore,
	numProjects int,
	historyPerProject int,
	openProjects int,
) {
	t.Helper()
	ctx := context.Background()
	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)

	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	choicesJSON := `[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]`

	type projInfo struct {
		projID string
		tpID   string
		botID  string
		idx    int
	}
	projs := make([]projInfo, numProjects)
	for p := 0; p < numProjects; p++ {
		proj := &store.Project{
			ID:      api.NewUUID(),
			Name:    fmt.Sprintf("p3-qc-proj-%03d", p),
			Slug:    fmt.Sprintf("p3-qc-proj-%03d", p),
			Created: base,
		}
		require.NoError(t, f.store.CreateProject(ctx, proj))
		_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRole.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      f.bob.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          proj.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
		tpID := createTestDecisionTopic(t, f.wcs, proj.ID, "general", f.bob.ID)
		botID := api.NewUUID()
		projs[p] = projInfo{projID: proj.ID, tpID: tpID, botID: botID, idx: p}
	}

	tx, err := cs.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	stmtDec, err := tx.PrepareContext(ctx,
		`INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, created_at, decided_at)
		 VALUES (?, ?, ?, ?, 0, ?, 'bot', ?, ?, 'agent', 1, 'History?', ?, false, 0, 'decided', true, ?, ?)`)
	require.NoError(t, err)
	stmtAns, err := tx.PrepareContext(ctx,
		`INSERT INTO decision_answers (id, decision_row_id, project_id, asker_agent_id, key, version, question_text, choice_id, choice_label, answered_by_user_id, answered_by_name, answered_by_email, provenance, outcome, surface, response_message_id, matched_default, created_at)
		 VALUES (?, ?, ?, ?, ?, 1, 'History?', 'yes', 'Yes', ?, 'Bob', 'bob@test.com', 'direct', 'answer', 'decisions_page', ?, false, ?)`)
	require.NoError(t, err)

	for _, pr := range projs {
		for j := 0; j < historyPerProject; j++ {
			decID := uuid.NewString()
			msgID := uuid.NewString()
			ansID := uuid.NewString()
			respMsgID := uuid.NewString()
			ts := base.Add(time.Duration(pr.idx*10000+j) * time.Second)
			key := fmt.Sprintf("h-%03d-%05d", pr.idx, j)
			_, err := stmtDec.ExecContext(ctx, decID, pr.projID, pr.tpID, msgID, pr.botID, key, key, choicesJSON, ts, ts)
			require.NoError(t, err)
			_, err = stmtAns.ExecContext(ctx, ansID, decID, pr.projID, pr.botID, key, f.bob.ID, respMsgID, ts)
			require.NoError(t, err)
		}
	}
	require.NoError(t, stmtDec.Close())
	require.NoError(t, stmtAns.Close())
	require.NoError(t, tx.Commit())

	for p := 0; p < openProjects && p < numProjects; p++ {
		pr := projs[p]
		_, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: pr.projID, Sender: "agent:bot", SenderID: pr.botID,
				Recipient: "chat", ThreadID: pr.tpID, Channel: "web",
				CreatedAt: base.Add(24 * time.Hour),
			},
			AskerAgentID:   pr.botID,
			AskerAgentName: "bot",
			Decisions: []messages.Decision{
				{Key: fmt.Sprintf("open-%03d", p), Question: "Open?", Choices: choices},
			},
		})
		require.NoError(t, err)
	}
}

// TestDecisionsQueryCount_ProfilePendingAndMade proves that profile
// GET /api/v1/users/me/decisions for tab=pending, tab=made&sort=recent, and
// tab=made&sort=project issue a constant number of SQL queries with respect to
// project count (5 vs 40) and history depth (10 vs 300) (design-scale §7 item 4).
func TestDecisionsQueryCount_ProfilePendingAndMade(t *testing.T) {
	type counts struct {
		pending     int64
		madeRecent  int64
		madeProject int64
	}
	measure := func(t *testing.T, numProjects, historyPerProject int) counts {
		t.Helper()
		f, cs, counter := newCountingDecisionsFixture(t)
		seedDecidedAndOpenHistoryForProfileQueryCount(t, f, cs, numProjects, historyPerProject, numProjects)

		var c counts
		counter.Reset(true)
		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=20", nil)
		pResp := decodeProfilePendingDecisions(t, rr)
		c.pending = counter.Count()
		counter.Reset(false)
		assert.Equal(t, numProjects, pResp.Counts.WaitingOnYou)
		assert.NotEmpty(t, pResp.Prompts)

		counter.Reset(true)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=recent&limit=20", nil)
		mrResp := decodeProfileMadeDecisions(t, rr)
		c.madeRecent = counter.Count()
		counter.Reset(false)
		assert.Len(t, mrResp.Entries, 20)

		counter.Reset(true)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=project", nil)
		mpResp := decodeProfileMadeByProjectDecisions(t, rr)
		c.madeProject = counter.Count()
		counter.Reset(false)
		assert.Len(t, mpResp.Groups, numProjects)

		t.Logf("projects=%d history=%d -> pending=%d, made/recent=%d, made/project=%d",
			numProjects, historyPerProject, c.pending, c.madeRecent, c.madeProject)
		return c
	}

	c5p10h := measure(t, 5, 10)
	c40p10h := measure(t, 40, 10)
	c5p300h := measure(t, 5, 300)

	assert.Equal(t, c5p10h.pending, c40p10h.pending, "tab=pending query count must be constant across 5 vs 40 projects")
	assert.Equal(t, c5p10h.pending, c5p300h.pending, "tab=pending query count must be constant across 10 vs 300 history rows/project")
	assert.Equal(t, c5p10h.madeRecent, c40p10h.madeRecent, "tab=made&sort=recent query count must be constant across 5 vs 40 projects")
	assert.Equal(t, c5p10h.madeRecent, c5p300h.madeRecent, "tab=made&sort=recent query count must be constant across 10 vs 300 history rows/project")
	assert.Equal(t, c5p10h.madeProject, c40p10h.madeProject, "tab=made&sort=project query count must be constant across 5 vs 40 projects")
	assert.Equal(t, c5p10h.madeProject, c5p300h.madeProject, "tab=made&sort=project query count must be constant across 10 vs 300 history rows/project")
}

// TestDecisionsP3_ScopeDifferential verifies that batched
// resolveProfileDecisionScopes produces identical scopes to legacy per-project
// scope resolution (ListTopicsIncludingDeleted + ListProjectDecisionAgents +
// GetAgentsByIDsIncludingDeleted) across active/soft-deleted topics,
// live/soft-deleted/hard-deleted agents, and unreadable-DM-heavy projects.
func TestDecisionsP3_ScopeDifferential(t *testing.T) {
	f, cs, _ := newCountingDecisionsFixture(t)
	ctx := context.Background()
	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)

	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	var projects []*store.Project
	for i := 0; i < 3; i++ {
		p := &store.Project{
			ID:      api.NewUUID(),
			Name:    fmt.Sprintf("diff-scope-%d", i),
			Slug:    fmt.Sprintf("diff-scope-%d", i),
			Created: base,
		}
		require.NoError(t, f.store.CreateProject(ctx, p))
		_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.bob.ID,
			ScopeType: store.RoleScopeProject, ScopeID: p.ID, CreatedBy: "test",
		})
		require.NoError(t, err)
		projects = append(projects, p)
	}

	// Project 0: active topic + soft-deleted topic + live agent + soft-deleted agent + hard-deleted agent.
	tp0Active := createTestDecisionTopic(t, f.wcs, projects[0].ID, "general", f.bob.ID)
	tp0Deleted := createTestDecisionTopic(t, f.wcs, projects[0].ID, "archived", f.bob.ID)
	require.NoError(t, f.wcs.DeleteTopic(ctx, tp0Deleted))

	agLive := &store.Agent{ID: api.NewUUID(), Slug: "ag-live", Name: "Live Agent", ProjectID: projects[0].ID, Phase: "running"}
	require.NoError(t, f.store.CreateAgent(ctx, agLive))
	agSoft := &store.Agent{ID: api.NewUUID(), Slug: "ag-soft", Name: "Soft Agent", ProjectID: projects[0].ID, Phase: "stopped"}
	require.NoError(t, f.store.CreateAgent(ctx, agSoft))
	_, err = cs.DB().ExecContext(ctx, "UPDATE agents SET deleted_at = ? WHERE id = ?", time.Now().UTC(), agSoft.ID)
	require.NoError(t, err)
	agHardID := api.NewUUID()

	for idx, ag := range []struct {
		id, name, ck string
	}{
		{agLive.ID, "Old Live Name", tp0Active},
		{agSoft.ID, "Old Soft Name", tp0Deleted},
		{agHardID, "Hard Deleted Snapshot", "dm:agent:" + agHardID + ":user:" + f.alice.ID},
	} {
		_, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: projects[0].ID, Sender: "agent:" + ag.name, SenderID: ag.id,
				Recipient: "chat", ThreadID: ag.ck, Channel: "web", CreatedAt: base.Add(time.Duration(idx) * time.Minute),
			},
			AskerAgentID: ag.id, AskerAgentName: ag.name,
			Decisions: []messages.Decision{{Key: fmt.Sprintf("k-%d", idx), Question: "Q?", Choices: choices}},
		})
		require.NoError(t, err)
	}

	// Project 1: DM-heavy with Alice's unreadable DMs + Bob's own DM.
	ag1ID := api.NewUUID()
	dmAlice := "dm:agent:" + ag1ID + ":user:" + f.alice.ID
	dmBob := "dm:agent:" + ag1ID + ":user:" + f.bob.ID
	for idx, ck := range []string{dmAlice, dmBob} {
		_, err := cs.DB().ExecContext(ctx,
			`INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, created_at)
			 VALUES (?, ?, ?, ?, 0, ?, 'dm-bot', ?, ?, 'agent', 1, 'DM?', '[{"id":"yes","label":"Yes"}]', false, 0, 'pending', false, ?)`,
			uuid.NewString(), projects[1].ID, ck, uuid.NewString(), ag1ID, fmt.Sprintf("dm-%d", idx), fmt.Sprintf("dm-%d", idx), base)
		require.NoError(t, err)
	}

	// Project 2: topic only, no decisions.
	createTestDecisionTopic(t, f.wcs, projects[2].ID, "general", f.bob.ID)

	viewer := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))
	memoCtx := withAuthzInputMemo(ctx)
	batched, err := f.srv.resolveProfileDecisionScopes(memoCtx, projects, viewer, nil, f.wcs)
	require.NoError(t, err)
	require.Len(t, batched, len(projects))

	for i, p := range projects {
		// Legacy per-project resolution via single-project store methods.
		acc := f.srv.chatUserAccess(memoCtx, viewer, p, []string{ChatActionRead}, chatThreadWriteActions, nil, false, false)
		var topics []WebChatTopic
		if acc.Can(ChatActionRead) {
			topics, err = f.wcs.ListTopicsIncludingDeleted(memoCtx, p.ID)
			require.NoError(t, err)
		}
		decAgents, err := f.store.ListProjectDecisionAgents(memoCtx, p.ID)
		require.NoError(t, err)
		var ids []string
		for _, da := range decAgents {
			ids = append(ids, da.ID)
		}
		var agMap map[string]*store.Agent
		if len(ids) > 0 {
			agMap, err = f.store.GetAgentsByIDsIncludingDeleted(memoCtx, ids)
			require.NoError(t, err)
		}
		want := f.srv.buildProjectReadableScope(memoCtx, p, viewer.ID(), nil, nil, acc, topics, decAgents, agMap)
		got := batched[i]

		assert.Equal(t, want.chatKeys, got.chatKeys, "project %d chatKeys", i)
		assert.Equal(t, want.conversationLabel, got.conversationLabel, "project %d labels", i)
		assert.Equal(t, want.conversationAvailable, got.conversationAvailable, "project %d available", i)
		assert.Equal(t, want.userCanPost, got.userCanPost, "project %d userCanPost", i)
		assert.Equal(t, want.agentNames, got.agentNames, "project %d agentNames", i)
		assert.Equal(t, want.queriedAgentIDs, got.queriedAgentIDs, "project %d queriedAgentIDs", i)
		assert.Len(t, got.liveAgents, len(want.liveAgents), "project %d liveAgents", i)
		assert.Len(t, got.agentsByID, len(want.agentsByID), "project %d agentsByID", i)
		// Authorization invariant: Alice's DM key must never appear in Bob's scope.
		assert.NotContains(t, got.chatKeys, dmAlice)
	}
}

type spyTopicsWebChatStore struct {
	WebChatStore
	requestedProjectIDs []string
}

func (s *spyTopicsWebChatStore) ListTopicsIncludingDeletedForProjects(ctx context.Context, projectIDs []string) (map[string][]WebChatTopic, error) {
	s.requestedProjectIDs = append([]string(nil), projectIDs...)
	return s.WebChatStore.ListTopicsIncludingDeletedForProjects(ctx, projectIDs)
}

// TestDecisionsP3_ChatReadGates pins both chat-read authorization gates:
//  1. resolveProfileDecisionScopes fetches topics only for projects where
//     ChatActionRead is granted (never querying or exposing topics for projects
//     without chat read).
//  2. buildProjectReadableScope excludes all topics when projectChatAccess does
//     not grant ChatActionRead, even if topics are passed in.
func TestDecisionsP3_ChatReadGates(t *testing.T) {
	f, _, _ := newCountingDecisionsFixture(t)
	ctx := context.Background()

	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)

	// Project A: Bob is a project-member (ChatActionRead granted).
	pReadable := &store.Project{ID: api.NewUUID(), Name: "Read Gate Allowed", Slug: "read-gate-allowed"}
	require.NoError(t, f.store.CreateProject(ctx, pReadable))
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.bob.ID,
		ScopeType: store.RoleScopeProject, ScopeID: pReadable.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	tpReadable := createTestDecisionTopic(t, f.wcs, pReadable.ID, "allowed-topic", f.bob.ID)

	// Project B: Bob is NOT a member (ChatActionRead denied).
	pDenied := &store.Project{ID: api.NewUUID(), Name: "Read Gate Denied", Slug: "read-gate-denied"}
	require.NoError(t, f.store.CreateProject(ctx, pDenied))
	tpDenied := createTestDecisionTopic(t, f.wcs, pDenied.ID, "secret-topic", f.alice.ID)

	viewer := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))
	memoCtx := withAuthzInputMemo(ctx)

	// Gate 1: resolveProfileDecisionScopes must pass ONLY pReadable to
	// ListTopicsIncludingDeletedForProjects and never include tpDenied.
	spy := &spyTopicsWebChatStore{WebChatStore: f.wcs}
	scopes, err := f.srv.resolveProfileDecisionScopes(memoCtx, []*store.Project{pReadable, pDenied}, viewer, nil, spy)
	require.NoError(t, err)
	require.Len(t, scopes, 2)
	assert.Equal(t, []string{pReadable.ID}, spy.requestedProjectIDs,
		"resolveProfileDecisionScopes must fetch topics only for chat-readable projects")
	assert.Contains(t, scopes[0].chatKeys, tpReadable)
	assert.NotContains(t, scopes[1].chatKeys, tpDenied)
	assert.Empty(t, scopes[1].conversationLabel)

	// Gate 2: buildProjectReadableScope must ignore passed-in topics when
	// projectChatAccess does not grant ChatActionRead.
	deniedTopics := []WebChatTopic{{ID: tpDenied, ProjectID: pDenied.ID, Name: "secret-topic"}}
	deniedScope := f.srv.buildProjectReadableScope(memoCtx, pDenied, viewer.ID(), nil, nil, ChatAccess{}, deniedTopics, nil, nil)
	assert.NotContains(t, deniedScope.chatKeys, tpDenied,
		"buildProjectReadableScope must gate topics on projectChatAccess.Can(ChatActionRead)")
	assert.Empty(t, deniedScope.conversationLabel)

	// Positive control: buildProjectReadableScope includes topics when ChatActionRead is granted.
	allowedAccess := f.srv.chatUserAccess(memoCtx, viewer, pReadable, []string{ChatActionRead}, chatThreadWriteActions, nil, false, false)
	require.True(t, allowedAccess.Can(ChatActionRead))
	allowedTopics := []WebChatTopic{{ID: tpReadable, ProjectID: pReadable.ID, Name: "allowed-topic"}}
	allowedScope := f.srv.buildProjectReadableScope(memoCtx, pReadable, viewer.ID(), nil, nil, allowedAccess, allowedTopics, nil, nil)
	assert.Contains(t, allowedScope.chatKeys, tpReadable)
	assert.Equal(t, "#allowed-topic", allowedScope.conversationLabel[tpReadable])
}

// TestDecisionsScale is the opt-in hub-level scale test (design-scale §7),
// skipped unless SCION_DECISIONS_SCALE=1.
func TestDecisionsScale(t *testing.T) {
	if os.Getenv("SCION_DECISIONS_SCALE") != "1" {
		t.Skip("set SCION_DECISIONS_SCALE=1 to run hub-level Decisions scale timings")
	}

	scenarios := []struct {
		name           string
		numProjects    int
		rowsPerProject int
		openProjects   int
	}{
		{name: "R_20p_10k_quiet", numProjects: 20, rowsPerProject: 500, openProjects: 0},
		{name: "R_20p_10k_1open", numProjects: 20, rowsPerProject: 500, openProjects: 1},
		{name: "S_200p_200k_quiet", numProjects: 200, rowsPerProject: 1000, openProjects: 0},
		{name: "S_200p_200k_1open", numProjects: 200, rowsPerProject: 1000, openProjects: 1},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f, cs, counter := newCountingDecisionsFixtureForScale(t)
			ctx := context.Background()
			d := cs.Dialect()
			memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
			require.NoError(t, err)

			base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
			choicesJSON := `[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]`
			detUUID := func(tag byte, p, j int) string {
				var u uuid.UUID
				u[0] = tag
				binary.BigEndian.PutUint32(u[4:8], uint32(p+1))
				binary.BigEndian.PutUint32(u[8:12], uint32(j+1))
				u[6] = (u[6] & 0x0f) | 0x40
				u[8] = (u[8] & 0x3f) | 0x80
				return u.String()
			}

			seedStart := time.Now()
			type projSeed struct {
				projID string
				tpID   string
				botID  string
				pIdx   int
			}
			projSeeds := make([]projSeed, sc.numProjects)
			var openSeeds []projSeed

			// Create projects, role bindings, and topics BEFORE opening the bulk
			// SQL transaction so SQLite's MaxOpenConns(1) pool never deadlocks.
			for p := 0; p < sc.numProjects; p++ {
				projID := detUUID(0x01, p, 0)
				require.NoError(t, f.store.CreateProject(ctx, &store.Project{
					ID: projID, Name: fmt.Sprintf("scale-%s-p-%03d", sc.name, p), Slug: fmt.Sprintf("sc-%s-p-%03d", sc.name, p), Created: base,
				}))
				_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.bob.ID,
					ScopeType: store.RoleScopeProject, ScopeID: projID, CreatedBy: "test",
				})
				require.NoError(t, err)
				tpID := createTestDecisionTopic(t, f.wcs, projID, "general", f.bob.ID)
				botID := detUUID(0x10, p, 0)
				ps := projSeed{projID: projID, tpID: tpID, botID: botID, pIdx: p}
				projSeeds[p] = ps
				if p < sc.openProjects {
					openSeeds = append(openSeeds, ps)
				}
			}

			tx, err := cs.DB().BeginTx(ctx, nil)
			require.NoError(t, err)
			stmtDec, err := tx.PrepareContext(ctx, rebindScaleSQL(d,
				`INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, facet_indexed, created_at, decided_at, withdrawn_at, dismissed_at)
				 VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, 'agent', 1, ?, ?, false, 0, ?, ?, true, ?, ?, ?, ?)`))
			require.NoError(t, err)
			stmtAns, err := tx.PrepareContext(ctx, rebindScaleSQL(d,
				`INSERT INTO decision_answers (id, decision_row_id, project_id, asker_agent_id, key, version, question_text, choice_id, choice_label, answered_by_user_id, answered_by_name, answered_by_email, provenance, outcome, surface, response_message_id, matched_default, facet_indexed, created_at)
				 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, 'Bob', 'bob@test.com', 'direct', ?, 'decisions_page', ?, false, true, ?)`))
			require.NoError(t, err)
			stmtFacet, err := tx.PrepareContext(ctx, rebindScaleSQL(d,
				`INSERT INTO decision_facet_entries (id, project_id, chat_key, kind, actor_id, name, email, latest_at, ref_count)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`))
			require.NoError(t, err)

			for p, ps := range projSeeds {
				projID, tpID, botID := ps.projID, ps.tpID, ps.botID
				var latestDecTS, latestAnsTS time.Time
				ansCount := 0

				for j := 0; j < sc.rowsPerProject; j++ {
					decID := detUUID(0x20, p, j)
					msgID := detUUID(0x40, p, j)
					key := fmt.Sprintf("k-%03d-%06d", p, j)
					ts := base.Add(time.Duration(p*100000+j) * time.Second)
					latestDecTS = ts
					qText := fmt.Sprintf("Deploy %d?", j)
					switch {
					case j%20 == 1:
						_, err = stmtDec.ExecContext(ctx, decID, projID, tpID, msgID, botID, "bot", key, key, qText, choicesJSON, "withdrawn", true, ts, nil, ts, nil)
						require.NoError(t, err)
					case j%20 == 2:
						_, err = stmtDec.ExecContext(ctx, decID, projID, tpID, msgID, botID, "bot", key, key, qText, choicesJSON, "dismissed", true, ts, nil, nil, ts)
						require.NoError(t, err)
						_, err = stmtAns.ExecContext(ctx, detUUID(0x30, p, j), decID, projID, botID, key, qText, "$dismissed", "Dismissed", f.bob.ID, "dismiss", detUUID(0x50, p, j), ts)
						require.NoError(t, err)
						latestAnsTS = ts
						ansCount++
					default:
						_, err = stmtDec.ExecContext(ctx, decID, projID, tpID, msgID, botID, "bot", key, key, qText, choicesJSON, "decided", true, ts, ts, nil, nil)
						require.NoError(t, err)
						_, err = stmtAns.ExecContext(ctx, detUUID(0x30, p, j), decID, projID, botID, key, qText, "yes", "Yes", f.bob.ID, "answer", detUUID(0x50, p, j), ts)
						require.NoError(t, err)
						latestAnsTS = ts
						ansCount++
					}
				}
				if sc.rowsPerProject > 0 {
					_, err = stmtFacet.ExecContext(ctx, detUUID(0x80, p, 0), projID, tpID, "asker", botID, "bot", "", latestDecTS, sc.rowsPerProject)
					require.NoError(t, err)
				}
				if ansCount > 0 {
					_, err = stmtFacet.ExecContext(ctx, detUUID(0x81, p, 0), projID, tpID, "answerer", f.bob.ID, "Bob", "bob@test.com", latestAnsTS, ansCount)
					require.NoError(t, err)
				}
			}
			require.NoError(t, stmtDec.Close())
			require.NoError(t, stmtAns.Close())
			require.NoError(t, stmtFacet.Close())
			require.NoError(t, tx.Commit())

			choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
			for _, osd := range openSeeds {
				_, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
					Message: &store.Message{
						ID: api.NewUUID(), ProjectID: osd.projID, Sender: "agent:bot", SenderID: osd.botID,
						Recipient: "chat", ThreadID: osd.tpID, Channel: "web", CreatedAt: base.Add(24 * time.Hour),
					},
					AskerAgentID: osd.botID, AskerAgentName: "bot",
					Decisions: []messages.Decision{{Key: fmt.Sprintf("open-%03d", osd.pIdx), Question: "Ship?", Choices: choices}},
				})
				require.NoError(t, err)
			}
			analyzeSQL := "ANALYZE"
			if d == dialect.Postgres {
				analyzeSQL = "ANALYZE decisions, decision_answers, decision_facet_entries"
			}
			_, err = cs.DB().ExecContext(ctx, analyzeSQL)
			require.NoError(t, err)
			t.Logf("[%s/%s] seeded %d projects x %d rows (+ %d open) in %v", sc.name, d, sc.numProjects, sc.rowsPerProject, sc.openProjects, time.Since(seedStart))

			const warmRuns = 10
			var qLimitZero, qBell, qPending, qMadeRecent, qMadeProject, qSingleProj int64

			coldLZ, medLZ, tailMedLZ, tailMaxLZ := measureHubColdAndWarmStats(t, warmRuns, func() {
				counter.Reset(true)
				rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
				qLimitZero = counter.Count()
				counter.Reset(false)
				resp := decodeProfilePendingDecisions(t, rr)
				require.Equal(t, sc.openProjects, resp.Counts.WaitingOnYou)
			})

			coldBell, medBell, tailMedBell, tailMaxBell := measureHubColdAndWarmStats(t, warmRuns, func() {
				counter.Reset(true)
				wTot, wProjs, berr := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
				qBell = counter.Count()
				counter.Reset(false)
				require.NoError(t, berr)
				require.Equal(t, sc.openProjects, wTot)
				require.Equal(t, sc.openProjects, wProjs)
			})

			coldPend, medPend := measureHubColdAndWarmMedian(t, 3, func() {
				counter.Reset(true)
				rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending", nil)
				qPending = counter.Count()
				counter.Reset(false)
				require.Equal(t, http.StatusOK, rr.Code)
			})

			coldMR, medMR := measureHubColdAndWarmMedian(t, 3, func() {
				counter.Reset(true)
				rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=recent", nil)
				qMadeRecent = counter.Count()
				counter.Reset(false)
				require.Equal(t, http.StatusOK, rr.Code)
			})

			coldMP, medMP := measureHubColdAndWarmMedian(t, 3, func() {
				counter.Reset(true)
				rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=project", nil)
				qMadeProject = counter.Count()
				counter.Reset(false)
				require.Equal(t, http.StatusOK, rr.Code)
			})

			coldSP, medSP := measureHubColdAndWarmMedian(t, 3, func() {
				counter.Reset(true)
				rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/decisions?tab=pending", projSeeds[0].projID), nil)
				qSingleProj = counter.Count()
				counter.Reset(false)
				require.Equal(t, http.StatusOK, rr.Code)
			})

			t.Logf("[%s/%s] HUB TIMINGS (cold / warm-med / calls7-10 med / calls7-10 max):", sc.name, d)
			t.Logf("  limit=0 (HTTP)               = %v / %v / %v / %v (%d queries)", coldLZ, medLZ, tailMedLZ, tailMaxLZ, qLimitZero)
			t.Logf("  waitingOnYouTotal            = %v / %v / %v / %v (%d queries)", coldBell, medBell, tailMedBell, tailMaxBell, qBell)
			t.Logf("  tab=pending                  = %v / %v (%d queries)", coldPend, medPend, qPending)
			t.Logf("  tab=made&sort=recent         = %v / %v (%d queries)", coldMR, medMR, qMadeRecent)
			t.Logf("  tab=made&sort=project        = %v / %v (%d queries)", coldMP, medMP, qMadeProject)
			t.Logf("  single-project (tab=pending) = %v / %v (%d queries)", coldSP, medSP, qSingleProj)
		})
	}
}

// TestDecisionsScale_BellTopicEventEndToEnd measures design-scale §1 row:
// "One topic event end-to-end (expansion + 500 recomputes, 2 workers)" at scale S
// (200 projects / 200k decisions, 500 members on the event's project).
// Skipped unless SCION_DECISIONS_SCALE=1.
func TestDecisionsScale_BellTopicEventEndToEnd(t *testing.T) {
	if os.Getenv("SCION_DECISIONS_SCALE") != "1" {
		t.Skip("set SCION_DECISIONS_SCALE=1 to run end-to-end bell topic event scale test")
	}
	f, cs, counter := newCountingDecisionsFixtureForScale(t)
	ctx := context.Background()
	d := cs.Dialect()
	setFastDecisionBell(t, f.srv, time.Millisecond)

	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)

	const (
		numProjects    = 200
		rowsPerProject = 1000
		numMembers     = 500
	)
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	choicesJSON := `[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]`
	detUUID := func(tag byte, a, b int) string {
		var u uuid.UUID
		u[0] = tag
		binary.BigEndian.PutUint32(u[4:8], uint32(a+1))
		binary.BigEndian.PutUint32(u[8:12], uint32(b+1))
		u[6] = (u[6] & 0x0f) | 0x40
		u[8] = (u[8] & 0x3f) | 0x80
		return u.String()
	}

	seedStart := time.Now()
	// 1. Create 200 projects + 1 topic each.
	projIDs := make([]string, numProjects)
	topicIDs := make([]string, numProjects)
	for p := 0; p < numProjects; p++ {
		pid := detUUID(0x01, p, 0)
		projIDs[p] = pid
		require.NoError(t, f.store.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("e2e-p-%03d", p), Slug: fmt.Sprintf("e2e-p-%03d", p), Created: base,
		}))
		topicIDs[p] = createTestDecisionTopic(t, f.wcs, pid, "general", f.bob.ID)
	}

	// 2. Create 500 active member users and bind them to project 0 (and a
	// rolling window of 20 other projects each so every user has realistic
	// multi-project membership).
	memberIDs := make([]string, numMembers)
	for m := 0; m < numMembers; m++ {
		uid := detUUID(0x70, m, 0)
		memberIDs[m] = uid
		require.NoError(t, f.store.CreateUser(ctx, &store.User{
			ID: uid, Email: fmt.Sprintf("member-%03d@test.com", m), DisplayName: fmt.Sprintf("Member %03d", m),
			Role: store.UserRoleMember, Status: "active", Created: base,
		}))
		_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid,
			ScopeType: store.RoleScopeProject, ScopeID: projIDs[0], CreatedBy: "test",
		})
		require.NoError(t, err)
	}

	// 3. Seed 200k settled & facet-indexed decisions across the 200 projects in one transaction.
	tx, err := cs.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	stmtDec, err := tx.PrepareContext(ctx, rebindScaleSQL(d,
		`INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, facet_indexed, created_at, decided_at)
		 VALUES (?, ?, ?, ?, 0, ?, 'bot', ?, ?, 'agent', 1, 'Deploy?', ?, false, 0, 'decided', true, true, ?, ?)`))
	require.NoError(t, err)
	stmtAns, err := tx.PrepareContext(ctx, rebindScaleSQL(d,
		`INSERT INTO decision_answers (id, decision_row_id, project_id, asker_agent_id, key, version, question_text, choice_id, choice_label, answered_by_user_id, answered_by_name, answered_by_email, provenance, outcome, surface, response_message_id, matched_default, facet_indexed, created_at)
		 VALUES (?, ?, ?, ?, ?, 1, 'Deploy?', 'yes', 'Yes', ?, 'Bob', 'bob@test.com', 'direct', 'answer', 'decisions_page', ?, false, true, ?)`))
	require.NoError(t, err)
	stmtFacet, err := tx.PrepareContext(ctx, rebindScaleSQL(d,
		`INSERT INTO decision_facet_entries (id, project_id, chat_key, kind, actor_id, name, email, latest_at, ref_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`))
	require.NoError(t, err)
	for p := 0; p < numProjects; p++ {
		pid, tpID, botID := projIDs[p], topicIDs[p], detUUID(0x10, p, 0)
		var lastTS time.Time
		for j := 0; j < rowsPerProject; j++ {
			decID := detUUID(0x20, p, j)
			msgID := detUUID(0x40, p, j)
			key := fmt.Sprintf("e2e-%03d-%06d", p, j)
			ts := base.Add(time.Duration(p*100000+j) * time.Second)
			lastTS = ts
			_, err = stmtDec.ExecContext(ctx, decID, pid, tpID, msgID, botID, key, key, choicesJSON, ts, ts)
			require.NoError(t, err)
			_, err = stmtAns.ExecContext(ctx, detUUID(0x30, p, j), decID, pid, botID, key, f.bob.ID, detUUID(0x50, p, j), ts)
			require.NoError(t, err)
		}
		_, err = stmtFacet.ExecContext(ctx, detUUID(0x80, p, 0), pid, tpID, "asker", botID, "bot", "", lastTS, rowsPerProject)
		require.NoError(t, err)
		_, err = stmtFacet.ExecContext(ctx, detUUID(0x81, p, 0), pid, tpID, "answerer", f.bob.ID, "Bob", "bob@test.com", lastTS, rowsPerProject)
		require.NoError(t, err)
	}
	require.NoError(t, stmtDec.Close())
	require.NoError(t, stmtAns.Close())
	require.NoError(t, stmtFacet.Close())
	require.NoError(t, tx.Commit())

	// 4. Post 1 open decision in project 0's topic, then ANALYZE.
	bot0 := detUUID(0x10, 0, 0)
	_, err = f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projIDs[0], Sender: "agent:bot", SenderID: bot0,
			Recipient: "chat", ThreadID: topicIDs[0], Channel: "web", CreatedAt: base.Add(48 * time.Hour),
		},
		AskerAgentID: bot0, AskerAgentName: "bot",
		Decisions: []messages.Decision{{Key: "e2e-open-1", Question: "Ship release?", Choices: []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}}},
	})
	require.NoError(t, err)
	analyzeSQL := "ANALYZE"
	if d == dialect.Postgres {
		analyzeSQL = "ANALYZE decisions, decision_answers, decision_facet_entries"
	}
	_, err = cs.DB().ExecContext(ctx, analyzeSQL)
	require.NoError(t, err)
	t.Logf("[E2E 200p/200k + 500 members] seeded in %v", time.Since(seedStart))

	// 5. Measure one topic event end-to-end: expansion + 500 recomputes on 2 workers.
	counter.Reset(true)
	t0 := time.Now()
	f.srv.enqueueDecisionBellTopic(projIDs[0], topicIDs[0])
	waitDecisionBellIdle(t, f.srv)
	elapsed := time.Since(t0)
	totalQueries := counter.Count()
	counter.Reset(false)

	t.Logf("[E2E 200p/200k + 500 members] topic event (expansion + 500 recomputes, 2 workers): elapsed=%v, total_queries=%d",
		elapsed, totalQueries)

	// Spot-check first and last members received their waiting-on-you bell notification.
	for _, uid := range []string{memberIDs[0], memberIDs[numMembers-1]} {
		notifs, nerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, uid, true)
		require.NoError(t, nerr)
		found := false
		for _, n := range notifs {
			if n.Kind == store.NotificationKindDecisionsWaiting && n.ResourceID == "1" {
				found = true
				break
			}
		}
		assert.True(t, found, "user %s must have decisions_waiting notification with count 1", uid)
	}
}
