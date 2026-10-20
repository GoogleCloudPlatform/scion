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
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Stream evaluation of legacy port proxy requests (design §3.5).
//
// A user request that the port-forward tunnel serves (proxyAgentPort's
// legacy branch) is evaluated before it is forwarded, with the evaluation
// a conduit port stream gets (checkLegacyPortRequest): the principal's
// status, agent.port_access with deny confirmation, the tcp target rule
// and the agent's placement. While the request is open it is tracked by
// the in-process re-check like a conduit port stream, so a re-check that
// denies it ends it. No grant is minted for it. Requests from an agent to
// its own ports are not evaluated here.

// legacyPortSessionPrefix marks the session id of a legacy port proxy
// request in the stream re-check's log lines.
const legacyPortSessionPrefix = "legacy-port:"

// legacyPortUnavailableMessage is the 503 message of a legacy port proxy
// request whose authorization could not be evaluated.
const legacyPortUnavailableMessage = "Port access could not be checked right now; try again"

// legacyPortClosed is the cause of a legacy port proxy request ended by
// the stream re-check; code is the re-check's close code.
type legacyPortClosed struct{ code uint32 }

func (e *legacyPortClosed) Error() string {
	return fmt.Sprintf("ended by the stream re-check (close code %d)", e.code)
}

// legacyPortRequest is a user request the port-forward tunnel serves,
// evaluated and tracked by the in-process re-check. Its methods accept a
// nil receiver (a request that is not evaluated): it then has no context
// of its own and is never ended by a re-check.
type legacyPortRequest struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	untrack func()
}

// startLegacyPortRequest evaluates a user request to agent's exposed port
// before the port-forward tunnel serves it. A refused request is answered
// here and ok is false. An allowed request is tracked by the in-process
// re-check until end is called; a re-check that denies it cancels its
// context. Non-user identities are not evaluated (nil, true).
func (s *Server) startLegacyPortRequest(w http.ResponseWriter, r *http.Request, identity Identity, agent *store.Agent, port int) (l *legacyPortRequest, ok bool) {
	if !conduitIsUserIdentity(identity) {
		return nil, true
	}
	st := &conduitUserStream{
		Kind:       grant.StreamKindTCP,
		Identity:   identity,
		AgentID:    agent.ID,
		ProjectID:  agent.ProjectID,
		Port:       port,
		SessionID:  legacyPortSessionPrefix + uuid.NewString(),
		LegacyPort: true,
	}
	cctx, cancel := context.WithTimeout(r.Context(), conduitAuthzCheckTimeout)
	verdict, err := s.checkLegacyPortRequest(cctx, st)
	if verdict != conduitAuthzUnavailable && cctx.Err() != nil {
		// As for a re-check: a verdict reached after the check's deadline
		// is not trusted.
		verdict, err = conduitAuthzUnavailable, fmt.Errorf("%w: check timed out", errConduitAuthzUnavailable)
	}
	cancel()
	if verdict != conduitAuthzAllowed {
		writeLegacyPortRefusal(w, r, verdict, err)
		return nil, false
	}
	ctx, cancelCause := context.WithCancelCause(r.Context())
	st.Close = func(code uint32, _ string) { cancelCause(&legacyPortClosed{code: code}) }
	return &legacyPortRequest{ctx: ctx, cancel: cancelCause, untrack: s.trackConduitUserStream(st)}, true
}

// context returns the request's context: one a re-check can end, or r's
// for a request that is not evaluated.
func (l *legacyPortRequest) context(r *http.Request) context.Context {
	if l == nil {
		return r.Context()
	}
	return l.ctx
}

// end untracks the request; call it when the handler returns.
func (l *legacyPortRequest) end() {
	if l == nil {
		return
	}
	l.untrack()
	l.cancel(context.Canceled)
}

// refused reports whether a re-check ended the request, and if so writes
// its answer: 404 when the agent is gone, else 403. Call it before
// anything is written for the request, whatever the tunnel returned.
func (l *legacyPortRequest) refused(w http.ResponseWriter, r *http.Request) bool {
	if l == nil {
		return false
	}
	var closed *legacyPortClosed
	if !errors.As(context.Cause(l.ctx), &closed) {
		return false
	}
	if closed.code == relay.CloseTargetNotFound {
		writeLegacyPortRefusal(w, r, conduitAuthzTargetGone, errConduitAgentGone)
	} else {
		writeLegacyPortRefusal(w, r, conduitAuthzDenied, errConduitForbidden)
	}
	return true
}

// writeLegacyPortRefusal answers a legacy port proxy request that the
// stream evaluation did not allow, by its class, with the answer the
// conduit port proxy gives for the same outcome:
//
//   - denied: 403 forbidden "Access denied" (writeConduitProxyError);
//   - target gone: 404 not found, as for an agent that does not exist
//     (authorizePortAccess);
//   - unavailable for the agent's saved placement: the placement refusal
//     (writeConduitProxyError);
//   - any other unavailable: 503 unavailable, retryable (the proxy error
//     page for a browser).
func writeLegacyPortRefusal(w http.ResponseWriter, r *http.Request, verdict conduitAuthzVerdict, cause error) {
	switch verdict {
	case conduitAuthzDenied:
		writeConduitProxyError(w, r, "", fmt.Errorf("%w: %w", errConduitForbidden, cause))
	case conduitAuthzTargetGone:
		writeErrorFromErr(w, store.ErrNotFound, "")
	default:
		var placement *PlacementConflictError
		if errors.As(cause, &placement) {
			writeConduitProxyError(w, r, "", placement)
			return
		}
		if isBrowserRequest(r) {
			writeProxyErrorHTML(w, http.StatusServiceUnavailable, "Service Unavailable", legacyPortUnavailableMessage)
			return
		}
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, legacyPortUnavailableMessage, nil)
	}
}
