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

// Package relayrole is a separate conduit relay process (scion server
// --role relay; design §3.9 "Separate relay"). The relay is policy-free:
// the hub admits every dialer (Admit), answers every RPC a principal sends
// on a relay-held session, checks every in-band credential refresh, and
// re-checks sessions and streams the relay schedules. The relay holds the
// sessions, splices streams and keeps each session's hub admission token,
// which it stores and presents unchanged and never parses or logs.
package relayrole

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// ConduitPath is the public path dialers open conduit sessions on.
const ConduitPath = "/api/v1/conduit"

// DefaultRecheckInterval is the default period of the session and stream
// re-check sweeps (conduit.authz_recheck_interval).
const DefaultRecheckInterval = 60 * time.Second

// Events delivers the hub's revocation events to the relay: Subscribe
// returns the subjects of matching events; AddOnListen runs fn each time
// the event connection is (re)established (events may have been missed).
type Events interface {
	Subscribe(subjects ...string) (events <-chan string, unsubscribe func())
	AddOnListen(fn func()) (remove func())
}

// Config configures a Role.
type Config struct {
	// Hub calls the hub internal API. Required.
	Hub *relay.HubClient
	// Clock drives the re-check timers (default: real time).
	Clock clock.Clock
	// RecheckInterval is the session and stream re-check sweep period
	// (default DefaultRecheckInterval).
	RecheckInterval time.Duration
	// Events, when set, triggers session and stream re-checks on the
	// hub's revocation events (Subjects) and on reconnects.
	Events   Events
	Subjects []string
	// NewSessionID generates session ids (default uuid v4).
	NewSessionID func() string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Role is a separate relay's policy-free front: it admits dialers through
// the hub and forwards their sessions' hub calls.
type Role struct {
	cfg   Config
	log   *slog.Logger
	clk   clock.Clock
	relay *relay.Relay

	mu       sync.Mutex
	sessions map[string]*heldSession

	// recheckMu guards the coalescing of re-check runs: recheckRunning is
	// set while one runs, and recheckPending holds the trigger of the one
	// run queued behind it ("" when none).
	recheckMu      sync.Mutex
	recheckRunning bool
	recheckPending string

	// Streams schedules the re-checks of the user streams this relay holds.
	Streams *StreamScheduler

	// untrustedIngress counts dials whose ingress headers were not trusted.
	untrustedIngress atomic.Uint64
	// invalidIngressCap counts trusted ingress dials whose lifetime cap
	// header was absent or invalid (cause ingress_cap_invalid).
	invalidIngressCap atomic.Uint64

	// tunnels are the tunnels this relay opened for its user sessions.
	tunnels tunnelTable
	// testHookTunnelEnded, when set (tests only, before any tunnel opens),
	// runs after a tunnel's splice has ended and the tunnel is untracked
	// and forgotten.
	testHookTunnelEnded func()
}

// heldSession is one relay-held session's hub admission.
type heldSession struct {
	kind  string
	id    string
	token string
}

// New returns a Role. Bind it to its relay before serving.
func New(cfg Config) (*Role, error) {
	if cfg.Hub == nil {
		return nil, errors.New("conduit relay role: a hub client is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.RecheckInterval <= 0 {
		cfg.RecheckInterval = DefaultRecheckInterval
	}
	if cfg.NewSessionID == nil {
		cfg.NewSessionID = uuid.NewString
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ro := &Role{cfg: cfg, log: cfg.Logger.With("component", "conduit-relay-role"), clk: cfg.Clock, sessions: map[string]*heldSession{}}
	ro.Streams = newStreamScheduler(ro)
	return ro, nil
}

// Bind sets the relay the role fronts (relay.New takes the role's Refresh,
// so the relay is built after the role).
func (ro *Role) Bind(r *relay.Relay) { ro.relay = r }

// token returns the admission token of a held session ("" when unknown).
func (ro *Role) token(sessionID string) string {
	ro.mu.Lock()
	defer ro.mu.Unlock()
	if h := ro.sessions[sessionID]; h != nil {
		return h.token
	}
	return ""
}

func (ro *Role) setToken(sessionID, token string) {
	if token == "" {
		return
	}
	ro.mu.Lock()
	defer ro.mu.Unlock()
	if h := ro.sessions[sessionID]; h != nil {
		h.token = token
	}
}

// closeSession closes a held session with code.
func (ro *Role) closeSession(sessionID string, code uint32, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ls, _, ok := ro.relay.Local(ctx, sessionID); ok {
		_ = ls.CloseWithCode(code, reason)
	}
}

// writeUnavailable answers a dialer the relay cannot admit right now: 503
// unavailable with reason (never 401 or 403).
func writeUnavailable(w http.ResponseWriter, message, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": "unavailable", "message": message, "details": map[string]string{"reason": reason},
	}})
}

// ServeHTTP serves GET /api/v1/conduit on the relay's public listener: a
// direct dial. The ingress headers are never trusted here: a dial that
// carries either is logged and counted (cause ingress_header_untrusted)
// and admitted as direct.
func (ro *Role) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(relay.HeaderIngressInstance) != "" || r.Header.Get(relay.HeaderIngressLifetimeCap) != "" {
		ro.ingressHeaderUntrusted("public listener")
	}
	ro.serve(w, r, relay.ArrivalDirect, "", 0)
}

// ServeIngress serves GET /api/v1/conduit proxied by a hub ingress over a
// relay-peer leg (the internal listener). peer is the relay-peer id the
// caller already authenticated as (relay.Relay.AuthenticatePeer). The dial
// is an ingress arrival only when the ingress instance header names that
// same peer; otherwise it is refused with 403 (cause
// ingress_header_untrusted), never served as direct. Only then is the
// ingress lifetime cap header read: a valid value lowers the session's
// lifetime cap (never raises it); an absent or invalid one leaves the
// relay's own cap (cause ingress_cap_invalid). The header is not covered
// by the relay-peer MAC, and needs not be, since it can only shorten a
// session (design §3.9, v2.21.53).
func (ro *Role) ServeIngress(w http.ResponseWriter, r *http.Request, peer string) {
	instance := r.Header.Get(relay.HeaderIngressInstance)
	if instance == "" || len(instance) > relay.MaxIngressInstanceLen || instance != peer {
		ro.ingressHeaderUntrusted("ingress leg")
		http.Error(w, "ingress instance missing or not the authenticated peer", http.StatusForbidden)
		return
	}
	lifetimeCap, ok := ingressLifetimeCap(r.Header.Get(relay.HeaderIngressLifetimeCap))
	if !ok {
		n := ro.invalidIngressCap.Add(1)
		ro.log.Warn("Conduit relay: ingress lifetime cap header absent or invalid; using this relay's cap",
			"cause", "ingress_cap_invalid", "ingress_instance", instance, "count", n)
	}
	ro.serve(w, r, relay.ArrivalIngress, instance, lifetimeCap)
}

// ingressLifetimeCap parses the ingress lifetime cap header: a decimal
// count of whole seconds (ASCII digits only, no sign) within
// config.ConduitMinLifetimeCap and config.ConduitMaxLifetimeCap. It
// reports false, with 0, for an absent or invalid value.
func ingressLifetimeCap(v string) (time.Duration, bool) {
	if v == "" || len(v) > 5 {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	d := time.Duration(secs) * time.Second
	if d < config.ConduitMinLifetimeCap || d > config.ConduitMaxLifetimeCap {
		return 0, false
	}
	return d, true
}

// ingressHeaderUntrusted logs and counts ingress headers this relay did
// not trust (cause ingress_header_untrusted).
func (ro *Role) ingressHeaderUntrusted(where string) {
	n := ro.untrustedIngress.Add(1)
	ro.log.Warn("Conduit relay: ingress headers not trusted on this dial", "cause", "ingress_header_untrusted", "where", where, "count", n)
}

// IngressHeaderUntrustedCount returns how many dials carried ingress
// headers this relay did not trust.
func (ro *Role) IngressHeaderUntrustedCount() uint64 { return ro.untrustedIngress.Load() }

// IngressCapInvalidCount returns how many trusted ingress dials carried
// an absent or invalid lifetime cap header.
func (ro *Role) IngressCapInvalidCount() uint64 { return ro.invalidIngressCap.Load() }

// serve admits a dial: the relay asks the hub to admit the dialer with the
// credential headers it presented and how the dial arrived, and serves the
// session as the principal the hub admitted, with lifetimeCap (0: the
// relay's own cap) lowering the session's lifetime cap. A hub refusal is passed on
// unchanged; an unreachable hub is 503.
func (ro *Role) serve(w http.ResponseWriter, r *http.Request, arrival, ingressInstance string, lifetimeCap time.Duration) {
	if r.URL.Path != ConduitPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !ro.relay.Admitting() {
		// Always before Admit, so an ingress may re-pick on this reason.
		writeUnavailable(w, "This relay is not admitting conduit sessions", relay.ReasonRelayNotAdmitting)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}
	sessionID := ro.cfg.NewSessionID()
	var adm relay.HubAdmitResponse
	err := ro.cfg.Hub.Call(r.Context(), relay.HubAdmitPath, relay.HubAdmitRequest{
		RelayInstanceID: ro.relay.InstanceID(),
		RelayGeneration: ro.relay.Generation(),
		SessionID:       sessionID,
		Host:            r.Host,
		RawQuery:        r.URL.RawQuery,
		Headers:         relay.ForwardCredentialHeaders(r.Header),
		Arrival:         arrival,
		IngressInstance: ingressInstance,
	}, &adm)
	var refusal *relay.HubRefusal
	switch {
	case errors.As(err, &refusal):
		if ct := refusal.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if www := refusal.Header.Get("WWW-Authenticate"); www != "" {
			w.Header().Set("WWW-Authenticate", www)
		}
		w.WriteHeader(refusal.Status)
		_, _ = w.Write(refusal.Body)
		return
	case err != nil:
		ro.log.Warn("Conduit relay admission unavailable: the hub could not be reached", "error", err)
		writeUnavailable(w, "Conduit admission is temporarily unavailable", relay.ReasonUpstreamUnreachable)
		return
	}
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	p := relay.Principal{
		Kind:            adm.PrincipalKind,
		ID:              adm.PrincipalID,
		ProjectID:       adm.ProjectID,
		ExecScope:       adm.ExecScope,
		Agent:           relay.AgentIncarnationFacts{LaunchID: adm.AgentLaunchID, Generation: adm.AgentGeneration},
		SessionID:       sessionID,
		GrantKeys:       relay.WelcomeGrantKeys(adm.GrantKeys),
		LifetimeCap:     lifetimeCap,
		IngressInstance: ingressInstance,
	}
	if adm.TokenRunID != "" {
		p.TokenRun = &relay.TokenRunBinding{RunID: adm.TokenRunID, Enforce: adm.TokenRunEnforce}
	}
	if p.Kind == registry.PrincipalUser {
		p.RPCHandler = &rpcForwarder{ro: ro, sessionID: sessionID}
	}
	ro.mu.Lock()
	ro.sessions[sessionID] = &heldSession{kind: p.Kind, id: p.ID, token: adm.AdmissionToken}
	ro.mu.Unlock()
	defer func() {
		ro.mu.Lock()
		delete(ro.sessions, sessionID)
		ro.mu.Unlock()
		ro.Streams.forgetSession(sessionID)
	}()
	// The session outlives no request deadline.
	if err := ro.relay.Serve(context.WithoutCancel(r.Context()), conn, p); err != nil && !errors.Is(err, relay.ErrNotServing) {
		ro.log.Debug("Conduit relay session ended", "session_id", sessionID, "principal_kind", p.Kind, "error", err)
	}
}

// rpcForwarder forwards the RPCs a relay-held user session sends to the
// hub, which answers them as the principal its admission token names.
type rpcForwarder struct {
	ro        *Role
	sessionID string
}

// HandleRPC implements conduit.RPCHandler.
func (f *rpcForwarder) HandleRPC(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	fail := func(status int, code, message string) *conduitv1.RpcResponse {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
		return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: int32(status), Body: body}
	}
	b, err := proto.Marshal(req)
	if err != nil {
		return fail(http.StatusBadRequest, "invalid_request", "malformed request")
	}
	// A tunnel the hub authorized is opened here (openTunnel). When its
	// target turns out stale, the request goes back to the hub with that
	// target excluded (or, for an owner relay that could not be reached,
	// every session that relay holds), and the hub re-checks and mints for
	// the session it resolves next.
	isTunnel := req.GetMethod() == http.MethodPost && req.GetPath() == relay.TunnelsRPCPath
	var (
		exclude, excludeRelays []string
		openTunnels            int
	)
	if isTunnel {
		// The hub applies the session's tunnel cap to this count.
		var release func()
		openTunnels, release = f.ro.tunnels.reserve(f.sessionID)
		defer release()
	}
	for attempt := 0; ; attempt++ {
		var out relay.HubRPCResponse
		err = f.ro.cfg.Hub.Call(ctx, relay.HubSessionPath(f.sessionID, "rpc"), relay.HubRPCRequest{
			AdmissionToken: f.ro.token(f.sessionID),
			RPC:            b,
			ExcludeTargets: exclude,
			ExcludeRelays:  excludeRelays,
			OpenTunnels:    openTunnels,
			// Every tunnel this relay opens goes to its stream
			// scheduler (openTunnel), so it lists tunnel_recheck.
			Features: []string{relay.HubFeatureTunnels, relay.HubFeatureTunnelRecheck},
		}, &out)
		var refusal *relay.HubRefusal
		switch {
		case err == nil:
			resp := &conduitv1.RpcResponse{}
			if err := proto.Unmarshal(out.RPC, resp); err != nil {
				return fail(http.StatusBadGateway, "unavailable", "malformed hub answer")
			}
			if out.Tunnel == nil {
				return resp
			}
			if !isTunnel {
				// An instruction to open a tunnel answers only a tunnel
				// request.
				return fail(http.StatusBadGateway, "unavailable", "malformed hub answer")
			}
			opened, err := f.ro.openTunnel(ctx, f.sessionID, resp, out.Tunnel)
			var stale *tunnelStaleError
			if errors.As(err, &stale) {
				if attempt >= MaxTunnelReResolves {
					opened = tunnelUnavailable(out.Tunnel)
				} else {
					if stale.relayUnreachable {
						excludeRelays = append(excludeRelays, out.Tunnel.OwnerInstanceID)
					} else {
						exclude = append(exclude, out.Tunnel.TargetSessionID)
					}
					continue
				}
			}
			opened.RequestId = req.GetRequestId()
			return opened
		case errors.As(err, &refusal):
			if code := closeCodeOf(refusal); code != 0 {
				// The hub no longer authorizes the session.
				go f.ro.closeSession(f.sessionID, code, relay.ReasonUnauthenticated)
			}
			return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: int32(refusal.Status), Body: refusal.Body}
		default:
			return fail(http.StatusServiceUnavailable, "unavailable", "the hub is temporarily unavailable")
		}
	}
}

// closeCodeOf returns the close code a hub refusal carries (0: none).
func closeCodeOf(r *relay.HubRefusal) uint32 {
	v, err := strconv.ParseUint(r.Header.Get(relay.HeaderCloseCode), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}

// Refresh implements relay.RefreshFunc for relay-held sessions: the hub
// checks the refreshed credential for the session's admission. A refusal
// closes the session (4401); a hub that cannot be reached, or cannot
// evaluate, closes nothing (the agent retries on its schedule).
func (ro *Role) Refresh(ctx context.Context, _ relay.Principal, sess relay.RefreshSession, ar *conduitv1.AuthRefresh) error {
	token := ro.token(sess.SessionID)
	if token == "" {
		return conduit.Reject(conduit.CloseUnauthenticated, relay.ReasonUnauthenticated)
	}
	var out relay.HubRefreshResponse
	err := ro.cfg.Hub.Call(ctx, relay.HubSessionPath(sess.SessionID, "refresh"), relay.HubRefreshRequest{AdmissionToken: token, Credential: ar.GetCredential()}, &out)
	var refusal *relay.HubRefusal
	switch {
	case err == nil:
		ro.setToken(sess.SessionID, out.AdmissionToken)
		return nil
	case errors.As(err, &refusal):
		return conduit.Reject(conduit.CloseUnauthenticated, relay.ReasonUnauthenticated)
	default:
		ro.log.Warn("Conduit relay: credential refresh could not be checked; keeping the session", "session_id", sess.SessionID, "error", err)
		return nil
	}
}
