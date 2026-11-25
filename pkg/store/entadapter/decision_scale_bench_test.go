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
	"sort"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionfacetentry"
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
	if t, ok := tb.(*testing.T); ok && os.Getenv("SCION_TEST_POSTGRES_DSN") == "" {
		// Under -tags integration + SCION_TEST_POSTGRES_URL, enttest.NewClient(t)
		// provisions an isolated ephemeral Postgres database and drops it on cleanup.
		return newTestCompositeStore(t)
	}
	if dsn := os.Getenv("SCION_TEST_POSTGRES_DSN"); dsn != "" {
		// Isolate benchmark runs in a fresh ephemeral schema so concurrent or
		// repeated runs never pollute a shared Postgres database.
		schemaName := "scale_bench_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		adminClient, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2})
		require.NoError(tb, err)
		adminCS := NewCompositeStore(adminClient)
		_, err = adminCS.DB().ExecContext(ctx, "CREATE SCHEMA "+schemaName)
		require.NoError(tb, err)
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		scopedDSN := dsn + sep + "search_path=" + schemaName
		client, err := entc.OpenPostgres(scopedDSN, entc.PoolConfig{MaxOpenConns: 4})
		if err != nil {
			_, _ = adminCS.DB().ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
			_ = adminCS.Close()
			require.NoError(tb, err)
		}
		require.NoError(tb, entc.AutoMigrate(ctx, client))
		cs := NewCompositeStore(client)
		tb.Cleanup(func() {
			_ = cs.Close()
			_, _ = adminCS.DB().ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
			_ = adminCS.Close()
		})
		return cs
	}
	dbPath := filepath.Join(tb.TempDir(), "scale.db")
	client, err := entc.OpenSQLite(dbPath, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(tb, err)
	require.NoError(tb, entc.AutoMigrate(ctx, client))
	cs := NewCompositeStore(client)
	tb.Cleanup(func() { _ = cs.Close() })
	return cs
}

func measureColdAndWarmStats(tb testing.TB, warmRuns int, fn func() error) (cold, warmMedian, tailMedian, tailMax time.Duration) {
	tb.Helper()
	t0 := time.Now()
	require.NoError(tb, fn())
	cold = time.Since(t0)

	samples := make([]time.Duration, warmRuns)
	for i := 0; i < warmRuns; i++ {
		t1 := time.Now()
		require.NoError(tb, fn())
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

	// Include a per-spec namespace byte so running R and S sequentially
	// against the same Postgres DSN does not collide on primary keys.
	var specTag byte
	for i := 0; i < len(sc.name); i++ {
		specTag ^= sc.name[i]
	}
	detUUID := func(tag byte, p, j int) uuid.UUID {
		var u uuid.UUID
		u[0] = tag
		u[1] = specTag
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
		var projFacetDeltas []facetEntryDelta

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
					SetFacetIndexed(markSettled).
					SetCreatedAt(ts)
				if markSettled {
					projFacetDeltas = append(projFacetDeltas, facetEntryDelta{
						ProjectID: pUUID,
						ChatKey:   chatKey,
						Kind:      decisionfacetentry.KindAsker,
						ActorID:   agentID.String(),
						Name:      agentName,
						LatestAt:  ts,
						Delta:     1,
					})
				}

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
						SetFacetIndexed(markSettled).
						SetCreatedAt(ts))
					if markSettled {
						projFacetDeltas = append(projFacetDeltas, facetEntryDelta{
							ProjectID: pUUID, ChatKey: chatKey, Kind: decisionfacetentry.KindAnswerer,
							ActorID: uid, Name: "User 0", LatestAt: ts, Delta: 1,
						})
					}
				case isOpen:
					dc.SetStatus(decision.StatusPending).SetSettled(false)
				case isWithdrawn:
					dc.SetStatus(decision.StatusWithdrawn).SetWithdrawnAt(ts).SetSettled(markSettled)
				case isDismissed:
					dc.SetStatus(decision.StatusDismissed).SetDismissedAt(ts).SetSettled(markSettled)
					uid := users[j%len(users)]
					uName := fmt.Sprintf("User %d", j%len(users))
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
						SetAnsweredByName(uName).
						SetProvenance(decisionanswer.ProvenanceDirect).
						SetOutcome(decisionanswer.OutcomeDismiss).
						SetSurface(decisionanswer.SurfaceDecisionsPage).
						SetResponseMessageID(respMsgID).
						SetFacetIndexed(markSettled).
						SetCreatedAt(ts))
					if markSettled {
						projFacetDeltas = append(projFacetDeltas, facetEntryDelta{
							ProjectID: pUUID, ChatKey: chatKey, Kind: decisionfacetentry.KindAnswerer,
							ActorID: uid, Name: uName, LatestAt: ts, Delta: 1,
						})
					}
				default:
					dc.SetStatus(decision.StatusDecided).SetDecidedAt(ts).SetSettled(markSettled)
					uid := users[j%len(users)]
					uName := fmt.Sprintf("User %d", j%len(users))
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
						SetAnsweredByName(uName).
						SetProvenance(decisionanswer.ProvenanceDirect).
						SetOutcome(decisionanswer.OutcomeAnswer).
						SetSurface(decisionanswer.SurfaceDecisionsPage).
						SetResponseMessageID(respMsgID).
						SetFacetIndexed(markSettled).
						SetCreatedAt(ts))
					if markSettled {
						projFacetDeltas = append(projFacetDeltas, facetEntryDelta{
							ProjectID: pUUID, ChatKey: chatKey, Kind: decisionfacetentry.KindAnswerer,
							ActorID: uid, Name: uName, LatestAt: ts, Delta: 1,
						})
					}
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
		if markSettled && len(projFacetDeltas) > 0 {
			require.NoError(tb, applyDecisionFacetDeltas(ctx, tx.Client(), projFacetDeltas))
		}
		require.NoError(tb, tx.Commit())
	}

	// On SQLite, skip ANALYZE when markSettled==false so backfill tests match
	// production hubs (which never run ANALYZE on SQLite).
	if markSettled || cs.Dialect() == dialect.Postgres {
		analyzeSQL := "ANALYZE"
		if cs.Dialect() == dialect.Postgres {
			analyzeSQL = "ANALYZE decisions, decision_answers, decision_facet_entries"
		}
		_, err := cs.DB().ExecContext(ctx, analyzeSQL)
		require.NoError(tb, err)
	}

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

			b.Run("ListDecisionFacets", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					ag, ans, ck, err := f.cs.ListDecisionFacets(ctx, f.projectIDs, f.allKeys)
					require.NoError(b, err)
					require.NotEmpty(b, ag)
					require.NotEmpty(b, ans)
					require.NotEmpty(b, ck)
				}
			})

			b.Run("ListProjectDecisionAgentsForProjects", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, err := f.cs.ListProjectDecisionAgentsForProjects(ctx, f.projectIDs)
					require.NoError(b, err)
					require.Len(b, got, sc.numProjects)
				}
			})

			b.Run("ProjectsWithDecisions", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, err := f.cs.ProjectsWithDecisions(ctx, f.projectIDs)
					require.NoError(b, err)
					require.Len(b, got, sc.numProjects)
				}
			})

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
				// Scope the update to this fixture's projects so a shared DB is never touched.
				_, err := f.cs.client.Decision.Update().
					Where(decision.ProjectIDIn(f.projectUUIDs...)).
					SetSettled(true).
					Save(ctx)
				require.NoError(b, err)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, err := f.cs.ProjectsWithUnsettledDecisions(ctx, f.projectIDs)
					require.NoError(b, err)
					require.Empty(b, got)
				}
			})

			b.Run("ListDecisionAnswers_MadeRecent_Page1", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					entries, next, err := f.cs.ListDecisionAnswersForProjects(ctx, f.projectIDs, f.keysByProject, store.ProjectDecisionAnswerFilter{}, "", 20)
					require.NoError(b, err)
					require.Len(b, entries, 20)
					require.NotEmpty(b, next)
				}
			})

			b.Run("ListDecisionAnswersPerProject_MadeProject", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					byProj, err := f.cs.ListDecisionAnswersPerProject(ctx, f.projectIDs, f.keysByProject, store.ProjectDecisionAnswerFilter{}, 6)
					require.NoError(b, err)
					require.Len(b, byProj, sc.numProjects)
				}
			})

			b.Run("ListDecisionAnswers_SingleProject", func(b *testing.B) {
				pid := f.projectIDs[1]
				pkeys := f.keysByProject[pid]
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					entries, _, err := f.cs.ListDecisionAnswers(ctx, []string{pid}, pkeys, store.ProjectDecisionAnswerFilter{}, "", 20)
					require.NoError(b, err)
					require.Len(b, entries, 20)
				}
			})
		})
	}
}

// decisionScaleSmokeSpec is the small correctness fixture run by default when
// SCION_DECISIONS_SCALE != "1" so TestDecisionsScaleStore and
// TestDecisionsScaleBackfill never skip in CI (and satisfy the entadapter
// Postgres no-unexpected-skip guard) while finishing in <1s on both dialects.
var decisionScaleSmokeSpec = decisionScaleSpec{
	name:             "smoke_2p_50",
	numProjects:      2,
	topicsPerProject: 2,
	agentsPerProject: 2,
	rowsPerProject:   50,
	openPerProject:   1,
}

// TestDecisionsScaleStore verifies store-level scale query correctness on a
// tiny fixture by default, or runs the full R/S(/X) timing table (cold first
// call + 10 warm calls on one pool, reporting warm median plus calls 7-10
// median/max to exercise Postgres pgx generic-plan caching) when
// SCION_DECISIONS_SCALE=1 (and optionally SCION_DECISIONS_SCALE_X=1 for 1M).
func TestDecisionsScaleStore(t *testing.T) {
	specs := []decisionScaleSpec{decisionScaleSmokeSpec}
	warmRuns := 1
	if os.Getenv("SCION_DECISIONS_SCALE") == "1" {
		specs = decisionScaleSpecs
		warmRuns = 10
	}
	ctx := context.Background()
	for _, sc := range specs {
		if sc.name == "X_200p_1M" && os.Getenv("SCION_DECISIONS_SCALE_X") != "1" {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			f := seedDecisionScaleFixture(t, sc, true)
			t.Logf("[%s/%s] seeded %d decisions + %d answers across %d projects in %v",
				sc.name, f.cs.Dialect(), f.totalDecisions, f.totalAnswers, sc.numProjects, f.seedDuration)

			coldProbe, medProbe, tailMedProbe, tailMaxProbe := measureColdAndWarmStats(t, warmRuns, func() error {
				unindexed, err := f.cs.hasUnindexedDecisionFacets(ctx, f.projectUUIDs)
				if err != nil {
					return err
				}
				require.False(t, unindexed)
				return nil
			})

			coldPWD, medPWD, tailMedPWD, tailMaxPWD := measureColdAndWarmStats(t, warmRuns, func() error {
				pwds, err := f.cs.ProjectsWithDecisions(ctx, f.projectIDs)
				if err != nil {
					return err
				}
				require.Len(t, pwds, sc.numProjects)
				return nil
			})

			coldFacets, medFacets, tailMedFacets, tailMaxFacets := measureColdAndWarmStats(t, warmRuns, func() error {
				ag, ans, ck, err := f.cs.ListDecisionFacets(ctx, f.projectIDs, f.allKeys)
				if err != nil {
					return err
				}
				require.NotEmpty(t, ag)
				require.NotEmpty(t, ans)
				require.NotEmpty(t, ck)
				return nil
			})

			singleKeys := f.keysByProject[f.projectIDs[0]]
			coldFacetsSingle, medFacetsSingle, tailMedFacetsSingle, tailMaxFacetsSingle := measureColdAndWarmStats(t, warmRuns, func() error {
				ag, ans, ck, err := f.cs.ListDecisionFacets(ctx, f.projectIDs[:1], singleKeys)
				if err != nil {
					return err
				}
				require.NotEmpty(t, ag)
				require.NotEmpty(t, ans)
				require.NotEmpty(t, ck)
				return nil
			})

			coldAgentsBatch, medAgentsBatch, tailMedAgentsBatch, tailMaxAgentsBatch := measureColdAndWarmStats(t, warmRuns, func() error {
				got, err := f.cs.ListProjectDecisionAgentsForProjects(ctx, f.projectIDs)
				if err != nil {
					return err
				}
				require.Len(t, got, sc.numProjects)
				return nil
			})

			coldUnsettled, medUnsettled, tailMedUnsettled, tailMaxUnsettled := measureColdAndWarmStats(t, warmRuns, func() error {
				unsettled, err := f.cs.ProjectsWithUnsettledDecisions(ctx, f.projectIDs)
				if err != nil {
					return err
				}
				require.Len(t, unsettled, sc.numProjects)
				return nil
			})

			coldOpen, medOpen, tailMedOpen, tailMaxOpen := measureColdAndWarmStats(t, warmRuns, func() error {
				openPrompts, err := f.cs.ListOpenPrompts(ctx, f.projectIDs, f.allKeys)
				if err != nil {
					return err
				}
				require.Len(t, openPrompts, sc.numProjects*sc.openPerProject)
				return nil
			})

			var nextCur string
			coldRecentP1, medRecentP1, tailMedRecentP1, tailMaxRecentP1 := measureColdAndWarmStats(t, warmRuns, func() error {
				recentPage1, nc, err := f.cs.ListDecisionAnswersForProjects(ctx, f.projectIDs, f.keysByProject, store.ProjectDecisionAnswerFilter{}, "", 20)
				if err != nil {
					return err
				}
				require.Len(t, recentPage1, 20)
				require.NotEmpty(t, nc)
				nextCur = nc
				return nil
			})

			coldRecentP2, medRecentP2, tailMedRecentP2, tailMaxRecentP2 := measureColdAndWarmStats(t, warmRuns, func() error {
				recentPage2, _, err := f.cs.ListDecisionAnswersForProjects(ctx, f.projectIDs, f.keysByProject, store.ProjectDecisionAnswerFilter{}, nextCur, 20)
				if err != nil {
					return err
				}
				require.Len(t, recentPage2, 20)
				return nil
			})

			coldPerProj, medPerProj, tailMedPerProj, tailMaxPerProj := measureColdAndWarmStats(t, warmRuns, func() error {
				byProj, err := f.cs.ListDecisionAnswersPerProject(ctx, f.projectIDs, f.keysByProject, store.ProjectDecisionAnswerFilter{}, 6)
				if err != nil {
					return err
				}
				require.Len(t, byProj, sc.numProjects)
				return nil
			})

			coldSingleAns, medSingleAns, tailMedSingleAns, tailMaxSingleAns := measureColdAndWarmStats(t, warmRuns, func() error {
				singleAns, _, err := f.cs.ListDecisionAnswers(ctx, f.projectIDs[1:2], f.keysByProject[f.projectIDs[1]], store.ProjectDecisionAnswerFilter{}, "", 20)
				if err != nil {
					return err
				}
				require.Len(t, singleAns, 20)
				return nil
			})

			coldDMHeavyAns, medDMHeavyAns, tailMedDMHeavyAns, tailMaxDMHeavyAns := measureColdAndWarmStats(t, warmRuns, func() error {
				dmHeavyAns, _, err := f.cs.ListDecisionAnswers(ctx, f.projectIDs[:1], f.keysByProject[f.projectIDs[0]], store.ProjectDecisionAnswerFilter{}, "", 20)
				if err != nil {
					return err
				}
				if sc.rowsPerProject >= 200 {
					require.Len(t, dmHeavyAns, 20)
				} else {
					require.NotEmpty(t, dmHeavyAns)
				}
				return nil
			})

			// Quiet-user measurement: scope update to this fixture's projects only.
			_, err := f.cs.client.Decision.Update().
				Where(decision.ProjectIDIn(f.projectUUIDs...)).
				SetSettled(true).
				Save(ctx)
			require.NoError(t, err)

			coldQuietUnsettled, medQuietUnsettled, tailMedQuietUnsettled, tailMaxQuietUnsettled := measureColdAndWarmStats(t, warmRuns, func() error {
				quietUnsettled, err := f.cs.ProjectsWithUnsettledDecisions(ctx, f.projectIDs)
				if err != nil {
					return err
				}
				require.Empty(t, quietUnsettled)
				return nil
			})

			coldQuietOpen, medQuietOpen, tailMedQuietOpen, tailMaxQuietOpen := measureColdAndWarmStats(t, warmRuns, func() error {
				quietOpen, err := f.cs.ListOpenPrompts(ctx, f.projectIDs, f.allKeys)
				if err != nil {
					return err
				}
				require.Empty(t, quietOpen)
				return nil
			})

			t.Logf("[%s/%s] STORE TIMINGS (cold / warm-med / calls7-10 med / calls7-10 max, warmRuns=%d):", sc.name, f.cs.Dialect(), warmRuns)
			t.Logf("  hasUnindexedDecisionFacets(%d)             = %v / %v / %v / %v", sc.numProjects, coldProbe, medProbe, tailMedProbe, tailMaxProbe)
			t.Logf("  ProjectsWithDecisions(%d)                  = %v / %v / %v / %v", sc.numProjects, coldPWD, medPWD, tailMedPWD, tailMaxPWD)
			t.Logf("  ListDecisionFacets(%d projects)            = %v / %v / %v / %v", sc.numProjects, coldFacets, medFacets, tailMedFacets, tailMaxFacets)
			t.Logf("  ListDecisionFacets(single project)         = %v / %v / %v / %v", coldFacetsSingle, medFacetsSingle, tailMedFacetsSingle, tailMaxFacetsSingle)
			t.Logf("  ListProjectDecisionAgentsForProjects(%d)   = %v / %v / %v / %v", sc.numProjects, coldAgentsBatch, medAgentsBatch, tailMedAgentsBatch, tailMaxAgentsBatch)
			t.Logf("  ProjectsWithUnsettledDecisions(%d open)    = %v / %v / %v / %v", sc.numProjects, coldUnsettled, medUnsettled, tailMedUnsettled, tailMaxUnsettled)
			t.Logf("  ListOpenPrompts(%d open)                   = %v / %v / %v / %v", sc.numProjects*sc.openPerProject, coldOpen, medOpen, tailMedOpen, tailMaxOpen)
			t.Logf("  ListDecisionAnswers(recent p1, 20)         = %v / %v / %v / %v", coldRecentP1, medRecentP1, tailMedRecentP1, tailMaxRecentP1)
			t.Logf("  ListDecisionAnswers(recent p2, 20)         = %v / %v / %v / %v", coldRecentP2, medRecentP2, tailMedRecentP2, tailMaxRecentP2)
			t.Logf("  ListDecisionAnswersPerProject(6/proj)      = %v / %v / %v / %v", coldPerProj, medPerProj, tailMedPerProj, tailMaxPerProj)
			t.Logf("  ListDecisionAnswers(single normal, 20)     = %v / %v / %v / %v", coldSingleAns, medSingleAns, tailMedSingleAns, tailMaxSingleAns)
			t.Logf("  ListDecisionAnswers(single DM-heavy, 20)   = %v / %v / %v / %v", coldDMHeavyAns, medDMHeavyAns, tailMedDMHeavyAns, tailMaxDMHeavyAns)
			t.Logf("  ProjectsWithUnsettledDecisions(quiet)      = %v / %v / %v / %v", coldQuietUnsettled, medQuietUnsettled, tailMedQuietUnsettled, tailMaxQuietUnsettled)
			t.Logf("  ListOpenPrompts(quiet)                     = %v / %v / %v / %v", coldQuietOpen, medQuietOpen, tailMedQuietOpen, tailMaxQuietOpen)
		})
	}
}

// TestDecisionsScaleBackfill verifies keyset-cursor settled-hint, facet
// backfill, and reconciler correctness on a small fixture by default, or
// measures a full Scale S (200p / 200k decisions) backfill on SQLite
// (file-backed, MaxOpenConns=1, no ANALYZE) and PostgreSQL when
// SCION_DECISIONS_SCALE=1.
func TestDecisionsScaleBackfill(t *testing.T) {
	ctx := context.Background()
	sc := decisionScaleSmokeSpec
	pageSize := 20
	minPages := 2
	if os.Getenv("SCION_DECISIONS_SCALE") == "1" {
		sc = decisionScaleSpecs[1] // S_200p_200k
		pageSize = DefaultDecisionIndexPageSize
		minPages = 10
	}
	f := seedDecisionScaleFixture(t, sc, false)
	t.Logf("[%s/%s] seeded %d decisions (all settled=false, facet_indexed=false) in %v",
		sc.name, f.cs.Dialect(), f.totalDecisions, f.seedDuration)

	median := func(ds []time.Duration) time.Duration {
		cp := append([]time.Duration(nil), ds...)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		return cp[len(cp)/2]
	}

	runPagedStep := func(stepName string, fn func(cursor string) (int, string, error)) (int, int) {
		var pageDurs []time.Duration
		totalUpdated := 0
		cursor := ""
		totalStart := time.Now()
		for {
			pStart := time.Now()
			n, nextCursor, err := fn(cursor)
			pDur := time.Since(pStart)
			require.NoError(t, err, "backfill step %s", stepName)
			if nextCursor == "" {
				break
			}
			pageDurs = append(pageDurs, pDur)
			totalUpdated += n
			cursor = nextCursor
		}
		totalDur := time.Since(totalStart)
		require.GreaterOrEqual(t, len(pageDurs), minPages, "backfill step %s page count", stepName)

		window := 5
		if len(pageDurs) < window {
			window = len(pageDurs)
		}
		firstMed := median(pageDurs[:window])
		lastMed := median(pageDurs[len(pageDurs)-window:])
		worst := pageDurs[0]
		for _, d := range pageDurs[1:] {
			if d > worst {
				worst = d
			}
		}
		t.Logf("[%s/%s] BACKFILL %s (%d rows, %d pages of %d):",
			sc.name, f.cs.Dialect(), stepName, totalUpdated, len(pageDurs), pageSize)
		t.Logf("  total=%v  first-%d med=%v  last-%d med=%v  worst-page=%v",
			totalDur, window, firstMed, window, lastMed, worst)
		return totalUpdated, len(pageDurs)
	}

	totalSwept, _ := runPagedStep("settled", func(cursor string) (int, string, error) {
		return f.cs.SweepSettledDecisionsPage(ctx, cursor, pageSize)
	})
	totalFacetDecs, _ := runPagedStep("facet_decisions", func(cursor string) (int, string, error) {
		return f.cs.IndexDecisionFacetsDecisionsPage(ctx, cursor, pageSize)
	})
	totalFacetAns, _ := runPagedStep("facet_answers", func(cursor string) (int, string, error) {
		return f.cs.IndexDecisionFacetsAnswersPage(ctx, cursor, pageSize)
	})

	recStart := time.Now()
	reconciled, err := f.cs.ReconcileDecisionFacets(ctx)
	recDur := time.Since(recStart)
	require.NoError(t, err)
	t.Logf("[%s/%s] BACKFILL reconcile (%d deleted): total=%v",
		sc.name, f.cs.Dialect(), reconciled, recDur)

	pending, err := f.cs.HasPendingDecisionIndexWork(ctx)
	require.NoError(t, err)
	require.False(t, pending, "no pending decision index work after full backfill")

	expectedSettled := f.totalDecisions - sc.numProjects*sc.openPerProject
	require.Equal(t, expectedSettled, totalSwept)
	require.Equal(t, f.totalDecisions, totalFacetDecs)
	require.Greater(t, totalFacetAns, 0)
}
