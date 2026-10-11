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

package tunnelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TunnelsPath is the RPC path of a tunnel request.
const TunnelsPath = "/v1/tunnels"

// conduitPath is the hub's conduit endpoint.
const conduitPath = "/api/v1/conduit"

// Error codes a tunnel request can be refused with.
const (
	codeAgentNotFound            = "agent_not_found"
	codeAgentNotRunning          = "agent_not_running"
	codeRateLimited              = "rate_limited"
	codeRuntimeBrokerUnavailable = "runtime_broker_unavailable"
	codeAgentSessionUnavailable  = "agent_session_unavailable"
	codeUnavailable              = "unavailable"
	codeInternalError            = "internal_error"
	codeForbidden                = "forbidden"
)

// session is one user session to the hub.
type session struct {
	ls      conduit.LocalSession
	id      string
	streams *streamBox
}

// tunnelRequest is the body of POST /v1/tunnels.
type tunnelRequest struct {
	Agent  string            `json:"agent"`
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
}

// tunnelResponse is the body of a successful POST /v1/tunnels.
type tunnelResponse struct {
	StreamID      uint32     `json:"streamId"`
	SessionID     string     `json:"sessionId"`
	AgentID       string     `json:"agentId"`
	ProjectID     string     `json:"projectId"`
	Kind          string     `json:"kind"`
	AuthzDeadline *time.Time `json:"authzDeadline,omitempty"`
}

// errorBody is the hub's error format.
type errorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// conduitURL turns the hub endpoint into its conduit WebSocket URL.
func conduitURL(endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid hub endpoint %q", endpoint)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("invalid hub endpoint %q: scheme must be http or https", endpoint)
	}
	u.Path = strings.TrimRight(u.Path, "/") + conduitPath
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// dial opens a user session with cred. Its errors carry exit codes.
func (c *client) dial(ctx context.Context, cred Credential) (*session, error) {
	u, err := conduitURL(c.cfg.Endpoint)
	if err != nil {
		return nil, Errorf(ExitUsage, "%v", err)
	}
	box := newStreamBox(c.cfg.streamWait())
	scfg := c.cfg.Session
	scfg.RPCHandler = nil
	scfg.StreamHandler = conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		if ps.ID()%2 != 0 {
			return conduit.Reject(conduit.CloseProtocolError, "bad_frame")
		}
		st, err := ps.Accept()
		if err != nil {
			return nil
		}
		box.deliver(ps.ID(), st)
		return nil
	})
	d := &ws.Dialer{
		URL: u,
		Header: func(context.Context) (http.Header, error) {
			h := http.Header{}
			h.Set("Authorization", "Bearer "+cred.Token)
			if c.cfg.ApplyTransport != nil {
				if err := c.cfg.ApplyTransport(h); err != nil {
					return nil, err
				}
			}
			return h, nil
		},
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_USER,
		PrincipalId:   c.user.ID,
		ClientVersion: c.cfg.ClientVersion,
	}
	dctx, cancel := context.WithTimeout(ctx, c.cfg.dialTimeout())
	defer cancel()
	s, w, err := conduit.Dial(dctx, d, scfg, hello)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, c.dialError(err)
	}
	ls, ok := s.(conduit.LocalSession)
	if !ok {
		_ = s.Close()
		return nil, Errorf(ExitError, "unexpected conduit session type")
	}
	return &session{ls: ls, id: w.GetSessionId(), streams: box}, nil
}

// dialError maps a failed session dial to its exit code.
func (c *client) dialError(err error) error {
	ep := c.cfg.Endpoint
	var de *ws.DialError
	if errors.As(err, &de) {
		switch {
		case de.StatusCode == 0:
			return Errorf(ExitHubUnavailable, "cannot reach the hub at %s: %v", ep, de.Err)
		case de.StatusCode == http.StatusUnauthorized:
			return Errorf(ExitCredential, "the hub at %s refused the credential; %s", ep, loginHint(ep))
		case de.StatusCode == http.StatusForbidden:
			return Errorf(ExitForbidden, "the hub at %s does not permit a tunnel session for this credential", ep)
		case de.StatusCode == http.StatusNotImplemented:
			return Errorf(ExitNoTunnelSupport, "the hub at %s does not support tunnels (HTTP %d)", ep, de.StatusCode)
		case de.StatusCode >= 500:
			return Errorf(ExitHubUnavailable, "the hub at %s is unavailable (HTTP %d)", ep, de.StatusCode)
		default:
			// 404 (no conduit route, or conduit off), 405, or a page
			// that is not a WebSocket upgrade.
			return Errorf(ExitNoTunnelSupport, "the hub at %s does not support tunnels (HTTP %d)", ep, de.StatusCode)
		}
	}
	var ce *conduit.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case conduit.CloseUnauthenticated:
			return Errorf(ExitCredential, "the hub at %s refused the credential; %s", ep, loginHint(ep))
		case conduit.CloseForbidden:
			return Errorf(ExitForbidden, "the hub at %s refused the session: %s", ep, reasonOr(ce.Reason, "forbidden"))
		case conduit.CloseRelayRestart, conduit.CloseRelayTimeout:
			return Errorf(ExitHubUnavailable, "the hub at %s is unavailable (%s)", ep, reasonOr(ce.Reason, "relay_restart"))
		}
		return Errorf(ExitError, "the hub at %s refused the session with code %d (%s)", ep, ce.Code, ce.Reason)
	}
	return Errorf(ExitHubUnavailable, "cannot reach the hub at %s: %v", ep, err)
}

// outcome is the client's reading of a tunnel response.
type outcome int

const (
	outOK outcome = iota
	// outDrain: a 503 with no error code (session draining, rule 5).
	outDrain
	// outUnauthorized: 401 (rule 6).
	outUnauthorized
	// outTransient: 503 unavailable, retried once on the same session.
	outTransient
	// outFail: a final failure.
	outFail
)

// classify reads a tunnel response: its outcome, the success body, and
// for a transient fault its reason, and for a final failure its error.
func classify(resp *conduitv1.RpcResponse) (outcome, *tunnelResponse, string, error) {
	status := int(resp.GetStatus())
	if status == http.StatusOK {
		var tr tunnelResponse
		if err := json.Unmarshal(resp.GetBody(), &tr); err != nil || tr.StreamID == 0 || tr.StreamID%2 != 0 {
			return outFail, nil, "", Errorf(ExitError, "the hub sent an invalid tunnel response")
		}
		return outOK, &tr, "", nil
	}
	var eb errorBody
	_ = json.Unmarshal(resp.GetBody(), &eb)
	code, msg := eb.Error.Code, eb.Error.Message
	reason, _ := eb.Error.Details["reason"].(string)
	fail := func(exit int, format string, args ...any) (outcome, *tunnelResponse, string, error) {
		return outFail, nil, "", Errorf(exit, format, args...)
	}
	switch status {
	case http.StatusUnauthorized:
		return outUnauthorized, nil, "", nil
	case http.StatusServiceUnavailable:
		switch code {
		case "":
			return outDrain, nil, "", nil
		case codeRuntimeBrokerUnavailable:
			return fail(ExitTargetUnavailable, "the agent's runtime broker is not connected (%s)", code)
		case codeAgentSessionUnavailable:
			return fail(ExitTargetUnavailable, "the agent's session is not connected (%s)", code)
		case codeUnavailable:
			return outTransient, nil, reasonOr(reason, codeUnavailable), nil
		}
		return fail(ExitHubUnavailable, "the hub is unavailable (%s)", code)
	case http.StatusInternalServerError:
		return fail(ExitError, "the hub failed the tunnel request (%s)", reasonOr(code, codeInternalError))
	case http.StatusForbidden:
		return fail(ExitForbidden, "not permitted to open this tunnel (%s)", reasonOr(code, codeForbidden))
	case http.StatusNotFound:
		if code == codeAgentNotFound {
			return fail(ExitAgentNotFound, "agent not found or deleted (%s)", code)
		}
		return fail(ExitNoTunnelSupport, "the hub does not support tunnels (unknown method)")
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return fail(ExitNoTunnelSupport, "the hub does not support tunnels (HTTP %d)", status)
	case http.StatusConflict:
		if code == codeAgentNotRunning {
			return fail(ExitAgentNotRunning, "the agent is not running (%s)", code)
		}
	case http.StatusTooManyRequests:
		return fail(ExitCapReached, "too many tunnels on this session (%s)", reasonOr(code, codeRateLimited))
	case http.StatusBadRequest:
		// Input is validated locally first; a 400 that still comes back
		// is not a usage error.
		return fail(ExitError, "the hub rejected the tunnel request: %s", reasonOr(msg, "invalid_request"))
	}
	return fail(ExitError, "the tunnel request failed (HTTP %d %s)", status, code)
}

// openTunnel asks for one tunnel and returns its stream. It applies the
// client rules: a draining session (bare 503, or one that
// sent GoAway) gets one retry on a fresh session; a 401 re-resolves the
// credential once and, when the identity is unchanged, gets one retry on
// a fresh session (open tunnels are left alone); 503 unavailable gets one
// retry on the same session after a jittered delay; anything else is
// final, including 500 internal_error.
func (c *client) openTunnel(ctx context.Context) (conduit.Stream, *tunnelEntry, error) {
	body, err := json.Marshal(tunnelRequest{Agent: c.target.AgentID, Kind: c.req.Kind(), Params: c.req.params()})
	if err != nil {
		return nil, nil, Errorf(ExitError, "encoding the tunnel request: %v", err)
	}
	freshUsed, transientUsed := false, false
	var lastReason string
	for {
		s := c.current()
		resp, err := s.ls.Call(ctx, &conduitv1.RpcRequest{
			Method:  http.MethodPost,
			Path:    TunnelsPath,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    body,
		})
		var out outcome
		var tr *tunnelResponse
		var final error
		var reason string
		switch {
		case err == nil:
			out, tr, reason, final = classify(resp)
		case ctx.Err() != nil:
			return nil, nil, ctx.Err()
		case errors.Is(err, conduit.ErrDraining) || goAwayReceived(s):
			out = outDrain
		default:
			return nil, nil, Errorf(ExitHubUnavailable, "the session to the hub at %s was lost: %v", c.cfg.Endpoint, err)
		}

		switch out {
		case outOK:
			st, err := s.streams.take(ctx, tr.StreamID)
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				return nil, nil, Errorf(ExitError, "the hub did not open the tunnel stream: %v", err)
			}
			entry := &tunnelEntry{sessionID: s.id, openedAt: c.cfg.now(), deadline: tr.AuthzDeadline}
			return st, entry, nil
		case outFail:
			return nil, nil, final
		case outDrain:
			if freshUsed {
				return nil, nil, Errorf(ExitHubUnavailable, "the hub at %s is draining sessions", c.cfg.Endpoint)
			}
			freshUsed = true
			if err := c.replaceSession(ctx, c.cred); err != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				return nil, nil, Errorf(ExitHubUnavailable, "the session was draining and a fresh one could not be opened: %v", err)
			}
		case outUnauthorized:
			if freshUsed {
				return nil, nil, Errorf(ExitCredential, "the hub at %s refused the credential; %s", c.cfg.Endpoint, loginHint(c.cfg.Endpoint))
			}
			freshUsed = true
			cred, err := c.reresolve(ctx)
			if err != nil {
				return nil, nil, err
			}
			if err := c.replaceSession(ctx, cred); err != nil {
				return nil, nil, err
			}
			c.cred = cred
		case outTransient:
			// The final line names the latest reason.
			lastReason = reason
			if transientUsed {
				return nil, nil, Errorf(ExitHubUnavailable, "the hub at %s is unavailable (%s)", c.cfg.Endpoint, lastReason)
			}
			transientUsed = true
			if err := c.cfg.sleep(ctx, c.cfg.jitter()); err != nil {
				return nil, nil, err
			}
		}
	}
}

// reresolve applies client rule 6 after a refused tunnel request: the
// credential is resolved once more without prompting, and it must belong
// to the same identity.
func (c *client) reresolve(ctx context.Context) (Credential, error) {
	ep := c.cfg.Endpoint
	cred := c.cfg.ResolveCredential()
	if cred.Token == "" {
		return Credential{}, Errorf(ExitCredential, "the credential for %s expired or was refused; %s", ep, loginHint(ep))
	}
	user, err := c.cfg.Hub.Whoami(ctx, cred)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
			return Credential{}, Errorf(ExitCredential, "the credential for %s expired or was refused; %s", ep, loginHint(ep))
		}
		return Credential{}, c.restError(ctx, err, "checking the credential")
	}
	if cred.Kind != c.id.Kind || user.ID != c.id.Subject {
		return Credential{}, Errorf(ExitCredential, "the credential for %s now belongs to a different identity; refusing to mix identities in one process (run the command again)", ep)
	}
	return cred, nil
}

// replaceSession dials a fresh session with cred and makes it current. The
// old session stays open for its tunnels until it ends.
func (c *client) replaceSession(ctx context.Context, cred Credential) error {
	s, err := c.dial(ctx, cred)
	if err != nil {
		return err
	}
	c.setCurrent(s)
	return nil
}

// goAwayReceived reports whether the hub asked s to drain.
func goAwayReceived(s *session) bool {
	select {
	case <-s.ls.GoAwayReceived():
		return true
	default:
		return false
	}
}

// streamBox matches hub-opened streams to the tunnel responses that name
// them. The hub opens a tunnel's stream before it answers the request, so
// a stream usually arrives first and waits here.
type streamBox struct {
	wait time.Duration
	mu   sync.Mutex
	m    map[uint32]chan conduit.Stream
}

func newStreamBox(wait time.Duration) *streamBox {
	return &streamBox{wait: wait, m: map[uint32]chan conduit.Stream{}}
}

func (b *streamBox) slot(id uint32) chan conduit.Stream {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.m[id]
	if !ok {
		ch = make(chan conduit.Stream, 1)
		b.m[id] = ch
	}
	return ch
}

// deliver parks a hub-opened stream until a response claims it. A stream
// no response claims within the wait is closed.
func (b *streamBox) deliver(id uint32, st conduit.Stream) {
	ch := b.slot(id)
	select {
	case ch <- st:
	default:
		_ = st.CloseWithCode(conduit.CloseProtocolError, "bad_frame")
		return
	}
	time.AfterFunc(b.wait, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.m[id] != ch {
			return
		}
		select {
		case orphan := <-ch:
			delete(b.m, id)
			_ = orphan.CloseWithCode(conduit.CloseCancelled, "cancelled")
		default:
		}
	})
}

// take waits for the stream with id.
func (b *streamBox) take(ctx context.Context, id uint32) (conduit.Stream, error) {
	ch := b.slot(id)
	t := time.NewTimer(b.wait)
	defer t.Stop()
	defer func() {
		b.mu.Lock()
		if b.m[id] == ch {
			delete(b.m, id)
		}
		b.mu.Unlock()
	}()
	select {
	case st := <-ch:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("stream %d did not arrive within %s", id, b.wait)
	}
}
