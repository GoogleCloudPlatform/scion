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
	"net/http"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// External bearer authentication.
//
// The Hub accepts a Google-issued end-user credential directly in
// Authorization: Bearer, without a credential-exchange round trip. This is
// the Phase 1 vertical slice: Google OIDC ID tokens for USER principals only.
// Access tokens, service accounts, the caching decorator, and the rate
// limiter all land in later phases (design §5).
//
// serveExternalBearer runs only after the request has already failed every
// other authentication path (Hub JWT, PAT, agent, proxy, federation). It is
// therefore load-bearing that it never changes the outcome of a request that
// authenticates today: when the token is not something this path can vouch
// for, it reports errExternalBearerNotApplicable and the caller emits its
// original, byte-identical rejection.
//
// A verified identity is resolved to a Hub user through the same
// GoogleIdentityResolver the GE exchange endpoint uses (ge_exchange.go,
// google_identity_resolver.go), so both mechanisms reach identical decisions
// for the same Google identity during the soak between them (design §3, §4.4).

// errExternalBearerNotApplicable reports that the token is not something the
// external-bearer path can vouch for (no Google trust configured, the token
// isn't a JWT with a Google issuer, or — in later phases — no issuer for the
// token shape). Callers fall back to their normal rejection message.
var errExternalBearerNotApplicable = errors.New("external bearer: not applicable")

// errExternalBearerPrincipalRejected reports that the verified identity's
// principal type is not accepted on this path in this phase (currently:
// service accounts — SA support lands in a later phase). It is a distinct
// sentinel, rather than an ad-hoc error, so a future outcome metric (design
// §4.7) can label this rejection without string matching, and so the mapping
// in serveExternalBearer stays entirely errors.Is-driven.
var errExternalBearerPrincipalRejected = errors.New("external bearer: principal type not accepted in this phase")

// errExternalBearerResolveFailed marks any error classifyResolveError sees
// (i.e. any error GoogleResolver.Resolve returns). It is always present on a
// Resolve error, regardless of the underlying cause, so a Resolve error can
// never fall into serveExternalBearer's default 401 arm: the specific 403
// arms (suspended, access denied, non-authoritative email, binding conflict,
// store.ErrNotFound) are checked first against the same, still-intact error
// chain, and everything else lands on the 503 store_error arm via this
// sentinel. That is deliberate: a validator/principal-policy failure (bad
// signature, wrong audience, SA in this phase, ...) is a credential
// rejection (401); anything from Resolve is either a known policy outcome or
// an internal/store fault (403/503) — never "invalid token" (design §4.4
// item 6, fix round 2; simplified in fix round 3 per review r3 optional
// finding 2, which showed the prior allowlist in classifyResolveError was
// redundant with serveExternalBearer's own arms and, if the two ever
// diverged, could misroute a future resolver error to 401).
var errExternalBearerResolveFailed = errors.New("external bearer: resolve failed")

// externalBearerKind classifies a bearer token for the external-bearer path.
type externalBearerKind int

const (
	externalBearerNotApplicable externalBearerKind = iota
	// externalBearerIDToken is a JWT whose unverified iss claims a Google
	// issuer. Verification (signature, exp, aud, ...) happens in the
	// validator; classification only routes the request.
	externalBearerIDToken
	// Access tokens (opaque, non-JWT) are classified in Phase 2.
)

// classifyExternalBearer classifies token for routing purposes only. It reads
// the JWT's unverified iss claim — cryptographic verification happens later,
// in cfg.GoogleValidator.
func classifyExternalBearer(token string) externalBearerKind {
	if !looksLikeJWT(token) {
		// Phase 1 does not handle opaque access tokens; treat as not
		// applicable rather than misclassifying it as an ID token.
		return externalBearerNotApplicable
	}
	iss, ok := peekJWTIssuer(token)
	if !ok {
		return externalBearerNotApplicable
	}
	if iss == googleIssuerHTTPS || iss == googleIssuerBare {
		return externalBearerIDToken
	}
	return externalBearerNotApplicable
}

// peekJWTIssuer extracts the iss claim from a JWT WITHOUT verifying its
// signature. Used only to route the request to the right verifier; the
// verifier itself always re-checks the issuer cryptographically.
func peekJWTIssuer(token string) (string, bool) {
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return "", false
	}
	var claims jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", false
	}
	return claims.Issuer, true
}

// googleTrust returns the trusted-issuer configuration for Google
// (accounts.google.com) if — and only if — it is configured as a user-type
// issuer with a non-empty expected_audience. Reads through cfg.FederationAuth
// on every call, so hot-reloaded trust config takes effect without a restart.
func googleTrust(cfg AuthConfig) (config.TrustedIssuerConfig, bool) {
	if cfg.FederationAuth == nil {
		return config.TrustedIssuerConfig{}, false
	}
	fedAuth := cfg.FederationAuth.Load()
	if fedAuth == nil {
		return config.TrustedIssuerConfig{}, false
	}
	trust, ok := fedAuth.IssuerConfig(googleIssuerHTTPS)
	if !ok || IssuerType(trust.IssuerType) != IssuerTypeUser || trust.ExpectedAudience == "" {
		return config.TrustedIssuerConfig{}, false
	}
	return trust, true
}

// serveExternalBearer attempts to authenticate the request with an external
// bearer token. It returns true when it has written a response or served the
// request, and false when the token is not an external bearer token the Hub
// can vouch for (the caller then emits its usual rejection).
func serveExternalBearer(w http.ResponseWriter, r *http.Request, next http.Handler,
	ctx context.Context, token string, cfg AuthConfig, log *slog.Logger) bool {

	user, err := authenticateExternalBearer(ctx, token, cfg)
	if err != nil {
		switch {
		case errors.Is(err, errExternalBearerNotApplicable):
			if cfg.Debug {
				log.Debug("External bearer not applicable", "error", err)
			}
			return false
		case errors.Is(err, ErrUserSuspended):
			log.Warn("External bearer rejected: user is suspended", "error", err)
			writeError(w, http.StatusForbidden, "user_suspended",
				"access denied: user account is suspended", nil)
			return true
		case errors.Is(err, ErrAccessDenied),
			errors.Is(err, errNonAuthoritativeEmail),
			errors.Is(err, errBindingConflict),
			errors.Is(err, errAmbiguousLinkage),
			errors.Is(err, store.ErrNotFound):
			log.Warn("External bearer rejected: forbidden", "error", err)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"access denied", nil)
			return true
		case errors.Is(err, ErrGoogleUpstreamError):
			log.Warn("External bearer: upstream verification unavailable", "error", err)
			writeError(w, http.StatusServiceUnavailable, "upstream_unavailable",
				"external identity provider unavailable", nil)
			return true
		case errors.Is(err, errExternalBearerResolveFailed):
			log.Error("External bearer: internal resolver error", "error", err)
			writeError(w, http.StatusServiceUnavailable, "store_error",
				"unable to verify user status", nil)
			return true
		default:
			// The token targeted a trusted issuer but failed verification or
			// policy (bad signature, wrong audience, unverified email, service
			// account in this phase, ...). Log the reason; the response never
			// leaks which specific check failed.
			log.Info("External bearer rejected", "error", err)
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"invalid external bearer token", nil)
			return true
		}
	}

	ctx = context.WithValue(ctx, userContextKey{}, user)
	ctx = contextWithIdentity(ctx, user)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(user))
	ctx = contextWithAuthType(ctx, AuthTypeExternalBearer)
	if cfg.Debug {
		log.Debug("External bearer authenticated", "email", user.Email(), "user_id", user.ID())
	}
	next.ServeHTTP(w, r.WithContext(ctx))
	return true
}

// authenticateExternalBearer verifies token against the Google trust
// configuration and resolves the verified identity to a Hub user. It returns
// errExternalBearerNotApplicable when the token cannot be attributed to
// Google trust at all; other errors describe a token that targeted Google
// trust but failed verification or policy.
//
// Phase 1 scope only: ID tokens for USER principals. A service-account
// identity is rejected outright (SA support is a later phase) rather than
// silently admitted.
func authenticateExternalBearer(ctx context.Context, token string, cfg AuthConfig) (UserIdentity, error) {
	trust, ok := googleTrust(cfg)
	if !ok {
		return nil, errExternalBearerNotApplicable
	}
	if classifyExternalBearer(token) != externalBearerIDToken {
		return nil, errExternalBearerNotApplicable
	}
	if cfg.GoogleValidator == nil || cfg.GoogleResolver == nil {
		return nil, errExternalBearerNotApplicable
	}

	id, err := cfg.GoogleValidator.ValidateIDToken(ctx, token, []string{trust.ExpectedAudience})
	if err != nil {
		return nil, fmt.Errorf("external bearer: %w", err)
	}
	if id.IsServiceAccount {
		// No SA branch in this phase (design §5 Phase 1): reject explicitly
		// rather than falling through to user resolution.
		return nil, errExternalBearerPrincipalRejected
	}

	u, err := cfg.GoogleResolver.Resolve(ctx, id, ResolvePolicy{})
	if err != nil {
		return nil, classifyResolveError(err)
	}
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)), nil
}

// classifyResolveError always wraps a Resolve error with
// errExternalBearerResolveFailed via a second %w, so the error keeps
// matching both that sentinel and its original chain (errors.Is unwraps a
// %w-tree regardless of which branch a target sits on; verified this holds
// for two %w verbs in one fmt.Errorf call). serveExternalBearer's specific
// 403 arms are checked before the errExternalBearerResolveFailed arm, so
// they still win for the outcomes they name — this wrap only guarantees that
// everything else lands on 503, never the 401 default reserved for
// validator/principal-policy failures.
//
// r3 review optional finding 2: an earlier version of this function
// allowlisted the specific-outcome sentinels and left everything else
// unwrapped, which worked only because it duplicated serveExternalBearer's
// own list — a future sentinel added to one list but not the other would
// have silently misrouted a resolver fault to 401. Always wrapping removes
// that duplication and that risk.
func classifyResolveError(err error) error {
	return fmt.Errorf("%w: %w", errExternalBearerResolveFailed, err)
}
