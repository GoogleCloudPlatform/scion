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

package tunnelclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// rpcCall is one tunnel request as the fake hub saw it.
type rpcCall struct {
	session int // 1-based dial index
	n       int // 1-based request index over all sessions
	ls      conduit.LocalSession
	req     *conduitv1.RpcRequest
	body    tunnelRequest
}

// fakeHub is an in-process conduit endpoint for user sessions: an
// httptest server that upgrades to ws and runs conduit.Accept with an RPC
// handler answering /v1/tunnels from a script.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// users maps a bearer token to the user id the hub admits it as.
	users map[string]string
	// dialStatus, when set, refuses each dial with that HTTP status and
	// the headers in dialHeader.
	dialStatus int
	dialHeader map[string]string
	// noRPC serves sessions with no RPC handler (an unknown-RPC answer).
	noRPC bool
	// handle answers a tunnel request (default: echo tunnel).
	handle func(c rpcCall) *conduitv1.RpcResponse

	dials   int
	bearers []string
	calls   []rpcCall
	live    []conduit.LocalSession
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{t: t, users: map[string]string{"tok1": "u1", "tok2": "u1", "tok3": "u2"}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(func() {
		h.mu.Lock()
		live := append([]conduit.LocalSession(nil), h.live...)
		h.mu.Unlock()
		for _, s := range live {
			_ = s.Close()
		}
		h.srv.Close()
	})
	return h
}

// set runs f under the hub's lock.
func (h *fakeHub) set(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f()
}

func (h *fakeHub) counts() (dials, calls int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dials, len(h.calls)
}

func (h *fakeHub) bearerList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bearers...)
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != conduitPath {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	h.dials++
	idx := h.dials
	status := h.dialStatus
	for k, v := range h.dialHeader {
		w.Header().Set(k, v)
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	h.bearers = append(h.bearers, tok)
	user, ok := h.users[tok]
	noRPC := h.noRPC
	h.mu.Unlock()
	if status != 0 {
		http.Error(w, "refused", status)
		return
	}
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	cfg := conduit.Config{}
	if !noRPC {
		cfg.RPCHandler = conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			return h.rpc(ctx, idx, req)
		})
	}
	s, err := conduit.Accept(context.WithoutCancel(r.Context()), c, cfg, userAdmitter{user: user, session: "s" + strconv.Itoa(idx)})
	if err != nil {
		return
	}
	ls := s.(conduit.LocalSession)
	h.mu.Lock()
	h.live = append(h.live, ls)
	h.mu.Unlock()
	<-ls.Done()
}

func (h *fakeHub) rpc(ctx context.Context, idx int, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	if req.GetPath() != TunnelsPath {
		return jsonResp(http.StatusNotFound, errBody("not_found", "unknown method", ""))
	}
	var body tunnelRequest
	_ = json.Unmarshal(req.GetBody(), &body)
	h.mu.Lock()
	c := rpcCall{session: idx, n: len(h.calls) + 1, ls: conduit.LocalSessionFromContext(ctx), req: req, body: body}
	h.calls = append(h.calls, c)
	handle := h.handle
	h.mu.Unlock()
	if handle == nil {
		return openTunnelResp(c, echo)
	}
	return handle(c)
}

// userAdmitter admits a user Hello whose principal id matches the user
// the bearer token authenticated as.
type userAdmitter struct {
	user    string
	session string
}

func (a userAdmitter) Admit(_ context.Context, hello *conduitv1.Hello) (*conduitv1.Welcome, error) {
	if hello.GetPrincipalKind() != conduitv1.PrincipalKind_PRINCIPAL_KIND_USER || hello.GetPrincipalId() != a.user {
		return nil, conduit.Reject(conduit.CloseForbidden, "forbidden")
	}
	return &conduitv1.Welcome{SessionId: a.session, RelayInstanceId: "relay-1", ConnectionEpoch: 1}, nil
}

func (userAdmitter) Refresh(context.Context, *conduitv1.AuthRefresh) error { return nil }

func jsonResp(status int, body any) *conduitv1.RpcResponse {
	data, _ := json.Marshal(body)
	return &conduitv1.RpcResponse{Status: int32(status), Headers: map[string]string{"Content-Type": "application/json"}, Body: data}
}

func errBody(code, msg, reason string) map[string]any {
	e := map[string]any{"code": code, "message": msg}
	if reason != "" {
		e["details"] = map[string]any{"reason": reason}
	}
	return map[string]any{"error": e}
}

// bareDrain is the session layer's answer on a draining session.
func bareDrain() *conduitv1.RpcResponse {
	return &conduitv1.RpcResponse{Status: http.StatusServiceUnavailable, Headers: map[string]string{conduit.RetryAfterHeader: "0"}, Body: []byte("conduit: session draining")}
}

// openTunnelResp opens the tunnel stream toward the caller (as the hub
// does, before answering), runs serve on it, and answers 200.
func openTunnelResp(c rpcCall, serve func(conduit.Stream)) *conduitv1.RpcResponse {
	kind, err := conduit.StreamKindFromProto(conduit.StreamKind(c.body.Kind).Proto())
	if err != nil {
		return jsonResp(http.StatusBadRequest, errBody("invalid_request", "bad kind", ""))
	}
	st, err := c.ls.OpenStream(context.Background(), &conduitv1.StreamOpen{Kind: kind.Proto(), Params: c.body.Params})
	if err != nil {
		return bareDrain()
	}
	go serve(st)
	d := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	return jsonResp(http.StatusOK, tunnelResponse{StreamID: st.ID(), SessionID: c.ls.Info().SessionID, AgentID: c.body.Agent, ProjectID: "p-1", Kind: c.body.Kind, AuthzDeadline: &d})
}

// echo copies the stream back to itself until the peer half-closes, then
// closes normally.
func echo(st conduit.Stream) {
	buf := make([]byte, 32*1024)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			if _, werr := st.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = st.Close()
			}
			return
		}
	}
}

// fakeREST is the REST side of the hub.
type fakeREST struct {
	mu sync.Mutex
	// users maps a token to its user; a missing token is 401.
	users map[string]User
	// whoamiErr, when set, fails every Whoami.
	whoamiErr error
	// resolveErr, when set, fails ResolveAgent.
	resolveErr error
	whoamis    int
	resolves   int
}

func newFakeREST() *fakeREST {
	return &fakeREST{users: map[string]User{"tok1": {ID: "u1"}, "tok2": {ID: "u1"}, "tok3": {ID: "u2"}}}
}

func (f *fakeREST) Whoami(_ context.Context, cred Credential) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.whoamis++
	if f.whoamiErr != nil {
		return User{}, f.whoamiErr
	}
	u, ok := f.users[cred.Token]
	if !ok {
		return User{}, &HTTPError{Status: http.StatusUnauthorized, Code: "unauthorized"}
	}
	return u, nil
}

func (f *fakeREST) ResolveAgent(_ context.Context, _ Credential, _, agent string) (Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.resolveErr != nil {
		return Target{}, f.resolveErr
	}
	return Target{AgentID: "id-" + agent, ProjectID: "p-1"}, nil
}

// credSeq returns the credentials in order, repeating the last.
func credSeq(creds ...Credential) func() Credential {
	var mu sync.Mutex
	i := 0
	return func() Credential {
		mu.Lock()
		defer mu.Unlock()
		if len(creds) == 0 {
			return Credential{}
		}
		c := creds[min(i, len(creds)-1)]
		i++
		return c
	}
}

// testConfig is a Config against h and rest with injected timing.
func testConfig(h *fakeHub, rest *fakeREST, creds func() Credential) Config {
	return Config{
		Endpoint:          h.srv.URL,
		ResolveCredential: creds,
		Hub:               rest,
		Jitter:            func() time.Duration { return 1500 * time.Millisecond },
		Sleep:             func(context.Context, time.Duration) error { return nil },
		StreamWait:        5 * time.Second,
		DialTimeout:       10 * time.Second,
		Stderr:            io.Discard,
	}
}

func oauth(tok string) Credential { return Credential{Token: tok, Kind: CredentialOAuth} }

// syncBuffer is a goroutine-safe bytes buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// stdioReq is a stdio tcp request carrying in on stdin.
func stdioReq(in io.Reader, out io.Writer) Request {
	return Request{Agent: "a1", Project: "proj", Port: 5173, Stdio: true, Stdin: in, Stdout: out}
}

// runCtx is a context bounded for one test run.
func runCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
