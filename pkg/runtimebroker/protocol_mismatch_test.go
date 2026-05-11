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

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestHandleAgentByID_QueryParameters(t *testing.T) {
	agentID := "test-agent-1"
	projectID := "project-123"

	agents := []api.AgentInfo{
		{
			ID:        agentID,
			Slug:      agentID,
			ProjectID: projectID,
			Labels:    map[string]string{"scion.agent": "true"},
		},
	}

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{
			name:       "projectId query param",
			query:      fmt.Sprintf("projectId=%s", projectID),
			wantStatus: http.StatusOK,
		},
		{
			name:       "groveId query param (legacy)",
			query:      fmt.Sprintf("groveId=%s", projectID),
			wantStatus: http.StatusOK,
		},
		{
			name:       "wrong projectId",
			query:      "projectId=wrong",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &protocolMockManager{
				agents: agents,
			}
			// Note: New expects agent.Manager. Config is ServerConfig in server.go
			srv := New(ServerConfig{}, mgr, nil)

			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/agents/%s?%s", agentID, tt.query), nil)
			w := httptest.NewRecorder()

			srv.Handler().ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("expected status %d, got %d: %s", tt.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

type protocolMockManager struct {
	agents []api.AgentInfo
}

func (m *protocolMockManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	return nil, nil
}
func (m *protocolMockManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	return nil, nil
}
func (m *protocolMockManager) Stop(ctx context.Context, agentID string) error {
	return nil
}
func (m *protocolMockManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	return true, nil
}
func (m *protocolMockManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	return m.agents, nil
}
func (m *protocolMockManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	return nil
}
func (m *protocolMockManager) MessageRaw(ctx context.Context, agentID, projectID string, keys string) error {
	return nil
}
func (m *protocolMockManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, nil
}
func (m *protocolMockManager) Close() {}

func TestProjectWorkspaceUploadRequest_JSON(t *testing.T) {
	t.Run("unmarshal legacy groveId", func(t *testing.T) {
		jsonData := `{"groveId": "p1", "storagePath": "/s", "workspacePath": "/w"}`
		var req ProjectWorkspaceUploadRequest
		if err := json.Unmarshal([]byte(jsonData), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "p1" {
			t.Errorf("ProjectID = %q, want %q", req.ProjectID, "p1")
		}
	})

	t.Run("unmarshal projectId", func(t *testing.T) {
		jsonData := `{"projectId": "p1", "storagePath": "/s", "workspacePath": "/w"}`
		var req ProjectWorkspaceUploadRequest
		if err := json.Unmarshal([]byte(jsonData), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "p1" {
			t.Errorf("ProjectID = %q, want %q", req.ProjectID, "p1")
		}
	})
}
