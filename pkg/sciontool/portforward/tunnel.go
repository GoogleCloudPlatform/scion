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

package portforward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	wire "github.com/GoogleCloudPlatform/scion/pkg/portforward"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/backoff"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/gorilla/websocket"
)

const (
	// reconnectDelay is the first reconnect delay; it doubles on each
	// consecutive failed connect up to maxReconnectDelay.
	reconnectDelay = 5 * time.Second
	// maxReconnectDelay caps the reconnect delay, and is the retry
	// interval while the hub answers 401.
	maxReconnectDelay    = 5 * time.Minute
	localRequestTimeout  = 60 * time.Second
	maxLocalResponseBody = 64 << 20
	// handshakeBodyLimit bounds how much of a refused upgrade's response
	// body goes into the error.
	handshakeBodyLimit = 1 << 10
)

// Package-level log hooks so tests can count state-change log lines.
var logError = log.Error

type Manager struct {
	client  *scionhub.Client
	http    *http.Client
	writeMu sync.Mutex

	// backoff paces reconnects after failed connects.
	backoff backoff.Backoff
	// connect runs one tunnel session (runOnce); after returns a channel
	// that fires after d (time.After). Both are overridable in tests.
	connect func(ctx context.Context) (connected bool, err error)
	after   func(d time.Duration) <-chan time.Time
}

func NewManager(client *scionhub.Client) *Manager {
	m := &Manager{
		client: client,
		http: &http.Client{
			Timeout: localRequestTimeout,
		},
		backoff: backoff.Backoff{
			Base:   reconnectDelay,
			Max:    maxReconnectDelay,
			Jitter: backoff.DefaultJitter,
		},
		after: time.After,
	}
	m.connect = m.runOnce
	return m
}

// Run keeps the tunnel connected until ctx is cancelled.
//
// After a session that connected, the next connect waits reconnectDelay.
// Each consecutive failed connect doubles the delay up to
// maxReconnectDelay. A 401 on the upgrade request moves straight to
// maxReconnectDelay: the agent token can be replaced in place while the
// container runs (by the token refresh loop or a reset-auth), so the tunnel
// keeps retrying slowly rather than stopping.
func (m *Manager) Run(ctx context.Context) {
	if m == nil || m.client == nil || !m.client.IsConfigured() {
		return
	}
	m.run(ctx)
}

func (m *Manager) run(ctx context.Context) {
	state := tunnelHealthy
	for {
		connected, err := m.connect(ctx)
		if ctx.Err() != nil {
			log.Info("Port-forward tunnel disconnected: context cancelled")
			return
		}
		var delay time.Duration
		switch {
		case connected:
			// The session was up; a drop afterwards starts a fresh backoff.
			m.backoff.Reset()
			state = tunnelHealthy
			delay = m.backoff.Failure()
			log.Error("Port-forward tunnel disconnected: %v; reconnecting in %s", err, delay)
		case scionhub.IsUnauthorized(err):
			delay = m.backoff.AtMax()
			if state != tunnelUnauthorized {
				logError("Port-forward tunnel: hub refused the agent credential (401); retrying every %s until it is accepted", m.backoff.Max)
			}
			state = tunnelUnauthorized
		default:
			delay = m.backoff.Failure()
			if state != tunnelBackoff {
				logError("Port-forward tunnel: connect failed: %v; backing off (up to %s between attempts)", err, m.backoff.Max)
			} else {
				log.Debug("Port-forward tunnel: connect failed: %v; next attempt in %s", err, delay)
			}
			state = tunnelBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-m.after(delay):
		}
	}
}

// tunnelState is the health of the reconnect loop, used to log only on
// changes.
type tunnelState int

const (
	tunnelHealthy      tunnelState = iota
	tunnelBackoff                  // consecutive connects failed
	tunnelUnauthorized             // the hub answered 401
)

// runOnce runs one tunnel session. connected reports whether the websocket
// upgrade succeeded; err is why the session ended or failed to start.
func (m *Manager) runOnce(ctx context.Context) (connected bool, err error) {
	endpoint, err := tunnelURL(m.client.HubURL(), m.client.AgentID())
	if err != nil {
		return false, err
	}
	header := http.Header{}
	header.Set("X-Scion-Agent-Token", m.client.AuthToken())
	if runID := m.client.RunID(); runID != "" {
		header.Set(scionhub.RunIDHeader, runID)
	}
	// Behind an identity-aware proxy the upgrade request needs the same
	// transport credential as every other hub request.
	if err := m.client.ApplyTransportHeaders(header); err != nil {
		log.Debug("Port-forward tunnel: no transport credential available: %v", err)
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			// On a refused upgrade the dialer returns the response with
			// (up to 1 KiB of) its body still readable.
			var body []byte
			if resp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, handshakeBodyLimit))
			}
			return false, &scionhub.StatusError{StatusCode: resp.StatusCode, Body: string(body)}
		}
		return false, err
	}
	defer func() { _ = conn.Close() }()
	// Close the connection when the context is cancelled to unblock ReadJSON.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	log.Info("Port-forward tunnel connected")
	for {
		var msg wire.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return true, err
		}
		if msg.Type != wire.MessageTypeRequest || msg.Request == nil {
			continue
		}
		go m.handleRequest(conn, msg.Request)
	}
}

func (m *Manager) handleRequest(conn *websocket.Conn, req *wire.Request) {
	resp := m.doLocalRequest(req)
	msgType := wire.MessageTypeResponse
	if resp.Error != "" {
		msgType = wire.MessageTypeError
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(wire.Message{Type: msgType, Response: resp}); err != nil {
		log.Error("Failed to write port-forward response: %v", err)
	}
}

func isLoopbackHost(host string) bool {
	if strings.ToLower(strings.TrimSpace(host)) == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (m *Manager) doLocalRequest(req *wire.Request) *wire.Response {
	if !isLoopbackHost(req.Host) {
		return &wire.Response{StreamID: req.StreamID, Error: "unauthorized host: only loopback addresses are allowed"}
	}
	target := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(req.Host, strconv.Itoa(req.Port)),
		Path:     req.Path,
		RawQuery: req.Query,
	}
	httpReq, err := http.NewRequest(req.Method, target.String(), bytes.NewReader(req.Body))
	if err != nil {
		return &wire.Response{StreamID: req.StreamID, Error: err.Error()}
	}
	httpReq.Header = cloneForwardHeaders(req.Header)
	httpReq.Host = target.Host

	resp, err := m.http.Do(httpReq)
	if err != nil {
		return &wire.Response{StreamID: req.StreamID, Error: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalResponseBody+1))
	if err != nil {
		return &wire.Response{StreamID: req.StreamID, Error: err.Error()}
	}
	if len(body) > maxLocalResponseBody {
		return &wire.Response{StreamID: req.StreamID, Error: "local response body too large"}
	}
	log.Debug("Local request forwarded: port=%d method=%s path=%s status=%d", req.Port, req.Method, req.Path, resp.StatusCode)
	return &wire.Response{
		StreamID: req.StreamID,
		Status:   resp.StatusCode,
		Header:   cloneForwardHeaders(resp.Header),
		Body:     body,
	}
}

func tunnelURL(hubURL, agentID string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(hubURL, "/") + "/api/v1/agents/" + url.PathEscape(agentID) + "/ports/tunnel")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported hub URL scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func cloneForwardHeaders(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for k, vals := range in {
		if hopByHopHeader(k) || sensitiveHeader(k) {
			continue
		}
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func hopByHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func sensitiveHeader(k string) bool {
	switch strings.ToLower(k) {
	case "authorization", "cookie", "x-scion-agent-token", "x-scion-broker-id", "x-scion-broker-hmac":
		return true
	default:
		return false
	}
}
