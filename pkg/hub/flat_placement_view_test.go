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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// P1.3 part 1 (ptone/scion#3269): the agent API exposes the stored placement.

// TestAgentAPI_PinnedRuntimeTargetView: the agent detail and list responses
// carry the read-only pinnedRuntimeTarget view ({id, type, runtimeBrokerId},
// the contract's name and shape) from stored data; an unpinned agent has
// none; a stale pin is reported as stored; and a PATCH cannot set it.
func TestAgentAPI_PinnedRuntimeTargetView(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	pinned := f.pinnedAgent(t, "view-pinned", string(state.PhaseStopped))
	legacy := f.unpinnedAgentOn(t, "view-legacy", f.legacy.ID, string(state.PhaseStopped))
	stale := f.stalePinnedAgent(t, "view-stale", string(state.PhaseStopped))

	view := func(t *testing.T, body []byte) (map[string]interface{}, bool) {
		t.Helper()
		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &m))
		if inner, ok := m["agent"].(map[string]interface{}); ok {
			m = inner
		}
		v, ok := m["pinnedRuntimeTarget"].(map[string]interface{})
		return v, ok
	}

	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/agents/"+pinned.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	v, ok := view(t, rec.Body.Bytes())
	require.True(t, ok, "a pinned agent carries pinnedRuntimeTarget: %s", rec.Body.String())
	assert.Equal(t, map[string]interface{}{
		"id": f.flat.RuntimeTarget.ID, "type": "docker", "runtimeBrokerId": f.flat.ID,
	}, v, "exactly the contract's keys, from stored data")

	rec = doRequest(t, f.srv, http.MethodGet, "/api/v1/agents/"+legacy.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	_, ok = view(t, rec.Body.Bytes())
	assert.False(t, ok, "an unpinned agent has no pinnedRuntimeTarget")

	rec = doRequest(t, f.srv, http.MethodGet, "/api/v1/agents/"+stale.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	v, ok = view(t, rec.Body.Bytes())
	require.True(t, ok)
	assert.Equal(t, f.flat.ID, v["runtimeBrokerId"], "a stale pin is reported as stored")
	var staleBody map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &staleBody))
	if inner, ok := staleBody["agent"].(map[string]interface{}); ok {
		staleBody = inner
	}
	assert.Equal(t, f.legacy.ID, staleBody["runtimeBrokerId"], "so a client can tell it from the current Runtime Broker")

	// The project agent list carries the same view.
	rec = doRequest(t, f.srv, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var list struct {
		Agents []map[string]interface{} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	found := false
	for _, item := range list.Agents {
		if item["id"] == pinned.ID {
			found = true
			pv, ok := item["pinnedRuntimeTarget"].(map[string]interface{})
			require.True(t, ok, "the list item carries pinnedRuntimeTarget")
			assert.Equal(t, f.flat.RuntimeTarget.ID, pv["id"])
		}
	}
	assert.True(t, found, "pinned agent listed")

	// The view is read-only: a PATCH naming it changes nothing.
	_ = doRequest(t, f.srv, http.MethodPatch, "/api/v1/agents/"+legacy.ID, map[string]interface{}{
		"pinnedRuntimeTarget": map[string]interface{}{"id": f.flat.RuntimeTarget.ID, "type": "docker", "runtimeBrokerId": f.flat.ID},
	})
	after, err := f.s.GetAgent(ctx, legacy.ID)
	require.NoError(t, err)
	assert.False(t, after.IsPinned(), "pinnedRuntimeTarget cannot be written through the API")
	assert.Equal(t, f.legacy.ID, after.RuntimeBrokerID)
	_ = store.PinnedPlacement{}
}
