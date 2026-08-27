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

// mockGitTokenSecretBackend supports both Resolve (for normal secret resolution)
// and Get (for the GITHUB_TOKEN exemption under NoAuth). It simulates a project
// with GITHUB_TOKEN stored as a project-scoped secret alongside LLM auth secrets.
type mockGitTokenSecretBackend struct {
	secrets      []secret.SecretWithValue
	getResponses map[string]*secret.SecretWithValue
	// getScopedResponses maps "name:scope" to a response, allowing
	// scope-aware lookups (e.g. project vs user scope for GITHUB_TOKEN).
	// When set for a key, it takes precedence over getResponses.
	getScopedResponses map[string]*secret.SecretWithValue
	getCalls           []getCall // track which secrets were fetched via Get
}

type getCall struct {
	Name    string
	Scope   string
	ScopeID string
}

func (m *mockGitTokenSecretBackend) Get(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	m.getCalls = append(m.getCalls, getCall{Name: name, Scope: scope, ScopeID: scopeID})
	// Check scope-specific responses first
	if m.getScopedResponses != nil {
		if sv, ok := m.getScopedResponses[name+":"+scope]; ok {
			return sv, nil
		}
	}
	if sv, ok := m.getResponses[name]; ok {
		return sv, nil
	}
	return nil, nil
}

func (m *mockGitTokenSecretBackend) Set(_ context.Context, _ *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	return false, nil, nil
}

func (m *mockGitTokenSecretBackend) Delete(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockGitTokenSecretBackend) List(_ context.Context, _ secret.Filter) ([]secret.SecretMeta, error) {
	return nil, nil
}

func (m *mockGitTokenSecretBackend) GetMeta(_ context.Context, _, _, _ string) (*secret.SecretMeta, error) {
	return nil, nil
}

func (m *mockGitTokenSecretBackend) Resolve(_ context.Context, _, _, _ string, _ *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return m.secrets, nil
}

func (m *mockGitTokenSecretBackend) HubID() string { return "test-hub" }

func TestBuildCreateRequest_NoAuth_GitHubTokenSurvives(t *testing.T) {
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

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	ghTokenValue := "ghp_test_pat_token_12345"
	apiKeyValue := "sk-ant-api-key-secret"

	backend := &mockGitTokenSecretBackend{
		secrets: []secret.SecretWithValue{
			{SecretMeta: secret.SecretMeta{Name: "CLAUDE_AUTH", SecretType: "file", Target: "~/.claude/.credentials.json"}, Value: "secret-data"},
			{SecretMeta: secret.SecretMeta{Name: "API_KEY", SecretType: "environment", Target: "API_KEY"}, Value: apiKeyValue},
		},
		getResponses: map[string]*secret.SecretWithValue{
			"GITHUB_TOKEN": {
				SecretMeta: secret.SecretMeta{
					Name:       "GITHUB_TOKEN",
					SecretType: "environment",
					Target:     "GITHUB_TOKEN",
					Scope:      secret.ScopeProject,
				},
				Value: ghTokenValue,
			},
		},
	}
	dispatcher.SetSecretBackend(backend)

	t.Run("NoAuth=true preserves GITHUB_TOKEN in ResolvedEnv", func(t *testing.T) {
		agent := &store.Agent{
			ID:              tid("agent-noauth-git"),
			Name:            "noauth-git-agent",
			Slug:            "noauth-git-agent",
			OwnerID:         tid("user-1"),
			ProjectID:       tid("project-1"),
			RuntimeBrokerID: tid("host-1"),
			AppliedConfig:   &store.AgentAppliedConfig{NoAuth: true},
		}

		req, err := dispatcher.buildCreateRequest(ctx, agent, "TestNoAuthGitToken")
		if err != nil {
			t.Fatalf("buildCreateRequest failed: %v", err)
		}

		// NoAuth must be set
		if !req.NoAuth {
			t.Error("expected req.NoAuth to be true")
		}

		// ResolvedSecrets must still be nil (NoAuth suppresses LLM secrets)
		if len(req.ResolvedSecrets) != 0 {
			t.Errorf("expected no resolved secrets with NoAuth, got %d", len(req.ResolvedSecrets))
		}

		// GITHUB_TOKEN must survive in ResolvedEnv for git operations
		if got := req.ResolvedEnv["GITHUB_TOKEN"]; got != ghTokenValue {
			t.Errorf("expected GITHUB_TOKEN=%q in ResolvedEnv, got %q", ghTokenValue, got)
		}

		// LLM API keys must NOT be injected into ResolvedEnv (NoAuth suppression)
		if v, ok := req.ResolvedEnv["API_KEY"]; ok && v != "" {
			t.Errorf("expected API_KEY to not be injected into ResolvedEnv with NoAuth, got %q", v)
		}
	})

	t.Run("NoAuth=true without project does not attempt GITHUB_TOKEN resolution", func(t *testing.T) {
		agent := &store.Agent{
			ID:              tid("agent-noauth-noproj"),
			Name:            "noauth-no-project",
			Slug:            "noauth-no-project",
			OwnerID:         tid("user-1"),
			ProjectID:       "", // No project
			RuntimeBrokerID: tid("host-1"),
			AppliedConfig:   &store.AgentAppliedConfig{NoAuth: true},
		}

		backend.getCalls = nil // reset

		req, err := dispatcher.buildCreateRequest(ctx, agent, "TestNoAuthNoProject")
		if err != nil {
			t.Fatalf("buildCreateRequest failed: %v", err)
		}

		if !req.NoAuth {
			t.Error("expected req.NoAuth to be true")
		}

		// Without a project, Get should not have been called for GITHUB_TOKEN
		for _, call := range backend.getCalls {
			if call.Name == "GITHUB_TOKEN" {
				t.Error("expected no GITHUB_TOKEN Get call when agent has no project")
			}
		}
	})

	t.Run("NoAuth=true falls back to user-scope GITHUB_TOKEN when project scope is empty", func(t *testing.T) {
		userGHTokenValue := "ghp_user_profile_token_67890"

		// Backend with no project-scope GITHUB_TOKEN but a user-scope one
		userScopeBackend := &mockGitTokenSecretBackend{
			secrets: []secret.SecretWithValue{
				{SecretMeta: secret.SecretMeta{Name: "API_KEY", SecretType: "environment", Target: "API_KEY"}, Value: apiKeyValue},
			},
			getScopedResponses: map[string]*secret.SecretWithValue{
				"GITHUB_TOKEN:" + secret.ScopeUser: {
					SecretMeta: secret.SecretMeta{
						Name:       "GITHUB_TOKEN",
						SecretType: "environment",
						Target:     "GITHUB_TOKEN",
						Scope:      secret.ScopeUser,
					},
					Value: userGHTokenValue,
				},
			},
		}
		dispatcher.SetSecretBackend(userScopeBackend)

		agent := &store.Agent{
			ID:              tid("agent-noauth-user-gh"),
			Name:            "noauth-user-gh-agent",
			Slug:            "noauth-user-gh-agent",
			OwnerID:         tid("user-1"),
			ProjectID:       tid("project-1"),
			RuntimeBrokerID: tid("host-1"),
			AppliedConfig:   &store.AgentAppliedConfig{NoAuth: true},
		}

		req, err := dispatcher.buildCreateRequest(ctx, agent, "TestNoAuthUserScopeGitToken")
		if err != nil {
			t.Fatalf("buildCreateRequest failed: %v", err)
		}

		if !req.NoAuth {
			t.Error("expected req.NoAuth to be true")
		}

		// ResolvedSecrets must still be nil (NoAuth suppresses LLM secrets)
		if len(req.ResolvedSecrets) != 0 {
			t.Errorf("expected no resolved secrets with NoAuth, got %d", len(req.ResolvedSecrets))
		}

		// GITHUB_TOKEN must be resolved from user scope
		if got := req.ResolvedEnv["GITHUB_TOKEN"]; got != userGHTokenValue {
			t.Errorf("expected GITHUB_TOKEN=%q in ResolvedEnv (user-scope fallback), got %q", userGHTokenValue, got)
		}

		// Verify both scopes were tried: project first, then user
		if len(userScopeBackend.getCalls) < 2 {
			t.Fatalf("expected at least 2 Get calls, got %d", len(userScopeBackend.getCalls))
		}
		if userScopeBackend.getCalls[0].Scope != secret.ScopeProject {
			t.Errorf("expected first Get call to be project scope, got %q", userScopeBackend.getCalls[0].Scope)
		}
		if userScopeBackend.getCalls[1].Scope != secret.ScopeUser {
			t.Errorf("expected second Get call to be user scope, got %q", userScopeBackend.getCalls[1].Scope)
		}

		// LLM API keys must NOT be in ResolvedEnv
		if v, ok := req.ResolvedEnv["API_KEY"]; ok && v != "" {
			t.Errorf("expected API_KEY to not be injected into ResolvedEnv with NoAuth, got %q", v)
		}

		// Restore original backend for remaining tests
		dispatcher.SetSecretBackend(backend)
	})

	t.Run("NoAuth=false resolves secrets normally including GITHUB_TOKEN", func(t *testing.T) {
		agent := &store.Agent{
			ID:              tid("agent-auth-git"),
			Name:            "auth-git-agent",
			Slug:            "auth-git-agent",
			OwnerID:         tid("user-1"),
			ProjectID:       tid("project-1"),
			RuntimeBrokerID: tid("host-1"),
			AppliedConfig:   &store.AgentAppliedConfig{},
		}

		req, err := dispatcher.buildCreateRequest(ctx, agent, "TestAuthGitToken")
		if err != nil {
			t.Fatalf("buildCreateRequest failed: %v", err)
		}

		if req.NoAuth {
			t.Error("expected req.NoAuth to be false")
		}

		// Secrets should be resolved normally
		if len(req.ResolvedSecrets) != 2 {
			t.Errorf("expected 2 resolved secrets, got %d", len(req.ResolvedSecrets))
		}

		// API_KEY env-type secret should be in ResolvedEnv
		if got := req.ResolvedEnv["API_KEY"]; got != apiKeyValue {
			t.Errorf("expected API_KEY=%q in ResolvedEnv, got %q", apiKeyValue, got)
		}
	})
}
