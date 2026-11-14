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

package entadapter

import (
	"context"
	"sort"
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

func TestScheduledExecRun_ListUnfinishedPages(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	project := uuid.NewString()
	var unfinished []string
	for i := 0; i < 7; i++ {
		evt := newExecEvent(t, s, project, store.ScheduledEventFired, time.Now())
		require.NoError(t, s.CreateScheduledExecRun(ctx, newExecRun(evt)))
		if i%3 == 0 {
			_, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{EventID: evt.ID, Outcome: store.ScheduledExecOutcomeOK})
			require.NoError(t, err)
			continue
		}
		unfinished = append(unfinished, evt.ID)
	}
	sort.Strings(unfinished)
	var got []string
	after := ""
	for {
		page, err := s.ListUnfinishedScheduledExecRuns(ctx, after, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		require.LessOrEqual(t, len(page), 2)
		for _, r := range page {
			assert.Nil(t, r.FinishedAt)
			got = append(got, r.EventID)
		}
		after = page[len(page)-1].EventID
	}
	assert.Equal(t, unfinished, got)

	byEvent, err := s.ListScheduledExecRunsByEvent(ctx, append([]string{"not-a-uuid", uuid.NewString()}, unfinished...))
	require.NoError(t, err)
	assert.Len(t, byEvent, len(unfinished))
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
