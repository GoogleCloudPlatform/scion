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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// writeCacheFile writes entries to dir's cache file as an older process
// would have left it.
func writeCacheFile(t *testing.T, dir string, entries map[string]*resolutionCacheEntry) []byte {
	t.Helper()
	data, err := json.MarshalIndent(resolutionCacheFile{Entries: entries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, resolutionCacheFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func readCacheFile(t *testing.T, dir string) resolutionCacheFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, resolutionCacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	var f resolutionCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func tempFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, resolutionCacheFileName+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGitHubResolutionCache_DirAndFileModes(t *testing.T) {
	parent := t.TempDir()

	t.Run("new directory and file", func(t *testing.T) {
		dir := filepath.Join(parent, "new", "cache")
		cache, err := newTestResolutionCache(dir, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		cache.putEntry("gh://o/r/s@main", ResolvedSkill{Name: "s"}, true)
		cache.Flush()
		assertMode(t, dir, 0o700)
		assertMode(t, filepath.Join(dir, resolutionCacheFileName), 0o600)
	})

	t.Run("existing loose directory and file are tightened", func(t *testing.T) {
		dir := filepath.Join(parent, "loose")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		writeCacheFile(t, dir, map[string]*resolutionCacheEntry{})
		if err := os.Chmod(filepath.Join(dir, resolutionCacheFileName), 0o666); err != nil {
			t.Fatal(err)
		}

		if _, err := newTestResolutionCache(dir, time.Hour); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dir, 0o700)
		assertMode(t, filepath.Join(dir, resolutionCacheFileName), 0o600)
	})
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: mode %o, want %o", filepath.Base(path), got, want)
	}
}

// TestGitHubResolutionCache_WriteFailureKeepsOldFile simulates a write that
// fails part way through: the existing file must be left exactly as it was,
// no temp file may remain, and the in-memory cache keeps working.
func TestGitHubResolutionCache_WriteFailureKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cache.putEntry("gh://o/r/first@main", ResolvedSkill{Name: "first"}, true)
	cache.Flush()
	before, err := os.ReadFile(filepath.Join(dir, resolutionCacheFileName))
	if err != nil {
		t.Fatal(err)
	}

	cache.writeData = func(w io.Writer, data []byte) error {
		if _, err := w.Write(data[:len(data)/2]); err != nil {
			return err
		}
		return errors.New("simulated write failure")
	}
	cache.putEntry("gh://o/r/second@main", ResolvedSkill{Name: "second"}, true)
	cache.Flush()

	after, err := os.ReadFile(filepath.Join(dir, resolutionCacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("cache file changed after a failed write")
	}
	if tmp := tempFilesIn(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
	if _, ok := cache.Get("gh://o/r/second@main"); !ok {
		t.Error("in-memory entry lost after a failed write")
	}
	if got := cache.saveCount.Load(); got != 1 {
		t.Errorf("saveCount = %d, want 1 (the failed write must not count)", got)
	}
}

// TestGitHubResolutionCache_LoadDropsUnusableEntriesAndRewrites checks that
// load keeps what stale-serve can still use and drops the rest, then
// rewrites the file once.
func TestGitHubResolutionCache_LoadDropsUnusableEntriesAndRewrites(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fresh := func() *resolutionCacheEntry {
		return &resolutionCacheEntry{Skill: ResolvedSkill{Name: "x"}, CachedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	freshCred := testCredKey("gh://o/r/fresh@main", "cred")
	staleBranch := testCredKey("gh://o/r/stale@main", "cred")
	sha := strings.Repeat("a", 40)
	freshSHA := "gh://o/r/s@" + sha
	expiredSHA := "gh://o/r/expired@" + sha
	tooOldBranch := "gh://o/r/old@main"
	oldFormat := "gh://o/r/legacy@main#deadbeef12345678"
	notGitHub := "https://example.com/skill"
	nilEntry := "gh://o/r/nil@main"

	writeCacheFile(t, dir, map[string]*resolutionCacheEntry{
		freshCred:   fresh(),
		staleBranch: {Skill: ResolvedSkill{Name: "stale"}, CachedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-90 * time.Minute), IsBranchRef: true},
		freshSHA:    fresh(),
		expiredSHA:  {Skill: ResolvedSkill{Name: "x"}, CachedAt: now.Add(-25 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
		tooOldBranch: {Skill: ResolvedSkill{Name: "x"}, CachedAt: now.Add(-MaxResolutionStaleAge - time.Minute),
			ExpiresAt: now.Add(-MaxResolutionStaleAge + 29*time.Minute), IsBranchRef: true},
		oldFormat: fresh(),
		notGitHub: fresh(),
		nilEntry:  nil,
	})

	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	wantKept := []string{freshCred, staleBranch, freshSHA}
	cache.mu.RLock()
	gotLen := len(cache.entries)
	for _, k := range wantKept {
		if _, ok := cache.entries[k]; !ok {
			t.Errorf("entry %d of wantKept was not loaded", indexOf(wantKept, k))
		}
	}
	cache.mu.RUnlock()
	if gotLen != len(wantKept) {
		t.Errorf("loaded %d entries, want %d", gotLen, len(wantKept))
	}

	if got := cache.saveCount.Load(); got != 1 {
		t.Fatalf("saveCount after load = %d, want exactly one rewrite", got)
	}
	onDisk := readCacheFile(t, dir)
	if len(onDisk.Entries) != len(wantKept) {
		t.Errorf("rewritten file has %d entries, want %d", len(onDisk.Entries), len(wantKept))
	}
	for _, k := range wantKept {
		if _, ok := onDisk.Entries[k]; !ok {
			t.Errorf("entry %d of wantKept missing from rewritten file", indexOf(wantKept, k))
		}
	}
	assertMode(t, filepath.Join(dir, resolutionCacheFileName), 0o600)

	// A second load finds nothing to drop and must not rewrite.
	cache2, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := cache2.saveCount.Load(); got != 0 {
		t.Errorf("clean load rewrote the file %d times, want 0", got)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestGitHubResolutionCache_LoadRewritesInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, resolutionCacheFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := cache.saveCount.Load(); got != 1 {
		t.Fatalf("saveCount = %d, want 1", got)
	}
	if f := readCacheFile(t, dir); len(f.Entries) != 0 {
		t.Errorf("rewritten file has %d entries, want 0", len(f.Entries))
	}
}

// TestGitHubResolutionCache_StaleBranchServedAfterReload checks that a branch
// entry past its TTL but within MaxResolutionStaleAge, written by an earlier
// process, is loaded and served stale straight away.
func TestGitHubResolutionCache_StaleBranchServedAfterReload(t *testing.T) {
	dir := t.TempDir()
	key := testCredKey("gh://o/r/s@main", "cred")
	now := time.Now()
	writeCacheFile(t, dir, map[string]*resolutionCacheEntry{
		key: {Skill: ResolvedSkill{Name: "old"}, CachedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-30 * time.Minute), IsBranchRef: true},
	})

	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get(key); ok {
		t.Fatal("entry past its TTL reported as fresh")
	}

	refreshed := make(chan struct{})
	fetch := func(context.Context) (ResolvedSkill, error) {
		close(refreshed)
		return ResolvedSkill{Name: "new"}, nil
	}
	skill, err := cache.ResolveWithFetch(context.Background(), key, "flight-reload", "cred-reload", "test-ref", true, nil, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Name != "old" {
		t.Fatalf("got %q, want the stale value %q", skill.Name, "old")
	}
	<-refreshed
	// Join the background refresh so it has finished before the test ends.
	_, _, _ = cache.flight.Do("flight-reload", func() (interface{}, error) { return nil, nil })
}

// TestGitHubResolutionCache_BurstCoalescesWrites checks that many Puts
// before the delayed write runs produce a single rewrite, and that a Put
// after that write schedules another.
func TestGitHubResolutionCache_BurstCoalescesWrites(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache.putEntry(fmt.Sprintf("gh://o/r/s%d@main", i), ResolvedSkill{Name: "s"}, true)
		}(i)
	}
	wg.Wait()
	if got := cache.saveCount.Load(); got != 0 {
		t.Fatalf("saveCount before flush = %d, want 0", got)
	}
	cache.Flush()
	if got := cache.saveCount.Load(); got != 1 {
		t.Fatalf("saveCount after burst = %d, want 1", got)
	}
	if f := readCacheFile(t, dir); len(f.Entries) != n {
		t.Fatalf("file has %d entries, want %d", len(f.Entries), n)
	}

	// Nothing pending: Flush is a no-op.
	cache.Flush()
	if got := cache.saveCount.Load(); got != 1 {
		t.Fatalf("idle Flush wrote the file; saveCount = %d", got)
	}

	cache.putEntry("gh://o/r/later@main", ResolvedSkill{Name: "later"}, true)
	cache.Flush()
	if got := cache.saveCount.Load(); got != 2 {
		t.Fatalf("saveCount after a later Put = %d, want 2", got)
	}
	if f := readCacheFile(t, dir); len(f.Entries) != n+1 {
		t.Fatalf("file has %d entries, want %d", len(f.Entries), n+1)
	}
}

// TestGitHubResolutionCache_DelayedWriteFires checks that the delayed write
// runs on its own, without an explicit Flush.
func TestGitHubResolutionCache_DelayedWriteFires(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, time.Hour, WithResolutionCacheSaveDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	flushed := make(chan struct{}, 1)
	cache.onFlush = func() { flushed <- struct{}{} }

	cache.putEntry("gh://o/r/s@main", ResolvedSkill{Name: "s"}, true)
	// The timeout only turns a missing write into a failure instead of a
	// hang; the test does not depend on timing to pass.
	select {
	case <-flushed:
	case <-time.After(30 * time.Second):
		t.Fatal("delayed write did not run")
	}

	if f := readCacheFile(t, dir); len(f.Entries) != 1 {
		t.Fatalf("file has %d entries, want 1", len(f.Entries))
	}
}

// TestGitHubResolutionCache_SaveDelayOption checks the save delay a cache
// is built with: the default without options, the option's value when
// positive, and the default again for a non-positive value.
func TestGitHubResolutionCache_SaveDelayOption(t *testing.T) {
	cases := []struct {
		name string
		opts []ResolutionCacheOption
		want time.Duration
	}{
		{"default", nil, DefaultResolutionCacheSaveDelay},
		{"set", []ResolutionCacheOption{WithResolutionCacheSaveDelay(time.Minute)}, time.Minute},
		{"zero keeps default", []ResolutionCacheOption{WithResolutionCacheSaveDelay(0)}, DefaultResolutionCacheSaveDelay},
		{"negative keeps default", []ResolutionCacheOption{WithResolutionCacheSaveDelay(-time.Second)}, DefaultResolutionCacheSaveDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, err := NewGitHubResolutionCache(t.TempDir(), time.Hour, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if cache.saveDelay != tc.want {
				t.Fatalf("saveDelay = %v, want %v", cache.saveDelay, tc.want)
			}
		})
	}
}

func TestGitHubSkillResolver_CredentialForURI(t *testing.T) {
	r := &GitHubSkillResolver{
		token: "default-value",
		provisionCredentials: map[string]string{
			"NAMED":            "named-value",
			"GH_ACME__PRIVATE": "repo-value",
			"GH_ACME":          "owner-value",
		},
	}
	cases := []struct {
		uri, want string
	}{
		{"gh://acme/other/s?token=NAMED", "named-value"},
		{"gh://acme/other/s?token=MISSING", ""},
		{"gh://acme/private/s", "repo-value"},
		{"gh://acme/other/s", "owner-value"},
		{"gh://someone/repo/s", "default-value"},
		{"not a uri", ""},
	}
	for _, tc := range cases {
		if got := r.CredentialForURI(tc.uri); got != tc.want {
			t.Errorf("CredentialForURI(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}

func TestGitHubDownloadToken(t *testing.T) {
	base := ContextWithGitHubToken(context.Background(), "default-value")
	lookup := func(uri string) string {
		if uri == "gh://acme/private/s?token=NAMED" {
			return "named-value"
		}
		return ""
	}
	withLookup := ContextWithGitHubCredentialLookup(base, lookup)

	marked := ResolvedSkill{githubCredentialRef: "gh://acme/private/s?token=NAMED"}
	if got := gitHubDownloadToken(withLookup, marked); got != "named-value" {
		t.Errorf("marked skill with lookup: got %q, want named-value", got)
	}
	if got := gitHubDownloadToken(base, marked); got != "default-value" {
		t.Errorf("marked skill without lookup: got %q, want default-value", got)
	}
	if got := gitHubDownloadToken(withLookup, ResolvedSkill{}); got != "default-value" {
		t.Errorf("unmarked skill: got %q, want default-value", got)
	}
	unknown := ResolvedSkill{githubCredentialRef: "gh://acme/other/s"}
	if got := gitHubDownloadToken(withLookup, unknown); got != "default-value" {
		t.Errorf("lookup with no credential: got %q, want default-value", got)
	}
}

// TestGitHubSkillResolver_MarksContentlessCacheHit checks that a skill served
// from a disk-loaded entry (no file content) carries the request's URI for
// the install-time credential lookup, and that the URI names only the secret
// and holds no credential value.
func TestGitHubSkillResolver_MarksContentlessCacheHit(t *testing.T) {
	dir := t.TempDir()
	const uri = "gh://acme/private/s?token=NAMED"
	const credential = "named-value-0123456789"
	ghRef, err := ParseGitHubSkillURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	key := resolutionCacheKey(ghRef, credential)
	now := time.Now()
	writeCacheFile(t, dir, map[string]*resolutionCacheEntry{
		key: {Skill: ResolvedSkill{Name: "s", URI: uri, Files: []ResolvedFile{{Path: "SKILL.md", URL: "https://raw.githubusercontent.com/acme/private/x/SKILL.md"}}},
			CachedAt: now, ExpiresAt: now.Add(time.Hour), IsBranchRef: true},
	})
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := &GitHubSkillResolver{
		token:                "default-value",
		provisionCredentials: map[string]string{"NAMED": credential},
		resolutionCache:      cache,
	}
	ctx := r.WithInstallCredentials(context.Background(), "")
	res, err := r.Resolve(ctx, []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil || len(res.Errors) != 0 || len(res.Resolved) != 1 {
		t.Fatalf("Resolve: err=%v result=%+v", err, res)
	}
	got := res.Resolved[0]
	if got.githubCredentialRef != uri {
		t.Errorf("githubCredentialRef = %q, want %q", got.githubCredentialRef, uri)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), credential) {
		t.Error("resolved skill prints the credential value")
	}
	if r.CredentialForURI(got.githubCredentialRef) != credential {
		t.Error("install-time lookup does not find the credential used for resolution")
	}

	// An entry that still has its content is not marked.
	cache.putEntry(key, ResolvedSkill{Name: "s", URI: uri, Files: []ResolvedFile{{Path: "SKILL.md", Content: []byte("x")}}}, true)
	res, err = r.Resolve(ctx, []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil || len(res.Resolved) != 1 {
		t.Fatalf("Resolve: err=%v result=%+v", err, res)
	}
	if res.Resolved[0].githubCredentialRef != "" {
		t.Errorf("skill with content marked for lookup: %q", res.Resolved[0].githubCredentialRef)
	}
}

// TestNewGitHubSkillResolverWithCredentials_UsesSingletonOnly checks that a
// resolver built with the broker's cache does not open the default cache as
// well: the default cache file, which a load would rewrite (it holds an
// expired entry), must be left untouched.
func TestNewGitHubSkillResolverWithCredentials_UsesSingletonOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultDir, err := GitHubResolutionCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(defaultDir, home) {
		t.Fatalf("default cache dir %q is not under the test HOME", defaultDir)
	}
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	before := writeCacheFile(t, defaultDir, map[string]*resolutionCacheEntry{
		"gh://o/r/s@main": {Skill: ResolvedSkill{Name: "s"}, CachedAt: old, ExpiresAt: old.Add(time.Minute), IsBranchRef: true},
	})

	singleton, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := NewGitHubSkillResolverWithCredentials("tok", nil, singleton)
	if r.resolutionCache != singleton {
		t.Fatal("resolver does not use the cache passed in")
	}
	after, err := os.ReadFile(filepath.Join(defaultDir, resolutionCacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("default cache file was rewritten although a singleton cache was passed in")
	}

	// Control: with no cache passed in, the default cache is opened, and its
	// load drops the expired entry and rewrites the file.
	r = NewGitHubSkillResolverWithCredentials("tok", nil, nil)
	if r.resolutionCache == nil {
		t.Fatal("no default cache opened")
	}
	after, err = os.ReadFile(filepath.Join(defaultDir, resolutionCacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("control: default cache file was not rewritten on load")
	}
}

// newStaleCacheForClose returns a cache in dir whose entries are stale as
// soon as they are written (negative TTL), holding a branch-ref entry for
// key with Version "v1", so the next ResolveWithFetch for key serves it and
// starts a background refresh.
func newStaleCacheForClose(t *testing.T, dir, key string) *GitHubResolutionCache {
	t.Helper()
	cache, err := newTestResolutionCache(dir, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cache.putEntry(key, ResolvedSkill{Name: "s", URI: key, Version: "v1"}, true)
	return cache
}

// TestGitHubResolutionCache_CloseWaitsForRefresh checks that Close waits
// for a background refresh that is still running and then writes its
// result to disk.
func TestGitHubResolutionCache_CloseWaitsForRefresh(t *testing.T) {
	const key = "gh://o/r/s@main"
	dir := t.TempDir()
	cache := newStaleCacheForClose(t, dir, key)

	started := make(chan struct{})
	release := make(chan struct{})
	fetch := func(context.Context) (ResolvedSkill, error) {
		close(started)
		<-release
		return ResolvedSkill{Name: "s", URI: key, Version: "v2"}, nil
	}
	got, err := cache.ResolveWithFetch(context.Background(), key, "flight", "cred", "ref", true, nil, fetch)
	if err != nil || got.Version != "v1" {
		t.Fatalf("ResolveWithFetch = %+v, %v; want the stale v1 entry", got, err)
	}
	<-started

	closed := make(chan error, 1)
	go func() { closed <- cache.Close(context.Background()) }()
	// Close must still be waiting while the refresh is held. The short wait
	// gives a Close that does not wait time to return and fail the test; a
	// correct Close passes however long it is.
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a refresh was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return after the refresh finished")
	}

	f := readCacheFile(t, dir)
	entry, ok := f.Entries[key]
	if !ok || entry.Skill.Version != "v2" {
		t.Fatalf("file entry = %+v, want the refreshed v2 entry", entry)
	}
}

// TestGitHubResolutionCache_CloseStopsWaitingOnContext checks that Close
// returns ctx.Err() once ctx is done, with a refresh still running, and
// still writes the pending entries.
func TestGitHubResolutionCache_CloseStopsWaitingOnContext(t *testing.T) {
	const key = "gh://o/r/s@main"
	dir := t.TempDir()
	cache := newStaleCacheForClose(t, dir, key)

	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	fetch := func(context.Context) (ResolvedSkill, error) {
		close(started)
		<-release
		return ResolvedSkill{}, errors.New("not reached by the assertions")
	}
	if _, err := cache.ResolveWithFetch(context.Background(), key, "flight", "cred", "ref", true, nil, fetch); err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cache.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
	f := readCacheFile(t, dir)
	if entry, ok := f.Entries[key]; !ok || entry.Skill.Version != "v1" {
		t.Fatalf("file entry = %+v, want the pending v1 entry", entry)
	}
}

// TestGitHubResolutionCache_NoRefreshAfterClose checks that after Close a
// stale entry is still served but no background refresh is started, and
// that a synchronous resolution still works.
func TestGitHubResolutionCache_NoRefreshAfterClose(t *testing.T) {
	const key = "gh://o/r/s@main"
	cache := newStaleCacheForClose(t, t.TempDir(), key)
	if err := cache.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var refreshStarted atomic.Int32
	hook := func(_ string, started bool) {
		if started {
			refreshStarted.Add(1)
		}
	}
	staleServeHook.Store(&hook)
	t.Cleanup(func() { staleServeHook.Store(nil) })

	var fetches atomic.Int32
	fetch := func(context.Context) (ResolvedSkill, error) {
		fetches.Add(1)
		return ResolvedSkill{Name: "s", URI: key, Version: "v2"}, nil
	}
	got, err := cache.ResolveWithFetch(context.Background(), key, "flight", "cred", "ref", true, nil, fetch)
	if err != nil || got.Version != "v1" {
		t.Fatalf("ResolveWithFetch = %+v, %v; want the stale v1 entry", got, err)
	}
	if n := refreshStarted.Load(); n != 0 {
		t.Fatalf("%d refreshes started after Close, want 0", n)
	}
	if n := fetches.Load(); n != 0 {
		t.Fatalf("fetch ran %d times after Close, want 0", n)
	}

	// A commit-SHA ref is never served stale, so it resolves synchronously.
	const shaKey = "gh://o/r/s@0123456789abcdef0123456789abcdef01234567"
	if _, err := cache.ResolveWithFetch(context.Background(), shaKey, "flight-sha", "cred", "ref", false, nil, fetch); err != nil {
		t.Fatalf("synchronous resolution after Close: %v", err)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("fetch ran %d times for the synchronous resolution, want 1", n)
	}
}

// TestGitHubResolutionCache_RepeatedCloseSharesOneWait checks that Close
// calls returning on ctx while a refresh runs all use the same wait for the
// refreshes, rather than each leaving a goroutine of its own behind, and
// that the wait ends once the refresh does.
func TestGitHubResolutionCache_RepeatedCloseSharesOneWait(t *testing.T) {
	const key = "gh://o/r/s@main"
	cache := newStaleCacheForClose(t, t.TempDir(), key)

	started := make(chan struct{})
	release := make(chan struct{})
	fetch := func(context.Context) (ResolvedSkill, error) {
		close(started)
		<-release
		return ResolvedSkill{}, errors.New("released")
	}
	if _, err := cache.ResolveWithFetch(context.Background(), key, "flight", "cred", "ref", true, nil, fetch); err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var first <-chan struct{}
	for i := 0; i < 3; i++ {
		if err := cache.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Close %d = %v, want context.Canceled", i, err)
		}
		done := cache.refreshesDone()
		if i == 0 {
			first = done
		} else if done != first {
			t.Fatalf("Close %d waits on a new channel; each Close starts its own wait", i)
		}
	}

	close(release)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not end after the refresh finished")
	}
	if err := cache.Close(context.Background()); err != nil {
		t.Fatalf("Close after the refresh finished = %v", err)
	}
}
