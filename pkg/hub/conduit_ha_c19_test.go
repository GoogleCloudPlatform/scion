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

//go:build integration

package hub

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/hack/conduit-c19/c19driver"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestConduitHA_C19_FunctionalSmoke validates ptone/scion#2796 checkbox
// "C19 (T9 isolation and drain)" (functional smoke, design §3.2.2 Harness
// bullet) using the shared hack/conduit-c19 load driver on a 2-node Postgres
// Conduit HA topology:
//   - N=50 fake agents (25 dialed to Hub A, 25 dialed to Hub B, so half of
//     deliveries and control RPCs cross the internal relay hop),
//   - 1 deliver-message every 2s per agent, mixed with 512 KiB bodies and
//     rotate-token control RPCs,
//   - broker RPC churn (list/status) over the broker's conduit session,
//   - one cross-node bulk TCP tunnel stream to agent 0 (on Hub B) with a
//     completely stalled reader throughout the run.
//
// Asserts: no session-window stall of other stream kinds, zero 429s at default
// MaxConcurrentRPCs, zero errors, and every persisted delivery message row
// reaches a terminal state (delivered) in Postgres. Latencies are logged for
// visibility and not asserted on CI loopback.
func TestConduitHA_C19_FunctionalSmoke(t *testing.T) {
	env := newConduitHAEnv(t)
	ctx := context.Background()
	guardSciontoolLog()

	const numAgents = 50

	// Start fake broker on Hub A with a handler answering list/status RPCs.
	var brokerRPCHandled atomic.Int64
	broker := env.StartBroker(t, env.HubA, conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		brokerRPCHandled.Add(1)
		switch req.GetPath() {
		case "/v1/broker/list", "/v1/broker/status":
			return &conduitv1.RpcResponse{Status: http.StatusOK, Body: []byte(`{"ok":true}`)}
		default:
			return &conduitv1.RpcResponse{Status: http.StatusNotFound}
		}
	}))

	// Seed N=50 agents in Postgres; even indices dial Hub B (cross-node from
	// Hub A's broker/user), odd indices dial Hub A (local). Agent 0 is on
	// Hub B so the stalled bulk tunnel from Hub A crosses the relay hop.
	targets := make([]c19driver.AgentTarget, numAgents)
	agentSlugs := make(map[string]string, numAgents)
	for i := 0; i < numAgents; i++ {
		slug := fmt.Sprintf("c19-agent-%02d", i)
		ag := &store.Agent{
			ID:              tid(slug),
			Slug:            slug,
			Name:            fmt.Sprintf("C19 Agent %02d", i),
			ProjectID:       env.Project.ID,
			OwnerID:         env.Owner.ID(),
			Ancestry:        []string{env.Owner.ID()},
			RuntimeBrokerID: env.BrokerID,
			Phase:           string(state.PhaseRunning),
		}
		require.NoError(t, env.HubA.Store.CreateAgent(ctx, ag))
		runID := uuid.NewString()
		_, err := env.HubA.Store.SetAgentRunID(ctx, ag.ID, runID, nil)
		require.NoError(t, err)
		if i == 0 {
			require.NoError(t, env.HubA.Store.UpdateAgentExposedPorts(ctx, ag.ID, []store.ExposedPort{
				{Port: 8080, Host: "127.0.0.1", Mode: "rw"},
			}))
		}
		agentSlugs[ag.ID] = slug

		node := env.HubA
		if i%2 == 0 {
			node = env.HubB
		}
		tok, err := node.Server.GenerateAgentToken(ag.ID, env.Project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		targets[i] = c19driver.AgentTarget{
			AgentID:   ag.ID,
			ProjectID: env.Project.ID,
			LaunchID:  runID,
			Token:     tok,
			HubURL:    node.Public.URL,
		}
	}

	var (
		msgMu  sync.Mutex
		msgIDs []string
	)
	ownerTok := haUserToken(t, env.HubA, env.Owner)
	hopsBeforeB := env.HubB.InternalRPCHops()

	res, err := c19driver.Run(ctx, c19driver.Config{
		Agents:                   targets,
		Duration:                 4500 * time.Millisecond,
		DeliverInterval:          2 * time.Second,
		LargeBodyCount:           4,
		RotateTokenCount:         10,
		BrokerRPCCount:           20,
		EnableBulkTunnelOnAgent0: true,
		Deliver: func(callCtx context.Context, agentID, body string) (int, string, error) {
			m := &store.Message{
				ID:            uuid.NewString(),
				ProjectID:     env.Project.ID,
				Sender:        "user:owner",
				SenderID:      env.Owner.ID(),
				Recipient:     "agent:" + agentSlugs[agentID],
				RecipientID:   agentID,
				Msg:           body,
				Type:          "instruction",
				AgentID:       agentID,
				DispatchState: store.MessageDispatchDispatched,
			}
			if err := env.HubA.Store.CreateMessage(callCtx, m); err != nil {
				return 0, "", err
			}
			msgMu.Lock()
			msgIDs = append(msgIDs, m.ID)
			msgMu.Unlock()

			text := body
			status, resp, err := broker.DeliverMessage(callCtx, agentID, &agentcontrol.DeliverRequest{
				V:          1,
				DeliveryID: "c19-" + uuid.NewString(),
				MessageIDs: []string{m.ID},
				Op:         agentcontrol.OpPaste,
				Text:       &text,
				DeadlineMS: 92000,
				SubmitKeys: []string{"Enter"},
			})
			return status, string(resp.Outcome), err
		},
		RotateToken: func(callCtx context.Context, agentID string) error {
			return env.HubA.Server.RequestAgentTokenRotation(callCtx, agentID)
		},
		BrokerRPC: func(callCtx context.Context, seq int) error {
			path := "/v1/broker/list"
			node := env.HubA
			if seq%2 == 1 {
				path = "/v1/broker/status"
				node = env.HubB
			}
			return node.Router().Do(callCtx, router.Request{
				Op:   router.OpStatefulRPC,
				Kind: registry.PrincipalBroker,
				ID:   env.BrokerID,
				Want: registry.Want{Capability: conduit.RPCImageStatus},
			}, func(c context.Context, r router.Resolved) error {
				resp, err := r.Session.Call(c, &conduitv1.RpcRequest{
					Method: http.MethodGet,
					Path:   path,
				})
				if err != nil {
					return err
				}
				if resp.GetStatus() != http.StatusOK {
					return fmt.Errorf("broker RPC %s answered %d", path, resp.GetStatus())
				}
				return nil
			})
		},
		StalledTunnel: func(_ context.Context, agentID string) (func(), error) {
			tc := haDialUserTunnel(t, env.HubA, env.Owner, ownerTok)
			st := tc.openTCP(t, agentID, 8080)
			return func() { _ = st.Close() }, nil
		},
	})
	require.NoError(t, err)

	deliverStats := c19driver.SummarizeLatencies(res.DeliverLatencies)
	rotateStats := c19driver.SummarizeLatencies(res.RotateLatencies)
	brokerStats := c19driver.SummarizeLatencies(res.BrokerRPCLatencies)
	t.Logf("C19 smoke: agents=%d deliveries=%d/%d large=%d rotate=%d brokerRPC=%d 429s=%d errors=%d stalledStart=%d stalledEnd=%d relayHopsB=%d",
		res.AgentCount, res.DeliverSucceeded, res.DeliverAttempts, res.LargeBodySucceeded,
		res.RotateTokenSucceeded, res.BrokerRPCSucceeded, res.Deliver429s, res.DeliverErrors,
		res.StalledBulkBytesAtStart, res.StalledBulkBytesWritten, env.HubB.InternalRPCHops()-hopsBeforeB)
	t.Logf("C19 latencies (logged, not asserted): deliver(p50=%s p99=%s max=%s) rotate(p50=%s p99=%s max=%s) brokerRPC(p50=%s p99=%s max=%s)",
		deliverStats.P50, deliverStats.P99, deliverStats.Max,
		rotateStats.P50, rotateStats.P99, rotateStats.Max,
		brokerStats.P50, brokerStats.P99, brokerStats.Max)

	assert.NoError(t, res.FirstError)
	assert.Equal(t, int64(0), res.Deliver429s, "must not hit 429 at default MaxConcurrentRPCs")
	assert.Equal(t, int64(0), res.DeliverNonDelivered, "every delivery response must report outcome=delivered")
	assert.Equal(t, int64(0), res.DeliverErrors, "all deliveries must succeed without session-window stall")
	assert.Equal(t, int64(4), res.LargeBodySucceeded, "all 512 KiB deliveries must succeed")
	assert.Equal(t, int64(0), res.LargeBodyErrors)
	assert.Equal(t, int64(10), res.RotateTokenSucceeded, "all rotate-token RPCs must succeed")
	assert.Equal(t, int64(0), res.RotateTokenErrors)
	assert.Equal(t, int64(20), res.BrokerRPCSucceeded, "all broker RPCs must succeed")
	assert.Equal(t, int64(0), res.BrokerRPCErrors)
	assert.Equal(t, int64(20), brokerRPCHandled.Load())
	assert.GreaterOrEqual(t, res.DeliverSucceeded, int64(numAgents), "every agent must complete at least one delivery")
	assert.GreaterOrEqual(t, res.StalledBulkBytesAtStart, int64(conduit.DefaultStreamWindow),
		"stalled bulk tunnel must have filled at least one stream receive window before load starts")
	assert.Equal(t, res.StalledBulkBytesAtStart, res.StalledBulkBytesWritten,
		"stalled bulk tunnel must remain backpressured across the entire load window")
	assert.Greater(t, env.HubB.InternalRPCHops()-hopsBeforeB, int64(0),
		"cross-node relay hops to Hub B must be exercised")

	// Every persisted message row must have reached a terminal state (delivered)
	// in real Postgres.
	require.NotEmpty(t, msgIDs)
	for _, id := range msgIDs {
		m, err := env.HubB.Store.GetMessage(ctx, id)
		require.NoError(t, err, "message %s", id)
		assert.Equal(t, store.MessageDispatchDelivered, m.DispatchState, "message %s must reach terminal delivered state", id)
	}
}
