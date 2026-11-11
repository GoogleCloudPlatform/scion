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
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestApplyAgentUpdate_RenameThenDelete_DoesNotCancelOtherAgentsScheduledEvents
// verifies the delete-time cleanup contract: cancelScheduledEventsForAgent
// (run on agent DELETE) selects the pending scheduled events belonging to the
// agent being deleted via eventTargetsAgent, which keys on ID/Slug only. Name
// is a mutable display field, not an identifier, so an event targeting one
// agent's Slug stays pending when a different agent is deleted, even if that
// agent's Name equals the first agent's Slug.
func TestApplyAgentUpdate_RenameThenDelete_DoesNotCancelOtherAgentsScheduledEvents(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	// A pending scheduled event targeting agent B (f.caller) by slug, the
	// same way the scheduler's own dispatch resolution (server.go's
	// GetAgentBySlug call) treats the payload's AgentName field.
	payload, err := json.Marshal(map[string]string{"agentName": f.caller.Slug})
	require.NoError(t, err)
	evt := &store.ScheduledEvent{
		ID:        tid("rename-delete-evt"),
		ProjectID: f.project.ID,
		EventType: "message",
		FireAt:    time.Now().Add(time.Hour),
		Payload:   string(payload),
		Status:    store.ScheduledEventPending,
	}
	require.NoError(t, f.store.CreateScheduledEvent(ctx, evt))

	// Set A's display Name to a value equal to B's Slug (allowed: Name is free-form).
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": f.caller.Slug})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())

	renamed, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.Equal(t, f.caller.Slug, renamed.Name, "rename must have taken effect")
	require.NotEqual(t, f.caller.Slug, renamed.Slug, "Slug must still be A's own, unchanged")

	// Delete agent A.
	delRec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, delRec.Code,
		"DELETE body: %s", delRec.Body.String())

	// B's scheduled event targets B by slug, not A. It must still be pending
	// after A is deleted (delete cleanup keys on ID/Slug, not Name).
	after, err := f.store.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduledEventPending, after.Status,
		"B's scheduled event must still be pending after A is deleted; delete cleanup keys on ID/Slug, not the display Name")
}

// TestDeleteAgent_StopsScheduledEventTimerWithoutNotFoundWarning verifies
// that deleting an agent cancels the stored scheduled events that target it
// and stops their in-memory timers, without a second cancel of the stored
// event. The stored event is already cancelled at that point, so a second
// cancel fails with ErrNotFound and logs a warning for every event.
func TestDeleteAgent_StopsScheduledEventTimerWithoutNotFoundWarning(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	logs := &levelLogHandler{}
	f.srv.agentLifecycleLog = slog.New(logs)
	f.srv.scheduler = NewScheduler(f.store, slog.Default())

	payload, err := json.Marshal(map[string]string{"agentId": f.target.ID})
	require.NoError(t, err)
	evt := store.ScheduledEvent{
		ID:        tid("delete-stop-timer-evt"),
		ProjectID: f.project.ID,
		EventType: "message",
		FireAt:    time.Now().Add(time.Hour),
		Payload:   string(payload),
		Status:    store.ScheduledEventPending,
	}
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
	f.srv.scheduler.scheduleTimer(ctx, evt)
	t.Cleanup(func() { f.srv.scheduler.StopEventTimer(evt.ID) })

	timerArmed := func() bool {
		f.srv.scheduler.mu.Lock()
		defer f.srv.scheduler.mu.Unlock()
		_, ok := f.srv.scheduler.timers[evt.ID]
		return ok
	}
	require.True(t, timerArmed(), "timer must be armed before the delete")

	delRec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, delRec.Code,
		"DELETE body: %s", delRec.Body.String())

	after, err := f.store.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduledEventCancelled, after.Status,
		"the stored event must be cancelled by the delete")
	require.Equal(t, "target agent deleted", after.Error,
		"the delete must record why the event was cancelled")
	require.False(t, timerArmed(), "the delete must stop the event's in-memory timer")

	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, rec := range logs.recs {
		if rec.level >= slog.LevelWarn && rec.attrs["event_id"] == evt.ID {
			t.Errorf("unexpected %s log for the cancelled event: %q (error=%v)",
				rec.level, rec.msg, rec.attrs["error"])
		}
	}
}

// fireAfterListStore claims one scheduled event as fired right after
// ListScheduledEvents returns it as pending. It reproduces an event that
// fires between the delete path's list and its cancel. It delegates
// unchanged until its fault switch is armed.
type fireAfterListStore struct {
	store.Store
	fault   *storeFaultSwitch
	eventID string
}

func (s *fireAfterListStore) ListScheduledEvents(ctx context.Context, filter store.ScheduledEventFilter, opts store.ListOptions) (*store.ListResult[store.ScheduledEvent], error) {
	res, err := s.Store.ListScheduledEvents(ctx, filter, opts)
	if err != nil || !s.fault.Active() {
		return res, err
	}
	claimer, ok := s.Store.(store.ScheduledEventClaimer)
	if !ok {
		return nil, errors.New("test store does not support ClaimScheduledEvent")
	}
	claimed, err := claimer.ClaimScheduledEvent(ctx, s.eventID, store.ScheduledEventFired)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, errors.New("test event was not pending when listed")
	}
	return res, nil
}

// TestDeleteAgent_KeepsScheduledEventFiredBeforeCancel verifies that the
// delete-time cancel only changes events that are still pending. An event
// that fires after the delete path lists it but before the cancel keeps its
// fired status and fire time, its timer is left alone, and the delete logs
// no warning for it.
func TestDeleteAgent_KeepsScheduledEventFiredBeforeCancel(t *testing.T) {
	evtID := tid("delete-fired-before-cancel-evt")
	srv, s := testServer(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *fireAfterListStore {
		return &fireAfterListStore{Store: inner, fault: f, eventID: evtID}
	})
	f := projectAgentAuthzSetupOn(t, srv, s)
	ctx := context.Background()

	logs := &levelLogHandler{}
	f.srv.agentLifecycleLog = slog.New(logs)
	f.srv.scheduler = NewScheduler(f.store, slog.Default())

	payload, err := json.Marshal(map[string]string{"agentId": f.target.ID})
	require.NoError(t, err)
	evt := store.ScheduledEvent{
		ID:        evtID,
		ProjectID: f.project.ID,
		EventType: "message",
		FireAt:    time.Now().Add(time.Hour),
		Payload:   string(payload),
		Status:    store.ScheduledEventPending,
	}
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
	f.srv.scheduler.scheduleTimer(ctx, evt)
	t.Cleanup(func() { f.srv.scheduler.StopEventTimer(evt.ID) })

	fault.Arm()

	delRec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, delRec.Code,
		"DELETE body: %s", delRec.Body.String())

	after, err := f.store.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduledEventFired, after.Status,
		"an event that fired before the cancel must keep its fired status")
	require.NotNil(t, after.FiredAt, "the delete must not clear the fire time")
	require.Empty(t, after.Error, "the delete must not record a cancel reason on a fired event")

	f.srv.scheduler.mu.Lock()
	_, armed := f.srv.scheduler.timers[evt.ID]
	f.srv.scheduler.mu.Unlock()
	require.True(t, armed, "the delete must not stop the timer of an event that is no longer pending")

	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, rec := range logs.recs {
		if rec.level >= slog.LevelWarn && rec.attrs["event_id"] == evt.ID {
			t.Errorf("unexpected %s log for the fired event: %q (error=%v)",
				rec.level, rec.msg, rec.attrs["error"])
		}
	}
}
