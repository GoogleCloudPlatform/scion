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

package conduit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const (
	testAgentID   = "agent-1"
	testProjectID = "project-1"
	waitTimeout   = 10 * time.Second
)

// testKey is a grant signing key and its public half.
type testKey struct {
	signer grant.Signer
	public grant.PublicKey
}

func newTestKey(t *testing.T, kid string) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{signer: grant.Signer{KeyID: kid, Key: priv}, public: grant.PublicKey{KeyID: kid, Key: pub}}
}

// fakeHub plays the hub's conduit endpoint and grant-key route.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	status  int   // non-zero: refuse the upgrade with this status
	reject  error // non-nil: refuse admission with this error
	welcome func(*conduitv1.Hello) *conduitv1.Welcome
	keys    []grant.PublicKey // served by the grant-key route
	tokens  []string          // X-Scion-Agent-Token of every conduit request

	conduitHits atomic.Int64
	keyHits     atomic.Int64
	hellos      chan *conduitv1.Hello
	sessions    chan core.LocalSession
}

func newFakeHub(t *testing.T, keys ...grant.PublicKey) *fakeHub {
	h := &fakeHub{
		t:        t,
		keys:     keys,
		hellos:   make(chan *conduitv1.Hello, 16),
		sessions: make(chan core.LocalSession, 16),
	}
	h.welcome = func(hello *conduitv1.Hello) *conduitv1.Welcome {
		inc := hello.GetCapabilities().GetEndpointIncarnation()
		if inc == "" {
			inc = "gen-1"
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return &conduitv1.Welcome{
			SessionId:           "sess-" + strconv.FormatInt(h.conduitHits.Load(), 10),
			RelayInstanceId:     "relay-a",
			ConnectionEpoch:     h.conduitHits.Load(),
			GrantKeys:           target.KeysToProto(h.keys),
			EndpointIncarnation: inc,
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/conduit", h.serveConduit)
	mux.HandleFunc("/api/v1/conduit/grant-keys", func(w http.ResponseWriter, r *http.Request) {
		h.keyHits.Add(1)
		if r.Header.Get("X-Scion-Agent-Token") == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		h.mu.Lock()
		keys := grant.ToWire(h.keys)
		h.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) serveConduit(w http.ResponseWriter, r *http.Request) {
	h.conduitHits.Add(1)
	h.mu.Lock()
	h.tokens = append(h.tokens, r.Header.Get("X-Scion-Agent-Token"))
	status, reject, welcome := h.status, h.reject, h.welcome
	h.mu.Unlock()
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	adm := admitFunc(func(_ context.Context, hello *conduitv1.Hello) (*conduitv1.Welcome, error) {
		h.hellos <- hello
		if reject != nil {
			return nil, reject
		}
		return welcome(hello), nil
	})
	s, err := core.Accept(r.Context(), conn, core.Config{}, adm)
	if err != nil {
		return
	}
	ls := s.(core.LocalSession)
	h.sessions <- ls
	<-ls.Done()
}

func (h *fakeHub) set(fn func(h *fakeHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

func (h *fakeHub) nextSession(t *testing.T) core.LocalSession {
	t.Helper()
	select {
	case s := <-h.sessions:
		return s
	case <-time.After(waitTimeout):
		t.Fatal("no conduit session")
		return nil
	}
}

func (h *fakeHub) nextHello(t *testing.T) *conduitv1.Hello {
	t.Helper()
	select {
	case hello := <-h.hellos:
		return hello
	case <-time.After(waitTimeout):
		t.Fatal("no Hello")
		return nil
	}
}

type admitFunc func(context.Context, *conduitv1.Hello) (*conduitv1.Welcome, error)

func (f admitFunc) Admit(ctx context.Context, h *conduitv1.Hello) (*conduitv1.Welcome, error) {
	return f(ctx, h)
}

func (admitFunc) Refresh(context.Context, *conduitv1.AuthRefresh) error {
	return core.Reject(core.CloseUnauthenticated, "unauthenticated")
}

// noBackoff redials at once.
func noBackoff() *core.Backoff { return &core.Backoff{Rand: func(int64) int64 { return 0 }} }

// startAgent runs an Agent against h until the test ends and returns it
// with a channel carrying Run's result.
func startAgent(t *testing.T, h *fakeHub, mod func(*Options)) (*Agent, <-chan error) {
	t.Helper()
	opts := Options{
		HubURL:    h.srv.URL,
		AgentID:   testAgentID,
		ProjectID: testProjectID,
		LaunchID:  "launch-1",
		Token:     func() string { return "token-1" },
		Backoff:   noBackoff(),
	}
	if mod != nil {
		mod(&opts)
	}
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(waitTimeout):
			t.Error("Run did not return after cancel")
		}
	})
	return a, done
}

// runResult waits for Run's result.
func runResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return")
		return nil
	}
}

// tcpOpen builds a TCP StreamOpen for port with a grant minted by key
// against the session described by w (incarnation overrides
// w.endpoint_incarnation when non-empty).
func tcpOpen(t *testing.T, key testKey, w core.SessionInfo, incarnation string, port int) *conduitv1.StreamOpen {
	t.Helper()
	params := map[string]string{grant.ParamHost: loopbackHost, grant.ParamPort: strconv.Itoa(port)}
	now := time.Now().Truncate(time.Second)
	tok, err := grant.Mint(&key.signer, grant.Claims{
		Issuer:    "scion-hub",
		Subject:   "user:u1",
		ProjectID: testProjectID,
		Target: grant.Target{
			Kind:                grant.TargetKindAgent,
			ID:                  testAgentID,
			EndpointIncarnation: incarnation,
			SessionID:           w.SessionID,
			ConnectionEpoch:     w.ConnectionEpoch,
		},
		Stream:    grant.StreamHeader{Kind: grant.StreamKindTCP, Params: params},
		NotBefore: now,
		Expiry:    now.Add(50 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_TCP, Params: params, Grant: tok}
}
