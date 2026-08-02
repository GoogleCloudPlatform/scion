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

// TestHTTPAgentDispatcher_DispatchAgentRestart_ResolvesEnvFromStorage verifies
// that DispatchAgentRestart resolves env vars from hub storage — the root cause
// fix for issue #723 where GOOGLE_CLOUD_PROJECT was missing on restart.
func TestHTTPAgentDispatcher_DispatchAgentRestart_ResolvesEnvFromStorage(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("broker-restart-env"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	agent := &store.Agent{
		ID:              tid("agent-restart-env"),
		Name:            "restart-env-agent",
		Slug:            "restart-env-agent",
		ProjectID:       tid("project-restart-env"),
		OwnerID:         tid("user-restart-env"),
		RuntimeBrokerID: tid("broker-restart-env"),
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{
				"TEMPLATE_VAR": "from-config",
			},
		},
	}

	// Seed a hub-scope env var (like GOOGLE_CLOUD_PROJECT) with injection_mode=always
	envVar := store.EnvVar{
		ID:            api.NewUUID(),
		Key:           "GOOGLE_CLOUD_PROJECT",
		Value:         "my-gcp-project",
		Scope:         store.ScopeProject,
		ScopeID:       agent.ProjectID,
		InjectionMode: store.InjectionModeAlways,
	}
	if _, err := memStore.UpsertEnvVar(ctx, &envVar); err != nil {
		t.Fatalf("seeding env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	err := dispatcher.DispatchAgentRestart(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentRestart failed: %v", err)
	}

	if !mockClient.restartCalled {
		t.Fatal("expected RestartAgent to be called")
	}

	env := mockClient.lastRestartResolvedEnv

	// GOOGLE_CLOUD_PROJECT should be present from storage resolution
	if got, ok := env["GOOGLE_CLOUD_PROJECT"]; !ok {
		t.Error("GOOGLE_CLOUD_PROJECT missing from restart resolvedEnv — storage env not resolved")
	} else if got != "my-gcp-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", got, "my-gcp-project")
	}

	// Template/config-level env vars should also be present
	if got, ok := env["TEMPLATE_VAR"]; !ok {
		t.Error("TEMPLATE_VAR missing from restart resolvedEnv — AppliedConfig.Env not merged")
	} else if got != "from-config" {
		t.Errorf("TEMPLATE_VAR = %q, want %q", got, "from-config")
	}

	// Identity vars should still be present at highest precedence
	if got := env["SCION_AGENT_ID"]; got != agent.ID {
		t.Errorf("SCION_AGENT_ID = %q, want %q", got, agent.ID)
	}
	if got := env["SCION_PROJECT_ID"]; got != agent.ProjectID {
		t.Errorf("SCION_PROJECT_ID = %q, want %q", got, agent.ProjectID)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentRestart_ResolvesSecrets verifies that
// DispatchAgentRestart resolves type-aware secrets and injects environment-type
// secrets into resolvedEnv.
func TestHTTPAgentDispatcher_DispatchAgentRestart_ResolvesSecrets(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("broker-restart-secrets"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	agent := &store.Agent{
		ID:              tid("agent-restart-secrets"),
		Name:            "restart-secrets-agent",
		Slug:            "restart-secrets-agent",
		ProjectID:       tid("project-restart-secrets"),
		OwnerID:         tid("user-restart-secrets"),
		RuntimeBrokerID: tid("broker-restart-secrets"),
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "API_KEY_SECRET",
					SecretType:    "environment",
					Target:        "API_KEY",
					Scope:         "project",
					ScopeID:       agent.ProjectID,
					InjectionMode: "always",
				},
				Value: "secret-api-key-value",
			},
		},
	})

	err := dispatcher.DispatchAgentRestart(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentRestart failed: %v", err)
	}

	env := mockClient.lastRestartResolvedEnv

	// Environment-type secret should be injected into resolvedEnv
	if got, ok := env["API_KEY"]; !ok {
		t.Error("API_KEY missing from restart resolvedEnv — secrets not resolved")
	} else if got != "secret-api-key-value" {
		t.Errorf("API_KEY = %q, want %q", got, "secret-api-key-value")
	}
}

// TestHTTPAgentDispatcher_DispatchAgentRestart_ConfigEnvTakesPrecedence verifies
// that explicit config env vars take precedence over storage-resolved vars,
// matching the same precedence rules as DispatchAgentStart.
func TestHTTPAgentDispatcher_DispatchAgentRestart_ConfigEnvTakesPrecedence(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("broker-restart-prec"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	agent := &store.Agent{
		ID:              tid("agent-restart-prec"),
		Name:            "restart-prec-agent",
		Slug:            "restart-prec-agent",
		ProjectID:       tid("project-restart-prec"),
		OwnerID:         tid("user-restart-prec"),
		RuntimeBrokerID: tid("broker-restart-prec"),
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{
				"SHARED_VAR": "from-config",
			},
		},
	}

	// Seed same key in storage
	envVar := store.EnvVar{
		ID:            api.NewUUID(),
		Key:           "SHARED_VAR",
		Value:         "from-storage",
		Scope:         store.ScopeProject,
		ScopeID:       agent.ProjectID,
		InjectionMode: store.InjectionModeAlways,
	}
	if _, err := memStore.UpsertEnvVar(ctx, &envVar); err != nil {
		t.Fatalf("seeding env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	err := dispatcher.DispatchAgentRestart(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentRestart failed: %v", err)
	}

	env := mockClient.lastRestartResolvedEnv

	// Config env should win over storage
	if got := env["SHARED_VAR"]; got != "from-config" {
		t.Errorf("SHARED_VAR = %q, want %q (config should take precedence over storage)", got, "from-config")
	}
}
