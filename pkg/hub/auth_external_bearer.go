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
	"strconv"
	"strings"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// External bearer authentication.
//
// The Hub accepts a Google-issued end-user or service-account credential
// directly in Authorization: Bearer, without a credential-exchange round
// trip: user ID tokens and access tokens (design §5 Phases 1-2), and
// service-account ID tokens gated by allowed_gcp_projects (design §5 Phase 3).
// A service-account access token is never accepted, on any phase.
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

// errSAAccessTokenRejected reports that a service-account identity was
// presented as an OAuth2 access token rather than an ID token. Design §4.2(ii)
// only makes SA ID tokens validate (azp/sub-bound audience rule); access
// tokens carry no equivalent binding, so an SA is never admitted this way, on
// any project. Renamed from errExternalBearerPrincipalRejected (Phase 2),
// which rejected every SA identity outright before Phase 3 gave ID tokens an
// admission policy.
var errSAAccessTokenRejected = errors.New("external bearer: service account access tokens are not accepted")

// errSAProjectNotAllowed reports that a service-account ID token's GCP
// project — parsed from its verified email by googleSAProject — is not
// listed in the Google issuer's allowed_gcp_projects (design §4.1, §4.4). An
// unparseable email (googleSAProject's second return false) and an unset
// allowed_gcp_projects (admits no service accounts at all) both take this path.
var errSAProjectNotAllowed = errors.New("external bearer: service account project not allowed")

// errExternalBearerRateLimited reports that the external-bearer path's
// per-client-IP budget (externalBearerRateLimiter, external_bearer_ratelimit.go)
// was exhausted on a credential-cache miss. Wrapped by
// *externalBearerRateLimitError so serveExternalBearer can recover the
// Retry-After value with errors.As while still matching this sentinel with
// errors.Is (design §4.4).
var errExternalBearerRateLimited = errors.New("external bearer: rate limited")

// externalBearerRateLimitError carries the computed Retry-After duration for
// a rate-limited request. It is a distinct concrete type (not a second
// sentinel) purely so the numeric retryAfterSeconds can travel with the
// error; errors.Is(err, errExternalBearerRateLimited) still holds via Unwrap.
type externalBearerRateLimitError struct {
	retryAfterSeconds int
}

func (e *externalBearerRateLimitError) Error() string { return errExternalBearerRateLimited.Error() }
func (e *externalBearerRateLimitError) Unwrap() error { return errExternalBearerRateLimited }

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
	// externalBearerAccessToken is any non-JWT token. Google OAuth2 access
	// tokens are opaque, so there is no shape to distinguish them from
	// garbage — classification alone cannot tell them apart (design §4.4:
	// "classification without prefix sniff"). What makes this safe is where
	// classifyExternalBearer is called from: authenticateExternalBearer only
	// reaches it after googleTrust has already confirmed Google trust is
	// configured. When it is not, the caller never calls this function at
	// all, so a non-JWT token still falls through to the original rejection
	// untouched (I1). Verification — which is where a garbage token actually
	// gets rejected — happens in cfg.GoogleValidator.ValidateAccessToken.
	externalBearerAccessToken
)

// classifyExternalBearer classifies token for routing purposes only, by shape
// alone. It reads a JWT's unverified iss claim — cryptographic verification
// happens later, in cfg.GoogleValidator — and otherwise treats any non-JWT
// token as a candidate access token. See externalBearerAccessToken's doc
// comment for why classifying every non-JWT token this way is still safe.
func classifyExternalBearer(token string) externalBearerKind {
	if !looksLikeJWT(token) {
		return externalBearerAccessToken
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

// trustAllowedProjects returns the GCP project IDs a service-account
// identity's project (googleSAProject) must match for trust to admit it
// (design §4.1, allowed_gcp_projects). Isolated behind this accessor, rather
// than reading trust.AllowedGCPProjects directly at the one call site, to
// keep the SA branch's coupling to the exact pkg/config field name in one
// place — TrustedIssuerConfig also has an unrelated, longer-standing
// AllowedProjects field (allowed_projects: hub-federation project scoping by
// JWT project_id claim, federation_auth.go's IssuerTypeHub case); the two
// are deliberately distinct fields (design §4.1 r7) and must not be confused.
func trustAllowedProjects(trust config.TrustedIssuerConfig) []string {
	return trust.AllowedGCPProjects
}

// containsFold reports whether target is present in list, compared
// case-insensitively. Used for the allowed_gcp_projects membership check:
// the Google-issuer entry's list is normalised to lower case at config load
// (design §4.1), but googleSAProject's parsed project is compared
// case-insensitively regardless, so this does not depend on that
// normalisation actually having run.
func containsFold(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}

// serveExternalBearer attempts to authenticate the request with an external
// bearer token. It returns true when it has written a response or served the
// request, and false when the token is not an external bearer token the Hub
// can vouch for (the caller then emits its usual rejection).
func serveExternalBearer(w http.ResponseWriter, r *http.Request, next http.Handler,
	ctx context.Context, token string, cfg AuthConfig, log *slog.Logger) bool {

	user, err := authenticateExternalBearer(ctx, r, token, cfg)
	if err != nil {
		var rlErr *externalBearerRateLimitError
		switch {
		case errors.Is(err, errExternalBearerNotApplicable):
			if cfg.Debug {
				log.Debug("External bearer not applicable", "error", err)
			}
			return false
		case errors.As(err, &rlErr):
			log.Info("External bearer rate limited", "retry_after_seconds", rlErr.retryAfterSeconds)
			w.Header().Set("Retry-After", strconv.Itoa(rlErr.retryAfterSeconds))
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
				"rate limit exceeded", nil)
			return true
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

// externalBearerCacheProbe is implemented by a GoogleCredentialValidator that
// can report whether a given (token, allowedClientIDs) pair is already
// cached (currently: *cachingGoogleCredentialValidator, google_credential_cache.go).
// authenticateExternalBearer type-asserts cfg.GoogleValidator against this
// interface — rather than adding Cached to the GoogleCredentialValidator
// interface itself — so plain (uncached) validators, including every test
// fake, are unaffected: they simply don't implement it, and are always
// treated as a cache miss for rate-limiting purposes (design §4.4).
type externalBearerCacheProbe interface {
	Cached(token string, allowedClientIDs []string) bool
}

// authenticateExternalBearer verifies token against the Google trust
// configuration and resolves the verified identity to a Hub user. It returns
// errExternalBearerNotApplicable when the token cannot be attributed to
// Google trust at all; other errors describe a token that targeted Google
// trust but failed verification or policy.
//
// A service-account identity is admitted only as an ID token whose GCP
// project (parsed from its verified email) is listed in the Google issuer's
// allowed_gcp_projects (design §4.1, §4.4); an SA access token is rejected on
// every project. A user identity is unaffected by any of this.
func authenticateExternalBearer(ctx context.Context, r *http.Request, token string, cfg AuthConfig) (UserIdentity, error) {
	trust, ok := googleTrust(cfg)
	if !ok {
		return nil, errExternalBearerNotApplicable
	}
	kind := classifyExternalBearer(token)
	if kind == externalBearerNotApplicable {
		return nil, errExternalBearerNotApplicable
	}
	if cfg.GoogleValidator == nil || cfg.GoogleResolver == nil {
		return nil, errExternalBearerNotApplicable
	}

	aud := []string{trust.ExpectedAudience}

	// Rate limit — consulted only on a credential-cache miss (design §4.4,
	// C5): a cache hit costs no upstream call, so it must not spend budget
	// that a genuine burst of distinct garbage tokens needs. cfg.ExternalBearerLimiter
	// is nil in tests that don't wire one (and in any config that never built
	// one), in which case the path is simply unlimited — the limiter's
	// presence is a production-wiring concern (server.go), not a correctness
	// requirement for the paths that don't set it.
	if cfg.ExternalBearerLimiter != nil {
		cached := false
		if probe, ok := cfg.GoogleValidator.(externalBearerCacheProbe); ok {
			cached = probe.Cached(token, aud)
		}
		if !cached {
			if allowed, retryAfter := cfg.ExternalBearerLimiter.Allow(r); !allowed {
				return nil, &externalBearerRateLimitError{retryAfterSeconds: retryAfter}
			}
		}
	}

	var id *ValidatedGoogleIdentity
	var err error
	switch kind {
	case externalBearerIDToken:
		id, err = cfg.GoogleValidator.ValidateIDToken(ctx, token, aud)
	case externalBearerAccessToken:
		id, err = cfg.GoogleValidator.ValidateAccessToken(ctx, token, aud)
	}
	if err != nil {
		return nil, fmt.Errorf("external bearer: %w", err)
	}
	policy := ResolvePolicy{}
	if id.IsServiceAccount {
		if kind == externalBearerAccessToken {
			return nil, errSAAccessTokenRejected
		}
		proj, ok := googleSAProject(id.Email)
		if !ok || !containsFold(trustAllowedProjects(trust), proj) {
			return nil, errSAProjectNotAllowed
		}
		// The project allowlist IS the authorization decision for a
		// first-time provision (design §4.3's ResolvePolicy.PreAuthorized):
		// it never bypasses the suspension check on an already-bound SA
		// user (S6), which Resolve enforces unconditionally.
		policy.PreAuthorized = true
	}

	u, err := cfg.GoogleResolver.Resolve(ctx, id, policy)
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
