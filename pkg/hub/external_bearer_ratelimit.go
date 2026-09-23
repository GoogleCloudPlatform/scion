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
	"net"
	"net/http"
)

// ---------------------------------------------------------------------------
// External-bearer rate limiter (design §4.4) — per-client-IP token bucket
// guarding the external-bearer authentication path (auth_external_bearer.go).
//
// Without it, any unauthenticated client could make the Hub call out to
// Google (the access-token verification round trip, or forced JWKS refreshes)
// for every random opaque string it sends as a bearer token. authenticateExternalBearer
// consults this limiter only on a Google-credential-cache MISS (a cache hit
// is already free of any upstream call, so it does not need — and must not
// be subject to — this budget; design §4.4, C5).
//
// Reuses geExchangeRateLimiter's bucket algorithm and geExchangeClientIP's
// safe trusted-proxy client-IP extraction (ge_exchange_ratelimit.go) rather
// than duplicating either: the two limiters differ only in their rate/burst
// budget, sized for different traffic shapes (a handful of pre-auth bridge
// replicas for the exchange endpoint, vs. arbitrary per-request callers here).
// ---------------------------------------------------------------------------

const (
	// externalBearerRatePerSecond is the sustained per-IP request budget for
	// external-bearer authentication attempts that miss the credential cache.
	externalBearerRatePerSecond = 5.0
	// externalBearerBurst is the maximum per-IP burst before the sustained
	// rate applies.
	externalBearerBurst = 20
)

// externalBearerRateLimiter is a per-client-IP token bucket rate limiter for
// the external-bearer authentication path.
type externalBearerRateLimiter struct {
	buckets     *geExchangeRateLimiter
	trustedNets []*net.IPNet
}

// newExternalBearerRateLimiter creates a rate limiter with the design's
// defaults (5 rps / burst 20 per IP). trustedProxies is parsed once at
// construction, matching how UnifiedAuthMiddleware itself resolves
// cfg.TrustedProxies (auth.go): trusted-proxy configuration is a startup-time
// setting, not part of the request-time hot-reload surface (that is
// FederationAuth's atomic.Pointer, which googleTrust reads fresh on every
// call — see auth_external_bearer.go).
func newExternalBearerRateLimiter(trustedProxies []string) *externalBearerRateLimiter {
	l := newGEExchangeRateLimiter()
	l.rate = externalBearerRatePerSecond
	l.burst = externalBearerBurst
	return &externalBearerRateLimiter{
		buckets:     l,
		trustedNets: parseTrustedProxies(trustedProxies),
	}
}

// Allow reports whether r's client IP is within budget. retryAfterSeconds is
// meaningful only when allowed is false.
func (l *externalBearerRateLimiter) Allow(r *http.Request) (allowed bool, retryAfterSeconds int) {
	clientIP := geExchangeClientIP(r, l.trustedNets)
	return l.buckets.Allow(clientIP)
}

// StartCleanup runs the background goroutine that evicts stale per-IP
// buckets, exiting when ctx is cancelled. Without this, the bounded bucket
// map fills permanently after maxEntries distinct client IPs and fails
// closed for every new one — see the caller (server.go's Start) for why this
// must run whenever the limiter is constructed (review r1 finding 1).
func (l *externalBearerRateLimiter) StartCleanup(ctx context.Context) {
	l.buckets.StartCleanup(ctx)
}
