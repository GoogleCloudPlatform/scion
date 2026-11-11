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

package artifacts

import (
	"net/http"
	"strconv"
	"time"
)

// LimitFamily names a family of authenticated artifact routes that share
// one set of per-principal rate limits. Each family has its own buckets,
// so using up one family never limits another.
type LimitFamily string

// The route families of the per-principal limiter.
const (
	// LimitReviewBeat covers PUT and DELETE
	// /api/v1/artifacts/{id}/review-sessions/{sid}: the heartbeat of an
	// open review tab, one beat a minute, plus a beat when the tab becomes
	// visible or the review moves to a newer version, and a DELETE when the
	// review is closed or its page left.
	LimitReviewBeat LimitFamily = "review_beat"
	// LimitReviewList covers reading who is reviewing an artifact. It is
	// defined for the publisher indicator, which has no route yet.
	LimitReviewList LimitFamily = "review_list"
	// LimitDraftWrite covers writing and deleting a saved draft review. It
	// is defined for durable draft reviews, which have no route yet.
	LimitDraftWrite LimitFamily = "draft_write"
)

// familyLimit is one family's rates: a principal's sustained rate and
// burst, and the rate of all principals together on one hub instance.
type familyLimit struct {
	perMinute, burst, globalPerMinute int
}

// principalFamilyLimits are the rates of every family.
//
// review_beat: one beat a minute per open tab, plus a visibility beat and
// a closing DELETE, with MaxReviewSessionsPerPrincipal live tabs on one
// artifact, is about 16 a minute; 24 leaves headroom for a reviewer
// with tabs on several artifacts. review_list and draft_write have no route
// yet; their rates are set for the planned uses: review_list read on page
// load, on return to a tab (at most every 30 s) and on entering edit mode,
// and draft_write an autosave every few seconds while typing.
var principalFamilyLimits = map[LimitFamily]familyLimit{
	LimitReviewBeat: {perMinute: 24, burst: 24, globalPerMinute: 2400},
	LimitReviewList: {perMinute: 10, burst: 10, globalPerMinute: 1000},
	LimitDraftWrite: {perMinute: 12, burst: 12, globalPerMinute: 1200},
}

// principalMaxClients caps the principals one family tracks at once. As
// in the share-link limiter, a principal arriving when the table is full
// of buckets that are still refilling is limited by the family's global
// bucket alone, so the table cannot grow without bound.
const principalMaxClients = 10000

// principalLimiter rate-limits authenticated artifact routes per
// principal, with a separate bucket set for each route family. It is the
// one limiter for every such route: a new route family adds an entry to
// principalFamilyLimits and charges it, never a limiter of its own.
//
// The limits are per hub instance, so a hub with several instances allows
// that many times the rates: they bound misuse, they are not a quota.
//
// A route charges it only after the caller is authorized for the artifact
// (see chargePrincipal), so only authorized principals create entries.
type principalLimiter struct {
	families map[LimitFamily]*rateLimiter
}

func newPrincipalLimiter(now func() time.Time) *principalLimiter {
	l := &principalLimiter{families: make(map[LimitFamily]*rateLimiter, len(principalFamilyLimits))}
	for f, lim := range principalFamilyLimits {
		l.families[f] = newRateLimiter(now, lim.perMinute, lim.burst, lim.globalPerMinute, lim.globalPerMinute, principalMaxClients)
	}
	return l
}

// allow charges one request of principal kind:ref to family. It returns
// false, and the seconds to wait, when the request is over the limit. An
// unknown family is refused: a route that names a family must have limits.
func (l *principalLimiter) allow(family LimitFamily, kind, ref string) (int, bool) {
	rl, ok := l.families[family]
	if !ok {
		return 60, false
	}
	return rl.allow(PrincipalRef(kind, ref))
}

// tracked reports how many principals family holds a bucket for (tests).
func (l *principalLimiter) tracked(family LimitFamily) int {
	rl, ok := l.families[family]
	if !ok {
		return 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.clients)
}

// principalLim returns the service's per-principal limiter.
func (s *Service) principalLim() *principalLimiter {
	s.principalLimOnce.Do(func() {
		if s.principalLimiter == nil {
			s.principalLimiter = newPrincipalLimiter(time.Now)
		}
	})
	return s.principalLimiter
}

// chargePrincipal charges one request of principal kind:ref to family and
// answers 429 rate_limited with Retry-After when it is over the limit,
// reporting whether the request may go on. Callers charge it only after
// the caller passed every authorization check of the route, and before
// they read the request body.
func (s *Service) chargePrincipal(w http.ResponseWriter, family LimitFamily, kind, ref string) bool {
	wait, ok := s.principalLim().allow(family, kind, ref)
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
	return false
}
