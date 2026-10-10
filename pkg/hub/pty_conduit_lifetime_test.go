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
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sseProbe is a live hub SSE subscription served by the fixture hub
// itself: GET /api/v1/agents/{id}/messages/stream on f.public as the
// agent's owner, fed by the hub's event publisher. It shares the server,
// listener and publisher with the PTY and conduit paths under test.
type sseProbe struct {
	pub     *ChannelEventPublisher
	agentID string
	ownerID string
	// convID is the owner's DM conversation with the agent, which a
	// real instruction from the owner is written to and which the stream
	// is scoped to under the default conversation setting.
	convID    string
	agentSlug string
	lines     chan string
	ended     chan struct{}
}

func newSSEProbe(t *testing.T, f *ptyConduitFixture) *sseProbe {
	t.Helper()
	p := &sseProbe{pub: NewChannelEventPublisher(), agentID: f.launched.ID, ownerID: f.launched.OwnerID,
		agentSlug: f.launched.Slug, lines: make(chan string, 64), ended: make(chan struct{})}
	t.Cleanup(p.pub.Close)
	f.srv.SetEventPublisher(p.pub)
	// Resolve the DM the way the instruction write path does, before
	// the stream opens, so the stream scopes itself to it.
	conv, err := messaging.ResolveOrCreateDMConversation(context.Background(), f.store, f.store,
		f.srv.messageLog, "user", p.ownerID, "agent", p.agentID)
	require.NoError(t, err)
	p.convID = conv.ConversationID
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.public.URL+"/api/v1/agents/"+f.launched.ID+"/messages/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.userToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	go func() {
		defer close(p.ended)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
	}()
	return p
}

// roundTrip publishes a message event carrying marker and waits for it
// on the same connection.
func (p *sseProbe) roundTrip(t *testing.T, marker string) {
	t.Helper()
	p.pub.publish("agent."+p.agentID+".message", UserMessageEvent{
		Sender: "user:" + p.ownerID, SenderID: p.ownerID,
		Recipient: "agent:" + p.agentSlug, RecipientID: p.agentID,
		AgentID: p.agentID, ConversationID: p.convID,
		Msg: marker, Type: "instruction",
	})
	timeout := time.After(10 * time.Second)
	for {
		select {
		case l := <-p.lines:
			if strings.HasPrefix(l, "data:") && strings.Contains(l, marker) {
				return
			}
		case <-p.ended:
			t.Fatalf("SSE stream ended before %q", marker)
		case <-timeout:
			t.Fatalf("SSE event %q not received", marker)
		}
	}
}

// TestAgentPTY_LifetimeCapHandoff (design §8 criterion 4, hub side): with
// the relay lifetime cap at 90s (fake relay clock), the relay sends GoAway
// on the agent's session 60s before the cap; the accepted PTY stream keeps
// working during the drain; at the 30s drain deadline the PTY client
// receives 4503 relay_restart, before the cap. A hub SSE stream on the
// same server, opened before the GoAway, keeps delivering events on one
// connection before, during and after the hand-off.
func TestAgentPTY_LifetimeCapHandoff(t *testing.T) {
	var relayClock *clock.Fake
	f := newPTYConduitFixture(t, func(o *ConduitRelayOptions) {
		o.LifetimeCap = 90 * time.Second
		relayClock = o.Clock.(*clock.Fake)
	})
	require.NotNil(t, relayClock)
	t0 := relayClock.Now()
	capAt := t0.Add(90 * time.Second)
	sse := newSSEProbe(t, f)
	sse.roundTrip(t, "before")

	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	sessions := f.agentSessions(t)
	require.Len(t, sessions, 1)
	rt := f.srv.conduit.Load()
	require.NotNil(t, rt)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ls, _, ok := rt.relay.Local(ctx, sessions[0].SessionID)
	require.True(t, ok)

	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	p := waitSpawn(t, f.spawned)
	echoRoundTrip(t, c, "before")

	relayClock.Advance(30*time.Second - time.Millisecond)
	require.False(t, ls.Info().Draining, "GoAway sent more than 60s before the cap")
	relayClock.Advance(time.Millisecond)
	require.True(t, ls.Info().Draining, "no GoAway 60s before the cap")
	assert.GreaterOrEqual(t, capAt.Sub(relayClock.Now()), relay.LifetimeGoAwayLead, "GoAway lead")

	// The accepted PTY stream is not cut by the GoAway; SSE is unaffected.
	echoRoundTrip(t, c, "draining")
	sse.roundTrip(t, "during drain")

	relayClock.Advance(30 * time.Second)
	ce := readUntilClose(t, c)
	assert.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, ce.Code)
	assert.Equal(t, relay.ReasonRelayRestart, ce.Text)
	assert.True(t, relayClock.Now().Before(capAt), "the PTY close must come before the cap (at +%s)", relayClock.Now().Sub(t0))
	waitClosedPTY(t, p)
	select {
	case <-ls.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the old session did not end at the drain deadline")
	}

	sse.roundTrip(t, "after")
}
