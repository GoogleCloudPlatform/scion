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

package apiclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// headerLog records the credential headers each request path received.
type headerLog struct {
	mu   sync.Mutex
	seen map[string][]string
}

func (l *headerLog) record(name string, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]string{}
	}
	l.seen[name] = append(l.seen[name], r.Header.Get("Authorization"))
}

// got returns the Authorization values that reached name, in order.
func (l *headerLog) got(name string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen[name]...)
}

// originTestNet serves named hosts ("hub.test:80", "storage.test:80",
// "hub.test:8080") from httptest servers on loopback. Its transport dials
// each name to its server, so the client sees distinct origins.
type originTestNet struct {
	addrs map[string]string
	log   headerLog
}

func newOriginTestNet(t *testing.T, routes map[string]http.HandlerFunc) *originTestNet {
	t.Helper()
	n := &originTestNet{addrs: map[string]string{}}
	for hostport, h := range routes {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.log.record(hostport+r.URL.Path, r)
			h(w, r)
		}))
		t.Cleanup(srv.Close)
		n.addrs[hostport] = srv.Listener.Addr().String()
	}
	return n
}

func (n *originTestNet) transport() *http.Transport {
	var d net.Dialer
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target, ok := n.addrs[addr]
			if !ok {
				return nil, fmt.Errorf("no test server for %s", addr)
			}
			return d.DialContext(ctx, network, target)
		},
	}
}

func (n *originTestNet) client(baseURL string, auth Authenticator) *http.Client {
	return NewTransport(baseURL,
		WithHTTPClient(&http.Client{Transport: n.transport(), Timeout: 10 * time.Second}),
		WithAuth(auth)).AuthenticatedHTTPClient()
}

func okHandler(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }

func redirectTo(loc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
		okHandler(w, r)
	}
}

func mustGet(t *testing.T, c *http.Client, req *http.Request) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", req.URL, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", req.URL, resp.StatusCode)
	}
}

func newGet(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s): %v", rawURL, err)
	}
	return req
}

func wantAuth(t *testing.T, n *originTestNet, path string, want ...string) {
	t.Helper()
	got := n.log.got(path)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Authorization at %s = %q, want %q", path, got, want)
	}
}

const hubToken = "Bearer hub-token"

func TestAuthenticatedHTTPClient_HubRequestCarriesCredential(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{"hub.test:80": okHandler})
	c := n.client("http://hub.test", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://hub.test/api/v1/files/x"))
	wantAuth(t, n, "hub.test:80/api/v1/files/x", hubToken)
}

func TestAuthenticatedHTTPClient_SameOriginRedirectKeepsCredential(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80": redirectTo("http://hub.test/next"),
	})
	c := n.client("http://hub.test", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://hub.test/start"))
	wantAuth(t, n, "hub.test:80/start", hubToken)
	wantAuth(t, n, "hub.test:80/next", hubToken)
}

func TestAuthenticatedHTTPClient_CrossOriginRedirectOmitsCredential(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80":     redirectTo("http://storage.test/object"),
		"storage.test:80": okHandler,
	})
	c := n.client("http://hub.test", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://hub.test/start"))
	wantAuth(t, n, "hub.test:80/start", hubToken)
	wantAuth(t, n, "storage.test:80/object", "")
}

func TestAuthenticatedHTTPClient_CrossOriginRedirectDropsCallerAuthorization(t *testing.T) {
	// net/http keeps a caller-set Authorization header on a redirect to the
	// same host name on another port; this client drops it there.
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80":   redirectTo("http://hub.test:8080/object"),
		"hub.test:8080": okHandler,
	})
	c := n.client("http://hub.test", nil)
	req := newGet(t, "http://hub.test/start")
	req.Header.Set("Authorization", "Bearer caller-set")
	mustGet(t, c, req)
	wantAuth(t, n, "hub.test:80/start", "Bearer caller-set")
	wantAuth(t, n, "hub.test:8080/object", "")
}

func TestAuthenticatedHTTPClient_SignedURLOnOtherHostOmitsCredential(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	recordTokens := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Host+"="+r.Header.Get("X-Scion-Agent-Token")+","+r.Header.Get("X-Scion-Run-Id"))
		mu.Unlock()
		okHandler(w, r)
	}
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80":     recordTokens,
		"storage.test:80": recordTokens,
	})
	c := n.client("http://hub.test", &AgentTokenAuth{Token: "agent-token", RunID: "run-1"})
	mustGet(t, c, newGet(t, "http://storage.test/bucket/object?X-Goog-Signature=abc"))
	mustGet(t, c, newGet(t, "http://hub.test/api/v1/files/x"))
	mu.Lock()
	got := strings.Join(tokens, "|")
	mu.Unlock()
	if want := "storage.test=,|hub.test=agent-token,run-1"; got != want {
		t.Errorf("agent token headers = %q, want %q", got, want)
	}

	b := newOriginTestNet(t, map[string]http.HandlerFunc{"storage.test:80": okHandler})
	c = b.client("http://hub.test", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://storage.test/bucket/object?X-Goog-Signature=abc"))
	wantAuth(t, b, "storage.test:80/bucket/object", "")
}

func TestAuthenticatedHTTPClient_OtherPortOnHubHostOmitsCredential(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80":   okHandler,
		"hub.test:8080": okHandler,
	})
	c := n.client("http://hub.test", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://hub.test:8080/object"))
	mustGet(t, c, newGet(t, "http://hub.test/api"))
	wantAuth(t, n, "hub.test:8080/object", "")
	wantAuth(t, n, "hub.test:80/api", hubToken)
}

func TestAuthenticatedHTTPClient_DefaultPortMatchesExplicitPort(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{"hub.test:80": okHandler})
	// Base URL without a port, request with the explicit default port.
	c := n.client("http://hub.test", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://hub.test:80/a"))
	// Base URL with the explicit default port (and upper-case host),
	// request without a port.
	c = n.client("http://HUB.test:80/", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://hub.test/b"))
	wantAuth(t, n, "hub.test:80/a", hubToken)
	wantAuth(t, n, "hub.test:80/b", hubToken)
}

func TestOriginOf_DefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"https://hub.example", "https://hub.example:443/x", true},
		{"https://hub.example:443", "https://HUB.example/x", true},
		{"http://hub.example", "http://hub.example:80/x", true},
		{"https://hub.example", "http://hub.example/x", false},
		{"https://hub.example", "https://hub.example:8443/x", false},
		{"http://hub.example", "http://hub.example:443/x", false},
		{"https://hub.example", "https://other.example/x", false},
		{"https://hub.example", "file:///tmp/x", false},
	} {
		want := parseOrigin(tc.a)
		req := newGet(t, tc.b)
		if got := sameOrigin(req.URL, want); got != tc.same {
			t.Errorf("sameOrigin(%q, origin of %q) = %v, want %v", tc.b, tc.a, got, tc.same)
		}
	}
}

func TestAuthenticatedHTTPClient_NoUsableBaseURLSendsNoCredential(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{"hub.test:80": okHandler})
	c := n.client("", &BearerAuth{Token: "hub-token"})
	mustGet(t, c, newGet(t, "http://hub.test/x"))
	wantAuth(t, n, "hub.test:80/x", "")
}
