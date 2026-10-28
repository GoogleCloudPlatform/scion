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

package bridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

const (
	testFederationToken = "fed-caller-token"
	testTransportToken  = "transport-oidc-token"
	testHubName         = "hub.example.com"
)

// recordedRequest is what a test server saw of one request.
type recordedRequest struct {
	scheme string
	host   string
	header http.Header
}

// headerRecorder is an httptest server that records every request it
// receives and answers with an empty JSON object.
type headerRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func newHeaderRecorder(t *testing.T) *headerRecorder {
	t.Helper()
	rec := &headerRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(rec.serve))
	t.Cleanup(rec.Close)
	return rec
}

// newTLSHeaderRecorder is newHeaderRecorder over https. httptest serves
// every TLS server with the same certificate.
func newTLSHeaderRecorder(t *testing.T) *headerRecorder {
	t.Helper()
	rec := &headerRecorder{}
	rec.Server = httptest.NewTLSServer(http.HandlerFunc(rec.serve))
	t.Cleanup(rec.Close)
	return rec
}

func (r *headerRecorder) serve(w http.ResponseWriter, req *http.Request) {
	r.record(req)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

func (r *headerRecorder) record(req *http.Request) {
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedRequest{scheme: scheme, host: req.Host, header: req.Header.Clone()})
}

func (r *headerRecorder) requests() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.reqs...)
}

// port returns the port the recorder listens on.
func (r *headerRecorder) port(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// withHostname returns rawURL with its hostname replaced and its port kept.
func withHostname(t *testing.T, rawURL, hostname string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort(hostname, u.Port())
	return u.String()
}

// installTestTransport replaces http.DefaultTransport, which both call sites
// build on, with one that trusts the httptest certificate (for any server
// name) and dials every host name on loopback, keeping the port. It returns
// the server TLS configuration for that certificate. The global transport
// is restored on cleanup; tests that use this helper must not call
// t.Parallel.
func installTestTransport(t *testing.T) *tls.Config {
	t.Helper()
	certSrv := httptest.NewTLSServer(http.NotFoundHandler())
	serverTLS := certSrv.TLS.Clone()
	roots := certSrv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	certSrv.Close()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	prev := http.DefaultTransport
	testTransport := &http.Transport{
		// The httptest certificate names example.com; check every server
		// against that name so test host names need no certificate.
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if net.ParseIP(host) == nil {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	http.DefaultTransport = testTransport
	t.Cleanup(func() {
		http.DefaultTransport = prev
		testTransport.CloseIdleConnections()
	})
	return serverTLS
}

// schemeMuxListener serves https and plain http on one port: a connection
// whose first byte starts a TLS handshake is wrapped in a TLS server.
type schemeMuxListener struct {
	net.Listener
	tlsConfig *tls.Config
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (l *schemeMuxListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, peekErr := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	pc := &peekedConn{Conn: c, r: br}
	if peekErr == nil && first[0] == 0x16 { // TLS handshake record
		return tls.Server(pc, l.tlsConfig), nil
	}
	return pc, nil
}

// originHub is a hub double that serves https and plain http on one port.
// A request without ?final=1 is redirected to redirectTo(r); a request with
// it is answered and recorded. When redirectTo is nil every request is
// answered.
type originHub struct {
	headerRecorder
	port     string
	endpoint string // https://<host>:<port>
}

func newOriginHub(t *testing.T, serverTLS *tls.Config, network, host string, redirectTo func(r *http.Request) string) *originHub {
	t.Helper()
	loopback := "127.0.0.1:0"
	if network == "tcp6" {
		loopback = "[::1]:0"
	}
	ln, err := net.Listen(network, loopback)
	if err != nil {
		t.Skipf("no %s loopback: %v", network, err)
	}
	h := &originHub{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirectTo != nil && r.URL.Query().Get("final") != "1" {
			http.Redirect(w, r, redirectTo(r), http.StatusFound)
			return
		}
		h.serve(w, r)
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(&schemeMuxListener{Listener: ln, tlsConfig: serverTLS}) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, h.port, _ = net.SplitHostPort(ln.Addr().String())
	h.endpoint = "https://" + net.JoinHostPort(host, h.port)
	return h
}

// staticTransportSource is a transportauth.TokenSource with a fixed token.
type staticTransportSource struct{}

func (staticTransportSource) Token() (string, error)     { return testTransportToken, nil }
func (staticTransportSource) SetToken(string, time.Time) {}
func (staticTransportSource) Expiry() time.Time          { return time.Time{} }

// transportAuthModes are the transport credential setups the call-site tests
// run under: none, and each transportauth header mode.
var transportAuthModes = map[string]*transportauth.HeaderMode{
	"no transport auth":          nil,
	"Authorization":              ptrMode(transportauth.HeaderAuthorization),
	"Proxy-Authorization":        ptrMode(transportauth.HeaderProxyAuthorization),
	"X-Serverless-Authorization": ptrMode(transportauth.HeaderServerlessAuthorization),
}

func ptrMode(m transportauth.HeaderMode) *transportauth.HeaderMode { return &m }

// newFederationTestBridge returns a bridge whose configured hub is endpoint,
// with transport auth in mode when mode is not nil.
func newFederationTestBridge(t *testing.T, endpoint string, mode ...*transportauth.HeaderMode) *Bridge {
	t.Helper()
	store, err := state.NewSQLite(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &Config{
		Hub:  HubConfig{Endpoint: endpoint, User: "admin@test"},
		Auth: AuthConfig{Scheme: "federation"},
	}
	var opts []BridgeOption
	if len(mode) > 0 && mode[0] != nil {
		opts = append(opts, WithTransportAuth(staticTransportSource{}, *mode[0]))
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, testMinter(t), cfg, nil, log, opts...)
	t.Cleanup(b.Shutdown) // runs before the store is closed
	return b
}

func federationCaller() *CallerIdentity {
	return &CallerIdentity{TokenType: "federation", RawToken: testFederationToken}
}

// federationCallSites are the two hub calls made with a federation caller's
// credential. A third call site must be added here.
var federationCallSites = map[string]func(t *testing.T, b *Bridge) error{
	"federatedUserGet": func(t *testing.T, b *Bridge) error {
		return b.federatedUserGet(context.Background(), federationCaller(), "/api/v1/auth/me", nil, nil)
	},
	"callerHubClient": func(t *testing.T, b *Bridge) error {
		client, err := b.callerHubClient(federationCaller())
		if err != nil {
			return err
		}
		_, err = client.Agents().List(t.Context(), nil)
		return err
	},
}

// containsURL reports whether text contains u, not followed by another
// port digit.
func containsURL(text, u string) bool {
	for i := strings.Index(text, u); i >= 0; {
		end := i + len(u)
		if end == len(text) || text[end] < '0' || text[end] > '9' {
			return true
		}
		next := strings.Index(text[end:], u)
		if next < 0 {
			return false
		}
		i = end + next
	}
	return false
}

// assertHubCredentials checks the credentials of a request the hub answered
// for a federation caller with transport auth in mode (nil: none).
func assertHubCredentials(t *testing.T, got recordedRequest, mode *transportauth.HeaderMode) {
	t.Helper()
	transportBearer := "Bearer " + testTransportToken
	if tok := got.header.Get(FederationTokenHeader); tok != testFederationToken {
		t.Errorf("%s = %q, want %q", FederationTokenHeader, tok, testFederationToken)
	}
	// Both call sites send the bridge's minted credential as the bearer.
	if a := got.header.Get("Authorization"); !strings.HasPrefix(a, "Bearer ") || a == transportBearer {
		t.Errorf("Authorization = %q, want the bridge's minted bearer", a)
	}
	wantProxy, wantServerless := "", ""
	if mode != nil {
		switch *mode {
		case transportauth.HeaderProxyAuthorization:
			wantProxy = transportBearer
		case transportauth.HeaderServerlessAuthorization:
			wantServerless = transportBearer
		}
	}
	if p := got.header.Get("Proxy-Authorization"); p != wantProxy {
		t.Errorf("Proxy-Authorization = %q, want %q", p, wantProxy)
	}
	if s := got.header.Get("X-Serverless-Authorization"); s != wantServerless {
		t.Errorf("X-Serverless-Authorization = %q, want %q", s, wantServerless)
	}
}

func TestFederationCallSites_HubOriginRequestsCarryCredentials(t *testing.T) {
	cases := map[string]func(r *http.Request) string{
		"direct request":       nil,
		"same-origin redirect": func(r *http.Request) string { return r.URL.Path + "?final=1" },
	}
	for caseName, redirectTo := range cases {
		for modeName, mode := range transportAuthModes {
			for site, call := range federationCallSites {
				t.Run(caseName+"/"+modeName+"/"+site, func(t *testing.T) {
					serverTLS := installTestTransport(t)
					hub := newOriginHub(t, serverTLS, "tcp", testHubName, redirectTo)
					if err := call(t, newFederationTestBridge(t, hub.endpoint, mode)); err != nil {
						t.Fatalf("call: %v", err)
					}
					got := hub.requests()
					if len(got) != 1 {
						t.Fatalf("hub answered %d requests, want 1", len(got))
					}
					if got[0].scheme != "https" {
						t.Errorf("hub answered over %s, want https", got[0].scheme)
					}
					assertHubCredentials(t, got[0], mode)
				})
			}
		}
	}
}

func TestFederationCallSites_OtherOriginRedirectRefused(t *testing.T) {
	const user, password = "hub-user", "s3cret-pw"
	// Each case redirects the first hub request to an origin other than the
	// hub's. other is an https server on another port; a target on the hub's
	// own port carries ?final=1 so the hub would answer and record it.
	cases := map[string]func(r *http.Request, hubPort, otherPort string) string{
		"other host": func(r *http.Request, _, otherPort string) string {
			return "https://other.example.com:" + otherPort + r.URL.Path
		},
		"sibling port": func(r *http.Request, _, otherPort string) string {
			return "https://" + testHubName + ":" + otherPort + r.URL.Path
		},
		"subdomain": func(r *http.Request, _, otherPort string) string {
			return "https://sub." + testHubName + ":" + otherPort + r.URL.Path
		},
		"scheme downgrade": func(r *http.Request, hubPort, _ string) string {
			return "http://" + testHubName + ":" + hubPort + r.URL.Path + "?final=1"
		},
		"trailing-dot host": func(r *http.Request, hubPort, _ string) string {
			return "https://" + testHubName + ".:" + hubPort + r.URL.Path + "?final=1"
		},
		"user information in Location": func(r *http.Request, _, otherPort string) string {
			return "https://" + user + ":" + password + "@other.example.com:" + otherPort + r.URL.Path
		},
	}
	for caseName, target := range cases {
		for modeName, mode := range transportAuthModes {
			for site, call := range federationCallSites {
				t.Run(caseName+"/"+modeName+"/"+site, func(t *testing.T) {
					serverTLS := installTestTransport(t)
					other := newTLSHeaderRecorder(t)
					otherPort := other.port(t)
					var hub *originHub
					hub = newOriginHub(t, serverTLS, "tcp", testHubName, func(r *http.Request) string {
						return target(r, hub.port, otherPort)
					})

					err := call(t, newFederationTestBridge(t, hub.endpoint, mode))
					if got := other.requests(); len(got) != 0 {
						t.Errorf("other origin received %d requests, want 0", len(got))
					}
					if got := hub.requests(); len(got) != 0 {
						t.Errorf("redirect target on the hub port received %d requests, want 0", len(got))
					}
					if !errors.Is(err, errFederationOtherOrigin) {
						t.Fatalf("call error = %v, want errFederationOtherOrigin", err)
					}
					msg := err.Error()
					if containsURL(msg, hub.endpoint) {
						t.Errorf("error %q includes the hub endpoint", msg)
					}
					if strings.Contains(msg, password) {
						t.Errorf("error %q includes the Location password", msg)
					}
					// callerHubClient errors carry the hop URL as the shared
					// hub client redacts it, which keeps a user name.
					if site == "federatedUserGet" && strings.Contains(msg, user) {
						t.Errorf("error %q includes the Location user name", msg)
					}
				})
			}
		}
	}
}

func TestFederationCallSites_IPv6HubOrigin(t *testing.T) {
	for site, call := range federationCallSites {
		t.Run(site, func(t *testing.T) {
			serverTLS := installTestTransport(t)
			var hub *originHub
			hub = newOriginHub(t, serverTLS, "tcp6", "::1", func(r *http.Request) string {
				return "https://[::1]:" + hub.port + r.URL.Path + "?final=1"
			})
			if err := call(t, newFederationTestBridge(t, hub.endpoint)); err != nil {
				t.Fatalf("call with a same-origin redirect on %s: %v", hub.endpoint, err)
			}
			got := hub.requests()
			if len(got) != 1 {
				t.Fatalf("hub answered %d requests, want 1", len(got))
			}
			assertHubCredentials(t, got[0], nil)
		})
	}
}

// countingRoundTripper counts the requests it is given and answers 200.
type countingRoundTripper struct{ calls atomic.Int32 }

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestFederationHeaderTransport_RefusesOtherOriginBeforeBase(t *testing.T) {
	base := &countingRoundTripper{}
	transport, err := newFederationHeaderTransport(base, testFederationToken, "https://"+testHubName)
	if err != nil {
		t.Fatalf("newFederationHeaderTransport: %v", err)
	}
	for _, target := range []string{
		"https://other.example.com/api",
		"http://" + testHubName + "/api",
		"https://" + testHubName + ":8443/api",
		"https://sub." + testHubName + "/api",
		"https://" + testHubName + "./api",
	} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			t.Errorf("RoundTrip(%s) returned a response, want none", target)
		}
		if !errors.Is(err, errFederationOtherOrigin) {
			t.Errorf("RoundTrip(%s) error = %v, want errFederationOtherOrigin", target, err)
		}
		if err != nil && strings.Contains(err.Error(), target) {
			t.Errorf("RoundTrip(%s) error %q includes the URL", target, err)
		}
	}
	if n := base.calls.Load(); n != 0 {
		t.Errorf("base transport called %d times for other origins, want 0", n)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+testHubName+"/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip to the hub origin: %v", err)
	}
	if n := base.calls.Load(); n != 1 {
		t.Errorf("base transport called %d times for the hub origin, want 1", n)
	}
}

func TestFederationHeaderTransport_OtherHostnameSamePortRefused(t *testing.T) {
	// One server reached under two names: requests to 127.0.0.1 are
	// redirected to localhost on the same port, which records them.
	rec := &headerRecorder{}
	rec.Server = httptest.NewUnstartedServer(nil)
	rec.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "127.0.0.1:") {
			http.Redirect(w, r, withHostname(t, rec.URL, "localhost")+r.URL.RequestURI(), http.StatusFound)
			return
		}
		rec.record(r)
	})
	rec.Start()
	t.Cleanup(rec.Close)

	transport, err := newFederationHeaderTransport(http.DefaultTransport, testFederationToken, rec.URL)
	if err != nil {
		t.Fatalf("newFederationHeaderTransport: %v", err)
	}
	_, err = (&http.Client{Transport: transport}).Get(rec.URL + "/api/v1/auth/me")
	if !errors.Is(err, errFederationOtherOrigin) {
		t.Errorf("request error = %v, want errFederationOtherOrigin", err)
	}
	if got := rec.requests(); len(got) != 0 {
		t.Errorf("other hostname received %d requests, want 0", len(got))
	}
}

func TestCanonicalHostPort(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"https://hub.example.com", "hub.example.com:443"},
		{"https://hub.example.com:443", "hub.example.com:443"},
		{"http://hub.example.com", "hub.example.com:80"},
		{"http://hub.example.com:80", "hub.example.com:80"},
		{"https://HUB.Example.COM:8443", "hub.example.com:8443"},
		{"https://[::1]", "[::1]:443"},
		{"https://[::1]:443", "[::1]:443"},
		{"http://[::1]", "[::1]:80"},
		{"http://[::1]:80", "[::1]:80"},
		{"https://[::1]:8443", "[::1]:8443"},
		// A trailing dot is kept, not removed: such a host is a different
		// origin from the configured one and is refused.
		{"https://hub.example.com.", "hub.example.com.:443"},
		{"https://hub.example.com.:443", "hub.example.com.:443"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalHostPort(u); got != tc.want {
			t.Errorf("canonicalHostPort(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFederationHeaderTransport_EndpointMatching(t *testing.T) {
	cases := []struct {
		endpoint, request string
		want              bool
	}{
		{"https://hub.example.com", "https://hub.example.com/api", true},
		{"https://hub.example.com", "https://hub.example.com:443/api", true},
		{"https://HUB.example.com/", "https://hub.example.com/api", true},
		{"HTTPS://hub.example.com", "https://hub.example.com/api", true},
		{"http://hub.example.com:8080", "http://hub.example.com:8080/api", true},
		{"http://hub.example.com", "http://hub.example.com:80/api", true},
		{"https://hub.example.com", "https://other.example.com/api", false},
		{"https://hub.example.com", "https://sub.hub.example.com/api", false},
		{"https://hub.example.com", "https://hub.example.com:8443/api", false},
		{"http://hub.example.com:8080", "http://hub.example.com/api", false},
		{"https://hub.example.com", "http://hub.example.com/api", false},
		{"https://hub.example.com:8443", "http://hub.example.com:8443/api", false},
		{"https://[::1]", "https://[::1]:443/api", true},
		{"https://[::1]:443", "https://[::1]/api", true},
		{"http://[::1]", "http://[::1]:80/api", true},
		{"https://[::1]", "https://[::1]:8443/api", false},
		{"https://[::1]", "http://[::1]/api", false},
		{"https://[::1]", "https://[::2]/api", false},
		{"https://hub.example.com", "https://hub.example.com./api", false},
		{"https://hub.example.com.", "https://hub.example.com/api", false},
		// User information in the request URL does not change its origin.
		{"https://hub.example.com", "https://u:p@hub.example.com/api", true},
	}
	for _, tc := range cases {
		transport, err := newFederationHeaderTransport(nil, testFederationToken, tc.endpoint)
		if err != nil {
			t.Fatalf("newFederationHeaderTransport(%q): %v", tc.endpoint, err)
		}
		u, err := url.Parse(tc.request)
		if err != nil {
			t.Fatal(err)
		}
		if got := transport.isHubOrigin(u); got != tc.want {
			t.Errorf("endpoint %q, request %q: hub origin = %v, want %v", tc.endpoint, tc.request, got, tc.want)
		}
	}
}

func TestNewFederationHeaderTransport_EndpointWithoutHost(t *testing.T) {
	for _, endpoint := range []string{"", "hub.example.com", "://bad"} {
		if _, err := newFederationHeaderTransport(http.DefaultTransport, testFederationToken, endpoint); err == nil {
			t.Errorf("newFederationHeaderTransport(%q) succeeded, want an error", endpoint)
		}
	}
}

func TestNewFederationHeaderTransport_ErrorsOmitEndpoint(t *testing.T) {
	const password = "s3cret-pw"
	endpoints := []string{
		"https://hub-user:" + password + "@",                    // parses, no host
		"hub-user:" + password + "@hub.example.com",             // parses, no host
		"https://hub-user:" + password + "@hub example.com",     // does not parse
		"https://hub-user:" + password + "@hub.example.com/%zz", // does not parse
	}
	for _, endpoint := range endpoints {
		_, err := newFederationHeaderTransport(http.DefaultTransport, testFederationToken, endpoint)
		if err == nil {
			t.Errorf("newFederationHeaderTransport(%q) succeeded, want an error", endpoint)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, password) || strings.Contains(msg, endpoint) || strings.Contains(msg, "hub-user") {
			t.Errorf("error for endpoint %q includes endpoint text: %q", endpoint, msg)
		}
	}
}

func TestFederationCallSites_ErrorsOmitEndpoint(t *testing.T) {
	const user, password = "hub-user", "s3cret-pw"
	cases := []struct {
		site, endpoint string
		call           func(t *testing.T, b *Bridge) error
	}{
		{"federatedUserGet", "https://" + user + ":" + password + "@hub example.com", federationCallSites["federatedUserGet"]},
		{"callerHubClient", "https://" + user + ":" + password + "@hub example.com", federationCallSites["callerHubClient"]},
	}
	for _, tc := range cases {
		err := tc.call(t, newFederationTestBridge(t, tc.endpoint))
		if err == nil {
			t.Errorf("%s with endpoint %q succeeded, want an error", tc.site, tc.endpoint)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, password) || strings.Contains(msg, user) || strings.Contains(msg, tc.endpoint) {
			t.Errorf("%s error for endpoint %q includes endpoint text: %q", tc.site, tc.endpoint, msg)
		}
	}
}

func TestFederatedUserGet_RequestFailureOmitsURL(t *testing.T) {
	// A closed port on loopback: the endpoint is accepted, the request fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + ln.Addr().String()
	ln.Close()

	err = federationCallSites["federatedUserGet"](t, newFederationTestBridge(t, endpoint))
	if err == nil {
		t.Fatal("federatedUserGet against a closed port succeeded, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "GET /api/v1/auth/me") {
		t.Errorf("error %q does not name the request path", msg)
	}
	if strings.Contains(msg, endpoint) {
		t.Errorf("error %q includes the endpoint URL", msg)
	}
}

func TestFederatedUserGet_RefusesEndpointWithUserInformation(t *testing.T) {
	const user, password = "hub-user", "s3cret-pw"
	endpoints := []string{
		"https://" + user + ":" + password + "@hub.example.com",     // user and password
		"https://" + user + ":p@ss" + password + "@hub.example.com", // raw '@' in the password
		"https://" + user + ":" + password + "/x@hub.example.com",   // raw '/' in the password
		"https://" + user + ":1234/5678@hub.example.com",            // parses as host "hub-user:1234" with no user information
		"https://" + user + "@hub.example.com",                      // user only
		user + ":" + password + "@hub.example.com",                  // opaque
		"https://hub.example.com/?k=v",                              // query
		"https://hub.example.com/?",                                 // bare '?'
		"https://hub.example.com/#frag",                             // fragment
		"https://hub.example.com/#",                                 // bare '#'
	}
	for _, endpoint := range endpoints {
		err := federationCallSites["federatedUserGet"](t, newFederationTestBridge(t, endpoint))
		if !errors.Is(err, errFederatedUserEndpoint) {
			t.Errorf("federatedUserGet with endpoint %q: error = %v, want errFederatedUserEndpoint", endpoint, err)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, password) || strings.Contains(msg, user) || strings.Contains(msg, endpoint) {
			t.Errorf("error for endpoint %q includes endpoint text: %q", endpoint, msg)
		}
	}

	hub := newHeaderRecorder(t)
	if err := federationCallSites["federatedUserGet"](t, newFederationTestBridge(t, hub.URL)); err != nil {
		t.Errorf("federatedUserGet with endpoint %q: %v, want success", hub.URL, err)
	}
	if got := hub.requests(); len(got) != 1 {
		t.Errorf("hub received %d requests, want 1", len(got))
	}
}

// validBridgeConfig returns a configuration that ValidateConfig accepts,
// with endpoint as the hub endpoint.
func validBridgeConfig(endpoint string) *Config {
	return &Config{
		Bridge: BridgeConfig{ExternalURL: "https://bridge.example.com"},
		Hub:    HubConfig{Endpoint: endpoint, User: "admin@test"},
		Auth:   AuthConfig{Scheme: "federation"},
	}
}

func TestValidateConfig_HubEndpoint(t *testing.T) {
	const user, password = "hub-user", "s3cret-pw"
	refused := []string{
		"https://" + user + ":" + password + "@hub.example.com",     // user and password
		"https://" + user + "@hub.example.com",                      // user only
		"https://" + user + ":p@ss" + password + "@hub.example.com", // raw '@' in the password
		"https://" + user + ":" + password + "/x@hub.example.com",   // raw '/' in the password
		"https://" + user + ":1234/5678@hub.example.com",            // parses with no user information
		user + ":" + password + "@hub.example.com",                  // opaque
		"hub.example.com", // no host
		"https://" + user + ":" + password + "@hub example.com", // does not parse
		"https://hub.example.com/?k=v",                          // query
		"https://hub.example.com/?",                             // bare '?'
		"https://hub.example.com/#frag",                         // fragment
		"https://hub.example.com/#",                             // bare '#'
	}
	for _, endpoint := range refused {
		err := ValidateConfig(validBridgeConfig(endpoint))
		if err == nil {
			t.Errorf("ValidateConfig accepted hub endpoint %q, want an error", endpoint)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, user) || strings.Contains(msg, password) || strings.Contains(msg, endpoint) {
			t.Errorf("ValidateConfig error for hub endpoint %q includes endpoint text: %q", endpoint, msg)
		}
	}
	for _, endpoint := range []string{"https://hub.example.com", "http://hub:8080", "https://[::1]:8443/base"} {
		if err := ValidateConfig(validBridgeConfig(endpoint)); err != nil {
			t.Errorf("ValidateConfig(hub endpoint %q): %v, want success", endpoint, err)
		}
	}
}

func TestCallerHubClient_RequestFailureOmitsUserInformation(t *testing.T) {
	// A closed port on loopback: a valid endpoint, a request that fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + ln.Addr().String()
	ln.Close()
	if err := ValidateConfig(validBridgeConfig(endpoint)); err != nil {
		t.Fatalf("ValidateConfig(%q): %v", endpoint, err)
	}

	err = federationCallSites["callerHubClient"](t, newFederationTestBridge(t, endpoint))
	if err == nil {
		t.Fatal("callerHubClient request against a closed port succeeded, want an error")
	}
	if msg := err.Error(); strings.Contains(msg, "@") {
		t.Errorf("callerHubClient error %q includes user information", msg)
	}
}
