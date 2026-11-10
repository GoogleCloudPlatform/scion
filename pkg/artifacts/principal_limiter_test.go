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
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// fixedClock is a settable clock for limiter tests.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func exhaust(t *testing.T, l *principalLimiter, f LimitFamily, kind, ref string) {
	t.Helper()
	burst := principalFamilyLimits[f].burst
	for i := 0; i < burst; i++ {
		if _, ok := l.allow(f, kind, ref); !ok {
			t.Fatalf("%s request %d of %s:%s refused within the burst", f, i+1, kind, ref)
		}
	}
	if wait, ok := l.allow(f, kind, ref); ok || wait < 1 {
		t.Fatalf("%s request past the burst: ok=%v wait=%d, want refused with a wait", f, ok, wait)
	}
}

func TestPrincipalLimiterFamiliesAreDefined(t *testing.T) {
	for _, f := range []LimitFamily{LimitReviewBeat, LimitReviewList, LimitDraftWrite} {
		lim, ok := principalFamilyLimits[f]
		if !ok || lim.perMinute < 1 || lim.burst < 1 || lim.globalPerMinute < lim.perMinute {
			t.Errorf("family %s limits %+v, want defined with global >= per principal", f, lim)
		}
	}
	if got := principalFamilyLimits[LimitReviewBeat]; got != (familyLimit{perMinute: 24, burst: 24, globalPerMinute: 2400}) {
		t.Errorf("review_beat limits %+v", got)
	}
	// The beat family must admit MaxReviewSessionsPerPrincipal live tabs
	// of a principal on one artifact beating once a minute, plus the extra
	// beats.
	if principalFamilyLimits[LimitReviewBeat].perMinute < 2*MaxReviewSessionsPerPrincipal {
		t.Errorf("review_beat %d/min is below two beats a minute for %d tabs",
			principalFamilyLimits[LimitReviewBeat].perMinute, MaxReviewSessionsPerPrincipal)
	}
}

func TestPrincipalLimiterFamilyIsolation(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	l := newPrincipalLimiter(clk.now)
	exhaust(t, l, LimitDraftWrite, PrincipalKindUser, "u1")
	if _, ok := l.allow(LimitReviewList, PrincipalKindUser, "u1"); !ok {
		t.Error("exhausting draft_write limited review_list")
	}
	// A full review_beat burst after draft_write is exhausted shows that
	// draft_write did not limit review_beat. Probing review_beat first
	// would spend one of its tokens and shorten the burst.
	exhaust(t, l, LimitReviewBeat, PrincipalKindUser, "u1")
	if _, ok := l.allow(LimitReviewList, PrincipalKindUser, "u1"); !ok {
		t.Error("exhausting review_beat limited review_list")
	}
}

func TestPrincipalLimiterPrincipalIsolation(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	l := newPrincipalLimiter(clk.now)
	exhaust(t, l, LimitReviewBeat, PrincipalKindUser, "u1")
	if _, ok := l.allow(LimitReviewBeat, PrincipalKindUser, "u2"); !ok {
		t.Error("one user's exhausted bucket limited another user")
	}
	// The key is kind:ref, so a user and an agent with the same ref are
	// different principals.
	if _, ok := l.allow(LimitReviewBeat, PrincipalKindAgent, "u1"); !ok {
		t.Error("a user's exhausted bucket limited an agent with the same ref")
	}
}

func TestPrincipalLimiterRefills(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	l := newPrincipalLimiter(clk.now)
	exhaust(t, l, LimitReviewBeat, PrincipalKindUser, "u1")
	clk.t = clk.t.Add(time.Minute / time.Duration(principalFamilyLimits[LimitReviewBeat].perMinute))
	if _, ok := l.allow(LimitReviewBeat, PrincipalKindUser, "u1"); !ok {
		t.Error("no token after one refill interval")
	}
}

func TestPrincipalLimiterUnknownFamilyRefused(t *testing.T) {
	l := newPrincipalLimiter(time.Now)
	if _, ok := l.allow(LimitFamily("nope"), PrincipalKindUser, "u1"); ok {
		t.Error("an unknown family was allowed")
	}
	if n := l.tracked(LimitFamily("nope")); n != 0 {
		t.Errorf("unknown family tracks %d principals", n)
	}
}

func TestPrincipalLimiterTableBounded(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	l := newPrincipalLimiter(clk.now)
	for i := 0; i < principalMaxClients+50; i++ {
		l.allow(LimitReviewBeat, PrincipalKindUser, "u"+strconv.Itoa(i))
	}
	if n := l.tracked(LimitReviewBeat); n > principalMaxClients {
		t.Errorf("table holds %d principals, cap %d", n, principalMaxClients)
	}
	if n := l.tracked(LimitReviewList); n != 0 {
		t.Errorf("review_list tracks %d principals after review_beat traffic only", n)
	}
}

func TestChargePrincipalWrites429(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := &Service{principalLimiter: newPrincipalLimiter(clk.now)}
	for i := 0; i < principalFamilyLimits[LimitReviewBeat].burst; i++ {
		if !s.chargePrincipal(httptest.NewRecorder(), LimitReviewBeat, PrincipalKindUser, "u1") {
			t.Fatalf("request %d refused", i+1)
		}
	}
	rec := httptest.NewRecorder()
	if s.chargePrincipal(rec, LimitReviewBeat, PrincipalKindUser, "u1") {
		t.Fatal("request past the burst allowed")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429", rec.Code)
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	if code := errCode(t, rec); code != "rate_limited" {
		t.Errorf("code %q, want rate_limited", code)
	}
}
