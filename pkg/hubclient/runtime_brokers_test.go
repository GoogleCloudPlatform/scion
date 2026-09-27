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

package hubclient

import (
	"encoding/json"
	"testing"
)

func TestListBrokerProjectsResponse_MarshalJSON(t *testing.T) {
	resp := ListBrokerProjectsResponse{
		Projects: []BrokerProjectInfo{
			{ProjectID: "p1", ProjectName: "Project 1"},
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := m["projects"]; !ok {
		t.Errorf("Missing 'projects' field")
	}
	if _, ok := m["groves"]; ok {
		t.Errorf("'groves' field should not be emitted, got %v", m["groves"])
	}

	projects := m["projects"].([]interface{})
	if len(projects) != 1 {
		t.Errorf("Expected 1 project, got %d", len(projects))
	}
	if projects[0].(map[string]interface{})["groveId"] != nil {
		t.Errorf("'groveId' field should not be emitted on project entries, got %v", projects[0].(map[string]interface{})["groveId"])
	}
}

func TestListBrokerProjectsResponse_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectsKey", func(t *testing.T) {
		data := `{"projects":[{"projectId":"p1","projectName":"Project 1"}]}`
		var resp ListBrokerProjectsResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if len(resp.Projects) != 1 {
			t.Errorf("Expected 1 project, got %d", len(resp.Projects))
		}
		if resp.Projects[0].ProjectID != "p1" {
			t.Errorf("Expected project ID 'p1', got '%s'", resp.Projects[0].ProjectID)
		}
	})

	t.Run("IgnoresLegacyGrovesKey", func(t *testing.T) {
		data := `{"groves":[{"projectId":"p1","projectName":"Project 1"}]}`
		var resp ListBrokerProjectsResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if len(resp.Projects) != 0 {
			t.Errorf("Projects = %+v, want empty (legacy 'groves' key must not be honored)", resp.Projects)
		}
	})
}

func TestBrokerHeartbeat_MarshalJSON(t *testing.T) {
	hb := BrokerHeartbeat{
		Status: "online",
		Projects: []ProjectHeartbeat{
			{
				ProjectID:  "p1",
				AgentCount: 1,
			},
		},
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := m["projects"]; !ok {
		t.Errorf("Missing 'projects' field")
	}
	if _, ok := m["groves"]; ok {
		t.Errorf("'groves' field should not be emitted, got %v", m["groves"])
	}

	projects := m["projects"].([]interface{})
	if projects[0].(map[string]interface{})["projectId"] != "p1" {
		t.Errorf("Expected projectId 'p1', got %v", projects[0].(map[string]interface{})["projectId"])
	}
	if _, ok := projects[0].(map[string]interface{})["groveId"]; ok {
		t.Errorf("'groveId' field should not be emitted on project entries, got %v", projects[0].(map[string]interface{})["groveId"])
	}
}

func TestBrokerHeartbeat_UnmarshalJSON(t *testing.T) {
	data := `{"status":"online","projects":[{"projectId":"p1","agentCount":2}]}`
	var hb BrokerHeartbeat
	if err := json.Unmarshal([]byte(data), &hb); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if hb.Status != "online" {
		t.Errorf("Expected status 'online', got '%s'", hb.Status)
	}
	if len(hb.Projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(hb.Projects))
	}
	if hb.Projects[0].ProjectID != "p1" {
		t.Errorf("Expected project ID 'p1', got '%s'", hb.Projects[0].ProjectID)
	}
	if hb.Projects[0].AgentCount != 2 {
		t.Errorf("Expected agent count 2, got %d", hb.Projects[0].AgentCount)
	}
}

func TestBrokerHeartbeat_UnmarshalJSON_GrovesKeyIgnored(t *testing.T) {
	// A payload keyed by the removed "groves" name no longer decodes into
	// any project: the field is gone, so encoding/json leaves it unset.
	data := `{"status":"online","groves":[{"projectId":"g1","agentCount":3}]}`
	var hb BrokerHeartbeat
	if err := json.Unmarshal([]byte(data), &hb); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if hb.Status != "online" {
		t.Errorf("Expected status 'online', got '%s'", hb.Status)
	}
	if len(hb.Projects) != 0 {
		t.Errorf("Expected no projects from a groves-keyed payload, got %d", len(hb.Projects))
	}
}

func TestProjectHeartbeat_UnmarshalJSON(t *testing.T) {
	data := `{"projectId":"p1","agentCount":5}`
	var ph ProjectHeartbeat
	if err := json.Unmarshal([]byte(data), &ph); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if ph.ProjectID != "p1" {
		t.Errorf("Expected project ID 'p1', got '%s'", ph.ProjectID)
	}
}

func TestProjectHeartbeat_UnmarshalJSON_GroveIdKeyIgnored(t *testing.T) {
	// The removed "groveId" name must not populate ProjectID: only
	// "projectId" is decoded.
	data := `{"groveId":"p1","agentCount":5}`
	var ph ProjectHeartbeat
	if err := json.Unmarshal([]byte(data), &ph); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if ph.ProjectID != "" {
		t.Errorf("Expected empty project ID (legacy 'groveId' key must not be honored), got '%s'", ph.ProjectID)
	}
}
