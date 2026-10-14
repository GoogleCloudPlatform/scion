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

// This file covers ptone/scion#1956's async-create path: a `failed`
// launch report the Hub actually applies must revoke the credential minted
// for that create, but a `failed` report that does not change the agent's
// outcome (it already reached running, or the launch is ending for an
// unrelated stop/suspend reason) must not.
package hub

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentLaunchReport_FailedAppliedRevokesCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-revoke-project"), Slug: "lr-revoke-project", Name: "LR Revoke Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-revoke-agent"), Slug: "lr-revoke-agent", Name: "LR Revoke Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	insertTestAgentCredential(t, s, agent.ID, project.ID, "lr-revoke-jti")

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "failed", ErrorCode: "boom",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	cred := getTestAgentCredential(t, s, "lr-revoke-jti")
	require.NotNil(t, cred.RevokedAt, "a failed launch report the store applies must revoke the agent's credential")
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
}

func TestAgentLaunchReport_SucceededDoesNotRevokeCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-nosuccess-revoke-project"), Slug: "lr-nosuccess-revoke-project", Name: "LR No-Revoke Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-nosuccess-revoke-agent"), Slug: "lr-nosuccess-revoke-agent", Name: "LR No-Revoke Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	insertTestAgentCredential(t, s, agent.ID, project.ID, "lr-nosuccess-jti")

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "succeeded",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	cred := getTestAgentCredential(t, s, "lr-nosuccess-jti")
	assert.Nil(t, cred.RevokedAt, "a succeeded launch report must not revoke the agent's credential")
}

// TestAgentLaunchReport_FailedDuringStopDoesNotRevokeCredential covers the
// applyLaunchReportActive Phase=Stopped/Stopping/Suspended branch: a stop or
// suspend landing while the launch is still active ends the launch with a
// Conflict/StaleLaunch answer regardless of what the broker's report says,
// because the agent's outcome is already decided by the stop/suspend, not by
// this report. That answer is never Result==Applied, so the handler's
// revoke condition must not fire — stop and suspend already revoke through
// their own dedicated paths (DispatchAgentDelete's handler,
// handlers_agent_lifecycle.go's suspendAgent), and must not be duplicated or
// pre-empted here with the wrong reason.
func TestAgentLaunchReport_FailedDuringStopDoesNotRevokeCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-stopped-revoke-project"), Slug: "lr-stopped-revoke-project", Name: "LR Stopped Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-stopped-revoke-agent"), Slug: "lr-stopped-revoke-agent", Name: "LR Stopped Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	insertTestAgentCredential(t, s, agent.ID, project.ID, "lr-stopped-jti")

	// A stop landing mid-launch: Phase=Stopped does not go through
	// UpdateAgent's phase=running launch-ending invariant (that only applies
	// to a running write), so this constructs the same concurrent "stop
	// during an active launch" state the Phase=Stopped branch handles,
	// without needing to go through the full stop dispatch.
	fresh, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	fresh.Phase = string(state.PhaseStopped)
	require.NoError(t, s.UpdateAgent(ctx, fresh))

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "failed", ErrorCode: "boom",
	})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	cred := getTestAgentCredential(t, s, "lr-stopped-jti")
	assert.Nil(t, cred.RevokedAt, "a failed report arriving during a stop must not revoke — the stop/suspend path owns that")
}

// TestAgentLaunchReport_FailedAfterRunningDoesNotRevokeCredential covers the
// applyLaunchReportEnded LaunchEndReasonSucceeded branch: once a launch has
// already ended successfully (the agent reached running), any further
// report — including a stray `failed` one arriving late from the broker —
// is answered Result=Completed with no row change. The handler's revoke
// condition (State==failed && HTTPStatus==0 && Result==Applied) must not
// fire here: the agent did start, and its credential is in active use.
func TestAgentLaunchReport_FailedAfterRunningDoesNotRevokeCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-afterrun-project"), Slug: "lr-afterrun-project", Name: "LR After-Run Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-afterrun-agent"), Slug: "lr-afterrun-agent", Name: "LR After-Run Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	insertTestAgentCredential(t, s, agent.ID, project.ID, "lr-afterrun-jti")

	// The launch succeeds: phase moves to running and the launch ends with
	// reason succeeded.
	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "succeeded",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A late, stray failed report for the same (now-ended) launch arrives
	// afterward.
	rec = postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "failed", ErrorCode: "boom",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	cred := getTestAgentCredential(t, s, "lr-afterrun-jti")
	assert.Nil(t, cred.RevokedAt, "a failed report once the agent already reached running must not revoke — the agent did start")
}
