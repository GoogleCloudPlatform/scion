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
// trip: user ID tokens and access tokens, and service-account ID tokens
// gated by allowed_gcp_projects. A service-account access token is never
// accepted.
//
// serveExternalBearer runs only after the request has already failed every
// other authentication path (Hub JWT, PAT, agent, proxy, federation). It is
// therefore load-bearing that it never changes the outcome of a request this
// path cannot vouch for: it reports errExternalBearerNotApplicable and the
// caller emits its original, byte-identical rejection.
//
// A verified identity is resolved to a Hub user through the same
// GoogleIdentityResolver the GE exchange endpoint uses (ge_exchange.go,
// google_identity_resolver.go), so both mechanisms reach identical decisions
// for the same Google identity during the soak between them.

// errExternalBearerNotApplicable reports that the token is not something the
// external-bearer path can vouch for (no Google trust configured, or a JWT
// whose issuer is missing or is not Google). Callers fall back to their
// normal rejection message.
var errExternalBearerNotApplicable = errors.New("external bearer: not applicable")

// errSAAccessTokenRejected reports that a service-account identity was
// presented as an OAuth2 access token rather than an ID token. Only SA ID
// tokens validate (azp/sub-bound audience rule); access tokens carry no
// equivalent binding, so an SA is never admitted this way, on any project.
var errSAAccessTokenRejected = errors.New("external bearer: service account access tokens are not accepted")

// errSAProjectNotAllowed reports that a service-account ID token's GCP
// project — parsed from its verified email by googleSAProject — is not
// listed in the Google issuer's allowed_gcp_projects. An unparseable email
// (googleSAProject's second return false) and an unset allowed_gcp_projects
// (admits no service accounts at all) both take this path.
var errSAProjectNotAllowed = errors.New("external bearer: service account project not allowed")

// errDomainNotAllowed reports that a user identity's verified email domain
// is not listed in the Google issuer's allowed_domains. Applies only to a
// non-service-account (user) principal; a service account's admission is
// governed by allowed_gcp_projects instead, never by this check. An unset
// allowed_domains means no issuer-level domain constraint — the Hub sign-in
// policy is the only gate in that case.
var errDomainNotAllowed = errors.New("external bearer: email domain not allowed")

// errExternalBearerRateLimited reports that the external-bearer path's
// per-client-IP budget (externalBearerRateLimiter, external_bearer_ratelimit.go)
// was exhausted on a credential-cache miss. Wrapped by
// *externalBearerRateLimitError so serveExternalBearer can recover the
// Retry-After value with errors.As while still matching this sentinel with
// errors.Is.
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
// signature, wrong audience, SA access token, SA project or user domain not
// allowed, ...) is a credential rejection (401); anything from Resolve is
// either a known policy outcome or an internal/store fault (403/503) —
// never "invalid token". See classifyResolveError below for why every
// Resolve error is wrapped rather than allowlisted.
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
	// garbage — classification alone cannot tell them apart. What makes this
	// safe is where classifyExternalBearer is called from:
	// authenticateExternalBearer only reaches it after googleTrust has
	// already confirmed Google trust is configured. When it is not, the
	// caller never calls this function at all, so a non-JWT token still
	// falls through to the original rejection untouched. Verification —
	// which is where a garbage token actually gets rejected — happens in
	// cfg.GoogleValidator.ValidateAccessToken.
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

// metricLabel maps the internal classification enum to the closed-set label
// value the external_bearer counter's "kind" uses (external_bearer_metrics.go).
// externalBearerNotApplicable maps to ExternalBearerKindUnknown: a request
// that classifies as not-applicable never becomes a candidate id_token/
// access_token as far as the metric is concerned.
func (k externalBearerKind) metricLabel() ExternalBearerKind {
	switch k {
	case externalBearerIDToken:
		return ExternalBearerKindIDToken
	case externalBearerAccessToken:
		return ExternalBearerKindAccessToken
	default:
		return ExternalBearerKindUnknown
	}
}

// externalBearerAttempt carries the kind/principal label values
// authenticateExternalBearer has discovered so far, threaded alongside its
// (UserIdentity, error) return so serveExternalBearer's outcome switch —
// already the single source of truth for the HTTP status this path returns
// — is also the single source of truth for the "outcome" label:
// duplicating that switch's classification a second time, here, would
// risk the metric and the HTTP response falling out of sync. Both labels
// start unknown and are only ever set forward (never reset): a rejection
// before classification succeeds, or before the validated identity's
// IsServiceAccount is known, records unknown rather than guessing.
type externalBearerAttempt struct {
	kind      ExternalBearerKind
	principal ExternalBearerPrincipal
}

// recordExternalBearer records one external-bearer outcome, nil-safe against
// every disabled state: cfg.ExternalBearerMetrics itself nil (never wired,
// e.g. most hand-built AuthConfigs in tests), the pointer wired but never
// Store()d, or Store()d with a nil interface (defensive). It also enforces
// the closed label set at this single boundary: a label that fails its
// valid() check is dropped (with a warning naming only the label and its
// type, never the value) rather than reaching any recorder, so "no other
// value can be emitted" holds even for a future call site that builds one
// from an untyped string literal instead of a named constant.
func recordExternalBearer(cfg AuthConfig, attempt externalBearerAttempt, outcome ExternalBearerOutcome) {
	switch {
	case !attempt.kind.valid():
		slog.Warn("external bearer: dropping metric record: invalid label", "label", "kind", "type", fmt.Sprintf("%T", attempt.kind))
		return
	case !attempt.principal.valid():
		slog.Warn("external bearer: dropping metric record: invalid label", "label", "principal", "type", fmt.Sprintf("%T", attempt.principal))
		return
	case !outcome.valid():
		slog.Warn("external bearer: dropping metric record: invalid label", "label", "outcome", "type", fmt.Sprintf("%T", outcome))
		return
	}
	if cfg.ExternalBearerMetrics == nil {
		return
	}
	rec := cfg.ExternalBearerMetrics.Load()
	if rec == nil || *rec == nil {
		return
	}
	(*rec).RecordExternalBearer(attempt.kind, attempt.principal, outcome)
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

// containsFold reports whether target is present in list, compared
// case-insensitively. Used for both the allowed_gcp_projects membership
// check (trust.AllowedGCPProjects — a distinct field from the unrelated,
// existing AllowedProjects/allowed_projects, hub-federation project scoping
// by JWT project_id claim, federation_auth.go's IssuerTypeHub case; the two
// must not be confused) and the allowed_domains membership check
// (trust.AllowedDomains). Neither list is rewritten at config load, so this
// case-insensitive comparison is what makes a mixed-case operator entry
// match googleSAProject's (always lower-case) parsed project, or domainOf's
// (always lower-case) parsed domain.
func containsFold(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}

// domainOf extracts the domain from a verified email address, for the
// allowed_domains membership check. It fails closed (ok=false) on any shape
// a naive last-"@" split could misread: no "@" at all, more than one "@", or
// an empty local or domain part. A trailing dot on the domain also fails
// closed rather than being silently stripped — this function decides a
// security check, not a display string, so an unusual shape is treated as
// "cannot confidently say what domain this is" rather than guessed at. The
// domain is lower-cased on success, matching containsFold's case-insensitive
// comparison.
func domainOf(email string) (domain string, ok bool) {
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	if strings.HasSuffix(parts[1], ".") {
		return "", false
	}
	return strings.ToLower(parts[1]), true
}

// serveExternalBearer attempts to authenticate the request with an external
// bearer token. It returns true when it has written a response or served the
// request, and false when the token is not an external bearer token the Hub
// can vouch for (the caller then emits its usual rejection).
func serveExternalBearer(w http.ResponseWriter, r *http.Request, next http.Handler,
	ctx context.Context, token string, cfg AuthConfig, log *slog.Logger) bool {

	user, attempt, err := authenticateExternalBearer(ctx, r, token, cfg)
	if err != nil {
		var rlErr *externalBearerRateLimitError
		switch {
		case errors.Is(err, errExternalBearerNotApplicable):
			recordExternalBearer(cfg, attempt, ExternalBearerOutcomeNotApplicable)
			if cfg.Debug {
				log.Debug("External bearer not applicable", "error", err)
			}
			return false
		case errors.As(err, &rlErr):
			recordExternalBearer(cfg, attempt, ExternalBearerOutcomeRateLimited)
			log.Info("External bearer rate limited", "retry_after_seconds", rlErr.retryAfterSeconds)
			w.Header().Set("Retry-After", strconv.Itoa(rlErr.retryAfterSeconds))
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
				"rate limit exceeded", nil)
			return true
		case errors.Is(err, ErrUserSuspended):
			recordExternalBearer(cfg, attempt, ExternalBearerOutcomeSuspended)
			log.Warn("External bearer rejected: user is suspended", "error", err)
			writeError(w, http.StatusForbidden, "user_suspended",
				"access denied: user account is suspended", nil)
			return true
		case errors.Is(err, ErrAccessDenied),
			errors.Is(err, errNonAuthoritativeEmail),
			errors.Is(err, errBindingConflict),
			errors.Is(err, errAmbiguousLinkage),
			errors.Is(err, store.ErrNotFound):
			recordExternalBearer(cfg, attempt, ExternalBearerOutcomeForbidden)
			log.Warn("External bearer rejected: forbidden", "error", err)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"access denied", nil)
			return true
		case errors.Is(err, ErrGoogleUpstreamError):
			recordExternalBearer(cfg, attempt, ExternalBearerOutcomeUpstreamError)
			log.Warn("External bearer: upstream verification unavailable", "error", err)
			writeError(w, http.StatusServiceUnavailable, "upstream_unavailable",
				"external identity provider unavailable", nil)
			return true
		case errors.Is(err, errExternalBearerResolveFailed):
			recordExternalBearer(cfg, attempt, ExternalBearerOutcomeStoreError)
			log.Error("External bearer: internal resolver error", "error", err)
			writeError(w, http.StatusServiceUnavailable, "store_error",
				"unable to verify user status", nil)
			return true
		default:
			// The token targeted a trusted issuer but failed verification or
			// policy (bad signature, wrong audience, unverified email, SA
			// access token, SA project or user domain not allowed, ...). Log
			// the reason; the response never leaks which specific check
			// failed.
			recordExternalBearer(cfg, attempt, ExternalBearerOutcomeRejected)
			log.Info("External bearer rejected", "error", err)
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"invalid external bearer token", nil)
			return true
		}
	}
	recordExternalBearer(cfg, attempt, ExternalBearerOutcomeOK)

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
// treated as a cache miss for rate-limiting purposes.
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
// allowed_gcp_projects; an SA access token is rejected on every project. A
// user identity is unaffected by any of this; instead, if the issuer's
// allowed_domains is non-empty, the verified email's domain must be listed
// there — a check that never applies to a service account.
func authenticateExternalBearer(ctx context.Context, r *http.Request, token string, cfg AuthConfig) (UserIdentity, externalBearerAttempt, error) {
	attempt := externalBearerAttempt{kind: ExternalBearerKindUnknown, principal: ExternalBearerPrincipalUnknown}

	trust, ok := googleTrust(cfg)
	if !ok {
		return nil, attempt, errExternalBearerNotApplicable
	}
	kind := classifyExternalBearer(token)
	if kind == externalBearerNotApplicable {
		return nil, attempt, errExternalBearerNotApplicable
	}
	if cfg.GoogleValidator == nil || cfg.GoogleResolver == nil {
		return nil, attempt, errExternalBearerNotApplicable
	}
	attempt.kind = kind.metricLabel()

	aud := []string{trust.ExpectedAudience}

	// Rate limit — consulted only on a credential-cache miss: a cache hit
	// costs no upstream call, so it must not spend budget that a genuine
	// burst of distinct garbage tokens needs. cfg.ExternalBearerLimiter is
	// nil in tests that don't wire one (and in any config that never built
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
				return nil, attempt, &externalBearerRateLimitError{retryAfterSeconds: retryAfter}
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
		return nil, attempt, fmt.Errorf("external bearer: %w", err)
	}
	if id.IsServiceAccount {
		attempt.principal = ExternalBearerPrincipalServiceAccount
	} else {
		attempt.principal = ExternalBearerPrincipalUser
	}

	policy := ResolvePolicy{}
	if id.IsServiceAccount {
		if kind == externalBearerAccessToken {
			return nil, attempt, errSAAccessTokenRejected
		}
		proj, ok := googleSAProject(id.Email)
		if !ok || !containsFold(trust.AllowedGCPProjects, proj) {
			return nil, attempt, errSAProjectNotAllowed
		}
		// The project allowlist IS the authorization decision for a
		// first-time provision (ResolvePolicy.PreAuthorized): it never
		// bypasses the suspension check on an already-bound SA user, which
		// Resolve enforces unconditionally.
		policy.PreAuthorized = true
	} else if len(trust.AllowedDomains) > 0 {
		// A user principal (never a service account, which took the branch
		// above): reject before Resolve if the verified email's domain isn't
		// listed. The Hub sign-in policy inside Resolve still runs
		// afterwards for every user, listed domain or not — this check
		// only narrows which domains reach that policy at all.
		domain, ok := domainOf(id.Email)
		if !ok || !containsFold(trust.AllowedDomains, domain) {
			return nil, attempt, errDomainNotAllowed
		}
	}

	u, err := cfg.GoogleResolver.Resolve(ctx, id, policy)
	if err != nil {
		return nil, attempt, classifyResolveError(err)
	}
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)), attempt, nil
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
// Wrapping unconditionally, rather than allowlisting the specific-outcome
// sentinels and leaving everything else unwrapped, avoids duplicating
// serveExternalBearer's own list: a future sentinel added to one list but
// not the other would otherwise silently misroute a resolver fault to 401.
func classifyResolveError(err error) error {
	return fmt.Errorf("%w: %w", errExternalBearerResolveFailed, err)
}
