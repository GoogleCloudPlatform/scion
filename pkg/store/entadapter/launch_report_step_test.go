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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A failed launch report records the step the launch failed at as the
// launch's step, so the CLI and web can show where it failed (design wave 3
// E-d and E-T4, ptone/scion#4670). Each test runs for a create launch and
// for both start kinds, whose failure ends follow startLaunchFailureEnd.

// stepTestLaunch is one launch kind the failed-report step tests cover.
type stepTestLaunch struct {
	name        string
	kind, prior string
}

var stepTestLaunches = []stepTestLaunch{
	{name: "create", kind: store.LaunchKindCreate},
	{name: "start", kind: store.LaunchKindStart, prior: "stopped"},
	{name: "restart", kind: store.LaunchKindRestart, prior: "running"},
}

// beginStepTestLaunch begins tc's launch and records a progress step,
// scheduling, as a broker does before the launch fails.
func beginStepTestLaunch(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string, tc stepTestLaunch) (*store.Agent, string) {
	t.Helper()
	var a *store.Agent
	var launchID string
	if tc.kind == store.LaunchKindCreate {
		a, launchID = beginTestLaunch(t, ctx, s, projectID, slug)
	} else {
		a, launchID = beginStartKindLaunch(t, ctx, s, projectID, slug, tc.kind, tc.prior, "")
	}
	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
		Step: api.LaunchStepScheduling, Message: "waiting to be scheduled",
	})
	require.NoError(t, err)
	require.Zero(t, answer.HTTPStatus, "setup progress answered %+v", answer)
	require.Equal(t, api.LaunchStepScheduling, updated.LaunchStep, "setup: the progress report records its step")
	return a, launchID
}

func failedStepReport(launchID, step string) store.LaunchReport {
	return store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: step, Message: "the agent image could not be pulled", ErrorCode: api.LaunchCodeImagePullFailed,
	}
}

// assertLaunchStep checks the step on the persisted row and in the client
// launch view.
func assertLaunchStep(t *testing.T, ctx context.Context, s *AgentStore, agentID, want string) {
	t.Helper()
	got, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, want, got.LaunchStep, "launch_step")
	view := store.ComputeAgentLaunch(got, time.Now())
	require.NotNil(t, view)
	assert.Equal(t, want, view.Step, "launch.step")
}

// TestReport_FailedPreRunningWritesLaunchStep: the broker's failed report on
// a launching row writes its step.
func TestReport_FailedPreRunningWritesLaunchStep(t *testing.T) {
	ctx := context.Background()
	for _, tc := range stepTestLaunches {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStepTestLaunch(t, ctx, s, projectID, "step-prerunning", tc)

			answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedStepReport(launchID, api.LaunchStepContainerCreate))
			require.NoError(t, err)
			require.Equal(t, store.LaunchReportResultApplied, answer.Result)
			require.Equal(t, store.LaunchEndReasonFailed, updated.LaunchEndReason)
			assert.Equal(t, api.LaunchStepContainerCreate, updated.LaunchStep)
			assertLaunchStep(t, ctx, s, a.ID, api.LaunchStepContainerCreate)
		})
	}
}

// TestReport_FailedWithoutStepKeepsLastStep: a failed report that carries
// no step leaves the last recorded step, the furthest the launch reached.
func TestReport_FailedWithoutStepKeepsLastStep(t *testing.T) {
	ctx := context.Background()
	for _, tc := range stepTestLaunches {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStepTestLaunch(t, ctx, s, projectID, "step-empty", tc)

			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedStepReport(launchID, ""))
			require.NoError(t, err)
			require.Equal(t, store.LaunchReportResultApplied, answer.Result)
			assertLaunchStep(t, ctx, s, a.ID, api.LaunchStepScheduling)
		})
	}
}

// TestReport_ErrorByAnotherWriter_FailedWritesLaunchStep: a failed report on
// a row another writer already moved to error writes its step.
func TestReport_ErrorByAnotherWriter_FailedWritesLaunchStep(t *testing.T) {
	ctx := context.Background()
	for _, tc := range stepTestLaunches {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStepTestLaunch(t, ctx, s, projectID, "step-error-writer", tc)
			setRowPhaseAndMemory(t, ctx, s, a.ID, "error", "")

			answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedStepReport(launchID, api.LaunchStepContainerCreate))
			require.NoError(t, err)
			require.Equal(t, store.LaunchReportResultApplied, answer.Result)
			require.Equal(t, store.LaunchEndReasonFailed, updated.LaunchEndReason)
			assertLaunchStep(t, ctx, s, a.ID, api.LaunchStepContainerCreate)
		})
	}
}

// TestReport_FailedConfirmingReapWritesLaunchStep: a late failed report that
// refines a launch the reaper timed out writes its step with the code and
// message it refines.
func TestReport_FailedConfirmingReapWritesLaunchStep(t *testing.T) {
	ctx := context.Background()
	for _, tc := range stepTestLaunches {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStepTestLaunch(t, ctx, s, projectID, "step-refine", tc)
			endStartLaunchByFailure(t, ctx, s, a, launchID, "reaper_deadline")

			answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedStepReport(launchID, api.LaunchStepContainerCreate))
			require.NoError(t, err)
			require.Equal(t, store.LaunchReportResultApplied, answer.Result, "the late failed report refines the reaped launch")
			require.Equal(t, api.LaunchCodeImagePullFailed, updated.LaunchError)
			require.Equal(t, store.LaunchEndReasonTimedOut, updated.LaunchEndReason)
			assertLaunchStep(t, ctx, s, a.ID, api.LaunchStepContainerCreate)
		})
	}
}

// TestReport_ErrorByAnotherWriter_NonFailedKeepsLaunchStep: on a row another
// writer moved to error, only a failed report writes its step. A progress
// report there ends the launch (409 stale_launch) and leaves the recorded
// step.
func TestReport_ErrorByAnotherWriter_NonFailedKeepsLaunchStep(t *testing.T) {
	ctx := context.Background()
	for _, tc := range stepTestLaunches {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStepTestLaunch(t, ctx, s, projectID, "step-error-progress", tc)
			setRowPhaseAndMemory(t, ctx, s, a.ID, "error", "")

			answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 2,
				Step: api.LaunchStepContainerCreate, Message: "preparing the container (image pull, volume mounts)",
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusConflict, answer.HTTPStatus)
			require.Equal(t, store.LaunchReportCodeStaleLaunch, answer.Code)
			require.Equal(t, store.LaunchEndReasonFailed, updated.LaunchEndReason)
			assertLaunchStep(t, ctx, s, a.ID, api.LaunchStepScheduling)
		})
	}
}
