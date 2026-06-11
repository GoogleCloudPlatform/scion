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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setupMessageTestAgent creates a project, runtime broker, and agent for message tests.
func setupMessageTestAgent(t *testing.T, s store.Store, phase string) (projectID, agentID string) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:       tid("msg-broker"),
		Name:     "msg-broker",
		Slug:     "msg-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	project := &store.Project{
		ID:         tid("msg-project"),
		Slug:       "msg-project",
		Name:       "msg-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:              tid("msg-agent"),
		Slug:            "msg-agent",
		Name:            "msg-agent",
		ProjectID:       project.ID,
		Phase:           phase,
		RuntimeBrokerID: broker.ID,
		Visibility:      store.VisibilityPrivate,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	return project.ID, agent.ID
}

// --- Stream H: Agent phase pre-check tests ---

func TestHandleAgentMessage_SuspendedReturns409(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseSuspended))

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), body)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeAgentNotRunning {
		t.Errorf("expected error code %q, got %q", ErrCodeAgentNotRunning, errResp.Error.Code)
	}
	if want := `Agent "msg-agent" is suspended. Use --wake to resume and deliver.`; errResp.Error.Message != want {
		t.Errorf("expected message %q, got %q", want, errResp.Error.Message)
	}
}

func TestHandleAgentMessage_StoppedReturns409(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("msg-broker-stop"), Name: "b", Slug: "b",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	_ = s.CreateRuntimeBroker(ctx, broker)
	project := &store.Project{
		ID: tid("msg-project-stop"), Slug: "msg-project-stop", Name: "msg-project-stop",
		Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateProject(ctx, project)
	agent := &store.Agent{
		ID: tid("msg-agent-stop"), Slug: "stopped-agent", Name: "stopped-agent",
		ProjectID: project.ID, Phase: string(state.PhaseStopped),
		RuntimeBrokerID: broker.ID, Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateAgent(ctx, agent)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:stopped-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agent.ID), body)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	json.NewDecoder(rec.Body).Decode(&errResp)
	if errResp.Error.Code != ErrCodeAgentNotRunning {
		t.Errorf("expected error code %q, got %q", ErrCodeAgentNotRunning, errResp.Error.Code)
	}
	if want := `Agent "stopped-agent" is stopped. Use 'scion start' to start a new session.`; errResp.Error.Message != want {
		t.Errorf("expected message %q, got %q", want, errResp.Error.Message)
	}
}

func TestHandleAgentMessage_ErrorReturns409(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("msg-broker-err"), Name: "b", Slug: "b",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	_ = s.CreateRuntimeBroker(ctx, broker)
	project := &store.Project{
		ID: tid("msg-project-err"), Slug: "msg-project-err", Name: "msg-project-err",
		Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateProject(ctx, project)
	agent := &store.Agent{
		ID: tid("msg-agent-err"), Slug: "error-agent", Name: "error-agent",
		ProjectID: project.ID, Phase: string(state.PhaseError),
		RuntimeBrokerID: broker.ID, Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateAgent(ctx, agent)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:error-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agent.ID), body)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	json.NewDecoder(rec.Body).Decode(&errResp)
	if errResp.Error.Code != ErrCodeAgentNotRunning {
		t.Errorf("expected error code %q, got %q", ErrCodeAgentNotRunning, errResp.Error.Code)
	}
	if want := `Agent "error-agent" is in error state. Use 'scion start' to restart.`; errResp.Error.Message != want {
		t.Errorf("expected message %q, got %q", want, errResp.Error.Message)
	}
}

func TestHandleAgentMessage_ProvisioningReturns409(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("msg-broker-prov"), Name: "b", Slug: "b",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	_ = s.CreateRuntimeBroker(ctx, broker)
	project := &store.Project{
		ID: tid("msg-project-prov"), Slug: "msg-project-prov", Name: "msg-project-prov",
		Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateProject(ctx, project)
	agent := &store.Agent{
		ID: tid("msg-agent-prov"), Slug: "prov-agent", Name: "prov-agent",
		ProjectID: project.ID, Phase: string(state.PhaseProvisioning),
		RuntimeBrokerID: broker.ID, Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateAgent(ctx, agent)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:prov-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agent.ID), body)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	json.NewDecoder(rec.Body).Decode(&errResp)
	if errResp.Error.Code != ErrCodeAgentNotRunning {
		t.Errorf("expected error code %q, got %q", ErrCodeAgentNotRunning, errResp.Error.Code)
	}
}

func TestHandleAgentMessage_RunningReturns200(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseRunning))

	// Set up a mock dispatcher for the running case
	srv.SetDispatcher(&brokerMockDispatcher{})

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), body)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp MessageDeliveryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "delivered" {
		t.Errorf("expected status %q, got %q", "delivered", resp.Status)
	}
	if resp.Agent != "msg-agent" {
		t.Errorf("expected agent %q, got %q", "msg-agent", resp.Agent)
	}
	if resp.AgentPhase != string(state.PhaseRunning) {
		t.Errorf("expected agent_phase %q, got %q", string(state.PhaseRunning), resp.AgentPhase)
	}
	if resp.MessageID == "" {
		t.Error("expected non-empty message_id")
	}
}

// --- Stream G: Broadcast partial-failure tests ---

func TestHandleProjectBroadcast_Returns202WithTargeting(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("bcast-broker"), Name: "b", Slug: "b",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	_ = s.CreateRuntimeBroker(ctx, broker)
	project := &store.Project{
		ID: tid("bcast-project"), Slug: "bcast-project", Name: "bcast-project",
		Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateProject(ctx, project)

	// Create agents in various phases
	for _, tc := range []struct {
		slug  string
		phase string
	}{
		{"agent-running-1", string(state.PhaseRunning)},
		{"agent-running-2", string(state.PhaseRunning)},
		{"agent-suspended", string(state.PhaseSuspended)},
		{"agent-stopped", string(state.PhaseStopped)},
		{"agent-error", string(state.PhaseError)},
	} {
		agent := &store.Agent{
			ID: api.NewUUID(), Slug: tc.slug, Name: tc.slug,
			ProjectID: project.ID, Phase: tc.phase,
			RuntimeBrokerID: broker.ID, Visibility: store.VisibilityPrivate,
		}
		_ = s.CreateAgent(ctx, agent)
	}

	// Set up mock dispatcher for direct fan-out
	srv.SetDispatcher(&brokerMockDispatcher{})

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Msg: "broadcast msg",
			Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/broadcast", project.ID), body)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp BroadcastAcceptedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("expected status %q, got %q", "accepted", resp.Status)
	}
	if resp.Total != 5 {
		t.Errorf("expected total 5, got %d", resp.Total)
	}
	if resp.Targeted != 2 {
		t.Errorf("expected targeted 2, got %d", resp.Targeted)
	}
	if resp.Skipped != 3 {
		t.Errorf("expected skipped 3, got %d", resp.Skipped)
	}
	if resp.SkippedBreakdown[string(state.PhaseSuspended)] != 1 {
		t.Errorf("expected 1 suspended in skipped_breakdown, got %d", resp.SkippedBreakdown[string(state.PhaseSuspended)])
	}
	if resp.SkippedBreakdown[string(state.PhaseStopped)] != 1 {
		t.Errorf("expected 1 stopped in skipped_breakdown, got %d", resp.SkippedBreakdown[string(state.PhaseStopped)])
	}
	if resp.SkippedBreakdown[string(state.PhaseError)] != 1 {
		t.Errorf("expected 1 error in skipped_breakdown, got %d", resp.SkippedBreakdown[string(state.PhaseError)])
	}
}

func TestHandleProjectBroadcast_AllRunning(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("bcast-broker-all"), Name: "b", Slug: "b",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	_ = s.CreateRuntimeBroker(ctx, broker)
	project := &store.Project{
		ID: tid("bcast-project-all"), Slug: "bcast-project-all", Name: "bcast-project-all",
		Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateProject(ctx, project)

	for i := 0; i < 3; i++ {
		slug := fmt.Sprintf("running-%d", i)
		agent := &store.Agent{
			ID: api.NewUUID(), Slug: slug, Name: slug,
			ProjectID: project.ID, Phase: string(state.PhaseRunning),
			RuntimeBrokerID: broker.ID, Visibility: store.VisibilityPrivate,
		}
		_ = s.CreateAgent(ctx, agent)
	}

	srv.SetDispatcher(&brokerMockDispatcher{})

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Msg: "hello all",
			Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/broadcast", project.ID), body)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp BroadcastAcceptedResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Targeted != 3 {
		t.Errorf("expected targeted 3, got %d", resp.Targeted)
	}
	if resp.Skipped != 0 {
		t.Errorf("expected skipped 0, got %d", resp.Skipped)
	}
	if len(resp.SkippedBreakdown) != 0 {
		t.Errorf("expected empty skipped_breakdown, got %v", resp.SkippedBreakdown)
	}
}

func TestHandleProjectBroadcast_NoAgents(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	project := &store.Project{
		ID: tid("bcast-project-empty"), Slug: "bcast-project-empty", Name: "bcast-project-empty",
		Visibility: store.VisibilityPrivate,
	}
	_ = s.CreateProject(ctx, project)

	srv.SetDispatcher(&brokerMockDispatcher{})

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Msg: "hello",
			Type: messages.TypeInstruction,
		},
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/broadcast", project.ID), body)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp BroadcastAcceptedResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Total != 0 {
		t.Errorf("expected total 0, got %d", resp.Total)
	}
	if resp.Targeted != 0 {
		t.Errorf("expected targeted 0, got %d", resp.Targeted)
	}
}
