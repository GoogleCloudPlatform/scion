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

package metadata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeTestIDTokenJWT(exp time.Time) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return hdr + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

type rateLimitStatusError struct {
	code       int
	retryAfter time.Duration
}

func (e *rateLimitStatusError) Error() string                     { return fmt.Sprintf("hub returned %d", e.code) }
func (e *rateLimitStatusError) HTTPStatusCode() int               { return e.code }
func (e *rateLimitStatusError) RetryAfterDuration() time.Duration { return e.retryAfter }

func TestIDTokenCache_JWTExpiryAndSequentialReuse(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var calls atomic.Int64

	srv := New(Config{
		Mode:    "assign",
		SAEmail: "agent@project.iam.gserviceaccount.com",
		FetchGCPIdentityToken: func(_ context.Context, audience string) (string, error) {
			n := calls.Add(1)
			mu.Lock()
			cur := now
			mu.Unlock()
			// Mint a JWT expiring in 10 minutes (shorter than hardcoded 55m).
			return makeTestIDTokenJWT(cur.Add(10*time.Minute)) + fmt.Sprintf("-%d", n), nil
		},
	})
	srv.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	get := func(path string) (int, string) {
		w := httptest.NewRecorder()
		srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code, w.Body.String()
	}

	// First fetch misses cache.
	code, tok1 := get("/identity?audience=https://svc.example.com")
	if code != http.StatusOK {
		t.Fatalf("first fetch: %d", code)
	}
	// Second fetch with same audience and extra format/licenses params hits cache.
	code, tok2 := get("/identity?audience=https://svc.example.com&format=full&licenses=TRUE")
	if code != http.StatusOK || tok2 != tok1 {
		t.Fatalf("cached fetch: %d %q, want 200 %q", code, tok2, tok1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("hub calls = %d, want 1", got)
	}

	// At +8m59s (61s before exp), still served from cache.
	mu.Lock()
	now = now.Add(8*time.Minute + 59*time.Second)
	mu.Unlock()
	code, tok3 := get("/identity?audience=https://svc.example.com")
	if code != http.StatusOK || tok3 != tok1 {
		t.Fatalf("at +8m59s: %d %q, want cached token", code, tok3)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("hub calls at +8m59s = %d, want 1", got)
	}

	// At +9m01s (< 60s before exp), must refresh from Hub.
	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()
	code, tok4 := get("/identity?audience=https://svc.example.com")
	if code != http.StatusOK || tok4 == tok1 {
		t.Fatalf("at +9m01s: %d %q, want refreshed token", code, tok4)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("hub calls at +9m01s = %d, want 2", got)
	}
}

func TestIDTokenCache_BoundedSizeEviction(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var calls atomic.Int64

	srv := New(Config{
		Mode:    "assign",
		SAEmail: "agent@project.iam.gserviceaccount.com",
		FetchGCPIdentityToken: func(_ context.Context, audience string) (string, error) {
			calls.Add(1)
			mu.Lock()
			cur := now
			mu.Unlock()
			return makeTestIDTokenJWT(cur.Add(1 * time.Hour)), nil
		},
	})
	srv.maxIDTokens = 3
	srv.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	for i := 0; i < 5; i++ {
		mu.Lock()
		now = now.Add(time.Second)
		mu.Unlock()
		w := httptest.NewRecorder()
		srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/identity?audience=aud-%d", i), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("aud-%d: status %d", i, w.Code)
		}
	}

	srv.idTokenMu.RLock()
	size := len(srv.cachedIDTokens)
	_, hasOldest := srv.cachedIDTokens["aud-0"]
	_, hasNewest := srv.cachedIDTokens["aud-4"]
	srv.idTokenMu.RUnlock()

	if size != 3 {
		t.Fatalf("cache size = %d, want 3", size)
	}
	if hasOldest {
		t.Error("expected oldest-expiring entry aud-0 to be evicted")
	}
	if !hasNewest {
		t.Error("expected newest entry aud-4 to remain cached")
	}
}

func TestIDTokenSingleflight_LeaderCancelDoesNotFailFollower(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64

	srv := New(Config{
		Mode:    "assign",
		SAEmail: "agent@project.iam.gserviceaccount.com",
		FetchGCPIdentityToken: func(ctx context.Context, audience string) (string, error) {
			calls.Add(1)
			close(started)
			<-release
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "id-token-" + audience, nil
		},
	})
	joined := make(chan struct{}, 1)
	srv.afterFollowerJoin = func(string) { joined <- struct{}{} }

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/identity?audience=aud-x", nil).WithContext(leaderCtx)
		srv.handleIdentityToken(w, r)
		leaderDone <- w.Code
	}()
	<-started

	followerDone := make(chan struct {
		code int
		body string
	}, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/identity?audience=aud-x", nil)
		srv.handleIdentityToken(w, r)
		followerDone <- struct {
			code int
			body string
		}{w.Code, w.Body.String()}
	}()
	<-joined

	// Cancel the leader's request context mid-fetch; follower must still succeed.
	cancelLeader()
	close(release)

	res := <-followerDone
	if res.code != http.StatusOK || res.body != "id-token-aud-x" {
		t.Fatalf("follower: %d %q, want 200 id-token-aud-x", res.code, res.body)
	}
	<-leaderDone
	if got := calls.Load(); got != 1 {
		t.Fatalf("hub calls = %d, want 1", got)
	}
}

func TestRateLimit_BackoffAndRetryAfter(t *testing.T) {
	f := newRefusalFixture(t)
	f.srv.rateLimit.backoff.Rand = func() float64 { return 0 }
	f.set(&rateLimitStatusError{code: http.StatusTooManyRequests, retryAfter: 5 * time.Second})

	// First 429 with Retry-After: 5s overrides the 2s base backoff.
	w := httptest.NewRecorder()
	f.srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, "/identity?audience=aud-a", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("first 429: status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("first 429 Retry-After = %q, want 5", got)
	}

	// Burst of retries inside the 5s window (both identity and access tokens)
	// must NOT hit the Hub.
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		f.srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, "/identity?audience=aud-b", nil))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("in-window id request %d: %d, want 429", i, w.Code)
		}
		if secs, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || secs < 1 {
			t.Fatalf("in-window Retry-After = %q, want >= 1", w.Header().Get("Retry-After"))
		}
	}
	if code, _ := f.accessToken(t); code != http.StatusTooManyRequests {
		t.Fatalf("in-window access token: %d, want 429 (shared bucket)", code)
	}
	if got := f.idCalls.Load(); got != 1 {
		t.Fatalf("hub id calls inside window = %d, want 1", got)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("hub access calls inside window = %d, want 0", got)
	}
	if got := f.logLines.Load(); got != 1 {
		t.Fatalf("log lines = %d, want 1", got)
	}

	// After 5s window expires, next 429 with small Retry-After (1s) uses
	// exponential backoff step 2 (4s > 1s).
	f.advance(5 * time.Second)
	f.set(&rateLimitStatusError{code: http.StatusTooManyRequests, retryAfter: 1 * time.Second})
	w = httptest.NewRecorder()
	f.srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, "/identity?audience=aud-a", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second 429: %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "4" {
		t.Fatalf("second 429 Retry-After = %q, want 4 (exponential backoff step 2)", got)
	}
	if got := f.idCalls.Load(); got != 2 {
		t.Fatalf("hub id calls = %d, want 2", got)
	}

	// After 4s window expires and Hub succeeds, backoff resets to base (2s).
	f.advance(4 * time.Second)
	f.set(nil)
	if code := f.idToken(t, "aud-a"); code != http.StatusOK {
		t.Fatalf("after window recovery: %d, want 200", code)
	}
	f.set(&rateLimitStatusError{code: http.StatusTooManyRequests})
	w = httptest.NewRecorder()
	f.srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, "/identity?audience=aud-new", nil))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "retry after 2s") {
		t.Fatalf("after reset: %d %q, want 429 with retry after 2s", w.Code, w.Body.String())
	}
}

func TestRateLimit_DirectPathHonorsRetryAfterHeader(t *testing.T) {
	var hits atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "7")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer hub.Close()

	srv := New(Config{Mode: "assign", SAEmail: "a@p.iam.gserviceaccount.com", HubURL: hub.URL, AuthToken: "t"})
	srv.rateLimit.backoff.Rand = func() float64 { return 0 }
	prev := logRefusal
	logRefusal = func(string, ...interface{}) {}
	defer func() { logRefusal = prev }()

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, "/identity?audience=aud-1", nil))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: %d, want 429", i, w.Code)
		}
		if got := w.Header().Get("Retry-After"); got == "" {
			t.Fatalf("request %d: missing Retry-After", i)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d, want 1 (5x retries collapsed by backoff window)", got)
	}
}
