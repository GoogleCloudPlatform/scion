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

//go:build !no_sqlite && (!hubshard || hubshard_4)

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedOwnedArtifact writes a published artifact owned by kind:ref and homed
// in project straight into the artifact store, and returns its id. The
// owner is a plain string there, so it may name an agent or user that does
// not exist (yet, or any more).
func seedOwnedArtifact(t *testing.T, st artifacts.Store, kind, ref, project string) string {
	t.Helper()
	now := time.Now()
	id := uuid.NewString()
	a := &artifacts.Artifact{ID: id, ScopeKind: artifacts.ScopeKindProject, ScopeRef: project, OwnerKind: kind,
		OwnerRef: ref, Title: "T", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: uuid.NewString(), ArtifactID: id, Seq: 1, Kind: artifacts.VersionKindPublish, EntryPath: "a.md",
		TotalBytes: 1, FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	sum := sha256.Sum256([]byte(id))
	f := artifacts.File{VersionID: v.ID, Path: "a.md", Size: 1, SHA256: hex.EncodeToString(sum[:]), MediaType: "text/markdown"}
	g := artifacts.Grant{ID: uuid.NewString(), ArtifactID: id, SubjectKind: artifacts.SubjectScope, SubjectRef: project,
		Permission: artifacts.GrantRead, CreatedAt: now}
	require.NoError(t, st.CreatePublished(context.Background(), a, v, []artifacts.File{f}, []artifacts.Grant{g}))
	return id
}

func artifactOrphaned(t *testing.T, st artifacts.Store, id string) bool {
	t.Helper()
	a, err := st.GetArtifact(context.Background(), id)
	require.NoError(t, err)
	return a.OrphanedAt != nil
}

// TestArtifactOwnersReconciledAcrossRemovalPaths: an agent can stop
// existing four ways: a soft delete and a hard delete through the delete
// engine, PurgeDeletedAgents, and a project delete. The last two run no
// agent lifecycle hook. After each, one maintenance pass marks the agent's
// artifacts orphaned; a live agent's are left alone; restoring the
// soft-deleted agent clears the mark on the next pass. A deleted user's
// artifacts are marked too. Nothing else about the artifacts changes.
func TestArtifactOwnersReconciledAcrossRemovalPaths(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	ctx := context.Background()

	// 1. Hard delete through the engine (no soft-delete retention).
	srv.config.SoftDeleteRetention = 0
	hard := setupBrokerAgentInPhase(t, s, "orph-hard", state.PhaseStopped)
	hardArt := seedOwnedArtifact(t, st, artifacts.PrincipalKindAgent, hard.ID, hard.ProjectID)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+hard.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := s.GetAgent(ctx, hard.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "hard delete removed the row")

	// 2. PurgeDeletedAgents: a soft-deleted row removed at the store level.
	srv.config.SoftDeleteRetention = time.Hour
	purged := setupBrokerAgentInPhase(t, s, "orph-purged", state.PhaseStopped)
	purgedArt := seedOwnedArtifact(t, st, artifacts.PrincipalKindAgent, purged.ID, purged.ProjectID)
	softDeleteForTest(t, srv, purged.ID)
	n, err := s.PurgeDeletedAgents(ctx, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)
	_, err = s.GetAgent(ctx, purged.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the purge removed the row")

	// 3. Project delete: the store cascade removes the project's agents.
	cascaded := setupBrokerAgentInPhase(t, s, "orph-cascade", state.PhaseStopped)
	cascadedArt := seedOwnedArtifact(t, st, artifacts.PrincipalKindAgent, cascaded.ID, cascaded.ProjectID)
	require.NoError(t, s.DeleteProject(ctx, cascaded.ProjectID))
	_, err = s.GetAgent(ctx, cascaded.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the project delete removed the agent")

	// 4. Soft delete through the engine (after the purge, so it survives).
	// setupBrokerAgentInPhase names the agent from its suffix, so the
	// artifact can be written before the agent is deleted.
	softID := tid("agent-lc-resume-orph-soft")
	softArt := seedOwnedArtifact(t, st, artifacts.PrincipalKindAgent, softID, tid("proj-lc-resume-orph-soft"))
	soft, _ := softDeletedWithEdge(t, srv, s, "orph-soft", store.DelegationPrincipalUser, tid("orph-soft-delegator"))
	require.Equal(t, softID, soft.ID)

	// A live agent and an active user, and a user that is then deleted.
	live := setupBrokerAgentInPhase(t, s, "orph-live", state.PhaseStopped)
	liveArt := seedOwnedArtifact(t, st, artifacts.PrincipalKindAgent, live.ID, live.ProjectID)
	createTestUserWithProjectRole(t, s, tid("orph-user-live"), "orph-user-live@test.com", live.ProjectID, store.ProjectRoleMember)
	liveUserArt := seedOwnedArtifact(t, st, artifacts.PrincipalKindUser, tid("orph-user-live"), live.ProjectID)
	createTestUserWithProjectRole(t, s, tid("orph-user-gone"), "orph-user-gone@test.com", live.ProjectID, store.ProjectRoleMember)
	goneUserArt := seedOwnedArtifact(t, st, artifacts.PrincipalKindUser, tid("orph-user-gone"), live.ProjectID)
	require.NoError(t, s.DeleteUser(ctx, tid("orph-user-gone")))

	before, err := st.GetArtifact(ctx, hardArt)
	require.NoError(t, err)

	srv.reapArtifactVersions(ctx)

	for name, id := range map[string]string{"hard delete": hardArt, "purge": purgedArt, "project delete": cascadedArt,
		"soft delete": softArt, "deleted user": goneUserArt} {
		assert.True(t, artifactOrphaned(t, st, id), "%s: the artifact is marked after one pass", name)
	}
	assert.False(t, artifactOrphaned(t, st, liveArt), "a live agent's artifact is not marked")
	assert.False(t, artifactOrphaned(t, st, liveUserArt), "an active user's artifact is not marked")
	after, err := st.GetArtifact(ctx, hardArt)
	require.NoError(t, err)
	after.OrphanedAt = nil
	assert.Equal(t, before, after, "only orphaned_at changes")

	// Restore the soft-deleted agent: the next pass clears its mark.
	loaded := mustGetAgent(t, s, soft.ID)
	require.NoError(t, srv.restoreAgentTx(ctx, loaded, AuditActor{}))
	srv.reapArtifactVersions(ctx)
	assert.False(t, artifactOrphaned(t, st, softArt), "restoring the agent clears the mark")
	assert.True(t, artifactOrphaned(t, st, hardArt), "other orphans stay marked")

	evs, err := st.ListEvents(ctx, softArt)
	require.NoError(t, err)
	var actions []string
	for _, e := range evs {
		actions = append(actions, e.Action)
		assert.Equal(t, artifacts.ActorKindSystem, e.ActorKind)
	}
	assert.Equal(t, []string{artifacts.EventOrphaned, artifacts.EventUnorphaned}, actions)
}

// TestArtifactDeleteOfOrphanOnRoutes: through the real hub, an admin of
// the home project deletes an artifact owned by a deleted agent; a member
// who is not an admin gets 403, a user outside the project the missing
// artifact's 404, and an agent of the project 403. The deleted artifact
// then answers 404 and its delete is recorded.
func TestArtifactDeleteOfOrphanOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p := artifactProject(t, s, "orphan-del")
	other := artifactProject(t, s, "orphan-del-other")
	createTestUserWithProjectRole(t, s, tid("orphan-admin"), "orphan-admin@test.com", p.ID, store.ProjectRoleAdmin)
	createTestUserWithProjectRole(t, s, tid("orphan-member"), "orphan-member@test.com", p.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("orphan-outsider"), "orphan-outsider@test.com", other.ID, store.ProjectRoleMember)
	admin, err := s.GetUser(ctx, tid("orphan-admin"))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, tid("orphan-member"))
	require.NoError(t, err)
	outsider, err := s.GetUser(ctx, tid("orphan-outsider"))
	require.NoError(t, err)
	_, agentTok := artifactAgent(t, srv, s, p.ID, "orphan-del-agent", AgentRoleBaseline)

	id := seedOwnedArtifact(t, st, artifacts.PrincipalKindAgent, tid("orphan-del-gone-agent"), p.ID)
	srv.reapArtifactVersions(ctx)
	require.True(t, artifactOrphaned(t, st, id), "the never-existing agent counts as gone")
	path := "/api/v1/artifacts/" + id

	rec := userArtifactRequest(t, srv, member, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"ownerDeleted":true`)

	memberRec := userArtifactRequest(t, srv, member, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusForbidden, memberRec.Code, memberRec.Body.String())
	agentRec := doRawAgentRequest(t, srv, http.MethodDelete, path, nil, agentTok)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, agentRec.Code, agentRec.Body.String())
	if agentRec.Code == http.StatusForbidden {
		assert.Equal(t, memberRec.Body.String(), agentRec.Body.String(), "every 403 has the same body")
	}
	missing := userArtifactRequest(t, srv, outsider, http.MethodDelete, "/api/v1/artifacts/"+uuid.NewString(), nil)
	rec = userArtifactRequest(t, srv, outsider, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, missing.Body.String(), rec.Body.String(), "a non-reader gets the missing artifact's 404")

	rec = userArtifactRequest(t, srv, admin, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusNotFound, userArtifactRequest(t, srv, admin, http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusNotFound, userArtifactRequest(t, srv, member, http.MethodGet, path, nil).Code)

	evs, err := st.ListEvents(ctx, id)
	require.NoError(t, err)
	var deleted []artifacts.Event
	for _, e := range evs {
		if e.Action == artifacts.EventDeleted {
			deleted = append(deleted, e)
		}
	}
	require.Len(t, deleted, 1)
	assert.Equal(t, artifacts.PrincipalKindUser, deleted[0].ActorKind)
	assert.Equal(t, admin.ID, deleted[0].ActorRef)
	assert.Equal(t, artifacts.AuthorityCustodianProjectAdmin, deleted[0].Authority)
}

// TestArtifactHostPrincipalsExist: the hub answers liveness from its agent
// and user tables: a live agent and an active user are live; a
// soft-deleted or missing agent and a suspended or missing user are not; a
// ref of another kind or with a malformed id is left out (counts as live).
func TestArtifactHostPrincipalsExist(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	ctx := context.Background()
	srv.config.SoftDeleteRetention = time.Hour
	live := setupBrokerAgentInPhase(t, s, "px-live", state.PhaseStopped)
	soft := setupBrokerAgentInPhase(t, s, "px-soft", state.PhaseStopped)
	softDeleteForTest(t, srv, soft.ID)
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: tid("px-active"), Email: "px-active@test.com", DisplayName: "a",
		Role: "member", Status: store.UserStatusActive}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: tid("px-suspended"), Email: "px-suspended@test.com", DisplayName: "s",
		Role: "member", Status: store.UserStatusSuspended}))

	agentRef := func(id string) string { return artifacts.PrincipalRef(artifacts.PrincipalKindAgent, id) }
	userRef := func(id string) string { return artifacts.PrincipalRef(artifacts.PrincipalKindUser, id) }
	refs := []string{agentRef(live.ID), agentRef(soft.ID), agentRef(tid("px-missing-agent")),
		userRef(tid("px-active")), userRef(tid("px-suspended")), userRef(tid("px-missing-user")),
		agentRef("not-a-uuid"), "group:" + tid("px-group"), "no-kind"}
	got, err := newArtifactHost(srv).PrincipalsExist(ctx, refs)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		agentRef(live.ID): true, agentRef(soft.ID): false, agentRef(tid("px-missing-agent")): false,
		userRef(tid("px-active")): true, userRef(tid("px-suspended")): false, userRef(tid("px-missing-user")): false,
	}, got)
}
