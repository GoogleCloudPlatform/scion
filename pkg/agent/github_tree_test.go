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
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestGitHubSkillTreePath(t *testing.T) {
	got := GitHubSkillTreePath("acme", "skills", "skills/my tool", testCommitSHA)
	want := "/repos/acme/skills/git/trees/" + testCommitSHA + ":skills/my%20tool?recursive=1"
	if got != want {
		t.Fatalf("GitHubSkillTreePath = %q, want %q", got, want)
	}
}

func TestDecodeGitHubSkillTree(t *testing.T) {
	body := `{"sha":"t","truncated":false,"tree":[
		{"path":"SKILL.md","mode":"100644","type":"blob","sha":"a","size":3},
		{"path":"scripts","mode":"040000","type":"tree","sha":"b"},
		{"path":"scripts/run.sh","mode":"100755","type":"blob","sha":"c","size":4},
		{"path":"scripts/odd.sh","mode":"100754","type":"blob","sha":"d","size":5},
		{"path":"link","mode":"120000","type":"blob","sha":"e","size":7}
	]}`
	got, err := DecodeGitHubSkillTree(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []GitHubTreeFile{
		{Path: "SKILL.md", SHA: "a", Size: 3, Mode: "0644"},
		{Path: "scripts/run.sh", SHA: "c", Size: 4, Mode: "0755"},
		// Any execute bit normalises to 0755, as ptone/scion#5657 does.
		{Path: "scripts/odd.sh", SHA: "d", Size: 5, Mode: "0755"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DecodeGitHubSkillTree =\n%+v\nwant\n%+v", got, want)
	}
}

func TestDecodeGitHubSkillTree_Rejects(t *testing.T) {
	blob := func(path string) string {
		return `{"truncated":false,"tree":[{"path":` + quoteJSON(path) + `,"mode":"100644","type":"blob","sha":"a","size":1}]}`
	}
	cases := []struct {
		name string
		body string
		want error
	}{
		{"truncated", `{"truncated":true,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":"a"}]}`, ErrGitHubTreeTruncated},
		{"submodule", `{"truncated":false,"tree":[{"path":"vendor","mode":"160000","type":"commit","sha":"a"}]}`, ErrGitHubTreeSubmodule},
		{"empty path", blob(""), ErrGitHubTreeUnsafePath},
		{"absolute", blob("/etc/passwd"), ErrGitHubTreeUnsafePath},
		{"parent", blob("../SKILL.md"), ErrGitHubTreeUnsafePath},
		{"inner parent", blob("a/../../b"), ErrGitHubTreeUnsafePath},
		{"dot segment", blob("./SKILL.md"), ErrGitHubTreeUnsafePath},
		{"empty segment", blob("a//b"), ErrGitHubTreeUnsafePath},
		{"trailing slash", blob("a/"), ErrGitHubTreeUnsafePath},
		{"backslash", blob(`a\\..\\b`), ErrGitHubTreeUnsafePath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := DecodeGitHubSkillTree(strings.NewReader(tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v (files %+v)", err, tc.want, files)
			}
		})
	}
}

// manyBlobsTree is a git trees API response listing n regular files plus
// a symlink, which does not count toward MaxGitHubSkillFiles.
func manyBlobsTree(n int) string {
	var b strings.Builder
	b.WriteString(`{"truncated":false,"tree":[{"path":"link","mode":"120000","type":"blob","sha":"l","size":1}`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `,{"path":"f%04d.md","mode":"100644","type":"blob","sha":"a","size":1}`, i)
	}
	b.WriteString("]}")
	return b.String()
}

func TestDecodeGitHubSkillTree_FileLimit(t *testing.T) {
	files, err := DecodeGitHubSkillTree(strings.NewReader(manyBlobsTree(MaxGitHubSkillFiles)))
	if err != nil || len(files) != MaxGitHubSkillFiles {
		t.Fatalf("at the limit: %d files, err %v; want %d, nil", len(files), err, MaxGitHubSkillFiles)
	}
	_, err = DecodeGitHubSkillTree(strings.NewReader(manyBlobsTree(MaxGitHubSkillFiles + 1)))
	if !errors.Is(err, ErrGitHubSkillTooManyFiles) {
		t.Fatalf("over the limit: err %v, want ErrGitHubSkillTooManyFiles", err)
	}
	if want := "skill has more than 1000 files"; err.Error() != want {
		t.Fatalf("error text %q, want %q", err.Error(), want)
	}
}

// TestGitHubSkillResolver_FileLimitBeforeDownload pins that the local
// resolver refuses a skill over MaxGitHubSkillFiles without downloading
// any of its files.
func TestGitHubSkillResolver_FileLimitBeforeDownload(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	mux.HandleFunc("/repos/owner/repo/git/trees/"+testCommitSHA+":skills/tool", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(manyBlobsTree(MaxGitHubSkillFiles + 1)))
	})
	var downloads atomic.Int64
	mux.HandleFunc("/raw/", func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write([]byte("x"))
	})
	r := newTestGitHubResolver(server)
	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://owner/repo/tool@" + testCommitSHA}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 0 || len(res.Errors) != 1 {
		t.Fatalf("want one error, got %+v", res)
	}
	if msg := res.Errors[0].Message; !strings.Contains(msg, "skill has more than 1000 files") {
		t.Fatalf("error %q does not name the file limit", msg)
	}
	if code := res.Errors[0].Code; code != SkillErrCodeUnsupportedContent {
		t.Fatalf("error code %q, want %q", code, SkillErrCodeUnsupportedContent)
	}
	if n := downloads.Load(); n != 0 {
		t.Fatalf("%d files downloaded before the limit was checked, want 0", n)
	}
}

func TestIsGitHubSkillTreeError(t *testing.T) {
	for _, err := range []error{ErrGitHubTreeSubmodule, ErrGitHubSkillTooManyFiles, ErrGitHubTreeTruncated, ErrGitHubTreeUnsafePath} {
		if !IsGitHubSkillTreeError(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("IsGitHubSkillTreeError(%v) = false, want true", err)
		}
	}
	if IsGitHubSkillTreeError(errors.New("network down")) {
		t.Error("IsGitHubSkillTreeError(other) = true, want false")
	}
}

func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// serveExecTestSkill registers a skill "tool" in owner/repo with a regular
// SKILL.md and an executable scripts/run.sh, at testCommitSHA (ref main).
// treeCalls counts tree listings.
func serveExecTestSkill(mux *http.ServeMux, treeCalls *atomic.Int64) {
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/git/trees/"+testCommitSHA+":skills/tool", func(w http.ResponseWriter, r *http.Request) {
		treeCalls.Add(1)
		if r.URL.Query().Get("recursive") != "1" {
			http.Error(w, "want recursive=1", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"t","truncated":false,"tree":[
			{"path":"SKILL.md","mode":"100644","type":"blob","sha":"a","size":7},
			{"path":"scripts","mode":"040000","type":"tree","sha":"b"},
			{"path":"scripts/run.sh","mode":"100755","type":"blob","sha":"c","size":18},
			{"path":"scripts/link","mode":"120000","type":"blob","sha":"d","size":6}
		]}`))
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/tool/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# tool\n"))
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/tool/scripts/run.sh", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\necho ok\n"))
	})
}

func fileModes(skill ResolvedSkill) map[string]string {
	m := make(map[string]string, len(skill.Files))
	for _, f := range skill.Files {
		m[f.Path] = f.Mode
	}
	return m
}

// TestGitHubSkillResolver_CarriesFileModes is the local half of
// ptone/scion#5781: files at any depth are resolved from one tree listing,
// each with its mode; the symlink is skipped.
func TestGitHubSkillResolver_CarriesFileModes(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var treeCalls atomic.Int64
	serveExecTestSkill(mux, &treeCalls)
	r := newTestGitHubResolver(server)

	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://owner/repo/tool@main"}}, ResolveOpts{})
	if err != nil || len(res.Errors) != 0 || len(res.Resolved) != 1 {
		t.Fatalf("Resolve: err=%v result=%+v", err, res)
	}
	want := map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755"}
	if got := fileModes(res.Resolved[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %v, want %v", got, want)
	}
	if n := treeCalls.Load(); n != 1 {
		t.Fatalf("tree listed %d times, want 1", n)
	}
}

// TestGitHubSkillResolver_ModeOnlyChangeChangesCacheKey pins that a skill
// whose only change is a file's exec bit gets a different skill content
// cache key, so the install re-hydrates (the exec-aware skillCacheKey from
// ptone/scion#5657, now fed by gh:// modes).
func TestGitHubSkillResolver_ModeOnlyChangeChangesCacheKey(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var treeCalls atomic.Int64
	serveExecTestSkill(mux, &treeCalls)
	r := newTestGitHubResolver(server)
	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://owner/repo/tool@main"}}, ResolveOpts{})
	if err != nil || len(res.Resolved) != 1 {
		t.Fatalf("Resolve: err=%v result=%+v", err, res)
	}
	exec := res.Resolved[0]
	plain := exec
	plain.Files = append([]ResolvedFile(nil), exec.Files...)
	for i := range plain.Files {
		plain.Files[i].Mode = "0644"
	}
	if exec.Hash != plain.Hash {
		t.Fatal("test setup: the bundle hash must not depend on modes")
	}
	if skillCacheKey(exec) == skillCacheKey(plain) {
		t.Fatal("a mode-only change kept the same skill cache key")
	}
}

func TestGitHubSkillResolver_RejectsUnusableTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"submodule", `{"truncated":false,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":"a","size":1},{"path":"vendor/lib","mode":"160000","type":"commit","sha":"b"}]}`, "submodule"},
		{"truncated", `{"truncated":true,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":"a","size":1}]}`, "truncated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, mux := newTestGitHubServer(t)
			mux.HandleFunc("/repos/owner/repo/git/trees/"+testCommitSHA+":skills/tool", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			r := newTestGitHubResolver(server)
			res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://owner/repo/tool@" + testCommitSHA}}, ResolveOpts{})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(res.Resolved) != 0 || len(res.Errors) != 1 {
				t.Fatalf("want one error, got %+v", res)
			}
			if msg := res.Errors[0].Message; !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not mention %q", msg, tc.want)
			}
			if code := res.Errors[0].Code; code != SkillErrCodeUnsupportedContent {
				t.Fatalf("error code %q, want %q", code, SkillErrCodeUnsupportedContent)
			}
		})
	}
}

// TestGitHubSkillResolver_ModelessExpiredSHAEntryNotServed pins that a
// mode-less entry for a commit-SHA ref past its TTL is not served when the
// re-resolve fails: SHA-ref entries have no stale window, before or after
// ptone/scion#5781 (the local counterpart of the hub's "expired SHA row is
// not served").
func TestGitHubSkillResolver_ModelessExpiredSHAEntryNotServed(t *testing.T) {
	mux := http.NewServeMux()
	var treeCalls atomic.Int64
	serveExecTestSkill(mux, &treeCalls)
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)

	uri := "gh://owner/repo/tool@" + testCommitSHA
	ghRef, err := ParseGitHubSkillURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.resolutionCache.mu.Lock()
	r.resolutionCache.entries[resolutionCacheKey(ghRef, r.token)] = &resolutionCacheEntry{
		Skill: ResolvedSkill{Name: "tool", URI: uri, Version: "legacy", Files: []ResolvedFile{
			{Path: "SKILL.md", Content: []byte("# tool\n")},
		}},
		CachedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute), IsBranchRef: false,
	}
	r.resolutionCache.mu.Unlock()
	r.cooldown.record(GitHubCooldownIdentity(r.token), clock.Now().Add(time.Minute))

	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 0 || len(res.Errors) != 1 {
		t.Fatalf("want the expired SHA-ref entry not served (one error), got %+v", res)
	}
	if n := treeCalls.Load(); n != 0 {
		t.Fatalf("%d tree calls during the cooldown, want 0", n)
	}
}

// TestGitHubSkillResolver_ModelessCacheEntry covers a local resolution cache
// entry recorded before file modes were carried (ptone/scion#5781): it is
// not served while a re-resolve can replace it, and it is served, as
// before, when the re-resolve fails (here: a rate-limit cooldown).
func TestGitHubSkillResolver_ModelessCacheEntry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cooldown bool
		stale    bool
	}{
		{name: "fresh entry is re-resolved"},
		{name: "stale entry is re-resolved", stale: true},
		{name: "fresh entry served when re-resolve fails", cooldown: true},
		{name: "stale entry served when re-resolve fails", cooldown: true, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			var treeCalls atomic.Int64
			serveExecTestSkill(mux, &treeCalls)
			clock := newFakeClock()
			r := newCooldownTestResolver(t, mux, clock)

			const uri = "gh://owner/repo/tool@main"
			ghRef, err := ParseGitHubSkillURI(uri)
			if err != nil {
				t.Fatal(err)
			}
			key := resolutionCacheKey(ghRef, r.token)
			now := time.Now()
			expires := now.Add(time.Hour)
			if tc.stale {
				expires = now.Add(-time.Minute)
			}
			r.resolutionCache.mu.Lock()
			r.resolutionCache.entries[key] = &resolutionCacheEntry{
				Skill: ResolvedSkill{Name: "tool", URI: uri, Version: "legacy", Files: []ResolvedFile{
					{Path: "SKILL.md", Content: []byte("# tool\n")},
					{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\necho ok\n")},
				}},
				CachedAt: now.Add(-time.Hour), ExpiresAt: expires, IsBranchRef: true,
			}
			r.resolutionCache.mu.Unlock()
			if tc.cooldown {
				r.cooldown.record(GitHubCooldownIdentity(r.token), clock.Now().Add(time.Minute))
			}

			res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
			if err != nil || len(res.Errors) != 0 || len(res.Resolved) != 1 {
				t.Fatalf("Resolve: err=%v result=%+v", err, res)
			}
			got := res.Resolved[0]
			if tc.cooldown {
				if got.Version != "legacy" || treeCalls.Load() != 0 {
					t.Fatalf("want the legacy entry served without a GitHub call, got version %q after %d tree calls", got.Version, treeCalls.Load())
				}
				return
			}
			if got.Version == "legacy" || treeCalls.Load() != 1 {
				t.Fatalf("want a re-resolve, got version %q after %d tree calls", got.Version, treeCalls.Load())
			}
			want := map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755"}
			if m := fileModes(got); !reflect.DeepEqual(m, want) {
				t.Fatalf("modes = %v, want %v", m, want)
			}
		})
	}
}
