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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePTY is a PTY process for the sciontool agent: it echoes input to
// output and records resizes.
type fakePTY struct {
	req     sconduit.PTYRequest
	pr      *io.PipeReader
	pw      *io.PipeWriter
	resizes chan [2]uint16
	closed  chan struct{}
	once    sync.Once
}

func newFakePTY(req sconduit.PTYRequest) *fakePTY {
	pr, pw := io.Pipe()
	return &fakePTY{req: req, pr: pr, pw: pw, resizes: make(chan [2]uint16, 8), closed: make(chan struct{})}
}

func (p *fakePTY) Read(b []byte) (int, error)  { return p.pr.Read(b) }
func (p *fakePTY) Write(b []byte) (int, error) { return p.pw.Write(b) }
func (p *fakePTY) Resize(cols, rows uint16) error {
	select {
	case p.resizes <- [2]uint16{cols, rows}:
	default:
	}
	return nil
}
func (p *fakePTY) Close() error {
	p.once.Do(func() {
		close(p.closed)
		_ = p.pw.Close()
		_ = p.pr.Close()
	})
	return nil
}

// ptyConduitFixture is a hub (relay hub-a, hub.conduit on) whose launched
// agent runs a sciontool conduit agent with a fake PTY spawner, plus a
// user token for the agent's owner.
type ptyConduitFixture struct {
	*relayFixture
	userToken  string
	peerSecret []byte
	spawned    chan *fakePTY
	// regFault, while set, fails every conduit registry operation on this
	// hub node (the relay's and the router's).
	regFault atomic.Bool
}

func newPTYConduitFixture(t *testing.T) *ptyConduitFixture {
	t.Helper()
	f := &ptyConduitFixture{peerSecret: make([]byte, 32), spawned: make(chan *fakePTY, 8)}
	_, _ = rand.Read(f.peerSecret)
	f.relayFixture = newRelayFixture(t, func(o *ConduitRelayOptions) {
		auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: f.peerSecret, SelfID: "hub-a"})
		require.NoError(t, err)
		o.PeerAuth = auth
		faulty := &registry.FaultStore{Inner: o.Store, Fault: func(context.Context, string) error {
			if f.regFault.Load() {
				return errors.New("injected registry fault")
			}
			return nil
		}}
		o.Store = faulty
		o.Registry = registry.New(faulty, registry.Config{})
	})
	require.True(t, f.srv.conduitServing())
	// Broker ids are UUIDs in the store: register the agent's broker under
	// one (attach supported until a test says otherwise) and point the
	// launched agent at it.
	ctx := context.Background()
	brokerID := uuid.NewString()
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "pty-broker", Slug: "pty-broker", Status: store.BrokerStatusOnline,
		Capabilities: attachCaps(true),
	}))
	f.launched.RuntimeBrokerID = brokerID
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	require.Equal(t, brokerID, got.RuntimeBrokerID)
	f.launched = got
	f.userToken, _, _, err = f.srv.userTokenService.GenerateTokenPair(
		f.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	return f
}

// setBrokerRow replaces the launched agent's broker row.
func (f *ptyConduitFixture) setBrokerRow(t *testing.T, caps *store.BrokerCapabilities, defaultProfile string, profiles ...store.BrokerProfile) {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: f.launched.RuntimeBrokerID, Name: "pty-broker", Slug: "pty-broker", Status: store.BrokerStatusOnline,
		Capabilities: caps, Profiles: profiles, DefaultProfile: defaultProfile,
	}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
}

func attachCaps(attach bool) *store.BrokerCapabilities {
	return &store.BrokerCapabilities{Attach: attach, Sync: true}
}

// startTCPOnlySession opens an agent session for the launched agent that
// advertises only tcp. (A sciontool agent always advertises pty where tmux
// is on PATH, so the no-pty case uses a bare session.)
func (f *ptyConduitFixture) startTCPOnlySession(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tok := f.agentToken(t, f.launched)
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(f.public.URL, "http") + "/api/v1/conduit",
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"X-Scion-Agent-Token": {tok}}, nil
		},
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		PrincipalId:   f.launched.ID,
		Capabilities:  &conduitv1.Capabilities{StreamKinds: []string{grant.StreamKindTCP}, EndpointIncarnation: f.launched.RunID},
	}
	s, _, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.Len(t, f.agentSessions(t), 1, "the tcp-only session is registered")
}

// startPTYAgent runs a sciontool conduit agent for the launched agent
// against hubURL, serving pty with a fake spawner, and returns once its
// session is admitted.
func (f *ptyConduitFixture) startPTYAgent(t *testing.T, hubURL string) {
	t.Helper()
	guardSciontoolLog()
	tok := f.agentToken(t, f.launched)
	admitted := make(chan *conduitv1.Welcome, 4)
	opts := sconduit.Options{
		HubURL:    hubURL,
		AgentID:   f.launched.ID,
		ProjectID: f.launched.ProjectID,
		LaunchID:  f.launched.RunID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) { admitted <- w },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:     fixtureClock{Fake: clock.NewFake(f.clock.Now()), now: f.clock.Now},
		SpawnPTY: func(_ context.Context, req sconduit.PTYRequest) (sconduit.PTYProcess, error) {
			p := newFakePTY(req)
			f.spawned <- p
			return p, nil
		},
	}
	a, err := sconduit.New(opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("conduit agent did not stop")
		}
	})
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
}

// startPeerRelay starts a second relay (another hub node) on the shared
// registry, signing peers with the fixture's secret and publishing the
// hub's grant keys, and returns a base URL whose /api/v1/conduit lands on
// it (as the launched agent) while every other path is the hub.
func (f *ptyConduitFixture) startPeerRelay(t *testing.T, id string) string {
	t.Helper()
	var internalH atomic.Pointer[http.Handler]
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := internalH.Load()
		if h == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		(*h).ServeHTTP(w, r)
	}))
	t.Cleanup(internal.Close)
	auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: f.peerSecret, SelfID: id})
	require.NoError(t, err)
	r, err := relay.New(relay.Config{
		InstanceID:       id,
		InternalEndpoint: internal.URL,
		Registry:         f.reg,
		Store:            f.regStore,
		Session:          conduit.Config{Clock: clock.Real()},
		GrantKeys:        f.srv.conduitWelcomeGrantKeys,
		PeerAuth:         auth,
		HTTPClient:       internal.Client(),
		Clock:            clock.NewFake(time.Now()),
	})
	require.NoError(t, err)
	h := r.InternalHandler()
	internalH.Store(&h)
	require.NoError(t, r.Start(context.Background()))
	t.Cleanup(r.Kill)

	hub := f.srv.Handler()
	principal := relay.Principal{
		Kind: registry.PrincipalAgent, ID: f.launched.ID, ProjectID: f.launched.ProjectID,
		Agent: agentIncarnationFacts(f.launched),
	}
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/conduit" {
			hub.ServeHTTP(w, req)
			return
		}
		conn, err := ws.Upgrade(w, req, nil, ws.Options{})
		if err != nil {
			return
		}
		_ = r.Serve(context.Background(), conn, principal)
	}))
	t.Cleanup(public.Close)
	return public.URL
}

func (f *ptyConduitFixture) ptyURL() string {
	return f.public.URL + "/api/v1/agents/" + f.launched.ID + "/pty"
}

// preflight sends the non-upgrade GET /pty and returns the status and the
// path it names (200: body.path; refusal: error.details.reason).
func (f *ptyConduitFixture) preflight(t *testing.T) (status int, path, reason string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ptyURL(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.userToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode == http.StatusOK {
		var ok ptyPreflightResponse
		require.NoError(t, json.Unmarshal(body, &ok), "preflight body: %s", body)
		return resp.StatusCode, ok.Path, ""
	}
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(body, &er), "preflight body: %s", body)
	if r, ok := er.Error.Details["reason"].(string); ok {
		reason = r
	}
	return resp.StatusCode, string(ptyPathNone), reason
}

// dialPTY opens the PTY WebSocket with query (e.g. "?cols=100&rows=30").
func (f *ptyConduitFixture) dialPTY(t *testing.T, query string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(f.ptyURL(), "http") + query
	c, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + f.userToken}})
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
		require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	}
	return c, resp, err
}

func waitSpawn(t *testing.T, ch <-chan *fakePTY) *fakePTY {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("no PTY spawned on the agent")
		return nil
	}
}

func waitClosedPTY(t *testing.T, p *fakePTY) {
	t.Helper()
	select {
	case <-p.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's PTY process was not closed")
	}
}

// echoRoundTrip sends s as client input and reads output until s comes
// back.
func echoRoundTrip(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	require.NoError(t, c.WriteJSON(wsprotocol.NewPTYDataMessage([]byte(s))))
	var got strings.Builder
	for !strings.Contains(got.String(), s) {
		var msg wsprotocol.PTYDataMessage
		require.NoError(t, c.ReadJSON(&msg))
		require.Equal(t, wsprotocol.TypeData, msg.Type)
		got.Write(msg.Data)
	}
}

// readUntilClose reads until the server's close frame and returns it.
func readUntilClose(t *testing.T, c *websocket.Conn) *websocket.CloseError {
	t.Helper()
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

// TestBrokerRowAttachUnsupported_Rules: contracts §2 "PTY path selection"
// rules 2 and 3. The agent's profile is AppliedConfig.Profile, else the
// broker's DefaultProfile, else the only profile; only an explicit
// Attach == false is unsupported; Capabilities.Attach decides only when no
// profile resolves or the resolved profile is the default.
func TestBrokerRowAttachUnsupported_Rules(t *testing.T) {
	yes, no := true, false
	prof := func(name string, attach *bool) store.BrokerProfile {
		return store.BrokerProfile{Name: name, Type: "docker", Available: true, Attach: attach}
	}
	for _, tc := range []struct {
		name         string
		agentProfile string
		broker       store.RuntimeBroker
		want         bool
	}{
		{"agent profile explicit false", "gpu",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", &yes), prof("gpu", &no)}, DefaultProfile: "default", Capabilities: attachCaps(true)}, true},
		{"agent profile explicit true beats default caps false", "gpu",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", nil), prof("gpu", &yes)}, DefaultProfile: "default", Capabilities: attachCaps(false)}, false},
		{"non-default profile with nil attach is supported, caps not borrowed", "gpu",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", &no), prof("gpu", nil)}, DefaultProfile: "default", Capabilities: attachCaps(false)}, false},
		{"agent names the default profile, nil attach, caps decide", "default",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", nil), prof("gpu", nil)}, DefaultProfile: "default", Capabilities: attachCaps(false)}, true},
		{"no agent profile: DefaultProfile explicit false", "",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", &no), prof("gpu", &yes)}, DefaultProfile: "default", Capabilities: attachCaps(true)}, true},
		{"no agent profile: DefaultProfile nil, caps false", "",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", nil)}, DefaultProfile: "default", Capabilities: attachCaps(false)}, true},
		{"no agent profile: DefaultProfile nil, caps true", "",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", nil)}, DefaultProfile: "default", Capabilities: attachCaps(true)}, false},
		{"only profile, no DefaultProfile, explicit false", "",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("only", &no)}, Capabilities: attachCaps(true)}, true},
		{"only profile, no DefaultProfile, nil, caps false", "",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("only", nil)}, Capabilities: attachCaps(false)}, true},
		{"no profile resolves, caps false", "",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("a", nil), prof("b", nil)}, Capabilities: attachCaps(false)}, true},
		{"no profiles, caps false", "", store.RuntimeBroker{Capabilities: attachCaps(false)}, true},
		{"no profiles, caps true", "", store.RuntimeBroker{Capabilities: attachCaps(true)}, false},
		{"no profiles, no caps (unknown)", "", store.RuntimeBroker{}, false},
		{"agent names an unregistered profile, caps false", "gpu",
			store.RuntimeBroker{Profiles: []store.BrokerProfile{prof("default", nil)}, DefaultProfile: "default", Capabilities: attachCaps(false)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &store.Agent{RuntimeBrokerID: "b"}
			if tc.agentProfile != "" {
				agent.AppliedConfig = &store.AgentAppliedConfig{Profile: tc.agentProfile}
			}
			b := tc.broker
			assert.Equal(t, tc.want, brokerRowAttachUnsupported(&b, agent))
		})
	}
}

// TestResolvePTYPath_DecidesFromStoredBrokerRow: rule 1. The decision reads
// only the stored broker row: with no broker connected anywhere (so no
// round-trip is possible) a row update alone moves the attach between the
// broker path (here refused: not connected) and the agent path.
func TestResolvePTYPath_DecidesFromStoredBrokerRow(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.startPTYAgent(t, f.public.URL)
	ctx := context.Background()
	no := false
	yes := true

	f.setBrokerRow(t, attachCaps(true), "")
	d := f.srv.resolvePTYPath(ctx, f.owner, f.launched)
	assert.Equal(t, ptyPathNone, d.Path)
	assert.Equal(t, ptyReasonBrokerNotConnected, d.Reason)

	f.setBrokerRow(t, attachCaps(false), "")
	assert.Equal(t, ptyPathAgent, f.srv.resolvePTYPath(ctx, f.owner, f.launched).Path)

	f.setBrokerRow(t, attachCaps(true), "default", store.BrokerProfile{Name: "default", Attach: &no})
	assert.Equal(t, ptyPathAgent, f.srv.resolvePTYPath(ctx, f.owner, f.launched).Path)

	f.setBrokerRow(t, attachCaps(false), "default", store.BrokerProfile{Name: "default", Attach: &yes})
	d = f.srv.resolvePTYPath(ctx, f.owner, f.launched)
	assert.Equal(t, ptyPathNone, d.Path)
	assert.Equal(t, ptyReasonBrokerNotConnected, d.Reason)
}

// TestPTYPath_PreflightAndOpenAgree: rule 4 and "broker first". For each
// case the preflight names a path and the WebSocket open takes exactly
// that path: broker (the broker receives the stream open), agent (the
// agent spawns the PTY) or none (503 with the same reason, no upgrade).
func TestPTYPath_PreflightAndOpenAgree(t *testing.T) {
	for _, tc := range []struct {
		name            string
		brokerAttach    bool
		brokerConnected bool
		agentPTY        *bool // nil: no agent session; false: a tcp-only session
		wantPath        ptyPath
		wantReason      string
	}{
		{"attach broker connected, pty agent: broker first", true, true, boolPtr(true), ptyPathBroker, ""},
		{"attach broker not connected, pty agent: not the agent path", true, false, boolPtr(true), ptyPathNone, ptyReasonBrokerNotConnected},
		{"unsupported broker, pty agent: agent", false, false, boolPtr(true), ptyPathAgent, ""},
		{"unsupported broker connected, pty agent: agent", false, true, boolPtr(true), ptyPathAgent, ""},
		{"unsupported broker, agent without pty: none", false, true, boolPtr(false), ptyPathNone, ptyReasonAgentPTYUnavailable},
		{"unsupported broker, no agent session: none", false, false, nil, ptyPathNone, ptyReasonAgentPTYUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			f.setBrokerRow(t, attachCaps(tc.brokerAttach), "")
			var b *fakeBroker
			if tc.brokerConnected {
				b = connectFakeBroker(t, f.srv, f.launched.RuntimeBrokerID)
			}
			switch {
			case tc.agentPTY == nil:
			case *tc.agentPTY:
				f.startPTYAgent(t, f.public.URL)
			default:
				f.startTCPOnlySession(t)
			}

			status, path, reason := f.preflight(t)
			assert.Equal(t, string(tc.wantPath), path)
			assert.Equal(t, tc.wantReason, reason)

			c, resp, err := f.dialPTY(t, "")
			switch tc.wantPath {
			case ptyPathNone:
				assert.Equal(t, http.StatusServiceUnavailable, status)
				require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
				require.NotNil(t, resp)
				assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			case ptyPathBroker:
				assert.Equal(t, http.StatusOK, status)
				require.NoError(t, err)
				require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(10*time.Second)))
				var open wsprotocol.StreamOpenMessage
				require.NoError(t, b.ws.ReadJSON(&open))
				assert.Equal(t, wsprotocol.StreamTypePTY, open.StreamType)
				assert.Empty(t, f.spawned, "the agent path was not used")
				_ = c.Close()
			case ptyPathAgent:
				assert.Equal(t, http.StatusOK, status)
				require.NoError(t, err)
				waitSpawn(t, f.spawned)
				echoRoundTrip(t, c, "agree")
			}
		})
	}
}

// TestAgentPTY_C10_UnsupportedBrokerPTYAgentAttaches: criterion 10, first
// half. A broker that reports attach unsupported plus an agent session
// advertising pty: the preflight is 200 naming the agent path, and an
// attach works end to end over the leaf protocol (initial size, typing
// round-trips, resize, ping/pong, detach).
func TestAgentPTY_C10_UnsupportedBrokerPTYAgentAttaches(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)

	status, path, _ := f.preflight(t)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(ptyPathAgent), path)

	c, _, err := f.dialPTY(t, "?cols=100&rows=30")
	require.NoError(t, err)
	p := waitSpawn(t, f.spawned)
	assert.Equal(t, sconduit.PTYRequest{Cols: 100, Rows: 30, Session: "scion"}, p.req)

	echoRoundTrip(t, c, "hello agent")

	// An out-of-range resize is dropped at the hub; the next one arrives.
	require.NoError(t, c.WriteJSON(wsprotocol.PTYResizeMessage{Type: wsprotocol.TypeResize, Cols: 0, Rows: 10}))
	require.NoError(t, c.WriteJSON(wsprotocol.PTYResizeMessage{Type: wsprotocol.TypeResize, Cols: 120, Rows: 40}))
	select {
	case sz := <-p.resizes:
		assert.Equal(t, [2]uint16{120, 40}, sz)
	case <-time.After(10 * time.Second):
		t.Fatal("resize did not reach the agent")
	}

	require.NoError(t, c.WriteJSON(wsprotocol.NewPingMessage()))
	for {
		var env wsprotocol.Envelope
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &env))
		if env.Type == wsprotocol.TypePong {
			break
		}
	}

	require.NoError(t, c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	waitClosedPTY(t, p)
}

// TestAgentPTY_C10_NeitherPathIs503NoUpgrade: criterion 10, second half.
// An agent whose broker reports attach unsupported and that has no session
// advertising pty: the preflight is 503 with a reason, and the WebSocket
// handshake is refused with the same 503 (nothing is upgraded).
func TestAgentPTY_C10_NeitherPathIs503NoUpgrade(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startTCPOnlySession(t)

	status, _, reason := f.preflight(t)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, ptyReasonAgentPTYUnavailable, reason)

	_, resp, err := f.dialPTY(t, "")
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	assert.Equal(t, wsprotocol.ErrCodeRuntimeAttachUnsupported, er.Error.Code)
	assert.Equal(t, ptyReasonAgentPTYUnavailable, er.Error.Details["reason"])
	assert.Empty(t, f.spawned)
}

// TestAgentPTY_C2_CrossNode: criterion 2 for the agent path. The agent's
// session is held by another relay (hub-b); the attach enters at hub-a,
// which resolves the session on hub-b and bridges the PTY stream through
// the internal relay API. Typing round-trips and resize works.
func TestAgentPTY_C2_CrossNode(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	hubB := f.startPeerRelay(t, "hub-b")
	f.startPTYAgent(t, hubB)

	recs := f.agentSessions(t)
	require.Len(t, recs, 1)
	require.Equal(t, "hub-b", recs[0].RelayInstanceID, "the agent session is on the other node")

	status, path, _ := f.preflight(t)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(ptyPathAgent), path)

	c, _, err := f.dialPTY(t, "?cols=90&rows=20")
	require.NoError(t, err)
	p := waitSpawn(t, f.spawned)
	assert.Equal(t, sconduit.PTYRequest{Cols: 90, Rows: 20, Session: "scion"}, p.req)
	echoRoundTrip(t, c, "across nodes")

	require.NoError(t, c.WriteJSON(wsprotocol.PTYResizeMessage{Type: wsprotocol.TypeResize, Cols: 132, Rows: 43}))
	select {
	case sz := <-p.resizes:
		assert.Equal(t, [2]uint16{132, 43}, sz)
	case <-time.After(10 * time.Second):
		t.Fatal("resize did not reach the agent across nodes")
	}
	require.NoError(t, c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	waitClosedPTY(t, p)
}

// TestPTYInitialSize_Clamps: client-supplied sizes are clamped into
// 1..4096 (default 80x24 when missing or unparsable) before minting, and
// the stream params are exactly cols, rows, session=scion, canonical.
func TestPTYInitialSize_Clamps(t *testing.T) {
	for _, tc := range []struct {
		query      string
		cols, rows int
	}{
		{"", 80, 24},
		{"cols=100&rows=30", 100, 30},
		{"cols=0&rows=-5", 1, 1},
		{"cols=99999&rows=4097", 4096, 4096},
		{"cols=wide&rows=", 80, 24},
		{"cols=0100&rows=030", 100, 30},
	} {
		t.Run(tc.query, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			require.NoError(t, err)
			cols, rows := ptyInitialSize(q)
			assert.Equal(t, tc.cols, cols)
			assert.Equal(t, tc.rows, rows)
			params := ptyStreamParams(cols, rows)
			require.NoError(t, conduitPTYTarget(params), "the params always pass the mint check")
			assert.Equal(t, map[string]string{grant.ParamCols: params[grant.ParamCols], grant.ParamRows: params[grant.ParamRows], grant.ParamSession: "scion"}, params)
		})
	}
}

// TestAgentPTY_GrantMatchesStreamOpenForOutOfRangeSize: an out-of-range
// client size is clamped before minting, so the grant and the StreamOpen
// carry the same params: the target (which verifies the grant against the
// StreamOpen and refuses a mismatch with 4403) spawns at the clamped size.
func TestAgentPTY_GrantMatchesStreamOpenForOutOfRangeSize(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)

	c, _, err := f.dialPTY(t, "?cols=0&rows=99999")
	require.NoError(t, err)
	p := waitSpawn(t, f.spawned)
	assert.Equal(t, sconduit.PTYRequest{Cols: 1, Rows: 4096, Session: "scion"}, p.req)
	echoRoundTrip(t, c, "clamped")
}

// TestAgentPTY_RevocationClosesBothLegs: the agent-path user stream is
// registered with trackConduitUserStream; a re-check that finds the
// permission gone (user suspended) closes both legs: the agent's PTY
// process ends and the client receives 4401 authz_expired.
func TestAgentPTY_RevocationClosesBothLegs(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	a := f.srv.conduitAuthz.Load()
	require.NotNil(t, a)
	require.Zero(t, a.Len())

	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	p := waitSpawn(t, f.spawned)
	echoRoundTrip(t, c, "before")
	require.Equal(t, 1, a.Len(), "the PTY stream is tracked for re-checks")

	ctx := context.Background()
	u, err := f.store.GetUser(ctx, f.launched.OwnerID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, u))
	a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})

	ce := readUntilClose(t, c)
	assert.Equal(t, 4401, ce.Code)
	assert.Equal(t, conduitReasonAuthzExpired, ce.Text)
	waitClosedPTY(t, p)
	assert.Zero(t, a.Len(), "the stream is untracked once it ends")
}

// TestAgentPTY_TargetGoneClosesWith4404: an agent deletion found by a
// re-check closes the PTY with 4404 target_not_found on the leaf.
func TestAgentPTY_TargetGoneClosesWith4404(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)

	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	p := waitSpawn(t, f.spawned)
	echoRoundTrip(t, c, "before")

	a := f.srv.conduitAuthz.Load()
	ctx := context.Background()
	require.NoError(t, f.store.DeleteAgent(ctx, f.launched.ID))
	a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})

	ce := readUntilClose(t, c)
	assert.Equal(t, wsprotocol.ClosePTYAgentNotFound, ce.Code)
	waitClosedPTY(t, p)
}

// TestPTYPath_NonDefaultProfileNilAttachTakesBrokerPath: rule 3. An agent
// on a non-default profile whose Attach is nil takes the broker path even
// though the broker's default runtime reports no attach; when the broker
// then refuses the attach, its 4501 attach_unsupported reaches the client
// unchanged.
func TestPTYPath_NonDefaultProfileNilAttachTakesBrokerPath(t *testing.T) {
	f := newPTYConduitFixture(t)
	no := false
	f.setBrokerRow(t, attachCaps(false), "default",
		store.BrokerProfile{Name: "default", Attach: &no}, store.BrokerProfile{Name: "gpu"})
	ctx := context.Background()
	f.launched.AppliedConfig = &store.AgentAppliedConfig{Profile: "gpu"}
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	f.launched = got
	b := connectFakeBroker(t, f.srv, f.launched.RuntimeBrokerID)
	f.startPTYAgent(t, f.public.URL)

	status, path, _ := f.preflight(t)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(ptyPathBroker), path)

	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(10*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, b.ws.ReadJSON(&open))
	require.NoError(t, b.ws.WriteJSON(wsprotocol.NewStreamCloseMessage(open.StreamID,
		wsprotocol.CloseReasonAttachUnsupported, wsprotocol.ClosePTYAttachUnsupported)))
	ce := readUntilClose(t, c)
	assert.Equal(t, wsprotocol.ClosePTYAttachUnsupported, ce.Code)
	assert.Equal(t, wsprotocol.CloseReasonAttachUnsupported, ce.Text)
	assert.Empty(t, f.spawned)
}

// TestPTYLeafCloseCode_Mapping: conduit stream closes map onto the PTY
// leaf contract; shared codes pass through with their reason.
func TestPTYLeafCloseCode_Mapping(t *testing.T) {
	for _, tc := range []struct {
		code     uint32
		reason   string
		wantCode int
	}{
		{0, "", 1000},
		{4401, "authz_expired", 4401},
		{4403, "grant_invalid", 4403},
		{4404, "target_not_found", 4404},
		{4503, "relay_restart", 4503},
		{4504, "upstream_unreachable", 4504},
		{1011, "internal", 1011},
		{4499, "cancelled", 4503},
		{4400, "bad_frame", 1011},
		{4409, "superseded_incarnation", 1011},
	} {
		code, reason := ptyLeafCloseCode(tc.code, tc.reason)
		assert.Equal(t, tc.wantCode, code, "conduit %d", tc.code)
		if tc.code != 0 {
			assert.Equal(t, tc.reason, reason)
		}
	}

	assert.Equal(t, &StreamClosedError{Code: 1000}, ptyLeafCloseForStream(io.EOF))
	assert.Equal(t, &StreamClosedError{Code: 4504, Reason: ptyCloseReasonUpstreamUnreachable},
		ptyLeafCloseForStream(errors.New("session lost")))
	assert.Equal(t, &StreamClosedError{Code: 4403, Reason: ptyCloseReasonForbidden},
		ptyAgentOpenError(errConduitForbidden))
}

// setAgentBroker points the launched agent at brokerID (which may have no
// row, or not be a valid broker id at all).
func (f *ptyConduitFixture) setAgentBroker(t *testing.T, brokerID string) {
	t.Helper()
	ctx := context.Background()
	f.launched.RuntimeBrokerID = brokerID
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	require.Equal(t, brokerID, got.RuntimeBrokerID)
	f.launched = got
}

// lockedLogBuffer collects JSON log lines written concurrently.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns the parsed log lines whose msg is msg.
func (b *lockedLogBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	lines := strings.Split(b.buf.String(), "\n")
	b.mu.Unlock()
	var out []map[string]any
	for _, line := range lines {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// capturePTYLogs routes the default logger to a buffer for the test. Call
// it after startPTYAgent (sciontool's log setup replaces the default).
func capturePTYLogs(t *testing.T) *lockedLogBuffer {
	t.Helper()
	b := &lockedLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

// TestPTYPath_UnreadableBrokerRowTakesBrokerPath: a broker row that cannot
// be read (no row, or an id the store rejects) is treated as supporting
// attach, and logged as such. Even with a pty-capable agent session
// running, the preflight names the broker path, the open goes to the
// broker, and the broker's 4501 attach_unsupported reaches the client
// unchanged; the agent path is not used.
func TestPTYPath_UnreadableBrokerRowTakesBrokerPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		brokerID  string
		wantClass string
	}{
		{"row missing", uuid.NewString(), "not_found"},
		{"id the store rejects", "not-a-uuid-broker", "read_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			f.setAgentBroker(t, tc.brokerID)
			b := connectFakeBroker(t, f.srv, tc.brokerID)
			f.startPTYAgent(t, f.public.URL)
			logs := capturePTYLogs(t)

			status, path, _ := f.preflight(t)
			require.Equal(t, http.StatusOK, status)
			require.Equal(t, string(ptyPathBroker), path)

			c, _, err := f.dialPTY(t, "")
			require.NoError(t, err)
			require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(10*time.Second)))
			var open wsprotocol.StreamOpenMessage
			require.NoError(t, b.ws.ReadJSON(&open))
			assert.Equal(t, wsprotocol.StreamTypePTY, open.StreamType)
			require.NoError(t, b.ws.WriteJSON(wsprotocol.NewStreamCloseMessage(open.StreamID,
				wsprotocol.CloseReasonAttachUnsupported, wsprotocol.ClosePTYAttachUnsupported)))
			ce := readUntilClose(t, c)
			assert.Equal(t, wsprotocol.ClosePTYAttachUnsupported, ce.Code)
			assert.Equal(t, wsprotocol.CloseReasonAttachUnsupported, ce.Text)
			assert.Empty(t, f.spawned, "the agent path was not used")

			recs := logs.records(t, "PTY path: broker row unreadable, treating attach as supported")
			require.Len(t, recs, 2, "logged once for the preflight and once for the open")
			for _, rec := range recs {
				assert.Equal(t, "WARN", rec["level"])
				assert.Equal(t, f.launched.ID, rec["agent_id"])
				assert.Equal(t, tc.brokerID, rec["broker_id"])
				assert.Equal(t, ptyAttachSourceUnreadableRow, rec["attach_source"])
				assert.Equal(t, tc.wantClass, rec["error_class"])
			}
		})
	}
}

// TestAgentPTY_FailClosedRefusals: with the broker reporting no attach and
// a pty-capable agent session running, the agent path is refused (503 with
// the reason, on the preflight and the handshake, nothing upgraded or
// spawned) when this node cannot open or police the stream: the user
// stream re-check is not running, conduit is not serving on this node, or
// the registry cannot be read.
func TestAgentPTY_FailClosedRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		apply      func(t *testing.T, f *ptyConduitFixture)
		wantReason string
	}{
		{"stream re-check not running", func(t *testing.T, f *ptyConduitFixture) {
			a := f.srv.conduitAuthz.Swap(nil)
			require.NotNil(t, a)
			t.Cleanup(func() { f.srv.conduitAuthz.CompareAndSwap(nil, a) })
		}, ptyReasonStreamAuthzDown},
		{"conduit not serving on this node", func(t *testing.T, f *ptyConduitFixture) {
			rt := f.srv.conduit.Swap(nil)
			require.NotNil(t, rt)
			t.Cleanup(func() { f.srv.conduit.CompareAndSwap(nil, rt) })
		}, ptyReasonConduitNotServing},
		{"registry unavailable", func(t *testing.T, f *ptyConduitFixture) {
			f.regFault.Store(true)
			t.Cleanup(func() { f.regFault.Store(false) })
		}, ptyReasonRegistryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			f.setBrokerRow(t, attachCaps(false), "")
			f.startPTYAgent(t, f.public.URL)
			tc.apply(t, f)

			status, _, reason := f.preflight(t)
			assert.Equal(t, http.StatusServiceUnavailable, status)
			assert.Equal(t, tc.wantReason, reason)

			_, resp, err := f.dialPTY(t, "")
			require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
			require.NotNil(t, resp)
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			var er ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
			assert.Equal(t, ErrCodeUnavailable, er.Error.Code)
			assert.Equal(t, tc.wantReason, er.Error.Details["reason"])
			assert.Empty(t, f.spawned)
		})
	}
}
