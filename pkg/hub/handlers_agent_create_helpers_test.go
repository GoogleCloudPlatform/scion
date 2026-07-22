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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPopulateAgentConfig_RelativeWorkspacePreserved(t *testing.T) {
	// Set up a temp HOME so hubManagedProjectPath can resolve.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	slug := "test-project"
	globalDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "projects", slug), 0755))

	srv, _ := testServer(t)

	project := &store.Project{
		ID:   tid("project-rel-ws"),
		Name: "Test Project",
		Slug: slug,
		// No GitRemote — hub-managed project
	}

	t.Run("relative workspace preserved", func(t *testing.T) {
		agent := &store.Agent{
			ID: tid("agent-rel"),
			AppliedConfig: &store.AgentAppliedConfig{
				Workspace: "packages/web",
			},
		}

		srv.populateAgentConfig(context.Background(), agent, project, nil)

		assert.Equal(t, "packages/web", agent.AppliedConfig.Workspace,
			"relative workspace should not be overwritten by hub-managed path")
	})

	t.Run("absolute workspace preserved", func(t *testing.T) {
		agent := &store.Agent{
			ID: tid("agent-abs"),
			AppliedConfig: &store.AgentAppliedConfig{
				Workspace: "/absolute/path",
			},
		}

		srv.populateAgentConfig(context.Background(), agent, project, nil)

		assert.Equal(t, "/absolute/path", agent.AppliedConfig.Workspace,
			"absolute workspace should be preserved as user override")
	})

	t.Run("empty workspace overwritten", func(t *testing.T) {
		agent := &store.Agent{
			ID: tid("agent-empty"),
			AppliedConfig: &store.AgentAppliedConfig{
				Workspace: "",
			},
		}

		srv.populateAgentConfig(context.Background(), agent, project, nil)

		expectedPath, err := hubManagedProjectPath(slug)
		require.NoError(t, err)
		assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace,
			"empty workspace should be overwritten with hub-managed path")
	})
}

// =============================================================================
// mergeInjectedSkills integration tests
// =============================================================================

func TestMergeInjectedSkills_ProjectScope(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("merge-proj-1"),
		Name: "Merge Test Project",
		Slug: "merge-test-project-1",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://team-tool@1.0",
	}))

	agent := &store.Agent{
		ID:            tid("merge-agent-proj"),
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.mergeInjectedSkills(ctx, agent, project)

	require.NotNil(t, agent.AppliedConfig.InlineConfig)
	assert.Contains(t, extractSkillURIs(agent.AppliedConfig.InlineConfig.Skills), "scion://team-tool@1.0")
}

func TestMergeInjectedSkills_UserScope(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID := tid("merge-user-1")
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  userID,
		SkillURI: "scion://personal-tool@2.0",
	}))

	agent := &store.Agent{
		ID:            tid("merge-agent-user"),
		OwnerID:       userID,
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.mergeInjectedSkills(ctx, agent, nil)

	require.NotNil(t, agent.AppliedConfig.InlineConfig)
	assert.Contains(t, extractSkillURIs(agent.AppliedConfig.InlineConfig.Skills), "scion://personal-tool@2.0")
}

func TestMergeInjectedSkills_HubScope(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	setting := api.HubSkillInjectionSetting{
		System:      []api.SkillReference{{URI: "scion://system-skill@1.0"}},
		UserDefined: []api.SkillReference{{URI: "scion://hub-tool@3.0"}},
	}
	raw, err := json.Marshal(setting)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "test", -1, "seeded")
	require.NoError(t, err)

	agent := &store.Agent{
		ID:            tid("merge-agent-hub"),
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.mergeInjectedSkills(ctx, agent, nil)

	require.NotNil(t, agent.AppliedConfig.InlineConfig)
	uris := extractSkillURIs(agent.AppliedConfig.InlineConfig.Skills)
	assert.Contains(t, uris, "scion://system-skill@1.0")
	assert.Contains(t, uris, "scion://hub-tool@3.0")
}

// TestMergeInjectedSkills_VersionConflict verifies that when the same base URI
// appears at two scopes with different version pins, the more-specific scope wins.
// Per the design (template > project > user > hub), project beats hub.
func TestMergeInjectedSkills_VersionConflict(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Hub declares version @1.0.
	setting := api.HubSkillInjectionSetting{
		UserDefined: []api.SkillReference{{URI: "scion://shared-tool@1.0"}},
	}
	raw, err := json.Marshal(setting)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "test", -1, "managed")
	require.NoError(t, err)

	project := &store.Project{
		ID:   tid("merge-proj-conflict"),
		Name: "Conflict Test",
		Slug: "conflict-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Project declares version @2.0 — higher specificity, must win.
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://shared-tool@2.0",
	}))

	agent := &store.Agent{
		ID:            tid("merge-agent-conflict"),
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.mergeInjectedSkills(ctx, agent, project)

	require.NotNil(t, agent.AppliedConfig.InlineConfig)
	uris := extractSkillURIs(agent.AppliedConfig.InlineConfig.Skills)
	assert.Contains(t, uris, "scion://shared-tool@2.0", "project version must win")
	assert.NotContains(t, uris, "scion://shared-tool@1.0", "hub version must be superseded")
}

func TestMergeInjectedSkills_TemplateSkillsPreservedAndMerged(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("merge-proj-tmpl"),
		Name: "Template Skills Project",
		Slug: "template-skills-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://injected-skill@1.0",
	}))

	agent := &store.Agent{
		ID: tid("merge-agent-tmpl"),
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{
				Skills: []api.SkillReference{
					{URI: "scion://template-skill@3.0"},
				},
			},
		},
	}

	srv.mergeInjectedSkills(ctx, agent, project)

	uris := extractSkillURIs(agent.AppliedConfig.InlineConfig.Skills)
	assert.Contains(t, uris, "scion://injected-skill@1.0", "injected skill must appear")
	assert.Contains(t, uris, "scion://template-skill@3.0", "template skill must be preserved")
}

// TestMergeInjectedSkills_TemplateWinsVersionConflict verifies that when the
// same base URI appears at both project and template scope, the template wins.
func TestMergeInjectedSkills_TemplateWinsVersionConflict(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("merge-proj-tmpl-conflict"),
		Name: "Template Conflict Project",
		Slug: "template-conflict-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://shared-skill@1.0",
	}))

	agent := &store.Agent{
		ID: tid("merge-agent-tmpl-conflict"),
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{
				Skills: []api.SkillReference{
					{URI: "scion://shared-skill@5.0"}, // template pins higher version
				},
			},
		},
	}

	srv.mergeInjectedSkills(ctx, agent, project)

	uris := extractSkillURIs(agent.AppliedConfig.InlineConfig.Skills)
	assert.Contains(t, uris, "scion://shared-skill@5.0", "template version must win")
	assert.NotContains(t, uris, "scion://shared-skill@1.0", "project version must be superseded")
}

// =============================================================================
// mergeSkillRefs unit tests
// =============================================================================

func TestMergeSkillRefs_PrecedenceOrder(t *testing.T) {
	hub := []api.SkillReference{{URI: "scion://shared-skill@1.0"}}
	user := []api.SkillReference{{URI: "scion://shared-skill@2.0"}}
	project := []api.SkillReference{{URI: "scion://shared-skill@3.0"}}
	template := []api.SkillReference{{URI: "scion://shared-skill@4.0"}}

	result := mergeSkillRefs(hub, user, project, template)

	require.Len(t, result, 1)
	assert.Equal(t, "scion://shared-skill@4.0", result[0].URI, "template (highest precedence) should win")
}

func TestMergeSkillRefs_ProjectBeatsHub(t *testing.T) {
	hub := []api.SkillReference{{URI: "scion://shared-skill@1.0"}}
	project := []api.SkillReference{{URI: "scion://shared-skill@9.0"}}

	result := mergeSkillRefs(hub, nil, project)

	require.Len(t, result, 1)
	assert.Equal(t, "scion://shared-skill@9.0", result[0].URI, "project should win over hub")
}

func TestMergeSkillRefs_EmptyScopes_NoPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		result := mergeSkillRefs(nil, nil, nil, nil)
		assert.Empty(t, result)
	})
}

func TestMergeSkillRefs_UniqueURIsAreUnioned(t *testing.T) {
	hub := []api.SkillReference{{URI: "scion://hub-skill@1.0"}}
	project := []api.SkillReference{{URI: "scion://project-skill@1.0"}}
	template := []api.SkillReference{{URI: "scion://template-skill@1.0"}}

	result := mergeSkillRefs(hub, nil, project, template)

	uris := extractSkillURIs(result)
	assert.Contains(t, uris, "scion://hub-skill@1.0")
	assert.Contains(t, uris, "scion://project-skill@1.0")
	assert.Contains(t, uris, "scion://template-skill@1.0")
	assert.Len(t, result, 3)
}

func TestMergeSkillRefs_DuplicateWithinScopeDeduped(t *testing.T) {
	refs := []api.SkillReference{
		{URI: "scion://my-skill@1.0"},
		{URI: "scion://my-skill@1.0"},
	}

	result := mergeSkillRefs(refs)

	assert.Len(t, result, 1)
	assert.Equal(t, "scion://my-skill@1.0", result[0].URI)
}

func TestMergeSkillRefs_VersionConflictWarnAndWin(t *testing.T) {
	// Same base URI at two different scopes — higher-index scope wins.
	low := []api.SkillReference{{URI: "scion://tool@1.0"}}
	high := []api.SkillReference{{URI: "scion://tool@2.0"}}

	// No panic, correct winner.
	result := mergeSkillRefs(low, high)

	require.Len(t, result, 1)
	assert.Equal(t, "scion://tool@2.0", result[0].URI, "higher-precedence scope must win")
}

// =============================================================================
// skillBaseURI unit tests
// =============================================================================

func TestSkillBaseURI_StripVersion(t *testing.T) {
	cases := []struct {
		uri      string
		expected string
	}{
		{"scion://my-skill@1.0", "scion://my-skill"},
		{"scion://my-skill@2.3.4", "scion://my-skill"},
		{"scion://my-skill", "scion://my-skill"},
		{"scion://org/my-skill@1.0", "scion://org/my-skill"},
		{"https://example.com/skills/my-skill@1.0", "https://example.com/skills/my-skill"},
		{"https://example.com/skills/my-skill", "https://example.com/skills/my-skill"},
		{"scion://my-skill@latest", "scion://my-skill"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			got := skillBaseURI(tc.uri)
			assert.Equal(t, tc.expected, got)
		})
	}
}

// =============================================================================
// Helpers
// =============================================================================

// extractSkillURIs returns the URI strings from a SkillReference slice.
func extractSkillURIs(refs []api.SkillReference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.URI)
	}
	return out
}
