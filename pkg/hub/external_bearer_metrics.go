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

// Observability for the external-bearer path, its credential cache, and the
// GE exchange endpoint it is replacing (design §4.7). Every label used below
// comes from a closed set, defined as constants here: nothing in
// auth_external_bearer.go, google_credential_cache.go or ge_exchange.go
// builds a label value any other way, so a value outside these sets can
// never be emitted.
//
// "external_bearer", "google_validator_cache" and "ge_exchange.requests" are
// logical names for these three counters, used throughout this design's
// comments and tests. See otel_external_bearer_metrics.go for the real,
// exported Cloud Monitoring metric types, and external_bearer_snapshot_metrics.go
// for the in-process /metrics section that exists regardless of GCP export
// configuration.
//
// Recorders are wired into Server (see server.go's New, and the Set*Metrics
// methods) the same way every other Hub OTel-backed recorder is: a plain,
// nil-safe interface field or method, upgraded from "disabled" to a real
// exporter once a MeterProvider exists (cmd/server_foreground.go). A nil
// recorder — or a nil value inside one, where the field is an
// *atomic.Pointer — only disables counting; it never changes what the
// underlying path does.

// ExternalBearerKind is the "kind" label on the external_bearer counter: the
// shape of the presented credential, as classified by classifyExternalBearer.
// ExternalBearerKindUnknown is used whenever a request is rejected before
// classification succeeds (e.g. no Google trust configured), per the
// design's closed-label-set rule.
type ExternalBearerKind string

const (
	ExternalBearerKindIDToken     ExternalBearerKind = "id_token"
	ExternalBearerKindAccessToken ExternalBearerKind = "access_token"
	ExternalBearerKindUnknown     ExternalBearerKind = "unknown"
)

// externalBearerKinds returns every valid ExternalBearerKind, in a fixed
// order, for use by valid() and by any code that needs a deterministic,
// complete enumeration (e.g. the /metrics snapshot).
func externalBearerKinds() []ExternalBearerKind {
	return []ExternalBearerKind{
		ExternalBearerKindIDToken,
		ExternalBearerKindAccessToken,
		ExternalBearerKindUnknown,
	}
}

// valid reports whether k is one of the named constants above — the closed
// set every ExternalBearerKind value must belong to.
func (k ExternalBearerKind) valid() bool {
	for _, v := range externalBearerKinds() {
		if v == k {
			return true
		}
	}
	return false
}

// ExternalBearerPrincipal is the "principal" label on the external_bearer
// counter. ExternalBearerPrincipalUnknown is used whenever a request is
// rejected before the validated identity's IsServiceAccount is known (e.g.
// verification itself failed).
type ExternalBearerPrincipal string

const (
	ExternalBearerPrincipalUser           ExternalBearerPrincipal = "user"
	ExternalBearerPrincipalServiceAccount ExternalBearerPrincipal = "service_account"
	ExternalBearerPrincipalUnknown        ExternalBearerPrincipal = "unknown"
)

// externalBearerPrincipals returns every valid ExternalBearerPrincipal, in a
// fixed order.
func externalBearerPrincipals() []ExternalBearerPrincipal {
	return []ExternalBearerPrincipal{
		ExternalBearerPrincipalUser,
		ExternalBearerPrincipalServiceAccount,
		ExternalBearerPrincipalUnknown,
	}
}

// valid reports whether p is one of the named constants above.
func (p ExternalBearerPrincipal) valid() bool {
	for _, v := range externalBearerPrincipals() {
		if v == p {
			return true
		}
	}
	return false
}

// ExternalBearerOutcome is the "outcome" label on the external_bearer
// counter. Each value maps 1:1 to a row of design §4.4's status table;
// serveExternalBearer's outcome switch is the single place that performs
// this mapping (see auth_external_bearer.go), so the metric can never
// diverge from the HTTP status it accompanies.
type ExternalBearerOutcome string

const (
	// ExternalBearerOutcomeOK is a successfully authenticated request.
	ExternalBearerOutcomeOK ExternalBearerOutcome = "ok"
	// ExternalBearerOutcomeNotApplicable is a request the path does not
	// vouch for at all; the caller falls through to its original,
	// byte-identical rejection.
	ExternalBearerOutcomeNotApplicable ExternalBearerOutcome = "not_applicable"
	// ExternalBearerOutcomeRejected is a 401: verification failure, an SA
	// access token, or an SA project or user email domain not on its
	// respective allowlist.
	ExternalBearerOutcomeRejected ExternalBearerOutcome = "rejected"
	// ExternalBearerOutcomeRateLimited is a 429 from the per-client-IP
	// limiter, consulted only on a credential-cache miss.
	ExternalBearerOutcomeRateLimited ExternalBearerOutcome = "rate_limited"
	// ExternalBearerOutcomeUpstreamError is a 503 upstream_unavailable:
	// ErrGoogleUpstreamError from the validator (Google itself is down or
	// erroring), distinct from ExternalBearerOutcomeStoreError below.
	ExternalBearerOutcomeUpstreamError ExternalBearerOutcome = "upstream_error"
	// ExternalBearerOutcomeSuspended is a 403 user_suspended.
	ExternalBearerOutcomeSuspended ExternalBearerOutcome = "suspended"
	// ExternalBearerOutcomeForbidden is a 403: ErrAccessDenied, a
	// non-authoritative email, a binding conflict, or a Resolve error
	// wrapping store.ErrNotFound (a binding pointing at a deleted user —
	// permanent, so it is forbidden rather than a retryable store fault).
	ExternalBearerOutcomeForbidden ExternalBearerOutcome = "forbidden"
	// ExternalBearerOutcomeStoreError is a 503 store_error: any other
	// Resolve fault (lookup/create faults on the binding or user stores).
	// Kept distinct from ExternalBearerOutcomeUpstreamError: one means
	// Google is unavailable, the other means the Hub's own store is, and
	// the two need different alerts.
	ExternalBearerOutcomeStoreError ExternalBearerOutcome = "store_error"
)

// externalBearerOutcomes returns every valid ExternalBearerOutcome, in a
// fixed order.
func externalBearerOutcomes() []ExternalBearerOutcome {
	return []ExternalBearerOutcome{
		ExternalBearerOutcomeOK,
		ExternalBearerOutcomeNotApplicable,
		ExternalBearerOutcomeRejected,
		ExternalBearerOutcomeRateLimited,
		ExternalBearerOutcomeUpstreamError,
		ExternalBearerOutcomeSuspended,
		ExternalBearerOutcomeForbidden,
		ExternalBearerOutcomeStoreError,
	}
}

// valid reports whether o is one of the named constants above.
func (o ExternalBearerOutcome) valid() bool {
	for _, v := range externalBearerOutcomes() {
		if v == o {
			return true
		}
	}
	return false
}

// ExternalBearerMetricsRecorder records the outcome of every external-bearer
// authentication attempt. Implementations must be safe for concurrent use.
type ExternalBearerMetricsRecorder interface {
	RecordExternalBearer(kind ExternalBearerKind, principal ExternalBearerPrincipal, outcome ExternalBearerOutcome)
}

// GoogleValidatorCacheResult is the "result" label on the
// google_validator_cache counter, recorded by the caching decorator
// (google_credential_cache.go) on every ValidateIDToken/ValidateAccessToken
// call.
type GoogleValidatorCacheResult string

const (
	// GoogleValidatorCacheHit is a live, successful cache entry.
	GoogleValidatorCacheHit GoogleValidatorCacheResult = "hit"
	// GoogleValidatorCacheMiss is a request not served from cache (the
	// decorator found no live entry, cached or not, for this key), including
	// a singleflight follower collapsed into another caller's in-flight
	// upstream call. The count of actual upstream calls is therefore
	// bounded above by, but not equal to, the miss count.
	GoogleValidatorCacheMiss GoogleValidatorCacheResult = "miss"
	// GoogleValidatorCacheNegativeHit is a live, negatively cached entry
	// (one of the four errors design §4.2(iii) allows to be cached
	// negatively).
	GoogleValidatorCacheNegativeHit GoogleValidatorCacheResult = "negative_hit"
)

// googleValidatorCacheResults returns every valid GoogleValidatorCacheResult,
// in a fixed order.
func googleValidatorCacheResults() []GoogleValidatorCacheResult {
	return []GoogleValidatorCacheResult{
		GoogleValidatorCacheHit,
		GoogleValidatorCacheMiss,
		GoogleValidatorCacheNegativeHit,
	}
}

// valid reports whether r is one of the named constants above.
func (r GoogleValidatorCacheResult) valid() bool {
	for _, v := range googleValidatorCacheResults() {
		if v == r {
			return true
		}
	}
	return false
}

// GoogleValidatorCacheMetricsRecorder records a single cache lookup outcome.
// Implementations must be safe for concurrent use.
type GoogleValidatorCacheMetricsRecorder interface {
	RecordGoogleValidatorCache(result GoogleValidatorCacheResult)
}

// GEExchangeOutcome is the "outcome" label on the ge_exchange.requests
// counter, recorded by handleGEGoogleExchange for every POST request — the
// endpoint's only routable method; a GET/HEAD gets a 405 and is not counted.
// The set is exactly the response codes the handler's POST path can already
// produce (design §4.7: "a bounded outcome set derived from its existing
// responses") — recording this metric never changes, and is never allowed
// to change, the exchange response's bytes.
//
// This counter is the exchange-deletion soak gate (design §4.7, §5): it must
// read zero, on every Hub replica, over the soak window before
// ge_exchange.go and its callers are deleted. Because the /metrics snapshot
// is per-process and resets on restart, checking a single sample after a
// restart is not evidence of zero traffic; the check must cover the full
// window, either via Cloud Monitoring's exported series (summed over the
// window) or via repeated /metrics samples from every replica.
type GEExchangeOutcome string

const (
	GEExchangeOutcomeOK                GEExchangeOutcome = "ok"
	GEExchangeOutcomeRateLimited       GEExchangeOutcome = "rate_limited"
	GEExchangeOutcomeNotConfigured     GEExchangeOutcome = "not_configured"
	GEExchangeOutcomeInvalidRequest    GEExchangeOutcome = "invalid_request"
	GEExchangeOutcomeBadRequest        GEExchangeOutcome = "bad_request"
	GEExchangeOutcomeInvalidCredential GEExchangeOutcome = "invalid_credential"
	GEExchangeOutcomeForbidden         GEExchangeOutcome = "forbidden"
	GEExchangeOutcomeExchangeFailed    GEExchangeOutcome = "exchange_failed"
)

// geExchangeOutcomes returns every valid GEExchangeOutcome, in a fixed
// order.
func geExchangeOutcomes() []GEExchangeOutcome {
	return []GEExchangeOutcome{
		GEExchangeOutcomeOK,
		GEExchangeOutcomeRateLimited,
		GEExchangeOutcomeNotConfigured,
		GEExchangeOutcomeInvalidRequest,
		GEExchangeOutcomeBadRequest,
		GEExchangeOutcomeInvalidCredential,
		GEExchangeOutcomeForbidden,
		GEExchangeOutcomeExchangeFailed,
	}
}

// valid reports whether o is one of the named constants above.
func (o GEExchangeOutcome) valid() bool {
	for _, v := range geExchangeOutcomes() {
		if v == o {
			return true
		}
	}
	return false
}

// GEExchangeMetricsRecorder records a single exchange request's outcome.
// Implementations must be safe for concurrent use.
type GEExchangeMetricsRecorder interface {
	RecordGEExchangeRequest(outcome GEExchangeOutcome)
}
