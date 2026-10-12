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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The in-memory testLoginStore (handlers_test_login_test.go) has no
// transaction or audit support of its own. These methods let it serve the
// handler's transactional user write; audits are accepted and discarded.
// Audit behaviour is asserted against a real store in
// handlers_test_login_hardening_test.go.

// The created field is also reported by the in-memory store path that the
// existing tests use, and is present in the JSON body.
func TestHandleTestLogin_CreatedField_JSON(t *testing.T) {
	ws, svc := newTestLoginWebServer(t, true)
	body := `{"email":"json@example.com"}`

	rec := doTestLogin(t, ws, svc, body, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, true, raw["created"])

	rec = doTestLogin(t, ws, svc, body, "")
	require.Equal(t, http.StatusOK, rec.Code)
	raw = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, false, raw["created"])
}

// AC: per-source-IP rate limit; the (N+1)th call in the window gets 429.
func TestHandleTestLogin_RateLimit(t *testing.T) {
	ws, svc := newTestLoginWebServer(t, true)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ws.testLoginLimiter.now = func() time.Time { return clock }

	const addr = "198.51.100.7:4000"
	body := `{"email":"ratelimit@example.com"}`
	for i := 0; i < testLoginRateBurst; i++ {
		require.Equal(t, http.StatusOK, doTestLogin(t, ws, svc, body, addr).Code, "call %d", i+1)
	}

	rec := doTestLogin(t, ws, svc, body, addr)
	assertTestLoginJSONError(t, rec, http.StatusTooManyRequests, ErrCodeRateLimited, "too many test-login requests")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	// The limit is per source IP: another address is unaffected, and a
	// different port on the same address is the same source.
	assert.Equal(t, http.StatusOK, doTestLogin(t, ws, svc, body, "198.51.100.8:4000").Code)
	assert.Equal(t, http.StatusTooManyRequests, doTestLogin(t, ws, svc, body, "198.51.100.7:5000").Code)

	// Forwarding headers do not pick a different bucket.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.RemoteAddr = addr
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Authorization", testLoginAuthHeader(t, svc))
	rec = httptest.NewRecorder()
	ws.handleTestLogin(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	// Tokens refill over time.
	clock = clock.Add(time.Duration(float64(time.Second) / testLoginRatePerSecond))
	assert.Equal(t, http.StatusOK, doTestLogin(t, ws, svc, body, addr).Code)
	assert.Equal(t, http.StatusTooManyRequests, doTestLogin(t, ws, svc, body, addr).Code)
}

// Rejected calls (here: bad challenge token) count toward the limit.
func TestHandleTestLogin_RateLimitCountsRejectedCalls(t *testing.T) {
	ws, svc := newTestLoginWebServer(t, true)
	ws.testLoginLimiter.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	const addr = "198.51.100.9:4000"
	for i := 0; i < testLoginRateBurst; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(`{}`))
		req.RemoteAddr = addr
		req.Header.Set("Authorization", "Bearer not-a-valid-token")
		rec := httptest.NewRecorder()
		ws.handleTestLogin(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, doTestLogin(t, ws, svc, `{"email":"a@example.com"}`, addr).Code)
}

// The bucket table is capped: at the cap, known sources are still served,
// new sources are refused (fail closed), and new sources are admitted again
// once idle buckets age out and a sweep runs.
func TestTestLoginLimiter_CapFailsClosedForNewSources(t *testing.T) {
	l := newTestLoginLimiter()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }

	ip := func(i int) string { return fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256) }
	for i := 0; i < testLoginLimiterMaxBuckets; i++ {
		require.True(t, l.Allow(ip(i)))
	}
	require.Len(t, l.buckets, testLoginLimiterMaxBuckets)

	// All buckets are live: a new source is refused, a known one served.
	clock = clock.Add(2 * testLoginLimiterSweepInterval)
	assert.False(t, l.Allow("192.0.2.1"), "new source at the cap must be refused")
	assert.True(t, l.Allow(ip(0)), "known source at the cap must be served")
	assert.Len(t, l.buckets, testLoginLimiterMaxBuckets, "the table never grows past the cap")

	// Buckets age out (ip(0) was refreshed above and stays).
	clock = clock.Add(testLoginLimiterMaxAge - time.Second)
	assert.True(t, l.Allow(ip(0)))
	clock = clock.Add(2 * time.Second)
	assert.True(t, l.Allow("192.0.2.1"), "new sources are admitted after idle buckets age out")
	assert.Len(t, l.buckets, 2)
}

// The sweep runs at most once per interval, so new sources flooding a full
// table do not each trigger a full scan.
func TestTestLoginLimiter_SweepIsRateLimited(t *testing.T) {
	l := newTestLoginLimiter()
	l.maxBuckets = 4
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }

	for i := 0; i < 4; i++ {
		require.True(t, l.Allow(fmt.Sprintf("10.0.0.%d", i)))
	}
	// First refusal sweeps (nothing idle yet) and records the sweep time.
	assert.False(t, l.Allow("10.0.1.1"))
	sweptAt := l.lastSweep
	require.Equal(t, clock, sweptAt)

	// The buckets become idle, but within the interval no sweep runs.
	clock = clock.Add(testLoginLimiterMaxAge + time.Second)
	l.lastSweep = clock.Add(-testLoginLimiterSweepInterval / 2)
	assert.False(t, l.Allow("10.0.1.2"), "no sweep within the interval")
	assert.Len(t, l.buckets, 4)

	// Once the interval has passed, the next new source sweeps and is admitted.
	clock = clock.Add(testLoginLimiterSweepInterval)
	assert.True(t, l.Allow("10.0.1.3"))
	assert.Len(t, l.buckets, 1)
}

func TestTestLoginRateKey(t *testing.T) {
	assert.Equal(t, "198.51.100.7", testLoginRateKey("198.51.100.7:4000"))
	assert.Equal(t, "198.51.100.7", testLoginRateKey("[::ffff:198.51.100.7]:4000"))
	assert.Equal(t, "2001:db8:1:2::/64", testLoginRateKey("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443"))
	assert.Equal(t, testLoginRateKey("[2001:db8:1:2::1]:1"), testLoginRateKey("[2001:db8:1:2:ffff::9]:2"),
		"addresses in one /64 share a bucket")
	assert.NotEqual(t, testLoginRateKey("[2001:db8:1:2::1]:1"), testLoginRateKey("[2001:db8:1:3::1]:1"))
	assert.Equal(t, "not-an-ip", testLoginRateKey("not-an-ip"))
}
