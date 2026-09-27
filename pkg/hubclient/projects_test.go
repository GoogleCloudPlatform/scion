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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProjectCacheRefreshResponse_JSON(t *testing.T) {
	t.Run("canonical round trip", func(t *testing.T) {
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

		var back ProjectCacheRefreshResponse
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if back.ProjectID != "p1" {
			t.Errorf("ProjectID = %q, want %q", back.ProjectID, "p1")
		}
	})

	t.Run("ignores bare legacy groveId", func(t *testing.T) {
		jsonData := `{"groveId": "p1"}`
		var r ProjectCacheRefreshResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if r.ProjectID != "" {
			t.Errorf("ProjectID = %q, want empty (legacy key must not be honored)", r.ProjectID)
		}
	})
}

func TestProjectCacheStatusResponse_JSON(t *testing.T) {
	t.Run("canonical round trip", func(t *testing.T) {
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

		var back ProjectCacheStatusResponse
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if back.ProjectID != "p1" {
			t.Errorf("ProjectID = %q, want %q", back.ProjectID, "p1")
		}
	})

	t.Run("ignores bare legacy groveId", func(t *testing.T) {
		jsonData := `{"groveId": "p1"}`
		var r ProjectCacheStatusResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if r.ProjectID != "" {
			t.Errorf("ProjectID = %q, want empty (legacy key must not be honored)", r.ProjectID)
		}
	})
}

func TestRegisterProjectResponse_JSON(t *testing.T) {
	t.Run("canonical round trip", func(t *testing.T) {
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

		var back RegisterProjectResponse
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if back.Project == nil || back.Project.ID != "p1" {
			t.Errorf("Project = %+v, want ID p1", back.Project)
		}
	})

	t.Run("ignores bare legacy grove field", func(t *testing.T) {
		jsonData := `{"grove": {"id": "p1", "name": "Project 1"}, "created": true}`
		var r RegisterProjectResponse
		if err := json.Unmarshal([]byte(jsonData), &r); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if r.Project != nil {
			t.Errorf("Project = %+v, want nil (legacy key must not be honored)", r.Project)
		}
	})
}

func TestProjectsList_IgnoresLegacyGrovesKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Only the legacy "groves" key is present; the canonical "projects"
		// key is absent, as if talking to a hub that only emitted the alias.
		_, _ = w.Write([]byte(`{"groves": [{"id": "p1"}], "totalCount": 1}`))
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Projects().List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(resp.Projects) != 0 {
		t.Errorf("Projects = %+v, want empty (legacy 'groves' key must not be honored)", resp.Projects)
	}
}
