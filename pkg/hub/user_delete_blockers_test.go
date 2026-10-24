// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2143 Phase 1: the user delete refusal lists agents
// and active delegations (409 user_delete_blocked), capped, and GET
// /api/v1/users/{id}/delete-blockers lists the same without deleting. A
// delete that passes deactivates the user's remaining delegator edges.

// userDeleteBlockedBody decodes a 409 user_delete_blocked refusal.
type userDeleteBlockedBody struct {
	Error struct {
		Code    string             `json:"code"`
		Message string             `json:"message"`
		Details userDeleteBlockers `json:"details"`
	} `json:"error"`
}

func decodeUserDeleteBlocked(t *testing.T, rec *httptest.ResponseRecorder) userDeleteBlockedBody {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body userDeleteBlockedBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.Equal(t, ErrCodeUserDeleteBlocked, body.Error.Code)
	return body
}

// createUserEdge records an active delegation edge from the user to the
// delegate in the project scope and returns it (with its ID).
func createUserEdge(t *testing.T, s store.Store, userID, delegateType, delegateID, projectID string) *store.DelegationEdge {
	t.Helper()
	e := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   userID,
		DelegateType:  delegateType,
		DelegateID:    delegateID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          "member",
		Active:        true,
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
	require.NotEmpty(t, e.ID)
	return e
}

func edgeRef(e *store.DelegationEdge) delegatorEdgeRef {
	return delegatorEdgeRef{ID: e.ID, DelegateType: e.DelegateType, DelegateID: e.DelegateID, ScopeType: e.ScopeType, ScopeID: e.ScopeID}
}

// requireEdgeSwept asserts the edge was deactivated by a user delete.
func requireEdgeSwept(t *testing.T, s store.Store, edgeID string) {
	t.Helper()
	got, err := s.GetDelegationEdge(context.Background(), edgeID)
	require.NoError(t, err)
	assert.False(t, got.Active, "the edge is deactivated")
	assert.Equal(t, store.EdgeDeactivationDelegatorDeleted, got.Cause)
	assert.NotEmpty(t, got.OpID)
	assert.NotNil(t, got.At)
}

// requireEdgeActive asserts the edge is active.
func requireEdgeActive(t *testing.T, s store.Store, edgeID string) {
	t.Helper()
	got, err := s.GetDelegationEdge(context.Background(), edgeID)
	require.NoError(t, err)
	assert.True(t, got.Active, "the edge is left active")
}

// getDeleteBlockers calls GET /api/v1/users/{id}/delete-blockers as the dev
// user and decodes a 200 body.
func getDeleteBlockers(t *testing.T, srv *Server, userID string) userDeleteBlockers {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/users/"+userID+"/delete-blockers", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var b userDeleteBlockers
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b), rec.Body.String())
	return b
}

// An active edge from the user to an agent the user does not own blocks the
// delete on its own: 409 user_delete_blocked with the edge in details.edges,
// details.agents empty, and the delegations message (not the agents one).
func TestDeleteUser_EdgeToLiveAgentBlocked(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	erinAgent := createOwnedAgent(t, s, "erin-agent", project.ID, erin.ID)
	edge := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, erinAgent.ID, project.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	body := decodeUserDeleteBlocked(t, rec)
	assert.Equal(t, userHasDelegationsDeleteMessage, body.Error.Message)
	assert.NotEqual(t, userOwnsAgentsDeleteMessage, body.Error.Message)
	assert.Equal(t, []delegatorEdgeRef{edgeRef(edge)}, body.Error.Details.Edges)
	assert.Empty(t, body.Error.Details.Agents)
	assert.False(t, body.Error.Details.Truncated)
	// Empty lists are [] on the wire, not null.
	var raw struct {
		Error struct {
			Details map[string]json.RawMessage `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.JSONEq(t, `[]`, string(raw.Error.Details["agents"]))

	_, err := s.GetUser(ctx, dave.ID)
	require.NoError(t, err, "a refused delete keeps the user")
	requireEdgeActive(t, s, edge.ID)
}

// An edge to one of the user's own agents is not listed a second time: the
// agent is listed, with the agents message and its deletionState field.
func TestDeleteUser_EdgeToOwnAgentListedOnce(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	a := createOwnedAgent(t, s, "dave-agent", project.ID, dave.ID)
	createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, a.ID, project.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	body := decodeUserDeleteBlocked(t, rec)
	assert.Equal(t, userOwnsAgentsDeleteMessage, body.Error.Message)
	assert.Equal(t, []ownedAgentRef{{ID: a.ID, Slug: a.Slug, ProjectID: project.ID, DeletionState: ""}}, body.Error.Details.Agents)
	assert.Empty(t, body.Error.Details.Edges)
	assert.Contains(t, rec.Body.String(), `"deletionState":""`, "deletionState is always present")
}

// An agent whose delete is already under way is listed with its state.
func TestDeleteUser_AgentDeletionStateListed(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	a := createOwnedAgent(t, s, "dave-failed", project.ID, dave.ID)
	seedAgentDeletion(t, s, a.ID, deleteSeed{
		name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError,
	})

	body := decodeUserDeleteBlocked(t, doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil))
	require.Len(t, body.Error.Details.Agents, 1)
	assert.Equal(t, a.ID, body.Error.Details.Agents[0].ID)
	assert.Equal(t, store.DeletionStateFailed, body.Error.Details.Agents[0].DeletionState)
}

// The user's only edge points at a soft-deleted agent: it does not block
// (nothing could be deleted to clear it), the plain delete returns 204 and
// deactivates the edge with delegator_deleted, and the agent cannot be
// restored afterwards, because its guard user is gone.
func TestDeleteUser_EdgeToSoftDeletedAgentSweptAndRestoreRefused(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	gone := softDeletedAgent(t, s, "dave-gone", project.ID, dave.ID, []string{dave.ID})
	edge := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, gone.ID, project.ID)

	b := getDeleteBlockers(t, srv, dave.ID)
	assert.Empty(t, b.Agents)
	assert.Empty(t, b.Edges)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := s.GetUser(ctx, dave.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	requireEdgeSwept(t, s, edge.ID)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+gone.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "the user or agent it belongs to")
	got, err := s.GetAgent(ctx, gone.ID)
	require.NoError(t, err)
	assert.False(t, got.DeletedAt.IsZero(), "a refused restore leaves the agent deleted")
	requireEdgeSwept(t, s, edge.ID)
}

// A user with no agents and no blocking edges is deleted (204). Edges whose
// delegate is gone (an agent or a user without a row) do not block,
// GET delete-blockers lists nothing for the user, and the delete leaves
// those edges delegator_deleted. Another user's edge is not touched.
func TestDeleteUser_DanglingEdgesSwept(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	toGoneAgent := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, tid("agent-purged"), project.ID)
	toGoneUser := createUserEdge(t, s, dave.ID, store.DelegationPrincipalUser, tid("user-purged"), project.ID)
	erinAgent := createOwnedAgent(t, s, "erin-agent", project.ID, erin.ID)
	erinEdge := createUserEdge(t, s, erin.ID, store.DelegationPrincipalAgent, erinAgent.ID, project.ID)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/users/"+dave.ID+"/delete-blockers", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"agents":[],"edges":[],"truncated":false}`, rec.Body.String())

	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := s.GetUser(ctx, dave.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	requireEdgeSwept(t, s, toGoneAgent.ID)
	requireEdgeSwept(t, s, toGoneUser.ID)
	a, err := s.GetDelegationEdge(ctx, toGoneAgent.ID)
	require.NoError(t, err)
	u, err := s.GetDelegationEdge(ctx, toGoneUser.ID)
	require.NoError(t, err)
	assert.Equal(t, a.OpID, u.OpID, "one delete records one operation ID")
	requireEdgeActive(t, s, erinEdge.ID)
}

// An active edge from the user to another user does not block (no
// production path creates one, and only deleting that other user could
// clear it): the delete returns 204 and deactivates it with
// delegator_deleted. GET delete-blockers lists nothing for it.
func TestDeleteUser_EdgeToUserDelegateSwept(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	edge := createUserEdge(t, s, dave.ID, store.DelegationPrincipalUser, erin.ID, project.ID)

	b := getDeleteBlockers(t, srv, dave.ID)
	assert.Empty(t, b.Agents)
	assert.Empty(t, b.Edges)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := s.GetUser(ctx, dave.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	requireEdgeSwept(t, s, edge.ID)
	_, err = s.GetUser(ctx, erin.ID)
	require.NoError(t, err, "the delegate user is not touched")
}

// A legacy active edge from the deleted user to a soft-deleted agent that
// another user owns does not block, and the delete leaves it
// delegator_deleted. A restore by the owner does not bring it back: restore
// reactivates only the edges its soft delete deactivated (cause
// agent_soft_delete under the agent's operation ID).
//   - With no soft-delete operation recorded, the restore succeeds and the
//     edge stays delegator_deleted.
//   - When the soft delete also deactivated an edge from the deleted user,
//     the restore is refused (that delegator is not live), and both edges
//     stay inactive.
//   - When the soft delete deactivated an edge from a live user, the restore
//     succeeds and reactivates that edge only; the deleted user's edge stays
//     delegator_deleted.
func TestDeleteUser_OtherOwnersSoftDeletedAgentEdgeStaysSwept(t *testing.T) {
	t.Run("restore without a soft-delete operation", func(t *testing.T) {
		srv, s, _, _, project := setupDemoPolicyTest(t)
		ctx := context.Background()
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		erin := newActiveMember(t, s, "user-erin", "erin@test.com")
		agent := softDeletedAgent(t, s, "erin-gone", project.ID, erin.ID, []string{erin.ID})
		legacy := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, agent.ID, project.ID)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		requireEdgeSwept(t, s, legacy.ID)

		rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.True(t, got.DeletedAt.IsZero(), "the owner's agent is restored")
		requireEdgeSwept(t, s, legacy.ID)
	})
	t.Run("restore with an edge the soft delete deactivated", func(t *testing.T) {
		srv, s, _, _, project := setupDemoPolicyTest(t)
		ctx := context.Background()
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		erin := newActiveMember(t, s, "user-erin", "erin@test.com")
		agent := softDeletedAgent(t, s, "erin-gone", project.ID, erin.ID, []string{erin.ID})
		opID := tid("soft-delete-op")
		require.NoError(t, s.SetAgentSoftDeleteOpID(ctx, agent.ID, opID))
		at := agent.DeletedAt
		softDeleted := &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalUser, DelegatorID: dave.ID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: agent.ID,
			ScopeType: store.RoleScopeProject, ScopeID: project.ID, Role: "member", Active: false,
			Deactivation: store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &at, OpID: opID},
		}
		require.NoError(t, s.CreateDelegationEdge(ctx, softDeleted))
		legacy := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, agent.ID, project.ID)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		requireEdgeSwept(t, s, legacy.ID)

		rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restore", nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "is not active, so its delegation cannot be restored")
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.False(t, got.DeletedAt.IsZero(), "a refused restore leaves the agent deleted")
		requireEdgeSwept(t, s, legacy.ID)
		e, err := s.GetDelegationEdge(ctx, softDeleted.ID)
		require.NoError(t, err)
		assert.False(t, e.Active)
		assert.Equal(t, store.EdgeDeactivationAgentSoftDelete, e.Cause)
	})
	t.Run("restore with an edge from a live user", func(t *testing.T) {
		srv, s, _, _, project := setupDemoPolicyTest(t)
		ctx := context.Background()
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		erin := newActiveMember(t, s, "user-erin", "erin@test.com")
		agent := softDeletedAgent(t, s, "erin-gone", project.ID, erin.ID, []string{erin.ID})
		opID := tid("soft-delete-op")
		require.NoError(t, s.SetAgentSoftDeleteOpID(ctx, agent.ID, opID))
		at := agent.DeletedAt
		owners := &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalUser, DelegatorID: erin.ID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: agent.ID,
			ScopeType: store.RoleScopeProject, ScopeID: project.ID, Role: "member", Active: false,
			Deactivation: store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &at, OpID: opID},
		}
		require.NoError(t, s.CreateDelegationEdge(ctx, owners))
		legacy := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, agent.ID, project.ID)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		requireEdgeSwept(t, s, legacy.ID)

		rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.True(t, got.DeletedAt.IsZero(), "the owner's agent is restored")
		requireEdgeActive(t, s, owners.ID)
		requireEdgeSwept(t, s, legacy.ID)
	})
}

// edgeHidingStore hides the user-delegator edge list from reads inside a
// transaction while its switch is armed, as if an edge were reactivated by
// a concurrent write after the delete's check read it.
type edgeHidingStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *edgeHidingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !s.fault.Active() {
		return s.Store.WithTx(ctx, fn)
	}
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&edgeHidingTx{Store: tx})
	})
}

type edgeHidingTx struct {
	store.Store
}

func (t *edgeHidingTx) GetDelegationEdgesForDelegator(ctx context.Context, delegatorType, delegatorID string) ([]*store.DelegationEdge, error) {
	if delegatorType == store.DelegationPrincipalUser {
		return nil, nil
	}
	return t.Store.GetDelegationEdgesForDelegator(ctx, delegatorType, delegatorID)
}

// The sweep runs in the delete transaction after the check and catches an
// active edge the check did not see: no active edge survives the user it
// names, even one to a live agent.
func TestDeleteUser_SweepDeactivatesEdgeTheCheckMissed(t *testing.T) {
	srv, s, _, _, project, _, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *edgeHidingStore {
		return &edgeHidingStore{Store: inner, fault: f}
	})
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	erinAgent := createOwnedAgent(t, s, "erin-agent", project.ID, erin.ID)
	edge := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, erinAgent.ID, project.ID)

	fault.Arm()
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireEdgeSwept(t, s, edge.ID)
}

// 51 agents: the refusal lists the first 50 by ID and sets truncated; GET
// delete-blockers returns the same lists.
func TestDeleteUser_BlockingAgentsCapped(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	var ids []string
	for i := 0; i < 51; i++ {
		ids = append(ids, createOwnedAgent(t, s, fmt.Sprintf("dave-%02d", i), project.ID, dave.ID).ID)
	}
	sort.Strings(ids)

	body := decodeUserDeleteBlocked(t, doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil))
	require.Len(t, body.Error.Details.Agents, 50)
	assert.True(t, body.Error.Details.Truncated)
	for i, a := range body.Error.Details.Agents {
		assert.Equal(t, ids[i], a.ID, "listed by ID, first 50")
	}
	assert.Equal(t, body.Error.Details, getDeleteBlockers(t, srv, dave.ID))
}

// The edge list is capped on its own and sets truncated too.
func TestDeleteUser_BlockingEdgesCapped(t *testing.T) {
	prev := userDeleteBlockersLimit
	userDeleteBlockersLimit = 2
	t.Cleanup(func() { userDeleteBlockersLimit = prev })

	srv, s, _, _, project := setupDemoPolicyTest(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	var ids []string
	for _, slug := range []string{"erin-a", "erin-b", "erin-c"} {
		a := createOwnedAgent(t, s, slug, project.ID, erin.ID)
		ids = append(ids, createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, a.ID, project.ID).ID)
	}
	sort.Strings(ids)

	body := decodeUserDeleteBlocked(t, doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil))
	require.Len(t, body.Error.Details.Edges, 2)
	assert.Equal(t, ids[0], body.Error.Details.Edges[0].ID)
	assert.Equal(t, ids[1], body.Error.Details.Edges[1].ID)
	assert.Empty(t, body.Error.Details.Agents)
	assert.True(t, body.Error.Details.Truncated)
}

// GET delete-blockers returns exactly the details of the refusal and
// writes nothing.
func TestUserDeleteBlockers_MatchesRefusal(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	a := createOwnedAgent(t, s, "dave-agent", project.ID, dave.ID)
	erinAgent := createOwnedAgent(t, s, "erin-agent", project.ID, erin.ID)
	edge := createUserEdge(t, s, dave.ID, store.DelegationPrincipalAgent, erinAgent.ID, project.ID)

	got := getDeleteBlockers(t, srv, dave.ID)
	assert.Equal(t, []ownedAgentRef{{ID: a.ID, Slug: a.Slug, ProjectID: project.ID}}, got.Agents)
	assert.Equal(t, []delegatorEdgeRef{edgeRef(edge)}, got.Edges)
	assert.False(t, got.Truncated)
	_, err := s.GetUser(ctx, dave.ID)
	require.NoError(t, err, "the listing deletes nothing")
	requireEdgeActive(t, s, edge.ID)

	body := decodeUserDeleteBlocked(t, doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil))
	assert.Equal(t, got, body.Error.Details)
}

// GET delete-blockers has the same authorization as the delete: a caller
// without user.delete gets 403 (holding user.read is not enough); a caller
// holding user.delete gets the list, and 404 for an unknown user. Any
// method other than GET is 405.
func TestUserDeleteBlockers_RequiresUserDelete(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	target := newActiveMember(t, s, "user-target", "target@test.com")
	reader := bulkInviteOnlyCaller(t, s, "reader", "user.read")
	deleter := bulkInviteOnlyCaller(t, s, "deleter", "user.delete")
	path := "/api/v1/users/" + target.ID + "/delete-blockers"

	rec := doRequestAsUser(t, srv, reader, http.MethodGet, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeForbidden, decodeErrorCode(t, rec.Body.Bytes()))

	rec = doRequestAsUser(t, srv, deleter, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"agents":[],"edges":[],"truncated":false}`, rec.Body.String())

	rec = doRequestAsUser(t, srv, deleter, http.MethodGet, "/api/v1/users/"+tid("user-unknown")+"/delete-blockers", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec = doRequestAsUser(t, srv, deleter, method, path, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s: %s", method, rec.Body.String())
	}
	_, err := s.GetUser(context.Background(), target.ID)
	require.NoError(t, err, "no method on delete-blockers deletes the user")
}

// TestDeleteUser_GuardOrder pins the guard order of DELETE
// /api/v1/users/{id}: in each row before the last, the target also owns an
// agent, so a guard that runs before the blocker check (self-delete, the
// user.delete decision, last super-admin, admin lockout) must answer with
// its own status and code, and the blocker check must answer before the
// last-owner guard. Nothing is deleted in any row.
func TestDeleteUser_GuardOrder(t *testing.T) {
	type setup func(t *testing.T) (srv *Server, s store.Store, actor *store.User, targetID string)
	rows := []struct {
		name       string
		setup      setup
		wantStatus int // 0 means 409
		wantCode   string
		wantMsg    string
	}{
		{
			name: "self-delete before blockers",
			setup: func(t *testing.T) (*Server, store.Store, *store.User, string) {
				srv, s, _, _, project := setupDemoPolicyTest(t)
				dev := getDevUser(t, srv, s)
				createOwnedAgent(t, s, "dev-agent", project.ID, dev.ID)
				return srv, s, nil, dev.ID
			},
			wantCode: ErrCodeConflict,
			wantMsg:  "cannot delete your own account",
		},
		{
			name: "user.delete authz before blockers",
			setup: func(t *testing.T) (*Server, store.Store, *store.User, string) {
				srv, s := testServerNoDevAuth(t)
				target := newActiveMember(t, s, "user-target", "target@test.com")
				actor := bulkInviteOnlyCaller(t, s, "reader", "user.read")
				projectID := pvSeedProject(t, s, "guard-order-authz-project")
				createOwnedAgent(t, s, "target-agent", projectID, target.ID)
				return srv, s, actor, target.ID
			},
			wantStatus: http.StatusForbidden,
			wantCode:   ErrCodeForbidden,
		},
		{
			name: "last super-admin before blockers",
			setup: func(t *testing.T) (*Server, store.Store, *store.User, string) {
				srv, s := testServerNoDevAuth(t)
				ctx := context.Background()
				target := newActiveMember(t, s, "user-last-super", "lastsuper@test.com")
				// The store admits a super-admin binding only from the
				// system reconciler (ensureSuperAdminBinding writes it so).
				ensureSuperAdminBinding(t, s, target.ID)
				// Make the target the only super-admin.
				superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
				require.NoError(t, err)
				system, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeSystem, "")
				require.NoError(t, err)
				for _, b := range system {
					if b.RoleDefinitionID == superRD.ID && b.PrincipalID != target.ID {
						require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
					}
				}
				actor := bulkInviteOnlyCaller(t, s, "deleter", "user.delete")
				projectID := pvSeedProject(t, s, "guard-order-lsa-project")
				createOwnedAgent(t, s, "last-super-agent", projectID, target.ID)
				return srv, s, actor, target.ID
			},
			wantCode: ErrCodeConflict,
			wantMsg:  "cannot delete the last super-admin; promote another user first",
		},
		{
			name: "admin lockout before blockers",
			setup: func(t *testing.T) (*Server, store.Store, *store.User, string) {
				srv, s := testServer(t)
				projectID, users, _ := lockoutProjectAdmins(t, s, "guard-order-lockout", 1)
				createOwnedAgent(t, s, "lockout-agent", projectID, users[0])
				return srv, s, nil, users[0]
			},
			wantCode: ErrCodeConstraintAdminLockout,
		},
		{
			name: "blockers before last owner",
			setup: func(t *testing.T) (*Server, store.Store, *store.User, string) {
				srv, s, _, _, _ := setupDemoPolicyTest(t)
				dave := newActiveMember(t, s, "user-dave", "dave@test.com")
				projectID := pvSeedProject(t, s, "guard-order-owned-project")
				createOwnerBinding(t, s, dave.ID, projectID, nil, nil)
				createOwnedAgent(t, s, "dave-agent", projectID, dave.ID)
				return srv, s, nil, dave.ID
			},
			wantCode: ErrCodeUserDeleteBlocked,
			wantMsg:  userOwnsAgentsDeleteMessage,
		},
		{
			name: "last owner once nothing blocks",
			setup: func(t *testing.T) (*Server, store.Store, *store.User, string) {
				srv, s, _, _, _ := setupDemoPolicyTest(t)
				dave := newActiveMember(t, s, "user-dave", "dave@test.com")
				projectID := pvSeedProject(t, s, "guard-order-owned-project")
				createOwnerBinding(t, s, dave.ID, projectID, nil, nil)
				return srv, s, nil, dave.ID
			},
			wantCode: ErrCodeLastOwner,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			srv, s, actor, targetID := row.setup(t)
			var rec *httptest.ResponseRecorder
			if actor != nil {
				rec = doRequestAsUser(t, srv, actor, http.MethodDelete, "/api/v1/users/"+targetID, nil)
			} else {
				rec = doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+targetID, nil)
			}
			want := row.wantStatus
			if want == 0 {
				want = http.StatusConflict
			}
			require.Equal(t, want, rec.Code, rec.Body.String())
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			assert.Equal(t, row.wantCode, body.Error.Code)
			if row.wantMsg != "" {
				assert.Equal(t, row.wantMsg, body.Error.Message)
			}
			_, err := s.GetUser(context.Background(), targetID)
			require.NoError(t, err, "a refused delete keeps the user")
		})
	}
}

// The deprecated allow-list delete applies the same blocker check (409
// user_delete_blocked with details.edges) and, once nothing blocks, the
// same delegator-edge sweep.
func TestDeprecatedAllowListDelete_EdgeBlockedThenSwept(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	erinAgent := createOwnedAgent(t, s, "erin-agent", project.ID, erin.ID)
	edge := createUserEdge(t, s, carol.ID, store.DelegationPrincipalAgent, erinAgent.ID, project.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	body := decodeUserDeleteBlocked(t, rec)
	assert.Equal(t, userHasDelegationsDeleteMessage, body.Error.Message)
	assert.Equal(t, []delegatorEdgeRef{edgeRef(edge)}, body.Error.Details.Edges)
	_, err := s.GetUser(ctx, carol.ID)
	require.NoError(t, err, "a refused delete keeps the invited user")
	requireEdgeActive(t, s, edge.ID)

	// The agent row goes (the edge is left dangling): nothing blocks.
	require.NoError(t, s.DeleteAgent(ctx, erinAgent.ID))
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = s.GetUser(ctx, carol.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	requireEdgeSwept(t, s, edge.ID)
}

// GET /api/v1/users/{id}/delete-blockers takes no further path segment. A
// request with one (or a trailing slash) answers 404 for every method, and
// does not fall through to the user handlers: a GET does not return the
// user, and a DELETE does not delete it.
func TestUserDeleteBlockers_TrailingSegmentNotFound(t *testing.T) {
	srv, s := testServer(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	base := "/api/v1/users/" + dave.ID + "/delete-blockers"
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base + "/extra"},
		{http.MethodGet, base + "/"},
		{http.MethodGet, base + "/a/b"},
		{http.MethodPost, base + "/extra"},
		{http.MethodDelete, base + "/extra"},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), dave.Email, "%s %s must not return the user", tc.method, tc.path)
	}
	_, err := s.GetUser(context.Background(), dave.ID)
	require.NoError(t, err, "no request below delete-blockers deletes the user")

	rec := doRequest(t, srv, http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
