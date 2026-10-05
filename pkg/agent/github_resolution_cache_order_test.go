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

// TestGitHubResolutionCache_FlightRecheckServesRememberedFailure checks
// that a caller which passed the pre-check before a concurrent flight
// recorded a not_found gets the remembered failure from the re-check inside
// its own flight, without fetching again.
func TestGitHubResolutionCache_FlightRecheckServesRememberedFailure(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const key = "gh://o/r/s@main#cred"
	notFound := &githubResolveError{code: SkillErrCodeNotFound, msg: "ref not found"}
	var calls atomic.Int32

	// The first caller to reach the join point (B) is held there; it has
	// already passed the pre-check. Later callers pass straight through.
	held := make(chan struct{})
	release := make(chan struct{})
	var joins atomic.Int32
	hook := func(string) {
		if joins.Add(1) == 1 {
			close(held)
			<-release
		}
	}
	flightJoinHook.Store(&hook)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	doneB := make(chan error, 1)
	go func() { doneB <- resolveCounting(t, cache, key, &calls, notFound) }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("caller B never reached the join point")
	}

	// A's flight gets the not_found, records it and finishes.
	errA := resolveCounting(t, cache, key, &calls, notFound)
	if !errors.Is(errA, notFound) {
		t.Fatalf("A: err = %v, want the not_found", errA)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch ran %d times after A, want 1", got)
	}

	close(release)
	var errB error
	select {
	case errB = <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("caller B did not return")
	}
	if !errors.Is(errB, notFound) {
		t.Fatalf("B: err = %v, want the remembered not_found", errB)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch ran %d times, want 1: B's flight must use the remembered failure", got)
	}
}

// TestGitHubResolutionCache_StaleEntryWinsOverRememberedFailure checks that
// a stale branch-ref entry is served ahead of a remembered failure for the
// same key, which happens when a background refresh records a not_found
// while the stale entry is still held.
func TestGitHubResolutionCache_StaleEntryWinsOverRememberedFailure(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const (
		key       = "gh://o/r/s@main#cred"
		flightKey = "flight|" + key
	)
	now := time.Now()
	cache.mu.Lock()
	cache.entries[key] = &resolutionCacheEntry{
		Skill:       ResolvedSkill{Name: "old", URI: key},
		CachedAt:    now.Add(-time.Hour),
		ExpiresAt:   now.Add(-30 * time.Minute),
		IsBranchRef: true,
	}
	cache.mu.Unlock()
	notFound := &githubResolveError{code: SkillErrCodeNotFound, msg: "ref not found"}
	cache.recordFailure(key, notFound)
	t.Cleanup(func() {
		// Let any background refresh finish before the temp dir goes away.
		_, _, _ = cache.flight.Do(flightKey, func() (interface{}, error) { return nil, nil })
	})

	skill, err := cache.ResolveWithFetch(context.Background(), key, flightKey, "cred", "ref", true, nil,
		func(context.Context) (ResolvedSkill, error) { return ResolvedSkill{}, notFound })
	if err != nil {
		t.Fatalf("err = %v, want the stale entry", err)
	}
	if skill.Name != "old" {
		t.Fatalf("got %q, want the stale entry %q", skill.Name, "old")
	}
}

// TestGitHubSkillResolver_RawDownloadNotFoundIsNotRemembered checks that a
// file the listing named but the raw download reports as missing is not
// remembered: the next Resolve fetches again.
func TestGitHubSkillResolver_RawDownloadNotFoundIsNotRemembered(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	mux.HandleFunc("/repos/acme/tools/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/acme/tools/contents/skills/lagging", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/lagging/SKILL.md", Type: "file", Size: 4},
		})
	})
	var rawHits atomic.Int32
	mux.HandleFunc("/raw/acme/tools/"+testCommitSHA+"/skills/lagging/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		rawHits.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	})

	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache

	for i := 0; i < 2; i++ {
		res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://acme/tools/lagging@main"}}, ResolveOpts{})
		if err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
		if len(res.Errors) != 1 || res.Errors[0].Code != SkillErrCodeNotFound {
			t.Fatalf("Resolve %d: errors = %+v, want one not_found", i, res.Errors)
		}
	}
	if got := rawHits.Load(); got != 2 {
		t.Fatalf("raw download was requested %d times, want 2 (the failure must not be remembered)", got)
	}
}

// TestGitHubSkillResolver_RememberedFailureNamesCallersOwnRef checks that a
// not_found remembered for one project is served to another project whose
// ?token= secret has the same value (so the same cache key) with that
// project's own ref in the message, never the first project's spelling.
func TestGitHubSkillResolver_RememberedFailureNamesCallersOwnRef(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var commitHits atomic.Int32
	mux.HandleFunc("/repos/acme/tools/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		commitHits.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	})

	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newResolver := func(secret string) *GitHubSkillResolver {
		r := newTestGitHubResolver(server)
		r.provisionCredentials = map[string]string{secret: "shared-value"}
		r.resolutionCache = cache
		return r
	}

	resolve := func(r *GitHubSkillResolver, secret, project string) ResolveError {
		t.Helper()
		uri := "gh://acme/tools/missing@main?token=" + secret
		res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{ProjectID: project})
		if err != nil {
			t.Fatalf("Resolve %s: %v", uri, err)
		}
		if len(res.Errors) != 1 || res.Errors[0].Code != SkillErrCodeNotFound {
			t.Fatalf("Resolve %s: errors = %+v, want one not_found", uri, res.Errors)
		}
		return res.Errors[0]
	}

	first := resolve(newResolver("P1_SECRET"), "P1_SECRET", "p1")
	if !strings.Contains(first.Message, "P1_SECRET") {
		t.Fatalf("first message %q does not name its own ref", first.Message)
	}
	second := resolve(newResolver("P2_SECRET"), "P2_SECRET", "p2")
	if got := commitHits.Load(); got != 1 {
		t.Fatalf("commits endpoint hit %d times, want 1 (second call served from the remembered failure)", got)
	}
	if strings.Contains(second.Message, "P1_SECRET") {
		t.Fatalf("second message %q names the other project's ref", second.Message)
	}
	if !strings.Contains(second.Message, "gh://acme/tools/missing@main?token=P2_SECRET") {
		t.Fatalf("second message %q does not name its own ref", second.Message)
	}
}
