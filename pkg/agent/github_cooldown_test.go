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

package agent

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a settable clock for GitHubCooldown.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// scriptedServer answers each request with the next handler in a queue
// (repeating the last one) and counts requests.
type scriptedServer struct {
	*httptest.Server
	mu       sync.Mutex
	handlers []http.HandlerFunc
	calls    atomic.Int64
}

func newScriptedServer(t *testing.T, handlers ...http.HandlerFunc) *scriptedServer {
	t.Helper()
	s := &scriptedServer{handlers: handlers}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.calls.Add(1)
		s.mu.Lock()
		idx := int(n - 1)
		if idx >= len(s.handlers) {
			idx = len(s.handlers) - 1
		}
		h := s.handlers[idx]
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func respond(status int, headers map[string]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func doGet(t *testing.T, c *GitHubCooldown, srv *scriptedServer, identity string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(srv.Client(), req, identity)
}

func requireRateLimit(t *testing.T, err error) *GitHubRateLimitError {
	t.Helper()
	var rl *GitHubRateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *GitHubRateLimitError, got %v", err)
	}
	return rl
}

func TestGitHubCooldown_StartsOnRateLimitResponses(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
	}{
		{"429", http.StatusTooManyRequests, nil, ""},
		{"403 remaining 0", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, ""},
		{"403 secondary limit body", http.StatusForbidden, nil,
			`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			c := NewGitHubCooldown(clock.Now)
			srv := newScriptedServer(t, respond(tc.status, tc.headers, tc.body))

			_, err := doGet(t, c, srv, "id")
			rl := requireRateLimit(t, err)
			if !rl.Sent {
				t.Error("expected Sent=true for a response that was received")
			}
			until, active := c.Active("id")
			if !active {
				t.Fatal("expected the identity to be in a cooldown")
			}
			if want := clock.Now().Add(GitHubCooldownDefault); !until.Equal(want) {
				t.Errorf("cooldown until %v, want %v (default)", until, want)
			}
		})
	}
}

func TestGitHubCooldown_Plain403IsNotRateLimitAndBodyIsKept(t *testing.T) {
	c := NewGitHubCooldown(newFakeClock().Now)
	const body = `{"message":"Resource not accessible by integration"}`
	srv := newScriptedServer(t, respond(http.StatusForbidden, nil, body))

	resp, err := doGet(t, c, srv, "id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body after peek = %q, want %q", got, body)
	}
	if _, active := c.Active("id"); active {
		t.Error("a plain 403 must not start a cooldown")
	}
}

func TestGitHubCooldown_Duration(t *testing.T) {
	clock := newFakeClock()
	now := clock.Now()
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"retry-after seconds", map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{"retry-after http date", map[string]string{"Retry-After": now.Add(45 * time.Second).Format(http.TimeFormat)}, 45 * time.Second},
		{"reset", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(now.Add(90*time.Second).Unix(), 10)}, 90 * time.Second},
		{"retry-after wins over reset", map[string]string{
			"Retry-After":       "20",
			"X-RateLimit-Reset": strconv.FormatInt(now.Add(200*time.Second).Unix(), 10),
		}, 20 * time.Second},
		{"zero retry-after falls through to reset", map[string]string{
			"Retry-After":       "0",
			"X-RateLimit-Reset": strconv.FormatInt(now.Add(40*time.Second).Unix(), 10),
		}, 40 * time.Second},
		{"reset in the past uses default", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)}, GitHubCooldownDefault},
		{"neither header uses default", nil, GitHubCooldownDefault},
		{"retry-after capped", map[string]string{"Retry-After": "3600"}, GitHubCooldownMax},
		{"reset capped", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}, GitHubCooldownMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewGitHubCooldown(clock.Now)
			srv := newScriptedServer(t, respond(http.StatusTooManyRequests, tc.headers, ""))
			_, err := doGet(t, c, srv, "id")
			rl := requireRateLimit(t, err)
			if want := now.Add(tc.want); !rl.RetryAt.Equal(want) {
				t.Errorf("RetryAt = %v, want %v", rl.RetryAt, want)
			}
		})
	}
}

func TestGitHubCooldown_NoRequestDuringCooldown(t *testing.T) {
	clock := newFakeClock()
	c := NewGitHubCooldown(clock.Now)
	srv := newScriptedServer(t,
		respond(http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, ""),
		respond(http.StatusOK, nil, "ok"),
	)

	_, err := doGet(t, c, srv, "id")
	first := requireRateLimit(t, err)

	clock.Advance(29 * time.Second)
	_, err = doGet(t, c, srv, "id")
	rl := requireRateLimit(t, err)
	if rl.Sent {
		t.Error("expected Sent=false for a request held back by the cooldown")
	}
	if !rl.RetryAt.Equal(first.RetryAt) {
		t.Errorf("RetryAt = %v, want %v", rl.RetryAt, first.RetryAt)
	}
	if got := srv.calls.Load(); got != 1 {
		t.Fatalf("expected no request during the cooldown; server saw %d", got)
	}
}

func TestGitHubCooldown_ClearsOnSuccessAfterT(t *testing.T) {
	clock := newFakeClock()
	c := NewGitHubCooldown(clock.Now)
	srv := newScriptedServer(t,
		respond(http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, ""),
		respond(http.StatusOK, nil, "ok"),
	)

	_, err := doGet(t, c, srv, "id")
	_ = requireRateLimit(t, err)

	clock.Advance(30 * time.Second)
	resp, err := doGet(t, c, srv, "id")
	if err != nil {
		t.Fatalf("expected the request after T to be sent, got %v", err)
	}
	_ = resp.Body.Close()
	if got := srv.calls.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want 2", got)
	}
	c.mu.Lock()
	_, present := c.until["id"]
	c.mu.Unlock()
	if present {
		t.Error("expected the cooldown entry to be cleared by the first success after T")
	}
}

func TestGitHubCooldown_IdentitiesAreIndependent(t *testing.T) {
	clock := newFakeClock()
	c := NewGitHubCooldown(clock.Now)
	srv := newScriptedServer(t,
		respond(http.StatusTooManyRequests, nil, ""),
		respond(http.StatusOK, nil, "ok"),
	)

	a := GitHubCooldownIdentity("credential-a")
	b := GitHubCooldownIdentity("credential-b")
	anon := GitHubCooldownIdentity("")
	if a == b || a == anon || b == anon {
		t.Fatalf("identities must differ: %q %q %q", a, b, anon)
	}

	_, err := doGet(t, c, srv, a)
	_ = requireRateLimit(t, err)

	for _, id := range []string{b, anon} {
		resp, err := doGet(t, c, srv, id)
		if err != nil {
			t.Fatalf("identity %d: unexpected error %v", len(id), err)
		}
		_ = resp.Body.Close()
	}
	if _, active := c.Active(a); !active {
		t.Error("the rate-limited identity must still be cooling down")
	}
}

func TestGitHubCooldown_LaterResponseNeverShortens(t *testing.T) {
	clock := newFakeClock()
	c := NewGitHubCooldown(clock.Now)
	long := c.record("id", clock.Now().Add(4*time.Minute))
	got := c.record("id", clock.Now().Add(time.Minute))
	if !got.Equal(long) {
		t.Errorf("cooldown shortened to %v, want %v", got, long)
	}
}

func TestGitHubCooldownIdentity_NoCredentialMaterial(t *testing.T) {
	const cred = "ghp_examplecredentialvalue"
	id := GitHubCooldownIdentity(cred)
	if strings.Contains(id, cred) {
		t.Error("identity must not contain the credential value")
	}
	if GitHubCooldownIdentityForInstallation("public") != GitHubCooldownIdentity("") {
		t.Error("public installation scope must map to the anonymous identity")
	}
	if GitHubCooldownIdentityForInstallation("42") == GitHubCooldownIdentityForInstallation("43") {
		t.Error("different installations must have different identities")
	}
}

// nilBodyTransport answers every request with resp and no body, as a custom
// RoundTripper might.
type nilBodyTransport struct {
	status  int
	headers map[string]string
}

func (n nilBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := http.Header{}
	for k, v := range n.headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: n.status, Header: h, Request: req}, nil
}

// TestGitHubCooldown_NilResponseBody: a RoundTripper response without a body is handled
// without a panic: a rate-limit response still starts the cooldown, and a
// plain 403 is returned to the caller.
func TestGitHubCooldown_NilResponseBody(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		limited bool
	}{
		{"429", http.StatusTooManyRequests, nil, true},
		{"403 remaining 0", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, true},
		{"plain 403", http.StatusForbidden, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			c := NewGitHubCooldown(clock.Now)
			client := &http.Client{Transport: nilBodyTransport{status: tc.status, headers: tc.headers}}
			req, err := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Do(client, req, "id")
			_, active := c.Active("id")
			if tc.limited {
				_ = requireRateLimit(t, err)
				if !active {
					t.Error("expected a cooldown")
				}
				return
			}
			if err != nil || resp == nil || resp.StatusCode != tc.status {
				t.Fatalf("expected the %d response, got resp=%v err=%v", tc.status, resp, err)
			}
			if active {
				t.Error("a plain 403 must not start a cooldown")
			}
		})
	}
}

// TestIsGitHubRateLimitResponse_NilBody: the 403 body check accepts a
// response with no body (http.Client fills one in, but this check does not
// rely on that).
func TestIsGitHubRateLimitResponse_NilBody(t *testing.T) {
	limited, err := isGitHubRateLimitResponse(&http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}})
	if err != nil || limited {
		t.Fatalf("a plain 403 with no body: got limited=%v err=%v", limited, err)
	}
}
