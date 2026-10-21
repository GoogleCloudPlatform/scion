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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
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
	if len(got) != len(want) || strings.Join(got, "|") != strings.Join(want, "|") {
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
		{"http://[::1]:8080", "http://[::1]:8080/x", true},
		{"http://[::1]:8080", "http://[::1]:8081/x", false},
		{"https://hub.example/scion", "https://hub.example/api/v1/x", true},
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

// stubTokenSource is a transportauth.TokenSource with a fixed token.
type stubTokenSource struct{ tok string }

func (s stubTokenSource) Token() (string, error)   { return s.tok, nil }
func (stubTokenSource) SetToken(string, time.Time) {}
func (stubTokenSource) Expiry() time.Time          { return time.Time{} }

func TestAuthenticatedHTTPClient_TransportAuthOnlyOnHubOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode transportauth.HeaderMode
	}{
		{"authorization", transportauth.HeaderAuthorization},
		{"proxy_authorization", transportauth.HeaderProxyAuthorization},
		{"serverless_authorization", transportauth.HeaderServerlessAuthorization},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			record := func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Host+r.URL.Path+" authorization="+r.Header.Get("Authorization")+
					" proxy="+r.Header.Get("Proxy-Authorization")+
					" serverless="+r.Header.Get("X-Serverless-Authorization"))
				mu.Unlock()
				if r.URL.Path == "/start" {
					http.Redirect(w, r, "http://storage.test/redirected", http.StatusFound)
					return
				}
				okHandler(w, r)
			}
			n := newOriginTestNet(t, map[string]http.HandlerFunc{
				"hub.test:80":     record,
				"storage.test:80": record,
			})
			hc := &http.Client{
				Transport: transportauth.Wrap(n.transport(), stubTokenSource{tok: "transport-token"}, tc.mode),
				Timeout:   10 * time.Second,
			}
			c := NewTransport("http://hub.test", WithHTTPClient(hc),
				WithAuth(&AgentTokenAuth{Token: "agent-token"})).AuthenticatedHTTPClient()
			mustGet(t, c, newGet(t, "http://hub.test/api"))
			mustGet(t, c, newGet(t, "http://storage.test/object?X-Goog-Signature=abc"))
			mustGet(t, c, newGet(t, "http://hub.test/start"))

			hubHeaders := map[transportauth.HeaderMode]string{
				transportauth.HeaderAuthorization:           " authorization=Bearer transport-token proxy= serverless=",
				transportauth.HeaderProxyAuthorization:      " authorization= proxy=Bearer transport-token serverless=",
				transportauth.HeaderServerlessAuthorization: " authorization= proxy= serverless=Bearer transport-token",
			}[tc.mode]
			none := " authorization= proxy= serverless="
			want := strings.Join([]string{
				"hub.test/api" + hubHeaders,
				"storage.test/object" + none,
				"hub.test/start" + hubHeaders,
				"storage.test/redirected" + none,
			}, "\n")
			mu.Lock()
			got := strings.Join(seen, "\n")
			mu.Unlock()
			if got != want {
				t.Errorf("transport credential headers:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestAuthenticatedHTTPClient_RedirectHopLimit(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			http.Redirect(w, r, "http://hub.test/loop", http.StatusFound)
		},
	})
	c := n.client("http://hub.test", &BearerAuth{Token: "hub-token"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := newGet(t, "http://hub.test/loop").WithContext(ctx)
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("GET with a redirect loop: got status %d, want an error", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("redirect loop error = %v, want it to mention stopped after 10 redirects", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 10 {
		t.Errorf("redirect loop requests = %d, want 10", hits)
	}
}

func TestAuthenticatedHTTPClient_BaseCheckRedirectStillApplies(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80":     redirectTo("http://storage.test/object"),
		"storage.test:80": okHandler,
	})
	calls := 0
	hc := &http.Client{
		Transport: n.transport(),
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			calls++
			return http.ErrUseLastResponse
		},
	}
	c := NewTransport("http://hub.test", WithHTTPClient(hc),
		WithAuth(&BearerAuth{Token: "hub-token"})).AuthenticatedHTTPClient()
	resp, err := c.Do(newGet(t, "http://hub.test/start"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || calls != 1 {
		t.Errorf("status %d after %d base CheckRedirect calls, want %d after 1", resp.StatusCode, calls, http.StatusFound)
	}
	wantAuth(t, n, "storage.test:80/object")
}

func TestAuthenticatedHTTPClient_BaseCheckRedirectKeepsCrossOriginDrop(t *testing.T) {
	n := newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80":   redirectTo("http://hub.test:8080/object"),
		"hub.test:8080": okHandler,
	})
	errBaseHook := errors.New("base hook saw Authorization")
	hc := &http.Client{
		Transport: n.transport(),
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.Header.Get("Authorization") != "" {
				return errBaseHook
			}
			return nil
		},
	}
	c := NewTransport("http://hub.test", WithHTTPClient(hc)).AuthenticatedHTTPClient()
	req := newGet(t, "http://hub.test/start")
	req.Header.Set("Authorization", "Bearer caller-set")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET: %v (Authorization still set when the base CheckRedirect ran)", err)
	}
	_ = resp.Body.Close()
	wantAuth(t, n, "hub.test:80/start", "Bearer caller-set")
	wantAuth(t, n, "hub.test:8080/object", "")
}
