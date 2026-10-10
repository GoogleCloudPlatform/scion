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
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerReadCountingStore counts GetRuntimeBroker calls.
type brokerReadCountingStore struct {
	store.Store
	reads *atomic.Int32
}

func (s brokerReadCountingStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	s.reads.Add(1)
	return s.Store.GetRuntimeBroker(ctx, id)
}

// setAgentRuntime sets the launched agent's runtime.
func (f *ptyConduitFixture) setAgentRuntime(t *testing.T, runtime string) {
	t.Helper()
	ctx := context.Background()
	f.launched.Runtime = runtime
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	require.Equal(t, runtime, got.Runtime)
	f.launched = got
}

// TestPTYPath_ManagedRuntime: an agent on a managed runtime never takes the
// broker path and the broker row is not read. With an agent session that
// serves a PTY (hub.conduit on) it takes the agent path; otherwise the
// preflight and the WebSocket open both answer 503
// runtime_attach_unsupported with reason managed_runtime, and nothing is
// upgraded. A non-managed agent on the same broker keeps the broker path.
func TestPTYPath_ManagedRuntime(t *testing.T) {
	for _, tc := range []struct {
		name            string
		runtime         string
		noBroker        bool // the agent row has no runtime broker
		brokerConnected bool
		conduitOff      bool
		agentPTY        bool
		wantPath        ptyPath
		wantReason      string
		wantBrokerReads bool
	}{
		{name: "managed, no broker on the row", runtime: ManagedRuntimePrefix + "test", noBroker: true,
			wantPath: ptyPathNone, wantReason: ptyReasonManagedRuntime},
		{name: "managed, broker on the row and connected", runtime: ManagedRuntimePrefix + "test", brokerConnected: true,
			wantPath: ptyPathNone, wantReason: ptyReasonManagedRuntime},
		{name: "managed, conduit off", runtime: ManagedRuntimePrefix + "test", brokerConnected: true, conduitOff: true, agentPTY: true,
			wantPath: ptyPathNone, wantReason: ptyReasonManagedRuntime},
		{name: "managed, agent pty session: agent path", runtime: ManagedRuntimePrefix + "test", brokerConnected: true, agentPTY: true,
			wantPath: ptyPathAgent},
		{name: "non-managed control: broker path", runtime: "docker", brokerConnected: true, agentPTY: true,
			wantPath: ptyPathBroker, wantBrokerReads: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			f.setAgentRuntime(t, tc.runtime)
			var b *fakeBroker
			if tc.brokerConnected {
				b = connectFakeBroker(t, f.srv, f.launched.RuntimeBrokerID)
			}
			if tc.noBroker {
				f.setAgentBroker(t, "")
			}
			if tc.agentPTY {
				f.startPTYAgent(t, f.public.URL)
			}
			if tc.conduitOff {
				setConduitExperiment(t, f.srv, false)
			}
			reads := &atomic.Int32{}
			prev := f.srv.store
			f.srv.store = brokerReadCountingStore{Store: prev, reads: reads}
			t.Cleanup(func() { f.srv.store = prev })

			status, path, reason := f.preflight(t)
			if tc.wantPath == ptyPathNone {
				assert.Equal(t, http.StatusServiceUnavailable, status)
			} else {
				assert.Equal(t, http.StatusOK, status)
			}
			assert.Equal(t, string(tc.wantPath), path)
			assert.Equal(t, tc.wantReason, reason)

			c, resp, err := f.dialPTY(t, "")
			switch tc.wantPath {
			case ptyPathNone:
				require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
				require.NotNil(t, resp)
				assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
				var er ErrorResponse
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
				assert.Equal(t, wsprotocol.ErrCodeRuntimeAttachUnsupported, er.Error.Code)
				assert.Equal(t, tc.wantReason, er.Error.Details["reason"])
				assert.Empty(t, f.spawned, "the agent path was not used")
				if b != nil {
					require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
					var open wsprotocol.StreamOpenMessage
					assert.Error(t, b.ws.ReadJSON(&open), "the broker received no stream open")
				}
			case ptyPathAgent:
				require.NoError(t, err)
				waitSpawn(t, f.spawned)
				echoRoundTrip(t, c, "managed")
			case ptyPathBroker:
				require.NoError(t, err)
				require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(10*time.Second)))
				var open wsprotocol.StreamOpenMessage
				require.NoError(t, b.ws.ReadJSON(&open))
				assert.Equal(t, wsprotocol.StreamTypePTY, open.StreamType)
				assert.Empty(t, f.spawned, "the agent path was not used")
				_ = c.Close()
			}

			if tc.wantBrokerReads {
				assert.Positive(t, reads.Load(), "the broker row was read")
			} else {
				assert.Zero(t, reads.Load(), "no broker lookup for a managed runtime")
			}
		})
	}
}
