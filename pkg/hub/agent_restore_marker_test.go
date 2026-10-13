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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The created event carries restoredAt only for a restore (ptone/scion#2951):
// web clients clear a delete tombstone only on a marked created.

func nextCreatedEvent(t *testing.T, ch <-chan Event) AgentCreatedEvent {
	t.Helper()
	select {
	case evt := <-ch:
		var data AgentCreatedEvent
		require.NoError(t, json.Unmarshal(evt.Data, &data))
		return data
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent.created")
		return AgentCreatedEvent{}
	}
}

func TestRestoreAgent_PublishesRestoredMarker(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	softDeleteAgent(t, f)

	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	f.srv.events = bus
	agentCh, unsub1 := bus.Subscribe("agent." + f.target.ID + ".created")
	defer unsub1()
	projectCh, unsub2 := bus.Subscribe("project." + f.project.ID + ".agent.created")
	defer unsub2()

	before := time.Now().Add(-time.Second)
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, f.targetPath()+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, ch := range []<-chan Event{agentCh, projectCh} {
		data := nextCreatedEvent(t, ch)
		assert.Equal(t, f.target.ID, data.AgentID)
		require.NotEmpty(t, data.RestoredAt, "a restore's created carries restoredAt")
		at, err := time.Parse(time.RFC3339, data.RestoredAt)
		require.NoError(t, err)
		assert.False(t, at.Before(before.Truncate(time.Second)), "restoredAt is the restore time")
	}
}

func TestCreateAgent_CreatedHasNoRestoredMarker(t *testing.T) {
	srv, _, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	srv.events = bus
	ch, unsub := bus.Subscribe("project." + project.ID + ".agent.created")
	defer unsub()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "no-marker", ProjectID: project.ID, Task: "do it",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	data := nextCreatedEvent(t, ch)
	assert.Empty(t, data.RestoredAt)

	raw, err := json.Marshal(data)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "restoredAt", "omitted from the wire for a plain create")
}

func TestEventBuilder_PublishAgentRestored(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()
	ch, unsub := pub.Subscribe("agent.a1.created")
	defer unsub()

	at := time.Date(2026, 10, 5, 1, 2, 3, 0, time.FixedZone("x", 3600))
	pub.PublishAgentRestored(context.Background(), &store.Agent{ID: "a1", ProjectID: "g1", Name: "n", Slug: "n"}, at)
	data := nextCreatedEvent(t, ch)
	assert.Equal(t, "a1", data.AgentID)
	assert.Equal(t, "2026-10-05T00:02:03Z", data.RestoredAt)
}
