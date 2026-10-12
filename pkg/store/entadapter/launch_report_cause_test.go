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

// This file covers the launch_cause column (ptone/scion#4666): a failed
// report's cause is stored with its launch_error, and cleared wherever
// launch_error is cleared.
package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testLaunchCause = "runtime_forbidden"

// failTestLaunchWithCause begins a create launch and ends it with a failed
// runtime_error report carrying testLaunchCause.
func failTestLaunchWithCause(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string) *store.Agent {
	t.Helper()
	a, launchID := beginTestLaunch(t, ctx, s, projectID, slug)
	_, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "launching", Message: "Failed to create agent", ErrorCode: "runtime_error", Cause: testLaunchCause,
	})
	require.NoError(t, err)
	require.Equal(t, testLaunchCause, updated.LaunchCause)
	return &updated
}

func TestReport_Cause_FailedPreRunning(t *testing.T) {
	cases := []struct {
		name  string
		cause string
	}{
		{"with cause", testLaunchCause},
		{"without cause", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			a, launchID := beginTestLaunch(t, ctx, s, projectID, "cause-pre-running")

			answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
				Step: "launching", Message: "Failed to create agent", ErrorCode: "runtime_error", Cause: tc.cause,
			})
			require.NoError(t, err)
			assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
			assert.Equal(t, "runtime_error", updated.LaunchError)
			assert.Equal(t, tc.cause, updated.LaunchCause)

			persisted, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.cause, persisted.LaunchCause, "the cause must be committed")
		})
	}
}

// TestReport_Cause_ErrorByAnotherWriterStoresCause: the phase=error branch
// stores the cause with the report's code.
func TestReport_Cause_ErrorByAnotherWriterStoresCause(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "cause-error-writer")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "error"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "launching", Message: "Failed to create agent", ErrorCode: "runtime_error", Cause: testLaunchCause,
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Equal(t, "runtime_error", updated.LaunchError)
	assert.Equal(t, testLaunchCause, updated.LaunchCause)
}

// TestReport_Cause_RefineAfterReapStoresCause: a failed report confirming
// a launch the reaper already timed out refines launch_error, and stores
// the cause with it.
func TestReport_Cause_RefineAfterReapStoresCause(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "cause-refine-after-reap")

	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonTimedOut))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "error"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "launching", Message: "Failed to create agent", ErrorCode: "runtime_error", Cause: testLaunchCause,
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)

	persisted, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "runtime_error", persisted.LaunchError)
	assert.Equal(t, testLaunchCause, persisted.LaunchCause)
}

// TestBeginLaunch_ClearsLaunchCause: a new launch starts with no cause.
func TestBeginLaunch_ClearsLaunchCause(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	failed := failTestLaunchWithCause(t, ctx, s, projectID, "cause-begin-clears")

	// BeginLaunch takes a created or provisioning row.
	failed.Phase = "created"
	require.NoError(t, s.UpdateAgent(ctx, failed))
	_, err := s.BeginLaunch(ctx, failed.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	after, err := s.GetAgent(ctx, failed.ID)
	require.NoError(t, err)
	assert.Equal(t, "", after.LaunchError)
	assert.Equal(t, "", after.LaunchCause, "a new launch must not carry the previous launch's cause")
}

// TestUpdateAgent_RunningClearsLaunchCause and
// TestUpdateAgentStatus_RunningClearsLaunchCause: a phase=running write
// clears the cause with launch_error.
func TestUpdateAgent_RunningClearsLaunchCause(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	failed := failTestLaunchWithCause(t, ctx, s, projectID, "cause-update-running")

	failed.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, failed))

	after, err := s.GetAgent(ctx, failed.ID)
	require.NoError(t, err)
	assert.Equal(t, "", after.LaunchError)
	assert.Equal(t, "", after.LaunchCause)
}

func TestUpdateAgentStatus_RunningClearsLaunchCause(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	failed := failTestLaunchWithCause(t, ctx, s, projectID, "cause-status-running")

	require.NoError(t, s.UpdateAgentStatus(ctx, failed.ID, store.AgentStatusUpdate{Phase: "running"}))

	after, err := s.GetAgent(ctx, failed.ID)
	require.NoError(t, err)
	assert.Equal(t, "", after.LaunchError)
	assert.Equal(t, "", after.LaunchCause)
}
