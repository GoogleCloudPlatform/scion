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
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentBadges_ModelSizeCapturedBeforeAliasResolution pins that create
// records the requested size alias (each tier and each short form,
// normalized) while Model is still resolved to the concrete model ID, and
// stamps the resolved harness.
func TestAgentBadges_ModelSizeCapturedBeforeAliasResolution(t *testing.T) {
	aliases := harness.DefaultModelAliases("claude")
	require.NotEmpty(t, aliases)
	cases := []struct{ model, size string }{
		{"small", "small"},
		{"medium", "medium"},
		{"large", "large"},
		{"extra-large", "extra-large"},
		{"s", "small"},
		{"m", "medium"},
		{"l", "large"},
		{"xl", "extra-large"},
		{"XL", "extra-large"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			f := newTiersFixture(t, tiersCapable)
			agent := f.newAgent(t, t.Context(), "size-"+tc.model, &api.ScionConfig{Harness: "claude", Model: tc.model})
			assert.Equal(t, tc.size, agent.AppliedConfig.ModelSize)
			assert.Equal(t, aliases[tc.size], agent.AppliedConfig.Model, "Model is still resolved to the concrete ID")
			assert.Equal(t, "claude", agent.AppliedConfig.Harness, "the resolved harness is stamped")
		})
	}
}

// TestAgentBadges_NoSizeForExplicitIDOrNoModel pins that an explicit model
// ID, or no model at all, records no size.
func TestAgentBadges_NoSizeForExplicitIDOrNoModel(t *testing.T) {
	f := newTiersFixture(t, tiersCapable)
	agent := f.newAgent(t, t.Context(), "size-explicit", &api.ScionConfig{Harness: "claude", Model: "claude-opus-4-8"})
	assert.Empty(t, agent.AppliedConfig.ModelSize)
	assert.Equal(t, "claude-opus-4-8", agent.AppliedConfig.Model)

	agent = f.newAgent(t, t.Context(), "size-none", &api.ScionConfig{Harness: "claude"})
	assert.Empty(t, agent.AppliedConfig.ModelSize)
	assert.Empty(t, agent.AppliedConfig.Model)

	agent = f.newAgent(t, t.Context(), "size-nil-config", nil)
	assert.Empty(t, agent.AppliedConfig.ModelSize)
	assert.Empty(t, agent.AppliedConfig.Harness, "a harness that does not resolve is not stamped")
}

// TestAgentBadges_ModelSizeFollowsModelPrecedence pins that the size comes
// from whichever layer (request > project > template > hub) supplied the
// model, and that a higher layer's explicit ID leaves no size even when a
// lower layer names an alias.
func TestAgentBadges_ModelSizeFollowsModelPrecedence(t *testing.T) {
	cases := []struct {
		name, request, project, snapshot, hub, want string
	}{
		{name: "request alias", request: "s", project: "large", snapshot: "large", hub: "large", want: "small"},
		{name: "project alias", project: "m", snapshot: "large", hub: "large", want: "medium"},
		{name: "template alias", snapshot: "xl", hub: "small", want: "extra-large"},
		{name: "hub alias", hub: "l", want: "large"},
		{name: "request ID beats project alias", request: "claude-opus-4-8", project: "large", want: ""},
		{name: "project ID beats template alias", project: "claude-opus-4-8", snapshot: "large", want: ""},
		{name: "template ID beats hub alias", snapshot: "claude-opus-4-8", hub: "large", want: ""},
		{name: "project alias beats template ID", project: "small", snapshot: "claude-opus-4-8", want: "small"},
		{name: "no layer sets a model", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTiersFixture(t, tiersCapable)
			setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultModel: tc.hub})
			f.project.Annotations = map[string]string{}
			if tc.project != "" {
				f.project.Annotations[projectSettingDefaultModel] = tc.project
			}
			f.template.AgentConfig = &api.ScionConfig{Model: tc.snapshot}
			reqCfg := &api.ScionConfig{Harness: "claude", Model: tc.request}
			agent := f.newAgent(t, t.Context(), "size-order", reqCfg)
			assert.Equal(t, tc.want, agent.AppliedConfig.ModelSize)
		})
	}
}

// TestAgentBadges_PatchModelUpdatesSize pins that a PATCH of the model
// records the new size alias, and that an explicit ID clears it.
func TestAgentBadges_PatchModelUpdatesSize(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Model = "claude-opus-5-5"
		a.AppliedConfig.ModelSize = "large"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude"}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"model": "s"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "small", got.AppliedConfig.ModelSize)
	assert.Equal(t, harness.DefaultModelAliases("claude")["small"], got.AppliedConfig.Model)

	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"model": "claude-opus-4-8"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.AppliedConfig.ModelSize, "an explicit model ID clears the size")
}

// harnessLookupCountingStore counts harness-config reads.
type harnessLookupCountingStore struct {
	store.Store
	lookups atomic.Int64
}

func (c *harnessLookupCountingStore) GetHarnessConfigBySlug(ctx context.Context, slug, scope, scopeID string) (*store.HarnessConfig, error) {
	c.lookups.Add(1)
	return c.Store.GetHarnessConfigBySlug(ctx, slug, scope, scopeID)
}

func (c *harnessLookupCountingStore) GetHarnessConfig(ctx context.Context, id string) (*store.HarnessConfig, error) {
	c.lookups.Add(1)
	return c.Store.GetHarnessConfig(ctx, id)
}

// badgeFixture holds a project with one agent of each badge shape, and a
// second project whose older agents share the first project's global
// harness-config reference.
type badgeFixture struct {
	srv     *Server
	counter *harnessLookupCountingStore
	project *store.Project
	// want maps agent ID to its expected harnessVendor and modelSize.
	want map[string][2]string
	// inProject is the set of agent IDs in project (the rest are in the
	// second project).
	inProject map[string]bool
}

func newBadgeFixture(t *testing.T) *badgeFixture {
	t.Helper()
	srv, s, counter, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *harnessLookupCountingStore {
		return &harnessLookupCountingStore{Store: inner}
	})
	ctx := context.Background()
	project := &store.Project{ID: tid("badges-project"), Name: "Badges", Slug: "badges-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	other := &store.Project{ID: tid("badges-project-2"), Name: "Badges 2", Slug: "badges-project-2"}
	require.NoError(t, s.CreateProject(ctx, other))
	// An opaque slug, so the harness is known only from the record.
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("badges-hc"), Name: "team-default", Slug: "team-default", Harness: "gemini-cli",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{Harness: "gemini-cli"},
	}))

	f := &badgeFixture{srv: srv, project: project, want: map[string][2]string{}, inProject: map[string]bool{}}
	addIn := func(p *store.Project, id string, ac *store.AgentAppliedConfig, vendor, size string) {
		a := &store.Agent{
			ID: tid(id), Slug: id, Name: id, ProjectID: p.ID,
			Phase: string(state.PhaseCreated), AppliedConfig: ac,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		f.want[a.ID] = [2]string{vendor, size}
		if p == project {
			f.inProject[a.ID] = true
		}
	}
	add := func(id string, ac *store.AgentAppliedConfig, vendor, size string) {
		addIn(project, id, ac, vendor, size)
	}
	add("badge-stamped", &store.AgentAppliedConfig{Harness: "codex", ModelSize: "medium"}, harness.VendorOpenAI, "medium")
	add("badge-inline", &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Harness: "opencode"}, ModelSize: "extra-large"}, "opencode", "extra-large")
	add("badge-custom", &store.AgentAppliedConfig{Harness: "my-harness"}, "", "")
	add("badge-none", &store.AgentAppliedConfig{}, "", "")
	for _, id := range []string{"badge-legacy-1", "badge-legacy-2", "badge-legacy-3", "badge-legacy-4"} {
		add(id, &store.AgentAppliedConfig{HarnessConfig: "team-default"}, harness.VendorGoogle, "")
	}
	for _, id := range []string{"badge-other-legacy-1", "badge-other-legacy-2"} {
		addIn(other, id, &store.AgentAppliedConfig{HarnessConfig: "team-default"}, harness.VendorGoogle, "")
	}

	f.counter = counter
	return f
}

// checkItems asserts the badge keys of every fixture agent in a decoded
// list body (all of them, or only project's when projectOnly): present with
// the expected value, or absent when empty.
func (f *badgeFixture) checkItems(t *testing.T, label string, body []byte, projectOnly bool) {
	t.Helper()
	seen := 0
	for _, item := range decodeItems(t, decodeObject(t, body)["agents"]) {
		var id string
		require.NoError(t, json.Unmarshal(item["id"], &id))
		want, ok := f.want[id]
		if !ok {
			continue
		}
		seen++
		for i, key := range []string{"harnessVendor", "modelSize"} {
			raw, present := item[key]
			if want[i] == "" {
				assert.False(t, present, "%s %s: %s must be omitted, got %s", label, id, key, raw)
				continue
			}
			assert.JSONEq(t, `"`+want[i]+`"`, string(raw), "%s %s: %s", label, id, key)
		}
	}
	wantSeen := len(f.want)
	if projectOnly {
		wantSeen = len(f.inProject)
	}
	assert.Equal(t, wantSeen, seen, "%s: every fixture agent listed", label)
}

// TestAgentBadges_ListViewsCarryBadgesWithBoundedLookups pins that every
// agent list path (global and project, legacy and sorted, full and compact)
// carries harnessVendor and modelSize, and that the harness of agents with
// no stamped harness is resolved with per-request memoized lookups: one
// project-scope read per (project, reference) and one global-scope read per
// reference, not one per agent.
func TestAgentBadges_ListViewsCarryBadgesWithBoundedLookups(t *testing.T) {
	f := newBadgeFixture(t)
	bases := map[string]string{
		"global":  "/api/v1/agents",
		"project": "/api/v1/projects/" + f.project.ID + "/agents",
	}
	// "team-default" is global. The project list: one project-scope miss
	// and one global hit, shared by its four older agents. The global
	// list: a project-scope miss in each of the two projects, and one
	// global hit shared by all six older agents.
	wantLookups := map[string]int64{"global": 3, "project": 2}
	for epName, base := range bases {
		for _, mode := range []string{"", "sort=updated&fit=500", "sort=updated&dir=desc&limit=50"} {
			for _, view := range []string{"", "view=compact"} {
				label := epName + " [" + mode + "] [" + view + "]"
				f.counter.lookups.Store(0)
				rec := doRequest(t, f.srv, http.MethodGet, withQuery(base, mode, view), nil)
				require.Equal(t, http.StatusOK, rec.Code, "%s: %s", label, rec.Body.String())
				f.checkItems(t, label, rec.Body.Bytes(), epName == "project")
				assert.Equal(t, wantLookups[epName], f.counter.lookups.Load(), "%s: harness-config lookups", label)
			}
		}
	}
}

// TestAgentBadges_SingleAgentGet pins the badges on the single-agent GET,
// for a stamped agent and for a legacy agent without a stamp.
func TestAgentBadges_SingleAgentGet(t *testing.T) {
	f := newBadgeFixture(t)
	for id, want := range f.want {
		rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/agents/"+id, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got AgentWithCapabilities
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, want[0], got.HarnessVendor, "%s harnessVendor", id)
		assert.Equal(t, want[1], got.ModelSize, "%s modelSize", id)
	}
}

// TestAgentBadges_AgentJWTListCarriesBadges pins the badges on the project
// agent list read with an agent token (listProjectAgentsSortedAgentJWT),
// full and compact.
func TestAgentBadges_AgentJWTListCarriesBadges(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()
	require.NoError(t, f.store.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("badges-jwt-hc"), Name: "team-default", Slug: "team-default", Harness: "gemini-cli",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{Harness: "gemini-cli"},
	}))
	self := f.createAgent(t, "badge-jwt-self", string(state.PhaseRunning), nil)
	want := map[string][2]string{self.ID: {"", ""}}
	for slug, c := range map[string]struct {
		ac   *store.AgentAppliedConfig
		want [2]string
	}{
		"badge-jwt-stamped": {&store.AgentAppliedConfig{Harness: "claude", ModelSize: "large"}, [2]string{harness.VendorAnthropic, "large"}},
		"badge-jwt-legacy":  {&store.AgentAppliedConfig{HarnessConfig: "team-default"}, [2]string{harness.VendorGoogle, ""}},
	} {
		a := &store.Agent{
			ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.project.ID,
			Phase: string(state.PhaseRunning), CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
			AppliedConfig: c.ac,
		}
		require.NoError(t, f.store.CreateAgent(ctx, a))
		want[a.ID] = c.want
	}
	tok := f.agentJWTFor(t, self.ID)

	for _, query := range []string{"sort=updated&fit=500", "sort=updated&fit=500&view=compact"} {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(query), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", query, rec.Body.String())
		seen := 0
		for _, item := range decodeItems(t, decodeObject(t, rec.Body.Bytes())["agents"]) {
			var id string
			require.NoError(t, json.Unmarshal(item["id"], &id))
			w, ok := want[id]
			if !ok {
				continue
			}
			seen++
			for i, key := range []string{"harnessVendor", "modelSize"} {
				raw, present := item[key]
				if w[i] == "" {
					assert.False(t, present, "%s %s: %s must be omitted, got %s", query, id, key, raw)
					continue
				}
				assert.JSONEq(t, `"`+w[i]+`"`, string(raw), "%s %s: %s", query, id, key)
			}
		}
		assert.Equal(t, len(want), seen, "%s: every agent listed", query)
	}
}

// TestAgentBadges_ReincarnateRederivesHarnessAndSize pins that a
// reincarnation's fresh config re-stamps the harness and re-captures the
// size from the replayed create inputs and the current layers, rather than
// keeping the outgoing generation's values.
func TestAgentBadges_ReincarnateRederivesHarnessAndSize(t *testing.T) {
	cases := []struct {
		name           string
		inline         *api.ScionConfig
		projectDefault string
		wantSize       string
	}{
		{name: "create-time alias replayed", inline: &api.ScionConfig{Harness: "claude", Model: "l"}, wantSize: "large"},
		{name: "size follows the current project layer", inline: &api.ScionConfig{Harness: "claude"}, projectDefault: "m", wantSize: "medium"},
		{name: "explicit ID in the current project layer clears it", inline: &api.ScionConfig{Harness: "claude"}, projectDefault: "claude-opus-4-8", wantSize: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()
			if tc.projectDefault != "" {
				project.Annotations = map[string]string{projectSettingDefaultModel: tc.projectDefault}
				require.NoError(t, s.UpdateProject(ctx, project))
			}
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				// The outgoing generation's values, which must not carry over.
				a.AppliedConfig.Harness = "stale-harness"
				a.AppliedConfig.ModelSize = "extra-large"
				a.AppliedConfig.Model = "stale-model"
				live, explicit := *tc.inline, *tc.inline
				a.AppliedConfig.InlineConfig = &live
				a.AppliedConfig.CreateInputs.InlineConfig = &explicit
			})

			fresh, _, err := srv.buildFreshAppliedConfig(ctx, agent, project, "")
			require.NoError(t, err)
			assert.Equal(t, "claude", fresh.Harness, "the harness is re-stamped")
			assert.Equal(t, tc.wantSize, fresh.ModelSize, "the size is re-captured")
		})
	}
}

// TestAgentBadges_StampPrecedence pins where create's harness stamp comes
// from: an inline harness first, then the harness of the harness-config
// create resolved (whichever layer named it: request, project, template
// default), then the live resolve; a harness that does not resolve is not
// stamped.
func TestAgentBadges_StampPrecedence(t *testing.T) {
	cases := []struct {
		name            string
		req             *api.ScionConfig
		projectHC       string
		templateDefault string
		want            string
	}{
		{name: "template default only", templateDefault: "team-x", want: "gemini-cli"},
		{name: "request names the config", req: &api.ScionConfig{HarnessConfig: "team-x"}, want: "gemini-cli"},
		{name: "project annotation names the config", projectHC: "team-x", want: "gemini-cli"},
		{name: "inline harness beats a resolved config of another harness", req: &api.ScionConfig{Harness: "claude", HarnessConfig: "team-x"}, want: "claude"},
		{name: "unresolvable config leaves no stamp", req: &api.ScionConfig{HarnessConfig: "no-such-config"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTiersFixture(t, tiersCapable)
			ctx := t.Context()
			require.NoError(t, f.s.CreateHarnessConfig(ctx, &store.HarnessConfig{
				ID: tid("stamp-team-x"), Name: "team-x", Slug: "team-x", Harness: "gemini-cli",
				Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
				Config: &store.HarnessConfigData{Harness: "gemini-cli"},
			}))
			f.project.Annotations = map[string]string{}
			if tc.projectHC != "" {
				f.project.Annotations[projectSettingDefaultHarnessConfig] = tc.projectHC
			}
			f.template.DefaultHarnessConfig = tc.templateDefault
			agent := f.newAgent(t, ctx, "stamp-order", tc.req)
			assert.Equal(t, tc.want, agent.AppliedConfig.Harness)
			if tc.want == "gemini-cli" {
				assert.Equal(t, tid("stamp-team-x"), agent.AppliedConfig.HarnessConfigID, "the stamp comes from the config create resolved")
			}
		})
	}
}
