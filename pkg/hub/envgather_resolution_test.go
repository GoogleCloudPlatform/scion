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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestResolution_PlainEnvVar verifies that a plain env var stored at user scope
// is resolved and included in the dispatch request's ResolvedEnv.
func TestResolution_PlainEnvVar(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID: "broker-res-1", Name: "res-broker", Slug: "res-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	project := &store.Project{ID: "project-res-1", Name: "res-project", Slug: "res-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-res-1", BrokerID: "broker-res-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Store a plain env var at user scope
	_, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
		ID:      api.NewUUID(),
		Key:     "MY_API_KEY",
		Value:   "plain-api-key-value",
		Scope:   "user",
		ScopeID: "user-res-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())

	agent := &store.Agent{
		ID:              "agent-res-1",
		Name:            "res-agent",
		Slug:            "res-agent",
		ProjectID:         "project-res-1",
		OwnerID:         "user-res-1",
		RuntimeBrokerID: "broker-res-1",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "test")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	if val, ok := req.ResolvedEnv["MY_API_KEY"]; !ok {
		t.Error("expected MY_API_KEY in ResolvedEnv")
	} else if val != "plain-api-key-value" {
		t.Errorf("expected value %q, got %q", "plain-api-key-value", val)
	}
}

// TestResolution_SecretUserScope verifies that a secret stored via the local
// backend at user scope is resolved and included in ResolvedSecrets.
func TestResolution_SecretUserScope(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID: "broker-res-2", Name: "res-broker-2", Slug: "res-broker-2",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	project := &store.Project{ID: "project-res-2", Name: "res-project-2", Slug: "res-project-2"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-res-2", BrokerID: "broker-res-2",
	}); err != nil {
		t.Fatal(err)
	}

	// Store a secret via the local backend
	backend := secret.NewLocalBackend(memStore, "test-hub-id")
	_, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name:       "SECRET_KEY",
		Value:      "secret-key-value",
		SecretType: secret.TypeEnvironment,
		Scope:      secret.ScopeUser,
		ScopeID:    "user-res-2",
	})
	if err != nil {
		t.Fatal(err)
	}

	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())
	dispatcher.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:              "agent-res-2",
		Name:            "res-agent-2",
		Slug:            "res-agent-2",
		ProjectID:         "project-res-2",
		OwnerID:         "user-res-2",
		RuntimeBrokerID: "broker-res-2",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "test")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	if len(req.ResolvedSecrets) == 0 {
		t.Fatal("expected ResolvedSecrets to contain SECRET_KEY")
	}

	found := false
	for _, rs := range req.ResolvedSecrets {
		if rs.Name == "SECRET_KEY" {
			found = true
			if rs.Value != "secret-key-value" {
				t.Errorf("expected secret value %q, got %q", "secret-key-value", rs.Value)
			}
			if rs.Source != "user" {
				t.Errorf("expected source %q, got %q", "user", rs.Source)
			}
		}
	}
	if !found {
		t.Error("SECRET_KEY not found in ResolvedSecrets")
	}
}

// TestResolution_ProjectEnvVar verifies that a project-scoped env var is resolved.
func TestResolution_ProjectEnvVar(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID: "broker-res-3", Name: "res-broker-3", Slug: "res-broker-3",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	project := &store.Project{ID: "project-res-3", Name: "res-project-3", Slug: "res-project-3"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-res-3", BrokerID: "broker-res-3",
	}); err != nil {
		t.Fatal(err)
	}

	// Store a project-scoped env var
	_, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
		ID:      api.NewUUID(),
		Key:     "GROVE_VAR",
		Value:   "project-var-value",
		Scope:   "project",
		ScopeID: "project-res-3",
	})
	if err != nil {
		t.Fatal(err)
	}

	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())

	agent := &store.Agent{
		ID:              "agent-res-3",
		Name:            "res-agent-3",
		Slug:            "res-agent-3",
		ProjectID:         "project-res-3",
		OwnerID:         "user-res-3",
		RuntimeBrokerID: "broker-res-3",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "test")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	if val, ok := req.ResolvedEnv["GROVE_VAR"]; !ok {
		t.Error("expected GROVE_VAR in ResolvedEnv")
	} else if val != "project-var-value" {
		t.Errorf("expected value %q, got %q", "project-var-value", val)
	}
}

// TestResolution_SecretPromotedEnvVar verifies the full round-trip for a
// "secret-promoted" env var: the key is stored via the secret backend (as
// happens with `scion hub env set --secret`) and deleted from the env_vars
// table. The dispatch should still resolve it via the secret backend.
func TestResolution_SecretPromotedEnvVar(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID: "broker-res-4", Name: "res-broker-4", Slug: "res-broker-4",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	project := &store.Project{ID: "project-res-4", Name: "res-project-4", Slug: "res-project-4"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-res-4", BrokerID: "broker-res-4",
	}); err != nil {
		t.Fatal(err)
	}

	backend := secret.NewLocalBackend(memStore, "test-hub-id")

	// Simulate the --secret flow: first store as plain env var, then promote
	// to secret (which deletes the plain env var).
	_, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
		ID:      api.NewUUID(),
		Key:     "GEMINI_API_KEY",
		Value:   "old-plain-value",
		Scope:   "user",
		ScopeID: "user-res-4",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Promote to secret
	_, _, err = backend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      "secret-gemini-value",
		SecretType: secret.TypeEnvironment,
		Scope:      secret.ScopeUser,
		ScopeID:    "user-res-4",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Delete the plain env var (simulates handler line 4184)
	_ = memStore.DeleteEnvVar(ctx, "GEMINI_API_KEY", "user", "user-res-4")

	// Verify: env_vars table should NOT have it
	vars, _ := memStore.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: "user-res-4", Key: "GEMINI_API_KEY"})
	if len(vars) > 0 {
		t.Fatal("expected GEMINI_API_KEY to be deleted from env_vars table after promotion")
	}

	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())
	dispatcher.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:              "agent-res-4",
		Name:            "res-agent-4",
		Slug:            "res-agent-4",
		ProjectID:         "project-res-4",
		OwnerID:         "user-res-4",
		RuntimeBrokerID: "broker-res-4",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "test")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	// Environment-type secrets should be injected into ResolvedEnv so the
	// broker receives them as plain env vars for auth resolution.
	if v, ok := req.ResolvedEnv["GEMINI_API_KEY"]; !ok {
		t.Error("GEMINI_API_KEY should be in ResolvedEnv (injected from env-type secret)")
	} else if v != "secret-gemini-value" {
		t.Errorf("ResolvedEnv GEMINI_API_KEY = %q, want %q", v, "secret-gemini-value")
	}

	// Should also be in ResolvedSecrets (resolved via secret backend)
	found := false
	for _, rs := range req.ResolvedSecrets {
		if rs.Name == "GEMINI_API_KEY" {
			found = true
			if rs.Value != "secret-gemini-value" {
				t.Errorf("expected secret value %q, got %q", "secret-gemini-value", rs.Value)
			}
		}
	}
	if !found {
		t.Fatal("expected GEMINI_API_KEY in ResolvedSecrets after secret promotion")
	}
}
