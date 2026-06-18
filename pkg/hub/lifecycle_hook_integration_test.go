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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Integration test: end-to-end lifecycle hook flow
//
// Wires: LifecycleHookEvaluator + HTTPExecutor (with mock token generator)
//        + ent-backed test store + httptest "registry" server.
// Validates the motivating register-on-running / deregister-on-stop flow.
// ---------------------------------------------------------------------------

// registryRequest captures a single request received by the mock registry.
type registryRequest struct {
	Method   string
	Path     string
	RawQuery string
	Body     string
	Headers  http.Header
}

// mockRegistry is an httptest server that records incoming requests for
// assertion. It acts as the external service registry.
type mockRegistry struct {
	mu       sync.Mutex
	requests []registryRequest
	server   *httptest.Server
}

func newMockRegistry() *mockRegistry {
	r := &mockRegistry{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, registryRequest{
			Method:   req.Method,
			Path:     req.URL.Path,
			RawQuery: req.URL.RawQuery,
			Body:     string(body),
			Headers:  req.Header.Clone(),
		})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	return r
}

func (r *mockRegistry) close() {
	r.server.Close()
}

func (r *mockRegistry) getRequests() []registryRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]registryRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *mockRegistry) waitForRequests(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		r.mu.Lock()
		count := len(r.requests)
		r.mu.Unlock()
		if count >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d registry requests (got %d)", n, count)
		case <-time.After(20 * time.Millisecond):
			// poll
		}
	}
}

// integrationTestStore creates a fresh ent-backed in-memory store for
// integration tests. Uses the same newTestStore helper as the executor tests.
func integrationTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	return s
}

// ---------------------------------------------------------------------------
// TestLifecycleHookIntegration_RegisterDeregisterFlow
//
// End-to-end test of the motivating use case:
//  1. Create a "register" hook (trigger=running, http action POST to registry)
//  2. Create a "deregister" hook (trigger=stopped, http action DELETE to registry)
//  3. Publish agent.status event transitioning agent to running
//  4. Assert registry received the register POST with correct body
//  5. Publish agent.status event transitioning agent to stopped
//  6. Assert registry received the deregister DELETE
// ---------------------------------------------------------------------------

func TestLifecycleHookIntegration_RegisterDeregisterFlow(t *testing.T) {
	ctx := context.Background()
	s := integrationTestStore(t)

	// --- Seed project, SA, and agent ---
	projectID := uuid.New().String()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:         projectID,
		Name:       "integration-project",
		Slug:       "integration-project",
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}))

	saID := uuid.New().String()
	saEmail := "test-sa@integration.iam.gserviceaccount.com"
	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
		ID:                 saID,
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              saEmail,
		ProjectID:          "gcp-project",
		DisplayName:        "Integration Test SA",
		DefaultScopes:      []string{"https://www.googleapis.com/auth/cloud-platform"},
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: "verified",
		CreatedBy:          "test-user",
		CreatedAt:          time.Now(),
	}))

	agent := &store.Agent{
		ID:         uuid.New().String(),
		Slug:       "integration-agent",
		Name:       "Integration Agent",
		Template:   "claude",
		ProjectID:  projectID,
		Phase:      string(state.PhaseStarting),
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// --- Start mock registry ---
	registry := newMockRegistry()
	defer registry.close()

	// --- Create lifecycle hooks in the store ---

	// Register hook: fires on "running", POSTs to the registry.
	registerHook := &store.LifecycleHook{
		ID:        uuid.New().String(),
		Name:      "register-agent",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerRunning,
		Action: &store.LifecycleHookAction{
			Type:           store.LifecycleHookActionHTTP,
			Method:         "POST",
			URL:            registry.server.URL + "/v1/agents/${AGENT_ID}",
			Headers:        map[string]string{"Content-Type": "application/json"},
			Body:           `{"agentId":"${AGENT_ID}","projectId":"${PROJECT_ID}","slug":"${AGENT_SLUG}","action":"register"}`,
			OnError:        store.LifecycleHookOnErrorLog,
			TimeoutSeconds: 10,
		},
		ExecutionIdentity: saID,
		Enabled:           true,
		Created:           time.Now(),
		Updated:           time.Now(),
	}
	require.NoError(t, s.CreateLifecycleHook(ctx, registerHook))

	// Deregister hook: fires on "stopped", DELETEs from the registry.
	deregisterHook := &store.LifecycleHook{
		ID:        uuid.New().String(),
		Name:      "deregister-agent",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerStopped,
		Action: &store.LifecycleHookAction{
			Type:           store.LifecycleHookActionHTTP,
			Method:         "DELETE",
			URL:            registry.server.URL + "/v1/agents/${AGENT_ID}",
			Headers:        map[string]string{"Content-Type": "application/json"},
			Body:           `{"agentId":"${AGENT_ID}","action":"deregister"}`,
			OnError:        store.LifecycleHookOnErrorLog,
			TimeoutSeconds: 10,
		},
		ExecutionIdentity: saID,
		Enabled:           true,
		Created:           time.Now(),
		Updated:           time.Now(),
	}
	require.NoError(t, s.CreateLifecycleHook(ctx, deregisterHook))

	// --- Wire up the executor + evaluator ---

	// Reuse mockTokenGenerator from lifecycle_hook_executor_test.go (same package).
	tokenGen := &mockTokenGenerator{
		accessToken: "integration-test-bearer-token",
		email:       "hub-sa@integration.iam.gserviceaccount.com",
	}
	auditLog := newCapturingAuditLogger()

	// Use a test executor that allows loopback (httptest binds to 127.0.0.1).
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	events := NewChannelEventPublisher()
	defer events.Close()

	evaluator := NewLifecycleHookEvaluator(s, events, executor, slog.Default())
	evaluator.Start()
	defer evaluator.Stop()

	// =======================================================================
	// Step 1: Transition agent to "running" — register hook should fire
	// =======================================================================

	agent.Phase = string(state.PhaseRunning)
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseRunning),
	}))
	events.PublishAgentStatus(ctx, agent)

	// Wait for the registry to receive the register request.
	registry.waitForRequests(t, 1, 5*time.Second)

	reqs := registry.getRequests()
	require.Len(t, reqs, 1, "expected exactly 1 registry request after running transition")

	// Verify the register request.
	assert.Equal(t, "POST", reqs[0].Method)
	assert.Equal(t, "/v1/agents/"+agent.ID, reqs[0].Path)
	assert.Equal(t, "Bearer integration-test-bearer-token",
		reqs[0].Headers.Get("Authorization"),
		"http action should include bearer token")

	// Verify body contains expected fields.
	var registerBody map[string]string
	require.NoError(t, json.Unmarshal([]byte(reqs[0].Body), &registerBody))
	assert.Equal(t, agent.ID, registerBody["agentId"])
	assert.Equal(t, projectID, registerBody["projectId"])
	assert.Equal(t, "integration-agent", registerBody["slug"])
	assert.Equal(t, "register", registerBody["action"])

	// Verify audit events.
	auditEvents := auditLog.getEvents()
	require.Len(t, auditEvents, 1)
	assert.True(t, auditEvents[0].Success)
	assert.Equal(t, "running", auditEvents[0].Trigger)
	assert.Equal(t, saEmail, auditEvents[0].ExecutionIdentity)

	// =======================================================================
	// Step 2: Transition agent to "stopped" — deregister hook should fire
	// =======================================================================

	agent.Phase = string(state.PhaseStopped)
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseStopped),
	}))
	events.PublishAgentStatus(ctx, agent)

	// Wait for the deregister request.
	registry.waitForRequests(t, 2, 5*time.Second)

	reqs = registry.getRequests()
	require.Len(t, reqs, 2, "expected 2 registry requests after stopped transition")

	// Verify the deregister request.
	assert.Equal(t, "DELETE", reqs[1].Method)
	assert.Equal(t, "/v1/agents/"+agent.ID, reqs[1].Path)
	assert.Equal(t, "Bearer integration-test-bearer-token",
		reqs[1].Headers.Get("Authorization"))

	var deregisterBody map[string]string
	require.NoError(t, json.Unmarshal([]byte(reqs[1].Body), &deregisterBody))
	assert.Equal(t, agent.ID, deregisterBody["agentId"])
	assert.Equal(t, "deregister", deregisterBody["action"])

	// Verify audit for the stopped transition.
	auditEvents = auditLog.getEvents()
	require.Len(t, auditEvents, 2)
	assert.True(t, auditEvents[1].Success)
	assert.Equal(t, "stopped", auditEvents[1].Trigger)
}

// ---------------------------------------------------------------------------
// TestLifecycleHookIntegration_SuspendedAndErrorDeregister
//
// Validates deregister hooks fire on suspended and error transitions too.
// ---------------------------------------------------------------------------

func TestLifecycleHookIntegration_SuspendedAndErrorDeregister(t *testing.T) {
	ctx := context.Background()
	s := integrationTestStore(t)

	// --- Seed project, SA ---
	projectID := uuid.New().String()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:         projectID,
		Name:       "suspend-project",
		Slug:       "suspend-project",
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}))

	saID := uuid.New().String()
	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
		ID:                 saID,
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              "sa@suspend.iam.gserviceaccount.com",
		ProjectID:          "gcp-project",
		DisplayName:        "Suspend Test SA",
		DefaultScopes:      []string{"https://www.googleapis.com/auth/cloud-platform"},
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: "verified",
		CreatedBy:          "test-user",
		CreatedAt:          time.Now(),
	}))

	// --- Start mock registry ---
	registry := newMockRegistry()
	defer registry.close()

	// --- Create hooks for suspended and error ---
	for _, trigger := range []string{
		store.LifecycleHookTriggerSuspended,
		store.LifecycleHookTriggerError,
	} {
		hook := &store.LifecycleHook{
			ID:        uuid.New().String(),
			Name:      fmt.Sprintf("deregister-on-%s", trigger),
			ScopeType: store.LifecycleHookScopeHub,
			Trigger:   trigger,
			Action: &store.LifecycleHookAction{
				Type:           store.LifecycleHookActionHTTP,
				Method:         "DELETE",
				URL:            registry.server.URL + "/v1/agents/${AGENT_ID}",
				Headers:        map[string]string{"Content-Type": "application/json"},
				Body:           fmt.Sprintf(`{"agentId":"${AGENT_ID}","trigger":"%s"}`, trigger),
				OnError:        store.LifecycleHookOnErrorLog,
				TimeoutSeconds: 10,
			},
			ExecutionIdentity: saID,
			Enabled:           true,
			Created:           time.Now(),
			Updated:           time.Now(),
		}
		require.NoError(t, s.CreateLifecycleHook(ctx, hook))
	}

	// --- Wire up ---
	tokenGen := &mockTokenGenerator{accessToken: "suspend-token", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	events := NewChannelEventPublisher()
	defer events.Close()

	evaluator := NewLifecycleHookEvaluator(s, events, executor, slog.Default())
	evaluator.Start()
	defer evaluator.Stop()

	// --- Agent 1: starting → suspended ---
	agent1 := &store.Agent{
		ID:         uuid.New().String(),
		Slug:       "suspend-agent",
		Name:       "Suspend Agent",
		Template:   "claude",
		ProjectID:  projectID,
		Phase:      string(state.PhaseStarting),
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent1.Phase = string(state.PhaseSuspended)
	require.NoError(t, s.UpdateAgentStatus(ctx, agent1.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseSuspended),
	}))
	events.PublishAgentStatus(ctx, agent1)
	registry.waitForRequests(t, 1, 5*time.Second)

	reqs := registry.getRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "DELETE", reqs[0].Method)
	assert.Contains(t, reqs[0].Body, `"trigger":"suspended"`)

	// --- Agent 2: starting → error ---
	agent2 := &store.Agent{
		ID:         uuid.New().String(),
		Slug:       "error-agent",
		Name:       "Error Agent",
		Template:   "claude",
		ProjectID:  projectID,
		Phase:      string(state.PhaseStarting),
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	agent2.Phase = string(state.PhaseError)
	require.NoError(t, s.UpdateAgentStatus(ctx, agent2.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseError),
	}))
	events.PublishAgentStatus(ctx, agent2)
	registry.waitForRequests(t, 2, 5*time.Second)

	reqs = registry.getRequests()
	require.Len(t, reqs, 2)
	assert.Equal(t, "DELETE", reqs[1].Method)
	assert.Contains(t, reqs[1].Body, `"trigger":"error"`)

	// Verify audit events for both transitions.
	auditEvents := auditLog.getEvents()
	require.Len(t, auditEvents, 2)
	assert.Equal(t, "suspended", auditEvents[0].Trigger)
	assert.Equal(t, "error", auditEvents[1].Trigger)
	assert.True(t, auditEvents[0].Success)
	assert.True(t, auditEvents[1].Success)
}

// ---------------------------------------------------------------------------
// TestLifecycleHookIntegration_AgentRegistryA2AFlow
//
// End-to-end test of the Google Cloud Agent Registry integration use case:
//  1. Register hook constructs an A2A agent card body with the per-agent
//     A2A Bridge endpoint URL built from PROJECT_SLUG and AGENT_SLUG.
//  2. Deregister hook issues a DELETE using the same slug-based service name.
//  3. Validates that PROJECT_SLUG is correctly populated and the agent card
//     body contains the specific per-agent A2A Bridge endpoint URL.
// ---------------------------------------------------------------------------

func TestLifecycleHookIntegration_AgentRegistryA2AFlow(t *testing.T) {
	ctx := context.Background()
	s := integrationTestStore(t)

	projectSlug := "my-scion-project"
	projectID := uuid.New().String()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:         projectID,
		Name:       "My Scion Project",
		Slug:       projectSlug,
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}))

	saID := uuid.New().String()
	saEmail := "agent-registry-sa@ptone-emblem.iam.gserviceaccount.com"
	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
		ID:                 saID,
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              saEmail,
		ProjectID:          "ptone-emblem",
		DisplayName:        "Agent Registry SA",
		DefaultScopes:      []string{"https://www.googleapis.com/auth/cloud-platform"},
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: "verified",
		CreatedBy:          "test-user",
		CreatedAt:          time.Now(),
	}))

	agentSlug := "test-a2a-agent"
	agent := &store.Agent{
		ID:         uuid.New().String(),
		Slug:       agentSlug,
		Name:       "Test A2A Agent",
		Template:   "claude",
		ProjectID:  projectID,
		Phase:      string(state.PhaseStarting),
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	registry := newMockRegistry()
	defer registry.close()

	// The hook body mirrors the real Agent Registry API request format.
	// It constructs an A2A agent card with the specific per-agent endpoint
	// URL from the A2A Bridge (using PROJECT_SLUG and AGENT_SLUG).
	a2aBridgeBase := "https://a2a.example.com"
	registerBody := `{` +
		`"displayName":"Scion Agent: ${PROJECT_SLUG}/${AGENT_SLUG}",` +
		`"agentSpec":{` +
		`"type":"A2A_AGENT_CARD",` +
		`"content":{` +
		`"name":"${AGENT_SLUG}",` +
		`"description":"Scion agent ${AGENT_SLUG} in project ${PROJECT_SLUG}",` +
		`"version":"1.0.0",` +
		`"supportedInterfaces":[{"url":"` + a2aBridgeBase + `/projects/${PROJECT_SLUG}/agents/${AGENT_SLUG}","protocolBinding":"JSONRPC","protocolVersion":"0.3"}],` +
		`"capabilities":{"streaming":true,"pushNotifications":true},` +
		`"defaultInputModes":["text/plain","application/json"],` +
		`"defaultOutputModes":["text/plain","application/json"],` +
		`"skills":[{"id":"${AGENT_SLUG}","name":"${AGENT_SLUG}","description":"Interact with agent ${AGENT_SLUG}","tags":["scion","a2a"]}],` +
		`"provider":{"organization":"Scion","url":"https://github.com/ptone/scion"}` +
		`}` +
		`}` +
		`}`

	registerHook := &store.LifecycleHook{
		ID:        uuid.New().String(),
		Name:      "agent-registry-register",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerRunning,
		Action: &store.LifecycleHookAction{
			Type:           store.LifecycleHookActionHTTP,
			Method:         "POST",
			URL:            registry.server.URL + "/v1alpha/projects/ptone-emblem/locations/us-central1/services?serviceId=scion-${PROJECT_SLUG}-${AGENT_SLUG}",
			Headers:        map[string]string{"Content-Type": "application/json"},
			Body:           registerBody,
			OnError:        store.LifecycleHookOnErrorRetry,
			TimeoutSeconds: 15,
		},
		ExecutionIdentity: saID,
		Enabled:           true,
		Created:           time.Now(),
		Updated:           time.Now(),
	}
	require.NoError(t, s.CreateLifecycleHook(ctx, registerHook))

	deregisterHook := &store.LifecycleHook{
		ID:        uuid.New().String(),
		Name:      "agent-registry-deregister",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerStopped,
		Action: &store.LifecycleHookAction{
			Type:           store.LifecycleHookActionHTTP,
			Method:         "DELETE",
			URL:            registry.server.URL + "/v1alpha/projects/ptone-emblem/locations/us-central1/services/scion-${PROJECT_SLUG}-${AGENT_SLUG}",
			Headers:        map[string]string{"Content-Type": "application/json"},
			OnError:        store.LifecycleHookOnErrorRetry,
			TimeoutSeconds: 15,
		},
		ExecutionIdentity: saID,
		Enabled:           true,
		Created:           time.Now(),
		Updated:           time.Now(),
	}
	require.NoError(t, s.CreateLifecycleHook(ctx, deregisterHook))

	tokenGen := &mockTokenGenerator{
		accessToken: "agent-registry-bearer-token",
		email:       "hub-sa@ptone-emblem.iam.gserviceaccount.com",
	}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	events := NewChannelEventPublisher()
	defer events.Close()

	evaluator := NewLifecycleHookEvaluator(s, events, executor, slog.Default())
	evaluator.Start()
	defer evaluator.Stop()

	// === Transition to running: should register with Agent Registry ===

	agent.Phase = string(state.PhaseRunning)
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseRunning),
	}))
	events.PublishAgentStatus(ctx, agent)

	registry.waitForRequests(t, 1, 5*time.Second)
	reqs := registry.getRequests()
	require.Len(t, reqs, 1)

	// Verify the POST was sent to the correct Agent Registry path with
	// PROJECT_SLUG and AGENT_SLUG substituted in the query parameter.
	assert.Equal(t, "POST", reqs[0].Method)
	expectedServiceID := fmt.Sprintf("scion-%s-%s", projectSlug, agentSlug)
	assert.Equal(t, "/v1alpha/projects/ptone-emblem/locations/us-central1/services", reqs[0].Path)
	assert.Equal(t, "serviceId="+expectedServiceID, reqs[0].RawQuery)

	// Verify the body contains a valid A2A agent card with the correct
	// per-agent A2A Bridge endpoint URL.
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(reqs[0].Body), &body))
	expectedDisplayName := fmt.Sprintf("Scion Agent: %s/%s", projectSlug, agentSlug)
	assert.Equal(t, expectedDisplayName, body["displayName"])

	agentSpec, ok := body["agentSpec"].(map[string]interface{})
	require.True(t, ok, "agentSpec should be a JSON object")
	assert.Equal(t, "A2A_AGENT_CARD", agentSpec["type"])

	content, ok := agentSpec["content"].(map[string]interface{})
	require.True(t, ok, "agentSpec.content should be a JSON object")
	assert.Equal(t, agentSlug, content["name"])

	interfaces, ok := content["supportedInterfaces"].([]interface{})
	require.True(t, ok, "content.supportedInterfaces should be an array")
	require.Len(t, interfaces, 1)
	iface, ok := interfaces[0].(map[string]interface{})
	require.True(t, ok)
	expectedA2AURL := fmt.Sprintf("%s/projects/%s/agents/%s", a2aBridgeBase, projectSlug, agentSlug)
	assert.Equal(t, expectedA2AURL, iface["url"],
		"A2A agent card URL should be the specific per-agent A2A Bridge endpoint")
	assert.Equal(t, "JSONRPC", iface["protocolBinding"])
	assert.Equal(t, "0.3", iface["protocolVersion"])

	assert.Equal(t, "Bearer agent-registry-bearer-token",
		reqs[0].Headers.Get("Authorization"))

	// === Transition to stopped: should deregister ===

	agent.Phase = string(state.PhaseStopped)
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseStopped),
	}))
	events.PublishAgentStatus(ctx, agent)

	registry.waitForRequests(t, 2, 5*time.Second)
	reqs = registry.getRequests()
	require.Len(t, reqs, 2)

	assert.Equal(t, "DELETE", reqs[1].Method)
	expectedDeletePath := fmt.Sprintf("/v1alpha/projects/ptone-emblem/locations/us-central1/services/%s", expectedServiceID)
	assert.Equal(t, expectedDeletePath, reqs[1].Path)
	assert.Equal(t, "Bearer agent-registry-bearer-token",
		reqs[1].Headers.Get("Authorization"))

	// Verify audit trail.
	auditEvents := auditLog.getEvents()
	require.Len(t, auditEvents, 2)
	assert.True(t, auditEvents[0].Success)
	assert.Equal(t, "running", auditEvents[0].Trigger)
	assert.Equal(t, saEmail, auditEvents[0].ExecutionIdentity)
	assert.True(t, auditEvents[1].Success)
	assert.Equal(t, "stopped", auditEvents[1].Trigger)
}
