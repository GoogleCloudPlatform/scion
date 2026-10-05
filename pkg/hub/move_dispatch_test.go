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
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMoveDispatchFixture(t *testing.T, caps *store.BrokerCapabilities) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID: tid("move-dispatch-broker"), Name: "move-dispatch", Slug: "move-dispatch",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline, Capabilities: caps,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	agent := &store.Agent{
		ID: tid("move-dispatch-agent"), Name: "mover", Slug: "mover", ProjectID: tid("project-1"),
		RuntimeBrokerID: broker.ID, RunID: "run-1",
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	return d, client, agent
}

// A localOnly delete goes to a broker advertising AgentMove as delete
// files, never the branch, localOnly.
func TestDispatchAgentDeleteLocalOnly_SendsLocalOnly(t *testing.T) {
	d, client, agent := newMoveDispatchFixture(t, &store.BrokerCapabilities{AgentMove: true})
	require.NoError(t, d.DispatchAgentDeleteLocalOnly(context.Background(), agent))
	require.True(t, client.deleteCalled)
	assert.True(t, client.lastDeleteOpts.localOnly)
	assert.True(t, client.lastDeleteOpts.deleteFiles)
	assert.False(t, client.lastDeleteOpts.removeBranch)
	assert.Equal(t, "run-1", client.lastDeleteOpts.runID)
}

// A broker without AgentMove would ignore localOnly and delete the shared
// workspace, so nothing is sent to it.
func TestDispatchAgentDeleteLocalOnly_RefusesBrokerWithoutAgentMove(t *testing.T) {
	for name, caps := range map[string]*store.BrokerCapabilities{
		"no capabilities": nil,
		"agentMove false": {Reprovision: true},
	} {
		t.Run(name, func(t *testing.T) {
			d, client, agent := newMoveDispatchFixture(t, caps)
			err := d.DispatchAgentDeleteLocalOnly(context.Background(), agent)
			require.True(t, errors.Is(err, errBrokerLacksAgentMove), "err = %v", err)
			assert.False(t, client.deleteCalled, "no delete may reach a broker without AgentMove")
		})
	}
}

// The move's provision carries the expected NFS workspace to the broker.
func TestDispatchAgentProvisionForMove_CarriesExpectedWorkspace(t *testing.T) {
	d, client, agent := newMoveDispatchFixture(t, &store.BrokerCapabilities{AgentMove: true})
	require.NoError(t, d.DispatchAgentProvisionForMove(context.Background(), agent, "agent-dir"))
	require.NotNil(t, client.lastCreateReq)
	assert.True(t, client.lastCreateReq.ProvisionOnly)
	assert.False(t, client.lastCreateReq.Reprovision)
	assert.Equal(t, "agent-dir", client.lastCreateReq.ExpectExistingNFSWorkspace)

	err := d.DispatchAgentProvisionForMove(context.Background(), agent, "")
	require.Error(t, err)
}

// The localOnly query parameter is rendered only when set.
func TestDeleteAgentQuery_LocalOnly(t *testing.T) {
	assert.Contains(t, deleteAgentQuery(context.Background(), "p", DeleteAgentOptions{DeleteFiles: true, LocalOnly: true}), "&localOnly=true")
	assert.False(t, strings.Contains(deleteAgentQuery(context.Background(), "p", DeleteAgentOptions{DeleteFiles: true}), "localOnly"))
}
