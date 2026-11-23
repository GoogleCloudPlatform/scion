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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestAgentHubInfo(t *testing.T) {
	srv, s := testServer(t)
	srv.SetHubID("hub-agent-endpoint-test")
	ctx := context.Background()

	projectID := tid("project-agent-hub-info")
	project := &store.Project{
		ID: projectID, Name: "Agent Hub Info Project", Slug: "agent-hub-info-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	userID := tid("user-agent-hub-info")
	createDCUser(t, s, userID, "owner-agent-hub-info@test.com", projectID, store.ProjectRoleOwner)

	agentID := tid("agent-hub-info-1")
	agent := &store.Agent{
		ID: agentID, Slug: "hub-info-agent", Name: "Hub Info Agent",
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Ancestry: []string{userID},
		Created:  time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken := srv.generateRecordedAgentToken(t, agentID, projectID, []AgentTokenScope{ScopeAgentStatusUpdate}, []string{userID})

	t.Run("valid agent token via X-Scion-Agent-Token", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agent/hub", nil, agentToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp AgentHubInfoResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.HubID != "hub-agent-endpoint-test" {
			t.Fatalf("hub_id = %q, want %q", resp.HubID, "hub-agent-endpoint-test")
		}
	})

	t.Run("valid agent token via Authorization Bearer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/hub", nil)
		req.Header.Set("Authorization", "Bearer "+agentToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp AgentHubInfoResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.HubID != "hub-agent-endpoint-test" {
			t.Fatalf("hub_id = %q, want %q", resp.HubID, "hub-agent-endpoint-test")
		}
	})

	t.Run("missing credential returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/hub", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid agent token returns 401", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agent/hub", nil, "not-a-valid-jwt")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-agent user caller returns 403 agents_only", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/agent/hub", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp ErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
			t.Fatalf("failed to decode error response: %v", err)
		}
		if errResp.Error.Code != ErrCodeAgentsOnly {
			t.Fatalf("error.code = %q, want %q", errResp.Error.Code, ErrCodeAgentsOnly)
		}
	})

	t.Run("POST returns 405", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/hub", nil, agentToken)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}
