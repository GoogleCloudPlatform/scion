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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// User tunnels over a conduit session (design §3.8 "Mechanics" and "Many
// agents from one workstation", Phase 5.1).
//
// A CLI dials GET /api/v1/conduit as a user principal (the same Bearer
// credential as attach) and asks for each tunnel with one RPC on that
// session:
//
//	POST /v1/tunnels {"agent": "<agent id>", "kind": "tcp|pty|ssh", "params": {...}}
//
// The hub authorizes the request against the named agent, resolves the
// agent's conduit session (local or on another relay), opens the target
// stream with a grant, then opens a stream toward the user session (even
// stream id) and copies framed data between the two. The response names
// that stream: {"streamId": N, ...}. The user stream is opened before the
// response is sent, so the CLI accepts relay-opened streams as they arrive
// and matches them by id.
//
// One user session carries any number of tunnels to any mix of agents.
// Tunnel state is keyed by (session id, stream id). Each tunnel is
// authorized on its own, gets its own grant and its own authorization
// deadline and re-check (§3.5), and closing it closes that tunnel only.
// Only the session's close or loss ends every tunnel on it.

// conduitTunnelsPath is the RPC path of a tunnel request.
const conduitTunnelsPath = "/v1/tunnels"

// conduitTunnelStreamCap is the most tunnels (open or opening) one user
// session may hold. It matches the dialer's default limit on the streams
// its peer may open (conduit.DefaultMaxConcurrentStreams), so the hub never
// opens a stream the CLI would refuse. A request beyond it gets an RPC 429.
var conduitTunnelStreamCap atomic.Int64

func init() { conduitTunnelStreamCap.Store(conduit.DefaultMaxConcurrentStreams) }

// conduitTunnelRequest is the body of POST /v1/tunnels.
type conduitTunnelRequest struct {
	// Agent is the agent id.
	Agent string `json:"agent"`
	// Kind is tcp, pty or ssh.
	Kind string `json:"kind"`
	// Params are the kind's params, as strings: tcp {port} (host may be
	// given and must be 127.0.0.1); pty {cols, rows} (default 80x24;
	// session may be given and must be "scion"); ssh none.
	Params map[string]string `json:"params,omitempty"`
}

// conduitTunnelResponse is the body of a successful POST /v1/tunnels.
type conduitTunnelResponse struct {
	// StreamID is the id of the stream the hub opened toward this
	// session for the tunnel.
	StreamID  uint32 `json:"streamId"`
	SessionID string `json:"sessionId"`
	AgentID   string `json:"agentId"`
	ProjectID string `json:"projectId"`
	Kind      string `json:"kind"`
	// AuthzDeadline is the tunnel's authorization deadline (§3.5), when
	// it has one.
	AuthzDeadline *time.Time `json:"authzDeadline,omitempty"`
}

// conduitTunnelFailure is a refusal of POST /v1/tunnels. Each one maps to
// exactly one status and error code, in conduitTunnelFailures.
type conduitTunnelFailure int

const (
	tunnelInvalidRequest conduitTunnelFailure = iota
	tunnelUnauthorized
	tunnelForbidden
	tunnelAgentNotFound
	tunnelAgentNotRunning
	tunnelRateLimited
	tunnelBrokerUnavailable
	tunnelSessionUnavailable
	tunnelInternal
)

// conduitTunnelFailures is the one mapping from a tunnel refusal to its
// RPC status and error code (design §3.8 rule 7). 404 goes only with
// agent_not_found.
var conduitTunnelFailures = map[conduitTunnelFailure]struct {
	status int
	code   string
}{
	tunnelInvalidRequest:     {http.StatusBadRequest, ErrCodeInvalidRequest},
	tunnelUnauthorized:       {http.StatusUnauthorized, ErrCodeUnauthorized},
	tunnelForbidden:          {http.StatusForbidden, ErrCodeForbidden},
	tunnelAgentNotFound:      {http.StatusNotFound, ErrCodeAgentNotFound},
	tunnelAgentNotRunning:    {http.StatusConflict, ErrCodeAgentNotRunning},
	tunnelRateLimited:        {http.StatusTooManyRequests, ErrCodeRateLimited},
	tunnelBrokerUnavailable:  {http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail},
	tunnelSessionUnavailable: {http.StatusServiceUnavailable, ErrCodeAgentSessionUnavailable},
	tunnelInternal:           {http.StatusInternalServerError, ErrCodeInternalError},
}

// conduitTunnelError is a refusal with its message for the client.
type conduitTunnelError struct {
	failure conduitTunnelFailure
	message string
}

func (e *conduitTunnelError) Error() string { return e.message }

func tunnelRefusal(f conduitTunnelFailure, format string, args ...any) *conduitTunnelError {
	return &conduitTunnelError{failure: f, message: fmt.Sprintf(format, args...)}
}

// conduitRPCJSON builds an RPC response with a JSON body.
func conduitRPCJSON(status int, body any) *conduitv1.RpcResponse {
	data, err := json.Marshal(body)
	if err != nil {
		return &conduitv1.RpcResponse{Status: http.StatusInternalServerError}
	}
	return &conduitv1.RpcResponse{
		Status:  int32(status),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    data,
	}
}

// conduitRPCError builds an RPC error response in the hub's error format.
func conduitRPCError(status int, code, message string) *conduitv1.RpcResponse {
	return conduitRPCJSON(status, ErrorResponse{Error: APIError{Code: code, Message: message}})
}

// response renders the refusal through conduitTunnelFailures.
func (e *conduitTunnelError) response() *conduitv1.RpcResponse {
	m, ok := conduitTunnelFailures[e.failure]
	if !ok {
		m = conduitTunnelFailures[tunnelInternal]
	}
	return conduitRPCError(m.status, m.code, e.message)
}

// conduitDrainingResponse is the answer to a tunnel request on a draining
// session. It is the same bare 503 the session layer sends for any RPC that
// reaches a draining session, so the client sees one answer however the
// request raced the GoAway.
func conduitDrainingResponse() *conduitv1.RpcResponse {
	return &conduitv1.RpcResponse{
		Status:  http.StatusServiceUnavailable,
		Headers: map[string]string{conduit.RetryAfterHeader: "0"},
		Body:    []byte(conduit.ErrDraining.Error()),
	}
}

// conduitTunnelKey identifies a tunnel: its user session and the stream
// the hub opened toward that session.
type conduitTunnelKey struct {
	SessionID string
	StreamID  uint32
}

// conduitTunnelSession is the tunnel state of one user conduit session.
// It is created when the session is admitted at the HTTP layer and serves
// the session's RPCs.
type conduitTunnelSession struct {
	s        *Server
	identity UserIdentity
	// credExpiry is when the credential that admitted the session expires
	// (zero: it has no expiry). New tunnels are refused after it; open
	// tunnels run under their own deadlines (§3.8 rule 6).
	credExpiry time.Time

	mu sync.Mutex
	// pending counts tunnel requests holding a slot that are not yet in
	// tunnels.
	pending int
	tunnels map[conduitTunnelKey]*conduitTunnel
}

func newConduitTunnelSession(s *Server, identity UserIdentity, credExpiry time.Time) *conduitTunnelSession {
	return &conduitTunnelSession{s: s, identity: identity, credExpiry: credExpiry, tunnels: map[conduitTunnelKey]*conduitTunnel{}}
}

// HandleRPC implements conduit.RPCHandler. Only POST /v1/tunnels is
// served; any other path is a bare 404 (never agent_not_found).
func (ts *conduitTunnelSession) HandleRPC(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	if req.GetPath() != conduitTunnelsPath {
		return conduitRPCError(http.StatusNotFound, ErrCodeNotFound, "unknown method")
	}
	if req.GetMethod() != http.MethodPost {
		allow := map[string]string{"Allow": http.MethodPost}
		resp := conduitRPCError(http.StatusMethodNotAllowed, ErrCodeInvalidRequest, "method not allowed")
		for k, v := range allow {
			resp.Headers[k] = v
		}
		return resp
	}
	ls := conduit.LocalSessionFromContext(ctx)
	if ls == nil {
		return tunnelRefusal(tunnelInternal, "no conduit session").response()
	}
	resp, err := ts.open(ctx, ls, req.GetBody())
	if err != nil {
		var te *conduitTunnelError
		if errors.As(err, &te) {
			return te.response()
		}
		if errors.Is(err, conduit.ErrDraining) {
			return conduitDrainingResponse()
		}
		slog.Warn("Conduit tunnel request failed", "user_id", ts.identity.ID(), "session_id", ls.Info().SessionID, "error", err)
		return tunnelRefusal(tunnelInternal, "tunnel request failed").response()
	}
	return conduitRPCJSON(http.StatusOK, resp)
}

// now is the hub's conduit clock (the grant key set's, the clock grants
// are minted on).
func (ts *conduitTunnelSession) now() time.Time {
	return ts.s.conduitGrantKeySet().now()
}

// reserve takes a tunnel slot on the session, or refuses with 429.
func (ts *conduitTunnelSession) reserve() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if int64(ts.pending+len(ts.tunnels)) >= conduitTunnelStreamCap.Load() {
		return tunnelRefusal(tunnelRateLimited, "too many tunnels on this session")
	}
	ts.pending++
	return nil
}

// commit moves a reserved slot to the open tunnel t (t == nil releases it).
func (ts *conduitTunnelSession) commit(t *conduitTunnel) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.pending--
	if t != nil {
		ts.tunnels[t.key] = t
	}
}

// remove forgets an ended tunnel.
func (ts *conduitTunnelSession) remove(key conduitTunnelKey) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.tunnels, key)
}

// open serves one tunnel request. Checks run in the order the agent routes
// use: the credential, the request shape, the session's cap, the agent's
// existence (404), authorization (403), and only then the agent's state
// (409), its broker (503) and its session (503), so a caller who may not
// use the agent learns nothing about it.
func (ts *conduitTunnelSession) open(ctx context.Context, ls conduit.LocalSession, body []byte) (*conduitTunnelResponse, error) {
	info := ls.Info()
	log := slog.Default().With("subsystem", "hub.conduit", "user_id", ts.identity.ID(), "session_id", info.SessionID)
	if !ts.credExpiry.IsZero() && !ts.now().Before(ts.credExpiry) {
		log.Info("Conduit tunnel refused", "cause", "credential_expired")
		return nil, tunnelRefusal(tunnelUnauthorized, "credential refused")
	}
	if info.Draining {
		return nil, conduit.ErrDraining
	}
	req, kind, params, err := parseConduitTunnelRequest(body)
	if err != nil {
		return nil, err
	}
	if err := ts.reserve(); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			ts.commit(nil)
		}
	}()

	s := ts.s
	agent, err := s.store.GetAgent(ctx, req.Agent)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, tunnelRefusal(tunnelAgentNotFound, "agent not found")
	case err != nil:
		return nil, fmt.Errorf("agent lookup: %w", err)
	case !agent.DeletedAt.IsZero():
		return nil, tunnelRefusal(tunnelAgentNotFound, "agent not found")
	}
	log = log.With("agent_id", agent.ID, "kind", kind)

	port, err := s.authorizeConduitTunnel(ctx, ts.identity, agent, kind, params)
	if err != nil {
		log.Info("Conduit tunnel refused", "cause", "forbidden", "error", err)
		return nil, tunnelRefusal(tunnelForbidden, "not permitted")
	}
	switch state.Phase(agent.Phase) {
	case state.PhaseStopped, state.PhaseStopping, state.PhaseSuspended:
		return nil, tunnelRefusal(tunnelAgentNotRunning, "agent is not running")
	}
	if s.conduitTunnelBrokerOffline(ctx, agent) {
		return nil, tunnelRefusal(tunnelBrokerUnavailable, "runtime broker not connected")
	}
	rt := s.conduit.Load()
	if rt == nil || rt.router == nil || s.conduitAuthz.Load() == nil {
		// User streams are opened only while their re-check runs.
		return nil, tunnelRefusal(tunnelSessionUnavailable, "agent session unavailable")
	}

	target, err := ts.openTarget(ctx, rt.router, agent, kind, params)
	if err != nil {
		if te := (*conduitTunnelError)(nil); errors.As(err, &te) {
			log.Info("Conduit tunnel refused", "cause", te.message, "error", err)
		}
		return nil, err
	}
	user, err := ls.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduit.StreamKind(kind).Proto(), Params: params})
	if err != nil {
		_ = target.CloseWithCode(conduit.CloseCancelled, "cancelled")
		return nil, err
	}

	t := &conduitTunnel{
		key:    conduitTunnelKey{SessionID: info.SessionID, StreamID: user.ID()},
		kind:   kind,
		user:   user,
		target: target,
	}
	tracked := &conduitUserStream{
		Kind:      kind,
		Identity:  ts.identity,
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		Port:      port,
		SessionID: info.SessionID,
		StreamID:  user.ID(),
		Close:     t.closeBoth,
	}
	t.untrack = s.trackConduitUserStream(tracked)
	t.tracked.Store(tracked)
	resp := &conduitTunnelResponse{
		StreamID:  user.ID(),
		SessionID: info.SessionID,
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		Kind:      kind,
	}
	if d := t.deadline(); !d.IsZero() {
		resp.AuthzDeadline = &d
	}
	// Registered before the splice starts, so the splice's removal
	// always finds it.
	ts.commit(t)
	committed = true
	go t.run(ts)
	log.Info("Conduit tunnel opened", "stream_id", user.ID())
	return resp, nil
}

// parseConduitTunnelRequest validates the request shape and returns the
// stream params the grant and both StreamOpens carry.
func parseConduitTunnelRequest(body []byte) (*conduitTunnelRequest, string, map[string]string, error) {
	var req conduitTunnelRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, "", nil, tunnelRefusal(tunnelInvalidRequest, "invalid request body")
	}
	if req.Agent == "" {
		return nil, "", nil, tunnelRefusal(tunnelInvalidRequest, "agent is required")
	}
	params := map[string]string{}
	for k, v := range req.Params {
		params[k] = v
	}
	switch req.Kind {
	case grant.StreamKindTCP:
		if h, ok := params[grant.ParamHost]; !ok {
			params[grant.ParamHost] = "127.0.0.1"
		} else if h != "127.0.0.1" {
			return nil, "", nil, tunnelRefusal(tunnelInvalidRequest, "tcp host must be 127.0.0.1")
		}
		if _, err := conduitTCPTarget(params, false); err != nil {
			return nil, "", nil, tunnelRefusal(tunnelInvalidRequest, "tcp params must be {port}")
		}
	case grant.StreamKindPTY:
		if _, ok := params[grant.ParamCols]; !ok {
			params[grant.ParamCols] = strconv.Itoa(ptyDefaultCols)
		}
		if _, ok := params[grant.ParamRows]; !ok {
			params[grant.ParamRows] = strconv.Itoa(ptyDefaultRows)
		}
		if _, ok := params[grant.ParamSession]; !ok {
			params[grant.ParamSession] = conduitPTYSession
		}
		if err := conduitPTYTarget(params); err != nil {
			return nil, "", nil, tunnelRefusal(tunnelInvalidRequest, "pty params must be {cols, rows} within 1..%d", conduitPTYMaxDim)
		}
	case grant.StreamKindSSH:
		if len(params) != 0 {
			return nil, "", nil, tunnelRefusal(tunnelInvalidRequest, "ssh takes no params")
		}
		params = nil
	default:
		return nil, "", nil, tunnelRefusal(tunnelInvalidRequest, "unknown tunnel kind")
	}
	return &req, req.Kind, params, nil
}

// authorizeConduitTunnel decides a tunnel request (§3.5): ActionTunnel for
// every request, and the kind's action (ActionPortAccess on an authorized
// agent-local target for tcp, ActionAttach for pty and ssh). It returns the
// tcp port (0 for other kinds). mintConduitGrant repeats these checks when
// it mints; deciding here first keeps a refused caller from learning the
// agent's state or session.
func (s *Server) authorizeConduitTunnel(ctx context.Context, identity UserIdentity, agent *store.Agent, kind string, params map[string]string) (int, error) {
	action := conduitStreamAction(kind)
	if action == "" {
		return 0, fmt.Errorf("%w: unknown stream kind %q", errConduitForbidden, kind)
	}
	if err := s.authorizeConduitAction(ctx, identity, agent, action); err != nil {
		return 0, err
	}
	if err := s.authorizeConduitAction(ctx, identity, agent, ActionTunnel); err != nil {
		return 0, err
	}
	if kind != grant.StreamKindTCP {
		return 0, nil
	}
	port, err := conduitTCPTarget(params, false)
	if err != nil {
		return 0, err
	}
	if err := s.authorizeConduitTCPTarget(agent, port); err != nil {
		return 0, err
	}
	return port, nil
}

// conduitTunnelBrokerOffline reports whether the stored broker row says
// the agent's broker is offline. A missing or unreadable row is not
// treated as offline: the session resolution decides then.
func (s *Server) conduitTunnelBrokerOffline(ctx context.Context, agent *store.Agent) bool {
	if agent.RuntimeBrokerID == "" {
		return false
	}
	b, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || b == nil {
		return false
	}
	return b.Status == store.BrokerStatusOffline
}

// openTarget resolves the agent's session that serves kind, mints a grant
// for exactly that session and opens the target stream with the same
// params, as the agent PTY path does.
func (ts *conduitTunnelSession) openTarget(ctx context.Context, rtr *router.Router, agent *store.Agent, kind string, params map[string]string) (conduit.Stream, error) {
	var st conduit.Stream
	req := router.Request{
		Op:    router.OpStream,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID, Capability: kind},
		Agent: agentIncarnationFacts(agent),
	}
	err := rtr.Do(ctx, req, func(ctx context.Context, res router.Resolved) error {
		tok, _, err := ts.s.mintConduitGrant(ctx, conduitGrantRequest{
			Identity: ts.identity,
			Agent:    agent,
			Stream:   grant.StreamHeader{Kind: kind, Params: params},
			Target: grant.Target{
				Kind:                grant.TargetKindAgent,
				ID:                  agent.ID,
				EndpointIncarnation: res.Want.Incarnation,
				SessionID:           res.Record.SessionID,
				ConnectionEpoch:     res.Record.ConnectionEpoch,
			},
			ViaTunnel: true,
		})
		if err != nil {
			return err
		}
		st, err = res.Session.OpenStream(ctx, &conduitv1.StreamOpen{
			Kind:   conduit.StreamKind(kind).Proto(),
			Params: params,
			Grant:  tok,
		})
		return err
	})
	if err != nil {
		return nil, conduitTunnelOpenError(err)
	}
	return st, nil
}

// conduitTunnelOpenError maps a failed target open to its refusal.
func conduitTunnelOpenError(err error) error {
	var ce *conduit.CloseError
	switch {
	case errors.Is(err, errConduitForbidden):
		return tunnelRefusal(tunnelForbidden, "not permitted")
	case errors.Is(err, errConduitInvalid):
		return tunnelRefusal(tunnelInvalidRequest, "invalid tunnel params")
	case errors.As(err, &ce) && ce.Code == relay.CloseTargetNotFound:
		return tunnelRefusal(tunnelAgentNotFound, "agent not found")
	case errors.As(err, &ce) && ce.Code == conduit.CloseForbidden:
		return tunnelRefusal(tunnelForbidden, "not permitted")
	case errors.Is(err, context.Canceled):
		return err
	default:
		// No session serves the kind, the registry or the owner relay is
		// unreachable, conduit was turned off, or the target refused for
		// another reason.
		slog.Debug("Conduit tunnel target unavailable", "error", err)
		return tunnelRefusal(tunnelSessionUnavailable, "agent session unavailable")
	}
}

// serveConduitUser serves GET /api/v1/conduit for an authenticated user
// (hub.conduit on): the session may only ask the hub for tunnels by RPC.
// The credential's expiry is captured here and bounds new tunnels on the
// session.
func (s *Server) serveConduitUser(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	rt := s.conduit.Load()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "Conduit relay is not running on this hub node (hub.conduit was enabled after startup; restart required)", nil)
		return
	}
	expiry, err := s.conduitCredentialExpiry(r)
	if err != nil {
		slog.Warn("Conduit: reading the admitting credential's expiry failed", "user_id", user.ID(), "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "Unable to read the credential's expiry", nil)
		return
	}
	if !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return
	}
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	ts := newConduitTunnelSession(s, user, expiry)
	p := relay.Principal{
		Kind:       registry.PrincipalUser,
		ID:         user.ID(),
		RPCHandler: ts,
	}
	ctx := context.WithoutCancel(r.Context())
	if err := rt.relay.Serve(ctx, conn, p); err != nil && !errors.Is(err, relay.ErrNotServing) {
		slog.Debug("Conduit user session ended", "user_id", user.ID(), "error", err)
	}
}

// conduitCredentialExpiry returns when the request's credential expires
// (zero: no expiry). The credential was validated by the auth middleware;
// this only reads its expiry: a hub user JWT's exp, a user access token's
// stored expiry, or the exp of an external bearer token.
func (s *Server) conduitCredentialExpiry(r *http.Request) (time.Time, error) {
	authType, _ := r.Context().Value(logging.AuthTypeKey{}).(string)
	switch authType {
	case AuthTypeJWT:
		if s.userTokenService == nil {
			return time.Time{}, errors.New("no user token service")
		}
		return s.userTokenService.GetTokenExpiry(extractBearerToken(r))
	case AuthTypeUAT:
		scoped, ok := GetIdentityFromContext(r.Context()).(*ScopedUserIdentity)
		if !ok || scoped.CredentialID() == "" {
			return time.Time{}, errors.New("access token identity has no credential id")
		}
		tok, err := s.store.GetUserAccessToken(r.Context(), scoped.CredentialID())
		if err != nil {
			return time.Time{}, err
		}
		if tok.ExpiresAt == nil {
			return time.Time{}, nil
		}
		return *tok.ExpiresAt, nil
	case AuthTypeExternalBearer:
		return jwtExpiryUnverified(extractBearerToken(r))
	default:
		return time.Time{}, nil
	}
}

// jwtExpiryUnverified reads the exp claim of an already validated JWT.
func jwtExpiryUnverified(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("bearer token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding token payload: %w", err)
	}
	var claims struct {
		Exp *json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("decoding token claims: %w", err)
	}
	if claims.Exp == nil {
		return time.Time{}, nil
	}
	f, err := claims.Exp.Float64()
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid exp claim: %w", err)
	}
	return time.Unix(int64(f), 0), nil
}
