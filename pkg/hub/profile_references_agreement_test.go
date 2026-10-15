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
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	entproject "github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	entcontributor "github.com/GoogleCloudPlatform/scion/pkg/ent/projectcontributor"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// columnRestrictedRefInputs reads the reference-count inputs the way the
// rebuild preflight does (ptone/scion#5057): column-restricted queries on
// the raw rows, applied_config decoded by store.AppliedConfigProfile, and
// the shared-workspace flag through store.Project.IsSharedWorkspace.
func columnRestrictedRefInputs(t *testing.T, ctx context.Context, client *ent.Client, brokerID string) agent.ProfileRefInputs {
	t.Helper()
	var in agent.ProfileRefInputs
	contributors, err := client.ProjectContributor.Query().
		Where(entcontributor.BrokerIDEQ(uuid.MustParse(brokerID))).
		Select(entcontributor.FieldProjectID, entcontributor.FieldLocalPath).All(ctx)
	require.NoError(t, err)
	for _, c := range contributors {
		in.Providers = append(in.Providers, agent.ProfileRefProvider{ProjectID: c.ProjectID.String(), LocalPath: c.LocalPath})
	}
	projects, err := client.Project.Query().
		Select(entproject.FieldSlug, entproject.FieldAnnotations, entproject.FieldGitRemote, entproject.FieldLabels).All(ctx)
	require.NoError(t, err)
	for _, p := range projects {
		sp := store.Project{Labels: p.Labels}
		if p.GitRemote != nil {
			sp.GitRemote = *p.GitRemote
		}
		in.Projects = append(in.Projects, agent.ProfileRefProject{
			ID: p.ID.String(), Slug: p.Slug, ActiveProfile: p.Annotations[projectSettingActiveProfile],
			SharedWorkspace: sp.IsSharedWorkspace(),
		})
	}
	agents, err := client.Agent.Query().Where(entagent.DeletedAtIsNil()).
		Select(entagent.FieldSlug, entagent.FieldProjectID, entagent.FieldRuntimeBrokerID, entagent.FieldAppliedConfig).All(ctx)
	require.NoError(t, err)
	for _, a := range agents {
		profile, _ := store.AppliedConfigProfile(a.AppliedConfig)
		in.Agents = append(in.Agents, agent.ProfileRefAgentRow{
			Slug: a.Slug, ProjectID: a.ProjectID.String(), RuntimeBrokerID: a.RuntimeBrokerID, AppliedProfile: profile,
		})
	}
	return in
}

// The hub's reference count (store reads mapped into the shared builder)
// and the preflight's (column-restricted reads into the same builder)
// agree on one fixture: equal references and tallies, and the preflight's
// counted agents sum to the references. The fixture covers linked,
// hub-native, global and unprovided projects, a shared-workspace project
// whose agent-info.json is unreadable, other-broker, unplaced and deleted
// agents, and applied configs that do not decode (invalid JSON, and a
// mistyped field other than profile), unplaced and co-located with no
// agent-info.json.
func TestProfileReferences_HubAndPreflightInputsAgree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	cs, client, err := newTestStoreWithClient(t)
	require.NoError(t, err)
	srv, s := testServerOnMigratedStore(t, cs)

	globalDir := t.TempDir()
	broker := newLocalPathTestBroker(t, s, "agree-broker")
	other := newLocalPathTestBroker(t, s, "agree-other")
	newLinked := func(name string) string {
		p := filepath.Join(t.TempDir(), name, config.DotScion)
		require.NoError(t, os.MkdirAll(p, 0o755))
		return p
	}
	linkedPath, sharedPath := newLinked("linked"), newLinked("shared")

	type proj struct {
		slug, localPath, active string
		provider, shared        bool
	}
	ids := map[string]string{}
	for _, p := range []proj{
		{slug: "agree-linked", localPath: linkedPath, provider: true, active: "local"},
		{slug: "agree-native", provider: true},
		{slug: globalProjectSlug, localPath: globalDir, provider: true},
		{slug: "agree-noprov", active: "remote"},
		{slug: "agree-shared", localPath: sharedPath, provider: true, shared: true},
	} {
		project := &store.Project{ID: tid("agree-" + p.slug), Name: p.slug, Slug: p.slug}
		if p.active != "" {
			project.Annotations = map[string]string{projectSettingActiveProfile: p.active}
		}
		if p.shared {
			project.GitRemote = "https://example.com/agree/shared.git"
			project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}
		}
		require.NoError(t, s.CreateProject(ctx, project))
		ids[p.slug] = project.ID
		if p.provider {
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: p.localPath,
				Status: store.BrokerStatusOnline,
			}))
		}
	}
	nativePath := config.BrokerProjectPath(globalDir, "", "agree-native")

	type ag struct {
		slug, project, brokerID, applied string
		raw                              string // a raw applied_config, written past the store
		deleted                          bool
	}
	for _, a := range []ag{
		{slug: "linked-found", project: "agree-linked", brokerID: broker.ID, applied: "hubval"},
		{slug: "native-missing", project: "agree-native", brokerID: broker.ID, applied: "hubval"},
		{slug: "native-corrupt", project: "agree-native", brokerID: broker.ID, applied: "local"},
		{slug: "global-found", project: globalProjectSlug, brokerID: broker.ID},
		{slug: "noprov", project: "agree-noprov", brokerID: broker.ID, applied: "np"},
		{slug: "unplaced", project: "agree-native", applied: "hubval"},
		{slug: "other", project: "agree-native", brokerID: other.ID, applied: "remote"},
		{slug: "deleted", project: "agree-native", brokerID: broker.ID, applied: "gone", deleted: true},
		{slug: "shared-unreadable", project: "agree-shared", brokerID: broker.ID, applied: "hubval"},
		{slug: "bad-json-unplaced", project: "agree-native", raw: `{"profile":"local"`},
		{slug: "bad-type-unplaced", project: "agree-native", raw: `{"profile":"local","thinkingLevel":"high"}`},
		{slug: "bad-json-placed", project: "agree-native", brokerID: broker.ID, raw: `{"profile":"local"`},
		{slug: "bad-type-placed", project: "agree-native", brokerID: broker.ID, raw: `{"profile":"local","thinkingLevel":"high"}`},
	} {
		id := tid("agree-agent-" + a.slug)
		sa := &store.Agent{ID: id, Slug: a.slug, Name: a.slug, ProjectID: ids[a.project], RuntimeBrokerID: a.brokerID}
		if a.applied != "" {
			sa.AppliedConfig = &store.AgentAppliedConfig{Profile: a.applied}
		}
		require.NoError(t, s.CreateAgent(ctx, sa))
		if a.raw != "" {
			require.NoError(t, client.Agent.UpdateOneID(uuid.MustParse(id)).SetAppliedConfig(a.raw).Exec(ctx))
		}
		if a.deleted {
			require.NoError(t, client.Agent.UpdateOneID(uuid.MustParse(id)).SetDeletedAt(time.Now()).Exec(ctx))
		}
	}
	writeAgentInfo(t, linkedPath, "linked-found", "local")
	writeAgentInfo(t, nativePath, "native-corrupt", "x")
	nativeDir, err := config.GetResolvedProjectDir(nativePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config.GetAgentHomePath(nativeDir, "native-corrupt"), "agent-info.json"), []byte("{not json"), 0o644))
	writeAgentInfo(t, globalDir, "global-found", "remote")
	// The shared-workspace agent: unreadable agent-info.json, provisioned
	// profile in the external agents root.
	writeAgentInfo(t, sharedPath, "shared-unreadable", "x")
	sharedDir, err := config.GetResolvedProjectDir(sharedPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config.GetAgentHomePath(sharedDir, "shared-unreadable"), "agent-info.json"), []byte("{not json"), 0o644))
	stateDir, _, err := agent.AgentStateDir(sharedDir, "shared-unreadable", true, ids["agree-shared"], true)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, config.ImageProvenanceFileName), []byte(`{"version":1,"profile":"shprov"}`), 0o600))

	hubRefs, err := srv.countProfileReferences(ctx, broker.ID, globalDir)
	require.NoError(t, err)
	preflightRefs, counted, ok := agent.BuildProfileReferencesDetailed(globalDir, broker.ID, columnRestrictedRefInputs(t, ctx, client, broker.ID))
	require.True(t, ok)

	assert.Equal(t, hubRefs, preflightRefs, "the hub and the preflight must count the same references")
	// The fixture's expected count, so agreement on a wrong count fails too.
	// "": native-corrupt (no provenance) and the four undecodable configs.
	// missing: native-missing, noprov, bad-json-placed, bad-type-placed.
	// unreadable: native-corrupt, shared-unreadable.
	if want := map[string]int{"local": 1, "remote": 1, "hubval": 2, "np": 1, "shprov": 1, "": 5}; !reflect.DeepEqual(hubRefs.Agents, want) {
		t.Errorf("hub refs.Agents = %v, want %v", hubRefs.Agents, want)
	}
	if want := map[string]int{"local": 1}; !reflect.DeepEqual(hubRefs.Projects, want) {
		t.Errorf("hub refs.Projects = %v, want %v", hubRefs.Projects, want)
	}
	if hubRefs.AgentInfoMissing != 4 || hubRefs.AgentInfoUnreadable != 2 {
		t.Errorf("hub agent-info tallies: missing %d unreadable %d, want 4 and 2", hubRefs.AgentInfoMissing, hubRefs.AgentInfoUnreadable)
	}
	sums := map[string]int{}
	for _, c := range counted {
		sums[c.Profile]++
	}
	if !reflect.DeepEqual(sums, preflightRefs.Agents) {
		t.Errorf("the counted agents sum to %v, refs.Agents is %v", sums, preflightRefs.Agents)
	}
}
