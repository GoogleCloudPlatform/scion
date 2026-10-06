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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backfillTestProject creates a project for the agent identity key backfill
// tests below and returns its ID.
func backfillTestProject(t *testing.T, cs *CompositeStore, slug string) string {
	t.Helper()
	ctx := context.Background()
	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Backfill Project",
		Slug:    slug,
		Created: time.Now(),
		Updated: time.Now(),
	}))
	return projectID
}

// setAgentCreated forces agentID's created timestamp directly via SQL.
// CreateAgent always sets Created to time.Now() itself, which back-to-back
// calls in the same test can race to the same millisecond, and the ent
// schema marks "created" Immutable (no generated update setter) precisely
// so application code cannot rewrite it after the fact. Going around that
// intentional guard is legitimate in these tests specifically: their whole
// purpose is to pin what happens when two agents' timestamps are in a
// specific, known order, which CreateAgent alone cannot guarantee.
func setAgentCreated(t *testing.T, cs *CompositeStore, agentID string, created time.Time) {
	t.Helper()
	db := cs.DB()
	require.NotNil(t, db)
	_, err := db.ExecContext(context.Background(),
		rebindForDialect(cs.Dialect(), "UPDATE agents SET created = ? WHERE id = ?"), created, agentID)
	require.NoError(t, err)
}

// TestBackfillAgentIdentityKeys_EarlierSlugBeatsLaterDisplayKey is the
// benign-direction collision test: the earlier-created agent's own slug
// collides with the later-created agent's display-name key. This direction
// worked even before the two-pass ordering fix below, because the earlier
// agent's slug is always inserted (as the first key in its own turn) before
// the later agent is ever processed at all. The migration must keep the
// earlier agent's slug key, log and skip the later agent's colliding
// display-name key, and still report success.
func TestBackfillAgentIdentityKeys_EarlierSlugBeatsLaterDisplayKey(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "backfill-collision-project")

	earlierID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: earlierID, Slug: "widget-bot", Name: "widget-bot",
		ProjectID: projectID, Phase: "stopped",
	}))
	laterID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: laterID, Slug: "gadget-bot", Name: "Widget Bot", // display key collides with earlierID's slug
		ProjectID: projectID, Phase: "stopped",
	}))

	base := time.Now().Add(-1 * time.Hour)
	setAgentCreated(t, cs, earlierID, base)
	setAgentCreated(t, cs, laterID, base.Add(time.Minute))

	require.NoError(t, cs.BackfillAgentIdentityKeys(ctx))

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)

	require.NotNil(t, findIdentityKey(keys, earlierID, "widget-bot"),
		"the earlier-created agent must keep the colliding key")
	require.Nil(t, findIdentityKey(keys, laterID, "widget-bot"),
		"the later-created agent's colliding display-name key must be dropped, not inserted")
	require.NotNil(t, findIdentityKey(keys, laterID, "gadget-bot"),
		"the later agent's own, non-colliding slug key must still be inserted")
}

// TestBackfillAgentIdentityKeys_LaterSlugSurvivesEarlierDisplayKey is the
// required reverse-direction collision test, and the one that actually
// exercises the two-pass ordering: the EARLIER-created agent's display-name
// key would equal the LATER-created agent's own slug. A single pass that
// writes each agent's full key set together, in created order, would insert
// the earlier agent's display-name key before the later agent is ever
// processed, permanently orphaning the later (live) agent's own slug --
// which is exactly the class of hole this migration exists to close, not
// reopen. The later agent's slug must survive untouched; only the earlier
// agent's colliding display-name key is dropped.
func TestBackfillAgentIdentityKeys_LaterSlugSurvivesEarlierDisplayKey(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "backfill-reverse-collision-project")

	earlierID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: earlierID, Slug: "early-agent", Name: "Later Agent", // display key collides with laterID's slug
		ProjectID: projectID, Phase: "stopped",
	}))
	laterID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: laterID, Slug: "later-agent", Name: "later-agent",
		ProjectID: projectID, Phase: "stopped",
	}))

	base := time.Now().Add(-1 * time.Hour)
	setAgentCreated(t, cs, earlierID, base)
	setAgentCreated(t, cs, laterID, base.Add(time.Minute))

	require.NoError(t, cs.BackfillAgentIdentityKeys(ctx))

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)

	require.NotNil(t, findIdentityKey(keys, earlierID, "early-agent"),
		"the earlier agent's own, non-colliding slug key must still be inserted")
	require.NotNil(t, findIdentityKey(keys, laterID, "later-agent"),
		"the later agent's own slug key must survive, not be pre-empted by an earlier agent's display-name key")
	require.Nil(t, findIdentityKey(keys, earlierID, "later-agent"),
		"the earlier agent's colliding display-name key must be dropped, not inserted")

	// The later agent must still be usable: its slug key is genuinely
	// reserved, not silently missing.
	renamed, err := cs.GetAgent(ctx, laterID)
	require.NoError(t, err)
	require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, laterID, projectID, []string{renamed.Slug, "totally-unique-key"}),
		"the later agent must still be able to write its own identity keys (e.g. via a rename) "+
			"now that its slug key genuinely exists")
}

// TestBackfillAgentIdentityKeys_DisplayVsDisplayKeepsFirst is the required
// display-vs-display test: two agents with different slugs, neither
// colliding with the other's slug, whose display names resolve to the same
// key. Created-order must still resolve this collision, the same as before
// the two-pass restructuring -- pass two (display-name keys) is still
// processed in created order.
func TestBackfillAgentIdentityKeys_DisplayVsDisplayKeepsFirst(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "backfill-display-vs-display-project")

	earlierID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: earlierID, Slug: "widget-a", Name: "Shared Display Name",
		ProjectID: projectID, Phase: "stopped",
	}))
	laterID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: laterID, Slug: "widget-b", Name: "Shared Display Name",
		ProjectID: projectID, Phase: "stopped",
	}))

	base := time.Now().Add(-1 * time.Hour)
	setAgentCreated(t, cs, earlierID, base)
	setAgentCreated(t, cs, laterID, base.Add(time.Minute))

	require.NoError(t, cs.BackfillAgentIdentityKeys(ctx))

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)

	require.NotNil(t, findIdentityKey(keys, earlierID, "widget-a"), "each agent's own slug key must be present")
	require.NotNil(t, findIdentityKey(keys, laterID, "widget-b"), "each agent's own slug key must be present")
	require.NotNil(t, findIdentityKey(keys, earlierID, "shared-display-name"),
		"the earlier-created agent must keep the shared display-name key")
	require.Nil(t, findIdentityKey(keys, laterID, "shared-display-name"),
		"the later-created agent's colliding display-name key must be dropped, not inserted")
}

// TestBackfillAgentIdentityKeys_EmptyDisplayNameKeySkipped is the required
// empty-name test: a legacy agent whose Name slugifies to "" must not get an
// empty display-name key row (agent_identity_keys.key is NotEmpty; an empty
// insert would be a permanent failure), and the migration must still
// succeed.
func TestBackfillAgentIdentityKeys_EmptyDisplayNameKeySkipped(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "backfill-empty-name-project")

	agentID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "legacy-empty-name", Name: "!!!",
		ProjectID: projectID, Phase: "stopped",
	}))

	require.NoError(t, cs.BackfillAgentIdentityKeys(ctx))

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, keys, 1, "only the slug key may be present; no empty display-name key")
	require.NotNil(t, findIdentityKey(keys, agentID, "legacy-empty-name"))
}

// TestBackfillAgentIdentityKeys_Idempotent is the required idempotency test:
// running the migration twice must not insert anything new, or error, the
// second time.
func TestBackfillAgentIdentityKeys_Idempotent(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "backfill-idempotent-project")

	agentID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "idempotent-agent", Name: "Totally Different Display Name",
		ProjectID: projectID, Phase: "stopped",
	}))

	require.NoError(t, cs.BackfillAgentIdentityKeys(ctx))
	firstRun, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, firstRun, 2, "expected the slug key and the display-name key")

	require.NoError(t, cs.BackfillAgentIdentityKeys(ctx), "a second run must not error")

	secondRun, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, secondRun, 2, "a second run must not insert anything new")
}

// TestBackfillAgentIdentityKeys_IncludesSoftDeletedAgents is the required
// soft-deleted-rows test: a soft-deleted agent's keys must still be
// reserved by the backfill, consistent with the persist-through-soft-delete
// invariant the rest of this feature already enforces.
func TestBackfillAgentIdentityKeys_IncludesSoftDeletedAgents(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "backfill-soft-deleted-project")

	agentID := uuid.New().String()
	a := &store.Agent{
		ID: agentID, Slug: "soft-deleted-agent", Name: "soft-deleted-agent",
		ProjectID: projectID, Phase: "stopped",
	}
	require.NoError(t, cs.CreateAgent(ctx, a))
	a.DeletedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, cs.UpdateAgent(ctx, a))

	require.NoError(t, cs.BackfillAgentIdentityKeys(ctx))

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, agentID, "soft-deleted-agent"),
		"a soft-deleted agent's key must still be backfilled")
}

// TestCompositeStore_Migrate_BackfillsAgentIdentityKeys pins the wiring
// between CompositeStore.Migrate and BackfillAgentIdentityKeys: nothing
// exercises the backfill through the full migration entry point elsewhere,
// so a mutation that simply removed the call from Migrate would pass every
// other test in this package (and pkg/api) without a single failure.
func TestCompositeStore_Migrate_BackfillsAgentIdentityKeys(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "backfill-migrate-wiring-project")

	agentID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "migrate-wired-agent", Name: "migrate-wired-agent",
		ProjectID: projectID, Phase: "stopped",
	}))

	require.NoError(t, cs.Migrate(ctx))

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, agentID, "migrate-wired-agent"),
		"Migrate must backfill this agent's identity key, not just create the table")
}
