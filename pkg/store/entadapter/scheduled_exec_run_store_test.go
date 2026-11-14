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

package entadapter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newExecEvent stores a claimed (fired) exec event and returns it.
func newExecEvent(t *testing.T, s *ScheduleStore, projectID, status string, firedAt time.Time) *store.ScheduledEvent {
	t.Helper()
	evt := &store.ScheduledEvent{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		EventType: "exec",
		FireAt:    firedAt,
		Payload:   `{"agentId":"x","command":["true"]}`,
		Status:    status,
	}
	require.NoError(t, s.CreateScheduledEvent(context.Background(), evt))
	if status != store.ScheduledEventPending {
		require.NoError(t, s.UpdateScheduledEventStatus(context.Background(), evt.ID, status, &firedAt, ""))
	}
	return evt
}

func newExecRun(evt *store.ScheduledEvent) *store.ScheduledExecRun {
	now := time.Now()
	return &store.ScheduledExecRun{
		EventID:                 evt.ID,
		ProjectID:               evt.ProjectID,
		TargetAgentID:           uuid.NewString(),
		InitiatorPrincipalKind:  "user",
		InitiatorPrincipalID:    "u1",
		InitiatorCredentialKind: "session",
		AuthorizationRevision:   3,
		ExecutorKind:            "scheduler",
		ExecutorID:              "scheduled_event:" + evt.ID,
		CommandSHA256:           "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Argc:                    2,
		TimeoutSeconds:          60,
		StartedAt:               now,
		LeaseUntil:              now.Add(180 * time.Second),
		Runtime:                 "docker",
		LaunchID:                "launch-1",
	}
}

func TestScheduledExecRun_CreateGetRoundTrip(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	evt := newExecEvent(t, s, uuid.NewString(), store.ScheduledEventFired, time.Now())
	run := newExecRun(evt)
	run.ScheduleID = uuid.NewString()
	require.NoError(t, s.CreateScheduledExecRun(ctx, run))

	got, err := s.GetScheduledExecRun(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledExecOutcomeStarting, got.Outcome)
	assert.Nil(t, got.FinishedAt)
	assert.Equal(t, run.ScheduleID, got.ScheduleID)
	assert.Equal(t, run.TargetAgentID, got.TargetAgentID)
	assert.Equal(t, run.CommandSHA256, got.CommandSHA256)
	assert.Equal(t, 2, got.Argc)
	assert.Equal(t, 3, got.AuthorizationRevision)
	assert.Equal(t, "launch-1", got.LaunchID)
	assert.WithinDuration(t, run.LeaseUntil, got.LeaseUntil, time.Second)

	// One run per event.
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)), store.ErrAlreadyExists)

	_, err = s.GetScheduledExecRun(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestScheduledExecRun_MarkRunningOnlyFromStarting(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	evt := newExecEvent(t, s, uuid.NewString(), store.ScheduledEventFired, time.Now())
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)))

	changed, err := s.MarkScheduledExecRunRunning(ctx, evt.ID)
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = s.MarkScheduledExecRunRunning(ctx, evt.ID)
	require.NoError(t, err)
	assert.False(t, changed, "already running")

	won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{EventID: evt.ID, Outcome: store.ScheduledExecOutcomeOK})
	require.NoError(t, err)
	require.True(t, won)
	changed, err = s.MarkScheduledExecRunRunning(ctx, evt.ID)
	require.NoError(t, err)
	assert.False(t, changed, "a finished run never goes back to running")
	got, err := s.GetScheduledExecRun(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledExecOutcomeOK, got.Outcome)
}

// TestScheduledExecRun_FinishFirstWriterWins: many concurrent finishers
// (the tracker, the sweeper, another replica) record exactly one outcome,
// and only the winner writes the event.
func TestScheduledExecRun_FinishFirstWriterWins(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	evt := newExecEvent(t, s, uuid.NewString(), store.ScheduledEventFired, time.Now())
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)))

	outcomes := []string{store.ScheduledExecOutcomeNonzero, store.ScheduledExecOutcomeAbandoned, store.ScheduledExecOutcomeError, store.ScheduledExecOutcomeTimeout}
	var wins atomic.Int32
	var winner atomic.Value
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := outcomes[i%len(outcomes)]
			code := i
			won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
				EventID: evt.ID, Outcome: o, ExitCode: &code,
				EventStatus: store.ScheduledEventFailed, EventError: o,
			})
			if err != nil {
				// SQLite may refuse a concurrent writer outright; that is
				// a loss, not a second win.
				return
			}
			if won {
				wins.Add(1)
				winner.Store(o)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, int32(1), wins.Load())
	got, err := s.GetScheduledExecRun(ctx, evt.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FinishedAt)
	assert.Equal(t, winner.Load(), got.Outcome)
	gotEvt, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFailed, gotEvt.Status)
	assert.Equal(t, winner.Load(), gotEvt.Error, "only the winner wrote the event")
}

// TestScheduledExecRun_FinishNeverOverwritesEventOutcome: the event is
// written only while it is fired or expired.
func TestScheduledExecRun_FinishNeverOverwritesEventOutcome(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	for _, status := range []string{store.ScheduledEventSkipped, store.ScheduledEventFailed, store.ScheduledEventCancelled} {
		evt := newExecEvent(t, s, uuid.NewString(), status, time.Now())
		require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)))
		won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
			EventID: evt.ID, Outcome: store.ScheduledExecOutcomeAbandoned,
			EventStatus: store.ScheduledEventFailed, EventError: "run outcome unknown",
		})
		require.NoError(t, err)
		assert.True(t, won, "the run itself is finished")
		gotEvt, err := s.GetScheduledEvent(ctx, evt.ID)
		require.NoError(t, err)
		assert.Equal(t, status, gotEvt.Status)
	}

	// expired is written like fired.
	evt := newExecEvent(t, s, uuid.NewString(), store.ScheduledEventExpired, time.Now())
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)))
	won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID: evt.ID, Outcome: store.ScheduledExecOutcomeNonzero,
		EventStatus: store.ScheduledEventFailed, EventError: "exit status 2",
	})
	require.NoError(t, err)
	require.True(t, won)
	gotEvt, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFailed, gotEvt.Status)
	assert.Equal(t, "exit status 2", gotEvt.Error)

	// A non-final outcome is refused.
	_, err = s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{EventID: evt.ID, Outcome: store.ScheduledExecOutcomeRunning})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestScheduledExecRun_ExpiredAndPollRotation(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	now := time.Now()
	var live []string
	var expired string
	for i := 0; i < 5; i++ {
		evt := newExecEvent(t, s, project, store.ScheduledEventFired, now)
		run := newExecRun(evt)
		run.StartedAt = now.Add(time.Duration(i) * time.Second)
		if i == 4 {
			run.LeaseUntil = now.Add(-time.Minute)
			expired = evt.ID
		} else {
			live = append(live, evt.ID)
		}
		require.NoError(t, s.CreateScheduledExecRun(ctx, run))
	}
	finishedEvt := newExecEvent(t, s, project, store.ScheduledEventFired, now)
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(finishedEvt)))
	_, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{EventID: finishedEvt.ID, Outcome: store.ScheduledExecOutcomeOK})
	require.NoError(t, err)

	exp, err := s.ListExpiredScheduledExecRuns(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, exp, 1)
	assert.Equal(t, expired, exp[0].EventID)

	// Claims rotate through every live run, least recently polled first,
	// across successive bounded claims, and skip excluded runs.
	seen := map[string]int{}
	for round := 0; round < 4; round++ {
		claimed, err := s.ClaimScheduledExecRunsForPoll(ctx, now.Add(time.Duration(round+10)*time.Second), 2, []string{live[0]})
		require.NoError(t, err)
		require.Len(t, claimed, 2)
		for _, r := range claimed {
			assert.NotEqual(t, live[0], r.EventID, "excluded")
			assert.NotEqual(t, expired, r.EventID, "expired runs are not polled")
			assert.NotEqual(t, finishedEvt.ID, r.EventID, "finished runs are not polled")
			seen[r.EventID]++
		}
	}
	for _, id := range live[1:] {
		assert.GreaterOrEqual(t, seen[id], 2, "every live run is polled in turn")
	}

	byEvent, err := s.ListScheduledExecRunsByEvent(ctx, append([]string{"not-a-uuid", uuid.NewString()}, live...))
	require.NoError(t, err)
	assert.Len(t, byEvent, len(live))
}

// TestScheduledExecRun_EventForeignKey: a run cannot be inserted for a
// missing event, and deleting an event deletes its run.
func TestScheduledExecRun_EventForeignKey(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	ghost := &store.ScheduledEvent{ID: uuid.NewString(), ProjectID: uuid.NewString()}
	assert.Error(t, s.CreateScheduledExecRun(ctx, newExecRun(ghost)), "no run without its event")
}

// TestDeleteSchedulesByProject_RemovesExecRuns: project deletion removes
// the project's runs, finished and unfinished, with its events, and keeps
// other projects' runs.
func TestDeleteSchedulesByProject_RemovesExecRuns(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project, other := uuid.NewString(), uuid.NewString()
	done := newExecEvent(t, s, project, store.ScheduledEventFired, time.Now())
	live := newExecEvent(t, s, project, store.ScheduledEventFired, time.Now())
	kept := newExecEvent(t, s, other, store.ScheduledEventFired, time.Now())
	for _, e := range []*store.ScheduledEvent{done, live, kept} {
		require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(e)))
	}
	_, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{EventID: done.ID, Outcome: store.ScheduledExecOutcomeNonzero})
	require.NoError(t, err)

	_, err = s.DeleteSchedulesByProject(ctx, project)
	require.NoError(t, err)
	for _, e := range []*store.ScheduledEvent{done, live} {
		_, err := s.GetScheduledExecRun(ctx, e.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
	_, err = s.GetScheduledExecRun(ctx, kept.ID)
	assert.NoError(t, err)
	// A fire past authorization cannot insert a run for a deleted event.
	assert.Error(t, s.CreateScheduledExecRun(ctx, newExecRun(live)))
	n, err := s.PurgeOldScheduledEvents(ctx, time.Now().Add(365*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = s.GetScheduledExecRun(ctx, kept.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestCreateScheduledEventChecked_LastSlot: concurrent authors racing for
// the last slot insert exactly one event; the check sees the rows the
// winner inserted.
func TestCreateScheduledEventChecked_LastSlot(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	const limit = 3
	for i := 0; i < limit-1; i++ {
		newExecEvent(t, s, project, store.ScheduledEventPending, time.Now())
	}
	errFull := errors.New("full")
	var wg sync.WaitGroup
	var inserted, refused atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evt := &store.ScheduledEvent{
				ID: uuid.NewString(), ProjectID: project, EventType: "exec", FireAt: time.Now().Add(time.Hour),
				Payload: `{"agentId":"x","command":["true"]}`, Status: store.ScheduledEventPending,
			}
			for {
				err := s.CreateScheduledEventChecked(ctx, evt, limit+1, func(pending []store.ScheduledEvent) error {
					if len(pending) >= limit {
						return errFull
					}
					return nil
				})
				switch {
				case err == nil:
					inserted.Add(1)
					return
				case errors.Is(err, errFull):
					refused.Add(1)
					return
				}
				// SQLite may refuse a concurrent writer as busy: retry.
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), inserted.Load())
	assert.Equal(t, int32(7), refused.Load())
	res, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: project, Status: store.ScheduledEventPending}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, res.Items, limit)
}

// TestPurgeOldScheduledEvents_RemovesExecRuns: a purged event's run goes
// with it; runs of kept events stay.
func TestPurgeOldScheduledEvents_RemovesExecRuns(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	old := newExecEvent(t, s, project, store.ScheduledEventFailed, time.Now().Add(-8*24*time.Hour))
	recent := newExecEvent(t, s, project, store.ScheduledEventFired, time.Now().Add(-time.Hour))
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(old)))
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(recent)))

	n, err := s.PurgeOldScheduledEvents(ctx, time.Now().Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = s.GetScheduledEvent(ctx, old.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetScheduledExecRun(ctx, old.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetScheduledExecRun(ctx, recent.ID)
	assert.NoError(t, err)
}

// TestListScheduledEvents_KeysetTraversal (P2-R2): a full traversal visits
// every matching event when UUID order opposes creation order and the
// target is older than the first page (the reviewer's fixture), with the
// 200-row page cap honoured.
func TestListScheduledEvents_KeysetTraversal(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	targetID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	old := time.Now().Add(-time.Hour)
	require.NoError(t, s.CreateScheduledEvent(ctx, &store.ScheduledEvent{
		ID: targetID, ProjectID: project, EventType: "exec", FireAt: time.Now().Add(time.Hour),
		Payload: `{"agentId":"target","command":["true"]}`, Status: store.ScheduledEventPending, CreatedAt: old,
	}))
	const n = 1000
	for i := 0; i < n; i++ {
		low := uuid.New()
		low[0] = 1
		require.NoError(t, s.CreateScheduledEvent(ctx, &store.ScheduledEvent{
			ID: low.String(), ProjectID: project, EventType: "message", FireAt: time.Now().Add(time.Hour),
			Payload: `{"agentId":"other","message":"x"}`, Status: store.ScheduledEventPending,
			CreatedAt: old.Add(time.Minute + time.Duration(i)*time.Millisecond),
		}))
	}
	cursor := ""
	seen := map[string]bool{}
	pages := 0
	for {
		result, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: project, Status: store.ScheduledEventPending},
			store.ListOptions{Limit: 1000, Cursor: cursor, SkipTotalCount: true})
		require.NoError(t, err)
		require.LessOrEqual(t, len(result.Items), 200, "page cap")
		for _, e := range result.Items {
			assert.False(t, seen[e.ID], "no event twice")
			seen[e.ID] = true
		}
		pages++
		require.Less(t, pages, 20)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	assert.Len(t, seen, n+1)
	assert.True(t, seen[targetID], "the older target is visited")

	// The earlier id-only cursor format is refused, never misread.
	_, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: project}, store.ListOptions{Cursor: targetID})
	assert.ErrorIs(t, err, store.ErrInvalidCursor)
}

// TestUnadmittedScheduledEvents (P2-R1): claimed (fired or expired) exec
// events older than the cutoff with no run are listed and failed; events
// with a run, recent claims, pending and other types are not; a recurring
// event's schedule records the failed run.
func TestUnadmittedScheduledEvents(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	longAgo := time.Now().Add(-time.Hour)
	fired := newExecEvent(t, s, project, store.ScheduledEventFired, longAgo)
	expired := newExecEvent(t, s, project, store.ScheduledEventExpired, longAgo)
	withRun := newExecEvent(t, s, project, store.ScheduledEventFired, longAgo)
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(withRun)))
	recent := newExecEvent(t, s, project, store.ScheduledEventFired, time.Now())
	pending := newExecEvent(t, s, project, store.ScheduledEventPending, longAgo)
	msg := &store.ScheduledEvent{ID: uuid.NewString(), ProjectID: project, EventType: "message", FireAt: longAgo,
		Payload: `{"agentId":"x","message":"m"}`, Status: store.ScheduledEventPending}
	require.NoError(t, s.CreateScheduledEvent(ctx, msg))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, msg.ID, store.ScheduledEventFired, &longAgo, ""))

	sched := newTestSchedule(project, "unadmitted")
	require.NoError(t, s.CreateSchedule(ctx, sched))
	recurring := &store.ScheduledEvent{ID: uuid.NewString(), ProjectID: project, EventType: "exec", FireAt: longAgo,
		Payload: `{"agentId":"x","command":["true"]}`, Status: store.ScheduledEventPending, ScheduleID: sched.ID}
	require.NoError(t, s.CreateScheduledEvent(ctx, recurring))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, recurring.ID, store.ScheduledEventFired, &longAgo, ""))

	cutoff := time.Now().Add(-time.Minute)
	got, err := s.ListUnadmittedScheduledEvents(ctx, "exec", cutoff, 100)
	require.NoError(t, err)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	assert.ElementsMatch(t, []string{fired.ID, expired.ID, recurring.ID}, ids)

	for _, e := range got {
		changed, err := s.FailUnadmittedScheduledEvent(ctx, e.ID, "exec", cutoff, "hub restarted before dispatch")
		require.NoError(t, err)
		assert.True(t, changed)
		again, err := s.FailUnadmittedScheduledEvent(ctx, e.ID, "exec", cutoff, "x")
		require.NoError(t, err)
		assert.False(t, again, "once")
		evt, err := s.GetScheduledEvent(ctx, e.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduledEventFailed, evt.Status)
		assert.Equal(t, "hub restarted before dispatch", evt.Error)
	}
	for _, e := range []*store.ScheduledEvent{withRun, recent, pending} {
		changed, err := s.FailUnadmittedScheduledEvent(ctx, e.ID, "exec", cutoff, "x")
		require.NoError(t, err)
		assert.False(t, changed, e.ID)
	}
	sc, err := s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, sc.ErrorCount, "the recurring event's schedule records the failed run")
}
