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
