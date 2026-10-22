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
	"net/http"
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
		require.NoError(t, d.DispatchAgentProvisionForMove(context.Background(), agent, "fresh-agent-dir", "reincarnation-1"))
		require.NotNil(t, client.lastCreateReq)
		assert.True(t, client.lastCreateReq.ProvisionOnly)
		assert.False(t, client.lastCreateReq.Reprovision)
		assert.Equal(t, "fresh-agent-dir", client.lastCreateReq.ExpectExistingNFSWorkspace)
		assert.Equal(t, "reincarnation-1", client.lastCreateReq.MoveReincarnationID, "the provision carries the move's reincarnation ID")
	})
	t.Run("the reincarnation ID goes with, and only with, fresh-agent-dir", func(t *testing.T) {
		d, client, agent := newMoveDispatchFixture(t, recreateCaps)
		require.Error(t, d.DispatchAgentProvisionForMove(context.Background(), agent, "fresh-agent-dir", ""))
		require.Error(t, d.DispatchAgentProvisionForMove(context.Background(), agent, "agent-dir", "reincarnation-1"))
		assert.Nil(t, client.lastCreateReq, "nothing may be sent")
	})
	for name, caps := range map[string]*store.BrokerCapabilities{
		"agentMove only":         {AgentMove: true},
		"agentMoveRecreate only": {AgentMoveRecreate: true},
		"no capabilities":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			d, client, agent := newMoveDispatchFixture(t, caps)
			err := d.DispatchAgentProvisionForMove(context.Background(), agent, "fresh-agent-dir", "reincarnation-1")
			require.Error(t, err)
			assert.True(t, errors.Is(err, errBrokerLacksAgentMoveRecreate) || errors.Is(err, errBrokerLacksAgentMove), "err = %v", err)
			assert.Nil(t, client.lastCreateReq, "nothing may be sent")
			assert.True(t, moveTargetUntouched(err), "a refusal before sending leaves the target untouched")
		})
	}
	t.Run("export expectations need only AgentMove", func(t *testing.T) {
		d, client, agent := newMoveDispatchFixture(t, &store.BrokerCapabilities{AgentMove: true})
		require.NoError(t, d.DispatchAgentProvisionForMove(context.Background(), agent, "agent-dir", ""))
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
			require.NoError(t, d.DispatchAgentDeleteRecreated(context.Background(), agent, "reincarnation-1"))
			require.True(t, client.deleteCalled)
			assert.True(t, client.lastDeleteOpts.deleteFiles)
			assert.False(t, client.lastDeleteOpts.removeBranch, "the branch lives on the remote and is never removed")
			assert.False(t, client.lastDeleteOpts.localOnly)
			assert.Equal(t, runID, client.lastDeleteOpts.runID)
			assert.Equal(t, agent.ProjectID, client.lastDeleteOpts.projectID)
			assert.Equal(t, "reincarnation-1", client.lastDeleteOpts.moveReincarnationID, "the delete carries the move's reincarnation ID")
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
			err := d.DispatchAgentDeleteRecreated(context.Background(), agent, "reincarnation-1")
			require.Error(t, err)
			assert.False(t, client.deleteCalled, "no delete may reach the broker")
		})
	}
}

// nilCredTokenGen authorizes and signs an agent token but returns no
// credential, so recording it fails (errAgentTokenRecord) before the
// provision request is sent.
type nilCredTokenGen struct{}

func (nilCredTokenGen) AuthorizeAgentToken(_ context.Context, a *store.Agent) (AgentTokenGrant, error) {
	return AgentTokenGrant{AgentID: a.ID, ProjectID: a.ProjectID}, nil
}

func (nilCredTokenGen) SignAgentToken(AgentTokenGrant, string) (string, *store.AgentCredential, error) {
	return "tok", nil, nil
}

// The send boundary of a forced move's provision, on the real dispatcher: an
// error returned before the request is sent leaves the target untouched (no
// rollback delete) and keeps its own sentinel; an error from the send on
// (a broker error, a transport error) is marked sent, so the rollback
// cleans up, unless it is the target's own not-fresh refusal. Removing the
// mark at the send boundary fails the sent rows.
func TestDispatchProvisionForMove_SendBoundary(t *testing.T) {
	ctx := context.Background()
	const fresh = "fresh-agent-dir"
	forMove := func(d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentProvisionForMove(ctx, a, fresh, "reincarnation-1")
	}
	direct := func(d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.dispatchProvision(ctx, a, "DispatchAgentProvisionForMove", false, fresh, "reincarnation-1")
	}
	withFixture := func(mutate func(d *HTTPAgentDispatcher, c *mockRuntimeBrokerClient, a *store.Agent)) func(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
		return func(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
			d, c, a := newMoveDispatchFixture(t, recreateCaps)
			if mutate != nil {
				mutate(d, c, a)
			}
			return d, c, a
		}
	}
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent)
		call      func(d *HTTPAgentDispatcher, a *store.Agent) error
		untouched bool
		notSent   bool // the request never reached the client
		keeps     func(t *testing.T, err error)
	}{
		{
			name: "placement refusal",
			setup: withFixture(func(_ *HTTPAgentDispatcher, _ *mockRuntimeBrokerClient, a *store.Agent) {
				a.PinnedRuntimeTargetID = "target-1"
			}),
			call: forMove, untouched: true, notSent: true,
			keeps: func(t *testing.T, err error) {
				_, refused := placementRefusalOf(err)
				assert.True(t, refused, "the placement refusal is kept: %v", err)
			},
		},
		{
			name: "missing endpoint",
			setup: withFixture(func(_ *HTTPAgentDispatcher, _ *mockRuntimeBrokerClient, a *store.Agent) {
				a.RuntimeBrokerID = tid("move-dispatch-missing")
			}),
			call: direct, untouched: true, notSent: true,
			keeps: func(t *testing.T, err error) { assert.ErrorIs(t, err, store.ErrNotFound) },
		},
		{
			name: "identity precheck",
			setup: func(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
				d, c, a, _ := precheckDispatcher(t, precheckGSA, false)
				return d, c, a
			},
			call: direct, untouched: true, notSent: true,
			keeps: func(t *testing.T, err error) {
				_, ok := identityMappingDispatchError(err)
				assert.True(t, ok, "the identity refusal is kept: %v", err)
			},
		},
		{
			name: "token record failure",
			setup: withFixture(func(d *HTTPAgentDispatcher, _ *mockRuntimeBrokerClient, _ *store.Agent) {
				d.SetTokenGenerator(nilCredTokenGen{})
			}),
			call: forMove, untouched: true, notSent: true,
			keeps: func(t *testing.T, err error) { assert.ErrorIs(t, err, errAgentTokenRecord) },
		},
		{
			name: "capability store error",
			setup: withFixture(func(_ *HTTPAgentDispatcher, _ *mockRuntimeBrokerClient, a *store.Agent) {
				a.RuntimeBrokerID = tid("move-dispatch-missing")
			}),
			call: forMove, untouched: true, notSent: true,
			keeps: func(t *testing.T, err error) { assert.ErrorIs(t, err, store.ErrNotFound) },
		},
		{
			name: "deferred route",
			setup: withFixture(func(_ *HTTPAgentDispatcher, c *mockRuntimeBrokerClient, _ *store.Agent) {
				c.returnErr = ErrLifecycleDeferred
			}),
			call: forMove, untouched: true,
			keeps: func(t *testing.T, err error) { assert.ErrorIs(t, err, ErrLifecycleDeferred) },
		},
		{
			name: "broker 500 after the send",
			setup: withFixture(func(_ *HTTPAgentDispatcher, c *mockRuntimeBrokerClient, _ *store.Agent) {
				c.returnErr = &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: `{"error":{"code":"runtime_error","message":"Failed to provision agent"}}`}
			}),
			call: forMove, untouched: false,
			keeps: func(t *testing.T, err error) {
				var se *brokerStatusError
				assert.True(t, errors.As(err, &se), "the broker status is kept: %v", err)
			},
		},
		{
			name: "transport error after the request was built",
			setup: withFixture(func(_ *HTTPAgentDispatcher, c *mockRuntimeBrokerClient, _ *store.Agent) {
				c.returnErr = errors.New("dial tcp 10.0.0.9:9800: connection refused")
			}),
			call: forMove, untouched: false,
			keeps: func(t *testing.T, err error) { assert.Contains(t, err.Error(), "connection refused") },
		},
		{
			name: "target refused, not fresh",
			setup: withFixture(func(_ *HTTPAgentDispatcher, c *mockRuntimeBrokerClient, _ *store.Agent) {
				c.returnErr = &brokerStatusError{StatusCode: http.StatusConflict, Body: freshAgentDirConflictBody}
			}),
			call: forMove, untouched: true,
			keeps: func(t *testing.T, err error) {
				assert.True(t, errors.Is(err, errProvisionSent), "sent, then refused: %v", err)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, c, a := tc.setup(t)
			err := tc.call(d, a)
			require.Error(t, err)
			assert.Equal(t, tc.untouched, moveTargetUntouched(err), "moveTargetUntouched(%v)", err)
			if tc.notSent {
				assert.Nil(t, c.lastCreateReq, "nothing reached the broker client")
				assert.False(t, errors.Is(err, errProvisionSent), "a pre-send error is never marked sent")
			}
			tc.keeps(t, err)
		})
	}
}

// The rollback delete is never sent without the move's reincarnation ID.
func TestDispatchAgentDeleteRecreated_RequiresReincarnationID(t *testing.T) {
	d, client, agent := newMoveDispatchFixture(t, recreateCaps)
	require.Error(t, d.DispatchAgentDeleteRecreated(context.Background(), agent, ""))
	assert.False(t, client.deleteCalled, "no delete may reach the broker")
}

// The delete query names the move's reincarnation ID only when it is set.
func TestDeleteAgentQuery_MoveReincarnationID(t *testing.T) {
	q := deleteAgentQuery(context.Background(), "p", DeleteAgentOptions{DeleteFiles: true, MoveReincarnationID: "r 1"})
	assert.Contains(t, q, "&moveReincarnationId=r+1")
	assert.NotContains(t, deleteAgentQuery(context.Background(), "p", DeleteAgentOptions{DeleteFiles: true}), "moveReincarnationId")
}
