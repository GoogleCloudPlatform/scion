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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// One evaluation, three entry points (design v2.21.40 §3.9).
//
// evaluateConduitStream decides whether a user stream's principal still
// holds the authorization the stream was opened with, against current
// state. It takes no grant: the renewal entry points verify the presented
// grant first (renewConduitStream) and pass in what its signed claims say,
// and the in-process re-check passes in what the node recorded at open.
// Every entry point acts on the returned class and adds no decision of its
// own:
//
//   - hub-held renewal: renewConduitStream (verify, evaluate, mint);
//   - relay-held renewal: recheckRelayStream, an adapter over
//     renewConduitStream;
//   - in-process re-check: checkConduitUserStream.

// conduitStreamEval is the input to evaluateConduitStream.
type conduitStreamEval struct {
	// AgentID is the agent the stream reaches.
	AgentID string
	// ProjectID is the project the stream was authorized in. The agent's
	// current project must equal it.
	ProjectID string
	// Identity returns the principal to evaluate. It is called after the
	// agent lookup and the project compare. An error wrapping
	// errConduitPrincipalInactive is a deny; any other error is
	// unavailable.
	Identity func() (Identity, error)
	// Kind is the grant stream kind.
	Kind string
	// ViaTunnel marks a stream opened through the tunnel path.
	ViaTunnel bool
	// Port is the agent-local port of a tcp stream.
	Port int
	// ConfirmDeny, if set, is asked before an action deny is reported;
	// false makes the decision unavailable instead.
	ConfirmDeny func(agent *store.Agent) bool
	// LogAttrs are added to the evaluation's log lines.
	LogAttrs []any
}

// evaluateConduitStream evaluates in and returns the class, a short reason
// for logs and metrics, and the cause. The cause is in the renewal error
// vocabulary, so ClassifyConduitRenewal of it agrees with the class:
//
//   - conduitAuthzAllowed, nil;
//   - conduitAuthzTargetGone, errConduitAgentGone: the agent is gone;
//   - conduitAuthzDenied, an error wrapping errConduitForbidden (the
//     agent moved to another project, or an action is no longer held) or
//     errConduitPrincipalInactive;
//   - conduitAuthzUnavailable, an error wrapping errConduitAuthzUnavailable
//     (a lookup failed) or errConduitRenewalUnavailable (the agent's saved
//     placement refuses it).
func (s *Server) evaluateConduitStream(ctx context.Context, in conduitStreamEval) (conduitAuthzVerdict, string, error) {
	agent, err := s.store.GetAgent(ctx, in.AgentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return conduitAuthzTargetGone, "agent not found", errConduitAgentGone
	case err != nil:
		return conduitAuthzUnavailable, "agent lookup failed", fmt.Errorf("%w: agent lookup: %v", errConduitAuthzUnavailable, err)
	case agent == nil:
		// The store returned neither an agent nor an error: an internal
		// inconsistency, not a deleted agent and not a decision.
		return conduitAuthzUnavailable, "agent lookup failed", fmt.Errorf("%w: agent lookup returned no agent", errConduitAuthzUnavailable)
	case !agent.DeletedAt.IsZero():
		return conduitAuthzTargetGone, "agent deleted", errConduitAgentGone
	case agent.ProjectID != in.ProjectID:
		return conduitAuthzDenied, "agent moved to another project", fmt.Errorf("%w: agent is no longer in the grant's project", errConduitForbidden)
	}
	ident, err := in.Identity()
	switch {
	case errors.Is(err, errConduitPrincipalInactive):
		return conduitAuthzDenied, "principal not active", err
	case err != nil:
		return conduitAuthzUnavailable, "principal lookup failed", err
	}
	var confirm func() bool
	if in.ConfirmDeny != nil {
		confirm = func() bool { return in.ConfirmDeny(agent) }
	}
	verdict, reason, err := s.authorizeConduitUserStream(ctx, conduitUserStreamAuthz{
		Identity:    ident,
		Agent:       agent,
		Kind:        in.Kind,
		ViaTunnel:   in.ViaTunnel,
		Port:        in.Port,
		ConfirmDeny: confirm,
	})
	switch verdict {
	case conduitAuthzAllowed:
	case conduitAuthzDenied:
		if errors.Is(err, errConduitPrincipalInactive) {
			return conduitAuthzDenied, reason, err
		}
		return conduitAuthzDenied, reason, fmt.Errorf("%w: %s: %w", errConduitForbidden, reason, err)
	default:
		return conduitAuthzUnavailable, reason, fmt.Errorf("%w: %s: %w", errConduitAuthzUnavailable, reason, err)
	}
	if reason, err := s.conduitStreamPlacement(ctx, agent, in.LogAttrs); err != nil {
		return conduitAuthzUnavailable, reason, err
	}
	return conduitAuthzAllowed, "", nil
}

// conduitStreamPlacement applies mintConduitGrant's placement guard
// (checkAgentPinnedPlacement) to a stream re-check, after the
// authorization verdict, so a stream to a pinned agent whose saved
// placement is inconsistent gets no renewal, as an open gets no grant. A
// placement refusal wraps errConduitRenewalUnavailable and the
// *PlacementConflictError: no renewal is minted, and the stream is not
// closed from here; it ends at its current deadline. A failed placement
// read for a pinned agent wraps errConduitAuthzUnavailable, like the other
// lookups that fail during a re-check, and errConduitRenewalUnavailable:
// no renewal this attempt, and nothing closes. As for an open, an unpinned
// agent keeps today's path when the read fails, and a managed runtime is
// not checked.
func (s *Server) conduitStreamPlacement(ctx context.Context, agent *store.Agent, logAttrs []any) (string, error) {
	if isManagedAgentRuntime(agent.Runtime) {
		return "", nil
	}
	_, err := checkAgentPinnedPlacement(ctx, s.store, agent)
	if err == nil {
		return "", nil
	}
	if r, refused := placementRefusalOf(err); refused {
		slog.Warn("Conduit renewal refused for the agent's saved placement; no renewal minted",
			append([]any{"agent_id", agent.ID, "code", r.Code}, logAttrs...)...)
		return "agent placement inconsistent", fmt.Errorf("%w: agent placement: %w", errConduitRenewalUnavailable, err)
	}
	if agent.IsPinned() {
		return "agent placement check failed", fmt.Errorf("%w: %w: agent placement check: %w", errConduitAuthzUnavailable, errConduitRenewalUnavailable, err)
	}
	return "", nil
}
