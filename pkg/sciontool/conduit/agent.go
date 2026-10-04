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

// Package conduit is the agent end of a conduit session (design §3.3,
// §3.10): sciontool dials the hub's /api/v1/conduit endpoint, keeps the
// session alive across relay drains and failures, and serves inbound TCP
// streams to in-container loopback ports after verifying each stream's
// grant.
package conduit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Environment variables read by sciontool.
const (
	// EnvLaunchID carries the agent's launch id, set by the broker on
	// create. It is presented as Hello.capabilities.endpoint_incarnation.
	EnvLaunchID = "SCION_LAUNCH_ID"
	// EnvHubConduit is set to "true" by a hub that serves conduit
	// sessions (hub.conduit on). Without it sciontool never dials
	// /api/v1/conduit.
	EnvHubConduit = "SCION_HUB_CONDUIT"
)

// Close codes this package classifies that pkg/conduit does not define
// (they belong to pkg/conduit/relay, which a target does not import).
const (
	closeTargetNotFound uint32 = 4404
	closeSuperseded     uint32 = 4409
	closeInternalError  uint32 = 1011
)

// credentialRefreshMinInterval rate-limits credential refreshes triggered
// by 4401, so a hub that keeps refusing the credential (e.g. a deleted
// agent) does not cause a refresh per attempt.
const credentialRefreshMinInterval = time.Minute

// ErrUnsupported means the hub answered the conduit endpoint with 404: it
// does not serve conduit sessions (an old hub, or hub.conduit off). The
// caller falls back to the port-forward tunnel.
var ErrUnsupported = errors.New("conduit: hub does not serve conduit sessions")

// TerminalError stops the dialer: the hub refused this agent in a way a
// reconnect cannot fix (4409 superseded launch, 4403 forbidden, 4404 not
// found, or an HTTP 403 on the upgrade).
type TerminalError struct {
	Code   uint32 // close code, or 0 for an HTTP refusal
	Status int    // HTTP status of a refused upgrade, else 0
	Reason string
}

func (e *TerminalError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("conduit: hub refused the session (HTTP %d): %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("conduit: hub refused the session (code %d): %s", e.Code, e.Reason)
}

// Options configure an Agent.
type Options struct {
	// HubURL is the hub's http(s) base URL.
	HubURL string
	// AgentID and ProjectID identify this agent.
	AgentID, ProjectID string
	// LaunchID is the broker-provided launch id (EnvLaunchID); empty
	// when the broker sent none.
	LaunchID string
	// ClientVersion is reported in Hello.
	ClientVersion string
	// Token returns the current agent token.
	Token func() string
	// ApplyTransportHeaders adds the transport credential (IAP/OIDC), if
	// any, to a hub request. Optional.
	ApplyTransportHeaders func(http.Header) error
	// RefreshCredential obtains a fresh agent token after the hub refused
	// the current one (4401 / HTTP 401). Optional.
	RefreshCredential func(ctx context.Context) error
	// HTTPClient fetches grant keys; pass the hub client's HTTP client so
	// transport settings live in one place (default http.DefaultClient).
	HTTPClient *http.Client
	// WS overrides the WebSocket dialer (tests, TLS).
	WS *websocket.Dialer
	// KeyRefreshInterval is the grant key refresh period
	// (default and maximum KeyRefreshInterval).
	KeyRefreshInterval time.Duration
	// DialLocal dials an in-container loopback address (test seam;
	// default net.Dialer). It is only ever called with a 127.0.0.1
	// address.
	DialLocal func(ctx context.Context, network, addr string) (net.Conn, error)
	// OnSession is called after each admitted session's grant keys are
	// installed. Optional.
	OnSession func(*conduitv1.Welcome)
	// Clock, Backoff and Session tune timers and the session config
	// (tests).
	Clock   clock.Clock
	Backoff *core.Backoff
	Session core.Config
}

// Agent is the agent's conduit dialer and TCP target.
type Agent struct {
	opts   Options
	url    string
	clk    clock.Clock
	keys   target.KeyHolder
	replay grant.ReplayCache

	mu            sync.Mutex
	applied       []*conduitv1.Welcome // Welcomes whose keys were applied, newest last
	lastCredRefsh time.Time
}

// New validates opts and returns an Agent.
func New(opts Options) (*Agent, error) {
	if opts.AgentID == "" || opts.ProjectID == "" {
		return nil, errors.New("conduit: agent and project ids are required")
	}
	if opts.Token == nil {
		return nil, errors.New("conduit: Token is required")
	}
	u, err := EndpointURL(opts.HubURL)
	if err != nil {
		return nil, err
	}
	if opts.KeyRefreshInterval <= 0 || opts.KeyRefreshInterval > KeyRefreshInterval {
		opts.KeyRefreshInterval = KeyRefreshInterval
	}
	opts.HTTPClient = keyClient(opts.HTTPClient)
	if opts.DialLocal == nil {
		d := &net.Dialer{Timeout: localDialTimeout}
		opts.DialLocal = d.DialContext
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real()
	}
	return &Agent{
		opts:   opts,
		url:    u,
		clk:    clk,
		replay: grant.NewMemoryReplayCache(clk.Now, 0),
	}, nil
}

// EndpointURL returns the ws(s) URL of the hub's conduit endpoint.
func EndpointURL(hubURL string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(hubURL, "/") + "/api/v1/conduit")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("conduit: unsupported hub URL scheme %q", u.Scheme)
	}
	return u.String(), nil
}

// Run keeps a conduit session to the hub until ctx ends (ctx.Err()), the
// hub turns out not to serve conduit (ErrUnsupported), or the hub refuses
// the agent terminally (*TerminalError).
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.refreshKeysLoop(ctx)

	cfg := a.opts.Session
	cfg.Clock = a.clk
	cfg.StreamHandler = core.StreamHandlerFunc(a.handleStream)
	r := &core.Reconnector{
		Dialer: &ws.Dialer{URL: a.url, Header: a.header, WS: a.opts.WS},
		Config: cfg,
		Hello:  a.hello,
		OnSession: func(_ context.Context, s core.Session, w *conduitv1.Welcome) {
			a.applyWelcomeKeys(w)
			log.Info("Conduit session established (session %s, relay %s, incarnation %q)",
				w.GetSessionId(), w.GetRelayInstanceId(), w.GetEndpointIncarnation())
			if a.opts.OnSession != nil {
				a.opts.OnSession(w)
			}
		},
		Backoff: a.opts.Backoff,
		Decide:  a.decide,
	}
	return r.Run(ctx)
}

// hello builds the Hello. The launch id is presented as the endpoint
// incarnation; the hub decides the admitted one (Welcome).
func (a *Agent) hello() *conduitv1.Hello {
	return &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		PrincipalId:   a.opts.AgentID,
		ClientVersion: a.opts.ClientVersion,
		Capabilities: &conduitv1.Capabilities{
			StreamKinds:         []string{grant.StreamKindTCP},
			EndpointIncarnation: a.opts.LaunchID,
		},
	}
}

// header returns the upgrade request's credentials, read afresh for every
// attempt so a refreshed token is used.
func (a *Agent) header(context.Context) (http.Header, error) {
	h := http.Header{}
	h.Set("X-Scion-Agent-Token", a.opts.Token())
	if a.opts.ApplyTransportHeaders != nil {
		if err := a.opts.ApplyTransportHeaders(h); err != nil {
			log.Debug("Conduit: no transport credential available: %v", err)
		}
	}
	return h, nil
}

// Action is what the dialer does after an attempt or session ends.
type Action int

const (
	// ActionBackoff redials after the default delay (exponential backoff
	// with full jitter, or the GoAway reconnect window for a planned
	// drain).
	ActionBackoff Action = iota
	// ActionMaxBackoff redials after BackoffMax (protocol errors).
	ActionMaxBackoff
	// ActionRefreshCredential refreshes the agent token, then redials
	// after the default delay.
	ActionRefreshCredential
	// ActionStop stops with a *TerminalError.
	ActionStop
	// ActionUnsupported stops with ErrUnsupported (the hub has no
	// conduit endpoint).
	ActionUnsupported
)

// Classify maps how an attempt or session ended to the dialer's action:
//
//	4409 superseded launch, 4403, 4404, HTTP 403  stop (terminal)
//	HTTP 404                                      stop: hub has no conduit
//	4401, HTTP 401                                refresh credential, redial
//	4400                                          redial after BackoffMax
//	4503                                          redial within the GoAway window
//	4504, 1011, anything else                     exponential backoff, jitter
func Classify(end core.End) Action {
	var de *ws.DialError
	if errors.As(end.DialErr, &de) {
		switch de.StatusCode {
		case http.StatusNotFound:
			return ActionUnsupported
		case http.StatusUnauthorized:
			return ActionRefreshCredential
		case http.StatusForbidden:
			return ActionStop
		}
		return ActionBackoff
	}
	switch end.Code() {
	case closeSuperseded, core.CloseForbidden, closeTargetNotFound:
		return ActionStop
	case core.CloseUnauthenticated:
		return ActionRefreshCredential
	case core.CloseProtocolError:
		return ActionMaxBackoff
	}
	return ActionBackoff
}

// decide implements Reconnector.Decide.
func (a *Agent) decide(ctx context.Context, end core.End, delay time.Duration) (time.Duration, error) {
	cause := endCause(end)
	switch Classify(end) {
	case ActionUnsupported:
		log.Info("Conduit: hub has no conduit endpoint (HTTP 404)")
		return 0, ErrUnsupported
	case ActionStop:
		te := terminalError(end)
		log.Error("Conduit: %v; not reconnecting", te)
		return 0, te
	case ActionMaxBackoff:
		log.Error("Conduit: protocol error (%v); reconnecting in %v", cause, core.BackoffMax)
		return core.BackoffMax, nil
	case ActionRefreshCredential:
		a.refreshCredential(ctx, cause)
	}
	if end.DialErr != nil {
		log.Warn("Conduit: connect failed (%v); retrying in %v", cause, delay)
	} else {
		log.Info("Conduit session ended (%v); reconnecting in %v", cause, delay)
	}
	return delay, nil
}

// refreshCredential refreshes the agent token, at most once per
// credentialRefreshMinInterval.
func (a *Agent) refreshCredential(ctx context.Context, cause string) {
	if a.opts.RefreshCredential == nil {
		return
	}
	a.mu.Lock()
	now := a.clk.Now()
	due := a.lastCredRefsh.IsZero() || now.Sub(a.lastCredRefsh) >= credentialRefreshMinInterval
	if due {
		a.lastCredRefsh = now
	}
	a.mu.Unlock()
	if !due {
		return
	}
	log.Info("Conduit: credential refused (%v); refreshing it", cause)
	if err := a.opts.RefreshCredential(ctx); err != nil {
		log.Warn("Conduit: credential refresh failed: %v", err)
	}
}

func endCause(end core.End) string {
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

func terminalError(end core.End) *TerminalError {
	var de *ws.DialError
	if errors.As(end.DialErr, &de) {
		return &TerminalError{Status: de.StatusCode, Reason: de.Error()}
	}
	te := &TerminalError{Code: end.Code()}
	var ce *core.CloseError
	switch {
	case end.GoAway != nil:
		te.Reason = end.GoAway.GetReason()
	case errors.As(end.DialErr, &ce), errors.As(end.SessionErr, &ce):
		te.Reason = ce.Reason
	}
	return te
}
