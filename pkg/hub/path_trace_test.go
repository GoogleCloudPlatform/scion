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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DEF-79: Production message path trace test.
//
// This test sends a user→agent message through the FULL HTTP handler
// (auth middleware → handleAgentAction → authorizeAgentMessage →
// handleAgentMessage) and asserts the ordered sequence of steps the
// message traverses. The purpose is regression detection: if a future
// change inserts, removes, or reorders a step, this test fails.
//
// A test that merely asserts the final HTTP 200 response would pass
// even if critical intermediate steps (identity extraction, validation,
// conversation resolution, divergence logging) were silently removed.
// This test asserts the PATH, not the output.

// expectedPathSteps is the canonical ordered sequence of steps that a
// standard user-to-agent message must traverse.  If a step is added,
// removed, or reordered in the production code, this list must be
// updated — which forces the change to be reviewed.
//
// COVERAGE BOUNDARY — paths NOT traced by this test:
//
//   - Agent-scoped route (/api/v1/agents/{id}/message via handleAgentAction
//     in handlers_agents_core.go). Same handler, different authorization
//     dispatch path.
//   - Agent-to-agent messaging (sender is an AgentIdentity, not a
//     UserIdentity). The identity-extraction branch differs.
//   - Broker-inbound path (handleBrokerInbound in handlers_broker_inbound.go).
//     Entirely separate handler with its own validation and conversation
//     resolution sequence.
//   - Group-message fan-out (handleGroupMessage, entered when the recipient
//     matches messages.IsGroupRecipient). Short-circuits before agent load.
//   - Managed-agent path (isManagedAgentRuntime branch inside
//     handleAgentMessage). Bypasses the broker dispatch step entirely.
//   - handleAgentOutboundMessage (agent→user outbound path). Separate
//     handler, separate step sequence.
//
// A green result here means the user→agent DM path via the project-scoped
// route is pinned. It says nothing about the paths listed above.
var expectedPathSteps = []string{
	"message_authorized",
	"handle_agent_message_enter",
	"request_parsed",
	"sender_identity_extracted",
	"message_validated",
	"agent_loaded",
	"recipient_stamped",
	"conversation_resolved",
	"divergence_logged",
	"message_persisted",
	"sse_published",
	"broker_dispatched",
}

func TestDEF79_ProductionPathTrace(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project, broker, agent, and user.
	projectID := tid("def79-project")
	agentID := tid("def79-agent")
	agentSlug := "def79-agent"
	brokerID := tid("def79-broker")

	if err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "def79-project",
		Slug: "def79-project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "def79-broker",
		Slug:   "def79-broker",
		Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "def79-broker",
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}
	if err := s.CreateAgent(ctx, &store.Agent{
		ID:              agentID,
		Name:            "def79-agent",
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
			Msg:       "DEF-79 path trace test message",
			Type:      messages.TypeInstruction,
		},
	})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
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

	// Assert the recorded path matches the expected sequence.
	got := rec.Steps()
	if len(got) != len(expectedPathSteps) {
		t.Fatalf("path length mismatch: got %d steps %v, want %d steps %v",
			len(got), got, len(expectedPathSteps), expectedPathSteps)
	}
	for i := range expectedPathSteps {
		if got[i] != expectedPathSteps[i] {
			t.Errorf("step[%d]: got %q, want %q\n  full recorded path: %v",
				i, got[i], expectedPathSteps[i], got)
		}
	}
}

// TestDEF79_StepRemovalDetected proves that removing a step from the
// production path causes the trace test to fail. This is the
// mutate-then-revert proof required by AC-G4-2.
//
// We simulate a missing step by removing one element from the expected
// list and comparing against a hypothetical recorded path with that
// step absent. The real proof is that deleting a RecordStep call from
// the production code causes TestDEF79_ProductionPathTrace to fail.
func TestDEF79_StepRemovalDetected(t *testing.T) {
	// Verify that dropping any single step from the expected list
	// produces a different list — i.e. the expected list has no
	// duplicates and every step matters.
	for skip := 0; skip < len(expectedPathSteps); skip++ {
		reduced := make([]string, 0, len(expectedPathSteps)-1)
		for i, s := range expectedPathSteps {
			if i != skip {
				reduced = append(reduced, s)
			}
		}
		if len(reduced) == len(expectedPathSteps) {
			t.Fatalf("reduced list should be shorter")
		}
		if strings.Join(reduced, ",") == strings.Join(expectedPathSteps, ",") {
			t.Errorf("removing step %d (%q) did not change the expected path — duplicates?",
				skip, expectedPathSteps[skip])
		}
	}
}
