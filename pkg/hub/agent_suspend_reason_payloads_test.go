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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createSuspendedAgent stores an agent suspended with reason and a retained
// completed activity in projectID.
func createSuspendedAgent(t *testing.T, s store.Store, projectID, name string, reason state.SuspendReason) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID: tid(name), ProjectID: projectID, Name: name, Slug: name,
		Phase: string(state.PhaseSuspended), Activity: string(state.ActivityCompleted),
		SuspendReason: string(reason), OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, string(reason), got.SuspendReason, "fixture: the store keeps the reason")
	return got
}

// statusEventSuspendReason publishes agent's status event and returns its
// raw suspendReason value, failing when the key is missing.
func statusEventSuspendReason(t *testing.T, agent *store.Agent) string {
	t.Helper()
	pub := NewChannelEventPublisher()
	defer pub.Close()
	ch, unsub := pub.Subscribe("agent." + agent.ID + ".status")
	defer unsub()
	pub.PublishAgentStatus(context.Background(), agent)
	evt := receiveBadgeEvent(t, ch)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(evt.Data, &m))
	raw, ok := m["suspendReason"]
	require.True(t, ok, "status event must always carry suspendReason: %s", evt.Data)
	return string(raw)
}

// TestAgentStatusEvent_SuspendReasonClearedOnResume pins that the SSE status
// event carries the suspend reason of a suspended agent, and, once the
// agent resumes (the store clears the reason), an explicit empty reason, so
// the web's delta merge drops the reason it merged earlier.
func TestAgentStatusEvent_SuspendReasonClearedOnResume(t *testing.T) {
	_, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "suspend-reason-events")
	agent := createSuspendedAgent(t, s, proj.ID, "sr-event", state.SuspendReasonIdle)

	assert.Equal(t, `"idle"`, statusEventSuspendReason(t, agent))

	require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID,
		store.AgentStatusUpdate{Phase: string(state.PhaseRunning), Activity: string(state.ActivityWorking)}))
	resumed, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Empty(t, resumed.SuspendReason, "fixture: resume clears the reason")

	assert.Equal(t, `""`, statusEventSuspendReason(t, resumed))
}

// TestAgentStatusEvent_SuspendReasonChangeIsVisible pins that converting a
// hub suspension to a user suspension (only the reason changes) counts as a
// visible status change.
func TestAgentStatusEvent_SuspendReasonChangeIsVisible(t *testing.T) {
	before := &store.Agent{ID: "a1", ProjectID: "p1", Phase: "suspended", SuspendReason: "idle"}
	after := *before
	after.SuspendReason = "user"
	assert.True(t, agentStatusVisiblyChanged(before, &after))
}

// TestSpaceMembers_CarrySuspendReason pins suspendReason on chat space
// member agents: present for a suspended agent with a reason, omitted for a
// running one.
func TestSpaceMembers_CarrySuspendReason(t *testing.T) {
	srv, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "suspend-reason-members")
	suspended := createSuspendedAgent(t, s, proj.ID, "sr-member-suspended", state.SuspendReasonStalled)
	running := &store.Agent{ID: tid("sr-member-running"), ProjectID: proj.ID, Name: "sr-member-running",
		Slug: "sr-member-running", Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateAgent(context.Background(), running))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	seen := 0
	for _, a := range resp.Agents {
		switch string(a["id"]) {
		case `"` + suspended.ID + `"`:
			assert.JSONEq(t, `"stalled"`, string(a["suspendReason"]))
			seen++
		case `"` + running.ID + `"`:
			_, present := a["suspendReason"]
			assert.False(t, present, "suspendReason is omitted when empty")
			seen++
		}
	}
	assert.Equal(t, 2, seen, "both agents listed")
}
