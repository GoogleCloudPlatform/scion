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
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
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
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
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
// owner is 503 (unavailable), not a stale route and not served.
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
	if errors.Is(err, relay.ErrStaleRoute) || conduit.CodeOf(err, 0) != conduit.CloseRelayRestart {
		t.Fatalf("Call = %v, want 4503 (fail closed)", err)
	}
}

// --- C7: relay-peer identity and user sessions ---

func signed(t *testing.T, auth relay.PeerAuth, method, url string, body []byte, want string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
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
	wrongKey, _ := relay.NewHMACPeerAuth(relay.HMACPeerAuthConfig{Key: bytes.Repeat([]byte{9}, 32), SelfID: "relay-b"})
	stale, _ := relay.NewHMACPeerAuth(relay.HMACPeerAuthConfig{Key: p.w.PeerKey, SelfID: "relay-b",
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
			{"no identity", func() *http.Request { return signed(t, nil, rt.method, rt.url, rt.body, want) }},
			{"wrong key", func() *http.Request { return signed(t, wrongKey, rt.method, rt.url, rt.body, want) }},
			{"stale timestamp", func() *http.Request { return signed(t, stale, rt.method, rt.url, rt.body, want) }},
			{"tampered want", func() *http.Request {
				r := signed(t, p.w.PeerAuth("relay-b"), rt.method, rt.url, rt.body, want)
				r.Header.Set(relay.HeaderWant, `{"project_id":"other","incarnation":"L1"}`)
				return r
			}},
			{"tampered path", func() *http.Request {
				r := signed(t, p.w.PeerAuth("relay-b"), rt.method, rt.url, rt.body, want)
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
		req := signed(t, p.w.PeerAuth("relay-b"), http.MethodGet, base+"self", nil, "")
		replay := req.Clone(context.Background())
		if got := do(t, req); got != http.StatusOK {
			t.Fatalf("first request %d, want 200", got)
		}
		if got := do(t, replay); got != http.StatusUnauthorized {
			t.Fatalf("replay %d, want 401", got)
		}
	})
	t.Run("rpc body swapped after signing", func(t *testing.T) {
		req := signed(t, p.w.PeerAuth("relay-b"), http.MethodPost, routes[1].url, rpcBody, want)
		other, _ := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r2", Method: "exec"})
		req.Body = io.NopCloser(bytes.NewReader(other))
		req.ContentLength = int64(len(other))
		if got := do(t, req); got != http.StatusBadRequest {
			t.Fatalf("status %d, want 400 (digest mismatch)", got)
		}
	})
}

// TestUserSessionNotRoutable (C7): the internal API refuses to route to a
// user session (403).
func TestUserSessionNotRoutable(t *testing.T) {
	p := newPair(t, echoConfig())
	p.w.SetPrincipal("u", relay.Principal{Kind: registry.PrincipalUser, ID: "user-1"})
	_, wel := p.a.MustDial("u", relaytest.UserHello("user-1"), conduit.Config{})
	url := p.a.Internal.URL + relay.InternalPathPrefix + "sessions/" + wel.GetSessionId() + "/rpc"
	body, _ := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1"})
	if got := do(t, signed(t, p.w.PeerAuth("relay-b"), http.MethodPost, url, body, `{"incarnation":""}`)); got != http.StatusForbidden {
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

// TestBridgeNoLeakAfterManyStreams: streams that end normally, by caller
// abort and by target abort leave no bridge and no target stream behind.
func TestBridgeNoLeakAfterManyStreams(t *testing.T) {
	p := newPair(t, echoConfig())
	rs := p.remote()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 6 {
		st, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
		if err != nil {
			t.Fatal(err)
		}
		switch i % 3 {
		case 0: // normal half-close round trip
			_ = st.(interface{ CloseWrite() error }).CloseWrite()
			_, _ = io.ReadAll(st)
		case 1: // caller aborts
			_ = st.CloseWithCode(conduit.CloseCancelled, "bye")
		case 2: // caller closes without reading
			_ = st.Close()
		}
	}
	p.a.Relay.WaitBridgesForTest()
	if n := p.a.Relay.ActiveBridges(); n != 0 {
		t.Fatalf("%d active bridges", n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for p.target.Stats().OpenStreams != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("target has %d open streams", p.target.Stats().OpenStreams)
		}
		time.Sleep(10 * time.Millisecond) // polling a counter with a deadline, not ordering
	}
}
