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
	if _, ok := m["groves"]; !ok {
		t.Errorf("Missing 'groves' field")
	}

	projects := m["projects"].([]interface{})
	groves := m["groves"].([]interface{})

	if len(projects) != 1 || len(groves) != 1 {
		t.Errorf("Expected 1 project/grove, got %d/%d", len(projects), len(groves))
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

	t.Run("HandleGrovesKey", func(t *testing.T) {
		data := `{"groves":[{"projectId":"p1","projectName":"Project 1"}]}`
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
	if _, ok := m["groves"]; !ok {
		t.Errorf("Missing 'groves' field")
	}

	projects := m["projects"].([]interface{})
	if projects[0].(map[string]interface{})["projectId"] != "p1" {
		t.Errorf("Expected projectId 'p1', got %v", projects[0].(map[string]interface{})["projectId"])
	}
	if projects[0].(map[string]interface{})["groveId"] != "p1" {
		t.Errorf("Expected groveId 'p1', got %v", projects[0].(map[string]interface{})["groveId"])
	}
}

func TestBrokerHeartbeat_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectsKey", func(t *testing.T) {
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
	})

	t.Run("HandleGrovesKey", func(t *testing.T) {
		data := `{"status":"online","groves":[{"groveId":"g1","agentCount":3}]}`
		var hb BrokerHeartbeat
		if err := json.Unmarshal([]byte(data), &hb); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if len(hb.Projects) != 1 {
			t.Fatalf("Expected 1 project, got %d", len(hb.Projects))
		}
		if hb.Projects[0].ProjectID != "g1" {
			t.Errorf("Expected project ID 'g1', got '%s'", hb.Projects[0].ProjectID)
		}
		if hb.Projects[0].AgentCount != 3 {
			t.Errorf("Expected agent count 3, got %d", hb.Projects[0].AgentCount)
		}
	})

	t.Run("ProjectsKeyTakesPrecedence", func(t *testing.T) {
		data := `{"status":"online","projects":[{"projectId":"p1","agentCount":1}],"groves":[{"groveId":"g1","agentCount":2}]}`
		var hb BrokerHeartbeat
		if err := json.Unmarshal([]byte(data), &hb); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if len(hb.Projects) != 1 {
			t.Fatalf("Expected 1 project, got %d", len(hb.Projects))
		}
		if hb.Projects[0].ProjectID != "p1" {
			t.Errorf("Expected project ID 'p1' (from projects key), got '%s'", hb.Projects[0].ProjectID)
		}
	})
}

func TestProjectHeartbeat_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectIdKey", func(t *testing.T) {
		data := `{"projectId":"p1","agentCount":5}`
		var ph ProjectHeartbeat
		if err := json.Unmarshal([]byte(data), &ph); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if ph.ProjectID != "p1" {
			t.Errorf("Expected project ID 'p1', got '%s'", ph.ProjectID)
		}
	})

	t.Run("HandleGroveIdKey", func(t *testing.T) {
		data := `{"groveId":"g1","agentCount":3}`
		var ph ProjectHeartbeat
		if err := json.Unmarshal([]byte(data), &ph); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if ph.ProjectID != "g1" {
			t.Errorf("Expected project ID 'g1', got '%s'", ph.ProjectID)
		}
	})

	t.Run("ProjectIdTakesPrecedence", func(t *testing.T) {
		data := `{"projectId":"p1","groveId":"g1","agentCount":1}`
		var ph ProjectHeartbeat
		if err := json.Unmarshal([]byte(data), &ph); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if ph.ProjectID != "p1" {
			t.Errorf("Expected project ID 'p1' (projectId takes precedence), got '%s'", ph.ProjectID)
		}
	})
}
