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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project delete refuses while the project has agents whose runtime
// deletion is not confirmed, unless force is given (ptone/scion#4811).

// projectDeleteConflictBody decodes a 409 project delete refusal.
type projectDeleteConflictBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Agents    []projectAgentRef `json:"agents"`
			Truncated bool              `json:"truncated"`
		} `json:"details"`
	} `json:"error"`
}

func decodeProjectDeleteConflict(t *testing.T, rec *httptest.ResponseRecorder) projectDeleteConflictBody {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body projectDeleteConflictBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeConflict, body.Error.Code)
	return body
}

func TestProjectDelete_LiveAgentRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)
	project, _, agent := setupOnlineBrokerAgent(t, s, "pd-guard-live")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	body := decodeProjectDeleteConflict(t, rec)
	require.Len(t, body.Error.Details.Agents, 1)
	assert.Equal(t, agent.ID, body.Error.Details.Agents[0].ID)
	assert.Equal(t, agent.Slug, body.Error.Details.Agents[0].Slug)
	assert.Empty(t, body.Error.Details.Agents[0].DeletionState)
	assert.False(t, body.Error.Details.Truncated)
	assert.Contains(t, body.Error.Message, "force")

	_, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err, "the project is kept")
	_, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err, "the agent record is kept")
	assert.Equal(t, 0, disp.deleteCalls, "no runtime delete is dispatched")
}

func TestProjectDelete_FailedAgentDeleteRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.SetDispatcher(&deleteDispatcher{})
	project, _, agent := setupOnlineBrokerAgent(t, s, "pd-guard-failed")
	seedAgentDeletion(t, s, agent.ID, deleteSeed{
		name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError,
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	body := decodeProjectDeleteConflict(t, rec)
	require.Len(t, body.Error.Details.Agents, 1)
	assert.Equal(t, agent.ID, body.Error.Details.Agents[0].ID)
	assert.Equal(t, store.DeletionStateFailed, body.Error.Details.Agents[0].DeletionState)

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err, "the failed agent record is kept")
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
}

// A soft-deleted agent was torn down on its runtime before the soft delete
// committed, so it does not block; the cascade removes its record.
func TestProjectDelete_SoftDeletedAgentDoesNotBlock(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)
	project, _, agent := setupOnlineBrokerAgent(t, s, "pd-guard-tomb")
	agent.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the soft-deleted record goes with the project")
	assert.Equal(t, 0, disp.deleteCalls, "a soft-deleted agent is not dispatched again")
}

func TestProjectDelete_ForceDeletesLiveAgents(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)
	project, _, agent := setupOnlineBrokerAgent(t, s, "pd-guard-force")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 1, disp.deleteCalls, "force keeps the best-effort runtime delete")
}

// The refusal list is capped; truncated reports that more agents block.
func TestProjectDelete_RefusalListCapped(t *testing.T) {
	orig := projectDeleteBlockingAgentsLimit
	projectDeleteBlockingAgentsLimit = 1
	t.Cleanup(func() { projectDeleteBlockingAgentsLimit = orig })

	srv, s := testServer(t)
	ctx := context.Background()
	srv.SetDispatcher(&deleteDispatcher{})
	project, broker, _ := setupOnlineBrokerAgent(t, s, "pd-guard-cap")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("pd-guard-cap-2"), Slug: "pd-guard-cap-2", Name: "Cap 2",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
	}))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	body := decodeProjectDeleteConflict(t, rec)
	assert.Len(t, body.Error.Details.Agents, 1)
	assert.True(t, body.Error.Details.Truncated)
}

// An actor without delete authority gets 403, not the agent list.
func TestProjectDelete_GuardRunsAfterAuthorization(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("pd-guard-authz")
	ownerID := tid("pd-guard-authz-own")
	memberID := tid("pd-guard-authz-mem")
	createRS3Project(t, s, projectID, ownerID)
	createRS3UserWithRole(t, s, memberID, memberID+"@test.com", projectID, store.ProjectRoleMember)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("pd-guard-authz-agt"), Slug: "pd-guard-authz-agt", Name: "Authz Agent", ProjectID: projectID,
	}))

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(memberID, memberID+"@test.com", "Member", "member", "web"),
	}
	_, decision := srv.deletionService.Delete(setTestIdentity(ctx, req.Actor), req)
	require.NotNil(t, decision)
	assert.Equal(t, http.StatusForbidden, decision.HTTPStatus)
	assert.Equal(t, ErrCodeProjectDeleteForbidden, decision.DenialCode)
	assert.Nil(t, decision.Details)
}

// The owner calling the service directly gets the same refusal, and Force
// on the request deletes.
func TestProjectDelete_ServiceForce(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("pd-guard-svc")
	ownerID := tid("pd-guard-svc-own")
	createRS3Project(t, s, projectID, ownerID)
	agentID := tid("pd-guard-svc-agt")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "pd-guard-svc-agt", Name: "Svc Agent", ProjectID: projectID,
	}))
	actor := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web")
	sctx := setTestIdentity(ctx, actor)

	_, decision := srv.deletionService.Delete(sctx, ProjectDeleteRequest{ProjectID: projectID, Actor: actor})
	require.NotNil(t, decision)
	assert.Equal(t, http.StatusConflict, decision.HTTPStatus)
	assert.Equal(t, ErrCodeConflict, decision.DenialCode)
	_, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)

	result, decision := srv.deletionService.Delete(sctx, ProjectDeleteRequest{ProjectID: projectID, Actor: actor, Force: true})
	require.Nil(t, decision)
	require.NotNil(t, result)
	_, err = s.GetAgent(ctx, agentID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}
