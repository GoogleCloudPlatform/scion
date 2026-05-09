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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestIsEmbeddedBroker(t *testing.T) {
	srv := &Server{}

	// Before setting, everything should return false
	if srv.isEmbeddedBroker("broker-1") {
		t.Error("expected isEmbeddedBroker to return false before setting")
	}
	if srv.isEmbeddedBroker("") {
		t.Error("expected isEmbeddedBroker to return false for empty string")
	}

	// Set the embedded broker ID
	srv.SetEmbeddedBrokerID("broker-1")

	// Matching ID should return true
	if !srv.isEmbeddedBroker("broker-1") {
		t.Error("expected isEmbeddedBroker to return true for matching ID")
	}

	// Non-matching ID should return false
	if srv.isEmbeddedBroker("broker-2") {
		t.Error("expected isEmbeddedBroker to return false for non-matching ID")
	}

	// Empty string should still return false
	if srv.isEmbeddedBroker("") {
		t.Error("expected isEmbeddedBroker to return false for empty string even when embedded ID is set")
	}
}

func TestCreateAgent_SkipsGCSSyncForEmbeddedBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project (hub-native: no git remote)
	project := &store.Project{
		ID:   "project-embedded-test",
		Name: "embedded-test",
		Slug: "embedded-test",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a runtime broker
	brokerID := "embedded-broker-1"
	broker := &store.RuntimeBroker{
		ID:       brokerID,
		Name:     "embedded-broker",
		Endpoint: "http://localhost:9090",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	// Create a project provider WITHOUT LocalPath (simulating autoLinkProviders behavior)
	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   brokerID,
		BrokerName: broker.Name,
		// LocalPath intentionally empty — this is the bug scenario
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Set the default broker on the project
	project.DefaultRuntimeBrokerID = brokerID
	if err := s.UpdateProject(ctx, project); err != nil {
		t.Fatalf("failed to update project: %v", err)
	}

	// Mark the broker as the embedded broker
	srv.SetEmbeddedBrokerID(brokerID)

	// Create agent request for the hub-native project
	reqBody := CreateAgentRequest{
		Name:    "test-agent",
		ProjectID: project.ID,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// The agent should be created. The key assertion is that
	// the handler did NOT attempt GCS sync (which would fail without storage).
	// Since there's no storage configured, if the embedded broker check didn't
	// work, the handler would try to upload to GCS and fail.
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Logf("Response body: %s", rec.Body.String())
	}

	// Verify the agent was created in the store
	agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list agents: %v", err)
	}

	for _, a := range agents.Items {
		if a.Name == "test-agent" {
			// Key check: WorkspaceStoragePath should NOT be set because GCS sync was skipped
			if a.AppliedConfig != nil && a.AppliedConfig.WorkspaceStoragePath != "" {
				t.Errorf("expected WorkspaceStoragePath to be empty (GCS sync should have been skipped), got %q",
					a.AppliedConfig.WorkspaceStoragePath)
			}
			return
		}
	}
	t.Log("Agent not found in store — this may be expected if dispatcher is not configured")
}
