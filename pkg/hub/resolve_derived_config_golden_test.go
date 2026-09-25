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
	"encoding/json"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestResolveDerivedConfig_GoldenCreatePath is the Phase 0 golden test called
// for by the agent-reincarnation design
// (/scion-volumes/scratchpad/projects/agent-migrate/design.md §7 Phase 0):
// extracting resolveDerivedConfig out of populateAgentConfig must be a pure
// refactor. This test drives populateAgentConfig — the same entry point the
// agent-create path calls — with a fixed set of inputs that exercise every
// kind of template/harness-config-derived field a `scion reincarnate`
// re-resolution will later replay against a fresh catalog: template ID/hash/
// hub-access-scopes, template-merged image/model/env defaults, harness-config
// ID/hash resolution, pre-start-hook stamping, and model-alias resolution.
// GitClone population (project/workspace-derived, not template/harness-
// derived) is exercised too, since it stays in populateAgentConfig above the
// resolveDerivedConfig call and must be unaffected by the split.
//
// Skill injection is neutralized (no hub/user/project injected skills
// configured) so the fixture is not coupled to the separately-tested skill-
// merge precedence (see the "mergeInjectedSkills integration tests" in
// handlers_agent_create_helpers_test.go).
//
// The expected JSON below was captured from this exact code path, and this
// test also passes unchanged against the pre-extraction parent commit
// (73eb1c85f) — confirmed by copying it there and running it, since
// populateAgentConfig existed as a single function at that commit. If it
// ever needs to change, the reason must be "the derivation logic changed on
// purpose" — never "the resolveDerivedConfig extraction changed something".
func TestResolveDerivedConfig_GoldenCreatePath(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	// The test hub seeds platform skills into "injected_skills" by default;
	// remove them so mergeInjectedSkills (called at the end of
	// resolveDerivedConfig) has nothing to inject and the fixture below does
	// not depend on that unrelated, separately-tested seed data.
	require.NoError(t, s.DeleteHubSetting(ctx, "injected_skills"))

	project := &store.Project{
		ID:        tid("golden-project"),
		Name:      "Golden Project",
		Slug:      "golden-project",
		OwnerID:   "dev@localhost",
		GitRemote: "https://example.com/golden/repo.git",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	hubHook, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Scope:  store.PreStartHookScopeHub,
		Name:   "Golden hub hook",
		Slug:   "golden-hub-hook",
		Script: "#!/bin/sh\necho golden\n",
	})
	require.NoError(t, err)

	harnessConfig := &store.HarnessConfig{
		ID:          tid("golden-harness-config"),
		Name:        "Golden Claude Web",
		Slug:        "claude-web",
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "hc-golden-hash-v1",
		Config: &store.HarnessConfigData{
			ModelAliases: map[string]string{
				"extra-large": "concrete-model-x",
			},
		},
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, harnessConfig))

	template := &store.Template{
		ID:                   tid("golden-template"),
		Name:                 "Golden Template",
		Slug:                 "golden-template",
		Harness:              "claude",
		DefaultHarnessConfig: "claude-web",
		Scope:                store.TemplateScopeGlobal,
		Status:               store.TemplateStatusActive,
		ContentHash:          "tmpl-golden-hash-v1",
		Config: &store.TemplateConfig{
			Image: "golden-template-image:v1",
			Model: "extra-large",
			Env: map[string]string{
				"TEMPLATE_ENV_KEY": "template-value",
			},
			HubAccess: &store.HubAccessConfig{
				Scopes: []string{"golden-scope"},
			},
		},
	}
	require.NoError(t, s.CreateTemplate(ctx, template))

	agent := &store.Agent{
		ID:        tid("golden-agent"),
		ProjectID: project.ID,
		OwnerID:   "dev@localhost",
		AppliedConfig: &store.AgentAppliedConfig{
			Workspace:   "/tmp/golden-workspace",
			CreatorName: "dev@localhost",
			AgentRole:   "baseline",
		},
	}

	srv.populateAgentConfig(ctx, agent, project, template)

	// AgentAppliedConfig.MarshalJSON omits Env unless the value came from
	// ResponseView(true); the golden pins Env, so compare that view (it only
	// differs from the stored config by dropping GITHUB_TOKEN, absent here).
	got, err := json.Marshal(agent.AppliedConfig.ResponseView(true))
	require.NoError(t, err)

	want := fmt.Sprintf(`{
  "image": "golden-template-image:v1",
  "env": {
    "TEMPLATE_ENV_KEY": "template-value"
  },
  "model": "concrete-model-x",
  "workspace": "/tmp/golden-workspace",
  "gitClone": {
    "url": "https://example.com/golden/repo.git",
    "branch": "main",
    "depth": 1
  },
  "templateId": %q,
  "templateHash": "tmpl-golden-hash-v1",
  "harnessConfigId": %q,
  "harnessConfigHash": "hc-golden-hash-v1",
  "creatorName": "dev@localhost",
  "hubAccessScopes": ["golden-scope"],
  "agentRole": "baseline",
  "inlineConfig": {
    "detached": null
  },
  "projectPreStartHookId": %q,
  "projectPreStartHookScript": "#!/bin/sh\necho golden\n"
}`, template.ID, harnessConfig.ID, hubHook.ID)

	require.JSONEq(t, want, string(got),
		"resolveDerivedConfig must derive the same AgentAppliedConfig as before the extraction from populateAgentConfig")
}
