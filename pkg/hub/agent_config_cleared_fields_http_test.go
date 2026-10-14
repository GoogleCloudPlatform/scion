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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentConfigPatch_ClearsReachTheStart (ptone/scion#3906): set, start,
// then clear, start. The start after the clear sends the cleared paths in
// the inline config's cleared_fields, the next start sends them again, and
// a later PATCH that sets a field again takes it off the list. The PATCH
// dispositions and warnings are unchanged.
func TestAgentConfigPatch_ClearsReachTheStart(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Env: map[string]string{"FOO": "1", "BAR": "2"}}
	})
	start := func() dispatchedStart {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Less(t, rec.Code, 300, rec.Body.String())
		starts := disp.snapshot()
		require.NotEmpty(t, starts)
		// Back to stopped, so the next PATCH is accepted.
		row, err := s.GetAgent(t.Context(), agent.ID)
		require.NoError(t, err)
		row.Phase = string(state.PhaseStopped)
		require.NoError(t, s.UpdateAgent(t.Context(), row))
		return starts[len(starts)-1]
	}

	_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{
			"max_turns": 9, "thinking_level": 3, "system_prompt": "be terse",
			"resources": map[string]interface{}{"requests": map[string]string{"cpu": "2"}},
		},
	})
	require.Equal(t, http.StatusOK, code, body)
	first := start()
	assert.Equal(t, 9, first.inline.MaxTurns)
	assert.Empty(t, first.inline.ClearedFields)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{
			"max_turns": 0, "thinking_level": nil, "system_prompt": "", "resources": nil,
			"env": map[string]string{"BAR": "2"},
		},
	})
	require.Equal(t, http.StatusOK, code, body)
	// C-17 unchanged: the clears are still reported as held for the next
	// reincarnation, since an older broker keeps the previous value.
	assert.ElementsMatch(t, []string{"config.env", "config.max_turns", "config.resources", "config.system_prompt", "config.thinking_level"}, resp.Disposition.HeldForReincarnate)
	assert.NotEmpty(t, resp.Warnings)

	want := []string{"env.FOO", "max_turns", "resources", "system_prompt", "thinking_level"}
	for i := 0; i < 2; i++ { // the start after the clear, and the one after it
		got := start()
		assert.Equal(t, want, got.inline.ClearedFields, "start %d", i+1)
		assert.Zero(t, got.inline.MaxTurns)
		assert.Nil(t, got.inline.ThinkingLevel)
	}

	_, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_turns": 4, "env": map[string]string{"BAR": "2", "FOO": "back"}},
	})
	require.Equal(t, http.StatusOK, code, body)
	got := start()
	assert.Equal(t, []string{"resources", "system_prompt", "thinking_level"}, got.inline.ClearedFields)
	assert.Equal(t, 4, got.inline.MaxTurns)

	// A client-sent list that differs from the stored one is refused like
	// any other fixed key.
	_, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"cleared_fields": []string{"harness"}},
	})
	assert.Equal(t, http.StatusBadRequest, code, body)
}
