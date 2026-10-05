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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestCoalesceFetch_WaiterDeadlineReportsFlightCause checks that a caller
// whose own deadline runs out while the shared fetch is retrying reports
// the fetch's latest recorded cause, with its Retry-After, instead of a bare
// timeout, and that the error still matches context.DeadlineExceeded.
func TestCoalesceFetch_WaiterDeadlineReportsFlightCause(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	recorded := make(chan struct{})
	fetch := func(fctx context.Context) (ResolvedSkill, error) {
		recordAttemptCause(fctx, &githubResolveError{
			code: SkillErrCodeUpstreamUnavailable, retryAfter: "7",
			msg: "GitHub API request to /repos/o/r/commits/main failed (status 503)",
		})
		close(recorded)
		<-release
		return ResolvedSkill{}, errors.New("released")
	}

	// Start the flight with a caller that has no deadline, and wait until it
	// has recorded its cause, so the caller below joins a flight that is
	// already retrying.
	go func() {
		_, _ = cache.coalesceFetch(context.Background(), "flight", "cred", "gh://o/r/s@main", "o/r/s (default)", true, fetch)
	}()
	<-recorded

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = cache.coalesceFetch(ctx, "flight", "cred", "gh://o/r/s@main", "o/r/s (default)", true, fetch)

	var rerr *githubResolveError
	if !errors.As(err, &rerr) {
		t.Fatalf("error %v is not a classified resolve error", err)
	}
	if rerr.code != SkillErrCodeUpstreamUnavailable {
		t.Errorf("code = %q, want %q", rerr.code, SkillErrCodeUpstreamUnavailable)
	}
	if rerr.retryAfter != "7" {
		t.Errorf("retryAfter = %q, want 7", rerr.retryAfter)
	}
	if !strings.Contains(err.Error(), "o/r/s (default)") || !strings.Contains(err.Error(), "status 503") {
		t.Errorf("error %q does not name the ref and the last cause", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("error does not match context.DeadlineExceeded")
	}
}

// TestCoalesceFetch_WaiterDeadlineWithoutCauseIsTimeout checks that a
// caller whose deadline runs out before the shared fetch recorded any cause
// still reports a plain timeout, and that cancellation stays unclassified.
func TestCoalesceFetch_WaiterDeadlineWithoutCauseIsTimeout(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	fetch := func(context.Context) (ResolvedSkill, error) {
		<-release
		return ResolvedSkill{}, errors.New("released")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = cache.coalesceFetch(ctx, "flight", "cred", "gh://o/r/s@main", "ref", true, fetch)
	var rerr *githubResolveError
	if !errors.As(err, &rerr) || rerr.code != SkillErrCodeTimeout || rerr.retryAfter != "" {
		t.Fatalf("error = %v (%+v), want a timeout with no Retry-After", err, rerr)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	_, err = cache.coalesceFetch(cctx, "flight", "cred", "gh://o/r/s@main", "ref", true, fetch)
	if !errors.Is(err, context.Canceled) || errors.As(err, &rerr) {
		t.Fatalf("cancelled caller: error = %v, want plain context.Canceled", err)
	}
}

// TestCoalesceFetch_FlightCauseClearedAfterFlight checks that the recorded
// cause belongs to its flight only: once the flight ends, it is gone.
func TestCoalesceFetch_FlightCauseClearedAfterFlight(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(fctx context.Context) (ResolvedSkill, error) {
		recordAttemptCause(fctx, &githubResolveError{code: SkillErrCodeUnreachable, msg: "no response"})
		return ResolvedSkill{}, errors.New("failed")
	}
	if _, err := cache.coalesceFetch(context.Background(), "flight", "cred", "k", "ref", true, fetch); err == nil {
		t.Fatal("expected the fetch error")
	}
	if c := cache.lastFlightCause("flight"); c != nil {
		t.Fatalf("cause %+v still recorded after the flight ended", c)
	}
}

// TestGitHubSkillResolver_WaiterReportsUpstreamCause follows a resolution
// through the resolver: GitHub answers 503 with a Retry-After that fits the
// shared fetch's own budget, so the fetch keeps retrying, while the caller's
// shorter deadline runs out. The caller gets upstream_unavailable with the
// Retry-After, not a bare timeout.
func TestGitHubSkillResolver_WaiterReportsUpstreamCause(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var gone atomic.Bool
	mux.HandleFunc("/repos/acme/flaky/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		if gone.Load() {
			// Ends the shared fetch at its next attempt once the test is done.
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/repos/acme/flaky/contents/skills/s", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{})
	})
	t.Cleanup(func() { gone.Store(true) })

	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache

	// The first attempt answers at once; the caller's deadline is well past
	// it and well before the 1s Retry-After backoff ends.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	res, err := r.Resolve(ctx, []api.SkillReference{{URI: "gh://acme/flaky/s@main"}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %+v, want one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != SkillErrCodeUpstreamUnavailable {
		t.Errorf("code = %q (%s), want %q", e.Code, e.Message, SkillErrCodeUpstreamUnavailable)
	}
	if e.RetryAfter != "1" {
		t.Errorf("RetryAfter = %q, want 1", e.RetryAfter)
	}
	if !strings.Contains(e.Message, "status 503") {
		t.Errorf("message %q does not name the last failure", e.Message)
	}
}

// TestGitHubSkillResolver_WaiterAfterRecoveryIsTimeout checks that a cause
// recorded for a request that later succeeded is not reported: the commit
// lookup fails once with a 503 and then answers, the listing that follows
// is slow, and a caller whose deadline expires during the listing gets a
// plain timeout, not the earlier 503.
func TestGitHubSkillResolver_WaiterAfterRecoveryIsTimeout(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var commitCalls atomic.Int32
	mux.HandleFunc("/repos/acme/flaky/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		if commitCalls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(strings.Repeat("a", 40)))
	})
	listing := make(chan struct{}, 1)
	release := make(chan struct{})
	mux.HandleFunc("/repos/acme/flaky/contents/skills/s", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case listing <- struct{}{}:
		default:
		}
		<-release
		http.Error(w, "gone", http.StatusNotFound)
	})
	// Registered after the server's own cleanup, so it runs first.
	t.Cleanup(func() { close(release) })

	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache

	// Expire the caller's deadline only once the fetch is in the listing,
	// past the recovered commit lookup.
	dctx := &deadlineOnSignal{Context: context.Background(), done: make(chan struct{})}
	go func() {
		<-listing
		close(dctx.done)
	}()

	res, err := r.Resolve(dctx, []api.SkillReference{{URI: "gh://acme/flaky/s@main"}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %+v, want one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != SkillErrCodeTimeout || e.RetryAfter != "" {
		t.Errorf("error = %+v, want a timeout with no Retry-After", e)
	}
	if strings.Contains(e.Message, "503") {
		t.Errorf("message %q reports the recovered 503", e.Message)
	}
	if got := commitCalls.Load(); got != 2 {
		t.Errorf("commit lookups = %d, want 2 (one 503, one success)", got)
	}
}

// deadlineOnSignal is a context that reports DeadlineExceeded once done is
// closed, so a test can expire a caller's deadline at a chosen point.
type deadlineOnSignal struct {
	context.Context
	done chan struct{}
}

func (d *deadlineOnSignal) Done() <-chan struct{} { return d.done }

func (d *deadlineOnSignal) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestUsableRetryAfter(t *testing.T) {
	cases := map[string]bool{
		"":                              false,
		"0":                             false,
		"-3":                            false,
		"soon":                          false,
		"1":                             true,
		" 30 ":                          true,
		"Wed, 21 Oct 2015 07:28:00 GMT": true,
	}
	for v, want := range cases {
		if got := usableRetryAfter(v); got != want {
			t.Errorf("usableRetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
