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
	"net/http"
	"time"

	"github.com/google/uuid"
)

// agentTokenRefusal is the outcome of verifyAgentTokenCredential.
type agentTokenRefusal int

const (
	// agentTokenAccepted: the token may authenticate the request.
	agentTokenAccepted agentTokenRefusal = iota
	// agentTokenRevoked: the credential is revoked or the agent is held.
	agentTokenRevoked
	// agentTokenInvalidSubject: the subject names no agent.
	agentTokenInvalidSubject
	// agentTokenRunRefused: the token's run check refused it.
	agentTokenRunRefused
	// agentTokenUnevaluated: the credential status, hold or token run
	// could not be read, so the token was neither accepted nor refused.
	agentTokenUnevaluated
)

// verifyAgentTokenCredential runs the checks that follow a successful
// ValidateAgentToken (signature and expiry) before an agent token
// authenticates anything: the credential's status (step 1a), an active
// agent hold (1b) and the token's run (1c). It returns ctx with the
// credential context values set (legacy marker or credential id) and
// the outcome. UnifiedAuthMiddleware maps a refusal to its HTTP response
// with writeAgentTokenRefusal; the conduit relay applies the same checks
// to a credential presented in band.
func verifyAgentTokenCredential(ctx context.Context, cfg AuthConfig, log *slog.Logger, claims *AgentTokenClaims, req runScopeRequest, source string) (context.Context, agentTokenRefusal) {
	// Step 1a: Agent-token authentication requires a successful
	// credential-status evaluation (Phase 1H). See
	// evaluateAgentCredentialStatus for the possible outcomes.
	var credState agentTokenCredentialState
	if cfg.CredentialStore != nil && claims.ID != "" {
		cred, isLegacy, credErr := evaluateAgentCredentialStatus(ctx, cfg.CredentialStore, claims.ID)
		credState.evaluated = true
		switch {
		case errors.Is(credErr, errAgentCredentialRevoked):
			return ctx, agentTokenRevoked
		case credErr != nil:
			// Any store error other than "not found" must not fall
			// back to authenticating the request: it means the
			// credential's status could not actually be determined,
			// so treat it as a retryable failure.
			log.Error("Agent credential status lookup failed",
				"agent_id", claims.Subject, "error", credErr)
			return ctx, agentTokenUnevaluated
		case isLegacy:
			// Legacy compatibility path: tokens issued before the
			// credential table existed have no credential record.
			// Retained pending a product decision; do not close or
			// widen this branch.
			log.Warn("Agent token not found in credential store (legacy/pre-table token)",
				"agent_id", claims.Subject, "jti_hash", hashJTI(claims.ID)[:8])
			ctx = context.WithValue(ctx, legacyTokenContextKey{}, true)
		default:
			// Credential found and active.
			credState.cred = cred
			ctx = context.WithValue(ctx, agentCredentialIDContextKey{}, cred.ID)
			// Update last_seen_at (fire-and-forget)
			go func() {
				_ = cfg.CredentialStore.UpdateAgentCredentialLastSeen(
					context.Background(), cred.ID, time.Now())
			}()
		}
	}

	// Step 1b: a held agent's token is refused on every
	// request, including tokens on the legacy path above
	// (ptone/scion#3433). Same response as a revoked
	// credential; a lookup fault is the same 503.
	if cfg.HoldStore != nil {
		// A subject that is not an agent UUID names no agent:
		// an authentication failure, refused before the hold
		// lookup.
		if _, perr := uuid.Parse(claims.Subject); perr != nil {
			return ctx, agentTokenInvalidSubject
		}
		held, holdErr := cfg.HoldStore.HasActiveAgentHold(ctx, claims.Subject)
		if holdErr != nil {
			log.Error("Agent hold lookup failed",
				"agent_id", claims.Subject, "error", holdErr)
			return ctx, agentTokenUnevaluated
		}
		if held {
			return ctx, agentTokenRevoked
		}
	}

	// Step 1c: the token's run, only when
	// server.auth.agent_run_scope is not off.
	if rs := cfg.AgentRunScope; rs != nil {
		switch rs.check(ctx, claims, credState, req, source) {
		case runScopeDeny:
			return ctx, agentTokenRunRefused
		case runScopeUnavailable:
			return ctx, agentTokenUnevaluated
		}
	}
	return ctx, agentTokenAccepted
}

// writeAgentTokenRefusal writes UnifiedAuthMiddleware's response to a
// refusal from verifyAgentTokenCredential.
func writeAgentTokenRefusal(w http.ResponseWriter, refusal agentTokenRefusal) {
	switch refusal {
	case agentTokenRevoked:
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"token has been revoked", nil)
	case agentTokenInvalidSubject:
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"invalid agent token", nil)
	case agentTokenRunRefused:
		writeAgentTokenRefused(w)
	default:
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"unable to verify credential status", nil)
	}
}
