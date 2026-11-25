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
		now := time.Now()
		evt := newExecEvent(t, s, uuid.NewString(), store.ScheduledEventFired, now)
		require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)))
		require.NoError(t, s.UpdateScheduledEventStatus(ctx, evt.ID, status, &now, "prior"))
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
				err := s.CreateScheduledEventChecked(ctx, evt, limit+1, func(snap store.ScheduledExecQuotaSnapshot) error {
					if len(snap.PendingEvents)+len(snap.ActiveSchedules) >= limit {
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
	oldAt := time.Now().Add(-8 * 24 * time.Hour)
	old := newExecEvent(t, s, project, store.ScheduledEventFired, oldAt)
	recent := newExecEvent(t, s, project, store.ScheduledEventFired, time.Now().Add(-time.Hour))
	require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(old)))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, old.ID, store.ScheduledEventFailed, &oldAt, "failed"))
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
	assert.Equal(t, 1, sc.RunCount, "the recurring event's schedule increments run_count")
	assert.Equal(t, 1, sc.ErrorCount, "the recurring event's schedule records the failed run")
	assert.Equal(t, store.ScheduleRunError, sc.LastRunStatus)
	assert.Equal(t, "hub restarted before dispatch", sc.LastRunError)

	// Even if errMsg is empty, FailUnadmittedScheduledEvent still records
	// ScheduleRunError (error_count++ per D6).
	laterAgo := longAgo.Add(time.Minute)
	recurringEmptyErr := &store.ScheduledEvent{
		ID: uuid.NewString(), ProjectID: project, EventType: "exec", FireAt: laterAgo,
		Payload: `{"agentId":"x","command":["true"]}`, Status: store.ScheduledEventPending, ScheduleID: sched.ID,
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, recurringEmptyErr))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, recurringEmptyErr.ID, store.ScheduledEventFired, &laterAgo, ""))
	changed, err := s.FailUnadmittedScheduledEvent(ctx, recurringEmptyErr.ID, "exec", cutoff, "")
	require.NoError(t, err)
	assert.True(t, changed)
	sc, err = s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, sc.RunCount)
	assert.Equal(t, 2, sc.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, sc.LastRunStatus)
}

// TestClaimScheduledExecRunsForPoll_FairAcrossTargets (P3-R2): a target with
// many older runs does not take every slot; each target gets one run per
// round, in the order of its least recently polled run.
func TestClaimScheduledExecRunsForPoll_FairAcrossTargets(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	slow, healthy := uuid.NewString(), uuid.NewString()
	now := time.Now()
	add := func(target string, started time.Time) string {
		evt := newExecEvent(t, s, project, store.ScheduledEventFired, now)
		run := newExecRun(evt)
		run.TargetAgentID = target
		run.StartedAt = started
		run.LeaseUntil = now.Add(time.Hour)
		require.NoError(t, s.CreateScheduledExecRun(ctx, run))
		return evt.ID
	}
	var slowIDs []string
	for i := 0; i < 4; i++ {
		slowIDs = append(slowIDs, add(slow, now.Add(-time.Hour+time.Duration(i)*time.Second)))
	}
	healthyID := add(healthy, now.Add(-time.Minute))

	claimed, err := s.ClaimScheduledExecRunsForPoll(ctx, now, 2, nil)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.ElementsMatch(t, []string{slowIDs[0], healthyID}, []string{claimed[0].EventID, claimed[1].EventID})

	// Next round: every eligible target gets a turn again (the healthy
	// target's only run included) and the slow target's runs rotate.
	claimed, err = s.ClaimScheduledExecRunsForPoll(ctx, now.Add(time.Second), 2, nil)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.ElementsMatch(t, []string{slowIDs[1], healthyID}, []string{claimed[0].EventID, claimed[1].EventID})

	// With only the slow target eligible, both slots go to its runs.
	claimed, err = s.ClaimScheduledExecRunsForPoll(ctx, now.Add(2*time.Second), 2, []string{healthyID})
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	for _, r := range claimed {
		assert.Equal(t, slow, r.TargetAgentID)
	}
}

// TestClaimScheduledExecRunsForPoll_SeveralSlowTargetsFairness (ptone/scion#8096):
// 2 slow targets x 20 older runs and 1 healthy target x 1 newer run with 2
// slots must give the healthy target a slot within two claims (one sweep),
// even when claims share the same timestamp or previously polled slow runs
// finish and are purged.
func TestClaimScheduledExecRunsForPoll_SeveralSlowTargetsFairness(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	// Force UUID order slowA < slowB < healthy so the first claim of 2
	// picks slowA and slowB before healthy.
	slowA := "00000000-0000-4000-8000-000000000001"
	slowB := "00000000-0000-4000-8000-000000000002"
	healthy := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	now := time.Now().UTC().Truncate(time.Second)

	add := func(target string, started time.Time) string {
		evt := newExecEvent(t, s, project, store.ScheduledEventFired, started)
		run := newExecRun(evt)
		run.TargetAgentID = target
		run.StartedAt = started
		run.LeaseUntil = now.Add(time.Hour)
		require.NoError(t, s.CreateScheduledExecRun(ctx, run))
		return evt.ID
	}

	for i := 0; i < 20; i++ {
		add(slowA, now.Add(-time.Hour+time.Duration(i)*time.Second))
		add(slowB, now.Add(-time.Hour+time.Duration(i)*time.Second))
	}
	healthyID := add(healthy, now.Add(-time.Minute))

	var excluded []string
	claim1, err := s.ClaimScheduledExecRunsForPoll(ctx, now, 2, excluded)
	require.NoError(t, err)
	require.Len(t, claim1, 2)
	for _, r := range claim1 {
		excluded = append(excluded, r.EventID)
	}

	// Second claim at the exact same instant: healthy MUST get a slot
	// rather than waiting behind slowA and slowB's remaining 19 older runs.
	claim2, err := s.ClaimScheduledExecRunsForPoll(ctx, now, 2, excluded)
	require.NoError(t, err)
	require.Len(t, claim2, 2)
	gotHealthy := false
	for _, r := range claim2 {
		if r.EventID == healthyID {
			gotHealthy = true
		}
	}
	require.True(t, gotHealthy, "healthy target must get a turn on claim 2, got %+v", claim2)

	// Finishing and purging the already-polled slow runs must not reset
	// slowA or slowB's persisted turn ahead of slowB's next turn.
	for _, r := range claim1 {
		_, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
			EventID: r.EventID, Outcome: store.ScheduledExecOutcomeOK,
		})
		require.NoError(t, err)
	}
	// claim2 gave slots to healthy and slowA; therefore slowB must lead claim3.
	for _, r := range claim2 {
		excluded = append(excluded, r.EventID)
	}
	claim3, err := s.ClaimScheduledExecRunsForPoll(ctx, now, 1, excluded)
	require.NoError(t, err)
	require.Len(t, claim3, 1)
	assert.Equal(t, slowB, claim3[0].TargetAgentID, "persisted per-agent turn survives finished runs")
}

// TestClaimScheduledExecRunsForPoll_CandidateWindowStress (ptone/scion#8096):
// more than 5,000 older unfinished runs on a slow target must not hide a
// healthy target's newer run outside a candidate-row window.
func TestClaimScheduledExecRunsForPoll_CandidateWindowStress(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	slow := "00000000-0000-4000-8000-000000000001"
	healthy := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	now := time.Now().UTC().Truncate(time.Second)

	const slowCount = 5200
	txClient, err := s.client.Tx(ctx)
	require.NoError(t, err)
	txStore := &ScheduleStore{client: txClient.Client(), inTx: true}
	for i := 0; i < slowCount; i++ {
		started := now.Add(-time.Hour + time.Duration(i)*time.Millisecond)
		evt := &store.ScheduledEvent{
			ID: uuid.NewString(), ProjectID: project, EventType: "exec",
			FireAt: started, FiredAt: &started, Payload: `{"agentId":"x","command":["true"]}`,
			Status: store.ScheduledEventFired,
		}
		require.NoError(t, txStore.CreateScheduledEvent(ctx, evt))
		run := newExecRun(evt)
		run.TargetAgentID = slow
		run.StartedAt = started
		run.LeaseUntil = now.Add(time.Hour)
		require.NoError(t, txStore.CreateScheduledExecRun(ctx, run))
	}
	require.NoError(t, txClient.Commit())

	healthyEvt := newExecEvent(t, s, project, store.ScheduledEventFired, now.Add(-time.Minute))
	healthyRun := newExecRun(healthyEvt)
	healthyRun.TargetAgentID = healthy
	healthyRun.StartedAt = now.Add(-time.Minute)
	healthyRun.LeaseUntil = now.Add(time.Hour)
	require.NoError(t, s.CreateScheduledExecRun(ctx, healthyRun))

	claimed, err := s.ClaimScheduledExecRunsForPoll(ctx, now, 32, nil)
	require.NoError(t, err)
	require.Len(t, claimed, 32)
	var foundHealthy bool
	for _, r := range claimed {
		if r.EventID == healthyEvt.ID {
			foundHealthy = true
		}
	}
	assert.True(t, foundHealthy, "healthy target must be claimed in the first batch even beyond 5,000 slow rows")
}

// TestClaimScheduledExecRunsForPoll_OncePerSweepWithExclusions (ptone/scion#8096):
// passing the sweep's already-claimed IDs claims every eligible run at most once.
func TestClaimScheduledExecRunsForPoll_OncePerSweepWithExclusions(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	now := time.Now().UTC()
	targets := []string{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}

	const total = 40
	for i := 0; i < total; i++ {
		evt := newExecEvent(t, s, project, store.ScheduledEventFired, now)
		run := newExecRun(evt)
		run.TargetAgentID = targets[i%len(targets)]
		run.StartedAt = now.Add(time.Duration(i) * time.Millisecond)
		run.LeaseUntil = now.Add(time.Hour)
		require.NoError(t, s.CreateScheduledExecRun(ctx, run))
	}

	seen := map[string]int{}
	var excluded []string
	for {
		claimed, err := s.ClaimScheduledExecRunsForPoll(ctx, now, 12, excluded)
		require.NoError(t, err)
		if len(claimed) == 0 {
			break
		}
		for _, r := range claimed {
			seen[r.EventID]++
			excluded = append(excluded, r.EventID)
		}
	}
	assert.Len(t, seen, total)
	for id, count := range seen {
		assert.Equal(t, 1, count, "run %s claimed more than once", id)
	}
}

func newTestExecSchedule(t *testing.T, s *ScheduleStore, project, name, status string) store.Schedule {
	t.Helper()
	next := time.Now().UTC().Add(time.Minute)
	sc := store.Schedule{
		ID:        uuid.NewString(),
		ProjectID: project,
		Name:      name,
		CronExpr:  "* * * * *",
		EventType: "exec",
		Payload:   `{"agentId":"target-1","command":["echo","hi"]}`,
		Status:    status,
		NextRunAt: &next,
	}
	require.NoError(t, s.CreateSchedule(context.Background(), &sc))
	return sc
}

func newRecurringExecEvent(t *testing.T, s *ScheduleStore, sc store.Schedule, firedAt time.Time) *store.ScheduledEvent {
	t.Helper()
	f := firedAt.UTC()
	evt := &store.ScheduledEvent{
		ID:         uuid.NewString(),
		ProjectID:  sc.ProjectID,
		ScheduleID: sc.ID,
		EventType:  "exec",
		FireAt:     f,
		FiredAt:    &f,
		Payload:    sc.Payload,
		Status:     store.ScheduledEventFired,
	}
	require.NoError(t, s.CreateScheduledEvent(context.Background(), evt))
	return evt
}

func TestCreateScheduledExecRun_RecurringOverlapLease(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)

	sc1 := newTestExecSchedule(t, s, project, "sched-1", store.ScheduleStatusActive)
	sc2 := newTestExecSchedule(t, s, project, "sched-2", store.ScheduleStatusActive)

	evt1 := newRecurringExecEvent(t, s, sc1, now)
	run1 := newExecRun(evt1)
	run1.ScheduleID = sc1.ID
	run1.StartedAt = now
	run1.LeaseUntil = now.Add(5 * time.Minute)
	require.NoError(t, s.CreateScheduledExecRun(ctx, run1))

	// Duplicate insert for the SAME event still returns ErrAlreadyExists.
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, run1), store.ErrAlreadyExists)

	// Overlapping fire on the same schedule while run1's lease is live is refused.
	evt2 := newRecurringExecEvent(t, s, sc1, now.Add(time.Minute))
	run2 := newExecRun(evt2)
	run2.ScheduleID = sc1.ID
	run2.StartedAt = now.Add(time.Minute)
	run2.LeaseUntil = now.Add(6 * time.Minute)
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, run2), store.ErrScheduledExecOverlap)
	_, getErr := s.GetScheduledExecRun(ctx, evt2.ID)
	assert.ErrorIs(t, getErr, store.ErrNotFound)

	// Another schedule and a one-shot event on the same target are unaffected.
	otherEvt := newRecurringExecEvent(t, s, sc2, now.Add(time.Minute))
	otherRun := newExecRun(otherEvt)
	otherRun.ScheduleID = sc2.ID
	otherRun.TargetAgentID = run1.TargetAgentID
	otherRun.StartedAt = now.Add(time.Minute)
	otherRun.LeaseUntil = now.Add(6 * time.Minute)
	require.NoError(t, s.CreateScheduledExecRun(ctx, otherRun))

	oneShotEvt := newExecEvent(t, s, project, store.ScheduledEventFired, now)
	oneShotRun := newExecRun(oneShotEvt)
	oneShotRun.TargetAgentID = run1.TargetAgentID
	require.NoError(t, s.CreateScheduledExecRun(ctx, oneShotRun))

	// Once run1's lease has expired (StartedAt >= LeaseUntil), a new run for sc1 is admitted.
	evt3 := newRecurringExecEvent(t, s, sc1, now.Add(6*time.Minute))
	run3 := newExecRun(evt3)
	run3.ScheduleID = sc1.ID
	run3.StartedAt = now.Add(6 * time.Minute)
	run3.LeaseUntil = now.Add(11 * time.Minute)
	require.NoError(t, s.CreateScheduledExecRun(ctx, run3))

	// Finishing run3 releases the DB overlap lease immediately even before LeaseUntil.
	won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:    evt3.ID,
		FinishedAt: now.Add(7 * time.Minute),
		Outcome:    store.ScheduledExecOutcomeOK,
	})
	require.NoError(t, err)
	require.True(t, won)

	evt4 := newRecurringExecEvent(t, s, sc1, now.Add(8*time.Minute))
	run4 := newExecRun(evt4)
	run4.ScheduleID = sc1.ID
	run4.StartedAt = now.Add(8 * time.Minute)
	run4.LeaseUntil = now.Add(13 * time.Minute)
	require.NoError(t, s.CreateScheduledExecRun(ctx, run4))
}

func TestFinishScheduledExecRun_UpdatesScheduleAccountingAtomically(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	base := time.Now().UTC().Truncate(time.Second)

	sc := newTestExecSchedule(t, s, project, "accounting", store.ScheduleStatusActive)

	// 1. Exit 0 -> success; RunCount=1, ErrorCount=0.
	e1 := newRecurringExecEvent(t, s, sc, base)
	r1 := newExecRun(e1)
	r1.ScheduleID = sc.ID
	require.NoError(t, s.CreateScheduledExecRun(ctx, r1))
	won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:    e1.ID,
		FinishedAt: base.Add(time.Second),
		Outcome:    store.ScheduledExecOutcomeOK,
	})
	require.NoError(t, err)
	require.True(t, won)

	got, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, 0, got.ErrorCount)
	assert.Equal(t, store.ScheduleRunSuccess, got.LastRunStatus)
	assert.Empty(t, got.LastRunError)

	// 2. Skipped -> LastRunStatus=skipped; RunCount=2, ErrorCount still 0!
	e2 := newRecurringExecEvent(t, s, sc, base.Add(time.Minute))
	r2 := newExecRun(e2)
	r2.ScheduleID = sc.ID
	require.NoError(t, s.CreateScheduledExecRun(ctx, r2))
	won, err = s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:     e2.ID,
		FinishedAt:  base.Add(time.Minute + time.Second),
		Outcome:     store.ScheduledExecOutcomeSkipped,
		EventStatus: store.ScheduledEventSkipped,
		EventError:  "previous run in progress",
	})
	require.NoError(t, err)
	require.True(t, won)

	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, got.RunCount)
	assert.Equal(t, 0, got.ErrorCount)
	assert.Equal(t, store.ScheduleRunSkipped, got.LastRunStatus)
	assert.Equal(t, "previous run in progress", got.LastRunError)

	// 3. Nonzero -> LastRunStatus=error; RunCount=3, ErrorCount=1.
	e3 := newRecurringExecEvent(t, s, sc, base.Add(2*time.Minute))
	r3 := newExecRun(e3)
	r3.ScheduleID = sc.ID
	require.NoError(t, s.CreateScheduledExecRun(ctx, r3))
	won, err = s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:     e3.ID,
		FinishedAt:  base.Add(2*time.Minute + time.Second),
		Outcome:     store.ScheduledExecOutcomeNonzero,
		EventStatus: store.ScheduledEventFailed,
		EventError:  "command exited with status 2",
	})
	require.NoError(t, err)
	require.True(t, won)

	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, got.RunCount)
	assert.Equal(t, 1, got.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, got.LastRunStatus)
	assert.Equal(t, "command exited with status 2", got.LastRunError)

	// 4. Concurrent CAS finishers on Run 4 count the run on the schedule ONCE.
	e4 := newRecurringExecEvent(t, s, sc, base.Add(3*time.Minute))
	r4 := newExecRun(e4)
	r4.ScheduleID = sc.ID
	require.NoError(t, s.CreateScheduledExecRun(ctx, r4))
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fin := store.ScheduledExecRunFinish{
				EventID:    e4.ID,
				FinishedAt: base.Add(3*time.Minute + time.Second),
				Outcome:    store.ScheduledExecOutcomeOK,
			}
			if i%2 == 1 {
				fin.Outcome = store.ScheduledExecOutcomeAbandoned
				fin.EventStatus = store.ScheduledEventFailed
				fin.EventError = "run outcome unknown after lease expiry"
			}
			for {
				w, finErr := s.FinishScheduledExecRun(ctx, fin)
				if finErr == nil {
					if w {
						wins.Add(1)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, got.RunCount)
}

func TestCompleteUnadmittedExecEvent_SkippedAndFailedAndAdmittedGuard(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	base := time.Now().UTC().Truncate(time.Second)

	sc := newTestExecSchedule(t, s, project, "preadmit", store.ScheduleStatusActive)

	// Pre-admission skip updates event to skipped and bumps RunCount without ErrorCount.
	eSkip := newRecurringExecEvent(t, s, sc, base)
	ok, err := s.CompleteUnadmittedExecEvent(ctx, eSkip.ID, store.ScheduledEventSkipped, "missed window", sc.ID, base)
	require.NoError(t, err)
	assert.True(t, ok)

	gotEvt, err := s.GetScheduledEvent(ctx, eSkip.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventSkipped, gotEvt.Status)
	assert.Equal(t, "missed window", gotEvt.Error)

	gotSc, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, gotSc.RunCount)
	assert.Equal(t, 0, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunSkipped, gotSc.LastRunStatus)

	// Pre-admission failure updates event to failed and bumps ErrorCount.
	eFail := newRecurringExecEvent(t, s, sc, base.Add(time.Minute))
	ok, err = s.CompleteUnadmittedExecEvent(ctx, eFail.ID, store.ScheduledEventFailed, "hub shutting down", sc.ID, base.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, ok)

	gotSc, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, gotSc.RunCount)
	assert.Equal(t, 1, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)

	// Once admitted (ScheduledExecRun row exists), CompleteUnadmittedExecEvent MUST NOT clobber it!
	eAdmitted := newRecurringExecEvent(t, s, sc, base.Add(2*time.Minute))
	rAdmitted := newExecRun(eAdmitted)
	rAdmitted.ScheduleID = sc.ID
	require.NoError(t, s.CreateScheduledExecRun(ctx, rAdmitted))

	ok, err = s.CompleteUnadmittedExecEvent(ctx, eAdmitted.ID, store.ScheduledEventFailed, "hub shutting down", sc.ID, base.Add(2*time.Minute))
	require.NoError(t, err)
	assert.False(t, ok)

	gotEvt, err = s.GetScheduledEvent(ctx, eAdmitted.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, gotEvt.Status)
	gotSc, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, gotSc.RunCount)
}

func TestCheckedExecQuota_CrossEventAndScheduleLastSlotAndSelfUpdate(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	const limit = 3

	// 1 pending exec event + 1 active exec schedule = 2 active items (1 slot left).
	newExecEvent(t, s, project, store.ScheduledEventPending, time.Now())
	active1 := newTestExecSchedule(t, s, project, "active-1", store.ScheduleStatusActive)

	// Paused exec schedule and active message schedule must NOT count.
	pausedToResume := newTestExecSchedule(t, s, project, "paused-resume", store.ScheduleStatusPaused)
	msgSched := store.Schedule{
		ID: uuid.NewString(), ProjectID: project, Name: "msg-1", CronExpr: "* * * * *",
		EventType: "message", Payload: `{"message":"hi"}`, Status: store.ScheduleStatusActive,
	}
	require.NoError(t, s.CreateSchedule(ctx, &msgSched))

	errFull := errors.New("quota full")
	quotaCheck := func(snap store.ScheduledExecQuotaSnapshot) error {
		if len(snap.PendingEvents)+len(snap.ActiveSchedules) >= limit {
			return errFull
		}
		return nil
	}

	// Race 3 one-shot creates + 3 active schedule creates + 1 resume for the single remaining slot!
	var wg sync.WaitGroup
	var winners, refused atomic.Int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evt := &store.ScheduledEvent{
				ID: uuid.NewString(), ProjectID: project, EventType: "exec",
				FireAt: time.Now().Add(time.Hour), Payload: `{"agentId":"x","command":["true"]}`,
				Status: store.ScheduledEventPending,
			}
			for tries := 0; tries < 50; tries++ {
				err := s.CreateScheduledEventChecked(ctx, evt, limit+1, quotaCheck)
				switch {
				case err == nil:
					winners.Add(1)
					return
				case errors.Is(err, errFull):
					refused.Add(1)
					return
				case errors.Is(err, store.ErrTransient):
					time.Sleep(time.Millisecond)
				default:
					return
				}
			}
		}()
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			next := time.Now().UTC().Add(time.Minute)
			sc := &store.Schedule{
				ID: uuid.NewString(), ProjectID: project, Name: uuid.NewString(),
				CronExpr: "* * * * *", EventType: "exec",
				Payload: `{"agentId":"x","command":["true"]}`, Status: store.ScheduleStatusActive,
				NextRunAt: &next,
			}
			for tries := 0; tries < 50; tries++ {
				err := s.CreateScheduleChecked(ctx, sc, limit+1, quotaCheck)
				switch {
				case err == nil:
					winners.Add(1)
					return
				case errors.Is(err, errFull):
					refused.Add(1)
					return
				case errors.Is(err, store.ErrTransient):
					time.Sleep(time.Millisecond)
				default:
					return
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		upd := pausedToResume
		upd.Status = store.ScheduleStatusActive
		for tries := 0; tries < 50; tries++ {
			err := s.UpdateScheduleChecked(ctx, &upd, store.ScheduleFieldMask{Status: true}, 0, false, nil, limit+1, quotaCheck)
			switch {
			case err == nil:
				winners.Add(1)
				return
			case errors.Is(err, errFull):
				refused.Add(1)
				return
			case errors.Is(err, store.ErrTransient):
				time.Sleep(time.Millisecond)
			default:
				return
			}
		}
	}()
	wg.Wait()

	assert.Equal(t, int32(1), winners.Load(), "exactly one concurrent author may claim the last slot")
	assert.Equal(t, int32(6), refused.Load())

	// Editing an ALREADY-active exec schedule at full quota must succeed because it excludes itself.
	editActive := active1
	editActive.Payload = `{"agentId":"target-1","command":["echo","edited"]}`
	require.NoError(t, s.UpdateScheduleChecked(ctx, &editActive, store.ScheduleFieldMask{Payload: true}, 0, false, nil, limit+1, quotaCheck))

	// ListSchedules filters by EventType.
	execList, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: project, EventType: "exec"}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	for _, item := range execList.Items {
		assert.Equal(t, "exec", item.EventType)
	}
}

// TestCreateScheduledExecRun_RequiresClaimedEventStatus (ptone/scion#8210):
// CreateScheduledExecRun inserts a run only while the parent event is claimed
// (fired or expired), refusing pending, firing, skipped, failed, and cancelled
// events for both one-shot and recurring runs while preserving ErrAlreadyExists
// when a run for that event already exists.
func TestCreateScheduledExecRun_RequiresClaimedEventStatus(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	sc := newTestExecSchedule(t, s, project, "status-guard", store.ScheduleStatusActive)

	for _, status := range []string{store.ScheduledEventFired, store.ScheduledEventExpired} {
		evt := newExecEvent(t, s, project, status, now)
		require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)), "status %s must admit", status)
		_, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
			EventID: evt.ID, FinishedAt: now.Add(time.Second),
			Outcome: store.ScheduledExecOutcomeError, EventStatus: store.ScheduledEventFailed, EventError: "boom",
		})
		require.NoError(t, err)
		// Even after the event moved to failed, re-inserting for the same event returns ErrAlreadyExists.
		assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)), store.ErrAlreadyExists)
	}

	for _, status := range []string{
		store.ScheduledEventPending,
		store.ScheduledEventFiring,
		store.ScheduledEventSkipped,
		store.ScheduledEventFailed,
		store.ScheduledEventCancelled,
	} {
		oneShot := newExecEvent(t, s, project, status, now)
		err := s.CreateScheduledExecRun(ctx, newExecRun(oneShot))
		assert.ErrorIs(t, err, store.ErrScheduledExecEventClosed, "one-shot status %s must refuse admission", status)
		_, getErr := s.GetScheduledExecRun(ctx, oneShot.ID)
		assert.ErrorIs(t, getErr, store.ErrNotFound)

		recEvt := &store.ScheduledEvent{
			ID: uuid.NewString(), ProjectID: project, ScheduleID: sc.ID,
			EventType: "exec", FireAt: now, FiredAt: &now,
			Payload: sc.Payload, Status: status,
		}
		require.NoError(t, s.CreateScheduledEvent(ctx, recEvt))
		recRun := newExecRun(recEvt)
		recRun.ScheduleID = sc.ID
		err = s.CreateScheduledExecRun(ctx, recRun)
		assert.ErrorIs(t, err, store.ErrScheduledExecEventClosed, "recurring status %s must refuse admission", status)
		_, getErr = s.GetScheduledExecRun(ctx, recEvt.ID)
		assert.ErrorIs(t, getErr, store.ErrNotFound)
	}
}

// TestCreateScheduledExecRun_RacesUnadmittedCompletionMutuallyExclusive (ptone/scion#8210):
// when CreateScheduledExecRun races FailUnadmittedScheduledEvent or
// CompleteUnadmittedExecEvent on the same event, exactly one side wins:
// either the run is admitted (and the unadmitted fail is a no-op) or the event
// is marked failed once (and CreateScheduledExecRun returns ErrScheduledExecEventClosed).
func TestCreateScheduledExecRun_RacesUnadmittedCompletionMutuallyExclusive(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	base := time.Now().UTC().Truncate(time.Second)

	retryTransient := func(fn func() error) error {
		for tries := 0; tries < 50; tries++ {
			err := fn()
			if !errors.Is(err, store.ErrTransient) {
				return err
			}
			time.Sleep(time.Millisecond)
		}
		return store.ErrTransient
	}

	for trial := 0; trial < 20; trial++ {
		sc := newTestExecSchedule(t, s, project, uuid.NewString(), store.ScheduleStatusActive)
		claimedAt := base.Add(-2 * time.Hour)
		evt := newRecurringExecEvent(t, s, sc, claimedAt)
		cutoff := base.Add(-time.Minute)

		run := newExecRun(evt)
		run.ScheduleID = sc.ID
		run.StartedAt = base
		run.LeaseUntil = base.Add(5 * time.Minute)

		start := make(chan struct{})
		var admitErr, failErr error
		var failChanged bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			admitErr = retryTransient(func() error {
				return s.CreateScheduledExecRun(ctx, run)
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			failErr = retryTransient(func() error {
				var err error
				if trial%2 == 0 {
					failChanged, err = s.FailUnadmittedScheduledEvent(ctx, evt.ID, "exec", cutoff, "hub restarted before dispatch")
				} else {
					failChanged, err = s.CompleteUnadmittedExecEvent(ctx, evt.ID, store.ScheduledEventFailed, "hub shutting down", sc.ID, base)
				}
				return err
			})
		}()
		close(start)
		wg.Wait()

		require.NoError(t, failErr)
		if admitErr == nil {
			assert.False(t, failChanged, "trial %d: admit won so unadmitted fail must be a no-op", trial)
			won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
				EventID: evt.ID, FinishedAt: base.Add(time.Second), Outcome: store.ScheduledExecOutcomeOK,
			})
			require.NoError(t, err)
			require.True(t, won)
			gotSc, err := s.GetSchedule(ctx, sc.ID)
			require.NoError(t, err)
			assert.Equal(t, 1, gotSc.RunCount, "trial %d: run_count must increment once", trial)
			assert.Equal(t, 0, gotSc.ErrorCount)
			gotEvt, err := s.GetScheduledEvent(ctx, evt.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ScheduledEventFired, gotEvt.Status)
		} else {
			assert.ErrorIs(t, admitErr, store.ErrScheduledExecEventClosed, "trial %d", trial)
			assert.True(t, failChanged, "trial %d: unadmitted fail won", trial)
			_, getErr := s.GetScheduledExecRun(ctx, evt.ID)
			assert.ErrorIs(t, getErr, store.ErrNotFound)
			gotSc, err := s.GetSchedule(ctx, sc.ID)
			require.NoError(t, err)
			assert.Equal(t, 1, gotSc.RunCount, "trial %d: run_count must increment once", trial)
			assert.Equal(t, 1, gotSc.ErrorCount)
			gotEvt, err := s.GetScheduledEvent(ctx, evt.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ScheduledEventFailed, gotEvt.Status)
		}
	}
}

// TestScheduledExecRun_TriggerFireOwnerLifecycle verifies P8's fire_id owner
// extension and em-2 invariants (a), (b), (c) on scheduled_exec_runs:
//   - single-owner validation (event_id vs fire_id, never schedule_id on trigger runs);
//   - UNIQUE fire_id idempotency and overlap_key enforcement;
//   - atomic trigger_fires status transition + trigger_subject_claims bind on admission;
//   - CAS FinishScheduledExecRun updating trigger_fires, triggers counters, and
//     releasing trigger_subject_claims in one transaction without overwriting;
//   - ClaimScheduledExecRunsForPoll and ListExpiredScheduledExecRuns covering fire_id runs;
//   - ReapExpiredTriggerFires skipping admitted runs and PurgeOldTriggerHistory deleting runs.
func TestScheduledExecRun_TriggerFireOwnerLifecycle(t *testing.T) {
	cs := newTestCompositeStore(t)
	s := cs.ScheduleStore
	ts := cs.TriggerStore
	ctx := context.Background()

	projectID := uuid.NewString()
	targetAgentID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	tr := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projectID,
		Name:          "exec-trig-1",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionExec,
		ActionPayload: `{"agentId":"` + targetAgentID + `","command":["true"]}`,
		SubjectMode:   store.TriggerSubjectModeSkip,
		Status:        store.TriggerStatusActive,
	}
	require.NoError(t, ts.CreateTrigger(ctx, tr))

	newFire := func(subject string) (*store.TriggerEvent, *store.TriggerFire) {
		ev := &store.TriggerEvent{
			ID:         uuid.NewString(),
			ProjectID:  projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   tr.SourceID,
			Type:       "webhook.received",
			Subject:    subject,
			DedupKey:   uuid.NewString(),
			OccurredAt: now,
			ReceivedAt: now,
			Status:     store.TriggerEventStatusPending,
		}
		require.NoError(t, ts.CreateTriggerEvent(ctx, ev))
		tf := &store.TriggerFire{
			ID:         uuid.NewString(),
			ProjectID:  projectID,
			TriggerID:  tr.ID,
			EventID:    ev.ID,
			Subject:    subject,
			ActionType: store.TriggerActionExec,
			Status:     store.TriggerFireStatusFiring,
			LeaseUntil: now.Add(10 * time.Minute),
			StartedAt:  now,
			CreatedAt:  now,
		}
		claimed, _, err := ts.ClaimTriggerFire(ctx, tf)
		require.NoError(t, err)
		require.True(t, claimed)
		return ev, tf
	}

	ev1, f1 := newFire("sub-1")
	claim1, existing, err := ts.ClaimTriggerSubject(ctx, tr.ID, 0, "sub-1", ev1.ID, f1.ID, now, now.Add(-5*time.Minute))
	require.NoError(t, err)
	require.Nil(t, existing)
	require.NotNil(t, claim1)

	// Invalid owner combinations rejected before DB insert.
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
		ProjectID: projectID, TargetAgentID: targetAgentID, CommandSHA256: "abc",
	}), store.ErrInvalidInput, "neither event_id nor fire_id")
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
		EventID: uuid.NewString(), FireID: f1.ID, ProjectID: projectID, TargetAgentID: targetAgentID, CommandSHA256: "abc",
	}), store.ErrInvalidInput, "both event_id and fire_id")
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
		FireID: f1.ID, ScheduleID: uuid.NewString(), ProjectID: projectID, TargetAgentID: targetAgentID, CommandSHA256: "abc",
	}), store.ErrInvalidInput, "fire_id with schedule_id forbidden")

	run1 := &store.ScheduledExecRun{
		FireID:         f1.ID,
		OverlapKey:     "trigger:" + tr.ID,
		ProjectID:      projectID,
		TargetAgentID:  targetAgentID,
		CommandSHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Argc:           1,
		TimeoutSeconds: 60,
		StartedAt:      now,
		NotAfter:       now.Add(time.Minute),
		LeaseUntil:     now.Add(3 * time.Minute),
		Runtime:        "docker",
		LaunchID:       "launch-1",
	}
	require.NoError(t, s.CreateScheduledExecRun(ctx, run1))

	// Admission transitioned trigger_fires to fired and bound the subject claim.
	gotF1, err := ts.GetTriggerFire(ctx, f1.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFired, gotF1.Status)
	assert.Equal(t, f1.ID, gotF1.ResultRef)
	assert.Equal(t, targetAgentID, gotF1.TargetAgentID)

	activeClaim, err := ts.GetActiveTriggerSubjectClaim(ctx, tr.ID, 0, "sub-1")
	require.NoError(t, err)
	assert.Equal(t, targetAgentID, activeClaim.AgentID, "admission must bind subject claim to target agent")

	// Invariant (a): duplicate fire_id is rejected.
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, run1), store.ErrAlreadyExists)

	// Overlap check via overlap_key (never schedule_id).
	_, f2 := newFire("sub-2")
	run2 := *run1
	run2.FireID = f2.ID
	assert.ErrorIs(t, s.CreateScheduledExecRun(ctx, &run2), store.ErrScheduledExecOverlap)

	// GetScheduledExecRun, ListScheduledExecRunsByFire, MarkScheduledExecRunRunning work by fire_id.
	gotRun, err := s.GetScheduledExecRun(ctx, f1.ID)
	require.NoError(t, err)
	assert.Empty(t, gotRun.EventID)
	assert.Equal(t, f1.ID, gotRun.FireID)
	assert.Empty(t, gotRun.ScheduleID)
	assert.Equal(t, "trigger:"+tr.ID, gotRun.OverlapKey)

	byFire, err := s.ListScheduledExecRunsByFire(ctx, []string{f1.ID, uuid.NewString()})
	require.NoError(t, err)
	require.Contains(t, byFire, f1.ID)

	changed, err := s.MarkScheduledExecRunRunning(ctx, f1.ID)
	require.NoError(t, err)
	assert.True(t, changed)

	// Invariant (c): ClaimScheduledExecRunsForPoll includes fire_id runs and honors excludeEventIDs.
	excluded, err := s.ClaimScheduledExecRunsForPoll(ctx, now.Add(time.Second), 10, []string{f1.ID})
	require.NoError(t, err)
	assert.Empty(t, excluded, "exclude list must exclude fire_id-owned runs")

	claimedPoll, err := s.ClaimScheduledExecRunsForPoll(ctx, now.Add(2*time.Second), 10, nil)
	require.NoError(t, err)
	require.Len(t, claimedPoll, 1)
	assert.Equal(t, f1.ID, claimedPoll[0].FireID)

	// ReapExpiredTriggerFires must NOT reap an admitted fire even past lease_until.
	reaped, err := ts.ReapExpiredTriggerFires(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	// f2 is unadmitted and expired, so f2 gets reaped while f1 stays intact.
	assert.Equal(t, 1, reaped)

	// Invariant (b): FinishScheduledExecRun CAS updates run, trigger_fires, triggers counters, and releases claim.
	exitCode := 2
	durMS := int64(450)
	outBytes := int64(12)
	won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		FireID:      f1.ID,
		FinishedAt:  now.Add(5 * time.Second),
		Outcome:     store.ScheduledExecOutcomeNonzero,
		ExitCode:    &exitCode,
		DurationMS:  &durMS,
		OutputBytes: &outBytes,
		FireStatus:  store.TriggerFireStatusFailed,
		FireError:   "exit status 2",
	})
	require.NoError(t, err)
	require.True(t, won)

	// Second writer loses CAS and cannot overwrite trigger_fires.
	won2, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		FireID:     f1.ID,
		FinishedAt: now.Add(6 * time.Second),
		Outcome:    store.ScheduledExecOutcomeOK,
		FireStatus: store.TriggerFireStatusFired,
	})
	require.NoError(t, err)
	assert.False(t, won2)

	gotF1, err = ts.GetTriggerFire(ctx, f1.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFailed, gotF1.Status)
	assert.Equal(t, "exit status 2", gotF1.Error)
	assert.NotNil(t, gotF1.FinishedAt)

	gotTr, err := ts.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, gotTr.FireCount, 1)
	assert.GreaterOrEqual(t, gotTr.ErrorCount, 1)

	_, err = ts.GetActiveTriggerSubjectClaim(ctx, tr.ID, 0, "sub-1")
	assert.ErrorIs(t, err, store.ErrNotFound, "FinishScheduledExecRun must release the subject claim")

	// PurgeOldTriggerHistory deletes fire-owned scheduled_exec_runs first in the same tx.
	_, firesPurged, err := ts.PurgeOldTriggerHistory(ctx, now.Add(time.Hour))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, firesPurged, 2)
	_, err = s.GetScheduledExecRun(ctx, f1.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestScheduledExecOverlapLockKey_PreservesLegacyScheduleHash pins O-a:
// schedule-owned runs keep hashing the raw scheduleID (not "schedule:"+id) so
// mixed-version replicas still serialize on the same Postgres advisory lock
// during a rolling upgrade, while trigger-owned runs hash "trigger:<id>".
func TestScheduledExecOverlapLockKey_PreservesLegacyScheduleHash(t *testing.T) {
	sid := uuid.NewString()
	tid := uuid.NewString()

	key, ok := scheduledExecOverlapLockKey(sid, "schedule:"+sid)
	require.True(t, ok)
	assert.Equal(t, store.StableProjectHash(sid), key, "schedule runs must keep legacy hash(scheduleID)")
	assert.NotEqual(t, store.StableProjectHash("schedule:"+sid), key)

	key, ok = scheduledExecOverlapLockKey("", "trigger:"+tid)
	require.True(t, ok)
	assert.Equal(t, store.StableProjectHash("trigger:"+tid), key, "trigger runs must hash overlapKey")

	_, ok = scheduledExecOverlapLockKey("", "")
	assert.False(t, ok, "one-shot runs without overlap scope must not lock")
}
