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

func TestProjectCacheRefreshResponse_JSON(t *testing.T) {
	t.Run("unmarshal legacy groveId field", func(t *testing.T) {
		jsonData := `{"groveId": "p1"}`
		var r ProjectCacheRefreshResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if r.ProjectID != "p1" {
			t.Errorf("ProjectID = %q, want %q", r.ProjectID, "p1")
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		r := ProjectCacheRefreshResponse{ProjectID: "p1"}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "p1" {
			t.Errorf("projectId = %v, want %q", m["projectId"], "p1")
		}
		if _, ok := m["groveId"]; ok {
			t.Errorf("legacy 'groveId' key present in marshal output, want absent: %v", m["groveId"])
		}
	})
}

func TestProjectCacheStatusResponse_JSON(t *testing.T) {
	t.Run("unmarshal legacy groveId field", func(t *testing.T) {
		jsonData := `{"groveId": "p1"}`
		var r ProjectCacheStatusResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if r.ProjectID != "p1" {
			t.Errorf("ProjectID = %q, want %q", r.ProjectID, "p1")
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		r := ProjectCacheStatusResponse{ProjectID: "p1"}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "p1" {
			t.Errorf("projectId = %v, want %q", m["projectId"], "p1")
		}
		if _, ok := m["groveId"]; ok {
			t.Errorf("legacy 'groveId' key present in marshal output, want absent: %v", m["groveId"])
		}
	})
}

func TestRegisterProjectResponse_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove field", func(t *testing.T) {
		jsonData := `{"grove": {"id": "p1", "name": "Project 1"}, "created": true}`
		var r RegisterProjectResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if r.Project == nil || r.Project.ID != "p1" {
			t.Errorf("Project = %+v, want ID p1", r.Project)
		}
	})

	t.Run("unmarshal project takes precedence", func(t *testing.T) {
		jsonData := `{"project": {"id": "p1"}, "grove": {"id": "p2"}, "created": true}`
		var r RegisterProjectResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if r.Project == nil || r.Project.ID != "p1" {
			t.Errorf("Project = %+v, want ID p1 (project should win)", r.Project)
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		r := RegisterProjectResponse{
			Project: &Project{ID: "p1", Name: "Project 1"},
			Created: true,
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if _, ok := m["project"]; !ok {
			t.Errorf("Missing 'project' field")
		}
		if _, ok := m["grove"]; ok {
			t.Errorf("legacy 'grove' key present in marshal output, want absent: %v", m["grove"])
		}
	})

	t.Run("marshal after legacy decode still omits legacy key", func(t *testing.T) {
		jsonData := `{"grove": {"id": "p1", "name": "Project 1"}, "created": true}`
		var r RegisterProjectResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}

		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}
		if _, ok := m["grove"]; ok {
			t.Errorf("legacy 'grove' key present in marshal output after decode, want absent: %v", m["grove"])
		}
	})
}
