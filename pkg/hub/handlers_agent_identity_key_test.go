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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// findIdentityKey returns the row in keys with the given agentID and key
// value, or nil if there is none.
func findIdentityKey(keys []*store.AgentIdentityKey, agentID, key string) *store.AgentIdentityKey {
	for _, k := range keys {
		if k.AgentID == agentID && k.Key == key {
			return k
		}
	}
	return nil
}

func callerPath(f *projectAgentAuthzFixture) string {
	return "/api/v1/projects/" + f.project.ID + "/agents/" + f.caller.ID
}

// TestApplyAgentUpdate_DisplayNameCollisionIsRejected is the Phase 1
// vertical-slice regression test for the display-vs-display collision case:
// two agents in the same project cannot hold the same display-name key.
// f.caller is renamed first (reserving its key), then renaming f.target to
// the same display name must be rejected with 409, and f.target's own row
// must not have been written.
func TestApplyAgentUpdate_DisplayNameCollisionIsRejected(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, callerPath(f),
		map[string]interface{}{"name": "Robot Two"})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": "Robot Two"})
	require.Equal(t, http.StatusConflict, rec.Code, "PATCH body: %s", rec.Body.String())

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, f.caller.ID, "robot-two"),
		"f.caller's own key row must exist")
	require.Nil(t, findIdentityKey(keys, f.target.ID, "robot-two"),
		"f.target must not have reserved the colliding key")

	got, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.NotEqual(t, "Robot Two", got.Name, "the rejected rename must not have been persisted")
}

// TestApplyAgentUpdate_ValidRenameWritesIdentityKeyRow is the Phase 1
// vertical-slice regression test for the success path: a non-colliding
// rename succeeds and writes an identity-key row for the new display name.
func TestApplyAgentUpdate_ValidRenameWritesIdentityKeyRow(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": "Builder Prime"})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())

	got, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.Equal(t, "Builder Prime", got.Name)

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, f.target.ID, "builder-prime"),
		"expected an identity-key row for the new display name")
	require.NotNil(t, findIdentityKey(keys, f.target.ID, f.target.Slug),
		"expected the agent's slug to also be reserved as an identity key")
}

// TestApplyAgentUpdate_DisplayNameEqualToOwnSlugIsSingleRow is the Phase 1
// vertical-slice regression test for the no-self-collision case: a display
// name whose key equals the agent's own Slug must not be treated as a
// collision against itself, and distinct({slug, key}) must collapse to one
// row rather than two.
func TestApplyAgentUpdate_DisplayNameEqualToOwnSlugIsSingleRow(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	// f.target.Slug is already a valid display-name key on its own (a
	// lowercase hex-and-dash string), so patching Name to the Slug value
	// itself produces newDisplayNameKey == agent.Slug.
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": f.target.Slug})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())

	got, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.Equal(t, f.target.Slug, got.Name)

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	var ownRows []*store.AgentIdentityKey
	for _, k := range keys {
		if k.AgentID == f.target.ID {
			ownRows = append(ownRows, k)
		}
	}
	require.Len(t, ownRows, 1, "a display name equal to the agent's own slug must collapse to a single row, not a self-collision")
	require.Equal(t, f.target.Slug, ownRows[0].Key)
}

// TestApplyAgentUpdate_HardDeleteFreesIdentityKeys is the regression test for
// the identity-key cascade on agent deletion: hard-deleting an agent must
// free every identity-key row it held, both its own Slug and any
// display-name key, so a later agent recreated with the same Slug can be
// renamed at all, and any other agent can take the freed display name.
func TestApplyAgentUpdate_HardDeleteFreesIdentityKeys(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	originalSlug := f.target.Slug

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": "Worker Bee"})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())

	delRec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, delRec.Code,
		"DELETE body: %s", delRec.Body.String())

	// Confirm this was a hard delete (the row is actually gone), not a soft
	// delete that would leave the identity-key rows legitimately reserved.
	_, err := f.store.GetAgent(ctx, f.target.ID)
	require.True(t, errors.Is(err, store.ErrNotFound), "expected the deleted agent to be gone, got: %v", err)

	keysAfterDelete, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.Nil(t, findIdentityKey(keysAfterDelete, f.target.ID, originalSlug),
		"the deleted agent's own slug key must not still be reserved")
	require.Nil(t, findIdentityKey(keysAfterDelete, f.target.ID, "worker-bee"),
		"the deleted agent's display-name key must not still be reserved")

	// Recreate an agent with the same Slug the deleted agent held, then
	// confirm it can be renamed -- this fails with 409 if the dead agent's
	// own slug key is still reserved.
	recreated := &store.Agent{
		ID: tid("recreated-same-slug"), Slug: originalSlug, Name: originalSlug,
		ProjectID: f.project.ID, Phase: string(state.PhaseStopped),
		CreatedBy: f.member.ID, OwnerID: f.member.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{},
		},
	}
	require.NoError(t, f.store.CreateAgent(ctx, recreated))
	recreatedPath := "/api/v1/projects/" + f.project.ID + "/agents/" + recreated.ID

	rec = doRequestAsUser(t, f.srv, f.member, http.MethodPatch, recreatedPath,
		map[string]interface{}{"name": "Totally Unique Name"})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())

	// A different, unrelated agent can take the freed display name.
	rec = doRequestAsUser(t, f.srv, f.member, http.MethodPatch, callerPath(f),
		map[string]interface{}{"name": "Worker Bee"})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())
}

// TestApplyAgentUpdate_RejectsInvalidDisplayNameCharset is the handler-level
// regression anchor for applyAgentUpdate actually calling ValidateDisplayName
// on the PATCHed Name, not just persisting it verbatim: a small-capital A is
// a rune Slugify folds to nothing on its own (see api.ValidateDisplayName),
// so this PATCH must be rejected with 400, and the rejected value must not
// have reached the store as a Name or an identity-key row.
func TestApplyAgentUpdate_RejectsInvalidDisplayNameCharset(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	originalName := f.target.Name

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": "ᴀdmin"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "PATCH body: %s", rec.Body.String())

	got, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.Equal(t, originalName, got.Name, "the rejected Name must not have been persisted")

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.Nil(t, findIdentityKey(keys, f.target.ID, "dmin"),
		"no identity-key row must have been written for the rejected PATCH")
}

// TestDeleteProject_FreesIdentityKeys is the regression test for the
// DeleteProject identity-key bulk-delete: hard-deleting a project must free
// every identity-key row belonging to its agents, not just the ones freed by
// DeleteAgent.
func TestDeleteProject_FreesIdentityKeys(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": "Worker Bee"})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())
	before, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotEmpty(t, before)
	require.NoError(t, f.store.DeleteProject(ctx, f.project.ID))
	after, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.Empty(t, after, "identity-key rows left after DeleteProject: %d", len(after))
}
