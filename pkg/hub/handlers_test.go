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

	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/store/sqlite"
)

// testDevToken is the development token used for testing.
const testDevToken = "scion_dev_test_token_for_unit_tests_1234567890"

// testServer creates a test server with an in-memory SQLite store.
// The server is configured with dev auth enabled using testDevToken.
func testServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken // Enable dev auth for testing
	srv := New(cfg, s)
	return srv, s
}

// doRequest performs an HTTP request against the test server.
// It automatically includes the dev auth token for authenticated endpoints.
func doRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Add dev auth token for authenticated endpoints
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doRequestNoAuth performs an HTTP request without authentication.
// Use this for testing unauthenticated access or auth endpoints themselves.
func doRequestNoAuth(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ============================================================================
// Health Endpoint Tests
// ============================================================================

func TestHealthz(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/healthz", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", resp.Status)
	}
}

func TestReadyz(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ready" {
		t.Errorf("expected status 'ready', got %q", resp["status"])
	}
}

// ============================================================================
// Agent Endpoint Tests
// ============================================================================

func TestAgentList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a grove first (agents reference groves)
	grove := &store.Grove{
		ID:        "grove_test123",
		Slug:      "test-grove",
		Name:      "Test Grove",
		GitRemote: "https://github.com/test/repo",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create some test agents
	for i := 0; i < 3; i++ {
		agent := &store.Agent{
			ID:           "agent_" + string(rune('a'+i)),
			Slug:      "test-agent-" + string(rune('a'+i)),
			Name:         "Test Agent " + string(rune('A'+i)),
			GroveID:      grove.ID,
			Status:       store.AgentStatusStopped,
			StateVersion: 1,
			Created:      time.Now(),
			Updated:      time.Now(),
		}
		if err := s.CreateAgent(ctx, agent); err != nil {
			t.Fatalf("failed to create agent: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListAgentsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Agents) != 3 {
		t.Errorf("expected 3 agents, got %d", len(resp.Agents))
	}

	if resp.TotalCount != 3 {
		t.Errorf("expected total 3, got %d", resp.TotalCount)
	}
}

func TestAgentCreate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker first
	broker := &store.RuntimeBroker{
		ID:     "host_test123",
		Slug:   "test-host",
		Name:   "Test Host",
				Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a grove with default runtime broker
	grove := &store.Grove{
		ID:                   "grove_abc123",
		Slug:                 "my-grove",
		Name:                 "My Grove",
		GitRemote:            "github.com/test/repo",
		DefaultRuntimeBrokerID: broker.ID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Register the broker as a provider to the grove
	contrib := &store.GroveProvider{
		GroveID:  grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
				Status:   store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	body := map[string]interface{}{
		"name":    "New Agent",
		"groveId": grove.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be set")
	}

	if resp.Agent.Slug != "new-agent" {
		t.Errorf("expected agentId 'new-agent', got %q", resp.Agent.Slug)
	}

	if resp.Agent.ID == "" {
		t.Error("expected ID to be set")
	}

	if resp.Agent.Status != store.AgentStatusPending {
		t.Errorf("expected status 'pending', got %q", resp.Agent.Status)
	}

	if resp.Agent.RuntimeBrokerID != broker.ID {
		t.Errorf("expected runtimeBrokerId %q, got %q", broker.ID, resp.Agent.RuntimeBrokerID)
	}
}

// TestAgentCreate_SingleProvider tests that when a grove has no default runtime broker
// but has exactly one online provider, that provider is used automatically.
func TestAgentCreate_SingleProvider(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "host_single",
		Slug:   "single-host",
		Name:   "Single Host",
				Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a grove WITHOUT a default runtime broker
	grove := &store.Grove{
		ID:        "grove_single",
		Slug:      "single-grove",
		Name:      "Single Grove",
		GitRemote: "github.com/test/single",
		// Note: DefaultRuntimeBrokerID is NOT set
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Register the broker as the only provider to the grove
	contrib := &store.GroveProvider{
		GroveID:  grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
				Status:   store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	// Create agent without specifying runtimeBrokerId
	body := map[string]interface{}{
		"name":    "Auto Resolved Agent",
		"groveId": grove.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should automatically use the single provider
	if resp.Agent.RuntimeBrokerID != broker.ID {
		t.Errorf("expected runtimeBrokerId %q (single provider), got %q", broker.ID, resp.Agent.RuntimeBrokerID)
	}
}

// TestAgentCreate_MultipleProviders tests that when a grove has multiple online providers
// but no default runtime broker, an error is returned requiring explicit selection.
func TestAgentCreate_MultipleProviders(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two runtime brokers
	broker1 := &store.RuntimeBroker{
		ID:     "host_multi1",
		Slug:   "multi-host-1",
		Name:   "Multi Host 1",
				Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker1); err != nil {
		t.Fatalf("failed to create runtime broker 1: %v", err)
	}

	broker2 := &store.RuntimeBroker{
		ID:     "host_multi2",
		Slug:   "multi-host-2",
		Name:   "Multi Host 2",
				Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker2); err != nil {
		t.Fatalf("failed to create runtime broker 2: %v", err)
	}

	// Create a grove WITHOUT a default runtime broker
	grove := &store.Grove{
		ID:        "grove_multi",
		Slug:      "multi-grove",
		Name:      "Multi Grove",
		GitRemote: "github.com/test/multi",
		// Note: DefaultRuntimeBrokerID is NOT set
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Register both brokers as providers to the grove
	contrib1 := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker1.ID,
		BrokerName: broker1.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, contrib1); err != nil {
		t.Fatalf("failed to add grove provider 1: %v", err)
	}

	contrib2 := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker2.ID,
		BrokerName: broker2.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, contrib2); err != nil {
		t.Fatalf("failed to add grove provider 2: %v", err)
	}

	// Attempt to create agent without specifying runtimeBrokerId
	body := map[string]interface{}{
		"name":    "Ambiguous Agent",
		"groveId": grove.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	// Should fail with 422 because multiple brokers are available and explicit selection is required
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected status 422, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Error.Code != ErrCodeNoRuntimeBroker {
		t.Errorf("expected error code %q, got %q", ErrCodeNoRuntimeBroker, errResp.Error.Code)
	}

	// Should include available brokers in the response details
	availableBrokers, ok := errResp.Error.Details["availableBrokers"].([]interface{})
	if !ok {
		t.Fatalf("expected availableBrokers in error details, got %v", errResp.Error.Details)
	}
	if len(availableBrokers) != 2 {
		t.Errorf("expected 2 available brokers in error, got %d", len(availableBrokers))
	}
}

func TestAgentGetByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create grove and agent
	grove := &store.Grove{
		ID:        "grove_xyz",
		Slug:      "grove-xyz",
		Name:      "Grove XYZ",
		GitRemote: "https://github.com/test/repo",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	agent := &store.Agent{
		ID:           "agent_test1",
		Slug:      "test-agent",
		Name:         "Test Agent",
		GroveID:      grove.ID,
		Status:       store.AgentStatusStopped,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/agent_test1", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Agent
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != "agent_test1" {
		t.Errorf("expected ID 'agent_test1', got %q", resp.ID)
	}
}

func TestAgentNotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/nonexistent", nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error.Code != ErrCodeNotFound {
		t.Errorf("expected error code %q, got %q", ErrCodeNotFound, resp.Error.Code)
	}
}

func TestAgentDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create grove and agent
	grove := &store.Grove{
		ID:        "grove_del",
		Slug:      "grove-del",
		Name:      "Grove Del",
		GitRemote: "https://github.com/test/repo",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	agent := &store.Agent{
		ID:           "agent_delete",
		Slug:      "delete-me",
		Name:         "Delete Me",
		GroveID:      grove.ID,
		Status:       store.AgentStatusStopped,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/agent_delete", nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify agent is deleted
	_, err := s.GetAgent(ctx, "agent_delete")
	if err == nil {
		t.Error("expected agent to be deleted")
	}
}

// ============================================================================
// Grove Endpoint Tests
// ============================================================================

func TestGroveList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		grove := &store.Grove{
			ID:        "grove_" + string(rune('a'+i)),
			Slug:      "grove-" + string(rune('a'+i)),
			Name:      "Grove " + string(rune('A'+i)),
			GitRemote: "https://github.com/test/repo" + string(rune('a'+i)),
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		if err := s.CreateGrove(ctx, grove); err != nil {
			t.Fatalf("failed to create grove: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groves", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListGrovesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Groves) != 2 {
		t.Errorf("expected 2 groves, got %d", len(resp.Groves))
	}
}

func TestGroveRegister(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"gitRemote": "https://github.com/test/my-project.git",
		"name":      "My Project",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body)

	// Grove register always returns 200 (idempotent)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp RegisterGroveResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Grove.ID == "" {
		t.Error("expected grove ID to be set")
	}

	if !resp.Created {
		t.Error("expected created to be true for new grove")
	}

	// The git remote should be normalized (no scheme, no .git suffix)
	if resp.Grove.GitRemote != "github.com/test/my-project" {
		t.Errorf("expected normalized git remote 'github.com/test/my-project', got %q", resp.Grove.GitRemote)
	}
}

func TestGroveRegisterIdempotent(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"gitRemote": "https://github.com/test/idempotent-repo",
		"name":      "Idempotent Repo",
	}

	// First registration
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 RegisterGroveResponse
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp1.Created {
		t.Error("expected created to be true for first registration")
	}

	// Second registration with same git remote
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200 for idempotent call, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 RegisterGroveResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should return the same grove
	if resp1.Grove.ID != resp2.Grove.ID {
		t.Errorf("expected same grove ID on idempotent call, got %q and %q", resp1.Grove.ID, resp2.Grove.ID)
	}

	// Second call should not have created=true
	if resp2.Created {
		t.Error("expected created to be false on second call")
	}
}

func TestGroveRegisterCaseInsensitive(t *testing.T) {
	srv, _ := testServer(t)

	// First registration with "Global" (title case)
	body1 := map[string]interface{}{
		"name": "Global",
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 RegisterGroveResponse
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp1.Created {
		t.Error("expected created to be true for first registration")
	}

	// Second registration with "global" (lowercase) - should match existing grove
	body2 := map[string]interface{}{
		"name": "global",
	}

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200 for idempotent call, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 RegisterGroveResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should return the same grove (case-insensitive match)
	if resp1.Grove.ID != resp2.Grove.ID {
		t.Errorf("expected same grove ID for case-insensitive match, got %q and %q", resp1.Grove.ID, resp2.Grove.ID)
	}

	// Second call should not have created=true
	if resp2.Created {
		t.Error("expected created to be false for case-insensitive match")
	}
}

func TestGroveRegisterBrokerDeduplication(t *testing.T) {
	srv, _ := testServer(t)

	// Register a grove with a broker
	body1 := map[string]interface{}{
		"name":      "Test Grove",
		"gitRemote": "https://github.com/test/dedup-host",
		"broker": map[string]interface{}{
			"name":    "test-host",
			"version": "1.0.0",
		},
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 RegisterGroveResponse
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	brokerID1 := resp1.Broker.ID

	// Register another grove with the same broker name (case-insensitive)
	body2 := map[string]interface{}{
		"name":      "Another Grove",
		"gitRemote": "https://github.com/test/another-grove",
		"broker": map[string]interface{}{
			"name":    "TEST-HOST", // Different case
			"version": "1.0.1",
		},
	}

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 RegisterGroveResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should reuse the same broker (case-insensitive match)
	if resp1.Broker.ID != resp2.Broker.ID {
		t.Errorf("expected same broker ID for case-insensitive match, got %q and %q", brokerID1, resp2.Broker.ID)
	}

	// The version should be updated
	if resp2.Broker.Version != "1.0.1" {
		t.Errorf("expected broker version to be updated to '1.0.1', got %q", resp2.Broker.Version)
	}
}

func TestGroveRegisterWithBrokerID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// First, create a broker directly (simulating Phase 1 + 2 of two-phase flow)
	broker := &store.RuntimeBroker{
		ID:     "host_twophase_test",
		Name:   "Two Phase Test Host",
		Slug:   "two-phase-test-host",
				Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Now register grove with brokerId (Phase 3)
	body := map[string]interface{}{
		"name":      "Two Phase Grove",
		"gitRemote": "https://github.com/test/twophase-grove",
		"brokerId":    broker.ID,
		"path":      "/path/to/project/.scion",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp RegisterGroveResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Grove.ID == "" {
		t.Error("expected grove ID to be set")
	}

	if !resp.Created {
		t.Error("expected created to be true for new grove")
	}

	// Broker should be populated in response
	if resp.Broker == nil {
		t.Error("expected broker to be set in response")
	} else if resp.Broker.ID != broker.ID {
		t.Errorf("expected broker ID %q, got %q", broker.ID, resp.Broker.ID)
	}

	// Should NOT have secretKey (two-phase flow doesn't generate secrets in grove registration)
	if resp.SecretKey != "" {
		t.Error("expected secretKey to be empty in new two-phase flow")
	}

	// Verify provider was created
	providers, err := s.GetGroveProviders(ctx, resp.Grove.ID)
	if err != nil {
		t.Fatalf("failed to get providers: %v", err)
	}
	if len(providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(providers))
	}
	if providers[0].BrokerID != broker.ID {
		t.Errorf("expected provider broker ID %q, got %q", broker.ID, providers[0].BrokerID)
	}
	if providers[0].LocalPath != "/path/to/project/.scion" {
		t.Errorf("expected localPath '/path/to/project/.scion', got %q", providers[0].LocalPath)
	}
}

func TestGroveRegisterWithInvalidBrokerID(t *testing.T) {
	srv, _ := testServer(t)

	// Try to register grove with non-existent brokerId
	body := map[string]interface{}{
		"name":      "Invalid Host Grove",
		"gitRemote": "https://github.com/test/invalid-host-grove",
		"brokerId":    "non-existent-host-id",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 (validation error), got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Error.Code != ErrCodeValidationError {
		t.Errorf("expected error code %q, got %q", ErrCodeValidationError, errResp.Error.Code)
	}
}

func TestAddProvider(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a grove
	grove := &store.Grove{
		ID:        "grove_contrib_test",
		Slug:      "contrib-test",
		Name:      "Provider Test Grove",
		GitRemote: "https://github.com/test/contrib-test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a broker
	broker := &store.RuntimeBroker{
		ID:     "host_contrib_test",
		Name:   "Provider Test Host",
		Slug:   "contrib-test-host",
				Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add provider via API
	body := map[string]interface{}{
		"brokerId":    broker.ID,
		"localPath": "/home/user/project/.scion",
		"mode":      "connected",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+grove.ID+"/providers", body)
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AddProviderResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Provider == nil {
		t.Fatal("expected provider in response")
	}
	if resp.Provider.BrokerID != broker.ID {
		t.Errorf("expected broker ID %q, got %q", broker.ID, resp.Provider.BrokerID)
	}
	if resp.Provider.LocalPath != "/home/user/project/.scion" {
		t.Errorf("expected localPath, got %q", resp.Provider.LocalPath)
	}

	// Verify grove now has default runtime broker set
	updatedGrove, err := s.GetGrove(ctx, grove.ID)
	if err != nil {
		t.Fatalf("failed to get updated grove: %v", err)
	}
	if updatedGrove.DefaultRuntimeBrokerID != broker.ID {
		t.Errorf("expected default runtime broker to be set to %q, got %q", broker.ID, updatedGrove.DefaultRuntimeBrokerID)
	}
}

func TestListProviders(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a grove
	grove := &store.Grove{
		ID:      "grove_list_contrib",
		Slug:    "list-contrib",
		Name:    "List Providers Grove",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create and add a broker as provider
	broker := &store.RuntimeBroker{
		ID:     "host_list_contrib",
		Name:   "List Providers Host",
		Slug:   "list-contrib-host",
				Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	contrib := &store.GroveProvider{
		GroveID:   grove.ID,
		BrokerID:    broker.ID,
		BrokerName:  broker.Name,
		LocalPath: "/test/path",
				Status:    store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add provider: %v", err)
	}

	// List providers
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groves/"+grove.ID+"/providers", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string][]store.GroveProvider
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	providers := resp["providers"]
	if len(providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(providers))
	}
	if providers[0].BrokerID != broker.ID {
		t.Errorf("expected broker ID %q, got %q", broker.ID, providers[0].BrokerID)
	}
}

func TestGroveGetByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:        "grove_gettest",
		Slug:      "get-test",
		Name:      "Get Test",
		GitRemote: "https://github.com/test/get-test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groves/grove_gettest", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Grove
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != "grove_gettest" {
		t.Errorf("expected ID 'grove_gettest', got %q", resp.ID)
	}
}

// ============================================================================
// RuntimeBroker Endpoint Tests
// ============================================================================

func TestRuntimeBrokerList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:            "host_test1",
		Name:          "Test Host",
		Slug:          "test-host",
				Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}
}

func TestRuntimeBrokerListByName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two brokers with different names
	broker1 := &store.RuntimeBroker{
		ID:            "host_name_test1",
		Name:          "Alpha Host",
		Slug:          "alpha-host",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	broker2 := &store.RuntimeBroker{
		ID:            "host_name_test2",
		Name:          "Beta Host",
		Slug:          "beta-host",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker1); err != nil {
		t.Fatalf("failed to create runtime broker 1: %v", err)
	}
	if err := s.CreateRuntimeBroker(ctx, broker2); err != nil {
		t.Fatalf("failed to create runtime broker 2: %v", err)
	}

	// Test filter by exact name
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?name=Alpha+Host", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}
	if len(resp.Brokers) > 0 && resp.Brokers[0].Name != "Alpha Host" {
		t.Errorf("expected broker name 'Alpha Host', got %q", resp.Brokers[0].Name)
	}

	// Test case-insensitive filter
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?name=beta+host", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}

	// Test no match
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?name=nonexistent", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 0 {
		t.Errorf("expected 0 brokers, got %d", len(resp.Brokers))
	}
}

func TestRuntimeBrokerGetByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:            "host_gettest",
		Name:          "Get Test Host",
		Slug:          "get-test-host",
				Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/host_gettest", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.RuntimeBroker
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != "host_gettest" {
		t.Errorf("expected ID 'host_gettest', got %q", resp.ID)
	}
}

func TestRuntimeBrokerListWithGroveLocalPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a grove
	grove := &store.Grove{
		ID:         "grove_localpath_test",
		Name:       "Local Path Test Grove",
		Slug:       "local-path-test",
		Visibility: store.VisibilityPrivate,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:            "host_localpath_test",
		Name:          "Local Path Test Host",
		Slug:          "local-path-test-host",
				Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add broker as grove provider with a local path
	contrib := &store.GroveProvider{
		GroveID:   grove.ID,
		BrokerID:    broker.ID,
		BrokerName:  broker.Name,
		LocalPath: "/path/to/project/.scion",
				Status:    store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	// List runtime brokers filtered by grove - should include localPath
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?groveId=grove_localpath_test", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersWithProviderResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}

	if resp.Brokers[0].ID != "host_localpath_test" {
		t.Errorf("expected broker ID 'host_localpath_test', got %q", resp.Brokers[0].ID)
	}

	if resp.Brokers[0].LocalPath != "/path/to/project/.scion" {
		t.Errorf("expected localPath '/path/to/project/.scion', got %q", resp.Brokers[0].LocalPath)
	}

	// List all runtime brokers (no grove filter) - should NOT include localPath field structure
	// (uses ListRuntimeBrokersResponse, not ListRuntimeBrokersWithProviderResponse)
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 ListRuntimeBrokersResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp2.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp2.Brokers))
	}
}

// ============================================================================
// Two-Phase Broker Registration Tests
// ============================================================================

// testServerWithBrokerAuth creates a test server with broker auth enabled.
func testServerWithBrokerAuth(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	srv := New(cfg, s)
	return srv, s
}

func TestBrokerRegistrationTwoPhaseFlow(t *testing.T) {
	srv, _ := testServerWithBrokerAuth(t)

	// Phase 1: Create broker registration (requires admin auth)
	createBody := map[string]interface{}{
		"name":         "two-phase-host",
		"capabilities": []string{"sync", "attach"},
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/brokers", createBody)
	if rec1.Code != http.StatusCreated {
		t.Errorf("Phase 1: expected status 201, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var createResp CreateBrokerRegistrationResponse
	if err := json.NewDecoder(rec1.Body).Decode(&createResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}

	if createResp.BrokerID == "" {
		t.Error("expected brokerId to be set")
	}
	if createResp.JoinToken == "" {
		t.Error("expected joinToken to be set")
	}
	if createResp.ExpiresAt.IsZero() {
		t.Error("expected expiresAt to be set")
	}

	// Phase 2: Complete broker join (unauthenticated - join token is auth)
	joinBody := map[string]interface{}{
		"brokerId":       createResp.BrokerID,
		"joinToken":    createResp.JoinToken,
		"hostname":     "test-machine",
		"version":      "1.0.0",
		"capabilities": []string{"sync", "attach"},
	}

	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join", joinBody)
	if rec2.Code != http.StatusOK {
		t.Errorf("Phase 2: expected status 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var joinResp BrokerJoinResponse
	if err := json.NewDecoder(rec2.Body).Decode(&joinResp); err != nil {
		t.Fatalf("failed to decode join response: %v", err)
	}

	if joinResp.SecretKey == "" {
		t.Error("expected secretKey to be set")
	}
	if joinResp.BrokerID != createResp.BrokerID {
		t.Errorf("expected brokerId %q, got %q", createResp.BrokerID, joinResp.BrokerID)
	}

	// Phase 3: Register grove with brokerId
	groveBody := map[string]interface{}{
		"name":      "Two Phase Grove",
		"gitRemote": "https://github.com/test/twophase",
		"brokerId":    joinResp.BrokerID,
	}

	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", groveBody)
	if rec3.Code != http.StatusOK {
		t.Errorf("Phase 3: expected status 200, got %d: %s", rec3.Code, rec3.Body.String())
	}

	var groveResp RegisterGroveResponse
	if err := json.NewDecoder(rec3.Body).Decode(&groveResp); err != nil {
		t.Fatalf("failed to decode grove response: %v", err)
	}

	if !groveResp.Created {
		t.Error("expected grove to be created")
	}
	if groveResp.Broker == nil {
		t.Error("expected broker in response")
	} else if groveResp.Broker.ID != joinResp.BrokerID {
		t.Errorf("expected broker ID %q, got %q", joinResp.BrokerID, groveResp.Broker.ID)
	}

	// The new flow should NOT return a secretKey from grove registration
	if groveResp.SecretKey != "" {
		t.Error("expected secretKey to be empty in new two-phase flow")
	}
}

func TestBrokerJoinWithInvalidToken(t *testing.T) {
	srv, _ := testServerWithBrokerAuth(t)

	// Phase 1: Create broker
	createBody := map[string]interface{}{
		"name": "invalid-token-host",
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/brokers", createBody)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("failed to create broker: %s", rec1.Body.String())
	}

	var createResp CreateBrokerRegistrationResponse
	if err := json.NewDecoder(rec1.Body).Decode(&createResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}

	// Try to join with invalid token
	joinBody := map[string]interface{}{
		"brokerId":    createResp.BrokerID,
		"joinToken": "invalid_token",
		"hostname":  "test-machine",
	}

	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join", joinBody)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for invalid token, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// ============================================================================
// Template Endpoint Tests
// ============================================================================

func TestTemplateList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	template := &store.Template{
		ID:         "tmpl_test1",
		Slug:       "test-template",
		Name:       "Test Template",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateTemplate(ctx, template); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/templates", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListTemplatesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Templates) != 1 {
		t.Errorf("expected 1 template, got %d", len(resp.Templates))
	}
}

func TestTemplateCreate(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"slug":       "new-template",
		"name":       "New Template",
		"harness":    "claude",
		"scope":      "global",
		"visibility": "private",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateTemplateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Template == nil {
		t.Fatalf("expected template in response, got nil")
	}

	if resp.Template.Slug != "new-template" {
		t.Errorf("expected slug 'new-template', got %q", resp.Template.Slug)
	}

	if resp.Template.Visibility != store.VisibilityPrivate {
		t.Errorf("expected visibility 'private', got %q", resp.Template.Visibility)
	}
}

// ============================================================================
// User Endpoint Tests
// ============================================================================

func TestUserList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          "user_test1",
		Email:       "test@example.com",
		DisplayName: "Test User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/users", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListUsersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Users) != 1 {
		t.Errorf("expected 1 user, got %d", len(resp.Users))
	}
}

func TestUserCreate(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"email":       "newuser@example.com",
		"displayName": "New User",
		"role":        "admin",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/users", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.User
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Email != "newuser@example.com" {
		t.Errorf("expected email 'newuser@example.com', got %q", resp.Email)
	}

	if resp.Role != store.UserRoleAdmin {
		t.Errorf("expected role 'admin', got %q", resp.Role)
	}
}

// ============================================================================
// Error Handling Tests
// ============================================================================

func TestMethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	// Try PATCH on /healthz which doesn't support it
	rec := doRequest(t, srv, http.MethodPatch, "/healthz", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestInvalidJSON(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a grove first
	grove := &store.Grove{
		ID:        "grove_invalid",
		Slug:      "invalid-grove",
		Name:      "Invalid Grove",
		GitRemote: "https://github.com/test/invalid",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader([]byte("{invalid json")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// CORS Tests
// ============================================================================

func TestCORSHeaders(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:3000")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	corsOrigin := rec.Header().Get("Access-Control-Allow-Origin")
	if corsOrigin != "http://localhost:3000" {
		t.Errorf("expected CORS origin 'http://localhost:3000', got %q", corsOrigin)
	}
}

func TestCORSPreflight(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/agents", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d", rec.Code)
	}

	corsOrigin := rec.Header().Get("Access-Control-Allow-Origin")
	if corsOrigin != "http://localhost:3000" {
		t.Errorf("expected CORS origin 'http://localhost:3000', got %q", corsOrigin)
	}
}
