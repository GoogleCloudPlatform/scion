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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deletion view's blocksStart (ptone/scion#3098) is computed only on the
// single-agent GET; every other surface omits it.

// seedFreeFailed is a failed delete with no outstanding intent: the view is
// shown but a start is allowed.
var seedFreeFailed = deleteSeed{name: "failed without intent", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}

// seedFreeInDoubt is an in_doubt delete whose broker intent has ended: the
// code alone does not block start.
var seedFreeInDoubt = deleteSeed{name: "in_doubt without intent", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt}

// freeSeeds are deletion views that are shown but do not block start.
var freeSeeds = []deleteSeed{seedFreeFailed, seedFreeInDoubt}

// getDeletionView calls the single-agent GET body builder as a member and
// returns the response's deletion object (nil for an explicit null).
func getDeletionView(t *testing.T, srv *Server, s store.Store, agentID string) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser("u-m", "m@test.com", "M", "member", "web")))
	rec := httptest.NewRecorder()
	srv.writeAgentGetResponse(rec, req, mustGetAgent(t, s, agentID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw, ok := decodeObject(t, rec.Body.Bytes())["deletion"]
	require.True(t, ok, "GET carries the deletion key: %s", rec.Body.String())
	return rawDeletionObject(t, raw)
}

func TestAgentGet_DeletionBlocksStart(t *testing.T) {
	cases := []struct {
		seed deleteSeed
		want string
	}{
		{seedLiveDeleting, "true"},
		{seedInDoubtIntent, "true"}, // held only by the outstanding broker intent
		{seedExpiredFinalize, "true"},
		{seedFreeFailed, "false"},
		{seedFreeInDoubt, "false"}, // the code alone is not a block
	}
	for i, tc := range cases {
		t.Run(tc.seed.name, func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "bs-get-"+string(rune('a'+i)), state.PhaseStopped)
			seedAgentDeletion(t, s, agent.ID, tc.seed)

			view := getDeletionView(t, srv, s, agent.ID)
			require.NotNil(t, view)
			require.Contains(t, view, "blocksStart")
			assert.JSONEq(t, tc.want, string(view["blocksStart"]))
		})
	}
}

// For every blocking and free marker, the GET's blocksStart is exactly
// whether the start gate refuses with delete_in_progress.
func TestAgentGet_DeletionBlocksStartMatchesStartGate(t *testing.T) {
	seeds := append(append([]deleteSeed{}, blockingSeeds...), freeSeeds...)
	for i, seed := range seeds {
		t.Run(seed.name, func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "bs-gate-"+string(rune('a'+i)), state.PhaseStopped)
			seedAgentDeletion(t, s, agent.ID, seed)

			view := getDeletionView(t, srv, s, agent.ID)
			require.NotNil(t, view)
			require.Contains(t, view, "blocksStart")
			var got bool
			require.NoError(t, json.Unmarshal(view["blocksStart"], &got))

			refusal := srv.startGate(context.Background(), mustGetAgent(t, s, agent.ID), startEntryStart)
			gateBlocks := refusal != nil && refusal.Code == ErrCodeDeleteInProgress
			assert.Equal(t, gateBlocks, got, "blocksStart matches the start gate")
		})
	}
}

// With no delete there is no deletion view to carry the field.
func TestAgentGet_NoDeletionNoBlocksStart(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "bs-none", state.PhaseStopped)
	assert.Nil(t, getDeletionView(t, srv, s, agent.ID))
}

// A failing check omits the field; the GET still answers 200.
func TestAgentGet_DeletionBlocksStartCheckErrorOmits(t *testing.T) {
	srv, s := testServer(t)
	// The wrapper fails every dispatch lookup; setup below writes through
	// the unwrapped store, so only the server's reads see the error.
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *dispatchErrStore {
		return &dispatchErrStore{Store: inner}
	})
	agent := setupBrokerAgentInPhase(t, s, "bs-err", state.PhaseStopped)
	// Not held by the row itself, so the check has to read the dispatch table.
	seedAgentDeletion(t, s, agent.ID, seedFreeFailed)

	view := getDeletionView(t, srv, s, agent.ID)
	require.NotNil(t, view)
	assert.JSONEq(t, `"failed"`, string(view["state"]))
	assert.NotContains(t, view, "blocksStart")
}

// List enrichment (every list builder and the compact view read it) omits
// the field even for a row that blocks start.
func TestAgentList_DeletionOmitsBlocksStart(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "bs-list", state.PhaseStopped)
	seedAgentDeletion(t, s, agent.ID, seedInDoubtIntent)

	items := []store.Agent{*mustGetAgent(t, s, agent.ID)}
	srv.enrichAgents(context.Background(), items)
	view := deletionJSON(t, items[0].Deletion)
	require.NotNil(t, view)
	assert.NotContains(t, view, "blocksStart")

	compact := toCompact(AgentWithCapabilities{Agent: items[0]})
	raw, err := json.Marshal(compact)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "blocksStart")
}

// SSE status events omit the field: the publisher has no store to answer
// the outstanding-intent term, and one payload serves both the agent and
// the project subjects.
func TestAgentStatusEvent_DeletionOmitsBlocksStart(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()
	ch, unsub := pub.Subscribe("agent.a1.status")
	defer unsub()

	lease := time.Now().Add(time.Minute)
	pub.PublishAgentStatus(context.Background(), &store.Agent{
		ID: "a1", ProjectID: "g1", Phase: "stopping",
		DeletionState: store.DeletionStateDeleting, DeletionLeaseAt: &lease, DeletionClaim: 2,
	})
	select {
	case evt := <-ch:
		var body struct {
			Deletion map[string]json.RawMessage `json:"deletion"`
		}
		require.NoError(t, json.Unmarshal(evt.Data, &body))
		require.NotNil(t, body.Deletion)
		assert.NotContains(t, body.Deletion, "blocksStart")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}
