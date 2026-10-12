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
	"maps"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Auto-expose legacy handling (ptone/scion#2562 §5(b) as amended by design
// A8 and A9): the auto-expose-env-normalize migration re-derives a stamped
// SCION_AUTO_EXPOSE_PORTS, and the env cleanup exempts the auto-expose keys
// from its AppliedConfig.Env allowlist.

type aeNormalizeAgent struct {
	projectAnno  string            // "" = no annotation
	templateEnv  map[string]string // nil = no template
	templateID   string            // the agent's recorded template ID; may be stale
	appliedEnv   map[string]string
	inlineEnv    map[string]string
	createInputs *store.AgentCreateInputs // nil = legacy agent
}

func setupAENormalizeAgent(t *testing.T, in aeNormalizeAgent) (*Server, store.Store, *store.Project, *store.Agent) {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	if in.projectAnno != "" {
		setProjectAnnotations(t, s, project, map[string]string{projectSettingAutoExposePortsEnabled: in.projectAnno})
		var err error
		project, err = s.GetProject(context.Background(), project.ID)
		require.NoError(t, err)
	}
	if in.templateEnv != nil {
		createAutoExposeTemplate(t, s, "ae-norm-tmpl", in.templateEnv)
	}
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		if in.templateEnv != nil {
			a.Template = "ae-norm-tmpl"
			a.AppliedConfig.TemplateID = in.templateID
		}
		a.AppliedConfig.Env = in.appliedEnv
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: in.inlineEnv}
		a.AppliedConfig.CreateInputs = in.createInputs
	})
	loaded, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	return srv, s, project, loaded
}

func runAENormalize(t *testing.T, s store.Store, params map[string]string) string {
	t.Helper()
	exec := &AutoExposeEnvNormalizeExecutor{Store: s}
	var buf bytes.Buffer
	require.NoError(t, exec.Run(context.Background(), &buf, params))
	return buf.String()
}

func reloadAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

func explicitKeep() *store.AgentCreateInputs {
	return &store.AgentCreateInputs{Workspace: "/tmp/reincarnate-workspace", InlineConfig: &api.ScionConfig{Env: map[string]string{"KEEP": "1"}}}
}

// TestAutoExposeEnvNormalize_MatchesReincarnate pins that the
// normalized SCION_AUTO_EXPOSE_PORTS equals what reincarnate derives for the
// same agent, that the stamp leaves InlineConfig.Env, and that a second run
// is a no-op. A stale stamp with no project or template value drops to
// inherited (the hub default at dispatch).
func TestAutoExposeEnvNormalize_MatchesReincarnate(t *testing.T) {
	cases := []struct {
		name        string
		projectAnno string
		templateEnv map[string]string
		appliedAE   string // "" = stamp in InlineConfig.Env only
		want        string // "" = absent
	}{
		{name: "inline-only hub stamp drops to inherited", want: ""},
		{name: "inline-only stamp, project annotation wins", projectAnno: "true", want: "true"},
		{name: "stamp in both maps drops to inherited", appliedAE: "false", want: ""},
		{name: "stamp in both maps, template tier re-derived", templateEnv: map[string]string{aeKey: "false"}, appliedAE: "true", want: "false"},
		{name: "stamp in both maps, project beats template", projectAnno: "false", templateEnv: map[string]string{aeKey: "true"}, appliedAE: "true", want: "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stamp := "false"
			if tc.want == "false" {
				stamp = "true"
			}
			applied := map[string]string{"KEEP": "1"}
			if tc.appliedAE != "" {
				applied[aeKey] = tc.appliedAE
				stamp = tc.appliedAE
			}
			srv, s, project, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
				projectAnno:  tc.projectAnno,
				templateEnv:  tc.templateEnv,
				appliedEnv:   applied,
				inlineEnv:    map[string]string{"KEEP": "1", aeKey: stamp},
				createInputs: explicitKeep(),
			})
			fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project, "")
			require.NoError(t, err)

			log := runAENormalize(t, s, nil)
			assert.Contains(t, log, "RE-DERIVE agent="+agent.ID+" key="+aeKey)
			assert.Contains(t, log, "re-derived auto-expose on 1 agent(s)")

			got := reloadAgent(t, s, agent.ID)
			freshAE, freshHas := fresh.Env[aeKey]
			gotAE, gotHas := got.AppliedConfig.Env[aeKey]
			assert.Equal(t, freshHas, gotHas, "presence must match reincarnate")
			assert.Equal(t, freshAE, gotAE, "value must match reincarnate")
			if tc.want == "" {
				assert.False(t, gotHas)
			} else {
				assert.Equal(t, tc.want, gotAE)
			}
			assert.Equal(t, map[string]string{"KEEP": "1"}, inlineEnv(got), "the stamp leaves InlineConfig.Env")
			assert.Equal(t, map[string]string{"KEEP": "1"}, createInputsEnv(got), "CreateInputs is untouched")
			assert.Equal(t, "1", got.AppliedConfig.Env["KEEP"])

			version := got.StateVersion
			log2 := runAENormalize(t, s, nil)
			assert.NotContains(t, log2, "RE-DERIVE")
			assert.Equal(t, version, reloadAgent(t, s, agent.ID).StateVersion, "second run is a no-op")
		})
	}
}

// TestAutoExposeEnvNormalize_LeavesOthersAlone covers the agents
// normalization must not touch: an explicit value (in CreateInputs), and an
// agent without CreateInputs, which cannot tell a stamp from an explicit
// value.
func TestAutoExposeEnvNormalize_LeavesOthersAlone(t *testing.T) {
	cases := []struct {
		name string
		in   aeNormalizeAgent
	}{
		{"explicit value", aeNormalizeAgent{
			projectAnno: "true",
			appliedEnv:  map[string]string{aeKey: "false"},
			inlineEnv:   map[string]string{aeKey: "false"},
			createInputs: &store.AgentCreateInputs{Workspace: "/tmp/reincarnate-workspace",
				InlineConfig: &api.ScionConfig{Env: map[string]string{aeKey: "false"}}},
		}},
		{"no CreateInputs", aeNormalizeAgent{
			projectAnno: "false",
			appliedEnv:  map[string]string{"KEEP": "1"},
			inlineEnv:   map[string]string{"KEEP": "1", aeKey: "true"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, s, _, agent := setupAENormalizeAgent(t, tc.in)
			wantApplied := maps.Clone(agent.AppliedConfig.Env)
			wantInline := maps.Clone(inlineEnv(agent))

			log := runAENormalize(t, s, nil)
			assert.NotContains(t, log, "RE-DERIVE")

			got := reloadAgent(t, s, agent.ID)
			assert.Equal(t, wantApplied, got.AppliedConfig.Env)
			assert.Equal(t, wantInline, inlineEnv(got))
			assert.Equal(t, agent.StateVersion, got.StateVersion, "no write")
		})
	}
}

// TestAutoExposeEnvNormalize_DryRun reports the re-derivation
// without writing.
func TestAutoExposeEnvNormalize_DryRun(t *testing.T) {
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		appliedEnv:   map[string]string{"KEEP": "1"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
		createInputs: explicitKeep(),
	})
	log := runAENormalize(t, s, map[string]string{"dryRun": "true"})
	assert.Contains(t, log, "WOULD RE-DERIVE agent="+agent.ID+" key="+aeKey)
	got := reloadAgent(t, s, agent.ID)
	assert.Equal(t, "true", inlineEnv(got)[aeKey])
	assert.Equal(t, agent.StateVersion, got.StateVersion)
}

// TestEnvCleanup_AutoExposeKeysExemptFromAllowlist is the drift test: a
// project-derived SCION_AUTO_EXPOSE_PORTS in AppliedConfig.Env has no plain
// source the allowlist can match, and neither do kept _PORTS_LIST and
// _INTERVAL values, yet the cleanup must keep them. The exemption is by exact
// name: SCION_AUTO_EXPOSE_MODE and other unsourced keys are still stripped.
func TestEnvCleanup_AutoExposeKeysExemptFromAllowlist(t *testing.T) {
	derived := map[string]string{
		aeKey:                          "true",
		"SCION_AUTO_EXPOSE_PORTS_LIST": "8080",
		"SCION_AUTO_EXPOSE_INTERVAL":   "5s",
	}
	stripped := map[string]string{
		"SCION_AUTO_EXPOSE_MODE":     "all",
		"SCION_AUTO_EXPOSE_MIN_PORT": "1024",
		"UNSOURCED_VAR":              "x",
	}
	applied := maps.Clone(derived)
	maps.Copy(applied, stripped)
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		projectAnno:  "true",
		appliedEnv:   applied,
		createInputs: &store.AgentCreateInputs{Workspace: "/tmp/reincarnate-workspace"},
	})

	snapshot := func() *store.AgentAppliedConfig {
		return &store.AgentAppliedConfig{Env: maps.Clone(applied), CreateInputs: &store.AgentCreateInputs{}}
	}
	recID := tid("ae-exempt-record")
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		ID: recID, AgentID: agent.ID, FromGeneration: 1, ToGeneration: 2,
		RequestedAt: time.Now(), State: store.AgentReincarnationStateCompleted,
		PreviousAppliedConfig: snapshot(), NewAppliedConfig: snapshot(),
	}))

	var buf bytes.Buffer
	require.NoError(t, (&AppliedConfigEnvCleanupExecutor{Store: s}).Run(context.Background(), &buf, nil))

	assert.Equal(t, derived, reloadAgent(t, s, agent.ID).AppliedConfig.Env)
	rec, err := s.GetAgentReincarnation(context.Background(), recID)
	require.NoError(t, err)
	assert.Equal(t, derived, rec.PreviousAppliedConfig.Env, "previous snapshot")
	assert.Equal(t, derived, rec.NewAppliedConfig.Env, "new snapshot")
}

// TestAutoExposeEnvNormalize_LeavesInlineTZToTZCleanup pins F2:
// normalization touches SCION_AUTO_EXPOSE_PORTS only, so a historical TZ held
// only in InlineConfig.Env stays for adoptLegacyTZ, which the TZ cleanup (and
// every TZ reader) runs, and it becomes a legacy pin.
func TestAutoExposeEnvNormalize_LeavesInlineTZToTZCleanup(t *testing.T) {
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		appliedEnv:   map[string]string{"KEEP": "1"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true", agentTZEnvKey: "Asia/Tokyo"},
		createInputs: explicitKeep(),
	})

	runAENormalize(t, s, nil)
	got := reloadAgent(t, s, agent.ID)
	assert.NotContains(t, inlineEnv(got), aeKey)
	assert.Equal(t, "Asia/Tokyo", inlineEnv(got)[agentTZEnvKey], "normalization leaves TZ alone")

	var buf bytes.Buffer
	_, err := (&AppliedConfigTZCleanupExecutor{Store: s}).run(context.Background(), &buf, nil)
	require.NoError(t, err)
	got = reloadAgent(t, s, agent.ID)
	assert.Equal(t, "Asia/Tokyo", got.AppliedConfig.ExplicitTimezone)
	assert.True(t, got.AppliedConfig.ExplicitTimezoneLegacy)
	assert.NotContains(t, inlineEnv(got), agentTZEnvKey)
	assert.NotContains(t, inlineEnv(got), aeKey)
}

// projectLookupStore makes GetProject fail with err.
type projectLookupStore struct {
	store.Store
	err error
}

func (s *projectLookupStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	return nil, s.err
}

// TestAutoExposeEnvNormalize_ProjectLookup covers the project
// lookup: a project that no longer exists contributes no tier, as at
// reincarnate, while any other lookup failure skips the agent so the next run
// retries it instead of dropping the project tier.
func TestAutoExposeEnvNormalize_ProjectLookup(t *testing.T) {
	t.Run("project gone", func(t *testing.T) {
		_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
			appliedEnv:   map[string]string{"KEEP": "1"},
			inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
			createInputs: explicitKeep(),
		})
		log := runAENormalize(t, &projectLookupStore{Store: s, err: store.ErrNotFound}, nil)
		assert.Contains(t, log, "RE-DERIVE agent="+agent.ID)
		got := reloadAgent(t, s, agent.ID)
		assert.NotContains(t, inlineEnv(got), aeKey)
		assert.NotContains(t, got.AppliedConfig.Env, aeKey)
	})
	t.Run("lookup error skips the agent", func(t *testing.T) {
		_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
			appliedEnv:   map[string]string{"KEEP": "1"},
			inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
			createInputs: explicitKeep(),
		})
		log := runAENormalize(t, &projectLookupStore{Store: s, err: errors.New("db down")}, nil)
		assert.Contains(t, log, "WARN agent="+agent.ID+" - skipped, retried by the next run")
		assert.NotContains(t, log, "RE-DERIVE")
		got := reloadAgent(t, s, agent.ID)
		assert.Equal(t, "true", inlineEnv(got)[aeKey])
		assert.Equal(t, agent.StateVersion, got.StateVersion)
	})
}

// TestAutoExposeEnvNormalize_TemplateRepushed covers N3: the
// template is resolved by the agent's template reference, as reincarnate
// does, so a template deleted and re-pushed under a new ID still supplies
// its tier.
func TestAutoExposeEnvNormalize_TemplateRepushed(t *testing.T) {
	srv, s, project, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		templateEnv:  map[string]string{aeKey: "false"},
		templateID:   tid("template-deleted-before-repush"),
		appliedEnv:   map[string]string{"KEEP": "1", aeKey: "true"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
		createInputs: explicitKeep(),
	})
	fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project, "")
	require.NoError(t, err)
	require.Equal(t, "false", fresh.Env[aeKey])

	runAENormalize(t, s, nil)
	assert.Equal(t, "false", reloadAgent(t, s, agent.ID).AppliedConfig.Env[aeKey])
}

// templateLookupStore makes GetTemplate fail with err.
type templateLookupStore struct {
	store.Store
	err error
}

func (s *templateLookupStore) GetTemplate(ctx context.Context, id string) (*store.Template, error) {
	return nil, s.err
}

// TestAutoExposeEnvNormalize_TemplateLookupError covers N2: a
// template lookup failure skips the agent rather than dropping the template
// tier.
func TestAutoExposeEnvNormalize_TemplateLookupError(t *testing.T) {
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		templateEnv:  map[string]string{aeKey: "false"},
		appliedEnv:   map[string]string{"KEEP": "1"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
		createInputs: explicitKeep(),
	})
	log := runAENormalize(t, &templateLookupStore{Store: s, err: errors.New("db down")}, nil)
	assert.Contains(t, log, "WARN agent="+agent.ID+" - skipped, retried by the next run")
	assert.Contains(t, log, "skipped 1 agent(s)")
	got := reloadAgent(t, s, agent.ID)
	assert.Equal(t, "true", inlineEnv(got)[aeKey])
	assert.Equal(t, agent.StateVersion, got.StateVersion)
}

// failingUpdateStore rejects every agent update.
type failingUpdateStore struct {
	store.Store
}

func (s *failingUpdateStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	return errors.New("update rejected")
}

// TestAutoExposeEnvNormalize_CountsOnlyWrites covers N4: an agent
// whose write fails is reported as skipped, not as re-derived.
func TestAutoExposeEnvNormalize_CountsOnlyWrites(t *testing.T) {
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		appliedEnv:   map[string]string{"KEEP": "1"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
		createInputs: explicitKeep(),
	})
	var buf bytes.Buffer
	res, err := (&AutoExposeEnvNormalizeExecutor{Store: &failingUpdateStore{Store: s}}).run(context.Background(), &buf, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.AgentsNormalized)
	assert.Equal(t, 1, res.AgentsSkipped)
	assert.NotContains(t, buf.String(), "RE-DERIVE")
	assert.Contains(t, buf.String(), "WARN agent="+agent.ID+" - failed to update, retried by the next run")
}

// TestAutoExposeEnvNormalize_TouchesOnlyAutoExpose pins that the
// normalization changes SCION_AUTO_EXPOSE_PORTS and nothing else: keys the
// env cleanup would strip (GITHUB_TOKEN, an unsourced key) stay.
func TestAutoExposeEnvNormalize_TouchesOnlyAutoExpose(t *testing.T) {
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		appliedEnv:   map[string]string{"KEEP": "1", "GITHUB_TOKEN": "x", "UNSOURCED_VAR": "y", "SCION_AUTO_EXPOSE_MODE": "all"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true", "GITHUB_TOKEN": "x"},
		createInputs: explicitKeep(),
	})
	runAENormalize(t, s, nil)
	got := reloadAgent(t, s, agent.ID)
	assert.Equal(t, map[string]string{"KEEP": "1", "GITHUB_TOKEN": "x", "UNSOURCED_VAR": "y", "SCION_AUTO_EXPOSE_MODE": "all"}, got.AppliedConfig.Env)
	assert.Equal(t, map[string]string{"KEEP": "1", "GITHUB_TOKEN": "x"}, inlineEnv(got))
}

// TestEnvCleanup_AutoExposeSecretStillStripped covers N1: the allowlist
// exemption never shields a live secret named like an auto-expose key.
func TestEnvCleanup_AutoExposeSecretStillStripped(t *testing.T) {
	_, s, project, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		appliedEnv:   map[string]string{aeKey: "true", "SCION_AUTO_EXPOSE_PORTS_LIST": "8080"},
		createInputs: &store.AgentCreateInputs{Workspace: "/tmp/reincarnate-workspace"},
	})
	require.NoError(t, s.CreateEnvVar(context.Background(), &store.EnvVar{
		ID: tid("ae-secret-envvar"), Key: "SCION_AUTO_EXPOSE_PORTS_LIST", Value: "8080",
		Scope: store.ScopeProject, ScopeID: project.ID, Secret: true,
	}))
	var buf bytes.Buffer
	require.NoError(t, (&AppliedConfigEnvCleanupExecutor{Store: s}).Run(context.Background(), &buf, nil))
	assert.Equal(t, map[string]string{aeKey: "true"}, reloadAgent(t, s, agent.ID).AppliedConfig.Env)
}

// TestAutoExposeEnvNormalize_RerunsThroughExecuteMigration runs the migration
// twice through the admin endpoint: it is seeded, the second run is accepted
// (the key is in rerunnableMigrations), and it writes no agent row.
func TestAutoExposeEnvNormalize_RerunsThroughExecuteMigration(t *testing.T) {
	key := entadapter.AutoExposeEnvNormalizeKey
	ctx := context.Background()
	srv, s := newTestServerWithStore(t)
	require.True(t, rerunnableMigrations[key])

	project := &store.Project{ID: tid("ae-rerun-project"), Name: "AE rerun", Slug: "ae-rerun"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("ae-rerun-agent"), Slug: "ae-rerun-agent", Name: "AE rerun agent", ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env:          map[string]string{"KEEP": "1"},
			InlineConfig: &api.ScionConfig{Env: map[string]string{"KEEP": "1", aeKey: "true"}},
			CreateInputs: explicitKeep(),
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	first := runMigrationViaHandler(t, srv, s, key, `{}`)
	require.Equal(t, store.MaintenanceStatusCompleted, first.Status, first.Result)
	assert.Contains(t, first.Result, "re-derived auto-expose on 1 agent(s)")
	after := reloadAgent(t, s, agent.ID)
	assert.NotContains(t, inlineEnv(after), aeKey)

	second := runMigrationViaHandler(t, srv, s, key, `{}`)
	require.Equal(t, store.MaintenanceStatusCompleted, second.Status, second.Result)
	assert.Contains(t, second.Result, "re-derived auto-expose on 0 agent(s)")
	assert.Equal(t, after.StateVersion, reloadAgent(t, s, agent.ID).StateVersion)
}

// aeConflictOnceStore fails the first agent update with ErrVersionConflict.
// When concurrentPatch is set, it first makes SCION_AUTO_EXPOSE_PORTS
// explicit on the stored row, as a configure PATCH racing the migration
// would.
type aeConflictOnceStore struct {
	store.Store
	concurrentPatch bool
	conflicted      bool
}

func (s *aeConflictOnceStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if s.conflicted {
		return s.Store.UpdateAgent(ctx, a)
	}
	s.conflicted = true
	if s.concurrentPatch {
		latest, err := s.GetAgent(ctx, a.ID)
		if err != nil {
			return err
		}
		latest.AppliedConfig.CreateInputs.InlineConfig.Env[aeKey] = "true"
		if err := s.Store.UpdateAgent(ctx, latest); err != nil {
			return err
		}
	}
	return store.ErrVersionConflict
}

// TestAutoExposeEnvNormalize_RetriesVersionConflict covers the
// optimistic-lock retry: a conflict is retried against the latest row, and a
// row a concurrent PATCH made explicit is neither written nor counted.
func TestAutoExposeEnvNormalize_RetriesVersionConflict(t *testing.T) {
	t.Run("conflict retried", func(t *testing.T) {
		_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
			appliedEnv:   map[string]string{"KEEP": "1"},
			inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
			createInputs: explicitKeep(),
		})
		var buf bytes.Buffer
		res, err := (&AutoExposeEnvNormalizeExecutor{Store: &aeConflictOnceStore{Store: s}}).run(context.Background(), &buf, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, res.AgentsNormalized)
		assert.Equal(t, 0, res.AgentsSkipped)
		assert.NotContains(t, inlineEnv(reloadAgent(t, s, agent.ID)), aeKey)
	})
	t.Run("re-read row no longer needs it", func(t *testing.T) {
		_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
			appliedEnv:   map[string]string{"KEEP": "1"},
			inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
			createInputs: explicitKeep(),
		})
		var buf bytes.Buffer
		res, err := (&AutoExposeEnvNormalizeExecutor{Store: &aeConflictOnceStore{Store: s, concurrentPatch: true}}).run(context.Background(), &buf, nil)
		require.NoError(t, err)
		assert.Equal(t, 0, res.AgentsNormalized)
		assert.NotContains(t, buf.String(), "RE-DERIVE")
		got := reloadAgent(t, s, agent.ID)
		assert.Equal(t, agent.StateVersion+1, got.StateVersion, "only the concurrent PATCH wrote the row")
		assert.Equal(t, "true", inlineEnv(got)[aeKey], "an explicit value is left alone")
	})
}

// TestAutoExposeEnvNormalize_ScansSoftDeletedAgents pins that soft-deleted
// agents are normalized too, as the env and TZ cleanups scan them, so a
// restored agent does not bring a stamp back.
func TestAutoExposeEnvNormalize_ScansSoftDeletedAgents(t *testing.T) {
	_, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.DeletedAt = time.Now()
		a.AppliedConfig.Env = map[string]string{"KEEP": "1"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: map[string]string{"KEEP": "1", aeKey: "true"}}
		a.AppliedConfig.CreateInputs = explicitKeep()
	})
	log := runAENormalize(t, s, nil)
	assert.Contains(t, log, "RE-DERIVE agent="+agent.ID)
	assert.NotContains(t, inlineEnv(reloadAgent(t, s, agent.ID)), aeKey)
}
