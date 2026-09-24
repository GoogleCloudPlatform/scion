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
// Recorders are wired into Server (see server.go's New, and the Set*Metrics
// methods) the same way every other Hub OTel-backed recorder is: a plain,
// nil-safe interface field or method, upgraded from "disabled" to a real
// exporter once a MeterProvider exists (cmd/server_foreground.go). A nil
// recorder — or a nil value inside one, where the field is an
// *atomic.Pointer — only disables counting; it never changes what the
// underlying path does.

// ExternalBearerKind is the "kind" label on scion_hub_external_bearer_total:
// the shape of the presented credential, as classified by
// classifyExternalBearer. ExternalBearerKindUnknown is used whenever a
// request is rejected before classification succeeds (e.g. no Google trust
// configured), per the design's closed-label-set rule.
type ExternalBearerKind string

const (
	ExternalBearerKindIDToken     ExternalBearerKind = "id_token"
	ExternalBearerKindAccessToken ExternalBearerKind = "access_token"
	ExternalBearerKindUnknown     ExternalBearerKind = "unknown"
)

// ExternalBearerPrincipal is the "principal" label on
// scion_hub_external_bearer_total. ExternalBearerPrincipalUnknown is used
// whenever a request is rejected before the validated identity's
// IsServiceAccount is known (e.g. verification itself failed).
type ExternalBearerPrincipal string

const (
	ExternalBearerPrincipalUser           ExternalBearerPrincipal = "user"
	ExternalBearerPrincipalServiceAccount ExternalBearerPrincipal = "service_account"
	ExternalBearerPrincipalUnknown        ExternalBearerPrincipal = "unknown"
)

// ExternalBearerOutcome is the "outcome" label on
// scion_hub_external_bearer_total. Each value maps 1:1 to a row of design
// §4.4's status table; serveExternalBearer's outcome switch is the single
// place that performs this mapping (see auth_external_bearer.go), so the
// metric can never diverge from the HTTP status it accompanies.
type ExternalBearerOutcome string

const (
	// ExternalBearerOutcomeOK is a successfully authenticated request.
	ExternalBearerOutcomeOK ExternalBearerOutcome = "ok"
	// ExternalBearerOutcomeNotApplicable is a request the path does not
	// vouch for at all; the caller falls through to its original,
	// byte-identical rejection.
	ExternalBearerOutcomeNotApplicable ExternalBearerOutcome = "not_applicable"
	// ExternalBearerOutcomeRejected is a 401: verification failure, an SA
	// access token, or an SA project (or, from a later phase, a user
	// domain) not on the allowlist.
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
	// Kept distinct from ExternalBearerOutcomeUpstreamError because the two
	// need different alerts (lead ruling, design §4.7 r7).
	ExternalBearerOutcomeStoreError ExternalBearerOutcome = "store_error"
)

// ExternalBearerMetricsRecorder records the outcome of every external-bearer
// authentication attempt. Implementations must be safe for concurrent use.
type ExternalBearerMetricsRecorder interface {
	RecordExternalBearer(kind ExternalBearerKind, principal ExternalBearerPrincipal, outcome ExternalBearerOutcome)
}

// GoogleValidatorCacheResult is the "result" label on
// scion_hub_google_validator_cache_total, recorded by the caching decorator
// (google_credential_cache.go) on every ValidateIDToken/ValidateAccessToken
// call.
type GoogleValidatorCacheResult string

const (
	// GoogleValidatorCacheHit is a live, successful cache entry.
	GoogleValidatorCacheHit GoogleValidatorCacheResult = "hit"
	// GoogleValidatorCacheMiss required an upstream call (the decorator
	// found no live entry, cached or not, for this key).
	GoogleValidatorCacheMiss GoogleValidatorCacheResult = "miss"
	// GoogleValidatorCacheNegativeHit is a live, negatively cached entry
	// (one of the four errors design §4.2(iii) allows to be cached
	// negatively).
	GoogleValidatorCacheNegativeHit GoogleValidatorCacheResult = "negative_hit"
)

// GoogleValidatorCacheMetricsRecorder records a single cache lookup outcome.
// Implementations must be safe for concurrent use.
type GoogleValidatorCacheMetricsRecorder interface {
	RecordGoogleValidatorCache(result GoogleValidatorCacheResult)
}

// GEExchangeOutcome is the "outcome" label on
// scion_hub_ge_exchange_requests_total, recorded by handleGEGoogleExchange.
// The set is exactly the response codes that handler can already produce
// (design §4.7: "a bounded outcome set derived from its existing
// responses") — recording this metric never changes, and is never allowed
// to change, the exchange response's bytes.
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

// GEExchangeMetricsRecorder records a single exchange request's outcome.
// Implementations must be safe for concurrent use.
type GEExchangeMetricsRecorder interface {
	RecordGEExchangeRequest(outcome GEExchangeOutcome)
}
