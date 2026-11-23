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

package substrate

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// connectRecord is what the fake CONNECT router saw on one tunnel.
type connectRecord struct {
	target    string // CONNECT request-target
	actorHdr  string // TargetActorHeader on the outer request
	clientCN  string // client certificate CN, TLS only
	alpn      string // negotiated ALPN, TLS only
	refused   int    // status the CONNECT was refused with, 0 if accepted
	innerReqs []innerRecord
	closed    chan struct{}
}

// innerRecord is one HTTP/1.1 request seen inside a tunnel.
type innerRecord struct {
	method, path, host, body string
	actorHdr, customHdr      string
}

// fakeConnectRouter is a raw TCP (optionally TLS) listener speaking the
// router's CONNECT protocol: it answers the outer CONNECT (refusing the
// first refuse[i] attempts with the given status), then serves inner
// HTTP/1.1 requests on the tunnel, echoing the request back.
type fakeConnectRouter struct {
	ln net.Listener

	mu          sync.Mutex
	refuse      []int // statuses for successive CONNECT attempts; then accept
	innerStatus int   // HTTP status answered inside the tunnel (0 -> 200 OK)
	tunnels     []*connectRecord
}

func newFakeConnectRouter(t *testing.T, tlsCfg *tls.Config) *fakeConnectRouter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	f := &fakeConnectRouter{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeConnectRouter) addr() string { return f.ln.Addr().String() }

func (f *fakeConnectRouter) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeConnectRouter) handle(c net.Conn) {
	rec := &connectRecord{closed: make(chan struct{})}
	defer func() {
		_ = c.Close()
		close(rec.closed)
	}()
	if tc, ok := c.(*tls.Conn); ok {
		if err := tc.Handshake(); err != nil {
			return
		}
		st := tc.ConnectionState()
		rec.alpn = st.NegotiatedProtocol
		if len(st.PeerCertificates) > 0 {
			rec.clientCN = st.PeerCertificates[0].Subject.CommonName
		}
	}
	br := bufio.NewReader(c)
	outer, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	rec.target = outer.RequestURI
	rec.actorHdr = outer.Header.Get(TargetActorHeader)

	f.mu.Lock()
	f.tunnels = append(f.tunnels, rec)
	refuse := 0
	if outer.Method == http.MethodConnect && len(f.refuse) > 0 {
		refuse, f.refuse = f.refuse[0], f.refuse[1:]
	}
	if outer.Method != http.MethodConnect {
		refuse = http.StatusMethodNotAllowed
	}
	rec.refused = refuse
	f.mu.Unlock()

	if refuse != 0 {
		body := fmt.Sprintf("refused %d", refuse)
		_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", refuse, http.StatusText(refuse), len(body), body)
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	for {
		inner, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		b, _ := io.ReadAll(inner.Body)
		ir := innerRecord{
			method: inner.Method, path: inner.URL.Path, host: inner.Host, body: string(b),
			actorHdr: inner.Header.Get(TargetActorHeader), customHdr: inner.Header.Get("X-Custom"),
		}
		f.mu.Lock()
		rec.innerReqs = append(rec.innerReqs, ir)
		statusCode := f.innerStatus
		f.mu.Unlock()
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		resp := fmt.Sprintf("%s %s %s", ir.method, ir.path, ir.body)
		if _, err := fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\n\r\n%s", statusCode, http.StatusText(statusCode), len(resp), resp); err != nil {
			return
		}
	}
}

func (f *fakeConnectRouter) snapshot() []connectRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]connectRecord, len(f.tunnels))
	for i, t := range f.tunnels {
		out[i] = *t
		out[i].innerReqs = append([]innerRecord(nil), t.innerReqs...)
	}
	return out
}

func (f *fakeConnectRouter) setRefuse(statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = statuses
}

func (f *fakeConnectRouter) setInnerStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.innerStatus = code
}

// recordSleeps replaces c.sleep with one that records the requested waits
// without sleeping.
func recordSleeps(c *RouterClient) *[]time.Duration {
	var waits []time.Duration
	var mu sync.Mutex
	c.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		return nil
	}
	return &waits
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRouterClient_ControlPortSendsTargetHeaderToEndpoint(t *testing.T) {
	type seen struct{ method, path, actor, custom, body string }
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.Header.Get(TargetActorHeader), r.Header.Get("X-Custom"), string(b)}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := NewRouterClient(srv.URL + "/")
	for _, port := range []int{ControlServerPort, 0} {
		resp, err := c.Do(testCtx(t), "space", "actor-1", port, http.MethodPost, "/exec", strings.NewReader("payload"), http.Header{"X-Custom": {"v"}})
		if err != nil {
			t.Fatalf("Do(port %d) error = %v", port, err)
		}
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK || body != "ok" {
			t.Errorf("Do(port %d) = %d %q, want 200 ok", port, resp.StatusCode, body)
		}
		want := seen{http.MethodPost, "/exec", "space/actor-1", "v", "payload"}
		if s := <-got; s != want {
			t.Errorf("Do(port %d): server saw %+v, want %+v", port, s, want)
		}
	}
}

func TestRouterClient_ControlPortRetries503WithBodyReplay(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer srv.Close()

	c := NewRouterClient(srv.URL)
	waits := recordSleeps(c)
	resp, err := c.Do(testCtx(t), "s", "a", ControlServerPort, http.MethodPost, "/bootstrap", strings.NewReader("secret-body"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || body != "done" {
		t.Errorf("Do() = %d %q, want 200 done", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("server saw %d attempts, want 3", len(bodies))
	}
	for i, b := range bodies {
		if b != "secret-body" {
			t.Errorf("attempt %d body = %q, want the replayed body", i, b)
		}
	}
	if want := routerRetryBackoff[:2]; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestRouterClient_503RetriesExhausted(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewRouterClient(srv.URL)
	waits := recordSleeps(c)
	resp, err := c.Do(testCtx(t), "s", "a", ControlServerPort, http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the final 503", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != len(routerRetryBackoff)+1 {
		t.Errorf("attempts = %d, want %d", attempts, len(routerRetryBackoff)+1)
	}
	if !equalDurations(*waits, routerRetryBackoff) {
		t.Errorf("waits = %v, want %v", *waits, routerRetryBackoff)
	}
}

func TestRouterClient_RetryStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewRouterClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	if _, err := c.Do(ctx, "s", "a", ControlServerPort, http.MethodGet, "/", nil, nil); err == nil {
		t.Error("Do() after the context was cancelled during backoff = nil error, want one")
	}
}

func TestRouterClient_ConnectPathOuterHeaderOnlyAndInnerHTTP(t *testing.T) {
	f := newFakeConnectRouter(t, nil)
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "http://unused.invalid", ConnectEndpoint: f.addr()})

	for i := 0; i < 2; i++ {
		resp, err := c.Do(testCtx(t), "space", "actor-1", 8080, http.MethodPost, "/v1/chat", strings.NewReader(fmt.Sprintf("msg-%d", i)), http.Header{"X-Custom": {"c"}})
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		if body, want := readBody(t, resp), fmt.Sprintf("POST /v1/chat msg-%d", i); resp.StatusCode != http.StatusOK || body != want {
			t.Errorf("Do() = %d %q, want 200 %q", resp.StatusCode, body, want)
		}
	}

	tunnels := f.snapshot()
	if len(tunnels) != 1 {
		t.Fatalf("CONNECT tunnels = %d, want 1 reused across both requests", len(tunnels))
	}
	tun := tunnels[0]
	if tun.target != "actor-1:8080" {
		t.Errorf("CONNECT target = %q, want actor-1:8080", tun.target)
	}
	if tun.actorHdr != "space/actor-1" {
		t.Errorf("outer %s = %q, want space/actor-1", TargetActorHeader, tun.actorHdr)
	}
	if len(tun.innerReqs) != 2 {
		t.Fatalf("inner requests = %d, want 2", len(tun.innerReqs))
	}
	for _, ir := range tun.innerReqs {
		if ir.actorHdr != "" {
			t.Errorf("inner request carries %s = %q; it belongs on the outer CONNECT only", TargetActorHeader, ir.actorHdr)
		}
		if ir.customHdr != "c" || ir.host != "actor-1:8080" || ir.path != "/v1/chat" {
			t.Errorf("inner request = %+v, want caller header, Host actor-1:8080, path /v1/chat", ir)
		}
	}
}

func TestRouterClient_RefusedConnectSurfacesStatus(t *testing.T) {
	f := newFakeConnectRouter(t, nil)
	f.setRefuse(http.StatusNotFound)
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "http://unused.invalid", ConnectEndpoint: f.addr()})
	waits := recordSleeps(c)

	resp, err := c.Do(testCtx(t), "s", "ghost", 8080, http.MethodGet, "/", nil, nil)
	if err != nil {
		t.Fatalf("Do() error = %v, want the refusal as a response", err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusNotFound || body != "refused 404" {
		t.Errorf("Do() = %d %q, want 404 with the router's body", resp.StatusCode, body)
	}
	if len(*waits) != 0 {
		t.Errorf("404 was retried (%v); only 503 is retryable", *waits)
	}
}

func TestRouterClient_RefusedConnect503RetriedWithBodyReplay(t *testing.T) {
	f := newFakeConnectRouter(t, nil)
	f.setRefuse(http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "http://unused.invalid", ConnectEndpoint: f.addr()})
	waits := recordSleeps(c)

	resp, err := c.Do(testCtx(t), "s", "waking", 9000, http.MethodPut, "/state", strings.NewReader("data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || body != "PUT /state data" {
		t.Errorf("Do() = %d %q, want 200 with the replayed body echoed", resp.StatusCode, body)
	}
	if !equalDurations(*waits, routerRetryBackoff[:2]) {
		t.Errorf("waits = %v, want %v", *waits, routerRetryBackoff[:2])
	}
	tunnels := f.snapshot()
	if len(tunnels) != 3 || tunnels[0].refused != 503 || tunnels[1].refused != 503 || tunnels[2].refused != 0 {
		t.Fatalf("tunnels = %+v, want two refused 503 then one accepted", tunnels)
	}
}

func TestRouterClient_ConnectEndpointRequired(t *testing.T) {
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "not a url"})
	if c.ConnectEndpoint != "" {
		t.Fatalf("ConnectEndpoint = %q, want empty for a host-less endpoint", c.ConnectEndpoint)
	}
	if _, err := c.Do(testCtx(t), "s", "a", 8080, http.MethodGet, "/", nil, nil); err == nil || !strings.Contains(err.Error(), "connect endpoint") {
		t.Errorf("Do() error = %v, want a missing connect endpoint error", err)
	}
}

func TestRouterClient_DropActorClosesOnlyThatActor(t *testing.T) {
	f := newFakeConnectRouter(t, nil)
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "http://unused.invalid", ConnectEndpoint: f.addr()})
	do := func(actor string, port int) {
		t.Helper()
		resp, err := c.Do(testCtx(t), "space", actor, port, http.MethodGet, "/", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = readBody(t, resp)
	}
	do("a", 8080)
	do("a", 9090)
	do("b", 8080)

	tunnelFor := func(target string) []connectRecord {
		var out []connectRecord
		for _, tun := range f.snapshot() {
			if tun.target == target {
				out = append(out, tun)
			}
		}
		return out
	}
	aTunnels := append(tunnelFor("a:8080"), tunnelFor("a:9090")...)
	bTunnels := tunnelFor("b:8080")
	if len(aTunnels) != 2 || len(bTunnels) != 1 {
		t.Fatalf("tunnels a=%d b=%d, want 2 and 1", len(aTunnels), len(bTunnels))
	}

	// http.Transport returns a connection to its idle pool asynchronously
	// after the body hits EOF; give it a moment so the tunnels are idle
	// (DropActor closes idle tunnels, not in-flight ones).
	time.Sleep(100 * time.Millisecond)
	c.DropActor("space", "a")

	for _, tun := range aTunnels {
		select {
		case <-tun.closed:
		case <-time.After(5 * time.Second):
			t.Errorf("tunnel %s still open after DropActor", tun.target)
		}
	}
	select {
	case <-bTunnels[0].closed:
		t.Error("DropActor(a) closed actor b's tunnel")
	case <-time.After(200 * time.Millisecond):
	}
	c.mu.Lock()
	for k := range c.pools {
		if k.actor == "a" {
			t.Errorf("pool %+v survived DropActor", k)
		}
	}
	if _, ok := c.pools[connectPoolKey{atespace: "space", actor: "b", port: 8080}]; !ok {
		t.Error("actor b's pool was dropped")
	}
	c.mu.Unlock()

	// The next request to a opens a fresh tunnel; b keeps reusing its own.
	do("a", 8080)
	do("b", 8080)
	if n := len(tunnelFor("a:8080")); n != 2 {
		t.Errorf("a:8080 tunnels = %d, want a new one after DropActor", n)
	}
	if n := len(tunnelFor("b:8080")); n != 1 {
		t.Errorf("b:8080 tunnels = %d, want the pooled one reused", n)
	}
}

func TestDefaultConnectEndpoint(t *testing.T) {
	port := strconv.Itoa(DefaultRouterConnectPort)
	cases := map[string]string{
		"https://atenet-router.ate-system.svc":       "atenet-router.ate-system.svc:" + port,
		" https://atenet-router.ate-system.svc/ ":    "atenet-router.ate-system.svc:" + port,
		"https://atenet-router.ate-system.svc:443/x": "atenet-router.ate-system.svc:" + port,
		"https://[fd00::1]":                          "[fd00::1]:" + port,
		"":                                           "",
		"atenet-router.ate-system.svc":               "",
	}
	for in, want := range cases {
		if got := DefaultConnectEndpoint(in); got != want {
			t.Errorf("DefaultConnectEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
	if got := NewRouterClient("https://atenet-router.ate-system.svc").ConnectEndpoint; got != "atenet-router.ate-system.svc:"+port {
		t.Errorf("NewRouterClient ConnectEndpoint = %q, want the derived default", got)
	}
}

// routerTLSFixture is a server CA, a client CA, and a router TLS source
// whose client bundle is signed by the client CA.
type routerTLSFixture struct {
	serverCert tls.Certificate
	clientCA   *testCA
	src        TLSConfigFunc
}

func newRouterTLSFixture(t *testing.T) routerTLSFixture {
	t.Helper()
	serverCA, clientCA := newTestCA(t, "servicedns"), newTestCA(t, "podidentity")
	serverCert, _ := serverCA.issue(t, "router", []string{"atenet-router.ate-system.svc"}, false)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	writeFile(t, caFile, serverCA.pem)
	bundle := filepath.Join(dir, "credential-bundle.pem")
	_, b := clientCA.issue(t, "scion-broker", nil, true)
	writeFile(t, bundle, b)
	src, err := NewRouterTLSSource(context.Background(), nil, &config.V1SubstrateConfig{
		RouterEndpoint:       "https://atenet-router.ate-system.svc",
		RouterCAFile:         caFile,
		RouterClientCertFile: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return routerTLSFixture{serverCert: serverCert, clientCA: clientCA, src: src}
}

func (fx routerTLSFixture) serverTLS() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{fx.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    poolOf(fx.clientCA),
		NextProtos:   []string{"h2", "http/1.1"},
	}
}

func TestRouterClient_ConnectPathOverMTLS(t *testing.T) {
	fx := newRouterTLSFixture(t)
	f := newFakeConnectRouter(t, fx.serverTLS())
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "https://atenet-router.ate-system.svc", ConnectEndpoint: f.addr(), TLS: fx.src})

	resp, err := c.Do(testCtx(t), "space", "actor-1", 8080, http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		t.Fatalf("Do() over mTLS error = %v", err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || body != "GET /healthz " {
		t.Errorf("Do() = %d %q", resp.StatusCode, body)
	}
	tunnels := f.snapshot()
	if len(tunnels) != 1 {
		t.Fatalf("tunnels = %d, want 1", len(tunnels))
	}
	if tun := tunnels[0]; tun.clientCN != "scion-broker" || tun.alpn != "http/1.1" || tun.actorHdr != "space/actor-1" {
		t.Errorf("tunnel = client CN %q, ALPN %q, actor %q; want scion-broker, http/1.1, space/actor-1", tun.clientCN, tun.alpn, tun.actorHdr)
	}
}

func TestRouterClient_ControlPortOverMTLS(t *testing.T) {
	fx := newRouterTLSFixture(t)
	type seen struct{ actor, cn, proto string }
	got := make(chan seen, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		got <- seen{r.Header.Get(TargetActorHeader), cn, r.Proto}
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = fx.serverTLS()
	srv.StartTLS()
	defer srv.Close()

	// The listener is on 127.0.0.1; the source's ServerName
	// (atenet-router.ate-system.svc, from router_endpoint) is what the
	// server certificate is verified against.
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: srv.URL, ConnectEndpoint: "unused.invalid:8444", TLS: fx.src})
	resp, err := c.Do(testCtx(t), "space", "actor-1", ControlServerPort, http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		t.Fatalf("Do() over mTLS error = %v", err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || body != "ok" {
		t.Errorf("Do() = %d %q", resp.StatusCode, body)
	}
	if s := <-got; s != (seen{"space/actor-1", "scion-broker", "HTTP/1.1"}) {
		t.Errorf("server saw %+v, want actor header, client cert scion-broker, HTTP/1.1", s)
	}
}

func TestRouterClient_MTLSRejectsUntrustedRouter(t *testing.T) {
	fx := newRouterTLSFixture(t)
	other := newTestCA(t, "impostor")
	impostorCert, _ := other.issue(t, "router", []string{"atenet-router.ate-system.svc"}, false)
	cfg := fx.serverTLS()
	cfg.Certificates = []tls.Certificate{impostorCert}
	f := newFakeConnectRouter(t, cfg)
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "https://atenet-router.ate-system.svc", ConnectEndpoint: f.addr(), TLS: fx.src})
	if resp, err := c.Do(testCtx(t), "s", "a", 8080, http.MethodGet, "/", nil, nil); err == nil {
		_ = readBody(t, resp)
		t.Fatal("Do() through a router with an untrusted certificate succeeded")
	}
}

func TestRouterClient_RejectsPlaintextEndpointWhenTLSEnabled(t *testing.T) {
	fx := newRouterTLSFixture(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewRouterClientWithOptions(RouterOptions{
		Endpoint:        srv.URL, // http://...
		ConnectEndpoint: "unused.invalid:8444",
		TLS:             fx.src,
	})
	resp, err := c.Do(testCtx(t), "s", "a", ControlServerPort, http.MethodPost, "/scion/v1/bootstrap", strings.NewReader("{}"), nil)
	if err == nil {
		_ = readBody(t, resp)
		t.Fatal("Do() with TLS != nil and http:// endpoint = nil error, want refusal")
	}
	if !strings.Contains(err.Error(), "https://") {
		t.Errorf("error = %v, want message requiring https://", err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("plaintext server received %d request(s), want 0 (must fail closed without dialing)", got)
	}
}

func TestRouterClient_ConnectInner503NotRetried(t *testing.T) {
	f := newFakeConnectRouter(t, nil)
	f.setInnerStatus(http.StatusServiceUnavailable)
	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "http://unused.invalid", ConnectEndpoint: f.addr()})
	waits := recordSleeps(c)

	resp, err := c.Do(testCtx(t), "s", "a", 8080, http.MethodPost, "/v1/run", strings.NewReader("payload"), nil)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if len(*waits) != 0 {
		t.Errorf("inner-tunnel 503 triggered retries (%v); must not replay after reaching actor", *waits)
	}
	tunnels := f.snapshot()
	if len(tunnels) != 1 || len(tunnels[0].innerReqs) != 1 {
		t.Errorf("tunnels = %+v, want exactly 1 tunnel with 1 inner request", tunnels)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestRouterClient_BodyLimitEnforced(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewRouterClient(srv.URL)
	oversized := io.LimitReader(zeroReader{}, maxRouterRequestBodyBytes+1)
	resp, err := c.Do(testCtx(t), "s", "a", ControlServerPort, http.MethodPost, "/scion/v1/bootstrap", oversized, nil)
	if err == nil {
		_ = readBody(t, resp)
		t.Fatal("Do() with oversized body = nil error, want limit error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want body-limit error", err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("server received %d request(s), want 0", got)
	}
}

func TestRouterClient_ConnectCancelledDuringHandshakeReturnsPromptly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Read CONNECT request, then stall without sending any response.
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		<-done
	}()

	c := NewRouterClientWithOptions(RouterOptions{Endpoint: "http://unused.invalid", ConnectEndpoint: ln.Addr().String()})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	resp, err := c.Do(ctx, "s", "a", 8080, http.MethodGet, "/healthz", nil, nil)
	elapsed := time.Since(start)
	if err == nil {
		_ = readBody(t, resp)
		t.Fatal("Do() against a stalled CONNECT = nil error, want deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Do() took %v to cancel stalled CONNECT, want prompt return (<2s)", elapsed)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
