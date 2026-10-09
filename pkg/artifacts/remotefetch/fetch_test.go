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

package remotefetch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	jpegBytes = append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0}, 32)...)
	gifBytes  = append([]byte("GIF89a"), bytes.Repeat([]byte{0}, 32)...)
	webpBytes = append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 32)...)
	svgBytes  = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
)

// fakeResolver answers from a table and counts lookups.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	calls   map[string]int
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[host]++
	a, ok := r.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return a, nil
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, v := range s {
		out[i] = netip.MustParseAddr(v)
	}
	return out
}

// logBuffer collects log output so tests can check what was logged and in
// which order.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// testEnv is a TLS server on loopback plus a fetcher that may reach it: the
// fetcher's deny rule is the production one except for the server's own
// address, its port is the server's, and it trusts the server's CA. The
// httptest certificate covers example.com and 127.0.0.1.
type testEnv struct {
	srv      *httptest.Server
	fetcher  *Fetcher
	resolver *fakeResolver
	logs     *logBuffer
	mu       sync.Mutex
	requests []*http.Request
	conns    int
}

func newTestEnv(t *testing.T, handler http.HandlerFunc, cfg Config) *testEnv {
	t.Helper()
	env := &testEnv{logs: &logBuffer{}}
	env.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.requests = append(env.requests, r.Clone(context.Background()))
		env.mu.Unlock()
		handler(w, r)
	}))
	env.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			env.mu.Lock()
			env.conns++
			env.mu.Unlock()
		}
	}
	env.srv.StartTLS()
	t.Cleanup(env.srv.Close)
	_, port, _ := net.SplitHostPort(env.srv.Listener.Addr().String())
	serverAddr := netip.MustParseAddr("127.0.0.1")
	env.resolver = &fakeResolver{answers: map[string][]netip.Addr{
		"example.com":    {serverAddr},
		"other.test":     {serverAddr},
		"private.test":   addrs("10.0.0.5"),
		"mixed.test":     addrs("93.184.216.34", "10.0.0.5"),
		"mixed6.test":    addrs("2606:2800:220:1::1", "fd00::1"),
		"metadata.test":  addrs("169.254.169.254"),
		"loopback6.test": addrs("::1"),
		"mapped.test":    addrs("::ffff:169.254.169.254"),
		"public-ok.test": {serverAddr},
		"ula.test":       addrs("fd12::5"),
		"nat64.test":     addrs("64:ff9b::a9fe:a9fe"),
	}}
	cfg.Logger = slog.New(slog.NewTextHandler(env.logs, nil))
	f := New(cfg)
	f.resolver = env.resolver
	f.port = port
	f.rootCAs = env.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	f.denied = func(a netip.Addr) bool { return a != serverAddr && isDenied(a) }
	env.fetcher = f
	return env
}

func (env *testEnv) url(host, path string) string {
	_, port, _ := net.SplitHostPort(env.srv.Listener.Addr().String())
	return "https://" + net.JoinHostPort(host, port) + path
}

func (env *testEnv) connections() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.conns
}

func reasonOf(t *testing.T, err error) Reason {
	t.Helper()
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a *remotefetch.Error", err)
	}
	return fe.Reason
}

func serveBytes(b []byte, declared string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if declared != "" {
			w.Header().Set("Content-Type", declared)
		}
		_, _ = w.Write(b)
	}
}

func TestFetchAllowedImageTypes(t *testing.T) {
	for name, tc := range map[string]struct {
		body []byte
		want string
	}{
		"png":  {pngBytes, "image/png"},
		"jpeg": {jpegBytes, "image/jpeg"},
		"gif":  {gifBytes, "image/gif"},
		"webp": {webpBytes, "image/webp"},
	} {
		t.Run(name, func(t *testing.T) {
			// The server declares a misleading type; the sniffed one wins.
			env := newTestEnv(t, serveBytes(tc.body, "text/html"), Config{})
			res, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/i"))
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if res.ContentType != tc.want || !bytes.Equal(res.Body, tc.body) || len(res.SHA256) != 64 {
				t.Fatalf("got %q %d bytes digest %q", res.ContentType, len(res.Body), res.SHA256)
			}
		})
	}
}

func TestFetchRefusesWrongTypes(t *testing.T) {
	for name, body := range map[string][]byte{
		"svg":                  svgBytes,
		"html declared as png": []byte("<html><body>hi</body></html>"),
		"empty":                {},
		"truncated png magic":  []byte("\x89PNG"),
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, serveBytes(body, "image/png"), Config{})
			_, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/i"))
			if got := reasonOf(t, err); got != ReasonType {
				t.Fatalf("reason %q, want %q", got, ReasonType)
			}
		})
	}
}

func TestFetchSizeCaps(t *testing.T) {
	big := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{1}, 2048)...)
	t.Run("content-length over cap", func(t *testing.T) {
		env := newTestEnv(t, serveBytes(big, "image/png"), Config{MaxBytes: 1024})
		_, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/i"))
		if got := reasonOf(t, err); got != ReasonTooLarge {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("streamed body over cap without content-length", func(t *testing.T) {
		env := newTestEnv(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			for i := 0; i < 4; i++ {
				_, _ = w.Write(big[i*len(big)/4 : (i+1)*len(big)/4])
				w.(http.Flusher).Flush()
			}
		}, Config{MaxBytes: 1024})
		_, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/i"))
		if got := reasonOf(t, err); got != ReasonTooLarge {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("exactly at cap is allowed", func(t *testing.T) {
		exact := big[:1024]
		env := newTestEnv(t, serveBytes(exact, "image/png"), Config{MaxBytes: 1024})
		if _, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/i")); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	})
}

func TestFetchTimeouts(t *testing.T) {
	t.Run("slow server hits the per-fetch timeout", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}, Config{Timeout: 200 * time.Millisecond})
		start := time.Now()
		_, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/slow"))
		if got := reasonOf(t, err); got != ReasonTimeout {
			t.Fatalf("reason %q", got)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("timeout not enforced: %v", time.Since(start))
		}
	})
	t.Run("caller's total budget applies", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}, Config{Timeout: time.Minute})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := env.fetcher.Fetch(ctx, env.url("example.com", "/slow"))
		if got := reasonOf(t, err); got != ReasonTimeout {
			t.Fatalf("reason %q", got)
		}
	})
}

func TestFetchStatus(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }, Config{})
	_, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/missing"))
	if got := reasonOf(t, err); got != ReasonStatus {
		t.Fatalf("reason %q", got)
	}
}

// TestFetchURLRules: scheme, userinfo, port and host rules refuse the URL
// before anything is resolved or dialed.
func TestFetchURLRules(t *testing.T) {
	env := newTestEnv(t, serveBytes(pngBytes, ""), Config{})
	_, port, _ := net.SplitHostPort(env.srv.Listener.Addr().String())
	for name, tc := range map[string]struct {
		url  string
		want Reason
	}{
		"http scheme":               {"http://example.com:" + port + "/i", ReasonScheme},
		"file scheme":               {"file:///etc/passwd", ReasonScheme},
		"gopher scheme":             {"gopher://example.com:" + port + "/", ReasonScheme},
		"ftp scheme":                {"ftp://example.com/i", ReasonScheme},
		"data URL":                  {"data:image/png;base64,AAAA", ReasonBadURL},
		"relative":                  {"/img.png", ReasonBadURL},
		"userinfo":                  {"https://user:pass@example.com:" + port + "/i", ReasonUserinfo},
		"userinfo name only":        {"https://user@example.com:" + port + "/i", ReasonUserinfo},
		"port other than allowed":   {"https://example.com:8443/i", ReasonPort},
		"decimal numeric host":      {"https://2130706433:" + port + "/i", ReasonHost},
		"hex numeric host":          {"https://0x7f000001:" + port + "/i", ReasonHost},
		"short dotted numeric host": {"https://127.1:" + port + "/i", ReasonHost},
		"octal dotted host":         {"https://0177.0.0.1:" + port + "/i", ReasonHost},
		"hex dotted host":           {"https://0x7f.0x0.0x0.0x1:" + port + "/i", ReasonHost},
		"zone in IPv6 literal":      {"https://[fe80::1%25eth0]:" + port + "/i", ReasonHost},
		"empty host":                {"https:///i", ReasonHost},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := env.fetcher.Fetch(context.Background(), tc.url)
			if got := reasonOf(t, err); got != tc.want {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
		})
	}
	if env.connections() != 0 {
		t.Fatalf("%d connections made for refused URLs", env.connections())
	}
}

// TestFetchDeniedAddresses: denied addresses, as IP literals or from DNS,
// are refused before any connection, and the denial is logged first.
func TestFetchDeniedAddresses(t *testing.T) {
	env := newTestEnv(t, serveBytes(pngBytes, ""), Config{})
	for name, host := range map[string]string{
		"cloud metadata literal":        "169.254.169.254",
		"cloud metadata via DNS":        "metadata.test",
		"cloud metadata mapped in IPv6": "[::ffff:169.254.169.254]",
		"mapped metadata via DNS":       "mapped.test",
		"loopback literal 127.0.0.2":    "127.0.0.2",
		"IPv6 loopback literal":         "[::1]",
		"IPv6 loopback via DNS":         "loopback6.test",
		"IPv4-mapped loopback":          "[::ffff:127.0.0.2]",
		"private via DNS":               "private.test",
		"private literal":               "192.168.1.10",
		"ULA literal":                   "[fd12:3456::1]",
		"link-local IPv6":               "[fe80::1]",
		"NAT64 of metadata":             "[64:ff9b::a9fe:a9fe]",
		"6to4 of loopback":              "[2002:7f00:1::1]",
		"unspecified":                   "0.0.0.0",
		"unspecified IPv6":              "[::]",
		"multicast":                     "224.0.0.1",
		"CGNAT":                         "100.64.0.1",
		"mixed public and private DNS":  "mixed.test",
		"mixed public and ULA DNS":      "mixed6.test",
		"metadata, IPv4-translated":     "[::ffff:0:a9fe:a9fe]",
		"ULA via DNS":                   "ula.test",
		"NAT64 of metadata via DNS":     "nat64.test",
	} {
		t.Run(name, func(t *testing.T) {
			before := strings.Count(env.logs.String(), "denied before connecting")
			_, err := env.fetcher.Fetch(context.Background(), env.url(strings.Trim(host, "[]"), "/i"))
			if got := reasonOf(t, err); got != ReasonDeniedAddress {
				t.Fatalf("reason %q, want %q", got, ReasonDeniedAddress)
			}
			if strings.Count(env.logs.String(), "denied before connecting") != before+1 {
				t.Fatalf("denial not logged; logs:\n%s", env.logs.String())
			}
		})
	}
	if env.connections() != 0 {
		t.Fatalf("%d connections made to denied hosts", env.connections())
	}
}

func TestIsDenied(t *testing.T) {
	for _, a := range []string{
		"169.254.169.254", "::ffff:169.254.169.254", "127.0.0.1", "127.255.255.254", "::1",
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.0.1", "fc00::1", "fdff::1",
		"169.254.1.1", "fe80::1", "224.0.0.1", "ff02::1", "0.0.0.0", "::", "0.1.2.3",
		"100.64.0.1", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"240.0.0.1", "255.255.255.255", "::127.0.0.1", "64:ff9b::7f00:1", "2001::1",
		"2001:db8::1", "2002:7f00:1::1", "fec0::1", "100::1",
		// IPv4-translated (SIIT) forms of loopback, metadata and private.
		"::ffff:0:7f00:1", "::ffff:0:a9fe:a9fe", "::ffff:0:a00:1",
		// Outside 2000::/3: denied by default.
		"fc00::1", "fd00::1", "fe80::1", "ff02::1", "4000::1", "5f00::1", "8000::1", "e000::1",
		// Non-global ranges inside 2000::/3.
		"2001:10::1", "2001:20::1", "3fff::1", "64:ff9b::808:808", "2002:808:808::1", "2001::808:808",
	} {
		if !isDenied(netip.MustParseAddr(a)) {
			t.Errorf("%s not denied", a)
		}
	}
	for _, a := range []string{
		"93.184.216.34", "8.8.8.8", "2606:2800:220:1::1", "1.1.1.1",
		// Public IPv4 in mapped and translated form is judged as IPv4.
		"::ffff:8.8.8.8", "::ffff:0:808:808",
		// Ordinary global unicast.
		"2a00:1450:4001::1", "2606:4700:4700::1111", "2001:4860:4860::8888", "3ffe::1",
	} {
		if isDenied(netip.MustParseAddr(a)) {
			t.Errorf("%s denied", a)
		}
	}
	if isDenied(netip.Addr{}) == false {
		t.Error("zero address not denied")
	}
}

// TestFetchRedirects: every hop is checked like the first, at most
// MaxRedirects hops are followed, and each hop's TLS name is its own.
func TestFetchRedirects(t *testing.T) {
	var env *testEnv
	env = newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, port, _ := net.SplitHostPort(env.srv.Listener.Addr().String())
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusFound)
		case "/final":
			_, _ = w.Write(pngBytes)
		case "/to-http":
			w.Header().Set("Location", "http://example.com:"+port+"/final")
			w.WriteHeader(http.StatusFound)
		case "/to-file":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
		case "/to-gopher":
			w.Header().Set("Location", "gopher://example.com:"+port+"/")
			w.WriteHeader(http.StatusFound)
		case "/to-userinfo":
			w.Header().Set("Location", "https://u:p@example.com:"+port+"/final")
			w.WriteHeader(http.StatusFound)
		case "/to-metadata":
			w.Header().Set("Location", "https://169.254.169.254:"+port+"/latest/meta-data/")
			w.WriteHeader(http.StatusFound)
		case "/to-private-name":
			w.Header().Set("Location", "https://private.test:"+port+"/final")
			w.WriteHeader(http.StatusFound)
		case "/to-other-port":
			w.Header().Set("Location", "https://example.com:8443/final")
			w.WriteHeader(http.StatusFound)
		case "/to-other-name":
			// other.test resolves to the same server, whose certificate
			// does not cover that name.
			w.Header().Set("Location", "https://other.test:"+port+"/final")
			w.WriteHeader(http.StatusFound)
		case "/loop":
			w.Header().Set("Location", "/loop")
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}, Config{})
	if _, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/ok")); err != nil {
		t.Fatalf("one same-host redirect: %v", err)
	}
	for path, want := range map[string]Reason{
		"/to-http":         ReasonScheme,
		"/to-file":         ReasonScheme,
		"/to-gopher":       ReasonScheme,
		"/to-userinfo":     ReasonUserinfo,
		"/to-metadata":     ReasonDeniedAddress,
		"/to-private-name": ReasonDeniedAddress,
		"/to-other-port":   ReasonPort,
		"/to-other-name":   ReasonConnect,
		"/loop":            ReasonRedirects,
	} {
		t.Run(path, func(t *testing.T) {
			_, err := env.fetcher.Fetch(context.Background(), env.url("example.com", path))
			if got := reasonOf(t, err); got != want {
				t.Fatalf("reason %q, want %q", got, want)
			}
		})
	}
}

// TestFetchSendsNoCredentials: no cookies, authorization or proxy headers
// reach the server, and proxy environment variables are ignored.
func TestFetchSendsNoCredentials(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	env := newTestEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "x"})
		w.Header().Set("Location", "/final")
		w.WriteHeader(http.StatusFound)
	}, Config{})
	env.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.requests = append(env.requests, r.Clone(context.Background()))
		env.mu.Unlock()
		if r.URL.Path != "/final" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "x"})
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write(pngBytes)
	})
	if _, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/start")); err != nil {
		t.Fatalf("Fetch (proxy env must be ignored): %v", err)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.requests) != 2 {
		t.Fatalf("%d requests, want 2", len(env.requests))
	}
	for _, r := range env.requests {
		for _, h := range []string{"Cookie", "Authorization", "Proxy-Authorization", "X-Scion-Agent-Token"} {
			if v := r.Header.Get(h); v != "" {
				t.Errorf("%s %s carried %s: %q", r.Method, r.URL.Path, h, v)
			}
		}
	}
}

// TestFetchResolvesOncePerHopAndDialsVettedAddress: the name is resolved
// once per hop, and only the first vetted address is dialed (no fallback to
// another address).
func TestFetchResolvesOncePerHopAndDialsVettedAddress(t *testing.T) {
	env := newTestEnv(t, serveBytes(pngBytes, ""), Config{ConnectTimeout: 500 * time.Millisecond})
	if _, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/i")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := env.resolver.calls["example.com"]; got != 1 {
		t.Fatalf("resolved %d times, want 1", got)
	}

	// First answer is an allowed but unreachable public test address; the
	// second is the reachable server. Dialing only the vetted first address
	// means the fetch fails instead of falling back.
	unreachable := netip.MustParseAddr("192.0.2.1") // denied by production rules
	env.fetcher.denied = func(a netip.Addr) bool {
		return a != unreachable && a != netip.MustParseAddr("127.0.0.1") && isDenied(a)
	}
	env.resolver.answers["fallback.test"] = []netip.Addr{unreachable, netip.MustParseAddr("127.0.0.1")}
	before := env.connections()
	_, err := env.fetcher.Fetch(context.Background(), env.url("fallback.test", "/i"))
	if err == nil {
		t.Fatal("fetch fell back to a second address")
	}
	if env.connections() != before {
		t.Fatal("the reachable second address was dialed")
	}
}

func TestPlausibleDNSName(t *testing.T) {
	for _, h := range []string{"example.com", "img.example.co.uk", "a-b.example", "x.y.z.example."} {
		if !plausibleDNSName(h) {
			t.Errorf("%q rejected", h)
		}
	}
	for _, h := range []string{"2130706433", "127.1", "0x7f.1", "1.2.3.0x4", "a..b", "exa mple.com", "ex%61mple.com", ""} {
		if plausibleDNSName(h) {
			t.Errorf("%q accepted", h)
		}
	}
}

func TestSniffImage(t *testing.T) {
	for b, want := range map[string]string{
		string(pngBytes): "image/png", string(jpegBytes): "image/jpeg", string(gifBytes): "image/gif",
		string(webpBytes): "image/webp", string(svgBytes): "", "<?xml version=\"1.0\"?><svg/>": "", "BM": "",
	} {
		if got := SniffImage([]byte(b)); got != want {
			t.Errorf("SniffImage(%q...) = %q, want %q", b[:min(8, len(b))], got, want)
		}
	}
}

// TestFetchConnectTimeout: a server that accepts the connection but never
// completes the TLS handshake is abandoned after the connect timeout, well
// before the per-fetch timeout.
func TestFetchConnectTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // never read or written
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f := New(Config{ConnectTimeout: 200 * time.Millisecond, Timeout: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(&logBuffer{}, nil))})
	f.resolver = &fakeResolver{answers: map[string][]netip.Addr{"example.com": addrs("127.0.0.1")}}
	f.port = port
	f.denied = func(a netip.Addr) bool { return a != netip.MustParseAddr("127.0.0.1") && isDenied(a) }

	start := time.Now()
	_, err = f.Fetch(context.Background(), "https://example.com:"+port+"/i")
	if got := reasonOf(t, err); got != ReasonConnect {
		t.Fatalf("reason %q, want %q", got, ReasonConnect)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("connect timeout not applied: %v", elapsed)
	}
}

// TestFetchUnresolvableHost: a host name that does not resolve fails with
// ReasonResolve, before any connection.
func TestFetchUnresolvableHost(t *testing.T) {
	env := newTestEnv(t, serveBytes(pngBytes, ""), Config{})
	_, err := env.fetcher.Fetch(context.Background(), env.url("unresolvable.test", "/i"))
	if got := reasonOf(t, err); got != ReasonResolve {
		t.Fatalf("reason %q, want %q", got, ReasonResolve)
	}
	if env.connections() != 0 {
		t.Fatalf("%d connections made", env.connections())
	}
}

// TestFetchBytesReturnsAnyContent: FetchBytes returns a non-image body
// unchanged and leaves ContentType empty, while Fetch still refuses it.
func TestFetchBytesReturnsAnyContent(t *testing.T) {
	body := []byte("\x1f\x8b\x08 not an image")
	env := newTestEnv(t, serveBytes(body, "application/gzip"), Config{})
	res, err := env.fetcher.FetchBytes(context.Background(), env.url("example.com", "/a.tar.gz"))
	if err != nil {
		t.Fatalf("FetchBytes: %v", err)
	}
	if !bytes.Equal(res.Body, body) || res.ContentType != "" || res.SHA256 == "" {
		t.Fatalf("unexpected result %+v", res)
	}
	if _, err := env.fetcher.Fetch(context.Background(), env.url("example.com", "/a.tar.gz")); reasonOf(t, err) != ReasonType {
		t.Fatalf("Fetch should still refuse non-images, got %v", err)
	}
}

// TestFetchBytesKeepsRequestRules: FetchBytes refuses the same URLs,
// addresses and sizes Fetch does.
func TestFetchBytesKeepsRequestRules(t *testing.T) {
	var env *testEnv
	env = newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, port, _ := net.SplitHostPort(env.srv.Listener.Addr().String())
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 2048))
		case "/big-chunked":
			// No Content-Length: the cap must apply while streaming.
			for i := 0; i < 4; i++ {
				_, _ = w.Write(bytes.Repeat([]byte("x"), 512))
				w.(http.Flusher).Flush()
			}
		case "/to-metadata":
			w.Header().Set("Location", "https://169.254.169.254:"+port+"/")
			w.WriteHeader(http.StatusFound)
		case "/loop":
			w.Header().Set("Location", "/loop")
			w.WriteHeader(http.StatusFound)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}, Config{MaxBytes: 1024})
	_, port, _ := net.SplitHostPort(env.srv.Listener.Addr().String())
	for name, tc := range map[string]struct {
		url  string
		want Reason
	}{
		"http":             {"http://example.com:" + port + "/", ReasonScheme},
		"userinfo":         {"https://u:p@example.com:" + port + "/", ReasonUserinfo},
		"loopback":         {"https://127.0.0.2:" + port + "/", ReasonDeniedAddress},
		"private name":     {env.url("private.test", "/"), ReasonDeniedAddress},
		"metadata":         {env.url("metadata.test", "/"), ReasonDeniedAddress},
		"redirect meta":    {env.url("example.com", "/to-metadata"), ReasonDeniedAddress},
		"oversize":         {env.url("example.com", "/big"), ReasonTooLarge},
		"oversize chunked": {env.url("example.com", "/big-chunked"), ReasonTooLarge},
		"loop":             {env.url("example.com", "/loop"), ReasonRedirects},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := env.fetcher.FetchBytes(context.Background(), tc.url)
			if got := reasonOf(t, err); got != tc.want {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFetchAllowedHosts: with AllowedHosts set, the first request and every
// redirect hop must name an allowed host; a refused hop makes no connection.
func TestFetchAllowedHosts(t *testing.T) {
	var env *testEnv
	env = newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, port, _ := net.SplitHostPort(env.srv.Listener.Addr().String())
		switch r.URL.Path {
		case "/same":
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusFound)
		case "/off-list":
			w.Header().Set("Location", "https://other.test:"+port+"/final")
			w.WriteHeader(http.StatusFound)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}, Config{AllowedHosts: []string{"Example.com"}})

	if _, err := env.fetcher.FetchBytes(context.Background(), env.url("example.com", "/same")); err != nil {
		t.Fatalf("allowed host with same-host redirect: %v", err)
	}
	before := env.connections()
	if _, err := env.fetcher.FetchBytes(context.Background(), env.url("other.test", "/final")); reasonOf(t, err) != ReasonHostNotAllowed {
		t.Fatalf("first hop off the list: got %v", err)
	}
	if _, err := env.fetcher.FetchBytes(context.Background(), env.url("127.0.0.1", "/final")); reasonOf(t, err) != ReasonHostNotAllowed {
		t.Fatalf("IP literal off the list: got %v", err)
	}
	if env.connections() != before {
		t.Fatalf("refused first hops must not connect")
	}
	if _, err := env.fetcher.FetchBytes(context.Background(), env.url("example.com", "/off-list")); reasonOf(t, err) != ReasonHostNotAllowed {
		t.Fatalf("redirect off the list: got %v", err)
	}
	if _, err := env.fetcher.Fetch(context.Background(), env.url("other.test", "/final")); reasonOf(t, err) != ReasonHostNotAllowed {
		t.Fatalf("Fetch honours AllowedHosts too: got %v", err)
	}
}
