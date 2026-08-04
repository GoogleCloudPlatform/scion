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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- resolveAsNeededForKeys tests ---

func TestResolveAsNeededForKeys_EnvVars(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-resolve"

	// Create hub-scope as_needed env var
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-asneeded-1"),
		Key:           "GEMINI_API_KEY",
		Value:         "gemini-key-value",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	// Create hub-scope always env var (should NOT be returned)
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-always-1"),
		Key:           "ALWAYS_KEY",
		Value:         "always-value",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAlways,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:            "agent-resolve-test",
		Name:          "resolve-test",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"GEMINI_API_KEY", "OTHER_KEY"}, nil)

	if v, ok := result["GEMINI_API_KEY"]; !ok {
		t.Error("expected GEMINI_API_KEY in result")
	} else if v != "gemini-key-value" {
		t.Errorf("GEMINI_API_KEY = %q, want %q", v, "gemini-key-value")
	}

	if _, ok := result["ALWAYS_KEY"]; ok {
		t.Error("ALWAYS_KEY (always mode) should not be in result")
	}

	if _, ok := result["OTHER_KEY"]; ok {
		t.Error("OTHER_KEY should not be in result (not in storage)")
	}
}

func TestResolveAsNeededForKeys_Secrets(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-secrets"

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)
	d.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "gemini-secret",
					SecretType:    "environment",
					Target:        "GEMINI_API_KEY",
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "as_needed",
				},
				Value: "secret-gemini-value",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:          "always-secret",
					SecretType:    "environment",
					Target:        "ALWAYS_SECRET",
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "always",
				},
				Value: "always-secret-value",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:          "file-secret",
					SecretType:    "file",
					Target:        "/tmp/secret.json",
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "as_needed",
				},
				Value: "file-secret-content",
			},
		},
	})

	agent := &store.Agent{
		ID:            "agent-secret-test",
		Name:          "secret-test",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"GEMINI_API_KEY", "ALWAYS_SECRET", "/tmp/secret.json"}, nil)

	if v, ok := result["GEMINI_API_KEY"]; !ok {
		t.Error("expected GEMINI_API_KEY in result (as_needed environment secret)")
	} else if v != "secret-gemini-value" {
		t.Errorf("GEMINI_API_KEY = %q, want %q", v, "secret-gemini-value")
	}

	if _, ok := result["ALWAYS_SECRET"]; ok {
		t.Error("ALWAYS_SECRET should not be in result (always mode)")
	}

	if _, ok := result["/tmp/secret.json"]; ok {
		t.Error("file-type secret should not be in result (only environment-type handled)")
	}
}

func TestResolveAsNeededForKeys_SecretNameFallback(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-fallback"

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)
	d.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "MY_API_KEY",
					SecretType:    "environment",
					Target:        "", // Empty target — should fall back to Name
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "as_needed",
				},
				Value: "api-key-value",
			},
		},
	})

	agent := &store.Agent{
		ID:            "agent-fallback-test",
		Name:          "fallback-test",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"MY_API_KEY"}, nil)

	if v, ok := result["MY_API_KEY"]; !ok {
		t.Error("expected MY_API_KEY in result (secret with empty Target should fall back to Name)")
	} else if v != "api-key-value" {
		t.Errorf("MY_API_KEY = %q, want %q", v, "api-key-value")
	}
}

func TestResolveAsNeededForKeys_ScopePrecedence(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-precedence"

	// Create hub-scope as_needed env var
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-hub-prec"),
		Key:           "API_KEY",
		Value:         "hub-value",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	// Create user-scope as_needed env var (higher precedence)
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-user-prec"),
		Key:           "API_KEY",
		Value:         "user-value",
		Scope:         store.ScopeUser,
		ScopeID:       "user-prec-1",
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:            "agent-prec-test",
		Name:          "prec-test",
		OwnerID:       "user-prec-1",
		ProjectID:     "project-prec-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"API_KEY"}, nil)

	// User scope should win over hub scope (higher precedence, last-wins)
	if v, ok := result["API_KEY"]; !ok {
		t.Error("expected API_KEY in result")
	} else if v != "user-value" {
		t.Errorf("API_KEY = %q, want %q (user scope should win over hub scope)", v, "user-value")
	}
}

func TestResolveAsNeededForKeys_NoBackend(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID("test-hub-nobackend")

	agent := &store.Agent{
		ID:            "agent-nobackend",
		Name:          "nobackend",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	// Should not panic with nil secret backend
	result := d.resolveAsNeededForKeys(ctx, agent, []string{"SOME_KEY"}, nil)

	if len(result) != 0 {
		t.Errorf("expected empty result with no matching env vars and no secret backend, got %v", result)
	}
}

func TestResolveAsNeededForKeys_EmptyKeys(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID("test-hub-empty")

	agent := &store.Agent{
		ID:            "agent-empty-keys",
		Name:          "empty-keys",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{}, nil)

	if len(result) != 0 {
		t.Errorf("expected empty result for empty keys, got %v", result)
	}
}

func TestResolveAsNeededForKeys_EnvVarPriorityOverSecret(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-priority"

	// Create hub-scope as_needed env var
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-priority"),
		Key:           "SHARED_KEY",
		Value:         "env-var-value",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)
	d.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "shared-secret",
					SecretType:    "environment",
					Target:        "SHARED_KEY",
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "as_needed",
				},
				Value: "secret-value",
			},
		},
	})

	agent := &store.Agent{
		ID:            "agent-priority-test",
		Name:          "priority-test",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"SHARED_KEY"}, nil)

	// Env var should be found first; secret should not overwrite (alreadySet check)
	if v, ok := result["SHARED_KEY"]; !ok {
		t.Error("expected SHARED_KEY in result")
	} else if v != "env-var-value" {
		t.Errorf("SHARED_KEY = %q, want %q (env var should take precedence over secret for same key)", v, "env-var-value")
	}
}

// --- Two-pass flow integration tests ---

// gatherMockBrokerClient is a test mock that can return env requirements from
// CreateAgentWithGather, simulating the broker's 202 response with needs.
type gatherMockBrokerClient struct {
	mockRuntimeBrokerClient

	// gatherEnvReqs is returned on the first CreateAgentWithGather call.
	gatherEnvReqs *RemoteEnvRequirementsResponse

	// finalizeEnvReqs is returned on subsequent CreateAgentWithGather calls
	// (simulating the DispatchFinalizeEnv replay). If nil, the finalize
	// succeeds (returns a normal response).
	finalizeEnvReqs *RemoteEnvRequirementsResponse

	callCount int
}

func (m *gatherMockBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	m.callCount++
	m.createCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastCreateReq = req

	if m.returnErr != nil {
		return nil, nil, m.returnErr
	}

	// First call: return env requirements (simulates broker 202)
	if m.callCount == 1 && m.gatherEnvReqs != nil {
		return nil, m.gatherEnvReqs, nil
	}

	// Subsequent calls (finalize): check if still missing
	if m.callCount > 1 && m.finalizeEnvReqs != nil {
		return nil, m.finalizeEnvReqs, nil
	}

	// Success case: all env satisfied
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:    req.ID,
			Slug:  req.Slug,
			Name:  req.Name,
			Phase: "running",
		},
		Created: true,
	}, nil, nil
}

func TestDispatchAgentCreateWithGather_TwoPass_FullResolution(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-twopass"

	broker := &store.RuntimeBroker{
		ID:       tid("broker-twopass"),
		Name:     "twopass-broker",
		Slug:     "twopass-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// Create hub-scope as_needed env var that should satisfy the need
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-twopass"),
		Key:           "GEMINI_API_KEY",
		Value:         "resolved-gemini-key",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &gatherMockBrokerClient{
		gatherEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "agent-twopass",
			Required: []string{"GEMINI_API_KEY"},
			HubHas:   []string{},
			Needs:    []string{"GEMINI_API_KEY"},
		},
		// finalizeEnvReqs is nil → finalize succeeds
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:              tid("agent-twopass"),
		Name:            "twopass-agent",
		Slug:            "twopass-agent",
		ProjectID:       "project-twopass",
		OwnerID:         "user-twopass",
		RuntimeBrokerID: broker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	envReqs, err := d.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreateWithGather: %v", err)
	}

	// The second pass should have resolved all needs — no env requirements returned
	if envReqs != nil {
		t.Errorf("expected nil envReqs (all needs resolved by as_needed), got %+v", envReqs)
	}

	// The broker should have been called twice (initial + finalize)
	if mockClient.callCount != 2 {
		t.Errorf("expected 2 broker calls (initial + finalize), got %d", mockClient.callCount)
	}
}

func TestDispatchAgentCreateWithGather_TwoPass_PartialResolution(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-partial"

	broker := &store.RuntimeBroker{
		ID:       tid("broker-partial"),
		Name:     "partial-broker",
		Slug:     "partial-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// Only create one of the two needed keys
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-partial"),
		Key:           "KNOWN_KEY",
		Value:         "known-value",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &gatherMockBrokerClient{
		gatherEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "agent-partial",
			Required: []string{"KNOWN_KEY", "UNKNOWN_KEY"},
			HubHas:   []string{},
			Needs:    []string{"KNOWN_KEY", "UNKNOWN_KEY"},
		},
		// Finalize will still report UNKNOWN_KEY as missing
		finalizeEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "agent-partial",
			Required: []string{"KNOWN_KEY", "UNKNOWN_KEY"},
			HubHas:   []string{"KNOWN_KEY"},
			Needs:    []string{"UNKNOWN_KEY"},
		},
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:              tid("agent-partial"),
		Name:            "partial-agent",
		Slug:            "partial-agent",
		ProjectID:       "project-partial",
		OwnerID:         "user-partial",
		RuntimeBrokerID: broker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	envReqs, err := d.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreateWithGather: %v", err)
	}

	// Should return remaining needs
	if envReqs == nil {
		t.Fatal("expected non-nil envReqs (partial resolution)")
	}
	if len(envReqs.Needs) != 1 || envReqs.Needs[0] != "UNKNOWN_KEY" {
		t.Errorf("expected Needs=[UNKNOWN_KEY], got %v", envReqs.Needs)
	}
}

func TestDispatchAgentCreateWithGather_TwoPass_NoMatch(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-nomatch"

	broker := &store.RuntimeBroker{
		ID:       tid("broker-nomatch"),
		Name:     "nomatch-broker",
		Slug:     "nomatch-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// No as_needed env vars or secrets for the needed key
	mockClient := &gatherMockBrokerClient{
		gatherEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "agent-nomatch",
			Required: []string{"MISSING_KEY"},
			HubHas:   []string{},
			Needs:    []string{"MISSING_KEY"},
		},
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:              tid("agent-nomatch"),
		Name:            "nomatch-agent",
		Slug:            "nomatch-agent",
		ProjectID:       "project-nomatch",
		OwnerID:         "user-nomatch",
		RuntimeBrokerID: broker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	envReqs, err := d.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreateWithGather: %v", err)
	}

	// No as_needed match — should pass through the original needs
	if envReqs == nil {
		t.Fatal("expected non-nil envReqs (no as_needed match)")
	}
	if len(envReqs.Needs) != 1 || envReqs.Needs[0] != "MISSING_KEY" {
		t.Errorf("expected Needs=[MISSING_KEY], got %v", envReqs.Needs)
	}

	// Should only have been called once (no finalize since no matches)
	if mockClient.callCount != 1 {
		t.Errorf("expected 1 broker call (no finalize needed), got %d", mockClient.callCount)
	}
}

func TestDispatchAgentCreateWithGather_NoNeeds(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("broker-noneeds"),
		Name:     "noneeds-broker",
		Slug:     "noneeds-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// Broker returns no needs (all env satisfied)
	mockClient := &gatherMockBrokerClient{}

	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID("test-hub-noneeds")

	agent := &store.Agent{
		ID:              tid("agent-noneeds"),
		Name:            "noneeds-agent",
		Slug:            "noneeds-agent",
		ProjectID:       "project-noneeds",
		OwnerID:         "user-noneeds",
		RuntimeBrokerID: broker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	envReqs, err := d.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreateWithGather: %v", err)
	}

	if envReqs != nil {
		t.Errorf("expected nil envReqs (no needs), got %+v", envReqs)
	}

	// Should only be called once
	if mockClient.callCount != 1 {
		t.Errorf("expected 1 broker call, got %d", mockClient.callCount)
	}
}

// --- resolveAsNeededForKeys tests with alternatives ---

func TestResolveAsNeededForKeys_AlternativeKeyMatching(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-alternatives"

	// Store GOOGLE_CLOUD_LOCATION (an alternative name, not the canonical key)
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-alt-location"),
		Key:           "GOOGLE_CLOUD_LOCATION",
		Value:         "us-central1",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:            "agent-alt-test",
		Name:          "alt-test",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	// The broker reports canonical key GOOGLE_CLOUD_REGION in Needs,
	// with alternatives that include GOOGLE_CLOUD_LOCATION.
	alternatives := map[string][]string{
		"GOOGLE_CLOUD_REGION": {"CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"GOOGLE_CLOUD_REGION"}, alternatives)

	// The stored var GOOGLE_CLOUD_LOCATION should match via alternatives and be
	// stored under the canonical key GOOGLE_CLOUD_REGION.
	if v, ok := result["GOOGLE_CLOUD_REGION"]; !ok {
		t.Error("expected GOOGLE_CLOUD_REGION in result (matched via alternative GOOGLE_CLOUD_LOCATION)")
	} else if v != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", v, "us-central1")
	}

	// GOOGLE_CLOUD_LOCATION should NOT appear as a separate key in the result
	if _, ok := result["GOOGLE_CLOUD_LOCATION"]; ok {
		t.Error("GOOGLE_CLOUD_LOCATION should not be in result as a separate key; it should be mapped to the canonical key")
	}
}

func TestResolveAsNeededForKeys_AlternativeKeyMatchingSecrets(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-alt-secrets"

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)
	d.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "cloud-location",
					SecretType:    "environment",
					Target:        "GOOGLE_CLOUD_LOCATION",
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "as_needed",
				},
				Value: "europe-west1",
			},
		},
	})

	agent := &store.Agent{
		ID:            "agent-alt-secret-test",
		Name:          "alt-secret-test",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	alternatives := map[string][]string{
		"GOOGLE_CLOUD_REGION": {"CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"GOOGLE_CLOUD_REGION"}, alternatives)

	// The secret targeting GOOGLE_CLOUD_LOCATION should match and be stored under
	// the canonical key GOOGLE_CLOUD_REGION.
	if v, ok := result["GOOGLE_CLOUD_REGION"]; !ok {
		t.Error("expected GOOGLE_CLOUD_REGION in result (matched via secret targeting alternative GOOGLE_CLOUD_LOCATION)")
	} else if v != "europe-west1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", v, "europe-west1")
	}
}

func TestResolveAsNeededForKeys_CanonicalKeyWinsOverAlternative(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-canonical-wins"

	// Store BOTH the canonical key and an alternative in the same scope.
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-canonical"),
		Key:           "GOOGLE_CLOUD_REGION",
		Value:         "us-central1",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-alt-dup"),
		Key:           "GOOGLE_CLOUD_LOCATION",
		Value:         "europe-west4",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:            "agent-canonical-wins",
		Name:          "canonical-wins",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	alternatives := map[string][]string{
		"GOOGLE_CLOUD_REGION": {"CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"},
	}

	result := d.resolveAsNeededForKeys(ctx, agent, []string{"GOOGLE_CLOUD_REGION"}, alternatives)

	// The canonical key's value must be preserved — the alternative must not
	// overwrite it regardless of ListEnvVars iteration order.
	if v, ok := result["GOOGLE_CLOUD_REGION"]; !ok {
		t.Error("expected GOOGLE_CLOUD_REGION in result")
	} else if v != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q (canonical value should win over alternative)", v, "us-central1")
	}

	// Only one entry should be in the result.
	if len(result) != 1 {
		t.Errorf("expected 1 result entry, got %d: %v", len(result), result)
	}
}

func TestResolveAsNeededForKeys_NilAlternatives(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-nil-alt"

	// Store canonical key directly
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-nil-alt"),
		Key:           "GOOGLE_CLOUD_REGION",
		Value:         "us-east1",
		Scope:         store.ScopeHub,
		ScopeID:       hubID,
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)

	agent := &store.Agent{
		ID:            "agent-nil-alt-test",
		Name:          "nil-alt-test",
		OwnerID:       "user-1",
		ProjectID:     "project-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	// nil alternatives should behave exactly like the old code path
	result := d.resolveAsNeededForKeys(ctx, agent, []string{"GOOGLE_CLOUD_REGION"}, nil)

	if v, ok := result["GOOGLE_CLOUD_REGION"]; !ok {
		t.Error("expected GOOGLE_CLOUD_REGION in result (exact match, nil alternatives)")
	} else if v != "us-east1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", v, "us-east1")
	}
}

// --- resolveSecrets tests: file-type and variable-type secrets pass through ---

func TestResolveSecrets_FileTypePassesThroughAsNeeded(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	hubID := "test-hub-file-passthrough"

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetHubID(hubID)
	d.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "AGY_TOKEN",
					SecretType:    store.SecretTypeFile,
					Target:        "~/.gemini/antigravity-cli/antigravity-oauth-token",
					Scope:         "project",
					ScopeID:       "project-file-test",
					InjectionMode: store.InjectionModeAsNeeded,
				},
				Value: "oauth-token-content",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:          "ENV_SECRET",
					SecretType:    store.SecretTypeEnvironment,
					Target:        "MY_ENV",
					Scope:         "project",
					ScopeID:       "project-file-test",
					InjectionMode: store.InjectionModeAsNeeded,
				},
				Value: "env-secret-value",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:          "VAR_SECRET",
					SecretType:    store.SecretTypeVariable,
					Target:        "MY_VAR",
					Scope:         "project",
					ScopeID:       "project-file-test",
					InjectionMode: store.InjectionModeAsNeeded,
				},
				Value: "var-secret-value",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:          "ALWAYS_ENV",
					SecretType:    store.SecretTypeEnvironment,
					Target:        "ALWAYS_KEY",
					Scope:         "project",
					ScopeID:       "project-file-test",
					InjectionMode: store.InjectionModeAlways,
				},
				Value: "always-env-value",
			},
		},
	})

	agent := &store.Agent{
		ID:        "agent-file-passthrough",
		Name:      "file-passthrough",
		OwnerID:   "user-1",
		ProjectID: "project-file-test",
	}

	result, err := d.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets: %v", err)
	}

	// Build lookup by name for easier assertions.
	byName := make(map[string]ResolvedSecret, len(result))
	for _, rs := range result {
		byName[rs.Name] = rs
	}

	// File-type secret with as_needed should pass through.
	if rs, ok := byName["AGY_TOKEN"]; !ok {
		t.Error("expected AGY_TOKEN (file-type, as_needed) to pass through resolveSecrets")
	} else {
		if rs.Type != store.SecretTypeFile {
			t.Errorf("AGY_TOKEN type = %q, want %q", rs.Type, store.SecretTypeFile)
		}
		if rs.Target != "~/.gemini/antigravity-cli/antigravity-oauth-token" {
			t.Errorf("AGY_TOKEN target = %q, want %q", rs.Target, "~/.gemini/antigravity-cli/antigravity-oauth-token")
		}
		if rs.Value != "oauth-token-content" {
			t.Errorf("AGY_TOKEN value = %q, want %q", rs.Value, "oauth-token-content")
		}
	}

	// Variable-type secret with as_needed should also pass through.
	if _, ok := byName["VAR_SECRET"]; !ok {
		t.Error("expected VAR_SECRET (variable-type, as_needed) to pass through resolveSecrets")
	}

	// Environment-type secret with as_needed should be filtered out
	// (handled by the two-pass env-gather flow instead).
	if _, ok := byName["ENV_SECRET"]; ok {
		t.Error("ENV_SECRET (environment-type, as_needed) should be filtered by resolveSecrets")
	}

	// Environment-type secret with always should pass through.
	if _, ok := byName["ALWAYS_ENV"]; !ok {
		t.Error("expected ALWAYS_ENV (environment-type, always) to pass through resolveSecrets")
	}
}
