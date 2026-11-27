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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/conduittest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// --- Shared helpers for #2796 HA row tests ---

func haSetBrokerAttach(t *testing.T, env *conduitHAEnv, attach bool) {
	t.Helper()
	ctx := context.Background()
	b, err := env.HubA.Store.GetRuntimeBroker(ctx, env.BrokerID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{Attach: attach, Sync: true}
	require.NoError(t, env.HubA.Store.UpdateRuntimeBroker(ctx, b))
}

func haUserToken(t *testing.T, node *conduitHANode, user *AuthenticatedUser) string {
	t.Helper()
	tok, _, _, err := node.Server.userTokenService.GenerateTokenPair(
		user.ID(), user.Email(), user.DisplayName(), user.Role(), ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

func haPTYPreflight(t *testing.T, node *conduitHANode, agentID, token string) (status int, path, reason string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, node.Public.URL+"/api/v1/agents/"+agentID+"/pty", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		var ok ptyPreflightResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&ok))
		return resp.StatusCode, ok.Path, ""
	}
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	if r, ok := er.Error.Details["reason"].(string); ok {
		reason = r
	}
	return resp.StatusCode, string(ptyPathNone), reason
}

func haDialPTY(t *testing.T, node *conduitHANode, agentID, token, query string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(node.Public.URL, "http") + "/api/v1/agents/" + agentID + "/pty" + query
	c, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + token}})
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
		require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	}
	return c, resp, err
}

func haWaitFakePTY(t *testing.T, ch <-chan *conduittest.FakePTY) *conduittest.FakePTY {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("fake agent did not spawn a PTY within 10s")
		return nil
	}
}

func haWaitFakePTYClosed(t *testing.T, p *conduittest.FakePTY) {
	t.Helper()
	select {
	case <-p.Closed:
	case <-time.After(10 * time.Second):
		t.Fatal("fake PTY was not closed within 10s")
	}
}

func haPTYEcho(t *testing.T, c *websocket.Conn, payload string) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, c.WriteJSON(wsprotocol.NewPTYDataMessage([]byte(payload))))
	var got strings.Builder
	for !strings.Contains(got.String(), payload) {
		var msg wsprotocol.PTYDataMessage
		require.NoError(t, c.ReadJSON(&msg))
		require.Equal(t, wsprotocol.TypeData, msg.Type)
		got.Write(msg.Data)
	}
}

func haPTYReadClose(t *testing.T, c *websocket.Conn, timeout time.Duration) *websocket.CloseError {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(timeout)))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		require.ErrorAs(t, err, &ce)
		return ce
	}
}

// haTunnelClient is a minimal user conduit session for opening TCP tunnels in
// HA tests (C5).
type haTunnelClient struct {
	ls      conduit.LocalSession
	mu      sync.Mutex
	streams map[uint32]conduit.Stream
}

func haDialUserTunnel(t *testing.T, node *conduitHANode, user *AuthenticatedUser, token string) *haTunnelClient {
	t.Helper()
	c := &haTunnelClient{streams: map[uint32]conduit.Stream{}}
	cfg := conduit.Config{
		Clock: clock.Real(),
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return nil
			}
			c.mu.Lock()
			c.streams[st.ID()] = st
			c.mu.Unlock()
			return nil
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(node.Public.URL, "http") + "/api/v1/conduit",
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer " + token}}, nil
		},
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_USER,
		PrincipalId:   user.ID(),
	}
	s, _, err := conduit.Dial(ctx, d, cfg, hello)
	require.NoError(t, err)
	c.ls = s.(conduit.LocalSession)
	t.Cleanup(func() { _ = c.ls.Close() })
	return c
}

func (c *haTunnelClient) openTCP(t *testing.T, agentID string, port int) conduit.Stream {
	t.Helper()
	body, err := json.Marshal(conduitTunnelRequest{
		Agent:  agentID,
		Kind:   "tcp",
		Params: map[string]string{"port": fmt.Sprintf("%d", port)},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := c.ls.Call(ctx, &conduitv1.RpcRequest{
		Method: http.MethodPost,
		Path:   conduitTunnelsPath,
		Body:   body,
	})
	require.NoError(t, err)
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	var ok conduitTunnelResponse
	require.NoError(t, json.Unmarshal(resp.GetBody(), &ok))

	deadline := time.Now().Add(10 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if st, found := c.streams[ok.StreamID]; found {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub-opened tunnel stream %d never arrived", ok.StreamID)
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		c.mu.Lock()
	}
}

// --- #2796 H-row tests ---

// TestConduitHA_2796_C2_AgentPTYCrossNode validates ptone/scion#2796 checkbox
// "C2 (agent path)": a PTY attach landing on Hub A reaches an agent whose
// conduit session is on Hub B over the internal peer relay hop, echoes input,
// forwards terminal resizes, and tears down the agent PTY on detach.
func TestConduitHA_2796_C2_AgentPTYCrossNode(t *testing.T) {
	env := newConduitHAEnv(t)
	haSetBrokerAttach(t, env, false)

	agent := env.StartAgentFor(t, env.HubB, env.Agent, conduittest.AgentOptions{EnablePTY: true})
	require.Equal(t, env.HubB.InstanceID, agent.RelayInstanceID())

	tok := haUserToken(t, env.HubA, env.Owner)
	hopsBeforeB := env.HubB.InternalStreamHops()
	hopsBeforeA := env.HubA.InternalStreamHops()

	wsConn, _, err := haDialPTY(t, env.HubA, env.Agent.ID, tok, "?cols=100&rows=30")
	require.NoError(t, err)

	pty := haWaitFakePTY(t, agent.PTYs.Spawned)
	assert.Equal(t, sconduit.PTYRequest{Cols: 100, Rows: 30, Session: "scion"}, pty.Req)
	assert.Equal(t, hopsBeforeB+1, env.HubB.InternalStreamHops(), "Hub B internal listener must receive the relayed PTY stream hop")
	assert.Equal(t, hopsBeforeA, env.HubA.InternalStreamHops(), "Hub A internal listener must not receive a stream hop")

	haPTYEcho(t, wsConn, "hello cross-node pty")

	require.NoError(t, wsConn.WriteJSON(wsprotocol.PTYResizeMessage{Type: wsprotocol.TypeResize, Cols: 120, Rows: 40}))
	select {
	case sz := <-pty.Resizes:
		assert.Equal(t, [2]uint16{120, 40}, sz)
	case <-time.After(10 * time.Second):
		t.Fatal("resize did not reach the agent PTY across nodes")
	}

	require.NoError(t, wsConn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	haWaitFakePTYClosed(t, pty)
}

// TestConduitHA_2796_C4_LifetimeHandoffCrossNode validates ptone/scion#2796
// checkbox "C4" (lifetime handoff) across two Postgres hub nodes: with a 90s
// relay lifetime cap on a fake relay clock, Hub B sends GoAway 60s before the
// cap; the in-flight cross-node PTY opened on Hub A keeps working during drain,
// the agent reconnects on a replacement session that serves new attaches, and
// at the 30s drain deadline (before the cap) the original PTY closes with 4503
// relay_restart while the accepted replacement PTY keeps echoing (even as the
// replacement session itself enters its own drain at t0+60s).
func TestConduitHA_2796_C4_LifetimeHandoffCrossNode(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second)
	relayClk := clock.NewFake(t0)
	env := newConduitHAEnvWith(t, conduitHAOptions{
		LifetimeCap:      90 * time.Second,
		SessionWriteWait: 2 * time.Minute,
		Clock:            relayClk,
		AuthzClock:       clock.Real(),
	})
	haSetBrokerAttach(t, env, false)

	agent := env.StartAgentFor(t, env.HubB, env.Agent, conduittest.AgentOptions{EnablePTY: true})
	origSessionID := agent.SessionID()
	require.NotEmpty(t, origSessionID)

	ls, _, ok := env.HubB.Relay().Local(context.Background(), origSessionID)
	require.True(t, ok, "agent session must be local on Hub B")

	tok := haUserToken(t, env.HubA, env.Owner)
	c1, _, err := haDialPTY(t, env.HubA, env.Agent.ID, tok, "")
	require.NoError(t, err)
	pty1 := haWaitFakePTY(t, agent.PTYs.Spawned)
	haPTYEcho(t, c1, "before-goaway")

	// Advance to 60s before the 90s lifetime cap: GoAway must fire on Hub B.
	capAt := t0.Add(90 * time.Second)
	relayClk.Advance(30*time.Second - time.Millisecond)
	require.False(t, ls.Info().Draining, "GoAway must not fire more than 60s before cap")
	relayClk.Advance(time.Millisecond)
	require.True(t, ls.Info().Draining, "GoAway must fire 60s before cap")

	// Accepted PTY stream stays live during drain.
	haPTYEcho(t, c1, "during-drain")

	// Agent reconnects and is admitted on a fresh replacement session.
	w2 := agent.WaitAdmitted(t, 10*time.Second)
	require.NotEmpty(t, w2.GetSessionId())
	require.NotEqual(t, origSessionID, w2.GetSessionId(), "replacement session must have a new session ID")

	// A new PTY attach on Hub A lands on the replacement session.
	c2, _, err := haDialPTY(t, env.HubA, env.Agent.ID, tok, "")
	require.NoError(t, err)
	pty2 := haWaitFakePTY(t, agent.PTYs.Spawned)
	haPTYEcho(t, c2, "on-replacement")

	// Advance to the 30s drain deadline (t0+60s, still before orig capAt t0+90s):
	// the old PTY closes with 4503 relay_restart. Note: the replacement session
	// was admitted at t0+30s so its own 90s-60s GoAway also fires at t0+60s;
	// its accepted PTY stream (c2) survives drain and keeps echoing.
	relayClk.Advance(30 * time.Second)
	ce := haPTYReadClose(t, c1, 10*time.Second)
	assert.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, ce.Code)
	assert.Equal(t, "relay_restart", ce.Text)
	assert.True(t, relayClk.Now().Before(capAt), "drain close must occur before the lifetime cap")
	haWaitFakePTYClosed(t, pty1)

	select {
	case <-ls.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("draining session did not end at the drain deadline")
	}

	haPTYEcho(t, c2, "after-drain")
	require.NoError(t, c2.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	haWaitFakePTYClosed(t, pty2)
}

// TestConduitHA_2796_C10_CapabilityTruthCrossNode validates ptone/scion#2796
// checkbox "C10" (capability truth) across two Postgres hub nodes:
//  1. Broker attach=false + agent on Hub B without PTY -> Hub A preflight 503
//     agent_pty_unavailable and WS dial 503 (no upgrade).
//  2. Broker attach=false + agent on Hub B with PTY -> Hub A preflight 200
//     path=agent and WS dial succeeds cross-node.
//  3. Broker attach=true (updated in Postgres) + agent still on Hub B with PTY
//     -> Hub A preflight 503 broker_not_connected (never falls back to agent
//     path) and WS dial 503 (no upgrade, no PTY spawned on agent).
func TestConduitHA_2796_C10_CapabilityTruthCrossNode(t *testing.T) {
	env := newConduitHAEnv(t)
	tok := haUserToken(t, env.HubA, env.Owner)

	// Case 1: attach=false, agent connected without PTY.
	haSetBrokerAttach(t, env, false)
	noPTYAgent := env.StartAgentFor(t, env.HubB, env.Agent, conduittest.AgentOptions{EnablePTY: false})
	status, path, reason := haPTYPreflight(t, env.HubA, env.Agent.ID, tok)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, string(ptyPathNone), path)
	assert.Equal(t, ptyReasonAgentPTYUnavailable, reason)

	_, resp, err := haDialPTY(t, env.HubA, env.Agent.ID, tok, "")
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.NoError(t, noPTYAgent.Close())

	// Case 2: attach=false, agent connected with PTY.
	ptyAgent := env.StartAgentFor(t, env.HubB, env.Agent, conduittest.AgentOptions{EnablePTY: true})
	require.NoError(t, pollUntil(5*time.Second, func() error {
		st, p, _ := haPTYPreflight(t, env.HubA, env.Agent.ID, tok)
		if st != http.StatusOK || p != string(ptyPathAgent) {
			return fmt.Errorf("preflight = %d path=%q, want 200 agent", st, p)
		}
		return nil
	}))
	c, _, err := haDialPTY(t, env.HubA, env.Agent.ID, tok, "")
	require.NoError(t, err)
	p := haWaitFakePTY(t, ptyAgent.PTYs.Spawned)
	haPTYEcho(t, c, "c10-agent-path")
	require.NoError(t, c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	haWaitFakePTYClosed(t, p)

	// Case 3: broker row flips to attach=true -> broker path selected (503
	// broker_not_connected because no legacy broker control channel is on Hub A),
	// never silently routed to the agent path.
	haSetBrokerAttach(t, env, true)
	status, path, reason = haPTYPreflight(t, env.HubA, env.Agent.ID, tok)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, string(ptyPathNone), path)
	assert.Equal(t, ptyReasonBrokerNotConnected, reason)

	_, resp, err = haDialPTY(t, env.HubA, env.Agent.ID, tok, "")
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Empty(t, ptyAgent.PTYs.Spawned, "agent must not spawn a PTY when broker attach=true")
}

// haAuthzMetrics records conduitStreamAuthzMetrics events for deterministic
// trigger/outcome assertions in C16b.
type haAuthzMetrics struct {
	mu     sync.Mutex
	counts map[string]int
	notify chan struct{}
}

func newHAAuthzMetrics() *haAuthzMetrics {
	return &haAuthzMetrics{
		counts: map[string]int{},
		notify: make(chan struct{}, 1),
	}
}

func (m *haAuthzMetrics) RecordConduitStreamAuthz(trigger, outcome, kind string) {
	m.mu.Lock()
	m.counts[trigger+"/"+outcome+"/"+kind]++
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

func (m *haAuthzMetrics) waitCount(t *testing.T, key string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		m.mu.Lock()
		got := m.counts[key]
		m.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-m.notify:
		case <-deadline:
			m.mu.Lock()
			got = m.counts[key]
			all := fmt.Sprintf("%v", m.counts)
			m.mu.Unlock()
			t.Fatalf("authz metric %q = %d, want >= %d within %s (all=%s)", key, got, want, timeout, all)
		}
	}
}

// TestConduitHA_2796_C16b_ActiveAuthzCrossNode validates ptone/scion#2796
// checkbox "C16b (T2 active authorization)" across two Postgres hub nodes with
// a deterministic fake AuthzClock, short authz_recheck_interval (200ms), and
// UserStreamAuthzMax (5s).
//
// Clauses covered:
//   - Renewal at interval: advancing AuthzClock by UserStreamAuthzMax fires
//     the interval check and records interval/renewed/pty for both streams
//     while permission holds; both PTYs keep echoing.
//   - NOTIFY revocation: with AuthzClock frozen (so the periodic sweep cannot
//     fire), revoking u1's attach role binding through Hub B closes u1's
//     cross-node PTY on Hub A with 4401 authz_expired and records
//     notify/closed/pty, while u2's stream stays open.
//   - Missed-notification sweep revocation: with Hub A's Postgres LISTEN
//     blocked via BlockLISTEN, revoking u2's attach role binding through Hub B
//     and advancing AuthzClock by one recheckInterval (200ms, far below
//     authzMax) closes u2's PTY on Hub A with 4401 authz_expired and records
//     sweep/closed/pty.
//
// Clause not covered here: interval-deny (closing with 4401 authz_expired when
// permission no longer holds at the authorization interval).
func TestConduitHA_2796_C16b_ActiveAuthzCrossNode(t *testing.T) {
	const (
		recheckInterval = 200 * time.Millisecond
		authzMax        = 5 * time.Second
		closeBound      = 5 * time.Second
	)
	authzClk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	env := newConduitHAEnvWith(t, conduitHAOptions{
		AuthzRecheckInterval: recheckInterval,
		UserStreamAuthzMax:   authzMax,
		AuthzClock:           authzClk,
	})
	haSetBrokerAttach(t, env, false)
	ctx := context.Background()

	metrics := newHAAuthzMetrics()
	var metricsIface conduitStreamAuthzMetrics = metrics
	env.HubA.Server.conduitAuthzMetrics.Store(&metricsIface)

	// Use Hub B's dev admin token for Hub B's role-binding DELETE API, and
	// create two project members (u1, u2) with a custom agent.attach binding.
	adminID := tid("c16b-admin")
	adminTok := env.HubB.Server.config.DevAuthToken

	rd, err := env.HubA.Store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "c16b-attach-" + tid("rd"),
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.attach"},
	})
	require.NoError(t, err)

	mkUser := func(name string) (*AuthenticatedUser, string, string) {
		uid := tid("c16b-" + name)
		email := name + "@c16b.test"
		createTestUserWithProjectRole(t, env.HubA.Store, uid, email, env.Project.ID, store.ProjectRoleMember)
		ensureHubMembership(ctx, env.HubA.Store, uid)
		b, err := env.HubA.Store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      uid,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          env.Project.ID,
			CreatedBy:        adminID,
		})
		require.NoError(t, err)
		u := NewAuthenticatedUser(uid, email, name, store.UserRoleMember, "api")
		return u, haUserToken(t, env.HubA, u), b.ID
	}
	_, u1Tok, u1BindID := mkUser("u1")
	_, u2Tok, u2BindID := mkUser("u2")

	deleteBindingOnHubB := func(bindingID string) {
		req, err := http.NewRequest(http.MethodDelete, env.HubB.Public.URL+"/api/v1/admin/role-bindings/"+bindingID, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+adminTok)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	}

	agent := env.StartAgentFor(t, env.HubB, env.Agent, conduittest.AgentOptions{EnablePTY: true})

	c1, _, err := haDialPTY(t, env.HubA, env.Agent.ID, u1Tok, "")
	require.NoError(t, err)
	pty1 := haWaitFakePTY(t, agent.PTYs.Spawned)
	haPTYEcho(t, c1, "u1-initial")

	c2, _, err := haDialPTY(t, env.HubA, env.Agent.ID, u2Tok, "")
	require.NoError(t, err)
	pty2 := haWaitFakePTY(t, agent.PTYs.Spawned)
	haPTYEcho(t, c2, "u2-initial")

	// 1. Renewal when permission still holds: advance AuthzClock by authzMax
	//    and require interval/renewed/pty to be recorded for both streams
	//    before checking post-renewal echoes.
	authzClk.Advance(authzMax)
	metrics.waitCount(t, conduitAuthzTriggerInterval+"/"+conduitAuthzOutcomeRenewed+"/pty", 2, 10*time.Second)
	haPTYEcho(t, c1, "u1-renewed")
	haPTYEcho(t, c2, "u2-renewed")

	// 2. Fast-path NOTIFY revocation: first confirm Hub A's Postgres LISTEN
	//    connection is actively receiving conduitAuthzChangedSubject events
	//    from Hub B (mirrors conduit_authz_postgres_test.go waitListening so
	//    the fast-path test never races initial LISTEN setup). With AuthzClock
	//    frozen (so sweep cannot fire), revoking u1 via Hub B API must close
	//    u1 with 4401 authz_expired via trigger=notify, while u2 stays open.
	listenCh, unsubListen := env.HubA.Events.Subscribe(conduitAuthzChangedSubject)
	probeMarker := conduitAuthzMatch{AgentID: "listen-probe-" + uuid.NewString()}
	require.NoError(t, pollUntil(10*time.Second, func() error {
		env.HubB.Events.PublishRaw(conduitAuthzChangedSubject, probeMarker)
		select {
		case evt := <-listenCh:
			if strings.Contains(string(evt.Data), probeMarker.AgentID) {
				return nil
			}
			return errors.New("waiting for matching LISTEN probe")
		case <-time.After(100 * time.Millisecond):
			return errors.New("Hub A LISTEN not yet receiving Hub B conduitAuthzChangedSubject")
		}
	}))
	unsubListen()

	revokeStart := time.Now()
	deleteBindingOnHubB(u1BindID)
	ce1 := haPTYReadClose(t, c1, closeBound)
	assert.LessOrEqual(t, time.Since(revokeStart), closeBound)
	assert.Equal(t, wsprotocol.ClosePTYAuthRequired, ce1.Code)
	assert.Equal(t, conduitReasonAuthzExpired, ce1.Text)
	metrics.waitCount(t, conduitAuthzTriggerNotify+"/"+conduitAuthzOutcomeClosed+"/pty", 1, 10*time.Second)
	haWaitFakePTYClosed(t, pty1)

	haPTYEcho(t, c2, "u2-still-open")

	// 3. Missed notification: block Hub A's Postgres LISTEN, revoke u2 through
	//    Hub B, and advance AuthzClock by one recheckInterval (200ms, far
	//    below authzMax=5s). The periodic sweep on Hub A must close u2 with
	//    4401 authz_expired and record trigger=sweep.
	unblock := env.BlockLISTEN(t, env.HubA)
	defer unblock()

	sweepStart := time.Now()
	deleteBindingOnHubB(u2BindID)
	authzClk.Advance(recheckInterval)
	ce2 := haPTYReadClose(t, c2, closeBound)
	assert.LessOrEqual(t, time.Since(sweepStart), closeBound)
	assert.Equal(t, wsprotocol.ClosePTYAuthRequired, ce2.Code)
	assert.Equal(t, conduitReasonAuthzExpired, ce2.Text)
	metrics.waitCount(t, conduitAuthzTriggerSweep+"/"+conduitAuthzOutcomeClosed+"/pty", 1, 10*time.Second)
	haWaitFakePTYClosed(t, pty2)
}

// TestConduitHA_2796_C5_SlowReaderIsolationCrossNode validates ptone/scion#2796
// checkbox "C5 (flow control)" slow-reader isolation across two Postgres hub
// nodes: a TCP tunnel stream with a completely stalled reader fills its stream
// receive window on the agent's conduit session without stalling an interactive
// PTY stream or control RPCs sharing the exact same agent session; draining the
// stalled tunnel resumes bulk data flow.
func TestConduitHA_2796_C5_SlowReaderIsolationCrossNode(t *testing.T) {
	env := newConduitHAEnv(t)
	haSetBrokerAttach(t, env, false)
	require.NoError(t, env.HubA.Store.UpdateAgentExposedPorts(context.Background(), env.Agent.ID, []store.ExposedPort{
		{Port: 8080, Host: "127.0.0.1", Mode: "rw"},
	}))

	var bulkWritten atomic.Int64
	bulkDone := make(chan struct{})
	dialLocal := func(_ context.Context, _, _ string) (net.Conn, error) {
		serverConn, clientConn := net.Pipe()
		go func() {
			defer func() {
				_ = serverConn.Close()
				select {
				case <-bulkDone:
				default:
					close(bulkDone)
				}
			}()
			chunk := make([]byte, 16*1024)
			for i := range chunk {
				chunk[i] = byte(i)
			}
			for {
				n, err := serverConn.Write(chunk)
				if n > 0 {
					bulkWritten.Add(int64(n))
				}
				if err != nil {
					return
				}
			}
		}()
		return clientConn, nil
	}

	agent := env.StartAgentFor(t, env.HubB, env.Agent, conduittest.AgentOptions{
		EnablePTY: true,
		DialLocal: dialLocal,
	})
	tok := haUserToken(t, env.HubA, env.Owner)

	// Open a cross-node TCP tunnel on Hub A to the agent on Hub B and do NOT
	// read from it, so its per-stream receive window fills and backpressures.
	tc := haDialUserTunnel(t, env.HubA, env.Owner, tok)
	tunnelStream := tc.openTCP(t, env.Agent.ID, 8080)
	t.Cleanup(func() { _ = tunnelStream.Close() })

	// Wait until the producer has written at least one stream window (256 KiB)
	// and then stalls for 5 consecutive 100ms quiet windows (500ms total) so
	// staged buffer fills under CPU load do not trigger mid-fill.
	require.NoError(t, pollUntil(15*time.Second, func() error {
		if bulkWritten.Load() < conduit.DefaultStreamWindow {
			return fmt.Errorf("bulkWritten=%d < %d", bulkWritten.Load(), conduit.DefaultStreamWindow)
		}
		return nil
	}))
	var stalledAt int64
	const requiredQuietWindows = 5
	quietCount := 0
	lastBytes := bulkWritten.Load()
	require.NoError(t, pollUntil(20*time.Second, func() error {
		time.Sleep(100 * time.Millisecond)
		nowBytes := bulkWritten.Load()
		if nowBytes == lastBytes {
			quietCount++
			if quietCount >= requiredQuietWindows {
				stalledAt = nowBytes
				return nil
			}
			return fmt.Errorf("confirming backpressure stall (%d/%d quiet windows at %d bytes)", quietCount, requiredQuietWindows, nowBytes)
		}
		quietCount = 0
		lastBytes = nowBytes
		return fmt.Errorf("bulk producer still advancing: %d", nowBytes)
	}))

	// While the TCP tunnel reader is completely stalled, an interactive PTY
	// stream and a control RPC on the exact same agent session must proceed
	// without stalling.
	ptyConn, _, err := haDialPTY(t, env.HubA, env.Agent.ID, tok, "")
	require.NoError(t, err)
	pty := haWaitFakePTY(t, agent.PTYs.Spawned)

	for i := 0; i < 10; i++ {
		haPTYEcho(t, ptyConn, fmt.Sprintf("keystroke-%d", i))
	}

	kicksBefore := agent.KickCount()
	require.NoError(t, env.HubA.Server.RequestAgentTokenRotation(context.Background(), env.Agent.ID))
	require.NoError(t, pollUntil(5*time.Second, func() error {
		if agent.KickCount() <= kicksBefore {
			return errors.New("waiting for rotate-token kick")
		}
		return nil
	}))

	// Confirm the stalled tunnel stream remained backpressured during the PTY
	// and control RPC work.
	assert.Equal(t, stalledAt, bulkWritten.Load(), "stalled TCP tunnel stream must remain backpressured while unread")

	// Draining the tunnel stream must resume producer writes cleanly.
	drainBuf := make([]byte, 64*1024)
	var drained int64
	for drained < 2*conduit.DefaultStreamWindow {
		n, err := tunnelStream.Read(drainBuf)
		require.NoError(t, err)
		drained += int64(n)
	}
	require.NoError(t, pollUntil(5*time.Second, func() error {
		if bulkWritten.Load() <= stalledAt {
			return fmt.Errorf("bulk producer did not resume after drain: %d <= %d", bulkWritten.Load(), stalledAt)
		}
		return nil
	}))

	_ = tunnelStream.Close()
	require.NoError(t, ptyConn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	haWaitFakePTYClosed(t, pty)
}
