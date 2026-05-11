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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestBrokerInfoResponse_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove fields", func(t *testing.T) {
		jsonData := `{
			"brokerId": "b1",
			"version": "1.0",
			"groves": [{"projectId": "p1", "projectName": "Project 1"}]
		}`
		var resp BrokerInfoResponse
		if err := json.Unmarshal([]byte(jsonData), &resp); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if len(resp.Projects) != 1 {
			t.Fatalf("Expected 1 project, got %d", len(resp.Projects))
		}
		if resp.Projects[0].ProjectID != "p1" {
			t.Errorf("ProjectID = %q, want %q", resp.Projects[0].ProjectID, "p1")
		}
	})

	t.Run("marshal dual fields", func(t *testing.T) {
		resp := BrokerInfoResponse{
			BrokerID: "b1",
			Version:  "1.0",
			Projects: []ProjectInfo{
				{ProjectID: "p1", ProjectName: "Project 1"},
			},
		}
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if _, ok := m["projects"]; !ok {
			t.Errorf("Missing 'projects' field")
		}
		if _, ok := m["groves"]; !ok {
			t.Errorf("Missing 'groves' field")
		}
	})
}

func TestProjectInfo_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove fields", func(t *testing.T) {
		jsonData := `{"groveId": "legacy-id", "groveName": "legacy-name"}`
		var info ProjectInfo
		if err := json.Unmarshal([]byte(jsonData), &info); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if info.ProjectID != "legacy-id" {
			t.Errorf("ProjectID = %q, want %q", info.ProjectID, "legacy-id")
		}
		if info.ProjectName != "legacy-name" {
			t.Errorf("ProjectName = %q, want %q", info.ProjectName, "legacy-name")
		}
	})

	t.Run("marshal dual fields", func(t *testing.T) {
		info := ProjectInfo{
			ProjectID:   "my-id",
			ProjectName: "my-name",
		}
		data, err := json.Marshal(info)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "my-id" || m["groveId"] != "my-id" {
			t.Errorf("ID fields mismatch: projectId=%v, groveId=%v", m["projectId"], m["groveId"])
		}
		if m["projectName"] != "my-name" || m["groveName"] != "my-name" {
			t.Errorf("Name fields mismatch: projectName=%v, groveName=%v", m["projectName"], m["groveName"])
		}
	})
}

func TestAgentResponse_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove fields", func(t *testing.T) {
		jsonData := `{"groveId": "legacy-id", "slug": "agent-1", "status": "running"}`
		var resp AgentResponse
		if err := json.Unmarshal([]byte(jsonData), &resp); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if resp.ProjectID != "legacy-id" {
			t.Errorf("ProjectID = %q, want %q", resp.ProjectID, "legacy-id")
		}
	})

	t.Run("marshal dual fields", func(t *testing.T) {
		resp := AgentResponse{
			ProjectID: "my-id",
			Slug:      "agent-1",
			Status:    "running",
		}
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "my-id" || m["groveId"] != "my-id" {
			t.Errorf("ID fields mismatch: projectId=%v, groveId=%v", m["projectId"], m["groveId"])
		}
	})
}

func TestCreateAgentRequest_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove fields", func(t *testing.T) {
		jsonData := `{
			"groveId": "legacy-id",
			"grovePath": "/legacy/path",
			"groveSlug": "legacy-slug",
			"name": "agent-1"
		}`
		var req CreateAgentRequest
		if err := json.Unmarshal([]byte(jsonData), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "legacy-id" {
			t.Errorf("ProjectID = %q, want %q", req.ProjectID, "legacy-id")
		}
		if req.ProjectPath != "/legacy/path" {
			t.Errorf("ProjectPath = %q, want %q", req.ProjectPath, "/legacy/path")
		}
		if req.ProjectSlug != "legacy-slug" {
			t.Errorf("ProjectSlug = %q, want %q", req.ProjectSlug, "legacy-slug")
		}
	})

	t.Run("marshal dual fields", func(t *testing.T) {
		req := CreateAgentRequest{
			ProjectID:   "my-id",
			ProjectPath: "/my/path",
			ProjectSlug: "my-slug",
			Name:        "agent-1",
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "my-id" || m["groveId"] != "my-id" {
			t.Errorf("ID fields mismatch")
		}
		if m["projectPath"] != "/my/path" || m["grovePath"] != "/my/path" {
			t.Errorf("Path fields mismatch")
		}
		if m["projectSlug"] != "my-slug" || m["groveSlug"] != "my-slug" {
			t.Errorf("Slug fields mismatch")
		}
	})
}

func TestMessageRequest_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove fields", func(t *testing.T) {
		jsonData := `{"grove_id": "legacy-id", "message": "hello"}`
		var req MessageRequest
		if err := json.Unmarshal([]byte(jsonData), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "legacy-id" {
			t.Errorf("ProjectID = %q, want %q", req.ProjectID, "legacy-id")
		}
	})

	t.Run("marshal dual fields", func(t *testing.T) {
		req := MessageRequest{
			ProjectID: "my-id",
			Message:   "hello",
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["project_id"] != "my-id" || m["grove_id"] != "my-id" {
			t.Errorf("ID fields mismatch")
		}
	})
}

func TestAgentInfoToResponse(t *testing.T) {
	tests := []struct {
		name             string
		info             api.AgentInfo
		expectedStatus   string
		expectedPhase    string
		expectedActivity string
		expectedReady    bool
	}{
		{
			name: "phase and activity set uses structured path",
			info: api.AgentInfo{
				Name:     "agent-structured",
				Phase:    "running",
				Activity: "thinking",
			},
			expectedStatus:   "thinking",
			expectedPhase:    "running",
			expectedActivity: "thinking",
			expectedReady:    true,
		},
		{
			name: "phase running with no activity uses phase as status",
			info: api.AgentInfo{
				Name:  "agent-phase-only",
				Phase: "running",
			},
			expectedStatus:   "running",
			expectedPhase:    "running",
			expectedActivity: "",
			expectedReady:    true,
		},
		{
			name: "phase stopped clears activity",
			info: api.AgentInfo{
				Name:  "agent-stopped-phase",
				Phase: "stopped",
			},
			expectedStatus:   "stopped",
			expectedPhase:    "stopped",
			expectedActivity: "",
			expectedReady:    false,
		},
		{
			name: "phase with waiting_for_input activity",
			info: api.AgentInfo{
				Name:     "agent-waiting",
				Phase:    "running",
				Activity: "waiting_for_input",
			},
			expectedStatus:   "waiting_for_input",
			expectedPhase:    "running",
			expectedActivity: "waiting_for_input",
			expectedReady:    true,
		},
		{
			name: "phase already set passes through unchanged",
			info: api.AgentInfo{
				Name:            "agent-1",
				Phase:           "running",
				ContainerStatus: "created", // should be ignored
			},
			expectedStatus:   "running",
			expectedPhase:    "running",
			expectedActivity: "",
			expectedReady:    true,
		},
		{
			name: "phase set to non-running value",
			info: api.AgentInfo{
				Name:            "agent-2",
				Phase:           "stopped",
				ContainerStatus: "Up 5 minutes",
			},
			expectedStatus:   "stopped",
			expectedPhase:    "stopped",
			expectedActivity: "",
			expectedReady:    false,
		},
		{
			name: "empty status with container up maps to running",
			info: api.AgentInfo{
				Name:            "agent-3",
				ContainerStatus: "Up 2 hours",
			},
			expectedStatus:   string(state.PhaseRunning),
			expectedPhase:    string(state.PhaseRunning),
			expectedActivity: "",
			expectedReady:    true,
		},
		{
			name: "empty status with container running maps to running",
			info: api.AgentInfo{
				Name:            "agent-4",
				ContainerStatus: "running",
			},
			expectedStatus:   string(state.PhaseRunning),
			expectedPhase:    string(state.PhaseRunning),
			expectedActivity: "",
			expectedReady:    true,
		},
		{
			name: "empty status with container created maps to provisioning",
			info: api.AgentInfo{
				Name:            "agent-5",
				ContainerStatus: "created",
			},
			expectedStatus:   string(state.PhaseProvisioning),
			expectedPhase:    string(state.PhaseProvisioning),
			expectedActivity: "",
			expectedReady:    false,
		},
		{
			name: "empty status with container exited maps to stopped",
			info: api.AgentInfo{
				Name:            "agent-6",
				ContainerStatus: "Exited (0) 5 minutes ago",
			},
			expectedStatus:   string(state.PhaseStopped),
			expectedPhase:    string(state.PhaseStopped),
			expectedActivity: "",
			expectedReady:    false,
		},
		{
			name: "empty status with container stopped maps to stopped",
			info: api.AgentInfo{
				Name:            "agent-7",
				ContainerStatus: "stopped",
			},
			expectedStatus:   string(state.PhaseStopped),
			expectedPhase:    string(state.PhaseStopped),
			expectedActivity: "",
			expectedReady:    false,
		},
		{
			name: "empty status with empty container status maps to created",
			info: api.AgentInfo{
				Name: "agent-8",
			},
			expectedStatus:   string(state.PhaseCreated),
			expectedPhase:    string(state.PhaseCreated),
			expectedActivity: "",
			expectedReady:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := AgentInfoToResponse(tt.info)
			if resp.Status != tt.expectedStatus {
				t.Errorf("Status = %q, want %q", resp.Status, tt.expectedStatus)
			}
			if resp.Phase != tt.expectedPhase {
				t.Errorf("Phase = %q, want %q", resp.Phase, tt.expectedPhase)
			}
			if resp.Activity != tt.expectedActivity {
				t.Errorf("Activity = %q, want %q", resp.Activity, tt.expectedActivity)
			}
			if resp.Ready != tt.expectedReady {
				t.Errorf("Ready = %v, want %v", resp.Ready, tt.expectedReady)
			}
		})
	}
}

func TestAgentInfoToResponseHarnessConfig(t *testing.T) {
	info := api.AgentInfo{
		Name:          "agent-harness",
		Phase:         "running",
		Template:      "default",
		HarnessConfig: "gemini",
	}

	resp := AgentInfoToResponse(info)
	if resp.HarnessConfig != "gemini" {
		t.Errorf("HarnessConfig = %q, want %q", resp.HarnessConfig, "gemini")
	}
	if resp.Template != "default" {
		t.Errorf("Template = %q, want %q", resp.Template, "default")
	}
}

func TestAgentInfoToResponseProfile(t *testing.T) {
	info := api.AgentInfo{
		Name:    "agent-profile",
		Phase:   "running",
		Profile: "docker-dev",
	}

	resp := AgentInfoToResponse(info)
	if resp.Profile != "docker-dev" {
		t.Errorf("Profile = %q, want %q", resp.Profile, "docker-dev")
	}
}
