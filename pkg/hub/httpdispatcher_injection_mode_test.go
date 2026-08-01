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

// TestResolveEnvFromStorage_InjectionMode verifies that resolveEnvFromStorage
// skips env vars with InjectionMode == "as_needed" and includes vars with
// InjectionMode == "always".
//
// Note: the ent schema defaults injection_mode to "as_needed" and the store
// adapter normalises empty strings to "as_needed" on write, so legacy env vars
// with an unset InjectionMode are indistinguishable from explicit "as_needed"
// at read time. The empty-string backward-compatibility case is tested at the
// secret layer (TestResolveSecrets_InjectionMode) where the mock backend
// bypasses the store's normalisation.
func TestResolveEnvFromStorage_InjectionMode(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	agent := envScopeTestAgent()

	// Seed env vars with explicit injection modes.
	vars := []store.EnvVar{
		{
			ID:            api.NewUUID(),
			Key:           "ALWAYS_VAR",
			Value:         "always-value",
			Scope:         store.ScopeProject,
			ScopeID:       agent.ProjectID,
			InjectionMode: store.InjectionModeAlways,
		},
		{
			ID:            api.NewUUID(),
			Key:           "AS_NEEDED_VAR",
			Value:         "as-needed-value",
			Scope:         store.ScopeProject,
			ScopeID:       agent.ProjectID,
			InjectionMode: store.InjectionModeAsNeeded,
		},
		{
			ID:      api.NewUUID(),
			Key:     "DEFAULT_VAR",
			Value:   "default-value",
			Scope:   store.ScopeProject,
			ScopeID: agent.ProjectID,
			// InjectionMode left empty — store normalises to "as_needed"
		},
	}
	for _, v := range vars {
		if _, err := memStore.UpsertEnvVar(ctx, &v); err != nil {
			t.Fatalf("seeding env var %s: %v", v.Key, err)
		}
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(envScopeTestHubID)

	resolved, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		t.Fatalf("resolveEnvFromStorage: %v", err)
	}

	// ALWAYS_VAR must be present.
	if got, ok := resolved["ALWAYS_VAR"]; !ok {
		t.Error("ALWAYS_VAR missing from resolved env, want present")
	} else if got != "always-value" {
		t.Errorf("ALWAYS_VAR = %q, want %q", got, "always-value")
	}

	// AS_NEEDED_VAR must NOT be present.
	if _, ok := resolved["AS_NEEDED_VAR"]; ok {
		t.Error("AS_NEEDED_VAR present in resolved env, want absent (injection_mode = as_needed)")
	}

	// DEFAULT_VAR (empty InjectionMode, normalised to as_needed by store) must NOT be present.
	if _, ok := resolved["DEFAULT_VAR"]; ok {
		t.Error("DEFAULT_VAR present in resolved env, want absent (store defaults empty to as_needed)")
	}
}

// TestBuildEnvSources_InjectionMode verifies that buildEnvSources skips
// as_needed env vars so they are not reported as sources.
func TestBuildEnvSources_InjectionMode(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	agent := envScopeTestAgent()

	vars := []store.EnvVar{
		{
			ID:            api.NewUUID(),
			Key:           "ALWAYS_VAR",
			Value:         "always-value",
			Scope:         store.ScopeProject,
			ScopeID:       agent.ProjectID,
			InjectionMode: store.InjectionModeAlways,
		},
		{
			ID:            api.NewUUID(),
			Key:           "AS_NEEDED_VAR",
			Value:         "as-needed-value",
			Scope:         store.ScopeProject,
			ScopeID:       agent.ProjectID,
			InjectionMode: store.InjectionModeAsNeeded,
		},
	}
	for _, v := range vars {
		if _, err := memStore.UpsertEnvVar(ctx, &v); err != nil {
			t.Fatalf("seeding env var %s: %v", v.Key, err)
		}
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(envScopeTestHubID)

	// resolvedEnv contains only the var that passed the filter.
	resolvedEnv := map[string]string{"ALWAYS_VAR": "always-value"}
	sources := d.buildEnvSources(ctx, agent, resolvedEnv)

	if got, ok := sources["ALWAYS_VAR"]; !ok || got != "project" {
		t.Errorf("ALWAYS_VAR source = %q (present=%v), want %q", got, ok, "project")
	}
	if _, ok := sources["AS_NEEDED_VAR"]; ok {
		t.Error("AS_NEEDED_VAR present in sources, want absent (injection_mode = as_needed)")
	}
}

// TestResolveSecrets_InjectionMode verifies that resolveSecrets skips secrets
// with InjectionMode == "as_needed" and includes secrets with InjectionMode ==
// "always" or "" (empty/unset).
func TestResolveSecrets_InjectionMode(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	agent := &store.Agent{
		ID:              "agent-injmode-1",
		Name:            "injmode-agent",
		Slug:            "injmode-agent",
		ProjectID:       "project-injmode-1",
		OwnerID:         "user-injmode-1",
		RuntimeBrokerID: "broker-injmode-1",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{
					Name:          "ALWAYS_SECRET",
					SecretType:    "environment",
					Target:        "ALWAYS_SECRET",
					Scope:         "project",
					ScopeID:       agent.ProjectID,
					InjectionMode: "always",
				},
				Value: "always-secret-value",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:          "AS_NEEDED_SECRET",
					SecretType:    "environment",
					Target:        "AS_NEEDED_SECRET",
					Scope:         "project",
					ScopeID:       agent.ProjectID,
					InjectionMode: "as_needed",
				},
				Value: "as-needed-secret-value",
			},
			{
				SecretMeta: secret.SecretMeta{
					Name:       "UNSET_SECRET",
					SecretType: "environment",
					Target:     "UNSET_SECRET",
					Scope:      "project",
					ScopeID:    agent.ProjectID,
					// InjectionMode left empty — legacy, treated as always
				},
				Value: "unset-secret-value",
			},
		},
	})

	resolved, err := d.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets: %v", err)
	}

	// Build a lookup for easy assertions.
	byName := make(map[string]ResolvedSecret, len(resolved))
	for _, r := range resolved {
		byName[r.Name] = r
	}

	// ALWAYS_SECRET must be present.
	if r, ok := byName["ALWAYS_SECRET"]; !ok {
		t.Error("ALWAYS_SECRET missing from resolved secrets, want present")
	} else if r.Value != "always-secret-value" {
		t.Errorf("ALWAYS_SECRET value = %q, want %q", r.Value, "always-secret-value")
	}

	// AS_NEEDED_SECRET must NOT be present.
	if _, ok := byName["AS_NEEDED_SECRET"]; ok {
		t.Error("AS_NEEDED_SECRET present in resolved secrets, want absent (injection_mode = as_needed)")
	}

	// UNSET_SECRET (empty InjectionMode) must be present — backward compatibility.
	if r, ok := byName["UNSET_SECRET"]; !ok {
		t.Error("UNSET_SECRET missing from resolved secrets, want present (empty injection_mode = always)")
	} else if r.Value != "unset-secret-value" {
		t.Errorf("UNSET_SECRET value = %q, want %q", r.Value, "unset-secret-value")
	}

	// Total count: should be 2 (always + unset), not 3.
	if len(resolved) != 2 {
		t.Errorf("resolved %d secrets, want 2", len(resolved))
	}
}
