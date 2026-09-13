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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAgentScopedPathTrace_MessageAuthorized pins the agent-scoped
// messaging.RecordStep(ctx, "message_authorized") call in
// handlers_agents_core.go (handleAgentAction → authorizeAgentMessage).
//
// The project-scoped path is pinned by TestDEF79_ProductionPathTrace in
// path_trace_test.go. This test covers the AGENT-scoped route
// (/api/v1/agents/{id}/message), which shares the handler but enters
// through a different authorization dispatch path.
//
// The assertion is intentionally narrow: we check only that
// "message_authorized" appears in the recorded steps. The full ordered
// sequence is the project-scoped test's job; this test exists to ensure
// the agent-scoped entry point records the authorization step.
func TestAgentScopedPathTrace_MessageAuthorized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project, broker, agent, and user — same fixture pattern as
	// TestDEF79_ProductionPathTrace but with distinct IDs.
	projectID := tid("agent-trace-project")
	agentID := tid("agent-trace-agent")
	agentSlug := "agent-trace-agent"
	brokerID := tid("agent-trace-broker")

	if err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "agent-trace-project",
		Slug: "agent-trace-project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "agent-trace-broker",
		Slug:   "agent-trace-broker",
		Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "agent-trace-broker",
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}
	if err := s.CreateAgent(ctx, &store.Agent{
		ID:              agentID,
		Name:            "agent-trace-agent",
		Slug:            agentSlug,
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// The dev user may already exist (auth middleware upsert). Ignore dup errors.
	_ = s.CreateUser(ctx, &store.User{
		ID:          DevUserID,
		Email:       "dev@localhost",
		DisplayName: "Development User",
	})

	// Set a recording dispatcher so dispatch succeeds.
	srv.SetDispatcher(&recordingDispatcher{})

	// Build the request with a PathRecorder injected into the context.
	rec := messaging.NewPathRecorder()
	body, _ := json.Marshal(MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:dev@localhost",
			Recipient: "agent:" + agentSlug,
			Msg:       "agent-scoped path trace test message",
			Type:      messages.TypeInstruction,
		},
	})

	// Use the AGENT-scoped route: /api/v1/agents/{id}/message
	// This enters via handleAgentByID → handleAgentAction, NOT the
	// project-scoped route in handlers_projects_core.go.
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/message",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	// Inject the PathRecorder into the request context.
	req = req.WithContext(messaging.ContextWithPathRecorder(req.Context(), rec))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Assert that "message_authorized" was recorded on this path.
	got := rec.Steps()
	found := false
	for _, step := range got {
		if step == "message_authorized" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("agent-scoped path did not record \"message_authorized\"\n  recorded steps: %v", got)
	}
}
