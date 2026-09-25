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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
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
// always removed; a key matching a secret-flagged EnvVar or a secret-store
// entry is removed even though a same-named plain EnvVar also exists; a key
// whose value matches a currently resolvable plain EnvVar is preserved; and
// a key matching no currently resolvable source is removed along with the
// rest (allowlist semantics -- see the decision rules documented on
// AppliedConfigEnvCleanupExecutor).
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
				"GITHUB_TOKEN":        "value-must-not-appear-in-log-1",
				"PROJECT_SECRET_VAR":  "value-must-not-appear-in-log-2",
				"PROJECT_PLAIN_VAR":   "plain-value",
				"USER_SECRET":         "value-must-not-appear-in-log-3",
				"TARGETED_SECRET_VAR": "value-must-not-appear-in-log-4",
				"UNRECOGNIZED_VAR":    "value-must-not-appear-in-log-5",
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

	// UNRECOGNIZED_VAR matches no currently resolvable plain source (no live
	// EnvVar, secret, template default or InlineConfig entry for that key),
	// so allowlist semantics strip it along with the confirmed secrets --
	// this cleanup cannot tell "orphaned residual value" apart from "value
	// typed directly into config", so it treats both as unsafe to keep.
	for _, stripped := range []string{"GITHUB_TOKEN", "PROJECT_SECRET_VAR", "USER_SECRET", "TARGETED_SECRET_VAR", "UNRECOGNIZED_VAR"} {
		if _, ok := env[stripped]; ok {
			t.Errorf("expected %q to be stripped, but it remains", stripped)
		}
	}
	for _, preserved := range []string{"PROJECT_PLAIN_VAR"} {
		if _, ok := env[preserved]; !ok {
			t.Errorf("expected %q to be preserved, but it was removed", preserved)
		}
	}

	logOutput := logBuf.String()
	if bytes.Contains(logBuf.Bytes(), []byte("value-must-not-appear-in-log")) {
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

	// KEEP_ME must match a currently resolvable plain source to survive the
	// allowlist; without a live declaration it would be stripped like any
	// other key with no matching source.
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      tid("envvar-idem-plain"),
		Key:     "KEEP_ME",
		Value:   "plain-value",
		Scope:   store.ScopeProject,
		ScopeID: project.ID,
		Secret:  false,
	}); err != nil {
		t.Fatalf("failed to create plain env var: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent-idem"),
		Slug:      "agent-idem",
		Name:      "Idempotent Agent",
		ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{
				"GITHUB_TOKEN": "gh-token-value",
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
			Env: map[string]string{"GITHUB_TOKEN": "gh-token-value"},
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

// TestAppliedConfigEnvCleanupStripsHubScopeSecret verifies that a key
// matching a hub-scoped secret is stripped. secretScopeFilters previously
// queried only user/project/runtime-broker scopes, so a hub-scoped secret
// name was invisible to this cleanup and any matching key survived.
func TestAppliedConfigEnvCleanupStripsHubScopeSecret(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{ID: tid("project-hubscope"), Name: "Hub Scope Project", Slug: "hubscope-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	secretBackend := &cleanupTestSecretBackend{
		byScope: map[string][]secret.SecretMeta{
			// cleanupTestSecretBackend.HubID() returns "test-hub", the scope
			// ID secretScopeFilters must now query under secret.ScopeHub.
			"hub/test-hub": {
				{Name: "HUB_SCOPED_SECRET", SecretType: "variable"},
			},
		},
	}

	agent := &store.Agent{
		ID:        tid("agent-hubscope"),
		Slug:      "agent-hubscope",
		Name:      "Hub Scope Agent",
		ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{"HUB_SCOPED_SECRET": "value-must-not-appear-in-log"},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: memStore, SecretBackend: secretBackend}
	var buf bytes.Buffer
	if err := exec.Run(ctx, &buf, nil); err != nil {
		t.Fatalf("cleanup Run failed: %v", err)
	}

	updated, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent: %v", err)
	}
	if _, ok := updated.AppliedConfig.Env["HUB_SCOPED_SECRET"]; ok {
		t.Error("expected HUB_SCOPED_SECRET (a hub-scope secret match) to be stripped, but it remains")
	}
}

// TestAppliedConfigEnvCleanupStripsPlainShadowedSecret verifies the plain-
// shadow case: a live secret and a live plain EnvVar share the same key
// name, and the persisted value is the old secret value (not the plain
// var's current value). The key must still be stripped -- a same-named
// plain declaration must not short-circuit the secret-name check, and a
// value mismatch alone is enough to disqualify it from the allowlist.
func TestAppliedConfigEnvCleanupStripsPlainShadowedSecret(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{ID: tid("project-shadow"), Name: "Shadow Project", Slug: "shadow-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	ownerID := tid("owner-shadow")

	// A plain project var now exists under the same name as a live user
	// secret, with a different current value than what was persisted.
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      tid("envvar-shadow-plain"),
		Key:     "SHADOWED_KEY",
		Value:   "current-plain-value",
		Scope:   store.ScopeProject,
		ScopeID: project.ID,
		Secret:  false,
	}); err != nil {
		t.Fatalf("failed to create plain env var: %v", err)
	}

	secretBackend := &cleanupTestSecretBackend{
		byScope: map[string][]secret.SecretMeta{
			"user/" + ownerID: {
				{Name: "SHADOWED_KEY", SecretType: "variable"},
			},
		},
	}

	agent := &store.Agent{
		ID:        tid("agent-shadow"),
		Slug:      "agent-shadow",
		Name:      "Shadow Agent",
		ProjectID: project.ID,
		OwnerID:   ownerID,
		AppliedConfig: &store.AgentAppliedConfig{
			// The persisted value is the old secret value, not the plain
			// var's current value -- proof this came from the secret, not
			// the later-added plain declaration.
			Env: map[string]string{"SHADOWED_KEY": "old-secret-value"},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: memStore, SecretBackend: secretBackend}
	var buf bytes.Buffer
	if err := exec.Run(ctx, &buf, nil); err != nil {
		t.Fatalf("cleanup Run failed: %v", err)
	}

	updated, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent: %v", err)
	}
	if _, ok := updated.AppliedConfig.Env["SHADOWED_KEY"]; ok {
		t.Error("expected SHADOWED_KEY to be stripped despite a same-named plain var existing, but it remains")
	}
}

// TestAppliedConfigEnvCleanupStripsKeyFromDeletedSecret verifies that a key
// whose originating secret has since been deleted is stripped rather than
// left in place. Allowlist semantics mean "no currently resolvable plain
// source" is sufficient on its own to strip, regardless of whether the key
// still matches a live secret name.
func TestAppliedConfigEnvCleanupStripsKeyFromDeletedSecret(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{ID: tid("project-deleted"), Name: "Deleted Secret Project", Slug: "deleted-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// No EnvVar, no secret, no template and no InlineConfig declare this key
	// -- it is exactly the state left behind once the originating secret is
	// deleted.
	agent := &store.Agent{
		ID:        tid("agent-deleted"),
		Slug:      "agent-deleted",
		Name:      "Deleted Secret Agent",
		ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{"NO_LONGER_DECLARED": "value-must-not-appear-in-log"},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: memStore, SecretBackend: &cleanupTestSecretBackend{}}
	var buf bytes.Buffer
	if err := exec.Run(ctx, &buf, nil); err != nil {
		t.Fatalf("cleanup Run failed: %v", err)
	}

	updated, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent: %v", err)
	}
	if _, ok := updated.AppliedConfig.Env["NO_LONGER_DECLARED"]; ok {
		t.Error("expected NO_LONGER_DECLARED (no live source) to be stripped, but it remains")
	}
}

// TestStartStillResolvesEnvAfterAppliedConfigStrip confirms the cleanup's
// core safety property directly, rather than by inspection alone: removing a
// key from AppliedConfig.Env does not change what DispatchAgentStart sends
// to the broker, as long as the key still resolves from a live source.
// DispatchAgentStart seeds its resolved-env map from AppliedConfig.Env only
// as a base layer and then re-resolves storage and secrets fresh on every
// call (see resolveEnvFromStorage/resolveSecrets in httpdispatcher.go), so a
// row with no AppliedConfig.Env entry at all for a still-live plain storage
// var must produce the same broker-bound env as a row that still has it.
func TestStartStillResolvesEnvAfterAppliedConfigStrip(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("host-strip"),
		Name:     "test-host-strip",
		Slug:     "test-host-strip",
		Endpoint: "http://localhost:9801",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}
	projectID := tid("project-strip")
	project := &store.Project{ID: projectID, Name: "Strip Project", Slug: "strip-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	provider := &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   tid("host-strip"),
		BrokerName: "test-host-strip",
		LocalPath:  "/home/user/projects/stripproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("envvar-strip-plain"),
		Key:           "STORAGE_PLAIN_VAR",
		Value:         "storage-plain-value",
		Scope:         store.ScopeProject,
		ScopeID:       projectID,
		InjectionMode: store.InjectionModeAlways,
	}); err != nil {
		t.Fatalf("failed to create plain env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	// AppliedConfig.Env is empty, as it would be right after the cleanup
	// migration stripped a stale entry for this same key -- STORAGE_PLAIN_VAR
	// is absent entirely, not merely present with a different value.
	agent := &store.Agent{
		ID:              tid("agent-strip"),
		Name:            "test-agent-strip",
		Slug:            "test-agent-strip",
		ProjectID:       projectID,
		RuntimeBrokerID: tid("host-strip"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Env:           map[string]string{},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	if err := dispatcher.DispatchAgentStart(ctx, agent, "task", false); err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if got := mockClient.lastResolvedEnv["STORAGE_PLAIN_VAR"]; got != "storage-plain-value" {
		t.Errorf("expected start to resolve STORAGE_PLAIN_VAR fresh from storage despite it being absent "+
			"from AppliedConfig.Env, got %q", got)
	}
}

// TestAppliedConfigEnvCleanupStripsInlineConfigEnv verifies that the cleanup
// also sweeps AppliedConfig.InlineConfig.Env: GITHUB_TOKEN and a key matching
// a live secret name are stripped there too, while a value with no matching
// live source (which would be stripped from AppliedConfig.Env under
// allowlist semantics) is left alone in InlineConfig.Env, since InlineConfig
// is the explicit, user-typed source of truth and has nothing else to match
// against.
func TestAppliedConfigEnvCleanupStripsInlineConfigEnv(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{ID: tid("project-inline"), Name: "Inline Project", Slug: "inline-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	ownerID := tid("owner-inline")

	secretBackend := &cleanupTestSecretBackend{
		byScope: map[string][]secret.SecretMeta{
			"user/" + ownerID: {
				{Name: "USER_SECRET_INLINE", SecretType: "variable"},
			},
		},
	}

	agent := &store.Agent{
		ID:        tid("agent-inline"),
		Slug:      "agent-inline",
		Name:      "Inline Agent",
		ProjectID: project.ID,
		OwnerID:   ownerID,
		AppliedConfig: &store.AgentAppliedConfig{
			// AppliedConfig.Env is empty here to isolate the InlineConfig.Env
			// behavior -- the aliasing bug this migration cleans up after can
			// leave the two fields out of sync once loaded back from the DB.
			Env: map[string]string{},
			InlineConfig: &api.ScionConfig{
				Env: map[string]string{
					"GITHUB_TOKEN":        "value-must-not-appear-in-log-1",
					"USER_SECRET_INLINE":  "value-must-not-appear-in-log-2",
					"EXPLICIT_INLINE_VAR": "user-typed-value",
				},
			},
		},
	}
	if err := memStore.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: memStore, SecretBackend: secretBackend}
	var buf bytes.Buffer
	if err := exec.Run(ctx, &buf, nil); err != nil {
		t.Fatalf("cleanup Run failed: %v", err)
	}

	updated, err := memStore.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to reload agent: %v", err)
	}
	require.NotNil(t, updated.AppliedConfig.InlineConfig)
	inlineEnv := updated.AppliedConfig.InlineConfig.Env

	for _, stripped := range []string{"GITHUB_TOKEN", "USER_SECRET_INLINE"} {
		if _, ok := inlineEnv[stripped]; ok {
			t.Errorf("expected InlineConfig.Env[%q] to be stripped, but it remains", stripped)
		}
	}
	if got := inlineEnv["EXPLICIT_INLINE_VAR"]; got != "user-typed-value" {
		t.Errorf("expected InlineConfig.Env[EXPLICIT_INLINE_VAR] (no matching live secret) to be preserved, got %q", got)
	}
}
