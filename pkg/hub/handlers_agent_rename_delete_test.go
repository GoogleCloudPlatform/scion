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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
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

// TestDeleteAgent_CancelsPendingOneShotEventsAuthoredByAgent verifies that
// deleting an agent (hard or soft) cancels pending one-shot scheduled events
// whose recorded authority is that agent with reason "creating agent deleted"
// and stops their timers (ptone/scion#5408). Self-targeted events keep the
// target-deleted precedence ("target agent deleted"), already-fired events
// keep their status, and events authored by other principals stay pending.
func TestDeleteAgent_CancelsPendingOneShotEventsAuthoredByAgent(t *testing.T) {
	cases := []struct {
		name      string
		retention time.Duration
	}{
		{"hard", 0},
		{"soft", time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			logs := &levelLogHandler{}
			srv.agentLifecycleLog = slog.New(logs)
			srv.scheduler = NewScheduler(s, slog.Default())
			srv.scheduler.srv = srv
			ctx := context.Background()

			agent := setupBrokerAgentInPhase(t, s, "oneshot-creator-"+tc.name, state.PhaseStopped)
			other := &store.Agent{
				ID: tid("oneshot-other-" + tc.name), Slug: "oneshot-other-" + tc.name,
				Name: "oneshot-other", ProjectID: agent.ProjectID, Phase: string(state.PhaseStopped),
			}
			require.NoError(t, s.CreateAgent(ctx, other))

			targetOtherPayload, err := json.Marshal(map[string]string{"agentId": other.ID, "message": "ping"})
			require.NoError(t, err)
			targetSelfPayload, err := json.Marshal(map[string]string{"agentId": agent.ID, "message": "self"})
			require.NoError(t, err)

			authored := withMockAgentRevision(store.ScheduledEvent{
				ID:        tid("oneshot-authored-" + tc.name),
				ProjectID: agent.ProjectID,
				EventType: "message",
				FireAt:    time.Now().Add(time.Hour),
				Payload:   string(targetOtherPayload),
				Status:    store.ScheduledEventPending,
				CreatedBy: agent.ID,
			}, agent.ID)
			selfTargeted := withMockAgentRevision(store.ScheduledEvent{
				ID:        tid("oneshot-self-" + tc.name),
				ProjectID: agent.ProjectID,
				EventType: "message",
				FireAt:    time.Now().Add(time.Hour),
				Payload:   string(targetSelfPayload),
				Status:    store.ScheduledEventPending,
				CreatedBy: agent.ID,
			}, agent.ID)
			firedAt := time.Now().Add(-time.Minute).UTC()
			alreadyFired := withMockAgentRevision(store.ScheduledEvent{
				ID:        tid("oneshot-fired-" + tc.name),
				ProjectID: agent.ProjectID,
				EventType: "message",
				FireAt:    firedAt,
				FiredAt:   &firedAt,
				Payload:   string(targetOtherPayload),
				Status:    store.ScheduledEventFired,
				CreatedBy: agent.ID,
			}, agent.ID)
			byOther := withMockAgentRevision(store.ScheduledEvent{
				ID:        tid("oneshot-by-other-" + tc.name),
				ProjectID: agent.ProjectID,
				EventType: "message",
				FireAt:    time.Now().Add(time.Hour),
				Payload:   string(targetOtherPayload),
				Status:    store.ScheduledEventPending,
				CreatedBy: other.ID,
			}, other.ID)
			byUser := withSessionRevision(store.ScheduledEvent{
				ID:        tid("oneshot-by-user-" + tc.name),
				ProjectID: agent.ProjectID,
				EventType: "message",
				FireAt:    time.Now().Add(time.Hour),
				Payload:   string(targetOtherPayload),
				Status:    store.ScheduledEventPending,
				CreatedBy: DevUserID,
			}, DevUserID)

			for _, evt := range []store.ScheduledEvent{authored, selfTargeted, alreadyFired, byOther, byUser} {
				e := evt
				require.NoError(t, s.CreateScheduledEvent(ctx, &e))
			}
			srv.scheduler.scheduleTimer(ctx, authored)
			srv.scheduler.scheduleTimer(ctx, selfTargeted)
			t.Cleanup(func() {
				srv.scheduler.StopEventTimer(authored.ID)
				srv.scheduler.StopEventTimer(selfTargeted.ID)
			})

			timerArmed := func(id string) bool {
				srv.scheduler.mu.Lock()
				defer srv.scheduler.mu.Unlock()
				_, ok := srv.scheduler.timers[id]
				return ok
			}
			require.True(t, timerArmed(authored.ID))
			require.True(t, timerArmed(selfTargeted.ID))

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

			gotAuthored, err := s.GetScheduledEvent(ctx, authored.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ScheduledEventCancelled, gotAuthored.Status)
			assert.Equal(t, "creating agent deleted", gotAuthored.Error)
			assert.False(t, timerArmed(authored.ID), "timer for authored event must be stopped")

			gotSelf, err := s.GetScheduledEvent(ctx, selfTargeted.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ScheduledEventCancelled, gotSelf.Status)
			assert.Equal(t, "target agent deleted", gotSelf.Error, "target-deleted takes precedence over creator-deleted")
			assert.False(t, timerArmed(selfTargeted.ID), "timer for self-targeted event must be stopped")

			gotFired, err := s.GetScheduledEvent(ctx, alreadyFired.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ScheduledEventFired, gotFired.Status)
			assert.NotNil(t, gotFired.FiredAt)
			assert.Empty(t, gotFired.Error)

			for _, unchanged := range []store.ScheduledEvent{byOther, byUser} {
				got, err := s.GetScheduledEvent(ctx, unchanged.ID)
				require.NoError(t, err)
				assert.Equal(t, store.ScheduledEventPending, got.Status, unchanged.ID)
				assert.Empty(t, got.Error, unchanged.ID)
			}

			logs.mu.Lock()
			defer logs.mu.Unlock()
			for _, rec := range logs.recs {
				if rec.level >= slog.LevelWarn && (rec.attrs["event_id"] == authored.ID || rec.attrs["event_id"] == selfTargeted.ID) {
					t.Errorf("unexpected %s log for cancelled event %v: %q (error=%v)",
						rec.level, rec.attrs["event_id"], rec.msg, rec.attrs["error"])
				}
			}
		})
	}
}

// TestDeleteAgent_KeepsAuthoredEventFiredBeforeCancel verifies that an event
// authored by the deleted agent (targeting another agent) that fires between
// ListScheduledEvents and CancelScheduledEventWithReason keeps its fired status.
func TestDeleteAgent_KeepsAuthoredEventFiredBeforeCancel(t *testing.T) {
	evtID := tid("delete-authored-fired-before-cancel-evt")
	srv, s := testServer(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *fireAfterListStore {
		return &fireAfterListStore{Store: inner, fault: f, eventID: evtID}
	})
	f := projectAgentAuthzSetupOn(t, srv, s)
	ctx := context.Background()

	logs := &levelLogHandler{}
	f.srv.agentLifecycleLog = slog.New(logs)
	f.srv.scheduler = NewScheduler(f.store, slog.Default())
	f.srv.scheduler.srv = f.srv

	payload, err := json.Marshal(map[string]string{"agentId": f.caller.ID, "message": "hi"})
	require.NoError(t, err)
	evt := withMockAgentRevision(store.ScheduledEvent{
		ID:        evtID,
		ProjectID: f.project.ID,
		EventType: "message",
		FireAt:    time.Now().Add(time.Hour),
		Payload:   string(payload),
		Status:    store.ScheduledEventPending,
		CreatedBy: f.target.ID,
	}, f.target.ID)
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
	f.srv.scheduler.scheduleTimer(ctx, evt)
	t.Cleanup(func() { f.srv.scheduler.StopEventTimer(evt.ID) })

	fault.Arm()

	delRec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, delRec.Code,
		"DELETE body: %s", delRec.Body.String())

	after, err := f.store.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduledEventFired, after.Status)
	require.NotNil(t, after.FiredAt)
	require.Empty(t, after.Error)

	f.srv.scheduler.mu.Lock()
	_, armed := f.srv.scheduler.timers[evt.ID]
	f.srv.scheduler.mu.Unlock()
	require.True(t, armed)
}

// TestFireEvent_CancelsWhenCreatingAgentGone verifies the fire-time backstop:
// a pending one-shot event whose creating agent was deleted via a path that did
// not cancel it (hard-deleted, soft-deleted, or missing) is cancelled with
// "creating agent deleted" instead of firing and failing silently with
// "message delivery refused" (ptone/scion#5408).
func TestFireEvent_CancelsWhenCreatingAgentGone(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s store.Store, projectID string) string
	}{
		{"hard deleted", func(t *testing.T, s store.Store, projectID string) string {
			id := tid("fire-creator-hard-del")
			require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
				ID: id, Slug: "fire-creator-hard-del", Name: "fire-creator-hard-del",
				ProjectID: projectID, Phase: string(state.PhaseStopped),
			}))
			require.NoError(t, s.DeleteAgent(context.Background(), id))
			return id
		}},
		{"soft deleted", func(t *testing.T, s store.Store, projectID string) string {
			id := tid("fire-creator-soft-del")
			require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
				ID: id, Slug: "fire-creator-soft-del", Name: "fire-creator-soft-del",
				ProjectID: projectID, Phase: string(state.PhaseStopped), DeletedAt: time.Now(),
			}))
			return id
		}},
		{"never existed", func(t *testing.T, s store.Store, projectID string) string {
			return tid("fire-creator-missing")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)
			ctx := context.Background()
			creatorID := tc.setup(t, s, projectID)

			evt := withMockAgentRevision(store.ScheduledEvent{
				ID:        tid("fire-creator-gone-" + tc.name),
				ProjectID: projectID,
				EventType: "message",
				FireAt:    time.Now().Add(-time.Minute),
				Payload:   `{"agentName":"other","message":"hello"}`,
				Status:    store.ScheduledEventPending,
				CreatedBy: creatorID,
			}, creatorID)
			require.NoError(t, s.CreateScheduledEvent(ctx, &evt))

			srv.scheduler.fireEvent(ctx, evt, false)

			got, err := s.GetScheduledEvent(ctx, evt.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ScheduledEventCancelled, got.Status)
			assert.Equal(t, "creating agent deleted", got.Error)
			assert.Nil(t, got.FiredAt)
		})
	}
}
