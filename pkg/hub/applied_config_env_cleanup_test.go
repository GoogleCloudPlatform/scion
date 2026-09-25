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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// cleanupTestSecretBackend is a minimal secret.SecretBackend fake whose List
// method returns caller-supplied metadata per scope, for testing
// AppliedConfigEnvCleanupExecutor's reachableSecretNames matching without
// pulling in a full secret store.
type cleanupTestSecretBackend struct {
	byScope map[string][]secret.SecretMeta // key: scope+"/"+scopeID
}

func (b *cleanupTestSecretBackend) key(scope, scopeID string) string { return scope + "/" + scopeID }

func (b *cleanupTestSecretBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	return false, nil, nil
}
func (b *cleanupTestSecretBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	return nil
}
func (b *cleanupTestSecretBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	return b.byScope[b.key(filter.Scope, filter.ScopeID)], nil
}
func (b *cleanupTestSecretBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) UpdateMeta(ctx context.Context, input *secret.UpdateMetaInput) (*secret.SecretMeta, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) Resolve(ctx context.Context, userID, projectID, brokerID string, opts *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) HubID() string { return "test-hub" }

// TestAppliedConfigEnvCleanupStripsGitHubTokenAndKnownSecrets is a
// mutation-resistant check of the cleanup's per-key decision: GITHUB_TOKEN is
// always removed, a key matching a secret-flagged EnvVar or a secret-store
// entry is removed, a key matching a non-secret-flagged EnvVar is preserved,
// and a key matching nothing live is left untouched (see the documented
// limitation on AppliedConfigEnvCleanupExecutor).
func TestAppliedConfigEnvCleanupStripsGitHubTokenAndKnownSecrets(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{ID: tid("project-cleanup"), Name: "Cleanup Project", Slug: "cleanup-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	ownerID := tid("owner-cleanup")

	// A secret-flagged EnvVar at project scope: its matching AppliedConfig.Env
	// key must be stripped.
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      tid("envvar-secret"),
		Key:     "PROJECT_SECRET_VAR",
		Value:   "does-not-matter",
		Scope:   store.ScopeProject,
		ScopeID: project.ID,
		Secret:  true,
	}); err != nil {
		t.Fatalf("failed to create secret-flagged env var: %v", err)
	}

	// A non-secret EnvVar at project scope: its matching AppliedConfig.Env
	// key is a legitimately plain, user-declared value and must be preserved.
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      tid("envvar-plain"),
		Key:     "PROJECT_PLAIN_VAR",
		Value:   "plain-value",
		Scope:   store.ScopeProject,
		ScopeID: project.ID,
		Secret:  false,
	}); err != nil {
		t.Fatalf("failed to create plain env var: %v", err)
	}

	secretBackend := &cleanupTestSecretBackend{
		byScope: map[string][]secret.SecretMeta{
			"user/" + ownerID: {
				{Name: "USER_SECRET", SecretType: "variable"},
				// Environment-type secret with a differing Target: the
				// persisted AppliedConfig.Env key is the Target, not Name.
				{Name: "GH_APP_TOKEN_SOURCE", SecretType: "environment", Target: "TARGETED_SECRET_VAR"},
			},
		},
	}

	agent := &store.Agent{
		ID:        tid("agent-cleanup"),
		Slug:      "agent-cleanup",
		Name:      "Cleanup Agent",
		ProjectID: project.ID,
		OwnerID:   ownerID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{
				"GITHUB_TOKEN":        "ghp_leaked",
				"PROJECT_SECRET_VAR":  "leaked-secret-value",
				"PROJECT_PLAIN_VAR":   "plain-value",
				"USER_SECRET":         "leaked-user-secret",
				"TARGETED_SECRET_VAR": "leaked-targeted-secret",
				"UNRECOGNIZED_VAR":    "user-typed-or-orphaned-value",
			},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: memStore, SecretBackend: secretBackend}
	var logBuf bytes.Buffer
	if err := exec.Run(ctx, &logBuf, nil); err != nil {
		t.Fatalf("cleanup Run failed: %v", err)
	}

	updated, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent: %v", err)
	}
	env := updated.AppliedConfig.Env

	for _, stripped := range []string{"GITHUB_TOKEN", "PROJECT_SECRET_VAR", "USER_SECRET", "TARGETED_SECRET_VAR"} {
		if _, ok := env[stripped]; ok {
			t.Errorf("expected %q to be stripped, but it remains", stripped)
		}
	}
	for _, preserved := range []string{"PROJECT_PLAIN_VAR", "UNRECOGNIZED_VAR"} {
		if _, ok := env[preserved]; !ok {
			t.Errorf("expected %q to be preserved, but it was removed", preserved)
		}
	}

	logOutput := logBuf.String()
	if bytes.Contains(logBuf.Bytes(), []byte("leaked")) {
		t.Errorf("cleanup log must never contain env values, got: %s", logOutput)
	}
}

// TestAppliedConfigEnvCleanupIsIdempotent verifies that running the cleanup a
// second time against an already-cleaned row is a no-op: no further store
// writes are attempted and the row is unchanged.
func TestAppliedConfigEnvCleanupIsIdempotent(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{ID: tid("project-idem"), Name: "Idempotent Project", Slug: "idem-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent-idem"),
		Slug:      "agent-idem",
		Name:      "Idempotent Agent",
		ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{
				"GITHUB_TOKEN": "ghp_leaked",
				"KEEP_ME":      "plain-value",
			},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: memStore}

	var buf1 bytes.Buffer
	if err := exec.Run(ctx, &buf1, nil); err != nil {
		t.Fatalf("first cleanup run failed: %v", err)
	}

	afterFirst, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent after first run: %v", err)
	}
	if _, ok := afterFirst.AppliedConfig.Env["GITHUB_TOKEN"]; ok {
		t.Fatal("expected GITHUB_TOKEN to be stripped after first run")
	}
	stateVersionAfterFirst := afterFirst.StateVersion

	var buf2 bytes.Buffer
	if err := exec.Run(ctx, &buf2, nil); err != nil {
		t.Fatalf("second cleanup run failed: %v", err)
	}

	afterSecond, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent after second run: %v", err)
	}
	if afterSecond.StateVersion != stateVersionAfterFirst {
		t.Errorf("expected second run to be a no-op (unchanged StateVersion), got %d -> %d",
			stateVersionAfterFirst, afterSecond.StateVersion)
	}
	if got := afterSecond.AppliedConfig.Env["KEEP_ME"]; got != "plain-value" {
		t.Errorf("expected KEEP_ME to survive both runs unchanged, got %q", got)
	}
}

// TestAppliedConfigEnvCleanupDryRunMakesNoChanges verifies the dryRun=true
// param reports what would change without writing anything.
func TestAppliedConfigEnvCleanupDryRunMakesNoChanges(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{ID: tid("project-dryrun"), Name: "DryRun Project", Slug: "dryrun-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent-dryrun"),
		Slug:      "agent-dryrun",
		Name:      "DryRun Agent",
		ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{"GITHUB_TOKEN": "ghp_leaked"},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: memStore}
	var buf bytes.Buffer
	if err := exec.Run(ctx, &buf, map[string]string{"dryRun": "true"}); err != nil {
		t.Fatalf("dry run failed: %v", err)
	}

	reloaded, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent: %v", err)
	}
	if _, ok := reloaded.AppliedConfig.Env["GITHUB_TOKEN"]; !ok {
		t.Error("dry run must not modify the stored row, but GITHUB_TOKEN was removed")
	}
}
