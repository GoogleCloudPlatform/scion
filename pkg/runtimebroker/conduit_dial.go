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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// The broker's conduit session (GET /api/v1/conduit, principal "broker").
//
// It is dialed alongside the control channel and never replaces it: the
// control channel keeps carrying every broker RPC, sync operation, PTY and
// the broker's presence. The conduit session carries no work yet, so its
// Hello advertises no stream kinds and no RPCs. The two connections have
// independent lifecycles: each reconnects on its own backoff, and losing
// one never closes the other.
//
// Fallback. Any refusal before the WebSocket upgrade (a non-101 answer,
// or a dial or TLS failure) means the hub does not serve this broker a
// conduit session for now; the broker stays on the control channel alone
// and re-probes on its own backoff, capped at conduitProbeMax. A 404
// (hub.conduit off, or a hub without the endpoint) and the 403 answered by
// a hub that admits agent principals only wait the full cap, so a hub with
// the experiment off sees at most one probe per conduitProbeMax. Closes
// after the upgrade follow the session close-code rules instead.

const (
	// conduitEndpointPath is the hub's conduit endpoint.
	conduitEndpointPath = "/api/v1/conduit"
	// conduitProbeMax caps the re-probe delay after a pre-upgrade refusal.
	conduitProbeMax = 5 * time.Minute
	// conduitRefusalBodyLimit bounds how much of a refused upgrade's body
	// is read to classify it.
	conduitRefusalBodyLimit = 4096
	// conduitAgentsOnlyMessage is the message of the 403 a hub answers
	// when it admits agent principals only.
	conduitAgentsOnlyMessage = "Conduit sessions are open to agent principals only"
	// closeSupersededIncarnation is 4409 (superseded_incarnation). It is
	// defined in pkg/conduit/relay, which a dialer does not import.
	closeSupersededIncarnation uint32 = 4409
)

// conduitProcessIncarnation is this process's endpoint incarnation, the
// value every broker Hello presents: a new id per process start, so a
// restarted broker is a new incarnation.
var conduitProcessIncarnation = uuid.NewString()

// ConduitDialConfig configures a broker's conduit dialer.
type ConduitDialConfig struct {
	HubEndpoint string
	BrokerID    string
	SecretKey   []byte
	// TransportSource and TransportMode add the transport-layer OIDC
	// credential, as on the control channel.
	TransportSource transportauth.TokenSource
	TransportMode   transportauth.HeaderMode
	// Version is reported as Hello.client_version.
	Version string
	// Incarnation is Hello.capabilities.endpoint_incarnation ("" =
	// conduitProcessIncarnation).
	Incarnation string
	// ExecScope is Hello.capabilities.exec_scope: the runtime target of a
	// flat Runtime Broker, "" otherwise (the hub's value for the broker).
	ExecScope string

	// Test seams. Clock drives backoff and session timers (nil: real);
	// Rand is the jitter source of both backoffs (nil: math/rand); WS
	// overrides the WebSocket dialer; Session tunes the session config;
	// OnEnd sees each attempt's or session's end with the chosen delay, or
	// the error Run stops with.
	Clock   clock.Clock
	Rand    func(n int64) int64
	WS      *websocket.Dialer
	Session conduit.Config
	OnEnd   func(end conduit.End, delay time.Duration, err error)
	Log     *slog.Logger
}

// ConduitDialer keeps a broker's conduit session to one hub.
type ConduitDialer struct {
	cfg ConduitDialConfig
	url string
	log *slog.Logger
	// session is the Reconnector's backoff (closes after the upgrade);
	// probe is the pre-upgrade refusal backoff. Both are used only on
	// Run's goroutine.
	session *conduit.Backoff
	probe   *conduit.Backoff
}

// errConduitForbidden stops the dialer after a 4403 close (terminal per
// the close-code table). The control channel is unaffected.
var errConduitForbidden = errors.New("conduit: hub refused the broker session (4403)")

// NewConduitDialer validates cfg and returns a dialer.
func NewConduitDialer(cfg ConduitDialConfig) (*ConduitDialer, error) {
	if cfg.BrokerID == "" {
		return nil, errors.New("conduit: broker id is required")
	}
	u, err := conduitEndpointURL(cfg.HubEndpoint)
	if err != nil {
		return nil, err
	}
	if cfg.Incarnation == "" {
		cfg.Incarnation = conduitProcessIncarnation
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &ConduitDialer{
		cfg:     cfg,
		url:     u,
		log:     log,
		session: &conduit.Backoff{Rand: cfg.Rand},
		probe:   &conduit.Backoff{Max: conduitProbeMax, Rand: cfg.Rand},
	}, nil
}

// conduitEndpointURL returns the ws(s) URL of the hub's conduit endpoint.
func conduitEndpointURL(hubEndpoint string) (string, error) {
	u, err := url.Parse(hubEndpoint)
	if err != nil {
		return "", fmt.Errorf("conduit: invalid hub endpoint: %w", err)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	// The path replaces any path of the endpoint, as on the control
	// channel, so the signed path is the requested one.
	u.Path = conduitEndpointPath
	return u.String(), nil
}

// Run keeps the session until ctx ends (ctx.Err()) or the hub refuses the
// broker with 4403 (errConduitForbidden).
func (d *ConduitDialer) Run(ctx context.Context) error {
	cfg := d.cfg.Session
	cfg.Clock = d.cfg.Clock
	r := &conduit.Reconnector{
		Dialer: &conduitWSDialer{url: d.url, header: d.header, ws: d.cfg.WS},
		Config: cfg,
		Hello:  d.hello,
		OnSession: func(_ context.Context, _ conduit.Session, w *conduitv1.Welcome) {
			d.log.Info("Conduit session established alongside the control channel",
				"session_id", w.GetSessionId(), "relay", w.GetRelayInstanceId(), "incarnation", w.GetEndpointIncarnation())
		},
		Backoff: d.session,
		Decide: func(_ context.Context, end conduit.End, delay time.Duration) (time.Duration, error) {
			delay, err := d.decide(end, delay)
			if d.cfg.OnEnd != nil {
				d.cfg.OnEnd(end, delay, err)
			}
			return delay, err
		},
	}
	return r.Run(ctx)
}

// hello is the broker's Hello: no stream kinds or RPCs (the session serves
// none yet), the process incarnation and the exec scope.
func (d *ConduitDialer) hello() *conduitv1.Hello {
	return &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   d.cfg.BrokerID,
		ClientVersion: d.cfg.Version,
		Capabilities: &conduitv1.Capabilities{
			EndpointIncarnation: d.cfg.Incarnation,
			ExecScope:           d.cfg.ExecScope,
		},
	}
}

// header signs each upgrade afresh (the HMAC carries a timestamp and a
// nonce).
func (d *ConduitDialer) header(context.Context) (http.Header, error) {
	return brokerUpgradeHeaders(d.cfg.HubEndpoint, conduitEndpointPath, d.cfg.BrokerID, d.cfg.SecretKey, d.cfg.TransportSource, d.cfg.TransportMode)
}

// decide implements Reconnector.Decide. delay is the Reconnector's choice
// from the session backoff (for a planned drain, the GoAway window).
//
//	pre-upgrade refusal (non-101, dial or TLS failure)
//	  404, 403 agents only                  INFO, re-probe after conduitProbeMax
//	  401, other 403                        ERROR, probe backoff (cap conduitProbeMax)
//	  anything else                         WARN, probe backoff
//	after the upgrade (close-code rules)
//	  4403                                  stop the conduit dialer (terminal)
//	  4400                                  redial after conduit.BackoffMax
//	  4409                                  redial after conduit.BackoffMax
//	  4503                                  redial within the GoAway window
//	  4401, 4504, 1011, anything else       session backoff (1s to 60s, full jitter)
func (d *ConduitDialer) decide(end conduit.End, delay time.Duration) (time.Duration, error) {
	var pre *conduitPreUpgradeError
	if errors.As(end.DialErr, &pre) {
		// A pre-upgrade refusal is a fallback, not a session failure:
		// it uses the probe backoff and leaves the session backoff alone.
		d.session.Reset()
		return d.fallback(pre), nil
	}
	if end.DialErr == nil {
		// A session was established: the next refusal probes from the
		// start of the probe backoff again.
		d.probe.Reset()
	}
	cause := conduitEndCause(end)
	switch end.Code() {
	case conduit.CloseForbidden:
		d.log.Error("Conduit: hub refused the broker session; not reconnecting it (the control channel is unaffected)", "cause", cause)
		return 0, errConduitForbidden
	case conduit.CloseProtocolError:
		d.log.Error("Conduit: protocol error; reconnecting after the maximum backoff", "cause", cause, "retry_in", conduit.BackoffMax)
		return conduit.BackoffMax, nil
	case closeSupersededIncarnation:
		// The hub keeps no authoritative broker incarnation yet, so it
		// cannot send 4409 to a broker. Once it does, 4409 must follow
		// the close-code table in full: the maximum backoff, and a
		// terminal stop after refusals have persisted for 10 minutes.
		d.log.Error("Conduit: incarnation refused; reconnecting after the maximum backoff", "cause", cause, "retry_in", conduit.BackoffMax)
		return conduit.BackoffMax, nil
	}
	if end.DialErr != nil {
		d.log.Warn("Conduit: session handshake failed; retrying", "cause", cause, "retry_in", delay)
	} else {
		d.log.Info("Conduit session ended; reconnecting", "cause", cause, "retry_in", delay)
	}
	return delay, nil
}

// fallback logs a pre-upgrade refusal and returns the re-probe delay.
func (d *ConduitDialer) fallback(e *conduitPreUpgradeError) time.Duration {
	switch {
	case e.StatusCode == http.StatusNotFound:
		d.log.Info("Conduit: hub does not serve conduit sessions (HTTP 404); using the control channel only", "retry_in", conduitProbeMax)
		return conduitProbeMax
	case e.agentsOnly():
		d.log.Info("Conduit: hub admits agent principals only (HTTP 403); using the control channel only", "retry_in", conduitProbeMax)
		return conduitProbeMax
	}
	delay := d.probe.Next()
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		d.log.Error("Conduit: hub refused the broker credentials; using the control channel only", "status", e.StatusCode, "error", e.Err, "retry_in", delay)
	} else {
		d.log.Warn("Conduit: connect failed; using the control channel only", "error", e.DialError, "retry_in", delay)
	}
	return delay
}

func conduitEndCause(end conduit.End) string {
	switch {
	case end.DialErr != nil:
		return end.DialErr.Error()
	case end.GoAway != nil:
		return fmt.Sprintf("code %d: %s", end.GoAway.GetCode(), end.GoAway.GetReason())
	case end.SessionErr != nil:
		return end.SessionErr.Error()
	}
	return "closed"
}

// conduitPreUpgradeError is a refusal before the WebSocket upgrade: the
// hub answered without 101 (StatusCode set, Body its first bytes) or
// could not be reached.
type conduitPreUpgradeError struct {
	*ws.DialError
	Body string
}

func (e *conduitPreUpgradeError) Unwrap() error { return e.DialError }

// agentsOnly reports whether e is the 403 of a hub that admits agent
// principals only.
func (e *conduitPreUpgradeError) agentsOnly() bool {
	return e.StatusCode == http.StatusForbidden && strings.Contains(e.Body, conduitAgentsOnlyMessage)
}

// conduitWSDialer is ws.Dialer, except that a refused upgrade keeps the
// first bytes of the response body (to tell the agents-only 403 apart)
// and every pre-upgrade failure is a *conduitPreUpgradeError.
type conduitWSDialer struct {
	url    string
	header func(context.Context) (http.Header, error)
	ws     *websocket.Dialer
}

var _ transport.Dialer = (*conduitWSDialer)(nil)

// Dial implements transport.Dialer.
func (d *conduitWSDialer) Dial(ctx context.Context) (transport.Conn, error) {
	h, err := d.header(ctx)
	if err != nil {
		return nil, &conduitPreUpgradeError{DialError: &ws.DialError{Err: fmt.Errorf("handshake headers: %w", err)}}
	}
	wd := d.ws
	if wd == nil {
		wd = &websocket.Dialer{
			ReadBufferSize:   wsprotocol.DefaultReadBufferSize,
			WriteBufferSize:  wsprotocol.DefaultWriteBufferSize,
			HandshakeTimeout: 30 * time.Second,
			Proxy:            http.ProxyFromEnvironment,
		}
	}
	c, resp, err := wd.DialContext(ctx, d.url, h)
	if err != nil {
		e := &conduitPreUpgradeError{DialError: &ws.DialError{Err: err}}
		if resp != nil {
			e.StatusCode = resp.StatusCode
			e.Header = resp.Header
			if resp.Body != nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, conduitRefusalBodyLimit))
				e.Body = string(b)
				_ = resp.Body.Close()
			}
		}
		return nil, e
	}
	return ws.New(c, ws.Options{}), nil
}

// startConduit starts the conduit dialer alongside the control channel.
// It is independent of the control channel: neither one's failure stops
// the other, and Stop ends both.
func (hc *HubConnection) startConduit(ctx context.Context, server *Server) {
	var execScope string
	if fi := server.flatInstance(); fi != nil {
		execScope = fi.Identity.RuntimeTarget.ID
	}
	d, err := NewConduitDialer(ConduitDialConfig{
		HubEndpoint:     hc.HubEndpoint,
		BrokerID:        hc.BrokerID,
		SecretKey:       hc.SecretKey,
		TransportSource: hc.TransportSource,
		TransportMode:   hc.TransportMode,
		Version:         server.version,
		ExecScope:       execScope,
		Log:             logging.Subsystem("broker.conduit").With("name", hc.Name),
	})
	if err != nil {
		slog.Warn("Conduit dialer not started; using the control channel only", "name", hc.Name, "error", err)
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	hc.mu.Lock()
	hc.conduitCancel = cancel
	hc.mu.Unlock()
	exitHook := hc.conduitExitHook
	hc.conduitWg.Add(1)
	go func() {
		defer hc.conduitWg.Done()
		if err := d.Run(cctx); err != nil && cctx.Err() == nil {
			slog.Warn("Conduit dialer stopped; using the control channel only", "name", hc.Name, "error", err)
		}
		if exitHook != nil {
			exitHook()
		}
	}()
}

// stopConduit stops the conduit dialer and waits for it to exit.
func (hc *HubConnection) stopConduit() {
	hc.mu.Lock()
	cancel := hc.conduitCancel
	hc.conduitCancel = nil
	hc.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	hc.conduitWg.Wait()
}
