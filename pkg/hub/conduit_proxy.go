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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// The port proxy over conduit (design §3.10). TCP happens only on the
// agent's in-container loopback, at the sciontool end: the hub resolves
// the agent's live conduit session, opens a grant-checked TCP stream on it
// and hands that stream to httputil.ReverseProxy as its only connection.
// Nothing on this path dials the network; the proxy's transport has no
// dialer of its own.

// conduitProxyHost is the only target host of a conduit TCP stream.
const conduitProxyHost = "127.0.0.1"

// ErrCodeAgentOffline: the agent has no conduit session and no
// port-forward tunnel.
const ErrCodeAgentOffline = "agent_offline"

// errConduitNoRoute: this node has no conduit router (relay not running).
var errConduitNoRoute = errors.New("conduit: no router on this node")

// openConduitPort opens a TCP stream to 127.0.0.1:port inside the agent
// over its conduit session, with a grant minted for identity against
// exactly the resolved session. Errors wrap router.ErrNoSession when the
// agent has no eligible session.
func (s *Server) openConduitPort(ctx context.Context, identity Identity, agent *store.Agent, port int) (net.Conn, error) {
	rt := s.conduit.Load()
	if rt == nil || rt.router == nil {
		return nil, errConduitNoRoute
	}
	params := map[string]string{grant.ParamHost: conduitProxyHost, grant.ParamPort: strconv.Itoa(port)}
	req := router.Request{
		Op:    router.OpStream,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID, Capability: grant.StreamKindTCP},
		Agent: relay.AgentIncarnationFacts{LaunchID: agent.LaunchID, Generation: int64(agent.Generation)},
	}
	var conn net.Conn
	err := rt.router.Do(ctx, req, func(ctx context.Context, res router.Resolved) error {
		tok, _, err := s.mintConduitGrant(ctx, conduitGrantRequest{
			Identity: identity,
			Agent:    agent,
			Stream:   grant.StreamHeader{Kind: grant.StreamKindTCP, Params: params},
			Target: grant.Target{
				Kind:                grant.TargetKindAgent,
				ID:                  agent.ID,
				EndpointIncarnation: res.Want.Incarnation,
				SessionID:           res.Record.SessionID,
				ConnectionEpoch:     res.Record.ConnectionEpoch,
			},
		})
		if err != nil {
			return err
		}
		st, err := res.Session.OpenStream(ctx, &conduitv1.StreamOpen{
			Kind:   conduitv1.StreamKind_STREAM_KIND_TCP,
			Params: params,
			Grant:  tok,
		})
		if err != nil {
			return err
		}
		conn = newStreamConn(st, agent.ID, port)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// conduitProxyTransport returns a transport whose only connection is
// conn: the first dial returns it, any further dial fails. It has no
// network dialer, no proxy and no keep-alive, so the proxied request
// cannot reach anything but the conduit stream.
func conduitProxyTransport(conn net.Conn) *http.Transport {
	var once sync.Once
	return &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var c net.Conn
			once.Do(func() { c = conn })
			if c == nil {
				return nil, errors.New("conduit proxy: the stream was already used")
			}
			return c, nil
		},
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("conduit proxy: TLS dial is not allowed")
		},
		DisableKeepAlives:  true,
		DisableCompression: true,
		MaxConnsPerHost:    1,
	}
}

// serveConduitProxy proxies r to the agent port over conn (a conduit
// stream). Requests, responses, WebSocket upgrades and event streams are
// streamed; there is no body buffer and no proxy timeout beyond the
// request's own lifetime. Credentials are stripped from the request and
// the response is sandboxed exactly as on the tunnel path.
func (s *Server) serveConduitProxy(w http.ResponseWriter, r *http.Request, agent *store.Agent, port int, proxyPath string, conn net.Conn) {
	start := time.Now()
	reqPath := "/" + strings.TrimPrefix(proxyPath, "/")
	target := net.JoinHostPort(conduitProxyHost, strconv.Itoa(port))
	status := 0
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.Out.URL.Path = reqPath
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = target
			for k := range pr.Out.Header {
				if sensitiveHeader(k) {
					pr.Out.Header.Del(k)
				}
			}
		},
		Transport:     conduitProxyTransport(conn),
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			status = resp.StatusCode
			for k := range resp.Header {
				if stripFromProxyResponse(k) {
					resp.Header.Del(k)
				}
			}
			resp.Header.Set("Content-Security-Policy", untrustedContentSandboxCSP)
			resp.Header.Set("X-Content-Type-Options", "nosniff")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status = http.StatusBadGateway
			if r.Context().Err() == nil {
				slog.Debug("Conduit proxy request failed", "agent_id", agent.ID, "port", port, "error", err)
			}
			writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, "Port proxy failed", nil)
		},
	}
	defer func() { _ = conn.Close() }()
	rp.ServeHTTP(w, r)
	slog.Debug("Proxy request",
		"agent_id", agent.ID,
		"port", port,
		"caller", identityStringFromContext(r.Context()),
		"method", r.Method,
		"path", reqPath,
		"status", status,
		"transport", "conduit",
		"duration", time.Since(start),
	)
}

// writeConduitProxyError maps an openConduitPort failure (other than "no
// session") to a response.
func writeConduitProxyError(w http.ResponseWriter, r *http.Request, agentID string, err error) {
	var ce *conduit.CloseError
	switch {
	case errors.Is(err, errConduitForbidden):
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
	case errors.Is(err, router.ErrRegistryUnavailable):
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "Conduit routing is unavailable", nil)
	case errors.As(err, &ce) && ce.Code == conduit.CloseRelayTimeout:
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, "Nothing is listening on the agent port", nil)
	default:
		if r.Context().Err() == nil {
			slog.Warn("Conduit proxy: opening the port stream failed", "agent_id", agentID, "error", err)
		}
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, "Port proxy failed", nil)
	}
}

// writeAgentOffline writes the 503 agent_offline response: the agent has
// neither a conduit session nor a port-forward tunnel. Exposed ports are
// kept; they work again when the agent reconnects.
func writeAgentOffline(w http.ResponseWriter, r *http.Request) {
	if isBrowserRequest(r) {
		writeProxyErrorHTML(w, http.StatusServiceUnavailable, "Agent Offline",
			"The agent is not connected to the hub right now. Its exposed ports will be reachable again when it reconnects.")
		return
	}
	writeError(w, http.StatusServiceUnavailable, ErrCodeAgentOffline, "The agent is not connected to the hub", nil)
}

// streamConn adapts a conduit stream to net.Conn for the proxy transport.
// Deadlines are not supported (the transport sets none on plain HTTP
// connections); the stream ends when it is closed or its session ends.
type streamConn struct {
	conduit.Stream
	local, remote net.Addr
}

func newStreamConn(st conduit.Stream, agentID string, port int) *streamConn {
	return &streamConn{
		Stream: st,
		local:  conduitAddr("hub"),
		remote: conduitAddr("agent:" + agentID + ":" + strconv.Itoa(port)),
	}
}

func (c *streamConn) LocalAddr() net.Addr              { return c.local }
func (c *streamConn) RemoteAddr() net.Addr             { return c.remote }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

// CloseWrite half-closes the stream when it supports it.
func (c *streamConn) CloseWrite() error {
	if hc, ok := c.Stream.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}

type conduitAddr string

func (conduitAddr) Network() string  { return "conduit" }
func (a conduitAddr) String() string { return string(a) }
