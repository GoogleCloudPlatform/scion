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
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type decisionScaleSpec struct {
	name             string
	numProjects      int
	topicsPerProject int
	agentsPerProject int
	rowsPerProject   int
	openPerProject   int
}

var decisionScaleSpecs = []decisionScaleSpec{
	{
		name:             "R_20p_10k",
		numProjects:      20,
		topicsPerProject: 5,
		agentsPerProject: 5,
		rowsPerProject:   500,
		openPerProject:   1,
	},
	{
		name:             "S_200p_200k",
		numProjects:      200,
		topicsPerProject: 5,
		agentsPerProject: 5,
		rowsPerProject:   1000,
		openPerProject:   1,
	},
	{
		name:             "X_200p_1M",
		numProjects:      200,
		topicsPerProject: 5,
		agentsPerProject: 5,
		rowsPerProject:   5000,
		openPerProject:   1,
	},
}

type decisionScaleFixture struct {
	cs             *CompositeStore
	projectIDs     []string
	projectUUIDs   []uuid.UUID
	keysByProject  map[string][]string
	allKeys        []string
	totalDecisions int
	totalAnswers   int
	seedDuration   time.Duration
}

// seedDecisionScaleFixture populates the investigation data mix (design-scale §7):
// 90% decided (direct), 5% withdrawn, 5% dismissed, openPerProject open rows
// per project (mix of pending and agent_interpreted awaiting confirm), plus
// project 0 has 80% of its rows in other users' DMs (unreadable to the viewer).
// When markSettled is true, settled rows are inserted with settled=true (new
// binary steady state); when false, all rows start settled=false (pre-backfill).
func openScaleCompositeStore(tb testing.TB) *CompositeStore {
	tb.Helper()
	ctx := context.Background()
	if dsn := os.Getenv("SCION_TEST_POSTGRES_DSN"); dsn != "" {
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 4})
		require.NoError(tb, err)
		require.NoError(tb, entc.AutoMigrate(ctx, client))
		cs := NewCompositeStore(client)
		tb.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	if t, ok := tb.(*testing.T); ok {
		return newTestCompositeStore(t)
	}
	dbPath := filepath.Join(tb.TempDir(), "scale.db")
	client, err := entc.OpenSQLite(dbPath, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(tb, err)
	require.NoError(tb, entc.AutoMigrate(ctx, client))
	cs := NewCompositeStore(client)
	tb.Cleanup(func() { _ = cs.Close() })
	return cs
}

func seedDecisionScaleFixture(tb testing.TB, sc decisionScaleSpec, markSettled bool) *decisionScaleFixture {
	tb.Helper()
	cs := openScaleCompositeStore(tb)
	ctx := context.Background()

	projectIDs := make([]string, sc.numProjects)
	projectUUIDs := make([]uuid.UUID, sc.numProjects)
	keysByProject := make(map[string][]string, sc.numProjects)
	allKeys := make([]string, 0, sc.numProjects*sc.topicsPerProject)

	for p := 0; p < sc.numProjects; p++ {
		pid := uuid.NewString()
		require.NoError(tb, cs.CreateProject(ctx, &store.Project{
			ID:      pid,
			Name:    fmt.Sprintf("scale-%s-p-%03d", sc.name, p),
			Slug:    fmt.Sprintf("s-%s-p-%03d", sc.name, p),
			Created: time.Now().UTC(),
		}))
		projectIDs[p] = pid
		projectUUIDs[p] = uuid.MustParse(pid)
		topics := make([]string, sc.topicsPerProject)
		for tp := 0; tp < sc.topicsPerProject; tp++ {
			topics[tp] = uuid.NewString()
			allKeys = append(allKeys, topics[tp])
		}
		keysByProject[pid] = topics
	}

	users := make([]string, 10)
	for u := range users {
		users[u] = uuid.NewString()
	}
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	detUUID := func(tag byte, p, j int) uuid.UUID {
		var u uuid.UUID
		u[0] = tag
		binary.BigEndian.PutUint32(u[4:8], uint32(p+1))
		binary.BigEndian.PutUint32(u[8:12], uint32(j+1))
		u[6] = (u[6] & 0x0f) | 0x40
		u[8] = (u[8] & 0x3f) | 0x80
		return u
	}

	seedStart := time.Now()
	const batchSize = 500
	totalDecisions := 0
	totalAnswers := 0

	// Commit per project so Postgres transactions stay bounded at scale X (1M rows).
	for p := 0; p < sc.numProjects; p++ {
		tx, err := cs.client.Tx(ctx)
		require.NoError(tb, err)

		pUUID := projectUUIDs[p]
		pid := projectIDs[p]
		topics := keysByProject[pid]
		agentIDs := make([]uuid.UUID, sc.agentsPerProject)
		for a := 0; a < sc.agentsPerProject; a++ {
			agentIDs[a] = detUUID(0x10, p, a)
		}

		for offset := 0; offset < sc.rowsPerProject; offset += batchSize {
			end := offset + batchSize
			if end > sc.rowsPerProject {
				end = sc.rowsPerProject
			}
			n := end - offset
			decCreates := make([]*ent.DecisionCreate, 0, n)
			ansCreates := make([]*ent.DecisionAnswerCreate, 0, n)

			for j := offset; j < end; j++ {
				decID := detUUID(0x20, p, j)
				ansID := detUUID(0x30, p, j)
				msgID := detUUID(0x40, p, j)
				respMsgID := detUUID(0x50, p, j)
				aIdx := j % sc.agentsPerProject
				agentID := agentIDs[aIdx]
				agentName := fmt.Sprintf("agent-%03d-%02d", p, aIdx)
				chatKey := topics[j%sc.topicsPerProject]
				// Project 0 is DM-heavy: 80% of non-open rows belong to other
				// users' DMs (unreadable to the viewer).
				isOpen := j >= sc.rowsPerProject-sc.openPerProject
				if p == 0 && !isOpen && j%5 != 0 {
					chatKey = fmt.Sprintf("dm:agent:%s:user:%s", agentID.String(), users[1+(j%(len(users)-1))])
				}
				key := fmt.Sprintf("k-%03d-%06d", p, j)
				ts := base.Add(time.Duration(p*100000+j) * time.Second)

				isWithdrawn := !isOpen && j%20 == 1
				isDismissed := !isOpen && !isWithdrawn && j%20 == 2
				isInterpOpen := isOpen && p%4 == 3

				dc := tx.Decision.Create().
					SetID(decID).
					SetProjectID(pUUID).
					SetChatKey(chatKey).
					SetMessageID(msgID).
					SetPosition(0).
					SetAskerAgentID(agentID).
					SetAskerAgentName(agentName).
					SetKey(key).
					SetKeyNorm(key).
					SetKeySource(decision.KeySourceAgent).
					SetVersion(1).
					SetQuestion(fmt.Sprintf("Deploy service %d in project %d?", j, p)).
					SetChoices(choices).
					SetCreatedAt(ts)

				switch {
				case isInterpOpen:
					dc.SetStatus(decision.StatusDecided).SetDecidedAt(ts).SetSettled(false)
					uid := users[0]
					ansCreates = append(ansCreates, tx.DecisionAnswer.Create().
						SetID(ansID).
						SetDecisionRowID(decID).
						SetProjectID(pUUID).
						SetAskerAgentID(agentID).
						SetKey(key).
						SetVersion(1).
						SetQuestionText(fmt.Sprintf("Deploy service %d in project %d?", j, p)).
						SetChoiceID("yes").
						SetChoiceLabel("Yes").
						SetAnsweredByUserID(uid).
						SetAnsweredByName("User 0").
						SetProvenance(decisionanswer.ProvenanceAgentInterpreted).
						SetOutcome(decisionanswer.OutcomeAnswer).
						SetInterpretedByAgentID(agentID).
						SetSourceMessageID(detUUID(0x60, p, j)).
						SetSurface(decisionanswer.SurfaceNative).
						SetResponseMessageID(respMsgID).
						SetCreatedAt(ts))
				case isOpen:
					dc.SetStatus(decision.StatusPending).SetSettled(false)
				case isWithdrawn:
					dc.SetStatus(decision.StatusWithdrawn).SetWithdrawnAt(ts).SetSettled(markSettled)
				case isDismissed:
					dc.SetStatus(decision.StatusDismissed).SetDismissedAt(ts).SetSettled(markSettled)
					uid := users[j%len(users)]
					ansCreates = append(ansCreates, tx.DecisionAnswer.Create().
						SetID(ansID).
						SetDecisionRowID(decID).
						SetProjectID(pUUID).
						SetAskerAgentID(agentID).
						SetKey(key).
						SetVersion(1).
						SetQuestionText(fmt.Sprintf("Deploy service %d in project %d?", j, p)).
						SetChoiceID("$dismissed").
						SetChoiceLabel("Dismissed").
						SetAnsweredByUserID(uid).
						SetAnsweredByName(fmt.Sprintf("User %d", j%len(users))).
						SetProvenance(decisionanswer.ProvenanceDirect).
						SetOutcome(decisionanswer.OutcomeDismiss).
						SetSurface(decisionanswer.SurfaceDecisionsPage).
						SetResponseMessageID(respMsgID).
						SetCreatedAt(ts))
				default:
					dc.SetStatus(decision.StatusDecided).SetDecidedAt(ts).SetSettled(markSettled)
					uid := users[j%len(users)]
					ansCreates = append(ansCreates, tx.DecisionAnswer.Create().
						SetID(ansID).
						SetDecisionRowID(decID).
						SetProjectID(pUUID).
						SetAskerAgentID(agentID).
						SetKey(key).
						SetVersion(1).
						SetQuestionText(fmt.Sprintf("Deploy service %d in project %d?", j, p)).
						SetChoiceID("yes").
						SetChoiceLabel("Yes").
						SetAnsweredByUserID(uid).
						SetAnsweredByName(fmt.Sprintf("User %d", j%len(users))).
						SetProvenance(decisionanswer.ProvenanceDirect).
						SetOutcome(decisionanswer.OutcomeAnswer).
						SetSurface(decisionanswer.SurfaceDecisionsPage).
						SetResponseMessageID(respMsgID).
						SetCreatedAt(ts))
				}
				decCreates = append(decCreates, dc)
			}
			require.NoError(tb, tx.Decision.CreateBulk(decCreates...).Exec(ctx))
			totalDecisions += len(decCreates)
			if len(ansCreates) > 0 {
				require.NoError(tb, tx.DecisionAnswer.CreateBulk(ansCreates...).Exec(ctx))
				totalAnswers += len(ansCreates)
			}
		}
		require.NoError(tb, tx.Commit())
	}

	analyzeSQL := "ANALYZE"
	if cs.Dialect() == dialect.Postgres {
		analyzeSQL = "ANALYZE decisions, decision_answers"
	}
	_, err := cs.DB().ExecContext(ctx, analyzeSQL)
	require.NoError(tb, err)

	return &decisionScaleFixture{
		cs:             cs,
		projectIDs:     projectIDs,
		projectUUIDs:   projectUUIDs,
		keysByProject:  keysByProject,
		allKeys:        allKeys,
		totalDecisions: totalDecisions,
		totalAnswers:   totalAnswers,
		seedDuration:   time.Since(seedStart),
	}
}

// BenchmarkDecisionsProfileScale is the committed opt-in scale benchmark
// (design-scale §7). Run e.g.:
//
//	go test -buildvcs=false -bench=BenchmarkDecisionsProfileScale -benchtime=3x -run=^$ ./pkg/store/entadapter
func BenchmarkDecisionsProfileScale(b *testing.B) {
	for _, sc := range decisionScaleSpecs {
		if sc.name == "X_200p_1M" && os.Getenv("SCION_DECISIONS_SCALE_X") != "1" {
			continue
		}
		b.Run(sc.name, func(b *testing.B) {
			f := seedDecisionScaleFixture(b, sc, true)
			ctx := context.Background()
			b.Logf("[%s/%s] seeded %d decisions + %d answers in %v",
				sc.name, f.cs.Dialect(), f.totalDecisions, f.totalAnswers, f.seedDuration)

			b.Run("ListOpenPrompts", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					prompts, err := f.cs.ListOpenPrompts(ctx, f.projectIDs, f.allKeys)
					require.NoError(b, err)
					require.Len(b, prompts, sc.numProjects*sc.openPerProject)
				}
			})

			b.Run("ProjectsWithUnsettledDecisions", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, err := f.cs.ProjectsWithUnsettledDecisions(ctx, f.projectIDs)
					require.NoError(b, err)
					require.Len(b, got, sc.numProjects)
				}
			})

			b.Run("ProjectsWithUnsettledDecisions_Quiet", func(b *testing.B) {
				// Settle all rows temporarily to measure the quiet-user fast path.
				_, err := f.cs.client.Decision.Update().SetSettled(true).Save(ctx)
				require.NoError(b, err)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, err := f.cs.ProjectsWithUnsettledDecisions(ctx, f.projectIDs)
					require.NoError(b, err)
					require.Empty(b, got)
				}
			})
		})
	}
}

// TestDecisionsScaleStore runs the store-level scale timing table when
// SCION_DECISIONS_SCALE=1 (and optionally SCION_DECISIONS_SCALE_X=1 for 1M).
func TestDecisionsScaleStore(t *testing.T) {
	if os.Getenv("SCION_DECISIONS_SCALE") != "1" {
		t.Skip("set SCION_DECISIONS_SCALE=1 to run store-level scale timings")
	}
	ctx := context.Background()
	for _, sc := range decisionScaleSpecs {
		if sc.name == "X_200p_1M" && os.Getenv("SCION_DECISIONS_SCALE_X") != "1" {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			f := seedDecisionScaleFixture(t, sc, true)
			t.Logf("[%s/%s] seeded %d decisions + %d answers across %d projects in %v",
				sc.name, f.cs.Dialect(), f.totalDecisions, f.totalAnswers, sc.numProjects, f.seedDuration)

			t0 := time.Now()
			pwds, err := f.cs.ProjectsWithDecisions(ctx, f.projectIDs)
			dPWD := time.Since(t0)
			require.NoError(t, err)
			require.Len(t, pwds, sc.numProjects)

			t0 = time.Now()
			unsettled, err := f.cs.ProjectsWithUnsettledDecisions(ctx, f.projectIDs)
			dUnsettled := time.Since(t0)
			require.NoError(t, err)
			require.Len(t, unsettled, sc.numProjects)

			t0 = time.Now()
			openPrompts, err := f.cs.ListOpenPrompts(ctx, f.projectIDs, f.allKeys)
			dOpen := time.Since(t0)
			require.NoError(t, err)
			require.Len(t, openPrompts, sc.numProjects*sc.openPerProject)

			// Quiet-user measurement (all rows settled).
			_, err = f.cs.client.Decision.Update().SetSettled(true).Save(ctx)
			require.NoError(t, err)
			t0 = time.Now()
			quietUnsettled, err := f.cs.ProjectsWithUnsettledDecisions(ctx, f.projectIDs)
			dQuietUnsettled := time.Since(t0)
			require.NoError(t, err)
			require.Empty(t, quietUnsettled)

			t0 = time.Now()
			quietOpen, err := f.cs.ListOpenPrompts(ctx, f.projectIDs, f.allKeys)
			dQuietOpen := time.Since(t0)
			require.NoError(t, err)
			require.Empty(t, quietOpen)

			t.Logf("[%s/%s] STORE TIMINGS:", sc.name, f.cs.Dialect())
			t.Logf("  ProjectsWithDecisions(%d)                  = %v", sc.numProjects, dPWD)
			t.Logf("  ProjectsWithUnsettledDecisions(%d open)    = %v", len(unsettled), dUnsettled)
			t.Logf("  ListOpenPrompts(%d open)                   = %v", len(openPrompts), dOpen)
			t.Logf("  ProjectsWithUnsettledDecisions(quiet)      = %v", dQuietUnsettled)
			t.Logf("  ListOpenPrompts(quiet)                     = %v", dQuietOpen)
		})
	}
}
