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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureRunStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	runErr := fn()
	require.NoError(t, w.Close())
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	require.NoError(t, r.Close())
	return buf.String(), runErr
}

func useFastScheduleRunPolling(t *testing.T) {
	t.Helper()
	origInit := scheduleRunInitialPollDelay
	origInterval := scheduleRunPollInterval
	scheduleRunInitialPollDelay = 5 * time.Millisecond
	scheduleRunPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		scheduleRunInitialPollDelay = origInit
		scheduleRunPollInterval = origInterval
	})
}

func TestScheduleRunViaHub_StandardMessageSchedule(t *testing.T) {
	saveScheduleFlags(t)
	outputFormat = ""

	const schedID = "11111111-2222-3333-4444-555555555555"
	const evtID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	firedAt := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	var runCalled atomic.Int32
	hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/schedules":
			_ = json.NewEncoder(w).Encode(hubclient.ListSchedulesResponse{
				Schedules: []hubclient.Schedule{{ID: schedID, Name: "standup", EventType: "message", Status: "active"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/schedules/"+schedID+"/run":
			runCalled.Add(1)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(hubclient.ScheduledEvent{
				ID:         evtID,
				ProjectID:  "proj-1",
				ScheduleID: schedID,
				EventType:  "message",
				Status:     "fired",
				Manual:     true,
				FireAt:     firedAt,
				FiredAt:    &firedAt,
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// Prefix resolution + text output
	out, err := captureRunStdout(t, func() error {
		return scheduleRunViaHub(context.Background(), hubCtx, "proj-1", "11111111")
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), runCalled.Load())
	assert.Contains(t, out, "Schedule "+schedID+" run triggered.")
	assert.Contains(t, out, "Event ID:   "+evtID)
	assert.Contains(t, out, "Type:       message")
	assert.Contains(t, out, "Status:     fired")

	// Full UUID + JSON output
	outputFormat = "json"
	jsonOut, err := captureRunStdout(t, func() error {
		return scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
	})
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &decoded))
	assert.Equal(t, evtID, decoded["id"])
	assert.Equal(t, true, decoded["manual"])
	assert.Equal(t, "fired", decoded["status"])
	_, hasExecOutput := decoded["execOutput"]
	assert.False(t, hasExecOutput)
}

func TestScheduleRunViaHub_ExecScheduleWithCapturedOutput(t *testing.T) {
	saveScheduleFlags(t)
	useFastScheduleRunPolling(t)
	outputFormat = ""

	const schedID = "22222222-2222-3333-4444-555555555555"
	const evtID = "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"
	firedAt := time.Date(2026, 11, 25, 12, 5, 0, 0, time.UTC)
	finishedAt := firedAt.Add(250 * time.Millisecond)
	exitZero := 0
	durMS := int64(250)
	outBytes := int64(18)

	var pollCount atomic.Int32
	var outputFetched atomic.Int32

	hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/schedules/"+schedID+"/run":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(hubclient.ScheduledEvent{
				ID:         evtID,
				ProjectID:  "proj-1",
				ScheduleID: schedID,
				EventType:  "exec",
				Status:     "fired",
				Manual:     true,
				FireAt:     firedAt,
				FiredAt:    &firedAt,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events/"+evtID:
			n := pollCount.Add(1)
			evt := hubclient.ScheduledEvent{
				ID:         evtID,
				ProjectID:  "proj-1",
				ScheduleID: schedID,
				EventType:  "exec",
				Status:     "fired",
				Manual:     true,
				FireAt:     firedAt,
				FiredAt:    &firedAt,
			}
			if n == 1 {
				evt.ExecSummary = &hubclient.ExecSummary{
					Outcome:     "running",
					CaptureMode: "json",
					StartedAt:   firedAt,
				}
			} else {
				evt.ExecSummary = &hubclient.ExecSummary{
					Outcome:      "ok",
					ExitCode:     &exitZero,
					DurationMS:   &durMS,
					OutputBytes:  &outBytes,
					CaptureMode:  "json",
					OutputStatus: "captured",
					StartedAt:    firedAt,
					FinishedAt:   &finishedAt,
				}
			}
			_ = json.NewEncoder(w).Encode(evt)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events/"+evtID+"/output":
			outputFetched.Add(1)
			_ = json.NewEncoder(w).Encode(hubclient.ExecOutput{
				CaptureMode:  "json",
				OutputStatus: "captured",
				Output:       json.RawMessage(`{"healthy":true}`),
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	out, err := captureRunStdout(t, func() error {
		return scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
	})
	require.NoError(t, err)
	assert.Equal(t, int32(2), pollCount.Load())
	assert.Equal(t, int32(1), outputFetched.Load())
	assert.Contains(t, out, "Outcome:    ok")
	assert.Contains(t, out, "Exit Code:  0")
	assert.Contains(t, out, "Duration:   250ms")
	assert.Contains(t, out, "Capture:    json")
	assert.Contains(t, out, "Captured Output:")
	assert.Contains(t, out, `"healthy": true`)

	// Verify --json includes both event fields and execOutput.
	pollCount.Store(0)
	outputFormat = "json"
	jsonOut, err := captureRunStdout(t, func() error {
		return scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
	})
	require.NoError(t, err)
	var decoded struct {
		ID          string                 `json:"id"`
		Manual      bool                   `json:"manual"`
		ExecSummary *hubclient.ExecSummary `json:"execSummary"`
		ExecOutput  *hubclient.ExecOutput  `json:"execOutput"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &decoded))
	assert.Equal(t, evtID, decoded.ID)
	assert.True(t, decoded.Manual)
	require.NotNil(t, decoded.ExecSummary)
	assert.Equal(t, "ok", decoded.ExecSummary.Outcome)
	require.NotNil(t, decoded.ExecOutput)
	assert.Equal(t, "json", decoded.ExecOutput.CaptureMode)
	assert.JSONEq(t, `{"healthy":true}`, string(decoded.ExecOutput.Output))
}

func TestScheduleRunViaHub_ExecOutputForbiddenStillPrintsStatus(t *testing.T) {
	saveScheduleFlags(t)
	useFastScheduleRunPolling(t)
	outputFormat = ""

	const schedID = "33333333-2222-3333-4444-555555555555"
	const evtID = "cccccccc-bbbb-cccc-dddd-eeeeeeeeeeee"
	firedAt := time.Date(2026, 11, 25, 12, 10, 0, 0, time.UTC)
	exitZero := 0

	hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/schedules/"+schedID+"/run":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(hubclient.ScheduledEvent{
				ID:         evtID,
				ProjectID:  "proj-1",
				ScheduleID: schedID,
				EventType:  "exec",
				Status:     "fired",
				Manual:     true,
				FireAt:     firedAt,
				FiredAt:    &firedAt,
				ExecSummary: &hubclient.ExecSummary{
					Outcome:      "ok",
					ExitCode:     &exitZero,
					CaptureMode:  "tail",
					OutputStatus: "captured",
					StartedAt:    firedAt,
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events/"+evtID+"/output":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"agent.attach required"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	out, err := captureRunStdout(t, func() error {
		return scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Outcome:    ok")
	assert.Contains(t, out, "requires attach permission")
}

func TestScheduleRunViaHub_ExecWithoutCaptureSkipsOutputFetch(t *testing.T) {
	saveScheduleFlags(t)
	useFastScheduleRunPolling(t)
	outputFormat = ""

	const schedID = "44444444-2222-3333-4444-555555555555"
	const evtID = "dddddddd-bbbb-cccc-dddd-eeeeeeeeeeee"
	firedAt := time.Date(2026, 11, 25, 12, 15, 0, 0, time.UTC)
	exitOne := 1

	var outputHits atomic.Int32
	hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/schedules/"+schedID+"/run":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(hubclient.ScheduledEvent{
				ID:         evtID,
				ProjectID:  "proj-1",
				ScheduleID: schedID,
				EventType:  "exec",
				Status:     "fired",
				Manual:     true,
				FireAt:     firedAt,
				FiredAt:    &firedAt,
				ExecSummary: &hubclient.ExecSummary{
					Outcome:   "nonzero",
					ExitCode:  &exitOne,
					StartedAt: firedAt,
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events/"+evtID+"/output":
			outputHits.Add(1)
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	out, err := captureRunStdout(t, func() error {
		return scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
	})
	require.NoError(t, err)
	assert.Equal(t, int32(0), outputHits.Load())
	assert.Contains(t, out, "Outcome:    nonzero")
	assert.Contains(t, out, "Exit Code:  1")
}

func TestScheduleRunViaHub_AuthorizationAndConflictErrors(t *testing.T) {
	saveScheduleFlags(t)
	outputFormat = ""

	const schedID = "55555555-2222-3333-4444-555555555555"

	t.Run("user 403 forbidden surfaces hub authorization error", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"missing required permission scheduled_event.update"}}`))
		}, hubsync.CredentialKindOAuth)

		err := scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run schedule")
		assert.Contains(t, err.Error(), "scheduled_event.update")
		assert.NotContains(t, err.Error(), "agent callers can only access")
	})

	t.Run("agent 403 insufficient permissions appends caller-scope hint", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"Insufficient permissions"}}`))
		}, hubsync.CredentialKindAgentToken)

		err := scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent callers can only access schedules and events they hold")
	})

	t.Run("409 conflict when manual run already in progress", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"a manual run is already in progress for this schedule"}}`))
		}, hubsync.CredentialKindOAuth)

		err := scheduleRunViaHub(context.Background(), hubCtx, "proj-1", schedID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a manual run is already in progress")
	})
}

func TestWindowsGate_ScheduleRunGated(t *testing.T) {
	root, ran := gateTestTree(t)
	applyPlatformRestrictions(root, "windows")
	root.SetArgs([]string{"schedule", "run", "11111111-2222-3333-4444-555555555555"})

	err := root.Execute()
	require.Error(t, err)
	assert.True(t, errors.Is(err, errUnsupportedOnWindows), "got %v", err)
	assert.Empty(t, *ran)
}
