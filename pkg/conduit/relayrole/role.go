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
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
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

// ServeHTTP serves GET /api/v1/conduit: the relay asks the hub to admit the
// dialer with the credential headers it presented, and serves the session
// as the principal the hub admitted. A hub refusal is passed on unchanged;
// an unreachable hub is 503.
func (ro *Role) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		writeUnavailable(w, "This relay is not admitting conduit sessions", "conduit_not_serving")
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
		Kind:      adm.PrincipalKind,
		ID:        adm.PrincipalID,
		ProjectID: adm.ProjectID,
		ExecScope: adm.ExecScope,
		Agent:     relay.AgentIncarnationFacts{LaunchID: adm.AgentLaunchID, Generation: adm.AgentGeneration},
		SessionID: sessionID,
		GrantKeys: relay.WelcomeGrantKeys(adm.GrantKeys),
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
	var out relay.HubRPCResponse
	err = f.ro.cfg.Hub.Call(ctx, relay.HubSessionPath(f.sessionID, "rpc"), relay.HubRPCRequest{AdmissionToken: f.ro.token(f.sessionID), RPC: b}, &out)
	var refusal *relay.HubRefusal
	switch {
	case err == nil:
		resp := &conduitv1.RpcResponse{}
		if err := proto.Unmarshal(out.RPC, resp); err != nil {
			return fail(http.StatusBadGateway, "unavailable", "malformed hub answer")
		}
		return resp
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
