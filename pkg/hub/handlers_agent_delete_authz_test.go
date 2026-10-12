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
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteReqWithAgent builds a DELETE request carrying agentIdent in its context
// (no user identity), so performAgentDelete's agent-caller branch is exercised.
func deleteReqWithAgent(agentIdent Identity) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/target", nil)
	return req.WithContext(contextWithIdentity(req.Context(), agentIdent))
}

// TestPerformAgentDelete_AgentCallerScopeAndProjectGate verifies an agent
// caller must hold ScopeAgentLifecycle and target an agent in its own project.
func TestPerformAgentDelete_AgentCallerScopeAndProjectGate(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)
	target := &store.Agent{ID: tid("target"), ProjectID: project.ID}

	t.Run("missing lifecycle scope is 403", func(t *testing.T) {
		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			ProjectID: project.ID,
			Scopes:    []AgentTokenScope{ScopeProjectRead}, // readonly/baseline: no lifecycle
		}}
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(agentIdent), target)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "project:agent:lifecycle")
	})

	t.Run("lifecycle scope but foreign project is 403", func(t *testing.T) {
		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			ProjectID: tid("some-other-project"),
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}}
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(agentIdent), target)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "within their own project")
	})

	t.Run("no identity at all is 403 (fail closed)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/target", nil)
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, req, target)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("empty project id on either side never authorizes", func(t *testing.T) {
		// Two empty strings compare equal, so an empty project id must be
		// rejected outright rather than allowed to match.
		lifecycleAgent := func(pid string) Identity {
			return &agentIdentityWrapper{&AgentTokenClaims{ProjectID: pid, Scopes: []AgentTokenScope{ScopeAgentLifecycle}}}
		}
		// Empty token project id against an empty-project target.
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(lifecycleAgent("")), &store.Agent{ID: tid("target"), ProjectID: ""})
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "within their own project")
		// Empty token project id against a real-project target.
		rec = httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(lifecycleAgent("")), target)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("lifecycle scope in the same project passes the gate", func(t *testing.T) {
		// The caller is a stored agent with a live delegation chain from the
		// project owner and is an ancestor of the target. Already-soft-deleted
		// target: 204 right after the gate (idempotency), so an authorized
		// agent clears the gate without a live broker.
		callerID := tid("delete-gate-caller")
		createDCAgent(t, s, callerID, project.ID, user.ID, AgentRoleFull)
		createDCEdge(t, s, store.DelegationPrincipalUser, user.ID, store.DelegationPrincipalAgent, callerID,
			store.RoleScopeProject, project.ID, string(AgentRoleFull))
		deleted := &store.Agent{
			ID: tid("target"), ProjectID: project.ID, OwnerID: user.ID,
			Ancestry: []string{user.ID, callerID}, DeletedAt: time.Now(),
		}
		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: callerID},
			ProjectID: project.ID,
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}}
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(agentIdent), deleted)
		assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "project:agent:lifecycle")
	})

	// Runs last: it records the edge backfill marker, after which every
	// agent caller needs a delegation edge.
	t.Run("lifecycle scope without a delegation edge after backfill is 403", func(t *testing.T) {
		_, err := s.UpsertHubSetting(context.Background(), "migration_delegation_edge_backfill_v1",
			json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
		require.NoError(t, err)
		deleted := &store.Agent{ID: tid("target"), ProjectID: project.ID, DeletedAt: time.Now()}
		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: tid("delete-gate-unknown")},
			ProjectID: project.ID,
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}}
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(agentIdent), deleted)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), agentTargetDenyMessage)
	})
}
