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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hub refusals of a GCP credential (ptone/scion#6436).

// refusalStatusError mimics the hub client's StatusError (HTTPStatusCode).
type refusalStatusError struct{ code int }

func (e *refusalStatusError) Error() string       { return fmt.Sprintf("hub returned error %d", e.code) }
func (e *refusalStatusError) HTTPStatusCode() int { return e.code }

// refusalFixture is a server whose hub fetches are counted and answer with
// the configured error (nil: success), on a fake clock, without jitter.
type refusalFixture struct {
	srv      *Server
	calls    atomic.Int64
	idCalls  atomic.Int64
	mu       sync.Mutex
	err      error
	now      time.Time
	token    string
	logLines atomic.Int64
}

func newRefusalFixture(t *testing.T) *refusalFixture {
	t.Helper()
	f := &refusalFixture{now: time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC), token: "credential-1"}
	f.srv = New(Config{
		Mode:    "assign",
		SAEmail: "agent@project.iam.gserviceaccount.com",
		TokenFunc: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.token
		},
		FetchGCPToken: func(ctx context.Context, scopes []string) (*GCPAccessTokenResponse, error) {
			f.calls.Add(1)
			f.mu.Lock()
			err := f.err
			f.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return &GCPAccessTokenResponse{AccessToken: "ya29.ok", ExpiresIn: 3599, TokenType: "Bearer"}, nil
		},
		FetchGCPIdentityToken: func(ctx context.Context, audience string) (string, error) {
			f.idCalls.Add(1)
			f.mu.Lock()
			err := f.err
			f.mu.Unlock()
			if err != nil {
				return "", err
			}
			return "id-token-" + audience, nil
		},
	})
	f.srv.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	for _, r := range []*refusalState{f.srv.accessRefusal, f.srv.idRefusal} {
		r.backoff.Rand = func() float64 { return 0 }
	}
	prev := logRefusal
	logRefusal = func(string, ...interface{}) { f.logLines.Add(1) }
	t.Cleanup(func() { logRefusal = prev })
	return f
}

func (f *refusalFixture) set(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *refusalFixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func (f *refusalFixture) setToken(tok string) { f.mu.Lock(); f.token = tok; f.mu.Unlock() }

func (f *refusalFixture) accessToken(t *testing.T) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.handleToken(w, httptest.NewRequest(http.MethodGet, "/token", nil))
	return w.Code, w.Body.String()
}

func (f *refusalFixture) idToken(t *testing.T, audience string) int {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.handleIdentityToken(w, httptest.NewRequest(http.MethodGet, "/identity?audience="+audience, nil))
	return w.Code
}

var errHubRefused = &refusalStatusError{code: http.StatusForbidden}

// A hub 403 opens a window: requests inside it get the cached refusal (403)
// without a hub call, one log line per back-off step, and the window grows
// (10s, 20s, 40s ...) up to the 5 min cap.
func TestRefusal_BackoffServesCachedRefusal(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errHubRefused)

	code, body := f.accessToken(t)
	if code != http.StatusForbidden || !strings.Contains(body, "credential refused by hub; retry after 10s") {
		t.Fatalf("first refusal: %d %q", code, body)
	}
	if strings.Contains(body, "credential-1") {
		t.Fatal("the refusal body carries the credential")
	}
	for i := 0; i < 100; i++ {
		if code, _ := f.accessToken(t); code != http.StatusForbidden {
			t.Fatalf("request %d inside the window: %d, want 403", i, code)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("hub calls inside the first window: %d, want 1", got)
	}
	if got := f.logLines.Load(); got != 1 {
		t.Fatalf("log lines for one back-off step: %d, want 1", got)
	}

	want := []time.Duration{20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	prev := 10 * time.Second
	for i, d := range want {
		f.advance(prev)
		_, body := f.accessToken(t)
		if !strings.Contains(body, fmt.Sprintf("retry after %s", d)) {
			t.Fatalf("step %d: body %q, want retry after %s", i+2, body, d)
		}
		prev = d
	}
	if got := f.calls.Load(); got != int64(1+len(want)) {
		t.Fatalf("hub calls: %d, want one per window (%d)", got, 1+len(want))
	}
	if got := f.logLines.Load(); got != int64(1+len(want)) {
		t.Fatalf("log lines: %d, want one per back-off step (%d)", got, 1+len(want))
	}
}

// Concurrent requests after a refusal make exactly one hub call per window.
func TestRefusal_ConcurrentRequestsOneHubCall(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errHubRefused)
	var wg sync.WaitGroup
	codes := make([]int, 50)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = f.accessToken(t)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusForbidden {
			t.Fatalf("request %d: %d, want 403", i, c)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("hub calls: %d, want 1", got)
	}
}

// A success resets the back-off: the next refusal starts at the base delay.
func TestRefusal_SuccessResets(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errHubRefused)
	f.accessToken(t)
	f.advance(10 * time.Second)
	f.accessToken(t) // second step: 20s
	f.advance(20 * time.Second)
	f.set(nil)
	if code, _ := f.accessToken(t); code != http.StatusOK {
		t.Fatalf("after the window: %d, want 200", code)
	}
	f.srv.mu.Lock()
	f.srv.cachedToken = nil // force the next request to the hub
	f.srv.mu.Unlock()
	f.set(errHubRefused)
	if _, body := f.accessToken(t); !strings.Contains(body, "retry after 10s") {
		t.Fatalf("after a success the back-off restarts at the base: %q", body)
	}
}

// A changed assignment (here: a new hub credential) is not blocked by the
// open window: the request goes to the hub at once, and a new refusal
// restarts at the base delay.
func TestRefusal_AssignmentChangeNotBlocked(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errHubRefused)
	f.accessToken(t)
	f.advance(10 * time.Second)
	f.accessToken(t) // 20s window open
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("setup: hub calls %d, want 2", got)
	}
	f.setToken("credential-2")
	f.set(nil)
	if code, _ := f.accessToken(t); code != http.StatusOK {
		t.Fatalf("after the credential changed: %d, want 200 (not blocked by the window)", code)
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("hub calls: %d, want 3 (the changed assignment went to the hub)", got)
	}
}

// Other hub errors keep the earlier behaviour: 502 and no back-off.
func TestRefusal_OtherErrorsNoBackoff(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errors.New("hub request: connection refused"))
	for i := 0; i < 3; i++ {
		if code, _ := f.accessToken(t); code != http.StatusBadGateway {
			t.Fatalf("request %d: %d, want 502", i, code)
		}
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("hub calls: %d, want 3 (no back-off for a non-403)", got)
	}
	f.set(&refusalStatusError{code: http.StatusInternalServerError})
	if code, _ := f.accessToken(t); code != http.StatusBadGateway {
		t.Fatalf("hub 500: %d, want 502", code)
	}
	if got := f.logLines.Load(); got != 0 {
		t.Fatalf("refusal log lines for non-403 errors: %d, want 0", got)
	}
}

// Identity tokens share one window across audiences.
func TestRefusal_IdentityTokensShareWindow(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errHubRefused)
	if code := f.idToken(t, "aud-a"); code != http.StatusForbidden {
		t.Fatalf("first: %d, want 403", code)
	}
	if code := f.idToken(t, "aud-b"); code != http.StatusForbidden {
		t.Fatalf("other audience in the window: %d, want 403", code)
	}
	if got := f.idCalls.Load(); got != 1 {
		t.Fatalf("hub calls: %d, want 1", got)
	}
	f.advance(10 * time.Second)
	f.set(nil)
	if code := f.idToken(t, "aud-b"); code != http.StatusOK {
		t.Fatalf("after the window: %d, want 200", code)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("access-token hub calls: %d, want 0 (separate state)", got)
	}
}

// The direct HTTP path classifies a hub 403 too.
func TestRefusal_DirectPathTyped(t *testing.T) {
	var hits atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "refused", http.StatusForbidden)
	}))
	defer hub.Close()
	srv := New(Config{Mode: "assign", SAEmail: "a@p.iam.gserviceaccount.com", HubURL: hub.URL, AuthToken: "t"})
	prev := logRefusal
	logRefusal = func(string, ...interface{}) {}
	defer func() { logRefusal = prev }()
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		srv.handleToken(w, httptest.NewRequest(http.MethodGet, "/token", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("request %d: %d, want 403", i, w.Code)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hub hits: %d, want 1", got)
	}
}

// A request that passed the first window check before the leader recorded
// the refusal, and takes the singleflight lock after the leader released it,
// must not become a second leader inside the window: one hub call, one
// back-off step, one log line. Deterministic: the second
// request is held between its first check and the lock until the leader is
// done.
func TestRefusal_LateRequestDoesNotBecomeSecondLeader(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errHubRefused)
	var hookCalls atomic.Int64
	reached := make(chan struct{})
	release := make(chan struct{})
	f.srv.afterRefusalCheck = func() {
		if hookCalls.Add(1) == 1 {
			close(reached)
			<-release
		}
	}
	lateCode := make(chan int, 1)
	go func() {
		code, _ := f.accessToken(t)
		lateCode <- code
	}()
	<-reached // the late request passed its first check, no window yet
	if code, _ := f.accessToken(t); code != http.StatusForbidden {
		t.Fatalf("leader: %d, want 403", code)
	}
	close(release)
	if code := <-lateCode; code != http.StatusForbidden {
		t.Fatalf("late request: %d, want 403 (the cached refusal)", code)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("hub calls: %d, want 1", got)
	}
	if got := f.logLines.Load(); got != 1 {
		t.Fatalf("log lines: %d, want 1", got)
	}
}

// The same for identity tokens of another audience.
func TestRefusal_LateIdentityRequestDoesNotBecomeSecondLeader(t *testing.T) {
	f := newRefusalFixture(t)
	f.set(errHubRefused)
	var hookCalls atomic.Int64
	reached := make(chan struct{})
	release := make(chan struct{})
	f.srv.afterRefusalCheck = func() {
		if hookCalls.Add(1) == 1 {
			close(reached)
			<-release
		}
	}
	lateCode := make(chan int, 1)
	go func() { lateCode <- f.idToken(t, "aud-late") }()
	<-reached
	if code := f.idToken(t, "aud-lead"); code != http.StatusForbidden {
		t.Fatalf("leader: %d, want 403", code)
	}
	close(release)
	if code := <-lateCode; code != http.StatusForbidden {
		t.Fatalf("late request: %d, want 403", code)
	}
	if got := f.idCalls.Load(); got != 1 {
		t.Fatalf("hub calls: %d, want 1", got)
	}
	if got := f.logLines.Load(); got != 1 {
		t.Fatalf("log lines: %d, want 1", got)
	}
}

// refused is idempotent within one open window for the same assignment;
// a new window, or another assignment, takes a step.
func TestRefusal_RefusedIdempotentWithinWindow(t *testing.T) {
	r := newRefusalState()
	r.backoff.Rand = func() float64 { return 0 }
	t0 := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	if d, stepped := r.refused(t0, "k1"); d != 10*time.Second || !stepped {
		t.Fatalf("first: %s %v, want 10s true", d, stepped)
	}
	if d, stepped := r.refused(t0.Add(3*time.Second), "k1"); d != 7*time.Second || stepped {
		t.Fatalf("inside the window: %s %v, want 7s false", d, stepped)
	}
	if d, stepped := r.refused(t0.Add(10*time.Second), "k1"); d != 20*time.Second || !stepped {
		t.Fatalf("after the window: %s %v, want 20s true", d, stepped)
	}
	if d, stepped := r.refused(t0.Add(11*time.Second), "k2"); d != 10*time.Second || !stepped {
		t.Fatalf("another assignment inside the window: %s %v, want 10s true (reset)", d, stepped)
	}
}

// The refresh loop's tick skips while a refusal window is open, and asks the
// hub again once it has closed; a refusal there is one step, one line.
func TestRefusal_RefreshTickSkipsWhileRefusing(t *testing.T) {
	f := newRefusalFixture(t)
	f.srv.mu.Lock()
	f.srv.cachedToken = &GCPAccessTokenResponse{AccessToken: "ya29.old", ExpiresIn: 100, TokenType: "Bearer", FetchedAt: time.Now()}
	f.srv.mu.Unlock()
	f.set(errHubRefused)
	ctx := context.Background()

	f.srv.refreshTick(ctx) // near expiry: asks the hub, which refuses
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("first tick hub calls: %d, want 1", got)
	}
	f.srv.refreshTick(ctx) // window open: skipped
	f.srv.refreshTick(ctx)
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("hub calls while the window is open: %d, want 1", got)
	}
	if got := f.logLines.Load(); got != 1 {
		t.Fatalf("log lines: %d, want 1", got)
	}
	f.advance(10 * time.Second)
	f.srv.refreshTick(ctx)
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("hub calls after the window: %d, want 2", got)
	}
}
