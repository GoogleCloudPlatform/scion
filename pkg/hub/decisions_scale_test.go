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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

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

		// Bulk-insert settled history directly into SQL for fast fixture setup.
		for j := 0; j < settledPerProject; j++ {
			decID := uuid.New()
			msgID := uuid.New()
			ts := base.Add(time.Duration(p*10000+j) * time.Second)
			key := fmt.Sprintf("h-%03d-%05d", p, j)
			_, err := cs.DB().ExecContext(ctx,
				`INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, created_at, withdrawn_at)
				 VALUES (?, ?, ?, ?, 0, ?, 'bot', ?, ?, 'agent', 1, 'History?', '[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]', false, 0, 'withdrawn', true, ?, ?)`,
				decID.String(), proj.ID, tpID, msgID.String(), botID, key, key, ts, ts,
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
	measure := func(t *testing.T, numProjects, settledPerProject, openProjects int) (limitZeroQueries, bellQueries int) {
		t.Helper()
		f, cs, counter := newCountingDecisionsFixture(t)
		seedSettledAndOpenHistoryForQueryCount(t, f, cs, numProjects, settledPerProject, openProjects)
		ctx := context.Background()

		// Warm any one-time caches unrelated to per-project/history scaling? No —
		// measure cold-per-request, same as production requests.
		counter.Reset()
		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
		resp := decodeProfilePendingDecisions(t, rr)
		limitZeroQueries = counter.Count()
		assert.Equal(t, openProjects, resp.Counts.WaitingOnYou)
		assert.Empty(t, resp.Prompts)

		counter.Reset()
		total, projs, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
		bellQueries = counter.Count()
		require.NoError(t, err)
		assert.Equal(t, openProjects, total)
		assert.Equal(t, openProjects, projs)

		t.Logf("projects=%d history=%d open=%d -> limit=0 queries=%d, waitingOnYouTotal queries=%d",
			numProjects, settledPerProject, openProjects, limitZeroQueries, bellQueries)
		return limitZeroQueries, bellQueries
	}

	t.Run("quiet_user_constant_in_projects_and_history", func(t *testing.T) {
		lz5p10h, bell5p10h := measure(t, 5, 10, 0)
		lz40p10h, bell40p10h := measure(t, 40, 10, 0)
		lz5p300h, bell5p300h := measure(t, 5, 300, 0)

		assert.Equal(t, lz5p10h, lz40p10h, "limit=0 query count must be constant across 5 vs 40 projects")
		assert.Equal(t, lz5p10h, lz5p300h, "limit=0 query count must be constant across 10 vs 300 history rows/project")
		assert.Equal(t, bell5p10h, bell40p10h, "waitingOnYouTotal query count must be constant across 5 vs 40 projects")
		assert.Equal(t, bell5p10h, bell5p300h, "waitingOnYouTotal query count must be constant across 10 vs 300 history rows/project")
		assert.LessOrEqual(t, bell40p10h, 12, "quiet-user waitingOnYouTotal must meet design §1 <= 12 queries target")
	})

	t.Run("one_open_project_constant_in_total_projects_and_history", func(t *testing.T) {
		lz5p10h, bell5p10h := measure(t, 5, 10, 1)
		lz40p10h, bell40p10h := measure(t, 40, 10, 1)
		lz5p300h, bell5p300h := measure(t, 5, 300, 1)

		assert.Equal(t, lz5p10h, lz40p10h, "limit=0 with 1 open project must be constant across 5 vs 40 projects")
		assert.Equal(t, lz5p10h, lz5p300h, "limit=0 with 1 open project must be constant across 10 vs 300 history rows/project")
		assert.Equal(t, bell5p10h, bell40p10h, "waitingOnYouTotal with 1 open project must be constant across 5 vs 40 projects")
		assert.Equal(t, bell5p10h, bell5p300h, "waitingOnYouTotal with 1 open project must be constant across 10 vs 300 history rows/project")
	})
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
			f, cs, counter := newCountingDecisionsFixture(t)
			ctx := context.Background()
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
			tx, err := cs.DB().BeginTx(ctx, nil)
			require.NoError(t, err)
			stmtDec, err := tx.PrepareContext(ctx,
				`INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, created_at, decided_at, withdrawn_at, dismissed_at)
				 VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, 'agent', 1, ?, ?, false, 0, ?, ?, ?, ?, ?, ?)`)
			require.NoError(t, err)
			stmtAns, err := tx.PrepareContext(ctx,
				`INSERT INTO decision_answers (id, decision_row_id, project_id, asker_agent_id, key, version, question_text, choice_id, choice_label, answered_by_user_id, answered_by_name, answered_by_email, provenance, outcome, surface, response_message_id, matched_default, created_at)
				 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, 'Bob', 'bob@test.com', 'direct', ?, 'decisions_page', ?, false, ?)`)
			require.NoError(t, err)

			type openSeed struct {
				projID string
				tpID   string
				botID  string
				pIdx   int
			}
			var openSeeds []openSeed

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
				if p < sc.openProjects {
					openSeeds = append(openSeeds, openSeed{projID: projID, tpID: tpID, botID: botID, pIdx: p})
				}

				for j := 0; j < sc.rowsPerProject; j++ {
					decID := detUUID(0x20, p, j)
					msgID := detUUID(0x40, p, j)
					key := fmt.Sprintf("k-%03d-%06d", p, j)
					ts := base.Add(time.Duration(p*100000+j) * time.Second)
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
					default:
						_, err = stmtDec.ExecContext(ctx, decID, projID, tpID, msgID, botID, "bot", key, key, qText, choicesJSON, "decided", true, ts, ts, nil, nil)
						require.NoError(t, err)
						_, err = stmtAns.ExecContext(ctx, detUUID(0x30, p, j), decID, projID, botID, key, qText, "yes", "Yes", f.bob.ID, "answer", detUUID(0x50, p, j), ts)
						require.NoError(t, err)
					}
				}
			}
			require.NoError(t, stmtDec.Close())
			require.NoError(t, stmtAns.Close())
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
			_, err = cs.DB().ExecContext(ctx, "ANALYZE")
			require.NoError(t, err)
			t.Logf("[%s] seeded %d projects x %d rows (+ %d open) in %v", sc.name, sc.numProjects, sc.rowsPerProject, sc.openProjects, time.Since(seedStart))

			counter.Reset()
			t0 := time.Now()
			rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
			dLimitZero := time.Since(t0)
			qLimitZero := counter.Count()
			resp := decodeProfilePendingDecisions(t, rr)
			require.Equal(t, sc.openProjects, resp.Counts.WaitingOnYou)

			counter.Reset()
			t0 = time.Now()
			wTot, wProjs, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
			dBell := time.Since(t0)
			qBell := counter.Count()
			require.NoError(t, err)
			require.Equal(t, sc.openProjects, wTot)
			require.Equal(t, sc.openProjects, wProjs)

			t.Logf("[%s] HUB TIMINGS: limit=0=%v (%d queries), waitingOnYouTotal=%v (%d queries)",
				sc.name, dLimitZero, qLimitZero, dBell, qBell)
		})
	}
}
