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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A succeeded report's message (the start's notice, for example the repository
// git config keys left out of the agent's private copy) becomes the agent's
// status message. A succeeded report without one leaves the message alone.
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
