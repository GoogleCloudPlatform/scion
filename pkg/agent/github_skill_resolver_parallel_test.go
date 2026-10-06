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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// fakeSkillTransport serves the GitHub contents and raw endpoints in
// process, without any network. Each skill's directory listing takes
// delay(name) to answer, and the transport records how many listings are
// in flight at once and the order in which they complete. A skill named
// "missing-*" answers 404.
type fakeSkillTransport struct {
	delay func(name string) time.Duration
	// onList, when set, runs at the start of each listing request.
	onList func(name string)

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	listed      []string
	completed   []string
}

func (f *fakeSkillTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	const listPrefix = "/repos/owner/repo/contents/skills/"
	switch {
	case strings.HasPrefix(path, listPrefix):
		name := strings.TrimPrefix(path, listPrefix)
		f.mu.Lock()
		f.inFlight++
		if f.inFlight > f.maxInFlight {
			f.maxInFlight = f.inFlight
		}
		f.listed = append(f.listed, name)
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.inFlight--
			f.completed = append(f.completed, name)
			f.mu.Unlock()
		}()
		if f.onList != nil {
			f.onList(name)
		}
		var d time.Duration
		if f.delay != nil {
			d = f.delay(name)
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		if strings.HasPrefix(name, "missing-") {
			return fakeResponse(req, http.StatusNotFound, []byte(`{"message":"Not Found"}`)), nil
		}
		content := "# " + name
		body, _ := json.Marshal([]githubContentEntry{{
			Name: "SKILL.md", Path: "skills/" + name + "/SKILL.md", Type: "file", Size: len(content),
		}})
		return fakeResponse(req, http.StatusOK, body), nil
	case strings.HasPrefix(path, "/raw/owner/repo/"+testCommitSHA+"/skills/"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/raw/owner/repo/"+testCommitSHA+"/skills/"), "/SKILL.md")
		return fakeResponse(req, http.StatusOK, []byte("# "+name)), nil
	}
	return fakeResponse(req, http.StatusNotFound, nil), nil
}

func fakeResponse(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
}

func newFakeTransportResolver(ft *fakeSkillTransport) *GitHubSkillResolver {
	return &GitHubSkillResolver{
		httpClient: &http.Client{Transport: ft},
		token:      "test-token",
		apiBase:    "http://github.invalid",
		rawBase:    "http://github.invalid/raw",
	}
}

// skillRef returns a ref pinned to testCommitSHA, so resolving it costs one
// directory listing plus one raw download and no commit lookup.
func skillRef(name string) api.SkillReference {
	return api.SkillReference{URI: "gh://owner/repo/" + name + "@" + testCommitSHA}
}

func resolvedNames(res *ResolveResult) []string {
	names := make([]string, 0, len(res.Resolved))
	for _, s := range res.Resolved {
		names = append(names, s.Name)
	}
	return names
}

func TestGitHubSkillResolver_Parallel_PreservesOrder(t *testing.T) {
	const n = 8
	ft := &fakeSkillTransport{delay: func(name string) time.Duration {
		// Earlier refs take longer, so they complete after later ones.
		var i int
		_, _ = fmt.Sscanf(name, "skill-%d", &i)
		return time.Duration(n-i) * 15 * time.Millisecond
	}}
	r := newFakeTransportResolver(ft)

	var refs []api.SkillReference
	var want []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("skill-%d", i)
		refs = append(refs, skillRef(name))
		want = append(want, name)
	}

	res, err := r.Resolve(context.Background(), refs, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if got := resolvedNames(res); !reflect.DeepEqual(got, want) {
		t.Errorf("resolved order = %v, want %v", got, want)
	}
	ft.mu.Lock()
	completed := append([]string(nil), ft.completed...)
	ft.mu.Unlock()
	if reflect.DeepEqual(completed, want) {
		t.Errorf("refs completed in input order %v; the test needs out-of-order completion", completed)
	}
}

func TestGitHubSkillResolver_Parallel_RespectsBound(t *testing.T) {
	for _, bound := range []int{0, 1, 2, 4} {
		t.Run(fmt.Sprintf("bound=%d", bound), func(t *testing.T) {
			want := bound
			if want == 0 {
				want = githubResolveConcurrency
			}
			ft := &fakeSkillTransport{delay: func(string) time.Duration { return 20 * time.Millisecond }}
			// Hold each listing until the bound has been reached once (or
			// a timeout passes), so reaching it does not depend on timing.
			ft.onList = func(string) {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					ft.mu.Lock()
					reached := ft.maxInFlight >= want
					ft.mu.Unlock()
					if reached {
						return
					}
					time.Sleep(time.Millisecond)
				}
			}
			r := newFakeTransportResolver(ft)
			r.maxConcurrent = bound

			var refs []api.SkillReference
			for i := 0; i < 12; i++ {
				refs = append(refs, skillRef(fmt.Sprintf("skill-%d", i)))
			}
			res, err := r.Resolve(context.Background(), refs, ResolveOpts{})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(res.Resolved) != len(refs) {
				t.Fatalf("resolved %d of %d refs; errors: %+v", len(res.Resolved), len(refs), res.Errors)
			}
			ft.mu.Lock()
			defer ft.mu.Unlock()
			if ft.maxInFlight > want {
				t.Errorf("max in flight = %d, want at most %d", ft.maxInFlight, want)
			}
			if ft.maxInFlight != want {
				t.Errorf("max in flight = %d, want the bound %d to be reached", ft.maxInFlight, want)
			}
		})
	}
}

func TestGitHubSkillResolver_Parallel_WallTime(t *testing.T) {
	const (
		n       = 8
		latency = 100 * time.Millisecond
	)
	ft := &fakeSkillTransport{delay: func(string) time.Duration { return latency }}
	r := newFakeTransportResolver(ft)

	var refs []api.SkillReference
	for i := 0; i < n; i++ {
		refs = append(refs, skillRef(fmt.Sprintf("skill-%d", i)))
	}
	start := time.Now()
	res, err := r.Resolve(context.Background(), refs, ResolveOpts{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != n {
		t.Fatalf("resolved %d of %d refs; errors: %+v", len(res.Resolved), n, res.Errors)
	}
	// With a bound of 4, 8 refs take about 2 rounds of latency; resolving
	// them one after another would take 8. The margin is wide on purpose:
	// RespectsBound pins the bound itself without relying on the clock.
	if serial := n * latency; elapsed >= serial*3/4 {
		t.Errorf("Resolve took %v, want well under the serial %v", elapsed, serial)
	}
}

// TestGitHubSkillResolver_Parallel_ErrorsMatchBoundOne checks that with a
// bound of 4 each ref's error is reported with the same code and message,
// in the same order, as with a bound of 1, and pins the expected codes.
func TestGitHubSkillResolver_Parallel_ErrorsMatchBoundOne(t *testing.T) {
	refs := []api.SkillReference{
		skillRef("missing-a"),
		skillRef("skill-1"),
		{URI: "not-a-github-uri"},
		skillRef("missing-b"),
		{URI: "gh://owner/repo/secret-skill@" + testCommitSHA + "?token=NO_SUCH_SECRET"},
		skillRef("skill-5"),
	}
	run := func(bound int) *ResolveResult {
		ft := &fakeSkillTransport{delay: func(name string) time.Duration {
			if name == "missing-a" {
				return 40 * time.Millisecond
			}
			return 5 * time.Millisecond
		}}
		r := newFakeTransportResolver(ft)
		r.maxConcurrent = bound
		res, err := r.Resolve(context.Background(), refs, ResolveOpts{})
		if err != nil {
			t.Fatalf("Resolve (bound %d): %v", bound, err)
		}
		return res
	}
	one := run(1)
	parallel := run(4)

	if !reflect.DeepEqual(parallel.Errors, one.Errors) {
		t.Errorf("bound 4 errors = %+v\nbound 1 errors = %+v", parallel.Errors, one.Errors)
	}
	if got, want := resolvedNames(parallel), resolvedNames(one); !reflect.DeepEqual(got, want) {
		t.Errorf("bound 4 resolved = %v, bound 1 resolved = %v", got, want)
	}

	wantCodes := []string{SkillErrCodeNotFound, "invalid_uri", SkillErrCodeNotFound, "resolve_failed"}
	if len(parallel.Errors) != len(wantCodes) {
		t.Fatalf("got %d errors, want %d: %+v", len(parallel.Errors), len(wantCodes), parallel.Errors)
	}
	for i, e := range parallel.Errors {
		if e.Code != wantCodes[i] {
			t.Errorf("error %d (%s): code = %q, want %q", i, e.URI, e.Code, wantCodes[i])
		}
	}
	if got, want := resolvedNames(parallel), []string{"skill-1", "skill-5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("resolved = %v, want %v", got, want)
	}
}

// TestGitHubSkillResolver_Parallel_CancelFailsQueuedRefsFast cancels the
// ctx while the first ref is in flight. The refs queued behind it still
// run, as they would one at a time, but fail at once with the cancellation,
// and Resolve returns only after all of them have finished.
func TestGitHubSkillResolver_Parallel_CancelFailsQueuedRefsFast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ft := &fakeSkillTransport{
		delay:  func(string) time.Duration { return time.Minute },
		onList: func(string) { cancel() },
	}
	r := newFakeTransportResolver(ft)
	r.maxConcurrent = 1

	refs := []api.SkillReference{skillRef("skill-0"), skillRef("skill-1"), skillRef("skill-2"), skillRef("skill-3")}
	done := make(chan *ResolveResult, 1)
	go func() {
		res, _ := r.Resolve(ctx, refs, ResolveOpts{})
		done <- res
	}()

	var res *ResolveResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Resolve did not return after ctx was canceled")
	}

	ft.mu.Lock()
	inFlight := ft.inFlight
	listed := append([]string(nil), ft.listed...)
	ft.mu.Unlock()
	if inFlight != 0 {
		t.Errorf("Resolve returned with %d requests still in flight", inFlight)
	}
	// Every queued ref was attempted, as it would be one at a time, rather
	// than skipped.
	if want := []string{"skill-0", "skill-1", "skill-2", "skill-3"}; !reflect.DeepEqual(listed, want) {
		t.Errorf("listings attempted = %v, want %v", listed, want)
	}
	if len(res.Resolved) != 0 {
		t.Errorf("resolved = %v, want none", resolvedNames(res))
	}
	if len(res.Errors) != len(refs) {
		t.Fatalf("got %d errors, want %d: %+v", len(res.Errors), len(refs), res.Errors)
	}
	for i, e := range res.Errors {
		if e.URI != refs[i].URI {
			t.Errorf("error %d URI = %q, want %q", i, e.URI, refs[i].URI)
		}
		if e.Code != "resolve_failed" || !strings.Contains(e.Message, context.Canceled.Error()) {
			t.Errorf("error %d = %+v, want a resolve_failed error carrying %q", i, e, context.Canceled)
		}
	}
}

// TestGitHubSkillResolver_Parallel_DeadlineKeepsSerialOutcomes: when the
// deadline passes while refs are still queued behind a slow one, each
// queued ref gets the outcome it would get one at a time. A ref that needs
// GitHub fails as a timeout, and a ref with a fresh cache entry is still
// served.
func TestGitHubSkillResolver_Parallel_DeadlineKeepsSerialOutcomes(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	ft := &fakeSkillTransport{delay: func(name string) time.Duration {
		if name == "slow" {
			return time.Minute
		}
		return 0
	}}
	r := newFakeTransportResolver(ft)
	r.resolutionCache = cache
	r.maxConcurrent = 1

	// Warm the cache for the "warm" ref.
	warm, err := r.Resolve(context.Background(), []api.SkillReference{skillRef("warm")}, ResolveOpts{})
	if err != nil || len(warm.Resolved) != 1 {
		t.Fatalf("warming the cache: err=%v result=%+v", err, warm)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// The slow ref holds the only slot until the deadline. cold-a queues
	// before it passes; warm and cold-b are reached only after it.
	res, err := r.Resolve(ctx, []api.SkillReference{
		skillRef("slow"), skillRef("cold-a"), skillRef("warm"), skillRef("cold-b"),
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got := resolvedNames(res); !reflect.DeepEqual(got, []string{"warm"}) {
		t.Errorf("resolved = %v, want [warm] served from the cache", got)
	}
	wantErrs := []string{"slow", "cold-a", "cold-b"}
	if len(res.Errors) != len(wantErrs) {
		t.Fatalf("got %d errors, want %d: %+v", len(res.Errors), len(wantErrs), res.Errors)
	}
	for i, name := range wantErrs {
		e := res.Errors[i]
		if e.URI != skillRef(name).URI {
			t.Errorf("error %d URI = %q, want the %s ref", i, e.URI, name)
		}
		if e.Code != SkillErrCodeTimeout {
			t.Errorf("%s ref: code = %q (%s), want %q", name, e.Code, e.Message, SkillErrCodeTimeout)
		}
	}
}

// panicSkillTransport panics on the listing of the skill named "boom" and
// otherwise serves like fakeSkillTransport.
type panicSkillTransport struct{ fakeSkillTransport }

func (p *panicSkillTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/contents/skills/boom") {
		panic("credential-value-must-not-leak")
	}
	return p.fakeSkillTransport.RoundTrip(req)
}

// TestGitHubSkillResolver_Parallel_RecoversPanic checks that a panic while
// resolving one ref is reported as that ref's error, with a generic message
// that carries nothing from the panic value, and the other refs resolve.
func TestGitHubSkillResolver_Parallel_RecoversPanic(t *testing.T) {
	pt := &panicSkillTransport{}
	r := &GitHubSkillResolver{
		httpClient: &http.Client{Transport: pt},
		token:      "test-token",
		apiBase:    "http://github.invalid",
		rawBase:    "http://github.invalid/raw",
	}

	refs := []api.SkillReference{skillRef("skill-0"), skillRef("boom"), skillRef("skill-2")}
	res, err := r.Resolve(context.Background(), refs, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := resolvedNames(res), []string{"skill-0", "skill-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("resolved = %v, want %v", got, want)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("got %d errors, want 1: %+v", len(res.Errors), res.Errors)
	}
	e := res.Errors[0]
	if e.URI != refs[1].URI || e.Code != "resolve_failed" {
		t.Errorf("error = %+v, want resolve_failed for %s", e, refs[1].URI)
	}
	if strings.Contains(e.Message, "credential-value") || strings.Contains(e.Message, "test-token") {
		t.Errorf("error message carries panic or credential material: %q", e.Message)
	}
}

// TestGitHubSkillResolver_Parallel_SharedCacheSingleFlight checks that
// duplicate refs resolved in parallel through the resolution cache still
// share one fetch.
func TestGitHubSkillResolver_Parallel_SharedCacheSingleFlight(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	ft := &fakeSkillTransport{delay: func(string) time.Duration { return 50 * time.Millisecond }}
	r := newFakeTransportResolver(ft)
	r.resolutionCache = cache

	refs := []api.SkillReference{skillRef("skill-0"), skillRef("skill-0"), skillRef("skill-0"), skillRef("skill-0")}
	res, err := r.Resolve(context.Background(), refs, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != len(refs) {
		t.Fatalf("resolved %d of %d refs; errors: %+v", len(res.Resolved), len(refs), res.Errors)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.listed) != 1 {
		t.Errorf("listed %d times, want 1 shared fetch", len(ft.listed))
	}
}
