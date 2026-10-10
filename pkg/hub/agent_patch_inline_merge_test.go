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
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3901: a config PATCH on an agent keeps every
// InlineConfig key it does not mention, and changes exactly the keys it does.

// createdInlineConfig is a --config-shaped InlineConfig carrying the fields
// the configure page never renders, plus a thinking level and max turns.
// The claude harness is named so that PATCH validation accepts max_turns
// (the generic harness does not support it).
func createdInlineConfig() *api.ScionConfig {
	tl := 40
	return &api.ScionConfig{
		Harness:       "claude",
		Volumes:       []api.VolumeMount{{Source: "/host/data", Target: "/data"}},
		Skills:        []api.SkillReference{{URI: "https://example.com/skills/review"}},
		MCPServers:    map[string]api.MCPServerConfig{"docs": {Transport: "stdio", Command: "docs-mcp"}},
		Services:      []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}},
		CommandArgs:   []string{"--verbose"},
		Kubernetes:    &api.KubernetesConfig{Namespace: "agents"},
		ThinkingLevel: &tl,
		MaxTurns:      3,
	}
}

func newInlineMergeTestAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker) *store.Agent {
	t.Helper()
	return newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		tl := 40
		a.AppliedConfig.ThinkingLevel = &tl
		a.AppliedConfig.InlineConfig = createdInlineConfig()
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			ThinkingLevel: &tl,
			InlineConfig:  createdInlineConfig(),
		}
	})
}

func getAgentViaAPI(t *testing.T, srv *Server, agentID string) store.Agent {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got store.Agent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.InlineConfig)
	return got
}

// TestApplyAgentUpdate_PatchMaxTurnsKeepsUnmentionedInlineConfig covers
// acceptance criteria 1 and 2: a PATCH of max_turns alone keeps volumes,
// skills, MCP servers, services, command args, kubernetes and the thinking
// level, live and as shown by GET.
func TestApplyAgentUpdate_PatchMaxTurnsKeepsUnmentionedInlineConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	agent := newInlineMergeTestAgent(t, s, project, broker)

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"max_turns": 5})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := getAgentViaAPI(t, srv, agent.ID)
	want := createdInlineConfig()
	want.MaxTurns = 5
	inline := got.AppliedConfig.InlineConfig
	assert.Equal(t, want.Volumes, inline.Volumes)
	assert.Equal(t, want.Skills, inline.Skills)
	assert.Equal(t, want.MCPServers, inline.MCPServers)
	assert.Equal(t, want.Services, inline.Services)
	assert.Equal(t, want.CommandArgs, inline.CommandArgs)
	assert.Equal(t, want.Kubernetes, inline.Kubernetes)
	assert.Equal(t, 5, inline.MaxTurns)
	require.NotNil(t, inline.ThinkingLevel, "InlineConfig thinking level must survive")
	assert.Equal(t, 40, *inline.ThinkingLevel)
	require.NotNil(t, got.AppliedConfig.ThinkingLevel, "live thinking level must survive a PATCH that does not name it")
	assert.Equal(t, 40, *got.AppliedConfig.ThinkingLevel)

	// CreateInputs records the max_turns edit and nothing else.
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	require.NotNil(t, ci.ThinkingLevel, "an absent thinking_level must not clear CreateInputs")
	assert.Equal(t, 40, *ci.ThinkingLevel)
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, 5, ci.InlineConfig.MaxTurns)
	assert.Equal(t, want.Volumes, ci.InlineConfig.Volumes)
	require.NotNil(t, ci.InlineConfig.ThinkingLevel)
	assert.Equal(t, 40, *ci.InlineConfig.ThinkingLevel)
}

// TestApplyAgentUpdate_PatchExplicitKeyChangesOnlyThatKey covers acceptance
// criterion 3: each explicit key replaces its own value (an explicit empty
// value or null included) and leaves every other key as it was.
func TestApplyAgentUpdate_PatchExplicitKeyChangesOnlyThatKey(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newInlineMergeTestAgent(t, s, project, broker)
	base := createdInlineConfig()

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"volumes": []map[string]interface{}{{"source": "/host/other", "target": "/other"}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	inline := getAgentViaAPI(t, srv, agent.ID).AppliedConfig.InlineConfig
	assert.Equal(t, []api.VolumeMount{{Source: "/host/other", Target: "/other"}}, inline.Volumes)
	assert.Equal(t, base.Skills, inline.Skills)
	assert.Equal(t, base.MCPServers, inline.MCPServers)
	assert.Equal(t, base.MaxTurns, inline.MaxTurns)

	// An explicit empty list clears only skills.
	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"skills": []interface{}{}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	inline = getAgentViaAPI(t, srv, agent.ID).AppliedConfig.InlineConfig
	assert.Empty(t, inline.Skills)
	assert.Equal(t, []api.VolumeMount{{Source: "/host/other", Target: "/other"}}, inline.Volumes)
	assert.Equal(t, base.Services, inline.Services)

	// An explicit thinking level changes it live and inline; an explicit
	// null then unsets it.
	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"thinking_level": 70})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := getAgentViaAPI(t, srv, agent.ID)
	require.NotNil(t, got.AppliedConfig.ThinkingLevel)
	assert.Equal(t, 70, *got.AppliedConfig.ThinkingLevel)
	require.NotNil(t, got.AppliedConfig.InlineConfig.ThinkingLevel)
	assert.Equal(t, 70, *got.AppliedConfig.InlineConfig.ThinkingLevel)
	assert.Equal(t, base.CommandArgs, got.AppliedConfig.InlineConfig.CommandArgs)

	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"thinking_level": nil})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = getAgentViaAPI(t, srv, agent.ID)
	assert.Nil(t, got.AppliedConfig.ThinkingLevel)
	assert.Nil(t, got.AppliedConfig.InlineConfig.ThinkingLevel)
	assert.Equal(t, base.Kubernetes, got.AppliedConfig.InlineConfig.Kubernetes)
}

// TestMergePresentInlineFields covers the helper directly: a nil old config,
// an empty presence set, a present nil value, and no aliasing of the old
// config's maps and slices.
func TestMergePresentInlineFields(t *testing.T) {
	t.Run("nil old config starts empty", func(t *testing.T) {
		got := mergePresentInlineFields(nil, &api.ScionConfig{MaxTurns: 7, SystemPrompt: "x"}, map[string]bool{"max_turns": true})
		require.NotNil(t, got)
		assert.Equal(t, 7, got.MaxTurns)
		assert.Empty(t, got.SystemPrompt, "an absent key is not copied from the request")
	})

	t.Run("nothing present keeps everything", func(t *testing.T) {
		old := createdInlineConfig()
		got := mergePresentInlineFields(old, &api.ScionConfig{}, nil)
		assert.Equal(t, old, got)
	})

	t.Run("a present key with a nil value clears it", func(t *testing.T) {
		old := createdInlineConfig()
		got := mergePresentInlineFields(old, &api.ScionConfig{MCPServers: nil}, map[string]bool{"mcp_servers": true})
		assert.Nil(t, got.MCPServers)
		assert.Equal(t, old.Volumes, got.Volumes)
	})

	t.Run("a non-canonical-case key is merged", func(t *testing.T) {
		// applyAgentUpdate lower-cases the raw keys, as encoding/json
		// matches field names case-insensitively.
		present := map[string]bool{strings.ToLower("Max_Turns"): true}
		got := mergePresentInlineFields(createdInlineConfig(), &api.ScionConfig{MaxTurns: 9}, present)
		assert.Equal(t, 9, got.MaxTurns)
	})

	t.Run("result does not alias the old config", func(t *testing.T) {
		old := createdInlineConfig()
		old.Env = map[string]string{"A": "1"}
		got := mergePresentInlineFields(old, &api.ScionConfig{}, map[string]bool{"max_turns": true})
		got.Env["A"] = "2"
		got.Volumes[0].Source = "/changed"
		assert.Equal(t, "1", old.Env["A"])
		assert.Equal(t, "/host/data", old.Volumes[0].Source)
	})
}

// TestApplyAgentUpdate_PatchMixedCaseKeyIsMerged sends a non-canonical-case
// key over HTTP: encoding/json decodes it, so it must also reach the merged
// InlineConfig.
func TestApplyAgentUpdate_PatchMixedCaseKeyIsMerged(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newInlineMergeTestAgent(t, s, project, broker)

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"Max_Turns": 9})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	inline := getAgentViaAPI(t, srv, agent.ID).AppliedConfig.InlineConfig
	assert.Equal(t, 9, inline.MaxTurns)
	assert.Equal(t, createdInlineConfig().Volumes, inline.Volumes)
}

// TestApplyAgentUpdate_HarnessSwitchIsRefused: the harness is fixed at
// creation (ptone/scion#3972), so a PATCH that would switch it is refused
// with 400, naming the key, and stores nothing, even together with keys
// that would be valid on their own. Echoing the current harness is ignored.
func TestApplyAgentUpdate_HarnessSwitchIsRefused(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	agent := newInlineMergeTestAgent(t, s, project, broker)

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"harness": "generic", "max_turns": 0})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "validation_error", body.Error.Code)
	fields, _ := body.Error.Details["fields"].(map[string]interface{})
	assert.Equal(t, []string{"config.harness"}, sortedFieldKeys(fields))

	stored, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "claude", stored.AppliedConfig.InlineConfig.Harness, "a refused PATCH must store nothing")
	assert.Equal(t, 3, stored.AppliedConfig.InlineConfig.MaxTurns, "a refused PATCH must store nothing")

	// The current harness, echoed, is not a change.
	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{"harness": "claude", "max_turns": 4})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	inline := getAgentViaAPI(t, srv, agent.ID).AppliedConfig.InlineConfig
	assert.Equal(t, "claude", inline.Harness)
	assert.Equal(t, 4, inline.MaxTurns)
}

// sortedFieldKeys returns m's keys, sorted.
func sortedFieldKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
