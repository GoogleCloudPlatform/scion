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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupSnapshotFixture is one fully populated config snapshot: every env
// map the cleanup inspects carries GITHUB_TOKEN, a key named after a live
// user-scope secret, and a plain key the rules must keep. Env additionally
// carries a key with no resolvable plain source (UNSOURCED_VAR), which the
// AppliedConfig.Env rule strips and the narrow rule keeps.
func cleanupSnapshotFixture() *store.AgentAppliedConfig {
	return &store.AgentAppliedConfig{
		Image: "example/image:1",
		Env: map[string]string{
			"GITHUB_TOKEN":  "value-must-not-appear-in-log-1",
			"USER_SECRET":   "value-must-not-appear-in-log-2",
			"HUB_SECRET":    "value-must-not-appear-in-log-3",
			"INLINE_PLAIN":  "inline-plain-value",
			"UNSOURCED_VAR": "value-must-not-appear-in-log-4",
		},
		InlineConfig: &api.ScionConfig{
			Env: map[string]string{
				"GITHUB_TOKEN": "value-must-not-appear-in-log-5",
				"USER_SECRET":  "value-must-not-appear-in-log-6",
				"INLINE_PLAIN": "inline-plain-value",
			},
		},
		CreateInputs: &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{
				Env: map[string]string{
					"GITHUB_TOKEN": "value-must-not-appear-in-log-7",
					"USER_SECRET":  "value-must-not-appear-in-log-8",
					"INLINE_PLAIN": "inline-plain-value",
				},
			},
		},
	}
}

type reincarnationCleanupFixture struct {
	store   store.Store
	backend *cleanupTestSecretBackend
	agent   *store.Agent
}

// newReincarnationCleanupFixture creates a project and an agent (with no env
// of its own) whose owner scope holds USER_SECRET; the hub scope holds
// HUB_SECRET.
func newReincarnationCleanupFixture(t *testing.T, name string) *reincarnationCleanupFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	project := &store.Project{ID: tid(name + "-project"), Name: name + " project", Slug: name + "-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	ownerID := tid(name + "-owner")
	agent := &store.Agent{
		ID: tid(name + "-agent"), Slug: name + "-agent", Name: name + " agent",
		ProjectID: project.ID, OwnerID: ownerID,
		AppliedConfig: &store.AgentAppliedConfig{Image: "example/image:2"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	backend := &cleanupTestSecretBackend{byScope: map[string][]secret.SecretMeta{
		"user/" + ownerID: {{Name: "USER_SECRET", SecretType: "variable"}},
		"hub/test-hub":    {{Name: "HUB_SECRET", SecretType: "variable"}},
	}}
	return &reincarnationCleanupFixture{store: s, backend: backend, agent: agent}
}

func (f *reincarnationCleanupFixture) createRecord(t *testing.T, id, agentID, state string) {
	t.Helper()
	rec := &store.AgentReincarnation{
		ID: id, AgentID: agentID, FromGeneration: 1, ToGeneration: 2,
		RequestedAt: time.Now(), State: state,
		PreviousAppliedConfig: cleanupSnapshotFixture(),
		NewAppliedConfig:      cleanupSnapshotFixture(),
	}
	require.NoError(t, f.store.CreateAgentReincarnation(context.Background(), rec))
}

func (f *reincarnationCleanupFixture) run(t *testing.T, params map[string]string) string {
	t.Helper()
	exec := &AppliedConfigEnvCleanupExecutor{Store: f.store, SecretBackend: f.backend}
	var logBuf bytes.Buffer
	require.NoError(t, exec.Run(context.Background(), &logBuf, params))
	assert.NotContains(t, logBuf.String(), "value-must-not-appear-in-log", "the cleanup log must carry key names only")
	return logBuf.String()
}

func (f *reincarnationCleanupFixture) get(t *testing.T, id string) *store.AgentReincarnation {
	t.Helper()
	rec, err := f.store.GetAgentReincarnation(context.Background(), id)
	require.NoError(t, err)
	return rec
}

func assertEnvKeys(t *testing.T, env map[string]string, want []string, field string) {
	t.Helper()
	got := make([]string, 0, len(env))
	for k := range env {
		got = append(got, k)
	}
	assert.ElementsMatch(t, want, got, field)
}

// TestAppliedConfigEnvCleanupStripsReincarnationSnapshots covers a terminal
// record of a live agent: each snapshot's Env gets the AppliedConfig.Env rule
// against the agent's scopes (GITHUB_TOKEN, both secret names and the
// unsourced key go; INLINE_PLAIN stays because the snapshot's own
// InlineConfig resolves it), and each InlineConfig.Env and
// CreateInputs.InlineConfig.Env gets the narrow rule.
func TestAppliedConfigEnvCleanupStripsReincarnationSnapshots(t *testing.T) {
	for _, state := range []string{store.AgentReincarnationStateCompleted, store.AgentReincarnationStateFailed} {
		t.Run(state, func(t *testing.T) {
			f := newReincarnationCleanupFixture(t, "reinc-strip-"+state)
			id := tid("reinc-strip-record-" + state)
			f.createRecord(t, id, f.agent.ID, state)

			log := f.run(t, nil)

			rec := f.get(t, id)
			for name, cfg := range map[string]*store.AgentAppliedConfig{"previous": rec.PreviousAppliedConfig, "new": rec.NewAppliedConfig} {
				require.NotNil(t, cfg, name)
				assertEnvKeys(t, cfg.Env, []string{"INLINE_PLAIN"}, name+".env")
				assertEnvKeys(t, cfg.InlineConfig.Env, []string{"INLINE_PLAIN"}, name+".inlineConfig.env")
				assertEnvKeys(t, cfg.CreateInputs.InlineConfig.Env, []string{"INLINE_PLAIN"}, name+".createInputs.inlineConfig.env")
				assert.Equal(t, "inline-plain-value", cfg.Env["INLINE_PLAIN"], name)
				assert.Equal(t, "example/image:1", cfg.Image, name+": non-env fields are untouched")

				for _, field := range []string{"env key=UNSOURCED_VAR", "env key=GITHUB_TOKEN", "inlineConfig.env key=USER_SECRET", "createInputs.inlineConfig.env key=USER_SECRET"} {
					assert.Contains(t, log, "STRIP reincarnation="+id+" agent="+f.agent.ID+" field=reincarnation."+name+"."+field)
				}
			}
			assert.Equal(t, state, rec.State, "state is untouched")
			assert.Contains(t, log, "Scanned 1 reincarnation record(s); normalized 1 (16 field(s)); skipped 0 non-terminal record(s).")
		})
	}
}

// failingSnapshotStore rejects the snapshot update for one record ID and
// passes every other call through.
type failingSnapshotStore struct {
	store.Store
	failID string
}

func (s *failingSnapshotStore) UpdateAgentReincarnationSnapshots(ctx context.Context, r *store.AgentReincarnation, expectState string) (bool, error) {
	if r.ID == s.failID {
		return false, errors.New("update rejected")
	}
	return s.Store.UpdateAgentReincarnationSnapshots(ctx, r, expectState)
}

// TestAppliedConfigEnvCleanupContinuesPastFailedReincarnationUpdate checks
// that a record whose update fails is reported and skipped, the records
// after it are still cleaned, and the summary line is still printed.
func TestAppliedConfigEnvCleanupContinuesPastFailedReincarnationUpdate(t *testing.T) {
	f := newReincarnationCleanupFixture(t, "reinc-fail")
	idA, idB := tid("reinc-fail-record-a"), tid("reinc-fail-record-b")
	f.createRecord(t, idA, f.agent.ID, store.AgentReincarnationStateCompleted)
	f.createRecord(t, idB, f.agent.ID, store.AgentReincarnationStateCompleted)
	// Fail the record the scan reaches first (records are paged by ID).
	failID, okID := idA, idB
	if idB < idA {
		failID, okID = idB, idA
	}

	exec := &AppliedConfigEnvCleanupExecutor{Store: &failingSnapshotStore{Store: f.store, failID: failID}, SecretBackend: f.backend}
	var logBuf bytes.Buffer
	require.NoError(t, exec.Run(context.Background(), &logBuf, nil))
	log := logBuf.String()
	assert.NotContains(t, log, "value-must-not-appear-in-log", "the cleanup log must carry key names only")

	assert.Contains(t, log, "WARN reincarnation="+failID+" - failed to update: update rejected")
	assert.Contains(t, log, "Scanned 2 reincarnation record(s);")

	failed := f.get(t, failID)
	assert.Contains(t, failed.PreviousAppliedConfig.Env, "GITHUB_TOKEN", "the failed record is left as it was")
	cleaned := f.get(t, okID)
	for name, cfg := range map[string]*store.AgentAppliedConfig{"previous": cleaned.PreviousAppliedConfig, "new": cleaned.NewAppliedConfig} {
		assertEnvKeys(t, cfg.Env, []string{"INLINE_PLAIN"}, name+".env")
	}
}

// TestAppliedConfigEnvCleanupReincarnationOfDeletedAgent covers a record
// whose agent row is gone: there are no agent scopes, so Env falls back to
// the narrow rule against the hub scope. GITHUB_TOKEN and the hub secret
// name go; the unsourced key and the name of the former owner's secret stay.
func TestAppliedConfigEnvCleanupReincarnationOfDeletedAgent(t *testing.T) {
	f := newReincarnationCleanupFixture(t, "reinc-gone")
	id := tid("reinc-gone-record")
	f.createRecord(t, id, tid("reinc-gone-missing-agent"), store.AgentReincarnationStateCompleted)

	f.run(t, nil)

	rec := f.get(t, id)
	for name, cfg := range map[string]*store.AgentAppliedConfig{"previous": rec.PreviousAppliedConfig, "new": rec.NewAppliedConfig} {
		assertEnvKeys(t, cfg.Env, []string{"USER_SECRET", "INLINE_PLAIN", "UNSOURCED_VAR"}, name+".env")
		assertEnvKeys(t, cfg.InlineConfig.Env, []string{"USER_SECRET", "INLINE_PLAIN"}, name+".inlineConfig.env")
		assertEnvKeys(t, cfg.CreateInputs.InlineConfig.Env, []string{"USER_SECRET", "INLINE_PLAIN"}, name+".createInputs.inlineConfig.env")
	}
}

// TestAppliedConfigEnvCleanupSkipsNonTerminalReincarnations checks that a
// record a running reincarnation still owns is left alone and counted.
func TestAppliedConfigEnvCleanupSkipsNonTerminalReincarnations(t *testing.T) {
	f := newReincarnationCleanupFixture(t, "reinc-live")
	id := tid("reinc-live-record")
	f.createRecord(t, id, f.agent.ID, store.AgentReincarnationStateProvisioning)

	log := f.run(t, nil)

	rec := f.get(t, id)
	assert.Equal(t, cleanupSnapshotFixture().Env, rec.PreviousAppliedConfig.Env)
	assert.Equal(t, cleanupSnapshotFixture().CreateInputs.InlineConfig.Env, rec.NewAppliedConfig.CreateInputs.InlineConfig.Env)
	assert.Contains(t, log, "skipped 1 non-terminal reincarnation record(s)")
	assert.NotContains(t, log, "reincarnation="+id)
}

// TestAppliedConfigEnvCleanupReincarnationDryRun checks that a dry run
// reports each key in the same form as a real run and writes nothing.
func TestAppliedConfigEnvCleanupReincarnationDryRun(t *testing.T) {
	f := newReincarnationCleanupFixture(t, "reinc-dry")
	id := tid("reinc-dry-record")
	f.createRecord(t, id, f.agent.ID, store.AgentReincarnationStateCompleted)
	before := f.get(t, id)

	log := f.run(t, map[string]string{"dryRun": "true"})

	assert.Contains(t, log, "WOULD STRIP reincarnation="+id+" agent="+f.agent.ID+" field=reincarnation.previous.env key=GITHUB_TOKEN")
	assert.Contains(t, log, "WOULD STRIP reincarnation="+id+" agent="+f.agent.ID+" field=reincarnation.new.createInputs.inlineConfig.env key=USER_SECRET")
	after := f.get(t, id)
	assert.Equal(t, before.PreviousAppliedConfig, after.PreviousAppliedConfig)
	assert.Equal(t, before.NewAppliedConfig, after.NewAppliedConfig)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt)
}

// TestAppliedConfigEnvCleanupReincarnationIsIdempotent checks that a second
// run over already-cleaned records finds nothing to do.
func TestAppliedConfigEnvCleanupReincarnationIsIdempotent(t *testing.T) {
	f := newReincarnationCleanupFixture(t, "reinc-idem")
	id := tid("reinc-idem-record")
	f.createRecord(t, id, f.agent.ID, store.AgentReincarnationStateCompleted)

	f.run(t, nil)
	first := f.get(t, id)
	log := f.run(t, nil)
	second := f.get(t, id)

	assert.Contains(t, log, "Scanned 1 reincarnation record(s); normalized 0 (0 field(s)); skipped 0 non-terminal record(s).")
	assert.NotContains(t, log, "STRIP")
	assert.Equal(t, first.PreviousAppliedConfig, second.PreviousAppliedConfig)
	assert.Equal(t, first.NewAppliedConfig, second.NewAppliedConfig)
}

// TestAppliedConfigEnvCleanupStripsAgentCreateInputsEnv covers the agent row's
// CreateInputs.InlineConfig.Env, which gets the narrow rule: GITHUB_TOKEN and
// a live secret name go, an explicit plain key stays.
func TestAppliedConfigEnvCleanupStripsAgentCreateInputsEnv(t *testing.T) {
	f := newReincarnationCleanupFixture(t, "agent-ci")
	ctx := context.Background()
	agent, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	agent.AppliedConfig.CreateInputs = cleanupSnapshotFixture().CreateInputs
	require.NoError(t, f.store.UpdateAgent(ctx, agent))

	log := f.run(t, nil)

	updated, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assertEnvKeys(t, updated.AppliedConfig.CreateInputs.InlineConfig.Env, []string{"INLINE_PLAIN"}, "createInputs.inlineConfig.env")
	assert.Contains(t, log, "STRIP agent="+f.agent.ID+" field=createInputs.inlineConfig.env key=GITHUB_TOKEN")
	assert.Contains(t, log, "STRIP agent="+f.agent.ID+" field=createInputs.inlineConfig.env key=USER_SECRET")

	log = f.run(t, nil)
	assert.NotContains(t, log, "STRIP", "a second run is a no-op")
}
