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
	"strings"
	"testing"
)

func TestAgent_JSON(t *testing.T) {
	t.Run("unmarshal canonical fields only", func(t *testing.T) {
		jsonData := `{
			"id": "agent-1",
			"project": "new-project",
			"projectId": "new-id"
		}`
		var a Agent
		if err := json.Unmarshal([]byte(jsonData), &a); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if a.Project != "new-project" {
			t.Errorf("Project = %q, want %q", a.Project, "new-project")
		}
		if a.ProjectID != "new-id" {
			t.Errorf("ProjectID = %q, want %q", a.ProjectID, "new-id")
		}
	})

	t.Run("unmarshal ignores legacy grove fields", func(t *testing.T) {
		// The decode-side legacy fallback is removed: a bare
		// "grove"/"groveId" with no canonical counterpart no longer
		// populates Project/ProjectID.
		jsonData := `{
			"id": "agent-1",
			"grove": "legacy-grove",
			"groveId": "legacy-id"
		}`
		var a Agent
		if err := json.Unmarshal([]byte(jsonData), &a); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if a.Project != "" {
			t.Errorf("Project = %q, want empty (legacy grove must not be honored)", a.Project)
		}
		if a.ProjectID != "" {
			t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", a.ProjectID)
		}
	})

	t.Run("marshal dual fields", func(t *testing.T) {
		a := Agent{
			Project:   "my-project",
			ProjectID: "my-id",
		}
		data, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["project"] != "my-project" {
			t.Errorf("project = %v, want %q", m["project"], "my-project")
		}
		if m["grove"] != "my-project" {
			t.Errorf("grove = %v, want %q", m["grove"], "my-project")
		}
		if m["projectId"] != "my-id" {
			t.Errorf("projectId = %v, want %q", m["projectId"], "my-id")
		}
		if m["groveId"] != "my-id" {
			t.Errorf("groveId = %v, want %q", m["groveId"], "my-id")
		}
	})
}
func TestProject_JSON(t *testing.T) {
	t.Run("unmarshal canonical fields only", func(t *testing.T) {
		jsonData := `{
			"id": "canonical-id",
			"name": "canonical-name",
			"projectType": "canonical-type"
		}`
		var p Project
		if err := json.Unmarshal([]byte(jsonData), &p); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if p.ID != "canonical-id" {
			t.Errorf("ID = %q, want %q", p.ID, "canonical-id")
		}
		if p.Name != "canonical-name" {
			t.Errorf("Name = %q, want %q", p.Name, "canonical-name")
		}
		if p.ProjectType != "canonical-type" {
			t.Errorf("ProjectType = %q, want %q", p.ProjectType, "canonical-type")
		}
	})

	t.Run("unmarshal ignores legacy grove fields", func(t *testing.T) {
		// The decode-side legacy fallback is removed: groveName/groveId/
		// groveType no longer populate Name/ID/ProjectType.
		jsonData := `{
			"groveName": "legacy-name",
			"groveId": "legacy-id",
			"groveType": "legacy-type"
		}`
		var p Project
		if err := json.Unmarshal([]byte(jsonData), &p); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if p.ID != "" {
			t.Errorf("ID = %q, want empty (legacy groveId must not be honored)", p.ID)
		}
		if p.Name != "" {
			t.Errorf("Name = %q, want empty (legacy groveName must not be honored)", p.Name)
		}
		if p.ProjectType != "" {
			t.Errorf("ProjectType = %q, want empty (legacy groveType must not be honored)", p.ProjectType)
		}
	})

	t.Run("marshal dual fields", func(t *testing.T) {
		p := Project{
			ID:          "my-id",
			Name:        "my-name",
			ProjectType: "my-type",
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		expected := map[string]string{
			"id":          "my-id",
			"groveId":     "my-id",
			"name":        "my-name",
			"groveName":   "my-name",
			"projectType": "my-type",
			"groveType":   "my-type",
		}

		for k, v := range expected {
			if m[k] != v {
				t.Errorf("Field %q = %v, want %v", k, m[k], v)
			}
		}
	})
}

func TestResolvedSecret_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove source", func(t *testing.T) {
		jsonData := `{"name": "MY_SECRET", "source": "grove"}`
		var secret ResolvedSecret
		if err := json.Unmarshal([]byte(jsonData), &secret); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if secret.Source != "project" {
			t.Errorf("Source = %q, want %q", secret.Source, "project")
		}
	})

	t.Run("marshal project source", func(t *testing.T) {
		secret := ResolvedSecret{
			Name:   "MY_SECRET",
			Source: "project",
		}
		data, err := json.Marshal(secret)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}
		if !strings.Contains(string(data), `"source":"project"`) {
			t.Errorf("Marshal output missing source:project: %s", string(data))
		}
	})
}
