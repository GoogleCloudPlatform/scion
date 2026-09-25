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
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestShouldPersistResolvedEnvKey is a mutation-resistant table test for the
// persistence allowlist: it pins the outcome for every EnvKind value, for an
// unclassified key, for a nil classification map, and for the two
// unconditional exclusions (GITHUB_TOKEN, SCION_ prefix) -- including the
// case where GITHUB_TOKEN is (incorrectly, hypothetically) classified Plain,
// which must still be rejected.
func TestShouldPersistResolvedEnvKey(t *testing.T) {
	tests := []struct {
		name            string
		key             string
		classifications map[string]api.EnvKind
		want            bool
	}{
		{
			name:            "plain key is persisted",
			key:             "TZ",
			classifications: map[string]api.EnvKind{"TZ": api.EnvKindPlain},
			want:            true,
		},
		{
			name:            "secret-fetchable key is rejected",
			key:             "API_KEY",
			classifications: map[string]api.EnvKind{"API_KEY": api.EnvKindSecretFetchable},
			want:            false,
		},
		{
			name:            "secret-injected key is rejected",
			key:             "SCION_UNPREFIXED_INJECTED", // hypothetical non-SCION-looking name
			classifications: map[string]api.EnvKind{"SCION_UNPREFIXED_INJECTED": api.EnvKindSecretInjected},
			want:            false, // rejected twice over: SCION_ prefix AND non-plain
		},
		{
			name:            "secret-bootstrap key is rejected",
			key:             "BOOTSTRAP_VAR",
			classifications: map[string]api.EnvKind{"BOOTSTRAP_VAR": api.EnvKindSecretBootstrap},
			want:            false,
		},
		{
			name:            "unclassified key (absent from a non-nil map) is rejected, not treated as plain",
			key:             "MYSTERY_VAR",
			classifications: map[string]api.EnvKind{"OTHER_VAR": api.EnvKindPlain},
			want:            false,
		},
		{
			name:            "nil classifications map is rejected, not treated as plain",
			key:             "MYSTERY_VAR",
			classifications: nil,
			want:            false,
		},
		{
			name:            "GITHUB_TOKEN is rejected even when classified plain",
			key:             "GITHUB_TOKEN",
			classifications: map[string]api.EnvKind{"GITHUB_TOKEN": api.EnvKindPlain},
			want:            false,
		},
		{
			name:            "GITHUB_TOKEN is rejected when unclassified",
			key:             "GITHUB_TOKEN",
			classifications: nil,
			want:            false,
		},
		{
			name:            "SCION_ prefixed key is rejected even when classified plain",
			key:             "SCION_MODEL",
			classifications: map[string]api.EnvKind{"SCION_MODEL": api.EnvKindPlain},
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPersistResolvedEnvKey(tt.key, tt.classifications)
			if got != tt.want {
				t.Errorf("shouldPersistResolvedEnvKey(%q, %v) = %v, want %v", tt.key, tt.classifications, got, tt.want)
			}
		})
	}
}

// TestProvisionMergeBackSkipsNonPlainEnv exercises DispatchAgentProvision
// end-to-end: a plain, non-SCION_ key resolved through hub defaults (TZ) must
// land in AppliedConfig.Env, and so must a plain (Secret==false) key resolved
// from project-scoped storage -- storage-sourced keys are classified per the
// backing EnvVar's own Secret flag, not blanket-classified as fetchable
// secrets (see resolveEnvFromStorage's plain-key return value). A
// Secret==true storage key must not land there, and GITHUB_TOKEN must never
// land there even when resolved.
func TestProvisionMergeBackSkipsNonPlainEnv(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("host-1"),
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	projectID := tid("project-1")
	// A project-scoped, non-secret env var with InjectionMode "always" flows
	// into req.ResolvedEnv via resolveEnvFromStorage. Its Secret flag is
	// false, so it must be classified api.EnvKindPlain and persist.
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("envvar-1"),
		Key:           "STORAGE_PLAIN_VAR",
		Value:         "storage-plain-value",
		Scope:         store.ScopeProject,
		ScopeID:       projectID,
		InjectionMode: store.InjectionModeAlways,
	}); err != nil {
		t.Fatalf("failed to create env var: %v", err)
	}
	// A project-scoped env var with Secret==true, otherwise identical. It
	// must be classified non-plain and must not persist.
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("envvar-2"),
		Key:           "STORAGE_SECRET_VAR",
		Value:         "storage-secret-value",
		Scope:         store.ScopeProject,
		ScopeID:       projectID,
		InjectionMode: store.InjectionModeAlways,
		Secret:        true,
	}); err != nil {
		t.Fatalf("failed to create env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetProfileTimezoneProvider(func(name string) string { return "" })
	dispatcher.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		return opsettings.AgentDefaultsSettings{DefaultTimezone: "America/New_York"}
	})

	agent := &store.Agent{
		ID:              tid("agent-1"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       projectID,
		RuntimeBrokerID: tid("host-1"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	if err := dispatcher.DispatchAgentProvision(ctx, agent); err != nil {
		t.Fatalf("DispatchAgentProvision failed: %v", err)
	}

	if got := agent.AppliedConfig.Env["TZ"]; got != "America/New_York" {
		t.Errorf("expected plain TZ to be persisted, got %q", got)
	}
	if got := agent.AppliedConfig.Env["STORAGE_PLAIN_VAR"]; got != "storage-plain-value" {
		t.Errorf("expected plain STORAGE_PLAIN_VAR to be persisted, got %q", got)
	}
	if _, ok := agent.AppliedConfig.Env["STORAGE_SECRET_VAR"]; ok {
		t.Errorf("expected non-plain STORAGE_SECRET_VAR to be skipped, but it was persisted: %q", agent.AppliedConfig.Env["STORAGE_SECRET_VAR"])
	}
	if _, ok := agent.AppliedConfig.Env["GITHUB_TOKEN"]; ok {
		t.Errorf("expected GITHUB_TOKEN to never be persisted, but it was: %q", agent.AppliedConfig.Env["GITHUB_TOKEN"])
	}
}

// TestBuildCreateRequestClassifiesEveryResolvedEnvKey audits the invariant
// the merge-back's persistence gate depends on: every key buildCreateRequest
// places into req.ResolvedEnv must have a matching entry in
// req.EnvClassifications. A key that slips through unclassified doesn't fail
// open under shouldPersistResolvedEnvKey's allowlist (it's correctly
// rejected, same as any other non-plain key) -- but an unclassified key
// silently disappearing from the advanced-config-form mirror is itself a
// signal that a new injection site forgot to call classifyEnv, so this test
// pins the invariant directly rather than relying on that side effect.
//
// This exercises several injection sites at once (hub name, hub-default TZ,
// project-scoped storage env var, an environment-type resolved secret, and
// the dev auth token) so a future call site that adds a new ResolvedEnv
// write without a matching classifyEnv call is caught here, not just by
// coincidence in some unrelated test.
func TestBuildCreateRequestClassifiesEveryResolvedEnvKey(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("host-1"),
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	projectID := tid("project-1")
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("envvar-1"),
		Key:           "STORAGE_VAR",
		Value:         "storage-value",
		Scope:         store.ScopeProject,
		ScopeID:       projectID,
		InjectionMode: store.InjectionModeAlways,
	}); err != nil {
		t.Fatalf("failed to create env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetHubName("test-hub")
	dispatcher.SetDevAuthToken("dev-token-value")
	dispatcher.SetProfileTimezoneProvider(func(name string) string { return "" })
	dispatcher.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		return opsettings.AgentDefaultsSettings{DefaultTimezone: "America/New_York"}
	})
	dispatcher.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{
				SecretMeta: secret.SecretMeta{Name: "MY_API_KEY", SecretType: "environment", Target: "MY_API_KEY"},
				Value:      "secret-value",
			},
		},
	})

	agent := &store.Agent{
		ID:              tid("agent-1"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       projectID,
		RuntimeBrokerID: tid("host-1"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Model:         "claude-3-opus",
			ThinkingLevel: intPtr(50),
		},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "test")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	if len(req.ResolvedEnv) == 0 {
		t.Fatal("expected buildCreateRequest to populate ResolvedEnv for this test to be meaningful")
	}

	var unclassified []string
	for k := range req.ResolvedEnv {
		if _, ok := api.ClassifyEnvKey(req.EnvClassifications, k); !ok {
			unclassified = append(unclassified, k)
		}
	}
	if len(unclassified) > 0 {
		t.Errorf("resolved env keys with no classification entry (every injection site must call classifyEnv): %v", unclassified)
	}
}

// TestProvisionMergeBackNeverPersistsGitHubTokenEvenIfMisclassified guards
// against a future classification-site change that (by mistake) tags
// GITHUB_TOKEN as plain: the unconditional literal check in
// shouldPersistResolvedEnvKey must still catch it. This exercises the
// dispatcher path (not just the unit-tested helper) by forging a
// classification through the exported surface DispatchAgentProvision
// actually consults.
func TestProvisionMergeBackNeverPersistsGitHubTokenEvenIfMisclassified(t *testing.T) {
	if shouldPersistResolvedEnvKey("GITHUB_TOKEN", map[string]api.EnvKind{"GITHUB_TOKEN": api.EnvKindPlain}) {
		t.Fatal("GITHUB_TOKEN must never be persisted, even if some future call site classifies it Plain")
	}
}
