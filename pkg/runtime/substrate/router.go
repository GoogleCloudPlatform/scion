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
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TargetActorHeader is the header Substrate's inbound atenet-router uses to
// address a specific actor (substrate-runtime.md §1). On the CONNECT path it
// must be on the outer CONNECT request: the router ignores inner headers.
const TargetActorHeader = "ate-target-actor"

// ControlServerPort is the actor port reached through the router's HTTPS
// listener (Service port 443). Every other actor port goes through the
// CONNECT-over-TLS listener (RouterClient.ConnectEndpoint).
const ControlServerPort = 80

// DefaultRouterConnectPort is atenet-router's CONNECT-over-TLS Service port.
const DefaultRouterConnectPort = 8444

// routerRetryBackoff is the wait before each retry of a request the router
// answered 503 (actor parked while resuming, router at capacity, or v1.0.1's
// retryable "resume outlasted the request"). A 503 from the router means the
// request never reached the actor, so a retry is safe.
var routerRetryBackoff = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}

// connectTunnelIdleTimeout closes pooled CONNECT tunnels before the router's
// own 60s idle-upstream timeout does, so a pooled tunnel is not reused just
// as the router drops it.
const connectTunnelIdleTimeout = 50 * time.Second

// connectDialTimeout bounds the TCP dial plus TLS handshake plus CONNECT
// exchange for one tunnel.
const connectDialTimeout = 30 * time.Second

// RouterClient sends HTTP requests to an actor through Substrate's inbound
// atenet-router. Substrate v1 serves only mTLS:
//
//   - port 80 (the actor's control server, `sciontool substrate-serve`)
//     goes to Endpoint, the router's HTTPS listener, as one request per
//     call carrying TargetActorHeader;
//   - any other actor port goes through ConnectEndpoint: TLS, then
//     `CONNECT <actor>:<port>` carrying TargetActorHeader, then cleartext
//     HTTP/1.1 inside the tunnel, over a per-(actor, port) connection pool
//     that DropActor discards when the actor is deleted or moves workers.
//
// An inbound request through the router auto-resumes a SUSPENDED actor —
// callers that must not do that (e.g. a health probe against an actor the
// hub believes is suspended) should check the actor's state via the ateapi
// Control client first rather than call through RouterClient.
//
// The router answers 404 for an unknown actor and 503 while it cannot reach
// one yet; Do retries 503 with backoff and returns any other status to the
// caller as an ordinary response (a refused CONNECT is returned the same
// way, so callers check one status code whichever path served them).
type RouterClient struct {
	// Endpoint is the router's base URL for port 80, e.g.
	// "https://atenet-router.ate-system.svc".
	Endpoint string
	// HTTPClient performs port-80 requests. Callers set a deadline on the
	// context passed to Do instead of a blanket client timeout, because a
	// single fixed timeout can't fit every call this client makes (an
	// exec's timeout_s is caller-chosen; see pkg/runtime's doExec).
	HTTPClient *http.Client
	// ConnectEndpoint is the router's CONNECT-over-TLS "host:port", e.g.
	// "atenet-router.ate-system.svc:8444". Required for ports other than
	// ControlServerPort.
	ConnectEndpoint string
	// TLS returns a fresh client TLS config (server CA, server name, client
	// certificate) for each new connection, so rotated credentials apply
	// without a restart. Nil means plaintext, which only tests use.
	TLS TLSConfigFunc

	// sleep waits between 503 retries; overridable for tests.
	sleep func(context.Context, time.Duration) error

	mu    sync.Mutex
	pools map[connectPoolKey]*http.Transport
}

// connectPoolKey identifies one CONNECT tunnel pool.
type connectPoolKey struct {
	atespace, actor string
	port            int
}

// RouterOptions configures NewRouterClientWithOptions.
type RouterOptions struct {
	// Endpoint is the port-80 HTTPS base URL.
	Endpoint string
	// ConnectEndpoint is the CONNECT-over-TLS host:port.
	ConnectEndpoint string
	// TLS supplies per-connection TLS configs. Nil means plaintext (tests).
	TLS TLSConfigFunc
}

// NewRouterClient builds a plaintext RouterClient targeting endpoint for
// port 80, and endpoint's host on DefaultRouterConnectPort for other ports.
// Production code uses NewRouterClientWithOptions with a TLS source.
func NewRouterClient(endpoint string) *RouterClient {
	return NewRouterClientWithOptions(RouterOptions{Endpoint: endpoint})
}

// NewRouterClientWithOptions builds a RouterClient from opts. An empty
// ConnectEndpoint defaults to Endpoint's host on DefaultRouterConnectPort.
func NewRouterClientWithOptions(opts RouterOptions) *RouterClient {
	c := &RouterClient{
		Endpoint:        strings.TrimRight(opts.Endpoint, "/"),
		ConnectEndpoint: opts.ConnectEndpoint,
		TLS:             opts.TLS,
		sleep:           sleepContext,
		pools:           make(map[connectPoolKey]*http.Transport),
	}
	if c.ConnectEndpoint == "" {
		c.ConnectEndpoint = DefaultConnectEndpoint(opts.Endpoint)
	}
	transport := &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     connectTunnelIdleTimeout,
	}
	if opts.TLS != nil {
		tlsSource := opts.TLS
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialTLS(ctx, tlsSource, network, addr)
		}
	}
	c.HTTPClient = &http.Client{Transport: transport}
	return c
}

// DefaultConnectEndpoint derives the CONNECT-over-TLS "host:port" from a
// router base URL: its host on DefaultRouterConnectPort. Returns "" when the
// URL has no host.
func DefaultConnectEndpoint(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return net.JoinHostPort(u.Hostname(), strconv.Itoa(DefaultRouterConnectPort))
}

// maxRouterRequestBodyBytes bounds how much of a RouterClient.Do request
// body is buffered for 503 replay, matching pkg/sciontool/substrate's 64 MiB
// maxBootstrapBodyBytes cap so a misbehaving caller cannot exhaust broker RAM.
const maxRouterRequestBodyBytes = 64 << 20

// Do sends method+path to port on the actor identified by atespace/actor,
// through the router, with the given body and extra headers (hdr may be
// nil). The body is buffered (up to maxRouterRequestBodyBytes) so a
// router-originated 503 can be retried; if body implements io.Closer, Do
// closes it before returning. The caller closes the returned response body.
func (c *RouterClient) Do(ctx context.Context, atespace, actor string, port int, method, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	var payload []byte
	if body != nil {
		if closer, ok := body.(io.Closer); ok {
			defer func() { _ = closer.Close() }()
		}
		b, err := io.ReadAll(io.LimitReader(body, maxRouterRequestBodyBytes+1))
		if err != nil {
			return nil, fmt.Errorf("substrate: read router request body: %w", err)
		}
		if len(b) > maxRouterRequestBodyBytes {
			return nil, fmt.Errorf("substrate: router request body exceeds %d-byte limit", maxRouterRequestBodyBytes)
		}
		payload = b
	}
	for attempt := 0; ; attempt++ {
		resp, retryable, err := c.doOnce(ctx, atespace, actor, port, method, path, payload, hdr)
		if err != nil {
			return nil, err
		}
		if !retryable || attempt >= len(routerRetryBackoff) {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		sleep := c.sleep
		if sleep == nil {
			sleep = sleepContext
		}
		if err := sleep(ctx, routerRetryBackoff[attempt]); err != nil {
			return nil, err
		}
	}
}

func (c *RouterClient) doOnce(ctx context.Context, atespace, actor string, port int, method, path string, payload []byte, hdr http.Header) (*http.Response, bool, error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	if port == ControlServerPort || port == 0 {
		if c.TLS != nil {
			u, err := url.Parse(c.Endpoint)
			if err != nil || !strings.EqualFold(u.Scheme, "https") {
				return nil, false, fmt.Errorf("substrate: router_endpoint %q must use https:// when TLS is enabled", c.Endpoint)
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, bodyReader)
		if err != nil {
			return nil, false, err
		}
		copyHeaders(req.Header, hdr)
		req.Header.Set(TargetActorHeader, atespace+"/"+actor)
		client := c.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, false, err
		}
		// Port 80 serves pkg/sciontool/substrate/server.go, which never emits
		// HTTP 503; any 503 on this path therefore originates from
		// atenet-router before the request reached the actor and is safe to
		// retry.
		return resp, resp.StatusCode == http.StatusServiceUnavailable, nil
	}

	if c.ConnectEndpoint == "" {
		return nil, false, fmt.Errorf("substrate: router connect endpoint is not configured (needed for actor port %d)", port)
	}
	authority := net.JoinHostPort(actor, strconv.Itoa(port))
	req, err := http.NewRequestWithContext(ctx, method, "http://"+authority+path, bodyReader)
	if err != nil {
		return nil, false, err
	}
	copyHeaders(req.Header, hdr)
	resp, err := c.connectTransport(atespace, actor, port).RoundTrip(req)
	if err != nil {
		var refused *connectRefusedError
		if errors.As(err, &refused) {
			// Surface a refused outer CONNECT (404 unknown actor, 503 not
			// reachable yet) as the response itself. Only a 503 on the outer
			// CONNECT (before any bytes reached the actor) is retryable.
			return &http.Response{
				Status:     refused.status,
				StatusCode: refused.code,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(refused.body)),
				Request:    req,
			}, refused.code == http.StatusServiceUnavailable, nil
		}
		return nil, false, err
	}
	// The tunnel succeeded and the request reached the actor; never replay
	// inner responses (including an actor-returned 503).
	return resp, false, nil
}

// DropActor closes and forgets every pooled CONNECT tunnel to
// atespace/actor. Called when an actor is deleted or moves workers so stale
// tunnels to the previous worker are not reused.
func (c *RouterClient) DropActor(atespace, actor string) {
	c.mu.Lock()
	var drop []*http.Transport
	for k, t := range c.pools {
		if k.atespace == atespace && k.actor == actor {
			drop = append(drop, t)
			delete(c.pools, k)
		}
	}
	c.mu.Unlock()
	for _, t := range drop {
		t.CloseIdleConnections()
	}
}

// connectTransport returns the pooled transport tunnelling to
// atespace/actor:port, creating it on first use.
func (c *RouterClient) connectTransport(atespace, actor string, port int) *http.Transport {
	key := connectPoolKey{atespace: atespace, actor: actor, port: port}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pools == nil {
		c.pools = make(map[connectPoolKey]*http.Transport)
	}
	if t, ok := c.pools[key]; ok {
		return t
	}
	t := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return c.dialConnect(ctx, atespace, actor, port)
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     connectTunnelIdleTimeout,
		ForceAttemptHTTP2:   false,
	}
	c.pools[key] = t
	return t
}

// connectRefusedError is a non-2xx answer to the outer CONNECT.
type connectRefusedError struct {
	code   int
	status string
	body   string
}

func (e *connectRefusedError) Error() string {
	return fmt.Sprintf("substrate: router refused CONNECT: %s", e.status)
}

// maxConnectRefusalBody caps how much of a refused CONNECT's body is kept.
const maxConnectRefusalBody = 4 << 10

// dialConnect opens one tunnel: TCP (+ TLS) to ConnectEndpoint, then
// `CONNECT <actor>:<port>` with TargetActorHeader on the outer request.
func (c *RouterClient) dialConnect(ctx context.Context, atespace, actor string, port int) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, connectDialTimeout)
	defer cancel()

	var conn net.Conn
	var err error
	if c.TLS != nil {
		conn, err = dialTLS(dctx, c.TLS, "tcp", c.ConnectEndpoint)
	} else {
		var d net.Dialer
		conn, err = d.DialContext(dctx, "tcp", c.ConnectEndpoint)
	}
	if err != nil {
		return nil, fmt.Errorf("substrate: dial router connect endpoint %s: %w", c.ConnectEndpoint, err)
	}
	if deadline, ok := dctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(dctx, func() {
		_ = conn.SetDeadline(time.Now())
	})

	authority := net.JoinHostPort(actor, strconv.Itoa(port))
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: authority},
		Host:   authority,
		Header: http.Header{TargetActorHeader: []string{atespace + "/" + actor}},
	}
	if err := req.Write(conn); err != nil {
		stop()
		_ = conn.Close()
		if cerr := dctx.Err(); cerr != nil {
			return nil, fmt.Errorf("substrate: write CONNECT %s: %w", authority, cerr)
		}
		return nil, fmt.Errorf("substrate: write CONNECT %s: %w", authority, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		stop()
		_ = conn.Close()
		if cerr := dctx.Err(); cerr != nil {
			return nil, fmt.Errorf("substrate: read CONNECT %s response: %w", authority, cerr)
		}
		return nil, fmt.Errorf("substrate: read CONNECT %s response: %w", authority, err)
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxConnectRefusalBody))
		stop()
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, &connectRefusedError{code: resp.StatusCode, status: resp.Status, body: string(b)}
	}
	if !stop() {
		_ = conn.Close()
		return nil, fmt.Errorf("substrate: CONNECT %s: %w", authority, dctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() == 0 {
		return conn, nil
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn replays bytes the CONNECT response reader buffered past the
// response head before reading from the connection again.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// dialTLS dials addr and completes a TLS handshake with a fresh config from
// source. The negotiated protocol is pinned to HTTP/1.1: the CONNECT
// listener speaks only HTTP/1.1, and the port-80 transport here is
// HTTP/1.1 as well.
func dialTLS(ctx context.Context, source TLSConfigFunc, network, addr string) (net.Conn, error) {
	cfg, err := source(ctx)
	if err != nil {
		return nil, err
	}
	cfg = cfg.Clone()
	cfg.NextProtos = []string{"http/1.1"}
	if cfg.ServerName == "" {
		if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
			cfg.ServerName = host
		}
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("substrate: TLS handshake with router %s: %w", addr, err)
	}
	return tc, nil
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
