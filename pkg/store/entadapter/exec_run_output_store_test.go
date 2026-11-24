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
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecRunOutput_CASWritePurgeAndBounds verifies A12, A14, and the store
// length bound on exec_run_outputs.
func TestExecRunOutput_CASWritePurgeAndBounds(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduleStore(t)
	projectID := uuid.NewString()

	// 1. Oversize output (> 64 KiB) is rejected with ErrInvalidInput before CAS.
	evtOver := newExecEvent(t, s, projectID, store.ScheduledEventFired, time.Now().Add(-time.Minute))
	runOver := newExecRun(evtOver)
	runOver.CaptureMode = "json"
	require.NoError(t, s.CreateScheduledExecRun(ctx, runOver))
	won, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:      evtOver.ID,
		Outcome:      store.ScheduledExecOutcomeOK,
		OutputStatus: "captured",
		Output:       bytes.Repeat([]byte("a"), store.MaxExecRunOutputBytes+1),
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	assert.False(t, won)

	// 2. CAS winner writes OutputStatus and exec_run_outputs row; CAS loser writes nothing.
	evt := newExecEvent(t, s, projectID, store.ScheduledEventFired, time.Now().Add(-2*time.Hour))
	run := newExecRun(evt)
	run.CaptureMode = "json"
	require.NoError(t, s.CreateScheduledExecRun(ctx, run))

	gotRun, err := s.GetScheduledExecRun(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, "json", gotRun.CaptureMode)
	assert.Equal(t, "", gotRun.OutputStatus)

	won1, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:      evt.ID,
		Outcome:      store.ScheduledExecOutcomeOK,
		OutputStatus: "captured",
		Output:       []byte(`{"winner":1}`),
	})
	require.NoError(t, err)
	require.True(t, won1)

	won2, err := s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:      evt.ID,
		Outcome:      store.ScheduledExecOutcomeNonzero,
		OutputStatus: "lost",
		Output:       []byte(`{"loser":2}`),
	})
	require.NoError(t, err)
	assert.False(t, won2)

	gotRun, err = s.GetScheduledExecRun(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledExecOutcomeOK, gotRun.Outcome)
	assert.Equal(t, "captured", gotRun.OutputStatus)

	content, err := s.GetScheduledExecRunOutput(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, `{"winner":1}`, string(content))

	// Non-existent run returns ErrNotFound.
	_, err = s.GetScheduledExecRunOutput(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)

	// 3. PurgeOldScheduledEvents removes exec_run_outputs along with runs in the same tx.
	purged, err := s.PurgeOldScheduledEvents(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, purged)
	_, err = s.GetScheduledExecRunOutput(ctx, evt.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// 4. DeleteSchedulesByProject removes exec_run_outputs for the project.
	evtDel := newExecEvent(t, s, projectID, store.ScheduledEventFired, time.Now())
	runDel := newExecRun(evtDel)
	runDel.CaptureMode = "json"
	require.NoError(t, s.CreateScheduledExecRun(ctx, runDel))
	won, err = s.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:      evtDel.ID,
		Outcome:      store.ScheduledExecOutcomeOK,
		OutputStatus: "captured",
		Output:       []byte(`{"proj":"del"}`),
	})
	require.NoError(t, err)
	require.True(t, won)

	_, err = s.DeleteSchedulesByProject(ctx, projectID)
	require.NoError(t, err)
	_, err = s.GetScheduledExecRunOutput(ctx, evtDel.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}
