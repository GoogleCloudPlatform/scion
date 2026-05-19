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

func TestCreateTemplateRequest_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectIdKey", func(t *testing.T) {
		data := `{"name":"tmpl","scope":"project","projectId":"p1"}`
		var req CreateTemplateRequest
		if err := json.Unmarshal([]byte(data), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "p1" {
			t.Errorf("Expected project ID 'p1', got '%s'", req.ProjectID)
		}
	})

	t.Run("HandleGroveIdKey", func(t *testing.T) {
		data := `{"name":"tmpl","scope":"project","groveId":"g1"}`
		var req CreateTemplateRequest
		if err := json.Unmarshal([]byte(data), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "g1" {
			t.Errorf("Expected project ID 'g1', got '%s'", req.ProjectID)
		}
	})
}

func TestCreateTemplateRequest_MarshalJSON(t *testing.T) {
	req := CreateTemplateRequest{
		Name:      "tmpl",
		Scope:     "project",
		ProjectID: "p1",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if m["projectId"] != "p1" {
		t.Errorf("Expected projectId 'p1', got %v", m["projectId"])
	}
	if m["groveId"] != "p1" {
		t.Errorf("Expected groveId 'p1', got %v", m["groveId"])
	}
}

func TestCloneTemplateRequest_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectIdKey", func(t *testing.T) {
		data := `{"name":"clone","scope":"project","projectId":"p1"}`
		var req CloneTemplateRequest
		if err := json.Unmarshal([]byte(data), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "p1" {
			t.Errorf("Expected project ID 'p1', got '%s'", req.ProjectID)
		}
	})

	t.Run("HandleGroveIdKey", func(t *testing.T) {
		data := `{"name":"clone","scope":"project","groveId":"g1"}`
		var req CloneTemplateRequest
		if err := json.Unmarshal([]byte(data), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "g1" {
			t.Errorf("Expected project ID 'g1', got '%s'", req.ProjectID)
		}
	})

	t.Run("ProjectIdTakesPrecedence", func(t *testing.T) {
		data := `{"name":"clone","scope":"project","projectId":"p1","groveId":"g1"}`
		var req CloneTemplateRequest
		if err := json.Unmarshal([]byte(data), &req); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if req.ProjectID != "p1" {
			t.Errorf("Expected project ID 'p1' (projectId takes precedence), got '%s'", req.ProjectID)
		}
	})
}

func TestCloneTemplateRequest_MarshalJSON(t *testing.T) {
	req := CloneTemplateRequest{
		Name:      "clone",
		Scope:     "project",
		ProjectID: "p1",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if m["projectId"] != "p1" {
		t.Errorf("Expected projectId 'p1', got %v", m["projectId"])
	}
	if m["groveId"] != "p1" {
		t.Errorf("Expected groveId 'p1', got %v", m["groveId"])
	}
}
