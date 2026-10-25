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

package runtimebroker

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// prefixedInstance is one Runtime Broker instance served under its own
// prefix behind a shared listener.
type prefixedInstance struct {
	id     string
	prefix string
	srv    *Server
}

func newPrefixedInstance(id string) *prefixedInstance {
	cfg := DefaultServerConfig()
	cfg.BrokerID = id
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = true
	cfg.BrokerAuthStrictMode = true
	return &prefixedInstance{
		id:     id,
		prefix: "/instances/" + id,
		srv:    New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{}),
	}
}

// setUpLate gives the instance a hub connection and builds its broker
// authentication, after its handler was built. It returns the key.
func (p *prefixedInstance) setUpLate(t *testing.T) []byte {
	t.Helper()
	creds := makeTestCreds("hub-"+p.id, p.id, "http://127.0.0.1:1")
	conn, err := p.srv.createHubConnection(creds.Name, creds)
	if err != nil {
		t.Fatal(err)
	}
	p.srv.hubMu.Lock()
	p.srv.hubConnections[creds.Name] = conn
	p.srv.hubMu.Unlock()
	p.srv.buildAuthMiddleware()
	if p.srv.loadBrokerMiddleware() == nil {
		t.Fatalf("precondition: instance %s has no broker authentication after setup", p.id)
	}
	key, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// sharedListener routes /instances/<id>/... to that instance's
// PrefixedHandler and answers 404 otherwise, as the host's listener does.
// The prefixed handlers are built once, here, before any instance has its
// broker authentication.
func sharedListener(instances ...*prefixedInstance) http.Handler {
	byPrefix := map[string]http.Handler{}
	for _, p := range instances {
		byPrefix[p.prefix] = p.srv.PrefixedHandler(p.prefix)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for prefix, h := range byPrefix {
			if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
				h.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
}

// prefixedStatus sends GET path to h, signed for brokerID with key over
// signedPath (the path the signature covers) unless key is nil.
func prefixedStatus(h http.Handler, path, signedPath, brokerID string, key []byte) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != nil {
		signed := httptest.NewRequest(http.MethodGet, signedPath, nil)
		signRequest(signed, brokerID, key)
		for k, v := range signed.Header {
			req.Header[k] = v
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestPrefixedHandler_LateBrokerAuthAppliesPerInstance: several instances
// share one listener whose prefixed handlers were built before any broker
// authentication existed. Authentication set up afterwards applies to the
// already-built prefixed handlers, verifies the signature over the full,
// prefixed path, and admits only the instance's own key.
func TestPrefixedHandler_LateBrokerAuthAppliesPerInstance(t *testing.T) {
	a := newPrefixedInstance("broker-a")
	b := newPrefixedInstance("broker-b")
	h := sharedListener(a, b)

	agentsA := a.prefix + "/api/v1/agents"
	agentsB := b.prefix + "/api/v1/agents"

	keyA := a.setUpLate(t)

	if got := prefixedStatus(h, agentsA, agentsA, a.id, keyA); got != http.StatusOK {
		t.Errorf("instance A, signed over its prefixed path after late setup: got %d, want %d", got, http.StatusOK)
	}
	if got := prefixedStatus(h, agentsA, "", "", nil); got != http.StatusUnauthorized {
		t.Errorf("instance A, unsigned prefixed request after late setup: got %d, want %d", got, http.StatusUnauthorized)
	}
	if got := prefixedStatus(h, agentsA, "/api/v1/agents", a.id, keyA); got != http.StatusUnauthorized {
		t.Errorf("instance A, signed over the stripped path only: got %d, want %d (the signature covers the prefixed path)", got, http.StatusUnauthorized)
	}
	if got := prefixedStatus(h, a.prefix+"/healthz", "", "", nil); got != http.StatusOK {
		t.Errorf("instance A, unauthenticated prefixed /healthz: got %d, want %d", got, http.StatusOK)
	}

	keyB := b.setUpLate(t)

	if got := prefixedStatus(h, agentsB, agentsB, b.id, keyB); got != http.StatusOK {
		t.Errorf("instance B, signed over its prefixed path after late setup: got %d, want %d", got, http.StatusOK)
	}
	if got := prefixedStatus(h, agentsB, "", "", nil); got != http.StatusUnauthorized {
		t.Errorf("instance B, unsigned prefixed request after late setup: got %d, want %d", got, http.StatusUnauthorized)
	}
	if got := prefixedStatus(h, agentsB, agentsB, a.id, keyA); got != http.StatusUnauthorized {
		t.Errorf("instance B, signed with instance A's key and ID: got %d, want %d", got, http.StatusUnauthorized)
	}
	if got := prefixedStatus(h, agentsA, agentsA, b.id, keyB); got != http.StatusUnauthorized {
		t.Errorf("instance A, signed with instance B's key and ID: got %d, want %d", got, http.StatusUnauthorized)
	}
	if got := prefixedStatus(h, agentsA, agentsA, a.id, keyA); got != http.StatusOK {
		t.Errorf("instance A, own key, after B's setup: got %d, want %d", got, http.StatusOK)
	}
}

// TestPrefixedHandler_LateBrokerAuthConcurrentRequests sends prefixed
// requests to several instances while their broker authentication is
// rebuilt, for the race detector. Once each instance has its
// authentication, an unsigned request is refused and its own signed request
// is admitted throughout.
func TestPrefixedHandler_LateBrokerAuthConcurrentRequests(t *testing.T) {
	a := newPrefixedInstance("broker-a")
	b := newPrefixedInstance("broker-b")
	h := sharedListener(a, b)
	keys := map[*prefixedInstance][]byte{a: a.setUpLate(t), b: b.setUpLate(t)}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for _, p := range []*prefixedInstance{a, b} {
		wg.Add(2)
		go func(p *prefixedInstance) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				p.srv.buildAuthMiddleware()
			}
		}(p)
		go func(p *prefixedInstance) {
			defer wg.Done()
			path := p.prefix + "/api/v1/agents"
			for i := 0; i < 50; i++ {
				if got := prefixedStatus(h, path, path, p.id, keys[p]); got != http.StatusOK {
					errs <- p.id + ": signed request refused during rebuild"
					return
				}
				if got := prefixedStatus(h, path, "", "", nil); got != http.StatusUnauthorized {
					errs <- p.id + ": unsigned request admitted during rebuild"
					return
				}
			}
		}(p)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
