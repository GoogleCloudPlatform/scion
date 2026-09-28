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

// The following tests cover CreateAgentRequest, CreateSubscriptionRequest,
// CreateSubscriptionTemplateRequest and CreateTokenRequest, none of which
// has a custom MarshalJSON: all four encode with Go's default JSON
// marshaling. Each test asserts the canonical projectId key is present and
// that no groveId key is emitted, so a regression that reintroduces a
// groveId-emitting encoder would be caught.

func TestCreateAgentRequest_MarshalJSON(t *testing.T) {
	req := CreateAgentRequest{
		Name:      "agent-1",
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
	if _, ok := m["groveId"]; ok {
		t.Errorf("Expected no groveId field, got %v", m["groveId"])
	}
}

func TestCreateSubscriptionRequest_MarshalJSON(t *testing.T) {
	req := CreateSubscriptionRequest{
		Scope:             "project",
		ProjectID:         "p1",
		TriggerActivities: []string{"COMPLETED"},
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
	if _, ok := m["groveId"]; ok {
		t.Errorf("Expected no groveId field, got %v", m["groveId"])
	}
}

func TestCreateSubscriptionTemplateRequest_MarshalJSON(t *testing.T) {
	req := CreateSubscriptionTemplateRequest{
		Name:              "subtmpl",
		Scope:             "project",
		ProjectID:         "p1",
		TriggerActivities: []string{"COMPLETED"},
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
	if _, ok := m["groveId"]; ok {
		t.Errorf("Expected no groveId field, got %v", m["groveId"])
	}
}

func TestCreateTokenRequest_MarshalJSON(t *testing.T) {
	req := CreateTokenRequest{
		Name:      "token-1",
		ProjectID: "p1",
		Scopes:    []string{"read"},
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
	if _, ok := m["groveId"]; ok {
		t.Errorf("Expected no groveId field, got %v", m["groveId"])
	}
}
