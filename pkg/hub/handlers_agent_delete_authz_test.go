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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// deleteReqWithAgent builds a DELETE request carrying agentIdent in its context
// (no user identity), so performAgentDelete's agent-caller branch is exercised.
func deleteReqWithAgent(agentIdent Identity) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/victim", nil)
	return req.WithContext(contextWithIdentity(req.Context(), agentIdent))
}

// TestPerformAgentDelete_AgentCallerScopeAndProjectGate verifies an agent
// caller must hold ScopeAgentLifecycle and target an agent in its own project.
func TestPerformAgentDelete_AgentCallerScopeAndProjectGate(t *testing.T) {
	srv, _, _, project := setupAgentRoleTest(t)
	victim := &store.Agent{ID: tid("victim"), ProjectID: project.ID}

	t.Run("missing lifecycle scope is 403", func(t *testing.T) {
		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			ProjectID: project.ID,
			Scopes:    []AgentTokenScope{ScopeProjectRead}, // readonly/baseline: no lifecycle
		}}
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(agentIdent), victim)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "project:agent:lifecycle")
	})

	t.Run("lifecycle scope but foreign project is 403", func(t *testing.T) {
		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			ProjectID: tid("some-other-project"),
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}}
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(agentIdent), victim)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "within their own project")
	})

	t.Run("no identity at all is 403 (fail closed)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/victim", nil)
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, req, victim)
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
		srv.performAgentDelete(rec, deleteReqWithAgent(lifecycleAgent("")), &store.Agent{ID: tid("victim"), ProjectID: ""})
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "within their own project")
		// Empty token project id against a real-project target.
		rec = httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(lifecycleAgent("")), victim)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("lifecycle scope in the same project passes the gate", func(t *testing.T) {
		// Already-soft-deleted: 204 right after the gate (idempotency), so an
		// authorized agent clears the gate without a live broker.
		deleted := &store.Agent{ID: tid("victim"), ProjectID: project.ID, DeletedAt: time.Now()}
		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			ProjectID: project.ID,
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}}
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, deleteReqWithAgent(agentIdent), deleted)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.NotContains(t, rec.Body.String(), "project:agent:lifecycle")
	})
}
