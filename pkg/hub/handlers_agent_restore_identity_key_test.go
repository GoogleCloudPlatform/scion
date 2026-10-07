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

package hub

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// softDeleteAgent soft-deletes f.target via the ordinary DELETE route,
// forcing the soft-delete branch (SoftDeleteRetention > 0, no force) rather
// than relying on server defaults. Returns once the row is confirmed to
// carry a non-zero DeletedAt.
func softDeleteAgent(t *testing.T, f *projectAgentAuthzFixture) {
	t.Helper()
	f.srv.config.SoftDeleteRetention = 24 * time.Hour

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code,
		"DELETE body: %s", rec.Body.String())

	deleted, err := f.store.GetAgent(context.Background(), f.target.ID)
	require.NoError(t, err)
	require.False(t, deleted.DeletedAt.IsZero(), "expected a soft delete, not a hard delete")
}

// TestRestoreAgent_RestoresAndReassertsOwnKey is the happy-path anchor for
// the restore-path identity-key wiring: restoring a soft-deleted agent
// through the project-scoped route clears DeletedAt and leaves its own slug
// key correctly held.
func TestRestoreAgent_RestoresAndReassertsOwnKey(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	softDeleteAgent(t, f)

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, f.targetPath()+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, "restore body: %s", rec.Body.String())

	restored, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.True(t, restored.DeletedAt.IsZero(), "restore must clear DeletedAt")

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, f.target.ID, f.target.Slug),
		"restore must leave the agent's own slug key held")
}

// TestRestoreAgent_LegacyEmptyDisplayNameKeySkipsEmptyKey is the regression
// anchor for the legacy-tolerance rule: a row whose Name predates
// api.ValidateDisplayName (which rejects an empty Slugify result today, but
// never retroactively validated a pre-existing Name) can have a Name that
// slugifies to "". Restoring it must skip the empty display-name key rather
// than try to write it -- agent_identity_keys.key has a NotEmpty schema
// validator, so writing it would turn every future restore of this agent
// into a permanent failure.
func TestRestoreAgent_LegacyEmptyDisplayNameKeySkipsEmptyKey(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	legacy := &store.Agent{
		ID: uuid.New().String(), Slug: "legacy-agent", Name: "!!!",
		ProjectID: f.project.ID, Phase: string(state.PhaseStopped),
		CreatedBy: f.member.ID, OwnerID: f.member.ID,
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{}},
		DeletedAt:     time.Now().Add(-1 * time.Hour),
	}
	require.NoError(t, f.store.CreateAgent(ctx, legacy))

	restorePath := "/api/v1/projects/" + f.project.ID + "/agents/" + legacy.ID + "/restore"
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, restorePath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "restore body: %s", rec.Body.String())

	restored, err := f.store.GetAgent(ctx, legacy.ID)
	require.NoError(t, err)
	require.True(t, restored.DeletedAt.IsZero(), "restore must clear DeletedAt")

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, legacy.ID, "legacy-agent"), "the slug key must still be asserted")

	count := 0
	for _, k := range keys {
		if k.AgentID == legacy.ID {
			count++
		}
	}
	require.Equal(t, 1, count, "only the slug key may be present; no empty display-name key")
}

// TestRestoreAgent_ReassertsBothSlugAndDisplayNameKeys is the coverage anchor
// for the display-name half of the reassertion: an agent whose display name
// differs from its slug must have BOTH keys (re)asserted on restore, not
// just its own slug. All 8 tests added alongside the original restore fix
// passed even without this reassertion, since none of them used a Name
// distinct from Slug -- this closes that gap.
func TestRestoreAgent_ReassertsBothSlugAndDisplayNameKeys(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	distinct := &store.Agent{
		ID: uuid.New().String(), Slug: "distinct-slug", Name: "Distinct Display Name",
		ProjectID: f.project.ID, Phase: string(state.PhaseStopped),
		CreatedBy: f.member.ID, OwnerID: f.member.ID,
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{}},
		DeletedAt:     time.Now().Add(-1 * time.Hour),
	}
	require.NoError(t, f.store.CreateAgent(ctx, distinct))

	restorePath := "/api/v1/projects/" + f.project.ID + "/agents/" + distinct.ID + "/restore"
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, restorePath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "restore body: %s", rec.Body.String())

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, distinct.ID, "distinct-slug"),
		"the slug key must be (re)asserted")
	require.NotNil(t, findIdentityKey(keys, distinct.ID, "distinct-display-name"),
		"the display-name key must be (re)asserted")
}

// TestRestoreAgent_NonProjectScopedRouteAlsoReassertsKeys confirms the
// non-project-scoped restore route (POST /api/v1/agents/{id}/restore) reaches
// the same guarded restoreAgent implementation as the project-scoped one
// exercised above, rather than a second, independently-wired copy that could
// drift from it.
func TestRestoreAgent_NonProjectScopedRouteAlsoReassertsKeys(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	softDeleteAgent(t, f)

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/agents/"+f.target.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, "restore body: %s", rec.Body.String())

	restored, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.True(t, restored.DeletedAt.IsZero(), "restore must clear DeletedAt")

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, f.target.ID, f.target.Slug),
		"restore must leave the agent's own slug key held")
}

// TestRestoreAgent_UndeleteIntoCollisionIsRejected covers the real window
// identity keys persisting through soft-delete cannot close on their own: an
// agent soft-deleted before the identity-key invariant existed (or before a
// backfill of it) may have no key rows of its own, so nothing stops a
// different, live agent from taking that value as its own display-name key
// in the meantime. Restoring the soft-deleted agent must detect that
// collision and fail, rather than restore an agent whose reasserted key
// collides with the one the live agent now holds.
func TestRestoreAgent_UndeleteIntoCollisionIsRejected(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	softDeleteAgent(t, f)

	// Simulate the pre-existing-invariant gap directly: strip the
	// soft-deleted agent's own key row, as if it had been soft-deleted
	// before this agent ever had one.
	require.NoError(t, f.store.DeleteAgentIdentityKeys(ctx, f.target.ID))

	// A live, differently-slugged agent takes that value as its
	// display-name key -- only possible because the row above is gone. The
	// fixture's Slugs are tid()-generated UUID strings, which Slugify passes
	// through unchanged (already lowercase hex and dashes), so f.target.Slug
	// doubles as a valid display name whose key equals it exactly.
	renameRec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, callerPath(f),
		map[string]interface{}{"name": f.target.Slug})
	require.Equal(t, http.StatusOK, renameRec.Code, "PATCH body: %s", renameRec.Body.String())

	restoreRec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, f.targetPath()+"/restore", nil)
	require.Equal(t, http.StatusConflict, restoreRec.Code, "restore body: %s", restoreRec.Body.String())

	// The rejected restore must not have partially applied.
	stillDeleted, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.False(t, stillDeleted.DeletedAt.IsZero(), "a rejected restore must not clear DeletedAt")
}

// TestRestoreAgent_PurgedAgentReturnsNotFound confirms restore does not
// depend on identity-key rows a hard delete or purge has already removed: an
// agent that no longer exists at all fails at the initial GetAgent lookup,
// the same as it always has, rather than restoreAgent's new identity-key
// reassertion trying to act on a row that is not there.
func TestRestoreAgent_PurgedAgentReturnsNotFound(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	softDeleteAgent(t, f)

	// Simulate a hard delete/purge completing while the agent was
	// soft-deleted -- both the agent row and its identity-key rows are gone.
	require.NoError(t, f.store.DeleteAgent(ctx, f.target.ID))

	restoreRec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, f.targetPath()+"/restore", nil)
	require.Equal(t, http.StatusNotFound, restoreRec.Code, "restore body: %s", restoreRec.Body.String())
}

// TestApplyAgentUpdate_SoftDeletedAgentStillReservesKey proves identity-key
// persistence through soft-delete from the rename side: a live agent cannot
// rename its way into a soft-deleted agent's still-held key, only a hard
// delete or purge frees it. The fixture's Slugs are tid()-generated UUID
// strings, which Slugify passes through unchanged, so f.target.Slug doubles
// as a valid display name whose key equals it exactly.
//
// projectAgentAuthzSetup builds f.target via the plain store.CreateAgent
// (this fixture predates the identity-key work and is shared by many other
// tests), which writes no identity-key row at all -- so its own slug is
// reserved here explicitly first, the same as it would have been had it
// gone through commitAgentCreate at creation.
func TestApplyAgentUpdate_SoftDeletedAgentStillReservesKey(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	require.NoError(t, f.store.ReplaceAgentIdentityKeys(ctx, f.target.ID, f.project.ID, []string{f.target.Slug}))
	softDeleteAgent(t, f)

	renameRec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, callerPath(f),
		map[string]interface{}{"name": f.target.Slug})
	require.Equal(t, http.StatusConflict, renameRec.Code,
		"a soft-deleted agent's key must still block a colliding rename: %s", renameRec.Body.String())
}

// TestCreateAgentInProject_SoftDeletedAgentStillReservesKey proves identity-
// key persistence through soft-delete from the create side. It mirrors
// TestCreateAgentInProject_OrderingCollisionIsRejected (Phase 2's ordering
// regression, where a renamed agent's display-name key blocks a later create
// with the same key) with a soft-delete inserted in between: the
// display-name key must go on blocking the create even after the agent that
// reserved it has been soft-deleted, since only a hard delete or purge frees
// it. The new agent's own Slug ("widget-bot") never collides with the
// soft-deleted agent's Slug ("paa-target"), so this exercises the
// identity-key table's uniqueness specifically, not the agent table's own
// slug/project_id uniqueness.
func TestCreateAgentInProject_SoftDeletedAgentStillReservesKey(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	attachAutoProvideBroker(t, f)
	ctx := context.Background()

	renameRec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": "Widget Bot"})
	require.Equal(t, http.StatusOK, renameRec.Code, "PATCH body: %s", renameRec.Body.String())

	softDeleteAgent(t, f)

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, createAgentPath(f),
		CreateAgentRequest{Name: "Widget Bot"})
	require.Equal(t, http.StatusConflict, rec.Code,
		"a soft-deleted agent's display-name key must still block a colliding create: %s", rec.Body.String())

	_, err := f.store.GetAgentBySlug(ctx, f.project.ID, "widget-bot")
	require.ErrorIs(t, err, store.ErrNotFound, "no new agent may have been created")
}
