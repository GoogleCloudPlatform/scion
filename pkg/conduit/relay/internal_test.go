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

package relay_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/gorilla/websocket"
)

// pair is an owner relay A holding an agent session and a peer relay B.
type pair struct {
	w      *relaytest.World
	a, b   *relaytest.Node
	target conduit.LocalSession
	rec    registry.SessionRecord
	want   registry.Want
}

func newPair(t *testing.T, cfg conduit.Config) *pair {
	t.Helper()
	return newPairWith(t, cfg, nil)
}

// newPairWith is newPair with modA applied to the owner relay's config.
func newPairWith(t *testing.T, cfg conduit.Config, modA func(*relay.Config)) *pair {
	t.Helper()
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", modA)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	target, _ := a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	rec := w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session
	return &pair{w: w, a: a, b: b, target: target, rec: rec,
		want: registry.Want{ProjectID: project, Incarnation: "L1"}}
}

func (p *pair) remote() *relay.RemoteSession {
	return relay.NewRemoteSession(p.b.Peers, p.a.Internal.URL, p.rec, p.want)
}

func echoConfig() conduit.Config {
	return conduit.Config{
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return err
			}
			go func() {
				if rz, ok := st.(conduit.Resizable); ok {
					go func() {
						for ws := range rz.Resizes() {
							_ = st.Resize(ws.Rows, ws.Cols) // echo swapped
						}
					}()
				}
				_, _ = io.Copy(st, st)
				_ = st.(interface{ CloseWrite() error }).CloseWrite()
			}()
			return nil
		}),
		RPCHandler: conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: 200, Body: append([]byte("echo:"), req.GetBody()...)}
		}),
	}
}

// TestRemoteRPCAndStream (C15 at relay level): relay B reaches a target
// held by relay A through the internal API; data, half-close and resize
// cross the framed hop.
func TestRemoteRPCAndStream(t *testing.T) {
	p := newPair(t, echoConfig())
	rs := p.remote()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r7", Method: "exec", Body: []byte("hi")})
	if err != nil || resp.GetStatus() != 200 || string(resp.GetBody()) != "echo:hi" {
		t.Fatalf("Call = %v, %v", resp, err)
	}
	big, err := rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r8", Body: make([]byte, conduit.MaxRPCBody+1)})
	if err != nil || big.GetStatus() != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized Call = %v, %v; want local 413", big, err)
	}

	st, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("0123456789abcdef"), 3*conduit.MaxDataFrame/16) // spans frames and windows
	go func() {
		_, _ = st.Write(payload)
		_ = st.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err := io.ReadAll(st)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo: %d bytes, err %v; want %d bytes", len(got), err, len(payload))
	}
	_ = st.Close()
	p.a.Relay.WaitBridgesForTest()
	if n := p.a.Relay.ActiveBridges(); n != 0 {
		t.Fatalf("%d bridges after the stream ended", n)
	}
}

func TestRemoteResizeForwarded(t *testing.T) {
	p := newPair(t, echoConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := p.remote().OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Resize(80, 24); err != nil {
		t.Fatal(err)
	}
	ws := relaytest.Wait(t, st.(conduit.Resizable).Resizes(), "echoed resize")
	if ws.Cols != 24 || ws.Rows != 80 {
		t.Fatalf("echoed resize = %+v, want 24x80", ws)
	}
}

// TestOwnerRefusesStaleRoute: the owner re-checks admission, so a route to
// an obsolete epoch, the wrong exec scope or a relay that does not hold
// the session is stale (the router re-resolves), never served.
func TestOwnerRefusesStaleRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(p *pair) *relay.RemoteSession
	}{
		{name: "obsolete epoch", mod: func(p *pair) *relay.RemoteSession {
			// The agent reconnects; the old record now names epoch 1 of a
			// deleted session.
			p.w.SetPrincipal("a2", agentPrincipal("L1", 1))
			_, _ = p.a.MustDial("a2", relaytest.AgentHello(agentID, "L1", "", "pty"), echoConfig())
			return p.remote()
		}},
		{name: "exec scope not interchangeable", mod: func(p *pair) *relay.RemoteSession {
			return relay.NewRemoteSession(p.b.Peers, p.a.Internal.URL, p.rec, registry.Want{ProjectID: project, Incarnation: "L1", ExecScope: "scope-x"})
		}},
		{name: "wrong incarnation", mod: func(p *pair) *relay.RemoteSession {
			return relay.NewRemoteSession(p.b.Peers, p.a.Internal.URL, p.rec, registry.Want{ProjectID: project, Incarnation: "L0"})
		}},
		{name: "relay does not hold the session", mod: func(p *pair) *relay.RemoteSession {
			return relay.NewRemoteSession(p.b.Peers, p.b.Internal.URL, p.rec, p.want)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t, echoConfig())
			rs := tc.mod(p)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1"}); !errors.Is(err, relay.ErrStaleRoute) {
				t.Fatalf("Call = %v, want ErrStaleRoute", err)
			}
			if _, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}); !errors.Is(err, relay.ErrStaleRoute) {
				t.Fatalf("OpenStream = %v, want ErrStaleRoute", err)
			}
		})
	}
}

// TestOwnerAdmissionReadErrorFailsClosed: a registry read error at the
// owner is 503 (unavailable) at the HTTP layer, which the caller maps to
// 4504 upstream_unreachable (a transient failure, design v2.5 §3.3.1): not
// a stale route, not served and never a planned 4503.
func TestOwnerAdmissionReadErrorFailsClosed(t *testing.T) {
	p := newPair(t, echoConfig())
	p.w.SetFault(func(op string) error {
		if op == registry.OpListPrincipalSessionsBySession {
			return errors.New("injected read error")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.remote().Call(ctx, &conduitv1.RpcRequest{RequestId: "r1"})
	if errors.Is(err, relay.ErrStaleRoute) {
		t.Fatalf("Call = %v, want 4504, not a stale route", err)
	}
	assertClose(t, err, conduit.CloseRelayTimeout, relay.ReasonUpstreamUnreachable)
}

// --- C7: relay-peer identity and user sessions ---

// signed builds a relay-peer request for the relay to (its instance id and
// current generation), signed with auth (nil: unsigned).
func signed(t *testing.T, to *relay.Relay, auth relay.PeerAuth, method, url string, body []byte, want string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	relay.SetPeerTarget(req, to.InstanceID(), to.Generation())
	if body != nil {
		sum := sha256.Sum256(body)
		req.Header.Set(relay.HeaderBodySHA256, hex.EncodeToString(sum[:]))
	}
	if want != "" {
		req.Header.Set(relay.HeaderWant, want)
	}
	if auth != nil {
		if err := auth.Sign(req); err != nil {
			t.Fatal(err)
		}
	}
	return req
}

func do(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestInternalAPIRejectsUnauthenticatedPeer (C7): every internal route
// answers 401 to a caller without a valid relay-peer identity.
func TestInternalAPIRejectsUnauthenticatedPeer(t *testing.T) {
	p := newPair(t, echoConfig())
	base := p.a.Internal.URL + relay.InternalPathPrefix
	want := `{"project_id":"` + project + `","incarnation":"L1"}`
	rpcBody, _ := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1"})
	wrongKey, _ := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: bytes.Repeat([]byte{9}, 32), SelfID: "relay-b"})
	stale, _ := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: p.w.PeerSecret, SelfID: "relay-b",
		Now: func() time.Time { return time.Now().Add(-10 * time.Minute) }})
	routes := []struct {
		name, method, url string
		body              []byte
	}{
		{"self", http.MethodGet, base + "self", nil},
		{"rpc", http.MethodPost, base + "sessions/" + p.rec.SessionID + "/rpc", rpcBody},
		{"stream", http.MethodGet, base + "sessions/" + p.rec.SessionID + "/stream", nil},
	}
	for _, rt := range routes {
		for _, tc := range []struct {
			name string
			req  func() *http.Request
		}{
			{"no identity", func() *http.Request { return signed(t, p.a.Relay, nil, rt.method, rt.url, rt.body, want) }},
			{"wrong key", func() *http.Request { return signed(t, p.a.Relay, wrongKey, rt.method, rt.url, rt.body, want) }},
			{"stale timestamp", func() *http.Request { return signed(t, p.a.Relay, stale, rt.method, rt.url, rt.body, want) }},
			{"tampered want", func() *http.Request {
				r := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), rt.method, rt.url, rt.body, want)
				r.Header.Set(relay.HeaderWant, `{"project_id":"other","incarnation":"L1"}`)
				return r
			}},
			{"tampered path", func() *http.Request {
				r := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), rt.method, rt.url, rt.body, want)
				r.URL.Path = strings.Replace(r.URL.Path, p.rec.SessionID, "other", 1) + "x"
				return r
			}},
		} {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				if got := do(t, tc.req()); got != http.StatusUnauthorized {
					t.Fatalf("status %d, want 401", got)
				}
			})
		}
	}
	t.Run("replayed nonce", func(t *testing.T) {
		req := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodGet, base+"self", nil, "")
		replay := req.Clone(context.Background())
		if got := do(t, req); got != http.StatusOK {
			t.Fatalf("first request %d, want 200", got)
		}
		if got := do(t, replay); got != http.StatusUnauthorized {
			t.Fatalf("replay %d, want 401", got)
		}
	})
	t.Run("rpc body swapped after signing", func(t *testing.T) {
		req := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodPost, routes[1].url, rpcBody, want)
		other, _ := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r2", Method: "exec"})
		req.Body = io.NopCloser(bytes.NewReader(other))
		req.ContentLength = int64(len(other))
		if got := do(t, req); got != http.StatusBadRequest {
			t.Fatalf("status %d, want 400 (digest mismatch)", got)
		}
	})
}

// TestInternalAPIBindsTargetRelay: relay-peer signatures bind the target
// relay instance and generation. A request signed for another instance or
// generation is refused as a stale route (409), a request without a target
// or with a target changed after signing is unauthenticated (401), and a
// request signed for an earlier generation is refused by the restarted
// relay.
func TestInternalAPIBindsTargetRelay(t *testing.T) {
	p := newPair(t, echoConfig())
	base := p.a.Internal.URL + relay.InternalPathPrefix
	want := `{"project_id":"` + project + `","incarnation":"L1"}`
	rpcBody, _ := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1"})
	gen := p.a.Relay.Generation()
	routes := []struct {
		name, method, url string
		body              []byte
	}{
		{"self", http.MethodGet, base + "self", nil},
		{"rpc", http.MethodPost, base + "sessions/" + p.rec.SessionID + "/rpc", rpcBody},
		{"stream", http.MethodGet, base + "sessions/" + p.rec.SessionID + "/stream", nil},
	}
	for _, rt := range routes {
		for _, tc := range []struct {
			name   string
			target func(*http.Request)
			// retarget changes the target after signing.
			retarget   func(*http.Request)
			wantStatus int
		}{
			{name: "wrong instance", target: func(r *http.Request) { relay.SetPeerTarget(r, "relay-b", gen) }, wantStatus: http.StatusConflict},
			{name: "stale generation", target: func(r *http.Request) { relay.SetPeerTarget(r, "relay-a", gen-1) }, wantStatus: http.StatusConflict},
			{name: "future generation", target: func(r *http.Request) { relay.SetPeerTarget(r, "relay-a", gen+1) }, wantStatus: http.StatusConflict},
			{name: "no target", target: func(r *http.Request) {
				r.Header.Del(relay.HeaderPeerTarget)
				r.Header.Del(relay.HeaderPeerTargetGeneration)
			}, wantStatus: http.StatusUnauthorized},
			{name: "target changed after signing", target: func(r *http.Request) { relay.SetPeerTarget(r, "relay-b", gen) },
				retarget: func(r *http.Request) { relay.SetPeerTarget(r, "relay-a", gen) }, wantStatus: http.StatusUnauthorized},
		} {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				req, err := http.NewRequest(rt.method, rt.url, bytes.NewReader(rt.body))
				if err != nil {
					t.Fatal(err)
				}
				if rt.body != nil {
					sum := sha256.Sum256(rt.body)
					req.Header.Set(relay.HeaderBodySHA256, hex.EncodeToString(sum[:]))
				}
				req.Header.Set(relay.HeaderWant, want)
				tc.target(req)
				if err := p.w.PeerAuth("relay-b").Sign(req); err != nil {
					t.Fatal(err)
				}
				if tc.retarget != nil {
					tc.retarget(req)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status %d, want %d", resp.StatusCode, tc.wantStatus)
				}
				if tc.wantStatus == http.StatusConflict {
					if got := resp.Header.Get(relay.HeaderStaleReason); got != "relay_target_mismatch" {
						t.Fatalf("stale reason %q, want relay_target_mismatch", got)
					}
				}
			})
		}
	}

	t.Run("remote session with a stale generation is a stale route", func(t *testing.T) {
		rec := p.rec
		rec.RelayGeneration = gen - 1
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rs := relay.NewRemoteSession(p.b.Peers, p.a.Internal.URL, rec, p.want)
		if _, err := rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1"}); !errors.Is(err, relay.ErrStaleRoute) {
			t.Fatalf("Call = %v, want ErrStaleRoute", err)
		}
	})

	t.Run("request signed before a restart is refused after it", func(t *testing.T) {
		captured := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodGet, base+"self", nil, "")
		restarted := p.w.StartNode("relay-a", nil)
		if restarted.Relay.Generation() <= gen {
			t.Fatalf("restarted generation %d, want > %d", restarted.Relay.Generation(), gen)
		}
		resent := captured.Clone(context.Background())
		u, err := url.Parse(restarted.Internal.URL)
		if err != nil {
			t.Fatal(err)
		}
		resent.URL.Scheme, resent.URL.Host, resent.Host = u.Scheme, u.Host, u.Host
		if got := do(t, resent); got != http.StatusConflict {
			t.Fatalf("request signed for the old generation %d, want 409", got)
		}
		fresh := signed(t, restarted.Relay, p.w.PeerAuth("relay-b"), http.MethodGet, restarted.Internal.URL+relay.InternalPathPrefix+"self", nil, "")
		if got := do(t, fresh); got != http.StatusOK {
			t.Fatalf("request for the new generation %d, want 200", got)
		}
	})
}

// TestInternalStreamCapabilityChecks (F9): a stream request must name its
// stream kind as the capability (400 otherwise, before any upgrade), and a
// StreamOpen whose kind differs from the admitted capability is closed with
// 4400 bad_frame without reaching the target.
func TestInternalStreamCapabilityChecks(t *testing.T) {
	p := newPair(t, echoConfig())
	url := p.a.Internal.URL + relay.InternalPathPrefix + "sessions/" + p.rec.SessionID + "/stream"
	t.Run("missing capability", func(t *testing.T) {
		want := `{"project_id":"` + project + `","incarnation":"L1"}`
		if got := do(t, signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodGet, url, nil, want)); got != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", got)
		}
	})
	t.Run("kind differs from the capability", func(t *testing.T) {
		want := `{"project_id":"` + project + `","incarnation":"L1","capability":"pty"}`
		req := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodGet, url, nil, want)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, resp, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(url, "http"), req.Header)
		if err != nil {
			t.Fatalf("dial: %v (%v)", err, resp)
		}
		defer func() { _ = c.Close() }()
		open, _ := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{
			Kind: conduitv1.StreamKind_STREAM_KIND_LOGS}}})
		if err := c.WriteMessage(websocket.BinaryMessage, open); err != nil {
			t.Fatal(err)
		}
		_, b, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		f := &conduitv1.Frame{}
		if err := proto.Unmarshal(b, f); err != nil {
			t.Fatal(err)
		}
		sc := f.GetStreamClose()
		if sc == nil {
			t.Fatalf("first frame %v, want stream_close", f)
		}
		assertClose(t, &conduit.CloseError{Code: sc.GetCode(), Reason: sc.GetReason()}, conduit.CloseProtocolError, relay.ReasonBadFrame)
		if n := p.target.Stats().OpenStreams; n != 0 {
			t.Fatalf("target has %d open streams", n)
		}
	})
	t.Run("caller rejects an unknown kind", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := p.remote().OpenStream(ctx, &conduitv1.StreamOpen{})
		assertClose(t, err, conduit.CloseProtocolError, relay.ReasonBadFrame)
	})
}

// TestBridgeCloseWait: when the owner closes a hop with a code it relays
// from the target, the bridge keeps the link until the caller closes its
// side, capped at the handshake timeout; a protocol error the owner
// detected itself (a kind other than the admitted capability) closes the
// link at once.
func TestBridgeCloseWait(t *testing.T) {
	const wait = 2 * time.Second
	reject := conduit.Config{
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			return ps.Reject(conduit.CloseProtocolError, relay.ReasonBadFrame)
		}),
	}
	cases := []struct {
		name     string
		cfg      conduit.Config
		kind     conduitv1.StreamKind
		waitsCap bool
	}{
		{"target rejects 4400: waits for the caller", reject, conduitv1.StreamKind_STREAM_KIND_PTY, true},
		{"kind mismatch 4400: closes at once", echoConfig(), conduitv1.StreamKind_STREAM_KIND_LOGS, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPairWith(t, tc.cfg, func(c *relay.Config) { c.Session.HandshakeTimeout = wait })
			url := p.a.Internal.URL + relay.InternalPathPrefix + "sessions/" + p.rec.SessionID + "/stream"
			want := `{"project_id":"` + project + `","incarnation":"L1","capability":"pty"}`
			req := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodGet, url, nil, want)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, resp, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(url, "http"), req.Header)
			if err != nil {
				t.Fatalf("dial: %v (%v)", err, resp)
			}
			defer func() { _ = c.Close() }()
			open, _ := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{Kind: tc.kind}}})
			if err := c.WriteMessage(websocket.BinaryMessage, open); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, b, err := c.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			f := &conduitv1.Frame{}
			if err := proto.Unmarshal(b, f); err != nil {
				t.Fatal(err)
			}
			sc := f.GetStreamClose()
			if sc == nil {
				t.Fatalf("first frame %v, want stream_close", f)
			}
			if sc.GetCode() != conduit.CloseProtocolError {
				t.Fatalf("stream_close code %d, want %d", sc.GetCode(), conduit.CloseProtocolError)
			}

			// The caller read the close but keeps its side open.
			if tc.waitsCap {
				if n := p.a.Relay.ActiveBridges(); n != 1 {
					t.Fatalf("%d active bridges while the caller's side is open, want 1", n)
				}
				p.a.Clock.Advance(wait - time.Nanosecond)
				if n := p.a.Relay.ActiveBridges(); n != 1 {
					t.Fatalf("%d active bridges before the wait passed, want 1", n)
				}
				p.a.Clock.Advance(time.Nanosecond)
			}
			// Without the wait the clock is never advanced: the owner must
			// close the link on its own.
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatal("caller read a frame after stream_close, want the link closed")
			} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				t.Fatal("the owner did not close the link")
			}
			p.a.Relay.WaitBridgesForTest()
			if n := p.a.Relay.ActiveBridges(); n != 0 {
				t.Fatalf("%d active bridges after the link closed", n)
			}
		})
	}
}

// TestUserSessionNotRoutable (C7): the internal API refuses to route to a
// user session (403).
func TestUserSessionNotRoutable(t *testing.T) {
	p := newPair(t, echoConfig())
	p.w.SetPrincipal("u", relay.Principal{Kind: registry.PrincipalUser, ID: "user-1"})
	_, wel := p.a.MustDial("u", relaytest.UserHello("user-1"), conduit.Config{})
	url := p.a.Internal.URL + relay.InternalPathPrefix + "sessions/" + wel.GetSessionId() + "/rpc"
	body, _ := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1"})
	if got := do(t, signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodPost, url, body, `{"incarnation":""}`)); got != http.StatusForbidden {
		t.Fatalf("status %d, want 403", got)
	}
}

// TestUserStreamOpenClosed4403 (C7): a user session that sends StreamOpen
// toward the relay is closed with 4403; a target (agent) gets the stream
// refused with 4403 and keeps its session.
func TestUserStreamOpenClosed4403(t *testing.T) {
	p := newPair(t, echoConfig())
	p.w.SetPrincipal("u", relay.Principal{Kind: registry.PrincipalUser, ID: "user-1"})
	user, _ := p.a.MustDial("u", relaytest.UserHello("user-1"), conduit.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := user.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}); conduit.CodeOf(err, 0) != conduit.CloseForbidden {
		t.Fatalf("user OpenStream = %v, want 4403", err)
	}
	relaytest.WaitClosed(t, user.Done(), "user session close")
	if code := conduit.CodeOf(user.Err(), 0); code != conduit.CloseForbidden {
		t.Fatalf("user session ended with %v, want 4403", user.Err())
	}

	if _, err := p.target.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}); conduit.CodeOf(err, 0) != conduit.CloseForbidden {
		t.Fatalf("agent OpenStream = %v, want 4403", err)
	}
	select {
	case <-p.target.Done():
		t.Fatal("agent session closed after a refused stream")
	default:
	}
}

// --- T8: cancellation during opening and late accept ---

// TestBridgeCancelDuringOpening4499BothLegs: the caller cancels while the
// target has not decided; the target leg sees StreamClose 4499 and a later
// Accept fails; no bridge leaks.
func TestBridgeCancelDuringOpening4499BothLegs(t *testing.T) {
	pending := make(chan conduit.PendingStream, 1)
	targetCancelled := make(chan uint32, 1)
	cfg := conduit.Config{
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			pending <- ps
			return nil // decide later
		}),
		Interceptor: func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
			if c := f.GetStreamClose(); dir == conduit.Inbound && c != nil {
				select {
				case targetCancelled <- c.GetCode():
				default:
				}
			}
			return []*conduitv1.Frame{f}
		},
	}
	p := newPair(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.remote().OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
		errc <- err
	}()
	ps := relaytest.Wait(t, pending, "target StreamOpen")
	cancel()
	if err := relaytest.Wait(t, errc, "caller OpenStream"); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller OpenStream = %v, want context.Canceled", err)
	}
	if code := relaytest.Wait(t, targetCancelled, "target StreamClose"); code != conduit.CloseCancelled {
		t.Fatalf("target leg closed with %d, want 4499", code)
	}
	if _, err := ps.Accept(); !errors.Is(err, conduit.ErrStreamCancelled) {
		t.Fatalf("late Accept = %v, want ErrStreamCancelled", err)
	}
	p.a.Relay.WaitBridgesForTest()
	if n := p.a.Relay.ActiveBridges(); n != 0 {
		t.Fatalf("%d active bridges", n)
	}
}

// TestBridgeLateAcceptCleanedUp: the target accepts, but the caller has
// gone in the meantime; the owner closes the accepted target stream with
// 4499 instead of leaking it.
func TestBridgeLateAcceptCleanedUp(t *testing.T) {
	accepted := make(chan conduit.Stream, 1)
	cfg := conduit.Config{StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		st, err := ps.Accept()
		if err != nil {
			return err
		}
		accepted <- st
		return nil
	})}
	p := newPair(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callerGone := make(chan struct{})
	p.a.Relay.SetAfterOpenHookForTest(func(hopDone <-chan struct{}) {
		// The target has accepted; now the caller disappears before the
		// owner forwards the accept.
		cancel()
		<-hopDone
		close(callerGone)
	})
	_, err := p.remote().OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err == nil {
		t.Fatal("caller OpenStream succeeded after cancel")
	}
	relaytest.WaitClosed(t, callerGone, "caller hop to end")
	st := relaytest.Wait(t, accepted, "target accept")
	_, rerr := io.ReadAll(st)
	if code := conduit.CodeOf(rerr, 0); code != conduit.CloseCancelled {
		t.Fatalf("target stream ended with %v, want 4499", rerr)
	}
	p.a.Relay.WaitBridgesForTest()
	if n := p.a.Relay.ActiveBridges(); n != 0 {
		t.Fatalf("%d active bridges", n)
	}
	if n := p.target.Stats().OpenStreams; n != 0 {
		t.Fatalf("target has %d open streams after late accept", n)
	}
}

// TestBridgeNoLeakAfterManyStreams (T8): streams that end normally, by
// caller abort, by caller close and by target abort leave no bridge, no
// target stream and no goroutine behind (owner handler, caller-side hop
// read loops, splice copiers and resize forwarders included).
func TestBridgeNoLeakAfterManyStreams(t *testing.T) {
	const modeParam = "test_mode"
	ended := make(chan struct{}, 16)
	cfg := conduit.Config{StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, open *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		st, err := ps.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer func() { ended <- struct{}{} }()
			if open.GetParams()[modeParam] == "target_abort" {
				_ = st.CloseWithCode(conduit.CloseForbidden, "forbidden: test target abort")
			} else {
				_, _ = io.Copy(st, st)
				_ = st.(interface{ CloseWrite() error }).CloseWrite()
			}
			// Resizes is closed once the stream has ended both ways.
			for range st.(conduit.Resizable).Resizes() {
			}
		}()
		return nil
	})}
	p := newPair(t, cfg)
	rs := p.remote()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Warm up one stream so lazily started goroutines (HTTP transport
	// connection pools) are part of the baseline.
	warm, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	_ = warm.(interface{ CloseWrite() error }).CloseWrite()
	_, _ = io.ReadAll(warm)
	relaytest.Wait(t, ended, "warm-up target stream to end")
	p.a.Relay.WaitBridgesForTest()
	settle(t, "warm-up target stream removal", func() bool { return p.target.Stats().OpenStreams == 0 })
	baseline := runtime.NumGoroutine()

	modes := []string{"normal", "caller_abort", "caller_close", "target_abort"}
	for i := range 2 * len(modes) {
		mode := modes[i%len(modes)]
		st, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY, Params: map[string]string{modeParam: mode}})
		if err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "normal": // half-close round trip
			_ = st.(interface{ CloseWrite() error }).CloseWrite()
			if _, err := io.ReadAll(st); err != nil {
				t.Fatalf("normal stream: %v", err)
			}
		case "caller_abort":
			_ = st.CloseWithCode(conduit.CloseCancelled, relay.ReasonCancelled)
		case "caller_close": // closes without reading
			_ = st.Close()
		case "target_abort":
			_, err := io.ReadAll(st)
			if code := conduit.CodeOf(err, 0); code != conduit.CloseForbidden {
				t.Fatalf("target abort reached the caller as %v, want 4403", err)
			}
			_ = st.Close()
		}
		relaytest.Wait(t, ended, mode+" target stream to end")
	}
	p.a.Relay.WaitBridgesForTest()
	if n := p.a.Relay.ActiveBridges(); n != 0 {
		t.Fatalf("%d active bridges", n)
	}
	// The target's stream table and the goroutine count have no
	// completion signal; settle is bounded and only checks for leaks.
	settle(t, "target stream table to empty", func() bool { return p.target.Stats().OpenStreams == 0 })
	settle(t, "goroutines to return to the baseline", func() bool { return runtime.NumGoroutine() <= baseline })
}

// settle re-checks cond until it holds, failing after 10s. It is a leak
// check for counters without a completion signal, never synchronisation.
func settle(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
		<-time.After(time.Millisecond)
	}
}
