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
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHarnessBackfillTestStore sets up a CompositeStore plus a seeded project,
// for tests that create pre-column ("legacy") agent rows directly through
// the ent client rather than through AgentStore.CreateAgent — CreateAgent
// always sets harness_config via harnessConfigOf, so it can't produce the
// harness_config-IS-NULL rows these tests need to exercise the reconcile
// path itself (ptone/scion#2146).
func newHarnessBackfillTestStore(t *testing.T) (*CompositeStore, uuid.UUID) {
	t.Helper()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	projectUID := uuid.New()
	_, err := client.Project.Create().
		SetID(projectUID).
		SetName("harness-backfill-project").
		SetSlug("harness-backfill-project").
		Save(context.Background())
	require.NoError(t, err)

	return cs, projectUID
}

// createLegacyAgent inserts an agent row directly via the ent client, with
// harness_config left at its zero value (NULL) and applied_config set to the
// given raw JSON (or left NULL if rawAppliedConfig is ""), simulating a row
// written before the harness_config column existed — exactly the shape
// ReconcileHarnessConfigColumn exists to repair.
func createLegacyAgent(t *testing.T, cs *CompositeStore, projectUID uuid.UUID, slug, rawAppliedConfig string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	create := cs.client.Agent.Create().
		SetID(id).
		SetSlug(slug).
		SetName(slug).
		SetProjectID(projectUID)
	if rawAppliedConfig != "" {
		create = create.SetAppliedConfig(rawAppliedConfig)
	}
	_, err := create.Save(context.Background())
	require.NoError(t, err)
	return id
}

// harnessConfigIsNil reports whether id's harness_config column is currently
// NULL (as opposed to a non-NULL "" sentinel or a real value) — a
// distinction that matters because the reconcile's convergence to an empty
// working set depends on it. Tests use this instead of just asserting the
// Go-level value is empty, since "" is what both a NULL column and a
// written-empty-sentinel column decode to.
func harnessConfigIsNil(t *testing.T, cs *CompositeStore, id uuid.UUID) bool {
	t.Helper()
	exists, err := cs.client.Agent.Query().
		Where(agent.IDEQ(id), agent.HarnessConfigIsNil()).
		Exist(context.Background())
	require.NoError(t, err)
	return exists
}

// reconcilePendingCount returns the number of rows ReconcileHarnessConfigColumn
// would currently select (harness_config IS NULL AND applied_config IS NOT
// NULL) — the set that must converge to (and stay at) zero once every row
// has been visited by a column-aware binary.
func reconcilePendingCount(t *testing.T, cs *CompositeStore) int {
	t.Helper()
	n, err := cs.client.Agent.Query().
		Where(agent.HarnessConfigIsNil(), agent.AppliedConfigNotNil()).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

func TestReconcileHarnessConfigColumn_ValidHarness(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "valid-harness",
		`{"harnessConfig":"claude"}`)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig)

	// And the actual --harness filter now matches, which is the point.
	result, err := cs.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, id.String(), result.Items[0].ID)
}

// TestReconcileHarnessConfigColumn_InvalidJSON covers the sentinel behavior
// for a truly-invalid-JSON row: it must leave NULL and land on the ""
// sentinel, not stay NULL forever.
func TestReconcileHarnessConfigColumn_InvalidJSON(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "invalid-json", "{not json at all")

	// Must not fail the whole migration over one corrupt row.
	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.HarnessConfig, "a row that isn't valid JSON at all has nothing usable to extract")
	assert.False(t, harnessConfigIsNil(t, cs, id),
		"an invalid-JSON row must be written as the \"\" sentinel, not left NULL")
}

// TestReconcileHarnessConfigColumn_SanitizedGCPModeRow covers a row whose
// applied_config parses fine but needed sanitizing (an invalid GCP metadata
// mode): it must still have its HarnessConfig backfilled —
// parseAppliedConfig returns a non-nil cfg AND a non-nil error in this
// case, and treating any non-nil error as "corrupt, skip" would
// permanently lose this agent from every future --harness match.
// entAgentToStore applies the identical
// tolerance when building the response-facing HarnessConfig, so the
// reconcile must match that, not be stricter than it.
func TestReconcileHarnessConfigColumn_SanitizedGCPModeRow(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "sanitized-gcp-mode",
		`{"harnessConfig":"claude","gcpIdentity":{"metadataMode":"bogus"}}`)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig,
		"a sanitized-but-parseable row must still be backfilled — it is not the same as a corrupt row")

	result, err := cs.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, id.String(), result.Items[0].ID)
}

// TestReconcileHarnessConfigColumn_NoAppliedConfig covers a row with no
// applied_config text at all — NULL applied_config, not just an empty
// harness. Such a row is excluded by the reconcile's own
// AppliedConfigNotNil() query guard, so it is never visited, and stays
// harness_config NULL forever — that's fine: there's nothing to reconcile
// from, and this shape doesn't occur for any agent write path in this
// codebase (CreateAgent's marshalAppliedConfig always sets a real document
// when a harness could exist). Distinguished from "empty-string
// applied_config", which the reconcile DOES visit — see the next test.
func TestReconcileHarnessConfigColumn_NoAppliedConfig(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "no-applied-config", "")

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.HarnessConfig)
	assert.True(t, harnessConfigIsNil(t, cs, id),
		"a NULL applied_config is excluded by the query guard, so it is never visited and stays NULL")
}

// TestReconcileHarnessConfigColumn_EmptyHarnessConfigValueNotBackfilled
// covers valid JSON with no harnessConfig key at all — this DOES get
// visited (applied_config is non-NULL), and must land on the "" sentinel,
// not stay NULL.
func TestReconcileHarnessConfigColumn_EmptyHarnessConfigValueNotBackfilled(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	// Valid JSON, but no harnessConfig key at all — nothing to backfill.
	id := createLegacyAgent(t, cs, projectUID, "no-harness-key", `{"image":"img:1"}`)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.HarnessConfig)
	assert.False(t, harnessConfigIsNil(t, cs, id),
		"a visited row with no harness must land on the \"\" sentinel, not stay NULL")
}

// TestReconcileHarnessConfigColumn_LegacyHarnessKeyNotBackfilled covers a
// row using the pre-0be8382 "harness" key instead of "harnessConfig"
// (declined as a display/filter gap — but this row must still leave the
// reconcile's pending set like any other no-usable-harness row, or it
// would be rescanned every boot forever via a different path).
func TestReconcileHarnessConfigColumn_LegacyHarnessKeyNotBackfilled(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "legacy-harness-key", `{"harness":"claude"}`)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.HarnessConfig, "the legacy \"harness\" key is not read by parseAppliedConfig")
	assert.False(t, harnessConfigIsNil(t, cs, id),
		"a legacy-key row must still land on the \"\" sentinel and leave the pending set")

	noMatch, err := cs.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, noMatch.Items, "the legacy key's value is not exposed as a match")
}

// TestReconcileHarnessConfigColumn_ConvergesToEmptySet covers: after one
// reconcile pass, every row this migration can usefully touch (no-harness,
// invalid-JSON, and legacy-key rows included) must have LEFT the pending
// set (harness_config IS NULL AND applied_config IS NOT NULL). A second
// pass must find nothing left to do. Without the sentinel rule, all of
// these rows would stay NULL forever, so this count would never reach zero
// and the set would only grow — contradicting the "a caught-up Hub does one
// empty-result query" invariant the docs claim.
func TestReconcileHarnessConfigColumn_ConvergesToEmptySet(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	createLegacyAgent(t, cs, projectUID, "valid", `{"harnessConfig":"claude"}`)
	createLegacyAgent(t, cs, projectUID, "no-harness-key", `{"image":"img:1"}`)
	createLegacyAgent(t, cs, projectUID, "invalid-json", "{not json")
	createLegacyAgent(t, cs, projectUID, "legacy-key", `{"harness":"claude"}`)
	createLegacyAgent(t, cs, projectUID, "sanitized", `{"harnessConfig":"gemini","gcpIdentity":{"metadataMode":"bogus"}}`)

	require.Equal(t, 5, reconcilePendingCount(t, cs), "sanity: all five rows start pending")

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))
	assert.Equal(t, 0, reconcilePendingCount(t, cs),
		"a single pass must reconcile every row it can usefully touch, including no-harness/invalid/legacy rows")

	// A second pass has nothing to do — this is the "caught-up Hub does one
	// empty-result query" property the docs now correctly claim.
	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))
	assert.Equal(t, 0, reconcilePendingCount(t, cs))
}

// TestReconcileHarnessConfigColumn_CrossesPageBoundary covers paging: with
// more legacy rows than the reconcile's page size, pagination must still
// visit every row, including the first row of the second page and the last
// row overall — not just "the first page happens to work". See
// TestReconcileHarnessConfigColumn_PagingAdvancesCursor, right below, for
// the mutation-sensitive companion: this test alone does not actually
// depend on keyset (IDGT) pagination working correctly, because
// every row here gets a distinct real value and therefore leaves the
// pending set regardless of whether the cursor advances (a re-issued,
// unbounded query would still only see the rows that are still pending). It
// is kept because it is still useful, real coverage of the boundary at
// production scale (505 > 500).
func TestReconcileHarnessConfigColumn_CrossesPageBoundary(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	const totalRows = 505 // > the reconcile's default page size (500)
	ids := make([]uuid.UUID, totalRows)
	for i := 0; i < totalRows; i++ {
		ids[i] = createLegacyAgent(t, cs, projectUID,
			fmt.Sprintf("page-agent-%03d", i),
			fmt.Sprintf(`{"harnessConfig":"harness-%03d"}`, i))
	}

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	// Spot-check the first row, the last row of the first page, the first
	// row of the second page, and the last row overall — the boundary
	// itself and both ends of the full range.
	for _, i := range []int{0, 499, 500, totalRows - 1} {
		got, err := cs.client.Agent.Get(ctx, ids[i])
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("harness-%03d", i), got.HarnessConfig,
			"row %d's harness_config was not backfilled correctly", i)
	}

	// And every single row, via the count the --harness filter itself would
	// see for each distinct value — proves nothing was silently dropped in
	// the middle of the run either.
	for i := 0; i < totalRows; i++ {
		result, err := cs.ListAgents(ctx, store.AgentFilter{
			HarnessConfig: fmt.Sprintf("harness-%03d", i),
		}, store.ListOptions{})
		require.NoError(t, err)
		require.Lenf(t, result.Items, 1, "row %d not found by its harness_config filter", i)
	}
}

// TestReconcileHarnessConfigColumn_PagingAdvancesCursor is a test that
// actually fails if the keyset cursor (lastID/IDGT) stops advancing.
//
// Under the sentinel design, NO black-box behavioral test can tell a broken
// cursor from a working one when every visited row leaves the pending set
// on that same visit: with no lower bound, the next unbounded
// `ORDER BY id LIMIT n` query simply returns whatever still matches
// `harness_config IS NULL`, which already excludes every row the previous
// page just fixed. TestReconcileHarnessConfigColumn_CrossesPageBoundary,
// for example, still passes if lastID's assignment is mutated into a no-op.
// The cursor only matters when some row *legitimately stays pending* across
// a single call — which, by design, should no longer happen for any real
// row, so a realistic test can't produce that condition either.
//
// So this test constructs it directly, via the harnessConfigReconcileTestStuckIDs
// seam: two rows are marked "stuck" (their update is skipped, so they stay
// pending after this call, exactly like a row a real run legitimately
// couldn't fix). lastID still advances past them (set unconditionally at
// the top of the loop, before the stuck check), so a correct cursor moves on
// to the rows that sort after them. The other rows — chosen to sort AFTER
// the stuck ones — must still be reconciled correctly. Verified directly
// against the mutant: with lastID's advancement disabled, the unbounded
// query keeps re-fetching the same stuck row (the smallest ID still
// matching) forever and this test times out; with the real cursor it
// passes in well under a second. Both the mutation and its revert were
// applied only to a scratch working copy and never committed.
func TestReconcileHarnessConfigColumn_PagingAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	oldPageSize := harnessConfigReconcilePageSize
	harnessConfigReconcilePageSize = 1
	defer func() { harnessConfigReconcilePageSize = oldPageSize }()

	const totalRows = 6
	ids := make([]uuid.UUID, totalRows)
	harnessByID := make(map[uuid.UUID]string, totalRows)
	for i := 0; i < totalRows; i++ {
		harness := fmt.Sprintf("cursor-harness-%d", i)
		id := createLegacyAgent(t, cs, projectUID, fmt.Sprintf("cursor-agent-%d", i),
			fmt.Sprintf(`{"harnessConfig":%q}`, harness))
		ids[i] = id
		harnessByID[id] = harness
	}

	// Mark the two smallest IDs (by sort order, the order the reconcile
	// visits them in) as "stuck" — they must remain pending after this
	// call, and a correct cursor must still reach and reconcile everything
	// that sorts after them within the same call.
	sorted := append([]uuid.UUID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	stuckIDs := map[uuid.UUID]bool{sorted[0]: true, sorted[1]: true}

	oldStuck := harnessConfigReconcileTestStuckIDs
	harnessConfigReconcileTestStuckIDs = stuckIDs
	defer func() { harnessConfigReconcileTestStuckIDs = oldStuck }()

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	for id := range stuckIDs {
		assert.True(t, harnessConfigIsNil(t, cs, id),
			"a stuck row must remain pending after the one call that skipped it")
	}
	for _, id := range sorted[2:] {
		got, err := cs.client.Agent.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, harnessByID[id], got.HarnessConfig,
			"a row sorting after the stuck ones must still be reconciled — only reachable if the cursor advances past them instead of re-fetching them forever")
	}
}

// TestReconcileHarnessConfigColumn_Idempotent covers idempotency: a second
// call's SELECT (scoped to harness_config IS NULL) simply doesn't return a
// row that's already non-NULL, so a second call is a cheap no-op and never
// re-derives a row's harness_config from a later applied_config change —
// only CreateAgent/UpdateAgent's own sync does that. This is a different
// property from the UPDATE-side Where(HarnessConfigIsNil()) guard, which
// protects the narrower window between one reconcile call's own SELECT and
// its own UPDATE, not across two separate calls like this test exercises —
// see TestReconcileHarnessConfigColumn_DoesNotOverwriteConcurrentWrite for
// that case.
func TestReconcileHarnessConfigColumn_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "idempotent-agent",
		`{"harnessConfig":"claude"}`)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "claude", got.HarnessConfig)

	// Mutate applied_config directly (bypassing CreateAgent/UpdateAgent's
	// sync entirely) between the two reconcile calls, exactly like an
	// old-binary replica writing a fresh value without updating the column.
	_, err = cs.client.Agent.UpdateOneID(id).
		SetAppliedConfig(`{"harnessConfig":"gemini"}`).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx), "a second call must not error")

	gotAfter, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", gotAfter.HarnessConfig,
		"a row whose harness_config is already non-NULL must not be re-derived by this migration")
}

// TestReconcileHarnessConfigColumn_ToleratesConcurrentDelete covers: a
// concurrent hard delete landing between
// ReconcileHarnessConfigColumn's SELECT and its per-row UPDATE must be
// skipped (logged), not treated as a boot failure. An ent mutation hook
// intercepts exactly the UpdateOne for the targeted row and deletes it
// first, so the UPDATE that follows genuinely hits a gone row and
// genuinely returns ent's NotFound.
func TestReconcileHarnessConfigColumn_ToleratesConcurrentDelete(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	survivor := createLegacyAgent(t, cs, projectUID, "survivor", `{"harnessConfig":"claude"}`)
	deleted := createLegacyAgent(t, cs, projectUID, "deleted-mid-reconcile", `{"harnessConfig":"gemini"}`)

	// Both rows are created normally and are still present — the SELECT
	// inside ReconcileHarnessConfigColumn will see both. This hook is
	// registered on this test's own client, so it cannot affect any other
	// test's store.
	cs.client.Agent.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if am, ok := m.(*ent.AgentMutation); ok && am.Op() == ent.OpUpdateOne {
				if id, ok := am.ID(); ok && id == deleted {
					// Another replica deletes the row right between this
					// reconcile's SELECT (which already returned it) and
					// this UPDATE.
					if _, err := cs.client.Agent.Delete().Where(agent.IDEQ(deleted)).Exec(ctx); err != nil {
						return nil, err
					}
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx),
		"a concurrently deleted agent must not fail the whole reconcile")

	got, err := cs.client.Agent.Get(ctx, survivor)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig, "other rows must still be reconciled normally")

	_, err = cs.client.Agent.Get(ctx, deleted)
	assert.True(t, ent.IsNotFound(err), "the deleted row must actually be gone")
}

// TestReconcileHarnessConfigColumn_PreservesUpdatedTimestamp covers: this
// migration is a derived-column backfill, not a user-visible update, so its
// per-row UPDATE must not let
// ent's UpdateDefault(time.Now) bump the agent's `updated` column. Real
// consumers of that column include `scion list --sort updated`, the Hub's
// chat-v2 agent-roster last-activity fallback, and the notification
// stale-event guard — all of which would misbehave for every agent touched
// by a first-boot-after-upgrade reconcile if `updated` jumped to boot time.
func TestReconcileHarnessConfigColumn_PreservesUpdatedTimestamp(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "preserve-updated", `{"harnessConfig":"claude"}`)

	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := cs.client.Agent.UpdateOneID(id).SetUpdated(past).Save(ctx)
	require.NoError(t, err)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig)
	assert.True(t, got.Updated.Equal(past),
		"the reconcile must not bump updated: got %v, want %v", got.Updated, past)
}

// TestReconcileHarnessConfigColumn_DoesNotOverwriteConcurrentWrite guards
// the UPDATE-side Where(HarnessConfigIsNil()) predicate: a concurrent writer
// (e.g. another, already-serving replica's UpdateAgent call, such as the
// broker's harness overwrite) that sets a fresh, non-NULL harness_config
// between this reconcile's SELECT and its own UPDATE must win — the
// reconcile's own, by-then-stale value must never overwrite it. The
// injected write changes only harness_config (and, as a side effect,
// updated); applied_config stays byte-identical to what the reconcile's own
// SELECT read, so this test cannot pass merely because AppliedConfigEQ
// rejected the write — HarnessConfigIsNil() has to be the predicate doing
// the rejecting. Uses the same ent-mutation-hook technique as
// TestReconcileHarnessConfigColumn_ToleratesConcurrentDelete to force the
// real interleaving, guarded by a one-shot flag so the injected write
// (itself an UpdateOne) doesn't re-trigger the hook recursively.
func TestReconcileHarnessConfigColumn_DoesNotOverwriteConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "concurrent-write", `{"harnessConfig":"claude"}`)

	var injected bool
	var writerUpdated time.Time
	cs.client.Agent.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if !injected {
				if am, ok := m.(*ent.AgentMutation); ok && am.Op() == ent.OpUpdateOne {
					if mid, ok := am.ID(); ok && mid == id {
						injected = true
						writer, err := cs.client.Agent.UpdateOneID(id).
							SetHarnessConfig("gemini").
							Save(ctx)
						if err != nil {
							return nil, err
						}
						writerUpdated = writer.Updated
					}
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx),
		"a concurrent writer's fresher value must not turn into a reconcile failure")

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "gemini", got.HarnessConfig,
		"the concurrent writer's value must survive; the reconcile's own stale value must not overwrite it")
	assert.True(t, got.Updated.Equal(writerUpdated),
		"the concurrent writer's updated bump must survive: got %v, want %v", got.Updated, writerUpdated)
}

// TestReconcileHarnessConfigColumn_DoesNotOverwriteConcurrentLegacyWrite
// guards the UPDATE-side AppliedConfigEQ predicate: a pre-upgrade replica's
// UpdateAgent rewrites applied_config without touching harness_config at
// all, so harness_config stays NULL. The UPDATE's AppliedConfigEQ predicate
// must make the reconcile skip such a row — matching zero rows and leaving
// it NULL — rather than write a harness parsed from the now-superseded
// applied_config it read at SELECT time; a later reconcile call then picks
// up the current applied_config. Uses the same ent-mutation-hook technique
// as TestReconcileHarnessConfigColumn_DoesNotOverwriteConcurrentWrite,
// except the injected write sets AppliedConfig only, the way a pre-upgrade
// binary writes.
func TestReconcileHarnessConfigColumn_DoesNotOverwriteConcurrentLegacyWrite(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "concurrent-legacy-write", `{"harnessConfig":"claude"}`)

	var injected bool
	var legacyUpdated time.Time
	cs.client.Agent.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if !injected {
				if am, ok := m.(*ent.AgentMutation); ok && am.Op() == ent.OpUpdateOne {
					if mid, ok := am.ID(); ok && mid == id {
						injected = true
						legacy, err := cs.client.Agent.UpdateOneID(id).
							SetAppliedConfig(`{"harnessConfig":"gemini"}`).
							Save(ctx)
						if err != nil {
							return nil, err
						}
						legacyUpdated = legacy.Updated
					}
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx),
		"a concurrent pre-upgrade writer's change must not turn into a reconcile failure")

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.True(t, harnessConfigIsNil(t, cs, id),
		"harness_config must stay NULL so a later reconcile picks the row up")
	assert.True(t, got.Updated.Equal(legacyUpdated),
		"the legacy writer's updated bump must survive: got %v, want %v", got.Updated, legacyUpdated)

	require.NoError(t, cs.ReconcileHarnessConfigColumn(ctx),
		"a second reconcile must pick up the now-current applied_config")

	got2, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "gemini", got2.HarnessConfig,
		"the second reconcile must reflect the legacy writer's applied_config, not the original stale value")
}

func TestMigrateRunsHarnessConfigReconcile(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "via-migrate",
		`{"harnessConfig":"claude"}`)

	require.NoError(t, cs.Migrate(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig)

	// Migrate is called again on every boot in production — confirm it
	// stays a cheap no-op that doesn't error for an already-reconciled
	// store.
	require.NoError(t, cs.Migrate(ctx))
	assert.Equal(t, 0, reconcilePendingCount(t, cs))
}

// TestAgentStore_UpdateAgent_SyncsHarnessConfigColumn covers the
// update-path: CreateAgent's sync is covered by
// TestAgentStore_HarnessConfigFilter*, but UpdateAgent changing the value,
// and the column's value when AppliedConfig becomes nil, are the two
// agent_store.go paths that write harness_config on update. Both paths
// always SetHarnessConfig (never clear to NULL); "clearing" AppliedConfig
// means the column becomes "" (the sentinel), not NULL.
func TestAgentStore_UpdateAgent_SyncsHarnessConfigColumn(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "update-sync-agent")
	a.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "claude"}
	require.NoError(t, s.CreateAgent(ctx, a))

	byClaude, err := s.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, byClaude.Items, 1)

	// UpdateAgent to a new harness — matching the broker-overwrite-after-create
	// production path (pkg/hub/httpdispatcher.go).
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.AppliedConfig.HarnessConfig = "gemini"
	require.NoError(t, s.UpdateAgent(ctx, got))

	byClaudeAfter, err := s.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, byClaudeAfter.Items, "the stale value must no longer match")

	byGemini, err := s.ListAgents(ctx, store.AgentFilter{HarnessConfig: "gemini"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, byGemini.Items, 1)
	assert.Equal(t, a.ID, byGemini.Items[0].ID)

	// UpdateAgent with AppliedConfig set to nil must write the "" sentinel,
	// not leave the stale value, and must not leave the column NULL either
	// (NULL is reserved for "never written by a column-aware binary", which
	// this update is).
	got2, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got2.AppliedConfig = nil
	require.NoError(t, s.UpdateAgent(ctx, got2))

	byGeminiAfter, err := s.ListAgents(ctx, store.AgentFilter{HarnessConfig: "gemini"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, byGeminiAfter.Items, "clearing AppliedConfig must clear the harness_config column too")

	byClaudeStill, err := s.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, byClaudeStill.Items)

	uid, err := uuid.Parse(a.ID)
	require.NoError(t, err)
	assert.False(t, harnessConfigIsNil(t, NewCompositeStore(s.client), uid),
		"UpdateAgent must write the \"\" sentinel, never leave/clear to NULL")
}
