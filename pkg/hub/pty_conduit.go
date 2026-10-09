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
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/gorilla/websocket"
)

// PTY path selection (design §3.7 "PTY (Phase 2)", contracts §2 "PTY path
// selection"). One decision, resolvePTYPath, serves both the preflight
// (GET /pty without upgrade) and the WebSocket open, so a preflight 200
// names the path the attach takes:
//
//   - broker: the broker's control channel, as before conduit. With
//     hub.conduit on it is resolved through the router's owner-only legacy
//     broker adapter; off the owner node that is "not connected", exactly
//     as the IsConnected gate answered before.
//   - agent: the agent's conduit session, when the stored broker row says
//     the agent's runtime has no attach and the agent's session advertises
//     pty. The hub mints a pty grant and opens a PTY stream on the session
//     (local or on another relay); sciontool runs `tmux attach` itself.
//   - none: neither path can serve; the preflight and the open answer 503
//     with a reason, and nothing is upgraded.
//
// The leaf protocol (JSON data/resize/ping/pong, close codes) is the same
// on both paths.

// ptyPath is the path a PTY attach takes.
type ptyPath string

const (
	ptyPathBroker ptyPath = "broker"
	ptyPathAgent  ptyPath = "agent"
	ptyPathNone   ptyPath = "none"
)

// Reasons reported with a PTY path refusal (details.reason).
const (
	ptyReasonBrokerNotConnected  = "broker_not_connected"
	ptyReasonConduitNotServing   = "conduit_not_serving"
	ptyReasonStreamAuthzDown     = "stream_authz_unavailable"
	ptyReasonRegistryUnavailable = "registry_unavailable"
	ptyReasonAgentPTYUnavailable = "agent_pty_unavailable"
)

// Close reasons of an agent-path PTY stream that the hub chooses itself.
const (
	ptyCloseReasonUpstreamUnreachable = "upstream_unreachable"
	ptyCloseReasonForbidden           = "forbidden"
)

// PTY terminal size (contracts §2): the default size, and the smallest
// cols and rows a conduit PTY stream carries. The largest is
// conduitPTYMaxDim and the tmux session is conduitPTYSession, the values
// the grant check enforces.
const (
	ptyDefaultCols = 80
	ptyDefaultRows = 24
	ptyMinDim      = 1
)

// ptyPathDecision is resolvePTYPath's answer.
type ptyPathDecision struct {
	Path ptyPath
	// Status, Code, Message and Reason describe a refusal (Path none).
	Status  int
	Code    string
	Message string
	Reason  string
	// reportPath: the preflight names the path in its body (hub.conduit
	// on). With the experiment off the preflight is unchanged.
	reportPath bool
}

func (d ptyPathDecision) details() map[string]interface{} {
	if d.Reason == "" {
		return nil
	}
	return map[string]interface{}{"reason": d.Reason, "path": string(ptyPathNone)}
}

func ptyRefusal(status int, code, message, reason string) ptyPathDecision {
	return ptyPathDecision{Path: ptyPathNone, Status: status, Code: code, Message: message, Reason: reason}
}

// ptyPreflightResponse is the body of a preflight 200 with hub.conduit on.
type ptyPreflightResponse struct {
	Path string `json:"path"`
}

// writePTYPreflightOK answers a preflight whose attach can proceed.
func writePTYPreflightOK(w http.ResponseWriter, d ptyPathDecision) {
	if !d.reportPath {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, ptyPreflightResponse{Path: string(d.Path)})
}

// resolvePTYPath decides which path an attach to agent takes, for both
// the preflight and the WebSocket open. The caller has already
// authenticated identity and authorized it to attach.
//
// A missing runtime broker is 422, as before. With hub.conduit off the
// broker path is the only one (today's behaviour). With it on, the agent
// path is taken only when the stored broker row says the agent's runtime
// has no attach (brokerAttachUnsupported) and the agent has a session that
// advertises pty; a broker that supports attach keeps the broker path,
// and is 503 when it is not connected to this node.
func (s *Server) resolvePTYPath(ctx context.Context, identity Identity, agent *store.Agent) ptyPathDecision {
	if agent.RuntimeBrokerID == "" {
		return ptyRefusal(http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker,
			"Agent has no runtime broker", "")
	}
	if !s.experimentEnabled(conduitExperiment) {
		return s.brokerPTYPath(ctx, agent, false)
	}
	if !s.brokerAttachUnsupported(ctx, agent) {
		d := s.brokerPTYPath(ctx, agent, true)
		d.reportPath = true
		return d
	}
	d := s.agentPTYPath(ctx, identity, agent)
	d.reportPath = true
	return d
}

// brokerPTYPath is the broker path when the broker's control channel is
// held by this node, else a 503. With viaRouter it asks the router's
// legacy broker adapter (owner-only: router.ErrNoSession off the owner);
// otherwise the control channel directly. Both answer the same question.
func (s *Server) brokerPTYPath(ctx context.Context, agent *store.Agent, viaRouter bool) ptyPathDecision {
	connected := false
	if rt := s.conduit.Load(); viaRouter && rt != nil && rt.router != nil {
		_, err := rt.router.Resolve(ctx, brokerPTYRequest(agent), nil)
		connected = err == nil
	} else {
		connected = s.controlChannel != nil && s.controlChannel.IsConnected(agent.RuntimeBrokerID)
	}
	if !connected {
		// The reason is reported only with hub.conduit on (viaRouter);
		// the experiment-off response is unchanged.
		reason := ""
		if viaRouter {
			reason = ptyReasonBrokerNotConnected
		}
		return ptyRefusal(http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
			"Runtime broker not connected", reason)
	}
	return ptyPathDecision{Path: ptyPathBroker}
}

// agentPTYPath is the agent path when the agent has a conduit session
// advertising pty that this node can reach, else a 503 with the reason.
func (s *Server) agentPTYPath(ctx context.Context, identity Identity, agent *store.Agent) ptyPathDecision {
	rt := s.conduit.Load()
	if rt == nil || rt.router == nil {
		return ptyRefusal(http.StatusServiceUnavailable, ErrCodeUnavailable,
			"The agent's runtime does not support attach, and conduit is not serving on this hub node", ptyReasonConduitNotServing)
	}
	if !s.ptyStreamTrackable(identity) {
		return ptyRefusal(http.StatusServiceUnavailable, ErrCodeUnavailable,
			"The agent's runtime does not support attach, and stream authorization checks are not running", ptyReasonStreamAuthzDown)
	}
	_, err := rt.router.Resolve(ctx, agentPTYRequest(agent), nil)
	switch {
	case err == nil:
		return ptyPathDecision{Path: ptyPathAgent}
	case errors.Is(err, router.ErrRegistryUnavailable):
		return ptyRefusal(http.StatusServiceUnavailable, ErrCodeUnavailable,
			"Conduit routing is unavailable", ptyReasonRegistryUnavailable)
	default:
		return ptyRefusal(http.StatusServiceUnavailable, wsprotocol.ErrCodeRuntimeAttachUnsupported,
			"The agent's runtime does not support attach, and the agent has no conduit session that serves PTY",
			ptyReasonAgentPTYUnavailable)
	}
}

// ptyStreamTrackable reports whether a PTY stream opened for identity can
// be re-checked for its lifetime: a user stream is opened only while the
// stream re-check runs (as for port streams).
func (s *Server) ptyStreamTrackable(identity Identity) bool {
	if _, isUser := identity.(UserIdentity); isUser {
		return s.conduitAuthz.Load() != nil
	}
	return true
}

// brokerAttachUnsupported reports whether the stored broker row says the
// agent's runtime has no interactive attach (contracts §2 "PTY path
// selection"). The row is kept current by the broker heartbeat; there is
// no live broker round-trip.
//
// The agent's profile is its AppliedConfig.Profile, else the broker's
// DefaultProfile, else the broker's only profile. Only an explicit
// BrokerProfile.Attach == false is unsupported; nil (unknown, or a broker
// that predates the field) is supported, as runtime.HasAttachSupport
// treats a missing capability. Capabilities.Attach describes the broker's
// default runtime only, so it decides only when no profile resolves or the
// resolved profile is the default one and reports nothing itself. An
// unreadable broker row is supported, and logged: the broker path is
// today's behaviour, and the broker refuses an unsupported attach with
// 4501, which reaches the client unchanged.
func (s *Server) brokerAttachUnsupported(ctx context.Context, agent *store.Agent) bool {
	b, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || b == nil {
		class := "read_error"
		if (b == nil && err == nil) || errors.Is(err, store.ErrNotFound) {
			class = "not_found"
		}
		slog.Warn("PTY path: broker row unreadable, treating attach as supported",
			"agent_id", agent.ID, "broker_id", agent.RuntimeBrokerID,
			"attach_source", ptyAttachSourceUnreadableRow, "error_class", class)
		return false
	}
	return brokerRowAttachUnsupported(b, agent)
}

// ptyAttachSourceUnreadableRow is the attach_source logged when the
// broker row could not be read and attach is assumed supported.
const ptyAttachSourceUnreadableRow = "unreadable_row"

// brokerRowAttachUnsupported is brokerAttachUnsupported's rule over a
// broker row.
func brokerRowAttachUnsupported(b *store.RuntimeBroker, agent *store.Agent) bool {
	name := ""
	if agent.AppliedConfig != nil {
		name = agent.AppliedConfig.Profile
	}
	p, isDefault := brokerProfileFor(b, name)
	if p != nil && p.Attach != nil {
		return !*p.Attach
	}
	if p == nil && name != "" {
		// The agent names a profile the broker has not registered: its
		// runtime is not the default one, and nothing is known about it.
		return false
	}
	if p != nil && !isDefault {
		return false
	}
	return b.Capabilities != nil && !b.Capabilities.Attach
}

// brokerProfileFor returns the broker profile an agent with profile name
// runs on (name, else the broker's DefaultProfile, else the only profile),
// and whether that profile is the broker's default.
func brokerProfileFor(b *store.RuntimeBroker, name string) (*store.BrokerProfile, bool) {
	find := func(n string) *store.BrokerProfile {
		for i := range b.Profiles {
			if b.Profiles[i].Name == n {
				return &b.Profiles[i]
			}
		}
		return nil
	}
	if name != "" {
		p := find(name)
		if p == nil {
			return nil, false
		}
		isDefault := name == b.DefaultProfile || (b.DefaultProfile == "" && len(b.Profiles) == 1)
		return p, isDefault
	}
	if b.DefaultProfile != "" {
		if p := find(b.DefaultProfile); p != nil {
			return p, true
		}
	}
	if len(b.Profiles) == 1 {
		return &b.Profiles[0], b.DefaultProfile == ""
	}
	return nil, false
}

// brokerPTYRequest is the router request for the agent's broker.
func brokerPTYRequest(agent *store.Agent) router.Request {
	return router.Request{Op: router.OpStream, Kind: registry.PrincipalBroker, ID: agent.RuntimeBrokerID}
}

// agentPTYRequest is the router request for the agent's PTY-capable
// session.
func agentPTYRequest(agent *store.Agent) router.Request {
	return router.Request{
		Op:    router.OpStream,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID, Capability: grant.StreamKindPTY},
		Agent: agentIncarnationFacts(agent),
	}
}

// brokerPTYOpener returns how the broker path opens its stream: with
// hub.conduit serving, on the control channel the router's legacy broker
// adapter resolves (bound to that connection, re-resolved within the
// router's budget if it is replaced); otherwise nil, the broker's current
// control channel. Both open the same control channel stream.
func (s *Server) brokerPTYOpener(agent *store.Agent) func(ctx context.Context, cols, rows int) (*StreamProxy, error) {
	rt := s.conduit.Load()
	if rt == nil || rt.router == nil || !s.experimentEnabled(conduitExperiment) {
		return nil
	}
	return func(ctx context.Context, cols, rows int) (*StreamProxy, error) {
		var st *StreamProxy
		err := rt.router.Do(ctx, brokerPTYRequest(agent), func(ctx context.Context, res router.Resolved) error {
			ls, ok := res.Legacy.(*legacyBrokerSession)
			if !ok {
				return errors.New("pty: broker resolution is not a control channel")
			}
			var err error
			st, err = ls.OpenStream(ctx, wsprotocol.StreamTypePTY, agent.Slug, agent.ProjectID, cols, rows)
			return err
		})
		return st, err
	}
}

// ptyInitialSize reads the initial terminal size of an agent-path attach
// from the cols and rows query params: the default 80x24 for a missing or
// unparsable value, clamped into 1..4096 otherwise, so the grant and the
// StreamOpen always carry a valid size.
func ptyInitialSize(q url.Values) (cols, rows int) {
	dim := func(raw string, def int) int {
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return def
		}
		return min(max(n, ptyMinDim), conduitPTYMaxDim)
	}
	return dim(q.Get("cols"), ptyDefaultCols), dim(q.Get("rows"), ptyDefaultRows)
}

// ptyStreamParams is the params map of a conduit PTY stream: minted into
// the grant and sent in the StreamOpen unchanged (contracts §2).
func ptyStreamParams(cols, rows int) map[string]string {
	return map[string]string{
		grant.ParamCols:    strconv.Itoa(cols),
		grant.ParamRows:    strconv.Itoa(rows),
		grant.ParamSession: conduitPTYSession,
	}
}

// runAgentPTY runs an agent-path attach on the upgraded client conn.
func (s *Server) runAgentPTY(ctx context.Context, conn *websocket.Conn, identity Identity, agent *store.Agent, q url.Values) {
	cols, rows := ptyInitialSize(q)
	up := &agentPTYUpstream{s: s, identity: identity, agent: agent}
	session := newPTYSessionWithUpstream(ctx, agent.Slug, conn, up, cols, rows)
	defer session.Close()

	slog.Info("PTY session started", "agent_id", agent.ID, "slug", agent.Slug, "user", identity.ID(), "path", string(ptyPathAgent))
	if err := session.Run(); err != nil && !isExpectedPTYEnd(err) {
		slog.Error("PTY session error", "agent_id", agent.ID, "slug", agent.Slug, "path", string(ptyPathAgent), "error", err)
	}
	code, reason := session.CloseCause()
	slog.Info("PTY session ended", "agent_id", agent.ID, "slug", agent.Slug, "path", string(ptyPathAgent),
		"conduit_session_id", up.sessionID, "close_code", code, "close_reason", reason)
}

// ptyAgentReadBuffer is the size of one terminal output read from a
// conduit PTY stream.
const ptyAgentReadBuffer = 32 << 10

// agentPTYUpstream is a PTY stream on the agent's conduit session. The
// stream is registered with the user-stream re-check for its lifetime; a
// re-check that ends its authorization closes both legs, and the client
// receives that close code (4401 authz_expired, 4404 target_not_found).
type agentPTYUpstream struct {
	s        *Server
	identity Identity
	agent    *store.Agent

	st        conduit.Stream
	sessionID string
	untrack   func()
	buf       []byte
	// authzClose is set when the re-check closed the stream.
	authzClose atomic.Pointer[StreamClosedError]
}

// Open resolves the agent's PTY-capable session, mints a pty grant for
// exactly that session, opens the stream with the same params, and
// registers it for re-checks.
func (u *agentPTYUpstream) Open(ctx context.Context, cols, rows int) error {
	rt := u.s.conduit.Load()
	if rt == nil || rt.router == nil {
		return errConduitNoRoute
	}
	if !u.s.ptyStreamTrackable(u.identity) {
		return errConduitNoRoute
	}
	params := ptyStreamParams(cols, rows)
	err := rt.router.Do(ctx, agentPTYRequest(u.agent), func(ctx context.Context, res router.Resolved) error {
		tok, _, err := u.s.mintConduitGrant(ctx, conduitGrantRequest{
			Identity: u.identity,
			Agent:    u.agent,
			Stream:   grant.StreamHeader{Kind: grant.StreamKindPTY, Params: params},
			Target: grant.Target{
				Kind:                grant.TargetKindAgent,
				ID:                  u.agent.ID,
				EndpointIncarnation: res.Want.Incarnation,
				SessionID:           res.Record.SessionID,
				ConnectionEpoch:     res.Record.ConnectionEpoch,
			},
		})
		if err != nil {
			return err
		}
		st, err := res.Session.OpenStream(ctx, &conduitv1.StreamOpen{
			Kind:   conduitv1.StreamKind_STREAM_KIND_PTY,
			Params: params,
			Grant:  tok,
		})
		if err != nil {
			return err
		}
		u.st = st
		u.sessionID = res.Record.SessionID
		u.untrack = u.s.trackConduitUserStream(&conduitUserStream{
			Kind:      grant.StreamKindPTY,
			Identity:  u.identity,
			AgentID:   u.agent.ID,
			ProjectID: u.agent.ProjectID,
			SessionID: res.Record.SessionID,
			StreamID:  st.ID(),
			Close:     u.closeWithCode,
		})
		return nil
	})
	if err != nil {
		return ptyAgentOpenError(err)
	}
	u.buf = make([]byte, ptyAgentReadBuffer)
	return nil
}

// closeWithCode is the re-check's Close: it ends the stream on the target
// with code and reason, and the client leg with the same code.
func (u *agentPTYUpstream) closeWithCode(code uint32, reason string) {
	u.authzClose.CompareAndSwap(nil, &StreamClosedError{Code: int(code), Reason: reason})
	_ = u.st.CloseWithCode(code, reason)
}

func (u *agentPTYUpstream) Read(context.Context) ([]byte, error) {
	n, err := u.st.Read(u.buf)
	if n > 0 {
		return u.buf[:n], nil
	}
	return nil, u.streamEnd(err)
}

func (u *agentPTYUpstream) Write(data []byte) error {
	if _, err := u.st.Write(data); err != nil {
		return u.streamEnd(err)
	}
	return nil
}

// Resize forwards a resize within 1..4096; an out-of-range one is dropped
// (contracts §2), and the stream stays open.
func (u *agentPTYUpstream) Resize(cols, rows int) error {
	if cols < ptyMinDim || cols > conduitPTYMaxDim || rows < ptyMinDim || rows > conduitPTYMaxDim {
		slog.Debug("PTY resize out of range dropped", "agent_id", u.agent.ID, "cols", cols, "rows", rows)
		return nil
	}
	return u.st.Resize(uint16(cols), uint16(rows))
}

func (u *agentPTYUpstream) Close() {
	if u.untrack != nil {
		u.untrack()
	}
	if u.st != nil {
		_ = u.st.Close()
	}
}

// streamEnd maps the error that ended the stream to the client close
// (a *StreamClosedError, which ptyCloseCause passes through).
func (u *agentPTYUpstream) streamEnd(err error) error {
	if ce := u.authzClose.Load(); ce != nil {
		return ce
	}
	return ptyLeafCloseForStream(err)
}

// ptyAgentOpenError maps a failed agent-path open. A refusal with a close
// code (from the target or the relay) and a hub-side permission refusal
// become the matching client close; anything else stays an open failure
// (4503 stream_open_failed, retry).
func ptyAgentOpenError(err error) error {
	var ce *conduit.CloseError
	switch {
	case errors.As(err, &ce):
		code, reason := ptyLeafCloseCode(ce.Code, ce.Reason)
		return &StreamClosedError{Code: code, Reason: reason}
	case errors.Is(err, errConduitForbidden):
		return &StreamClosedError{Code: wsprotocol.ClosePTYForbidden, Reason: ptyCloseReasonForbidden}
	default:
		return err
	}
}

// ptyLeafCloseForStream maps the error a conduit PTY stream ended with:
// an orderly close (tmux detached or exited) is 1000, a close code maps
// per ptyLeafCloseCode, and a lost session (no close code) is 4504
// upstream_unreachable.
func ptyLeafCloseForStream(err error) error {
	var ce *conduit.CloseError
	switch {
	case errors.Is(err, io.EOF):
		return &StreamClosedError{Code: wsprotocol.ClosePTYNormal}
	case errors.As(err, &ce):
		code, reason := ptyLeafCloseCode(ce.Code, ce.Reason)
		return &StreamClosedError{Code: code, Reason: reason}
	default:
		return &StreamClosedError{Code: wsprotocol.ClosePTYUpstreamTimeout, Reason: ptyCloseReasonUpstreamUnreachable}
	}
}

// ptyLeafCloseCode maps a conduit close code (design §3.3.1) to the PTY
// leaf close code (pkg/wsprotocol/pty_close.go). Codes both contracts
// define keep their meaning and pass through; 0 is the normal close;
// 4499 (open cancelled) is a retriable 4503; any other code is 1011 with
// its reason kept for diagnosis.
func ptyLeafCloseCode(code uint32, reason string) (int, string) {
	switch code {
	case conduit.CloseNormal:
		return wsprotocol.ClosePTYNormal, ""
	case conduit.CloseUnauthenticated, conduit.CloseForbidden, uint32(wsprotocol.ClosePTYAgentNotFound),
		conduit.CloseRelayRestart, conduit.CloseRelayTimeout, uint32(wsprotocol.ClosePTYInternalError):
		return int(code), reason
	case conduit.CloseCancelled:
		return wsprotocol.ClosePTYUpstreamUnavailable, reason
	default:
		return wsprotocol.ClosePTYInternalError, reason
	}
}
