//go:build !no_sqlite

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

package hub

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var recreateCaps = &store.BrokerCapabilities{AgentMove: true, AgentMoveRecreate: true}

// A forced move's provision carries the fresh-agent-dir expectation to a
// broker advertising AgentMoveRecreate, and is never sent to one without
// it (nor, as before, to one without AgentMove).
func TestDispatchAgentProvisionForMove_FreshAgentDirNeedsRecreateCapability(t *testing.T) {
	t.Run("capable broker", func(t *testing.T) {
		d, client, agent := newMoveDispatchFixture(t, recreateCaps)
		require.NoError(t, d.DispatchAgentProvisionForMove(context.Background(), agent, "fresh-agent-dir"))
		require.NotNil(t, client.lastCreateReq)
		assert.True(t, client.lastCreateReq.ProvisionOnly)
		assert.False(t, client.lastCreateReq.Reprovision)
		assert.Equal(t, "fresh-agent-dir", client.lastCreateReq.ExpectExistingNFSWorkspace)
	})
	for name, caps := range map[string]*store.BrokerCapabilities{
		"agentMove only":         {AgentMove: true},
		"agentMoveRecreate only": {AgentMoveRecreate: true},
		"no capabilities":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			d, client, agent := newMoveDispatchFixture(t, caps)
			err := d.DispatchAgentProvisionForMove(context.Background(), agent, "fresh-agent-dir")
			require.Error(t, err)
			assert.True(t, errors.Is(err, errBrokerLacksAgentMoveRecreate) || errors.Is(err, errBrokerLacksAgentMove), "err = %v", err)
			assert.Nil(t, client.lastCreateReq, "nothing may be sent")
			assert.True(t, moveTargetUntouched(err), "a refusal before sending leaves the target untouched")
		})
	}
	t.Run("export expectations need only AgentMove", func(t *testing.T) {
		d, client, agent := newMoveDispatchFixture(t, &store.BrokerCapabilities{AgentMove: true})
		require.NoError(t, d.DispatchAgentProvisionForMove(context.Background(), agent, "agent-dir"))
		require.NotNil(t, client.lastCreateReq)
	})
}

// A forced move's rollback delete removes the agent's files on the broker,
// never its branch, never localOnly, always naming the project and naming
// the run only when the agent row carries one.
func TestDispatchAgentDeleteRecreated_SendsFilesDelete(t *testing.T) {
	for _, runID := range []string{"run-dst", ""} {
		t.Run("run="+runID, func(t *testing.T) {
			d, client, agent := newMoveDispatchFixture(t, recreateCaps)
			agent.RunID = runID
			require.NoError(t, d.DispatchAgentDeleteRecreated(context.Background(), agent))
			require.True(t, client.deleteCalled)
			assert.True(t, client.lastDeleteOpts.deleteFiles)
			assert.False(t, client.lastDeleteOpts.removeBranch, "the branch lives on the remote and is never removed")
			assert.False(t, client.lastDeleteOpts.localOnly)
			assert.Equal(t, runID, client.lastDeleteOpts.runID)
			assert.Equal(t, agent.ProjectID, client.lastDeleteOpts.projectID)
			assert.Equal(t, agent.Slug, client.lastAgentID)
		})
	}
}

// The rollback delete is never sent to a broker without AgentMoveRecreate.
func TestDispatchAgentDeleteRecreated_RefusesBrokerWithoutCapability(t *testing.T) {
	for name, caps := range map[string]*store.BrokerCapabilities{
		"agentMove only":  {AgentMove: true},
		"no capabilities": nil,
	} {
		t.Run(name, func(t *testing.T) {
			d, client, agent := newMoveDispatchFixture(t, caps)
			err := d.DispatchAgentDeleteRecreated(context.Background(), agent)
			require.Error(t, err)
			assert.False(t, client.deleteCalled, "no delete may reach the broker")
		})
	}
}
