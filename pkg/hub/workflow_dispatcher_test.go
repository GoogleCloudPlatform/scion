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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatcherTestStore is a minimal in-memory store for dispatcher unit tests.
// It only implements WorkflowRun operations.
type dispatcherTestStore struct {
	store.Store
	runs map[string]*store.WorkflowRun
}

func newDispatcherTestStore() *dispatcherTestStore {
	return &dispatcherTestStore{
		runs: make(map[string]*store.WorkflowRun),
	}
}

func (s *dispatcherTestStore) GetWorkflowRun(_ context.Context, id string) (*store.WorkflowRun, error) {
	r, ok := s.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *dispatcherTestStore) TransitionWorkflowRun(_ context.Context, id string, update store.WorkflowRunTransition, fromStatus []string) (*store.WorkflowRun, error) {
	r, ok := s.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	allowed := false
	for _, st := range fromStatus {
		if r.Status == st {
			allowed = true
			break
		}
	}
	if !allowed {
		cp := *r
		return &cp, store.ErrVersionConflict
	}
	r.Status = update.Status
	if update.BrokerID != nil {
		r.BrokerID = update.BrokerID
	}
	if update.StartedAt != nil {
		r.StartedAt = update.StartedAt
	}
	if update.FinishedAt != nil {
		r.FinishedAt = update.FinishedAt
	}
	if update.ResultJSON != nil {
		r.ResultJSON = update.ResultJSON
	}
	if update.ErrorMessage != nil {
		r.ErrorMessage = update.ErrorMessage
	}
	if update.TraceURL != nil {
		r.TraceURL = update.TraceURL
	}
	cp := *r
	return &cp, nil
}

func (s *dispatcherTestStore) GetGroveProviders(_ context.Context, groveID string) ([]store.GroveProvider, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestWorkflowRunDispatcher_Subscribe_Replay(t *testing.T) {
	st := newDispatcherTestStore()
	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	runID := api.NewUUID()

	// Manually inject buffered logs.
	entry := WorkflowLogEntry{
		Stream:    "stdout",
		Line:      "hello\n",
		Timestamp: time.Now(),
	}
	d.logsMu.Lock()
	d.logs[runID] = []WorkflowLogEntry{entry}
	d.logsMu.Unlock()

	// Subscribe should get the buffered log back.
	buffered, sub, unsub := d.Subscribe(runID)
	defer unsub()

	require.Len(t, buffered, 1)
	assert.Equal(t, "stdout", buffered[0].Stream)
	assert.Equal(t, "hello\n", buffered[0].Line)

	// Sub channel should start empty (no live events yet).
	assert.Empty(t, sub)
}

func TestWorkflowRunDispatcher_PublishAndReceive(t *testing.T) {
	st := newDispatcherTestStore()
	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	runID := api.NewUUID()
	_, sub, unsub := d.Subscribe(runID)
	defer unsub()

	// Publish a status event.
	evt := WorkflowRunEvent{
		Kind:      WorkflowRunEventStatus,
		RunID:     runID,
		Status:    "running",
		Timestamp: time.Now(),
	}
	d.publishEvent(runID, evt)

	select {
	case received := <-sub:
		assert.Equal(t, WorkflowRunEventStatus, received.Kind)
		assert.Equal(t, "running", received.Status)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestWorkflowRunDispatcher_HandleWorkflowLogEvent(t *testing.T) {
	st := newDispatcherTestStore()
	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	runID := api.NewUUID()
	_, sub, unsub := d.Subscribe(runID)
	defer unsub()

	payload := wsprotocol.WorkflowLogPayload{
		RunID:     runID,
		Stream:    "stderr",
		Line:      "an error line\n",
		Timestamp: time.Now().Format(time.RFC3339Nano),
	}
	d.HandleWorkflowLogEvent("broker-1", payload)

	// Should appear in live sub channel.
	select {
	case evt := <-sub:
		require.Equal(t, WorkflowRunEventLog, evt.Kind)
		require.NotNil(t, evt.Log)
		assert.Equal(t, "stderr", evt.Log.Stream)
		assert.Equal(t, "an error line\n", evt.Log.Line)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for log event")
	}

	// Should also be buffered.
	d.logsMu.RLock()
	logs := d.logs[runID]
	d.logsMu.RUnlock()
	require.Len(t, logs, 1)
	assert.Equal(t, "an error line\n", logs[0].Line)
}

func TestWorkflowRunDispatcher_HandleWorkflowStatusEvent(t *testing.T) {
	st := newDispatcherTestStore()
	runID := api.NewUUID()
	st.runs[runID] = &store.WorkflowRun{
		ID:     runID,
		Status: store.WorkflowRunStatusProvisioning,
	}

	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	_, sub, unsub := d.Subscribe(runID)
	defer unsub()

	payload := wsprotocol.WorkflowStatusPayload{
		RunID:  runID,
		Status: store.WorkflowRunStatusRunning,
		At:     time.Now().Format(time.RFC3339Nano),
	}
	d.HandleWorkflowStatusEvent(context.Background(), "broker-1", payload)

	// Status should be updated in the store.
	updated, err := st.GetWorkflowRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkflowRunStatusRunning, updated.Status)

	// Status event emitted.
	select {
	case evt := <-sub:
		assert.Equal(t, WorkflowRunEventStatus, evt.Kind)
		assert.Equal(t, store.WorkflowRunStatusRunning, evt.Status)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for status event")
	}
}

func TestWorkflowRunDispatcher_HandleWorkflowOutputEvent_Succeeded(t *testing.T) {
	st := newDispatcherTestStore()
	runID := api.NewUUID()
	st.runs[runID] = &store.WorkflowRun{
		ID:     runID,
		Status: store.WorkflowRunStatusRunning,
	}

	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	_, sub, unsub := d.Subscribe(runID)
	defer unsub()

	resultStr := `{"output":"hello"}`
	payload := wsprotocol.WorkflowOutputPayload{
		RunID:      runID,
		ExitCode:   0,
		ResultJSON: &resultStr,
	}
	d.HandleWorkflowOutputEvent(context.Background(), "broker-1", payload)

	// Store should reflect succeeded status.
	updated, err := st.GetWorkflowRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkflowRunStatusSucceeded, updated.Status)
	require.NotNil(t, updated.ResultJSON)
	assert.Equal(t, resultStr, *updated.ResultJSON)

	// Terminal event emitted.
	select {
	case evt := <-sub:
		assert.Equal(t, WorkflowRunEventTerminal, evt.Kind)
		assert.Equal(t, store.WorkflowRunStatusSucceeded, evt.Status)
		assert.Equal(t, 0, evt.ExitCode)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal event")
	}
}

func TestWorkflowRunDispatcher_HandleWorkflowOutputEvent_Canceled(t *testing.T) {
	st := newDispatcherTestStore()
	runID := api.NewUUID()
	st.runs[runID] = &store.WorkflowRun{
		ID:     runID,
		Status: store.WorkflowRunStatusRunning,
	}

	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	errStr := "canceled"
	payload := wsprotocol.WorkflowOutputPayload{
		RunID:    runID,
		ExitCode: 2,
		Error:    &errStr,
	}
	d.HandleWorkflowOutputEvent(context.Background(), "broker-1", payload)

	updated, err := st.GetWorkflowRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkflowRunStatusCanceled, updated.Status)
}

func TestWorkflowRunDispatcher_HandleWorkflowOutputEvent_TimedOut(t *testing.T) {
	st := newDispatcherTestStore()
	runID := api.NewUUID()
	st.runs[runID] = &store.WorkflowRun{
		ID:     runID,
		Status: store.WorkflowRunStatusRunning,
	}

	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	errStr := "timed_out"
	payload := wsprotocol.WorkflowOutputPayload{
		RunID:    runID,
		ExitCode: 2,
		Error:    &errStr,
	}
	d.HandleWorkflowOutputEvent(context.Background(), "broker-1", payload)

	updated, err := st.GetWorkflowRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkflowRunStatusTimedOut, updated.Status)
}

func TestWorkflowRunDispatcher_Unsubscribe(t *testing.T) {
	st := newDispatcherTestStore()
	d := NewWorkflowRunDispatcher(st, nil, slog.Default())

	runID := api.NewUUID()
	_, sub, unsub := d.Subscribe(runID)

	// Verify subscriber registered.
	d.subsMu.RLock()
	assert.Len(t, d.subs[runID], 1)
	d.subsMu.RUnlock()

	// Unsub should remove it and close the channel.
	unsub()

	d.subsMu.RLock()
	assert.Empty(t, d.subs[runID])
	d.subsMu.RUnlock()

	// Channel should be closed (zero-value read without blocking).
	select {
	case _, ok := <-sub:
		assert.False(t, ok, "channel should be closed after unsub")
	default:
		t.Fatal("channel was not closed after unsub")
	}
}
