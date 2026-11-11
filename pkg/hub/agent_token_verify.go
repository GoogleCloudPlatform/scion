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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// agentTokenRefusal is the outcome of verifyAgentTokenCredential.
type agentTokenRefusal int

const (
	// agentTokenAccepted: the token may authenticate the request.
	agentTokenAccepted agentTokenRefusal = iota
	// agentTokenRevoked: the credential is revoked.
	agentTokenRevoked
	// agentTokenHeld: the agent is held.
	agentTokenHeld
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
			// Update last_seen_at (fire-and-forget, at most once per
			// agentCredentialLastSeenInterval).
			touchAgentCredentialLastSeen(ctx, cfg.CredentialStore, cred, time.Now())
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
			return ctx, agentTokenHeld
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

// Agent-token refusal causes. A cause is logged only: the client gets
// writeAgentTokenRefused whatever the cause.
const (
	agentTokenCauseRevoked            = "revoked"              // credential revoked
	agentTokenCauseHeld               = "held"                 // agent held
	agentTokenCauseInvalidSubject     = "invalid_subject"      // subject names no agent
	agentTokenCauseInvalidToken       = "invalid_token"        // X-Scion-Agent-Token failed validation
	agentTokenCauseHeaderNotValidated = "header_not_validated" // X-Scion-Agent-Token reached dev auth unvalidated
	agentTokenCauseRefreshRevoked     = "refresh_revoked"      // token refresh found the credential revoked
)

// agentTokenRefusedLogMessage is the message of the log line for a
// refused agent token.
const agentTokenRefusedLogMessage = "agent token refused"

// refuseAgentToken logs cause and attrs at info level on log (nil:
// slog.Default) and writes the shared agent-token refusal.
func refuseAgentToken(ctx context.Context, w http.ResponseWriter, log *slog.Logger, cause string, attrs ...any) {
	if log == nil {
		log = slog.Default()
	}
	log.InfoContext(ctx, agentTokenRefusedLogMessage, append([]any{"cause", cause}, attrs...)...)
	writeAgentTokenRefused(w)
}

// writeAgentTokenRefusal writes UnifiedAuthMiddleware's response to a
// refusal from verifyAgentTokenCredential for the token of agentID: the
// shared refusal for every cause, or 503 when the token could not be
// evaluated.
func writeAgentTokenRefusal(ctx context.Context, w http.ResponseWriter, log *slog.Logger, refusal agentTokenRefusal, agentID string) {
	switch refusal {
	case agentTokenRevoked:
		refuseAgentToken(ctx, w, log, agentTokenCauseRevoked, "agent_id", agentID)
	case agentTokenHeld:
		refuseAgentToken(ctx, w, log, agentTokenCauseHeld, "agent_id", agentID)
	case agentTokenInvalidSubject:
		refuseAgentToken(ctx, w, log, agentTokenCauseInvalidSubject, "agent_id", agentID)
	case agentTokenRunRefused:
		// The run-scope checker has logged the outcome.
		writeAgentTokenRefused(w)
	default:
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"unable to verify credential status", nil)
	}
}

// agentCredentialLastSeenDue reports whether a credential last seen at
// lastSeen (nil: never) is due a last_seen_at write at now.
func agentCredentialLastSeenDue(lastSeen *time.Time, now time.Time) bool {
	return lastSeen == nil || now.Sub(*lastSeen) >= agentCredentialLastSeenInterval
}

// agentCredentialLastSeenSpawnKey is a test seam: a func(func()) under
// this request context key runs the last_seen_at write instead of a new
// goroutine, so a test can tell deterministically whether a write was
// made.
type agentCredentialLastSeenSpawnKey struct{}

// touchAgentCredentialLastSeen writes cred's last_seen_at in the
// background when the row read for this request shows it older than the
// interval. The write is best effort (5s timeout, error ignored) and never
// affects the request; a failed write leaves the row old, so the next
// request writes again. Concurrent requests at the interval boundary may
// each write; that is acceptable. A nil store or credential writes nothing.
func touchAgentCredentialLastSeen(ctx context.Context, creds store.AgentCredentialStore, cred *store.AgentCredential, now time.Time) {
	if creds == nil || cred == nil {
		return
	}
	if !agentCredentialLastSeenDue(cred.LastSeenAt, now) {
		return
	}
	credID := cred.ID
	write := func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = creds.UpdateAgentCredentialLastSeen(bg, credID, now)
	}
	if spawn, ok := ctx.Value(agentCredentialLastSeenSpawnKey{}).(func(func())); ok {
		spawn(write)
		return
	}
	go write()
}
