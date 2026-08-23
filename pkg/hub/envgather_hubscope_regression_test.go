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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestBuildEnvGatherResponse_HubScopeSecretWarning is a regression test for #721.
//
// When a broker reports a needed key (e.g., GEMINI_API_KEY) and the Hub has a
// hub-scope secret for that key, buildEnvGatherResponse should emit a warning
// indicating the key exists at hub scope. Currently, the cross-check at
// handlers_agents_core.go:1172-1209 only checks user and project scope for
// both env vars and secrets, missing hub-scope entirely.
//
// This test SHOULD PASS once the fix is applied. With the current code it will
// FAIL because the hub-scope check is missing.
func TestBuildEnvGatherResponse_HubScopeSecretWarning(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create a hub-scope secret for GEMINI_API_KEY
	if err := st.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-hubscope-gemini"),
		Key:            "GEMINI_API_KEY",
		EncryptedValue: "encrypted-gemini-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GEMINI_API_KEY",
		Scope:          store.ScopeHub,
		ScopeID:        "test-hub-id", // matches testServer's hub ID
		InjectionMode:  store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	// Set up the local secret backend so the cross-check can query it
	backend := secret.NewLocalBackend(st, "test-hub-id", "test-secret")
	srv.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:        "agent-hubscope-warn",
		Name:      "hubscope-warn-agent",
		OwnerID:   "some-owner",
		ProjectID: "some-project",
	}

	// Simulate broker response: GEMINI_API_KEY is required but not satisfied
	brokerReqs := &RemoteEnvRequirementsResponse{
		AgentID:  agent.ID,
		Required: []string{"GEMINI_API_KEY"},
		HubHas:   []string{},
		Needs:    []string{"GEMINI_API_KEY"},
	}

	resp := srv.buildEnvGatherResponse(ctx, agent, brokerReqs)

	// The cross-check should detect that GEMINI_API_KEY exists at hub scope
	// and emit a warning. With the current code, no warning is emitted because
	// only user and project scopes are checked.
	if len(resp.HubWarnings) == 0 {
		t.Error("expected HubWarnings for GEMINI_API_KEY (exists at hub scope but not dispatched); got none")
	}

	// Verify the warning mentions hub scope
	found := false
	for _, w := range resp.HubWarnings {
		if strings.Contains(w, "GEMINI_API_KEY") && strings.Contains(w, "hub") {
			found = true
			break
		}
	}
	if !found && len(resp.HubWarnings) > 0 {
		t.Errorf("expected warning mentioning GEMINI_API_KEY at hub scope; got warnings: %v", resp.HubWarnings)
	}
}

// TestBuildEnvGatherResponse_HubScopeEnvVarWarning is a regression test for #721.
//
// When a broker reports a needed key and the Hub has a hub-scope env var for
// that key, buildEnvGatherResponse should emit a warning. Currently, the
// cross-check only queries user and project scope for env vars.
func TestBuildEnvGatherResponse_HubScopeEnvVarWarning(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create a hub-scope env var for GEMINI_API_KEY with as_needed mode
	if err := st.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("env-hubscope-gemini"),
		Key:           "GEMINI_API_KEY",
		Value:         "hub-gemini-key",
		Scope:         store.ScopeHub,
		ScopeID:       "test-hub-id",
		InjectionMode: store.InjectionModeAsNeeded,
	}); err != nil {
		t.Fatal(err)
	}

	agent := &store.Agent{
		ID:        "agent-hubscope-env-warn",
		Name:      "hubscope-env-warn-agent",
		OwnerID:   "some-owner",
		ProjectID: "some-project",
	}

	brokerReqs := &RemoteEnvRequirementsResponse{
		AgentID:  agent.ID,
		Required: []string{"GEMINI_API_KEY"},
		HubHas:   []string{},
		Needs:    []string{"GEMINI_API_KEY"},
	}

	resp := srv.buildEnvGatherResponse(ctx, agent, brokerReqs)

	if len(resp.HubWarnings) == 0 {
		t.Error("expected HubWarnings for GEMINI_API_KEY (exists as hub-scope env var but not dispatched); got none")
	}

	found := false
	for _, w := range resp.HubWarnings {
		if strings.Contains(w, "GEMINI_API_KEY") && strings.Contains(w, "hub") {
			found = true
			break
		}
	}
	if !found && len(resp.HubWarnings) > 0 {
		t.Errorf("expected warning mentioning GEMINI_API_KEY at hub scope; got warnings: %v", resp.HubWarnings)
	}
}

// TestResolveSecrets_HubScope_AsNeeded_Filtered is a regression test for #721.
//
// Hub-scope secrets with injection_mode=as_needed are correctly filtered out by
// resolveSecrets. This test documents the current behavior — the filtering
// itself is correct. The bug is that the env-gather cross-check doesn't detect
// that the filtered secret exists when it should have been available.
func TestResolveSecrets_HubScope_AsNeeded_Filtered(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	agent := &store.Agent{
		ID:              "agent-hubscope-filter",
		Name:            "hubscope-filter",
		Slug:            "hubscope-filter",
		ProjectID:       "project-hubscope-1",
		OwnerID:         "user-hubscope-1",
		RuntimeBrokerID: "broker-hubscope-1",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	hubID := "hub-hubscope-test"
	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, nil)
	d.SetHubID(hubID)
	d.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "GEMINI_API_KEY",
					SecretType:    "environment",
					Target:        "GEMINI_API_KEY",
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "as_needed",
				},
				Value: "gemini-key-value",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:          "ALWAYS_HUB_SECRET",
					SecretType:    "environment",
					Target:        "ALWAYS_HUB_SECRET",
					Scope:         "hub",
					ScopeID:       hubID,
					InjectionMode: "always",
				},
				Value: "always-hub-value",
			},
		},
	})

	resolved, err := d.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets: %v", err)
	}

	byName := make(map[string]ResolvedSecret, len(resolved))
	for _, r := range resolved {
		byName[r.Name] = r
	}

	// Hub-scope as_needed should be filtered out
	if _, ok := byName["GEMINI_API_KEY"]; ok {
		t.Error("GEMINI_API_KEY (hub scope, as_needed) should be filtered out")
	}

	// Hub-scope always should be included
	if r, ok := byName["ALWAYS_HUB_SECRET"]; !ok {
		t.Error("ALWAYS_HUB_SECRET (hub scope, always) should be present")
	} else if r.Value != "always-hub-value" {
		t.Errorf("ALWAYS_HUB_SECRET value = %q, want %q", r.Value, "always-hub-value")
	}
}
