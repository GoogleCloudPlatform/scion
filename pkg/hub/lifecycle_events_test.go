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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// lifecycleTestServer is a create-capable test server (fake dispatcher)
// with the lifecycle sink set. pinned selects a pinned hub ID.
func lifecycleTestServer(t *testing.T, pinned bool) (*Server, lifecycleEventStore, string, *telemetryemit.MemorySink, *telemetryemit.MemoryCounters) {
	t.Helper()
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	ls, ok := s.(lifecycleEventStore)
	require.True(t, ok, "the test store must support the lifecycle observer")
	srv.config.HubIDPinned = pinned
	// The done-when delete is a hard delete (design 11): no retention.
	srv.config.SoftDeleteRetention = 0
	if !pinned {
		srv.config.HubIDSource = lifecycleevents.HubIDSourceHostname
	}
	sink := telemetryemit.NewMemorySink()
	counters := telemetryemit.NewMemoryCounters()
	srv.SetLifecycleEventsSink(sink, counters)
	return srv, ls, project.ID, sink, counters
}

func lifecycleCreateAgent(t *testing.T, srv *Server, projectID, name string) string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: name, ProjectID: projectID, Task: "do it",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotEmpty(t, resp.Agent.ID)
	return resp.Agent.ID
}

func lifecycleDrain(t *testing.T, ls lifecycleEventStore, sink telemetryemit.Sink, now func() time.Time) {
	t.Helper()
	d, err := lifecycleevents.NewDrainer(lifecycleevents.DrainerConfig{Store: ls, Sink: sink, Now: now})
	require.NoError(t, err)
	_, err = d.DrainOnce(context.Background())
	require.NoError(t, err)
}

func lifecycleDecode(t *testing.T, payloads [][]byte) []*tc.LifecycleEvent {
	t.Helper()
	out := make([]*tc.LifecycleEvent, 0, len(payloads))
	for _, p := range payloads {
		var ev tc.LifecycleEvent
		require.NoError(t, tc.Unmarshal(p, &ev), string(p))
		out = append(out, &ev)
	}
	return out
}

func lifecycleSeq(evs []*tc.LifecycleEvent) string {
	parts := make([]string, 0, len(evs))
	for _, e := range evs {
		s := fmt.Sprintf("%s@%d", e.Transition, e.TransitionRevision)
		if e.Cause != "" {
			s += "(" + e.Cause + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// lostDeleteStore loses every delete after delivery, forcing a
// redelivery of the same rows.
type lostDeleteStore struct{ lifecycleEventStore }

func (lostDeleteStore) CompleteLifecycleOutboxRow(context.Context, int64, string) (bool, error) {
	return false, errors.New("injected: process stopped before the delete")
}

// TestLifecycleEvents_SliceOneDoneWhen is the slice 1 done-when flow
// (WP1 design 11) at the slice 2 head: an HTTP create through a fake
// dispatcher, then a delete through the delete engine, drained to a
// MemorySink. In slice 1 the dispatch-result and delete-claim writes were
// unobserved, which gave created(1), a gap, resynced(unobserved_change, 3),
// deleted(4); slice 2 observes them, so the history is dense from created
// to deleted with no resync. A forced redelivery sends byte-identical
// events.
func TestLifecycleEvents_SliceOneDoneWhen(t *testing.T) {
	srv, ls, projectID, sink, counters := lifecycleTestServer(t, true)
	state, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	require.Equal(t, telemetryemit.ObserverStateEnabled, state)
	require.True(t, ls.AgentWriteObserverInstalled())

	agentID := lifecycleCreateAgent(t, srv, projectID, "lc-done-when")
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, agentGone(t, srv.store, agentID), "the done-when delete is a hard delete")

	// First drain: delivered, but the deletes are lost (crash after the
	// sink acknowledged). Second drain, after the leases expire: the same
	// rows again.
	lifecycleDrain(t, lostDeleteStore{ls}, sink, time.Now)
	first := sink.Delivered()
	lifecycleDrain(t, ls, sink, func() time.Time { return time.Now().Add(time.Minute) })
	all := sink.Delivered()
	require.Len(t, all, 2*len(first))
	for i := range first {
		assert.True(t, bytes.Equal(first[i], all[len(first)+i]), "redelivered event %d is not byte-identical", i)
	}

	evs := lifecycleDecode(t, first)
	seq := lifecycleSeq(evs)
	require.True(t, strings.HasPrefix(seq, "created@1 ") && strings.HasSuffix(seq, fmt.Sprintf(" deleted@%d", len(evs))),
		"dense history from created to deleted: %s (counters: %s)", seq, counters)
	require.NotContains(t, seq, "resynced", "every write is observed in slice 2")
	created, deleted := evs[0], evs[len(evs)-1]
	for _, e := range evs {
		assert.Equal(t, agentID, e.AgentID)
		assert.Equal(t, projectID, e.ProjectID)
		assert.Equal(t, "scion-hub/test-hub-id", e.ProducerNamespace)
	}
	require.NotNil(t, created.Metadata)
	assert.NotEmpty(t, created.Metadata.CreatorPrincipalType, "the creator comes from the create's delegation edge")
	assert.NotEmpty(t, created.Metadata.CreatorPrincipalID)
	assert.Equal(t, tc.DeleteModeHard, deleted.DeleteMode)
	require.NotNil(t, deleted.Metadata, "deleted carries the last metadata")
	assert.Equal(t, created.Metadata.Name, deleted.Metadata.Name)

	st, err := ls.LifecycleOutboxStats(context.Background())
	require.NoError(t, err)
	assert.Zero(t, st.Pending, "delivered rows are removed")
}

// TestLifecycleEvents_ExperimentOff: with hub.lifecycle_events off (the
// default) the same flow writes no lifecycle rows, and startup starts no
// drainer (review r6 N4).
func TestLifecycleEvents_ExperimentOff(t *testing.T) {
	srv, ls, projectID, _, counters := lifecycleTestServer(t, true)
	agentID := lifecycleCreateAgent(t, srv, projectID, "lc-off")
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.False(t, ls.AgentWriteObserverInstalled())
	st, err := ls.LifecycleOutboxStats(context.Background())
	require.NoError(t, err)
	assert.Zero(t, st.Pending, "no outbox rows with the observer off")

	srv.startLifecycleEvents(context.Background())
	srv.lifecycleEvents.mu.Lock()
	running := srv.lifecycleEvents.drainer != nil
	stateNow := srv.lifecycleEvents.state
	srv.lifecycleEvents.mu.Unlock()
	assert.False(t, running, "no drainer without an observer or a backlog")
	assert.Equal(t, telemetryemit.ObserverStateDisabledExperimentOff, stateNow)
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricObserverState, telemetryemit.ObserverStateDisabledExperimentOff))
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricDrainerState, telemetryemit.DrainerStateIdleDisabled))
	assert.Zero(t, counters.Get(telemetryemit.MetricDrainerState, telemetryemit.DrainerStateRunning))
}

// TestLifecycleEvents_BacklogDrainerStartsAndExits: rows left in the
// outbox from a time the observer was installed are delivered at startup
// by a drainer that then exits (review r6 N4).
func TestLifecycleEvents_BacklogDrainerStartsAndExits(t *testing.T) {
	srv, ls, projectID, sink, counters := lifecycleTestServer(t, true)
	_, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	lifecycleCreateAgent(t, srv, projectID, "lc-backlog")
	ls.SetAgentWriteObserver(nil)

	done := srv.startLifecycleEvents(context.Background())
	require.NotNil(t, done, "a backlog starts the drainer")
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the backlog drainer did not exit once the outbox was empty")
	}
	evs := lifecycleDecode(t, sink.Delivered())
	assert.True(t, strings.HasPrefix(lifecycleSeq(evs), "created@1"), lifecycleSeq(evs))
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricDrainerState, telemetryemit.DrainerStateIdleDisabled),
		"the backlog-only drainer reports idle once it exits: %s", counters)
}

// TestLifecycleEvents_UnpinnedHubID: a derived hub ID (hostname) is not a
// stable producer namespace, so the observer is not installed and the
// refusal is reported (design 3.3, B4).
func TestLifecycleEvents_UnpinnedHubID(t *testing.T) {
	srv, ls, projectID, _, counters := lifecycleTestServer(t, false)
	state, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	assert.Equal(t, telemetryemit.ObserverStateDisabledUnpinnedHubID, state)
	assert.False(t, ls.AgentWriteObserverInstalled())
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricObserverState, telemetryemit.ObserverStateDisabledUnpinnedHubID))

	lifecycleCreateAgent(t, srv, projectID, "lc-unpinned")
	st, err := ls.LifecycleOutboxStats(context.Background())
	require.NoError(t, err)
	assert.Zero(t, st.Pending)
}

// TestLifecycleEvents_PinnedHubID: a pinned ID, or the persisted file on
// a single-process SQLite hub, installs the observer.
func TestLifecycleEvents_PinnedHubID(t *testing.T) {
	srv, ls, _, _, _ := lifecycleTestServer(t, true)
	state, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	assert.Equal(t, telemetryemit.ObserverStateEnabled, state)
	assert.True(t, ls.AgentWriteObserverInstalled())

	srv2, ls2, _, _, _ := lifecycleTestServer(t, false)
	srv2.config.HubIDSource = lifecycleevents.HubIDSourcePersistedFile
	state, err = srv2.installLifecycleObserver()
	require.NoError(t, err)
	assert.Equal(t, telemetryemit.ObserverStateEnabled, state, "the persisted file is stable on SQLite")
	assert.True(t, ls2.AgentWriteObserverInstalled())
}

// TestLifecycleEvents_NoSinkRefusesObserver: without a sink no drainer
// would deliver or trim the outbox, so the observer is not installed
// (wp1s1 review r1 R4).
func TestLifecycleEvents_NoSinkRefusesObserver(t *testing.T) {
	srv, ls, projectID, _, _ := lifecycleTestServer(t, true)
	counters := telemetryemit.NewMemoryCounters()
	srv.SetLifecycleEventsSink(nil, counters)
	state, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	assert.Equal(t, telemetryemit.ObserverStateDisabledNoSink, state)
	assert.False(t, ls.AgentWriteObserverInstalled())
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricObserverState, telemetryemit.ObserverStateDisabledNoSink))
	lifecycleCreateAgent(t, srv, projectID, "lc-no-sink")
	has, err := ls.LifecycleOutboxHasRows(context.Background())
	require.NoError(t, err)
	assert.False(t, has, "no rows without an observer")
}

// TestLifecycleEvents_BacklogDrainerClearsAndRestarts: once a backlog-only
// drainer exits, its state is cleared, so a later start (for example with
// the observer now installed) runs a new drainer (wp1s1 review r1 R4).
func TestLifecycleEvents_BacklogDrainerClearsAndRestarts(t *testing.T) {
	srv, ls, projectID, sink, counters := lifecycleTestServer(t, true)
	_, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	lifecycleCreateAgent(t, srv, projectID, "lc-restart-1")
	ls.SetAgentWriteObserver(nil)

	done := srv.startLifecycleEvents(context.Background())
	require.NotNil(t, done)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the backlog drainer did not exit")
	}
	require.Eventually(t, func() bool {
		srv.lifecycleEvents.mu.Lock()
		defer srv.lifecycleEvents.mu.Unlock()
		return srv.lifecycleEvents.drainer == nil
	}, 10*time.Second, 10*time.Millisecond, "an exited drainer is cleared")

	_, err = srv.installLifecycleObserver()
	require.NoError(t, err)
	srv.startLifecycleEvents(context.Background())
	srv.lifecycleEvents.mu.Lock()
	running := srv.lifecycleEvents.drainer != nil
	srv.lifecycleEvents.mu.Unlock()
	require.True(t, running, "a later start with the observer installed runs a drainer")
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricDrainerState, telemetryemit.DrainerStateRunning))

	lifecycleCreateAgent(t, srv, projectID, "lc-restart-2")
	require.Eventually(t, func() bool { return len(sink.Delivered()) >= 2 }, 30*time.Second, 20*time.Millisecond,
		"the new drainer delivers the second create")
}
