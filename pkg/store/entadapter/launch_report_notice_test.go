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

// This file covers design t1-async-create-v11.md §6 test case H-1 (the
// report answer function), one case per branch of §3.7.
package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A succeeded report's message (the start's notice, for example the repository
// git config keys left out of the agent's private copy) becomes the agent's
// status message. A succeeded report without one leaves an empty message empty.
func TestReport_SucceededMessageBecomesAgentMessage(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	const notice = "agent git config: left out core.editor from the agent's copy of .git/config; settings that name a command or a hook location are never given to agents"

	a, launchID := beginTestLaunch(t, ctx, s, projectID, "notice-succeeded")
	_, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded, Message: notice,
	})
	require.NoError(t, err)
	assert.Equal(t, "running", updated.Phase)
	assert.Equal(t, notice, updated.Message)

	b, launchID2 := beginTestLaunch(t, ctx, s, projectID, "notice-none")
	before := b.Message
	_, updated2, err := s.ApplyLaunchReport(ctx, b.ID, b.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID2, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	assert.Equal(t, "running", updated2.Phase)
	assert.Equal(t, before, updated2.Message)
}

// A succeeded report that arrives after the agent already reports running (a late or resent report) does not
// replace the agent's own status message with its notice.
func TestReport_LateSucceededDoesNotReplaceRunningMessage(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "notice-late")
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPhase("running").SetMessage("working on the task").Save(ctx)
	require.NoError(t, err)

	_, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		Message: "agent git config: left out core.editor from the agent's copy of .git/config",
	})
	require.NoError(t, err)
	assert.Equal(t, "running", updated.Phase)
	assert.Equal(t, "working on the task", updated.Message, "a succeeded report on a running agent must not replace its status message")
}

// A succeeded report without a message on a pre-running agent (the first
// running write of a launch) clears any stale error or launch-progress message
// left on the row from an earlier failed start or launch step (ptone/scion#9889).
func TestReport_SucceededWithoutMessageClearsStaleErrorMessage(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	const staleErr = "agent start refused: the configured metadata protection could not be set up in this container (metadata_protection_unavailable)."

	a := makeAgent(projectID, "stale-err-async-start")
	a.Phase = "error"
	a.Message = staleErr
	a.RuntimeBrokerID = "broker-1"
	require.NoError(t, s.CreateAgent(ctx, a))

	// BeginStartLaunch expects phase=starting (written by beginStartDispatch).
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Phase:   "starting",
		IfPhase: "error",
	}))
	launchID, err := s.BeginStartLaunch(ctx, a.ID, store.LaunchKindStart, 5*time.Minute, store.BeginLaunchOptions{
		PriorPhase: "error",
	})
	require.NoError(t, err)

	// Confirm the stale error is still on the row while starting (for rollback).
	starting, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, staleErr, starting.Message)

	// Launch succeeds with no notice message: stale error must be cleared.
	_, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	assert.Equal(t, "running", updated.Phase)
	assert.Empty(t, updated.Message, "stale launch error must be cleared when a subsequent launch succeeds")

	persisted, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, persisted.Message)
}
