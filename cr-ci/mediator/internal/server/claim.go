package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/claim"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/oidc"
)

// ClaimHeader carries the runner's one-time claim token (claim contract v2).
const ClaimHeader = "X-Runner-Claim"

// handleClaim implements claim contract v2:
//
//  1. Authorization: Bearer <Google ID token> of the runner identity
//     (EXEC_SA_NAME, audience MEDIATOR_URL). Missing or invalid: 401, and the
//     claim is NOT consumed, so a principal who read the claim token from the
//     execution env cannot burn or steal it.
//  2. X-Runner-Claim: <32 lowercase hex>. Missing, malformed, unknown,
//     expired or used: 410.
//  3. 200 application/json {"jit_config": "..."}; the claim is consumed.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	idToken, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || idToken == "" {
		s.log().Warn("claim rejected", "event", logging.EventClaimRejected, "reason", "identity_missing")
		plain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := s.o.RunnerOIDC.Verify(r.Context(), idToken); err != nil {
		if errors.Is(err, oidc.ErrKeysUnavailable) {
			// Not the runner's fault: 503 so it retries. Nothing consumed.
			s.log().Error("claim deferred: cannot verify the runner identity", "event", logging.EventError, "stage", "claim_identity", "error", err.Error())
			plain(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		s.log().Warn("claim rejected", "event", logging.EventClaimRejected, "reason", "identity_invalid", "detail", err.Error())
		plain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	token := r.Header.Get(ClaimHeader)
	if len(token) != claim.TokenLen {
		s.log().Warn("claim rejected", "event", logging.EventClaimRejected, "reason", "malformed")
		plain(w, http.StatusGone, "gone")
		return
	}
	jit, nameID, ok := s.o.Claims.Consume(token)
	if !ok {
		s.log().Warn("claim rejected", "event", logging.EventClaimRejected, "reason", "unknown_expired_or_used")
		plain(w, http.StatusGone, "gone")
		return
	}
	b, err := json.Marshal(struct {
		JITConfig string `json:"jit_config"`
	}{jit})
	if err != nil {
		plain(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	s.log().Info("claim consumed", "event", logging.EventClaimConsumed, "runner", RunnerName(nameID))
}
