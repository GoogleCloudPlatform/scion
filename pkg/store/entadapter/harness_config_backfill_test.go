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
// harness_config-IS-NULL rows these tests need to exercise the backfill/
// reconcile path itself (ptone/scion#2146 review R4-2).
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
// given raw JSON (or left empty if rawAppliedConfig is ""), simulating a row
// written before the harness_config column existed — exactly the shape
// BackfillHarnessConfigColumn exists to repair.
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

func TestBackfillHarnessConfigColumn_ValidHarness(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "valid-harness",
		`{"harnessConfig":"claude"}`)

	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig)

	// And the actual --harness filter now matches, which is the point.
	result, err := cs.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, id.String(), result.Items[0].ID)
}

func TestBackfillHarnessConfigColumn_InvalidJSON(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "invalid-json", "{not json at all")

	// Must not fail the whole migration over one corrupt row.
	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.HarnessConfig, "a row that isn't valid JSON at all has nothing usable to extract")
}

// TestBackfillHarnessConfigColumn_SanitizedGCPModeRow is the exact
// ptone/scion#2146 review R4-1 regression probe: a row whose applied_config
// parses fine but needed sanitizing (an invalid GCP metadata mode) must
// still have its HarnessConfig backfilled — parseAppliedConfig returns a
// non-nil cfg AND a non-nil error in this case, and the pre-R4-1 backfill
// treated any non-nil error as "corrupt, skip", permanently losing this
// agent from every future --harness match (the one-shot marker meant it
// would never be revisited). entAgentToStore applies the identical
// tolerance when building the response-facing HarnessConfig, so the
// backfill must match that, not be stricter than it.
func TestBackfillHarnessConfigColumn_SanitizedGCPModeRow(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "sanitized-gcp-mode",
		`{"harnessConfig":"claude","gcpIdentity":{"metadataMode":"bogus"}}`)

	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig,
		"a sanitized-but-parseable row must still be backfilled (R4-1) — it is not the same as a corrupt row")

	result, err := cs.ListAgents(ctx, store.AgentFilter{HarnessConfig: "claude"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, id.String(), result.Items[0].ID)
}

func TestBackfillHarnessConfigColumn_NoAppliedConfig(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "no-applied-config", "")

	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.HarnessConfig)
}

func TestBackfillHarnessConfigColumn_EmptyHarnessConfigValueNotBackfilled(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	// Valid JSON, but no harnessConfig key at all — nothing to backfill.
	id := createLegacyAgent(t, cs, projectUID, "no-harness-key", `{"image":"img:1"}`)

	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.HarnessConfig)
}

// TestBackfillHarnessConfigColumn_CrossesPageBoundary is the ptone/scion#2146
// review R4-2/R4-4 paging coverage: with more legacy rows than the
// migration's page size (500), the keyset (IDGT) pagination must still visit
// every row exactly once, including the first row of the second page and the
// last row overall — not just "the first page happens to work".
func TestBackfillHarnessConfigColumn_CrossesPageBoundary(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	const totalRows = 505 // > the migration's pageSize (500)
	ids := make([]uuid.UUID, totalRows)
	for i := 0; i < totalRows; i++ {
		ids[i] = createLegacyAgent(t, cs, projectUID,
			fmt.Sprintf("page-agent-%03d", i),
			fmt.Sprintf(`{"harnessConfig":"harness-%03d"}`, i))
	}

	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx))

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

// TestBackfillHarnessConfigColumn_Idempotent is the ptone/scion#2146 review
// R4-2/R4-5 idempotency coverage: after the marker gate was dropped in favor
// of an every-boot reconcile scoped to harness_config IS NULL (R4-5, option
// (b)), idempotency now comes from that WHERE clause rather than a marker —
// a second call must be a cheap no-op for rows already populated, and must
// NOT clobber an already-populated column even if applied_config changes
// afterward (this is also the R4-4 IsNil-guard-closes-the-overwrite-window
// property, and it is the flip side of the documented mixed-version-rollout
// residual gap in dev-notes: a row's harness_config, once non-NULL, is never
// re-derived from a later applied_config change by this migration — only by
// CreateAgent/UpdateAgent's own sync).
func TestBackfillHarnessConfigColumn_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "idempotent-agent",
		`{"harnessConfig":"claude"}`)

	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "claude", got.HarnessConfig)

	// Mutate applied_config directly (bypassing CreateAgent/UpdateAgent's
	// sync entirely) between the two backfill calls, exactly like an
	// old-binary replica writing a fresh value without updating the column.
	_, err = cs.client.Agent.UpdateOneID(id).
		SetAppliedConfig(`{"harnessConfig":"gemini"}`).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, cs.BackfillHarnessConfigColumn(ctx), "a second call must not error")

	gotAfter, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", gotAfter.HarnessConfig,
		"a row whose harness_config is already non-NULL must not be re-derived by this migration")
}

func TestMigrateRunsHarnessConfigBackfill(t *testing.T) {
	ctx := context.Background()
	cs, projectUID := newHarnessBackfillTestStore(t)

	id := createLegacyAgent(t, cs, projectUID, "via-migrate",
		`{"harnessConfig":"claude"}`)

	require.NoError(t, cs.Migrate(ctx))

	got, err := cs.client.Agent.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.HarnessConfig)

	// Migrate is called again on every boot in production — confirm it
	// stays a cheap no-op that doesn't error for an already-backfilled
	// store.
	require.NoError(t, cs.Migrate(ctx))
}

// TestAgentStore_UpdateAgent_SyncsHarnessConfigColumn is the ptone/scion#2146
// review R4-2 update-path coverage: CreateAgent's sync was already covered
// by TestAgentStore_HarnessConfigFilter*, but no test exercised UpdateAgent
// changing the value, or clearing it when AppliedConfig becomes nil — the
// exact two agent_store.go:441-445 branches (SetHarnessConfig /
// ClearHarnessConfig).
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

	// UpdateAgent with AppliedConfig set to nil must clear the column, not
	// leave the last-known value behind.
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
}
