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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	scionportforward "github.com/GoogleCloudPlatform/scion/pkg/sciontool/portforward"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// No path without a target-side deadline (design §3.5 Q14 v2.21.45;
// contracts §2 "No conduit session (6.6b-3d)"): in external relay mode with
// enforce, a user request to an agent with no conduit session is refused
// with the existing retryable 503 and never served on a legacy route. Off
// and in-process profiles keep the legacy fallback.

// noSessionRefusal is the open refusal metric entry of the rule on route
// with reason label reason.
func noSessionRefusal(route, reason string) string {
	return route + "/" + reason + "/" + conduitCauseNoConduitSession
}

// startLegacyTunnel connects the agent's port-forward tunnel (an older
// sciontool's) to the hub at f.base and waits until srv holds it.
func startLegacyTunnel(t *testing.T, f *conduitProxyFixture, srv *Server) {
	t.Helper()
	guardSciontoolLog()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := scionportforward.NewManager(scionhub.NewClientWithConfig(f.base, f.agentToken(t, f.launched), f.launched.ID))
	go m.Run(ctx)
	require.Eventually(t, func() bool { return srv.portTunnels.has(f.launched.ID) },
		10*time.Second, 20*time.Millisecond, "tunnel not established")
}

// getA sends a request through hub A's port proxy route as the agent's
// owner.
func (w *separateRelayWorld) getA(t *testing.T, path string, header http.Header) *http.Response {
	t.Helper()
	tok, _, _, err := w.srv.userTokenService.GenerateTokenPair(
		w.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodGet,
		w.base+"/api/v1/agents/"+w.launched.ID+"/ports/"+strconv.Itoa(w.app.port)+"/proxy"+path, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// appHits returns how many requests reached the agent's app so far.
func (f *conduitProxyFixture) appHits() int { return len(f.app.got) }

// requireNoSessionBody checks the 503 routing-unavailable body of the
// rule: code unavailable, the restart hint, and no cause in the body.
func requireNoSessionBody(t *testing.T, body []byte) {
	t.Helper()
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(body, &er), string(body))
	assert.Equal(t, ErrCodeUnavailable, er.Error.Code)
	assert.Equal(t, conduitNoSessionMessage, er.Error.Message)
	assert.NotContains(t, string(body), conduitCauseNoConduitSession, "the cause is in logs and metrics only")
}

// TestConduitNoSessionRefused_Condition: the rule applies only in external
// relay mode with enforce, to user principals.
func TestConduitNoSessionRefused_Condition(t *testing.T) {
	ctx := context.Background()
	inProcess := newConduitFixture(t)
	enforceTargetDeadlines(inProcess.srv)
	extEnforce := newUpgradeFixture(t, &openSetup{enforce: true})
	extOff := newUpgradeFixture(t, &openSetup{})
	agent := newAgentIdentityFromStore(inProcess.agent)

	assert.True(t, extEnforce.cf.srv.conduitLegacyRouteRefused(ctx, inProcess.owner, conduitRouteProxy, ErrCodeUnavailable, conduitCauseNoConduitSession, "a"))
	assert.True(t, extEnforce.cf.srv.conduitLegacyRouteRefused(ctx, inProcess.portOnly, conduitRouteProxy, ErrCodeUnavailable, conduitCauseNoConduitSession, "a"), "scoped token over a user")
	assert.Equal(t, []string{noSessionRefusal(conduitRouteProxy, ErrCodeUnavailable), noSessionRefusal(conduitRouteProxy, ErrCodeUnavailable)}, extEnforce.o.rec.list())
	assert.False(t, extEnforce.cf.srv.conduitLegacyRouteRefused(ctx, agent, conduitRouteProxy, ErrCodeUnavailable, conduitCauseNoConduitSession, "a"), "an agent principal")
	assert.False(t, extEnforce.cf.srv.conduitLegacyRouteRefused(ctx, nil, conduitRouteProxy, ErrCodeUnavailable, conduitCauseNoConduitSession, "a"), "no principal")
	assert.False(t, extOff.cf.srv.conduitLegacyRouteRefused(ctx, inProcess.owner, conduitRouteProxy, ErrCodeUnavailable, conduitCauseNoConduitSession, "a"), "off")
	assert.False(t, inProcess.srv.conduitLegacyRouteRefused(ctx, inProcess.owner, conduitRouteProxy, ErrCodeUnavailable, conduitCauseNoConduitSession, "a"), "in-process")
	assert.Empty(t, extOff.o.rec.list())
}

// TestSeparateRelay_NoSession_Proxy (C1): external relay mode, enforce, an
// agent with no conduit session but a port-forward tunnel: the proxy
// answers 503 unavailable with the restart hint, the tunnel carries no
// request, and the cause is counted, not sent. A WebSocket request gets
// the same answer.
func TestSeparateRelay_NoSession_Proxy(t *testing.T) {
	w := newSeparateRelayWorldWith(t, separateRelayOptions{enforce: true, noConduitAgent: true})
	rec := recordOpenRefusals(w.srv)
	startLegacyTunnel(t, w.conduitProxyFixture, w.srv)
	require.Empty(t, w.agentSessions(t), "the agent has no conduit session")

	resp := w.getA(t, "/hello", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	requireNoSessionBody(t, body)
	assert.Zero(t, w.appHits(), "the port-forward tunnel carried no request")

	ws := w.getA(t, "/socket", http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
		"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}})
	wsBody, err := io.ReadAll(ws.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, ws.StatusCode, string(wsBody))
	requireNoSessionBody(t, wsBody)
	assert.Zero(t, w.appHits())

	assert.Equal(t, []string{noSessionRefusal(conduitRouteProxy, ErrCodeUnavailable), noSessionRefusal(conduitRouteProxy, ErrCodeUnavailable)}, rec.list())
}

// TestSeparateRelay_NoSession_ProxyOff (C3): with off, the same request
// falls back to the port-forward tunnel and reaches the app.
func TestSeparateRelay_NoSession_ProxyOff(t *testing.T) {
	w := newSeparateRelayWorldWith(t, separateRelayOptions{noConduitAgent: true})
	rec := recordOpenRefusals(w.srv)
	startLegacyTunnel(t, w.conduitProxyFixture, w.srv)

	resp := w.getA(t, "/hello", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "hello /hello", string(body))
	assert.Empty(t, rec.list())
}

// TestConduitNoSession_ProxyInProcess (C4): an in-process profile with
// enforce keeps the fallback to the port-forward tunnel.
func TestConduitNoSession_ProxyInProcess(t *testing.T) {
	f := newConduitProxyFixtureServer(t, nil, enforceTargetDeadlines)
	rec := recordOpenRefusals(f.srv)
	startLegacyTunnel(t, f, f.srv)
	require.Empty(t, f.agentSessions(t), "the agent has no conduit session")

	resp := f.get(t, "/legacy?v=1", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.True(t, strings.HasPrefix(string(body), "hello /legacy"), string(body))
	assert.Empty(t, rec.list())
}

// TestConduitNoSession_PTYAfterUpgrade (P6): after the upgrade, an open
// that finds no conduit session fails as an ordinary open failure (the
// existing transient close), not as the renewal refusal, with nothing
// signed or tracked and no legacy route.
func TestConduitNoSession_PTYAfterUpgrade(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)

	o.keys.arm(f.cf.srv)
	u := &agentPTYUpstream{s: f.cf.srv, identity: f.cf.owner, agent: f.cf.agent}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := u.Open(ctx, 80, 24)
	require.ErrorIs(t, err, router.ErrNoSession)
	var ce *StreamClosedError
	assert.False(t, errors.As(err, &ce), "an open failure, not a chosen close")
	assert.Nil(t, u.st)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, trackedLeaves(t, f.cf.srv))
	assert.Empty(t, o.rec.list())
}

// TestConduitNoSession_PTYPath (C2): under the rule, a user attach to an
// agent with no conduit session gets 503 unavailable, reason
// registry_unavailable, with the restart hint, never
// stream_renewal_unsupported; the cause is counted. Off keeps the broker
// branch.
func TestConduitNoSession_PTYPath(t *testing.T) {
	ctx := context.Background()
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)
	d := f.cf.srv.resolvePTYPath(ctx, f.cf.owner, f.withBroker(t))
	assert.Equal(t, ptyPathNone, d.Path)
	assert.Equal(t, http.StatusServiceUnavailable, d.Status)
	assert.Equal(t, ErrCodeUnavailable, d.Code)
	assert.Equal(t, ptyReasonRegistryUnavailable, d.Reason)
	assert.Equal(t, conduitNoSessionMessage, d.Message)
	assert.Equal(t, []string{noSessionRefusal(conduitRoutePTY, ptyReasonRegistryUnavailable)}, o.rec.list())

	offSetup := &openSetup{}
	off := newUpgradeFixture(t, offSetup)
	d = off.cf.srv.resolvePTYPath(ctx, off.cf.owner, off.withBroker(t))
	assert.Equal(t, ptyReasonBrokerNotConnected, d.Reason, "off: the broker branch decided")
	assert.Empty(t, offSetup.rec.list())
}

// ptyURLB is the PTY route of the world's agent on hub B.
func (w *separateRelayWorld) ptyURLB() string {
	return w.publicB.URL + "/api/v1/agents/" + w.launched.ID + "/pty"
}

// TestSeparateRelay_NoSession_PTY (C2): through hub B, the preflight and
// the WebSocket open get the same 503 with no upgrade.
func TestSeparateRelay_NoSession_PTY(t *testing.T) {
	w := newSeparateRelayWorldWith(t, separateRelayOptions{enforce: true, noConduitAgent: true})
	header := http.Header{"Authorization": {"Bearer " + w.userTokenB}}

	req, err := http.NewRequest(http.MethodGet, w.ptyURLB(), nil)
	require.NoError(t, err)
	req.Header = header.Clone()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(body, &er), string(body))
	assert.Equal(t, ErrCodeUnavailable, er.Error.Code)
	assert.Equal(t, ptyReasonRegistryUnavailable, er.Error.Details["reason"])
	assert.NotContains(t, string(body), conduitCauseNoConduitSession)

	c, wsResp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(w.ptyURLB(), "http"), header)
	if c != nil {
		_ = c.Close()
	}
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
	require.NotNil(t, wsResp)
	assert.Equal(t, http.StatusServiceUnavailable, wsResp.StatusCode)
	_ = wsResp.Body.Close()
}

// TestSeparateRelay_NoSession_LegacyOnlyPort: under the rule, a port with
// a non-loopback host, which only the port-forward tunnel serves, gets the
// proxy 503 with no tunnel request, counted with cause legacy_only_port.
func TestSeparateRelay_NoSession_LegacyOnlyPort(t *testing.T) {
	w := newSeparateRelayWorldWith(t, separateRelayOptions{enforce: true, noConduitAgent: true})
	rec := recordOpenRefusals(w.srv)
	startLegacyTunnel(t, w.conduitProxyFixture, w.srv)
	w.setExposedHost(t, "0.0.0.0")

	resp := w.getA(t, "/hello", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(body, &er), string(body))
	assert.Equal(t, ErrCodeUnavailable, er.Error.Code)
	assert.NotContains(t, string(body), conduitCauseLegacyOnlyPort)
	assert.Zero(t, w.appHits(), "the port-forward tunnel carried no request")
	assert.Equal(t, []string{conduitRouteProxy + "/" + ErrCodeUnavailable + "/" + conduitCauseLegacyOnlyPort}, rec.list())
}

// TestSeparateRelay_NoSession_ConduitOff: with hub.conduit off at run time,
// under the rule the proxy answers 503 and the PTY 503 conduit_not_serving,
// with no legacy route, counted with cause conduit_not_routable. With off
// the legacy routes are unchanged.
func TestSeparateRelay_NoSession_ConduitOff(t *testing.T) {
	ctx := context.Background()
	t.Run("enforce", func(t *testing.T) {
		w := newSeparateRelayWorldWith(t, separateRelayOptions{enforce: true, noConduitAgent: true})
		rec := recordOpenRefusals(w.srv)
		startLegacyTunnel(t, w.conduitProxyFixture, w.srv)
		setConduitExperiment(t, w.srv, false)

		resp := w.getA(t, "/hello", nil)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
		assert.Zero(t, w.appHits(), "the port-forward tunnel carried no request")

		d := w.srv.resolvePTYPath(ctx, w.owner, w.launched)
		assert.Equal(t, http.StatusServiceUnavailable, d.Status)
		assert.Equal(t, ptyReasonConduitNotServing, d.Reason)
		assert.Equal(t, []string{
			conduitRouteProxy + "/" + ErrCodeUnavailable + "/" + conduitCauseConduitNotRoutable,
			conduitRoutePTY + "/" + ptyReasonConduitNotServing + "/" + conduitCauseConduitNotRoutable,
		}, rec.list())
	})
	t.Run("off", func(t *testing.T) {
		w := newSeparateRelayWorldWith(t, separateRelayOptions{noConduitAgent: true})
		rec := recordOpenRefusals(w.srv)
		startLegacyTunnel(t, w.conduitProxyFixture, w.srv)
		setConduitExperiment(t, w.srv, false)

		resp := w.getA(t, "/hello", nil)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		assert.Equal(t, "hello /hello", string(body))

		d := w.srv.resolvePTYPath(ctx, w.owner, w.launched)
		assert.Equal(t, ErrCodeRuntimeBrokerUnavail, d.Code, "the broker's terminal path decided")
		assert.Empty(t, rec.list())
	})
}
