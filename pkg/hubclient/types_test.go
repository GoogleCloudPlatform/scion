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

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
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
		if m["projectId"] != "my-id" {
			t.Errorf("projectId = %v, want %q", m["projectId"], "my-id")
		}
		if _, ok := m["grove"]; ok {
			t.Errorf("grove = %v, want key absent", m["grove"])
		}
		if _, ok := m["groveId"]; ok {
			t.Errorf("groveId = %v, want key absent", m["groveId"])
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

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
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
			"projectId":   "my-id",
			"name":        "my-name",
			"projectType": "my-type",
		}

		for k, v := range expected {
			if m[k] != v {
				t.Errorf("Field %q = %v, want %v", k, m[k], v)
			}
		}

		for _, legacyKey := range []string{"groveId", "groveName", "groveType"} {
			if _, ok := m[legacyKey]; ok {
				t.Errorf("legacy key %q present in marshal output, want absent: %v", legacyKey, m[legacyKey])
			}
		}
	})
}

func TestRuntimeBroker_JSON(t *testing.T) {
	t.Run("unmarshal legacy groves key", func(t *testing.T) {
		jsonData := `{"id": "b1", "groves": [{"projectId": "p1", "projectName": "Project 1"}]}`
		var b RuntimeBroker
		if err := json.Unmarshal([]byte(jsonData), &b); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if len(b.Projects) != 1 || b.Projects[0].ProjectID != "p1" {
			t.Errorf("Projects = %+v, want one project with ProjectID p1", b.Projects)
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		b := RuntimeBroker{
			ID:       "b1",
			Projects: []BrokerProjectInfo{{ProjectID: "p1", ProjectName: "Project 1"}},
		}
		data, err := json.Marshal(b)
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
		if _, ok := m["groves"]; ok {
			t.Errorf("legacy 'groves' key present in marshal output, want absent: %v", m["groves"])
		}
	})
}

func TestBrokerProjectInfo_JSON(t *testing.T) {
	t.Run("unmarshal legacy grove fields", func(t *testing.T) {
		jsonData := `{"groveId": "p1", "groveName": "Project 1"}`
		var i BrokerProjectInfo
		if err := json.Unmarshal([]byte(jsonData), &i); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if i.ProjectID != "p1" || i.ProjectName != "Project 1" {
			t.Errorf("ProjectID/ProjectName = %q/%q, want p1/Project 1", i.ProjectID, i.ProjectName)
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		i := BrokerProjectInfo{ProjectID: "p1", ProjectName: "Project 1"}
		data, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "p1" || m["projectName"] != "Project 1" {
			t.Errorf("projectId/projectName = %v/%v, want p1/Project 1", m["projectId"], m["projectName"])
		}
		for _, legacyKey := range []string{"groveId", "groveName"} {
			if _, ok := m[legacyKey]; ok {
				t.Errorf("legacy key %q present in marshal output, want absent: %v", legacyKey, m[legacyKey])
			}
		}
	})
}

func TestTemplate_JSON(t *testing.T) {
	t.Run("unmarshal legacy groveId field is not honored", func(t *testing.T) {
		jsonData := `{"id": "t1", "groveId": "p1"}`
		var tpl Template
		if err := json.Unmarshal([]byte(jsonData), &tpl); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if tpl.ProjectID != "" {
			t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", tpl.ProjectID)
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		tpl := Template{ID: "t1", ProjectID: "p1"}
		data, err := json.Marshal(tpl)
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
