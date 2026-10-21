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

// badgeFixture holds a project with one agent of each badge shape.
type badgeFixture struct {
	srv     *Server
	counter *harnessLookupCountingStore
	project *store.Project
	// want maps agent ID to its expected harnessVendor and modelSize.
	want map[string][2]string
	// legacyIDs are agents with no stamped harness that name it only
	// through a harness-config reference.
	legacyIDs []string
}

func newBadgeFixture(t *testing.T) *badgeFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("badges-project"), Name: "Badges", Slug: "badges-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	// An opaque slug, so the harness is known only from the record.
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("badges-hc"), Name: "team-default", Slug: "team-default", Harness: "gemini-cli",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{Harness: "gemini-cli"},
	}))

	f := &badgeFixture{srv: srv, project: project, want: map[string][2]string{}}
	add := func(id string, ac *store.AgentAppliedConfig, vendor, size string) string {
		a := &store.Agent{
			ID: tid(id), Slug: id, Name: id, ProjectID: project.ID,
			Phase: string(state.PhaseCreated), AppliedConfig: ac,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		f.want[a.ID] = [2]string{vendor, size}
		return a.ID
	}
	add("badge-stamped", &store.AgentAppliedConfig{Harness: "codex", ModelSize: "medium"}, harness.VendorOpenAI, "medium")
	add("badge-inline", &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Harness: "opencode"}, ModelSize: "extra-large"}, "opencode", "extra-large")
	add("badge-custom", &store.AgentAppliedConfig{Harness: "my-harness"}, "", "")
	add("badge-none", &store.AgentAppliedConfig{}, "", "")
	for _, id := range []string{"badge-legacy-1", "badge-legacy-2", "badge-legacy-3", "badge-legacy-4"} {
		f.legacyIDs = append(f.legacyIDs, add(id, &store.AgentAppliedConfig{HarnessConfig: "team-default"}, harness.VendorGoogle, ""))
	}

	f.counter = &harnessLookupCountingStore{Store: s}
	srv.store = f.counter
	return f
}

// checkItems asserts every fixture agent's badge keys in a decoded list
// body: present with the expected value, or absent when empty.
func (f *badgeFixture) checkItems(t *testing.T, label string, body []byte) {
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
	assert.Equal(t, len(f.want), seen, "%s: every fixture agent listed", label)
}

// TestAgentBadges_ListViewsCarryBadgesWithBoundedLookups pins that every
// agent list path (global and project, legacy and sorted, full and compact)
// carries harnessVendor and modelSize, and that the harness of agents with
// no stamped harness is resolved with one harness-config lookup chain per
// distinct reference per request, not one per agent.
func TestAgentBadges_ListViewsCarryBadgesWithBoundedLookups(t *testing.T) {
	f := newBadgeFixture(t)
	bases := map[string]string{
		"global":  "/api/v1/agents",
		"project": "/api/v1/projects/" + f.project.ID + "/agents",
	}
	for epName, base := range bases {
		for _, mode := range []string{"", "sort=updated&fit=500", "sort=updated&dir=desc&limit=50"} {
			for _, view := range []string{"", "view=compact"} {
				label := epName + " [" + mode + "] [" + view + "]"
				f.counter.lookups.Store(0)
				rec := doRequest(t, f.srv, http.MethodGet, withQuery(base, mode, view), nil)
				require.Equal(t, http.StatusOK, rec.Code, "%s: %s", label, rec.Body.String())
				f.checkItems(t, label, rec.Body.Bytes())
				// "team-default" is global: one project-scope miss and one
				// global hit, shared by all four legacy agents.
				assert.Equal(t, int64(2), f.counter.lookups.Load(), "%s: harness-config lookups", label)
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
