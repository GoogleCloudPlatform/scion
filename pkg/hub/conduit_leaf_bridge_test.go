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

package hub

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// newPTYSeparateRelayWorld is the separate relay world with an agent that
// also serves pty streams, with a fake spawner reporting each process.
func newPTYSeparateRelayWorld(t *testing.T) (*separateRelayWorld, chan *fakePTY) {
	t.Helper()
	spawned := make(chan *fakePTY, 4)
	w := newSeparateRelayWorld(t, func(w *separateRelayWorld) {
		w.spawnPTY = func(_ context.Context, req sconduit.PTYRequest) (sconduit.PTYProcess, error) {
			p := newFakePTY(req)
			spawned <- p
			return p, nil
		}
	})
	return w, spawned
}

// trackedLeaves returns the user streams a hub re-checks (the leaves it
// holds), with their bridges.
func trackedLeaves(t *testing.T, s *Server) []*conduitUserStream {
	t.Helper()
	a := s.conduitAuthz.Load()
	require.NotNil(t, a, "the hub runs no stream re-check")
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*conduitUserStream, 0, len(a.streams))
	for st := range a.streams {
		out = append(out, st)
	}
	return out
}

// TestSeparateRelay_PortProxyHubLeafOneBridge (design §3.9 hub-held leaves):
// a port proxy request entering hub B opens exactly one internal stream to
// relay-0, which hub B's leaf keeps as its bridge with relay-0's ids.
func TestSeparateRelay_PortProxyHubLeafOneBridge(t *testing.T) {
	w := newSeparateRelayWorld(t)
	sessions := w.agentSessions(t)
	require.Len(t, sessions, 1)

	events := w.getB(t, "/events")
	require.Equal(t, http.StatusOK, events.StatusCode)
	leaves := trackedLeaves(t, w.hubB)
	require.Len(t, leaves, 1)
	b := leaves[0].Bridge
	require.NotNil(t, b, "the leaf holds its bridge")
	want := relay.TargetStream{SessionID: sessions[0].SessionID, StreamID: w.lastAgentStream(t)}
	assert.Equal(t, want, b.Target())
	assert.Equal(t, want, relay.TargetStream{SessionID: leaves[0].SessionID, StreamID: leaves[0].StreamID})
	assert.Equal(t, "relay-0", b.Owner().InstanceID)
	assert.Equal(t, int64(1), w.relay.Relay.ActiveBridges(), "exactly one internal stream")
}

// TestSeparateRelay_PTYHubLeafOneBridge (design §3.9 hub-held leaves): an
// agent PTY opened by hub B goes over exactly one bridge to relay-0, which
// the leaf keeps, with relay-0's ids.
func TestSeparateRelay_PTYHubLeafOneBridge(t *testing.T) {
	w, spawned := newPTYSeparateRelayWorld(t)
	sessions := w.agentSessions(t)
	require.Len(t, sessions, 1)

	u := &agentPTYUpstream{s: w.hubB, identity: w.owner, agent: w.launched}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, u.Open(ctx, 80, 24))
	t.Cleanup(func() { _ = u.st.Close() })
	waitSpawn(t, spawned)

	b, ok := u.st.(relay.Bridge)
	require.True(t, ok, "the PTY leaf's stream is a bridge")
	want := relay.TargetStream{SessionID: sessions[0].SessionID, StreamID: w.lastAgentStream(t)}
	assert.Equal(t, want, b.Target())
	assert.Equal(t, "relay-0", b.Owner().InstanceID)
	leaves := trackedLeaves(t, w.hubB)
	require.Len(t, leaves, 1)
	assert.Equal(t, b, leaves[0].Bridge, "the tracked leaf keeps the same bridge")
	assert.Equal(t, int64(1), w.relay.Relay.ActiveBridges(), "exactly one internal stream")
}

// TestSeparateRelay_HubLeafBridgeRefusesAcceptWithoutTargetIDs: in external
// relay mode a hub-held leaf's stream is a strict bridge. An owner relay
// whose accept names no target stream gets the hop closed with 4400 and the
// open fails. In an in-process profile the same accept is still taken
// (the mixed-version compatibility path), with no bridge.
func TestSeparateRelay_HubLeafBridgeRefusesAcceptWithoutTargetIDs(t *testing.T) {
	open := &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}
	rec := registry.SessionRecord{SessionID: "remote-session", PrincipalKind: registry.PrincipalAgent, RelayInstanceID: "relay-x", RelayGeneration: 2}
	want := registry.Want{ProjectID: "p", Incarnation: "L1"}

	t.Run("external relay mode", func(t *testing.T) {
		w := newSeparateRelayWorld(t)
		fake := relaytest.StartFakeOwner(t, "", 0)
		res := router.Resolved{Session: relay.NewRemoteSession(&relay.PeerClient{Auth: externalRelayAuth(t, "hub-b")}, fake.URL, rec, want), Record: rec, Want: want}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, b, err := w.hubB.openConduitTargetStream(ctx, res, open)
		require.Error(t, err)
		assert.Nil(t, st)
		assert.Nil(t, b)
		assert.True(t, errors.Is(err, relay.ErrBridgeNoTarget), "%v", err)
		var ce *conduit.CloseError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, conduit.CloseProtocolError, ce.Code)
		select {
		case c := <-fake.Closes:
			require.NotNil(t, c)
			assert.Equal(t, conduit.CloseProtocolError, c.GetCode())
		case <-time.After(10 * time.Second):
			t.Fatal("the hop was not closed")
		}
	})
	t.Run("in-process profile", func(t *testing.T) {
		f := newRelayFixture(t, nil)
		fake := relaytest.StartFakeOwner(t, "", 0)
		res := router.Resolved{Session: relay.NewRemoteSession(&relay.PeerClient{Auth: externalRelayAuth(t, "hub-a")}, fake.URL, rec, want), Record: rec, Want: want}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, b, err := f.srv.openConduitTargetStream(ctx, res, open)
		require.NoError(t, err)
		t.Cleanup(func() { _ = st.Close() })
		assert.Nil(t, b, "no bridge in an in-process profile")
	})
}

// TestSeparateRelay_FeaturesReachHubRow: the features the agent's Hello
// advertised reach the registry row a hub resolves through another relay.
func TestSeparateRelay_FeaturesReachHubRow(t *testing.T) {
	w := newSeparateRelayWorld(t)
	res, err := w.hubB.conduitRouter().Resolve(context.Background(), agentPTYRequestFor(w.launched, grant.StreamKindTCP), nil)
	require.NoError(t, err)
	assert.Equal(t, "relay-0", res.Record.RelayInstanceID)
	assert.True(t, res.Record.Capabilities.HasFeature(conduit.FeatureStreamRenewal), "row features %v", res.Record.Capabilities.Features)
	assert.Equal(t, res.Record.Capabilities.Features, res.Session.Info().Capabilities.GetFeatures(), "the remote handle carries them")
}

// agentPTYRequestFor is the router request for an agent stream of kind.
func agentPTYRequestFor(a *store.Agent, kind string) router.Request {
	req := agentPTYRequest(a)
	req.Want.Capability = kind
	return req
}
