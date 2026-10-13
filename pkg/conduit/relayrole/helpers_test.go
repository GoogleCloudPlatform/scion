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

package relayrole

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// noopPeerAuth signs nothing; the fake hub does not authenticate.
type noopPeerAuth struct{}

func (noopPeerAuth) Sign(*http.Request) error             { return nil }
func (noopPeerAuth) Verify(*http.Request) (string, error) { return "relay", nil }

// hubAnswer is a fake hub's answer to one call.
type hubAnswer struct {
	status int
	body   any
	header http.Header
}

// fakeHub serves the hub internal API paths with programmable answers and
// records every call.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    map[string]int
	requests map[string][][]byte
	answer   func(path string, body []byte) hubAnswer
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{t: t, calls: map[string]int{}, requests: map[string][][]byte{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := pathKey(r.URL.Path)
		h.mu.Lock()
		h.calls[key]++
		h.requests[key] = append(h.requests[key], body)
		answer := h.answer
		h.mu.Unlock()
		a := hubAnswer{status: http.StatusServiceUnavailable}
		if answer != nil {
			a = answer(key, body)
		}
		for k, v := range a.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		if a.body != nil {
			_ = json.NewEncoder(w).Encode(a.body)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// pathKey names a hub call: the operation, with session ids dropped.
func pathKey(path string) string {
	switch {
	case path == relay.HubAdmitPath:
		return "admit"
	case path == relay.HubSessionRecheckPath:
		return "sessions/recheck"
	case path == relay.HubStreamRecheckPath:
		return "streams/recheck"
	case strings.HasSuffix(path, "/rpc"):
		return "rpc"
	case strings.HasSuffix(path, "/refresh"):
		return "refresh"
	}
	return path
}

func (h *fakeHub) setAnswer(f func(path string, body []byte) hubAnswer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answer = f
}

func (h *fakeHub) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[path]
}

// last decodes the last request body sent to path into v.
func (h *fakeHub) last(path string, v any) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	reqs := h.requests[path]
	if len(reqs) == 0 {
		h.t.Fatalf("no %s call", path)
	}
	if err := json.Unmarshal(reqs[len(reqs)-1], v); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHub) client() *relay.HubClient {
	return &relay.HubClient{BaseURL: h.srv.URL, Auth: noopPeerAuth{}}
}

// fakeEvents is an Events whose events and reconnects the test drives.
type fakeEvents struct {
	mu       sync.Mutex
	ch       chan string
	onListen []func()
}

func newFakeEvents() *fakeEvents { return &fakeEvents{ch: make(chan string, 16)} }

func (e *fakeEvents) Subscribe(...string) (<-chan string, func()) { return e.ch, func() {} }

func (e *fakeEvents) AddOnListen(fn func()) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onListen = append(e.onListen, fn)
	return func() {}
}

func (e *fakeEvents) reconnect() {
	e.mu.Lock()
	fns := append([]func(){}, e.onListen...)
	e.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func mustMarshalRPC(t *testing.T, m *conduitv1.RpcResponse) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
