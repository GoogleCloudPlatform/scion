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
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

const installTestURI = "gh://acme/private/s@main"

// newInstallTestResolver returns a resolver built the way cmd/create.go
// builds it (default credential, no named credentials) over a cache in a
// fresh directory that already holds a content-less entry for
// installTestURI, as a previous process would have left it. The resolver
// talks to a local test server; apiCalls counts its GitHub API requests.
func newInstallTestResolver(t *testing.T, credential string) (r *GitHubSkillResolver, apiCalls *atomic.Int64) {
	t.Helper()
	// The resolver falls back to the process GITHUB_TOKEN when credential
	// is empty; clear it so the test controls the credential.
	t.Setenv("GITHUB_TOKEN", "")
	const content = "# skill"
	server, mux := newTestGitHubServer(t)
	apiCalls = &atomic.Int64{}
	mux.HandleFunc("/repos/acme/private/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/acme/private/contents/skills/s", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: len(content)},
		})
	})
	mux.HandleFunc("/raw/acme/private/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(content))
	})

	dir := t.TempDir()
	ghRef, err := ParseGitHubSkillURI(installTestURI)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first.putEntry(resolutionCacheKey(ghRef, credential), ResolvedSkill{
		Name: "s", URI: installTestURI,
		Files: []ResolvedFile{{Path: "SKILL.md", URL: "https://raw.githubusercontent.com/acme/private/x/SKILL.md", Content: []byte(content)}},
	}, true)
	first.Flush()

	cache, err := NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r = NewGitHubSkillResolverWithCredentials(credential, nil, cache)
	r.httpClient = server.Client()
	r.apiBase = server.URL
	r.rawBase = server.URL + "/raw"
	return r, apiCalls
}

func resolveOneForTest(t *testing.T, ctx context.Context, r *GitHubSkillResolver) ResolvedSkill {
	t.Helper()
	res, err := r.Resolve(ctx, []api.SkillReference{{URI: installTestURI}}, ResolveOpts{})
	if err != nil || len(res.Errors) != 0 || len(res.Resolved) != 1 {
		t.Fatalf("Resolve: err=%v result=%+v", err, res)
	}
	return res.Resolved[0]
}

// TestCLIPrivateRepoInstallAfterReload follows cmd/create.go: a second
// create, in a new process, within the TTL. The disk entry has no content,
// so install downloads the files; it must do so with the credential used for
// resolution, supplied through WithInstallCredentials.
func TestCLIPrivateRepoInstallAfterReload(t *testing.T) {
	const credential = "cli-env-credential"
	r, apiCalls := newInstallTestResolver(t, credential)

	ctx := ContextWithSkillResolver(context.Background(), r)
	ctx = r.WithInstallCredentials(ctx, credential)
	got := resolveOneForTest(t, ctx, r)

	if n := apiCalls.Load(); n != 0 {
		t.Fatalf("disk entry not used: %d GitHub API calls", n)
	}
	if got.Files[0].Content != nil {
		t.Fatal("expected a content-less disk hit")
	}
	if dl := gitHubDownloadToken(ctx, got); dl != credential {
		t.Fatalf("install download credential = %q, want %q", dl, credential)
	}
	if GitHubTokenFromContext(ctx) != credential {
		t.Error("WithInstallCredentials did not set the default GitHub credential")
	}
}

// TestContentlessHitWithoutInstallCredentialResolvesAgain checks that a
// content-less, credential-scoped entry is not used when the install
// context cannot supply that credential: the ref is resolved again with
// content, so install downloads nothing.
func TestContentlessHitWithoutInstallCredentialResolvesAgain(t *testing.T) {
	const credential = "cli-env-credential"
	cases := []struct {
		name string
		ctx  func(r *GitHubSkillResolver) context.Context
	}{
		{"no lookup", func(*GitHubSkillResolver) context.Context { return context.Background() }},
		{"lookup returns nothing", func(*GitHubSkillResolver) context.Context {
			return ContextWithGitHubCredentialLookup(context.Background(), func(string) string { return "" })
		}},
		{"lookup returns a different credential", func(*GitHubSkillResolver) context.Context {
			return ContextWithGitHubCredentialLookup(context.Background(), func(string) string { return "other" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, apiCalls := newInstallTestResolver(t, credential)
			ctx := tc.ctx(r)

			got := resolveOneForTest(t, ctx, r)
			if apiCalls.Load() == 0 {
				t.Fatal("content-less entry was used; expected a fresh resolution")
			}
			if !hasAllFileContent(got) || len(got.Files) != 1 {
				t.Fatalf("fresh resolution has no content: %+v", got.Files)
			}
			if got.githubCredentialRef != "" {
				t.Error("skill with content marked for an install lookup")
			}

			// The fresh result replaced the entry in memory, so the next
			// resolution is served from the cache with content.
			calls := apiCalls.Load()
			again := resolveOneForTest(t, ctx, r)
			if apiCalls.Load() != calls {
				t.Error("second resolution called GitHub again")
			}
			if !hasAllFileContent(again) {
				t.Error("second resolution has no content")
			}
		})
	}
}

// TestContentlessHitWithoutCredentialIsUsed checks that an entry resolved
// without any credential is still used when content-less: install downloads
// it without a credential, exactly as resolution did.
func TestContentlessHitWithoutCredentialIsUsed(t *testing.T) {
	r, apiCalls := newInstallTestResolver(t, "")
	got := resolveOneForTest(t, context.Background(), r)
	if apiCalls.Load() != 0 {
		t.Fatal("public content-less entry was not used")
	}
	if got.Files[0].Content != nil {
		t.Fatal("expected a content-less disk hit")
	}
}

func TestGitHubSkillResolver_FlushCache(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cache.putEntry("gh://o/r/s@main", ResolvedSkill{Name: "s"}, true)
	r := NewGitHubSkillResolverWithCredentials("x", nil, cache)
	r.FlushCache()
	if f := readCacheFile(t, dir); len(f.Entries) != 1 {
		t.Fatalf("file has %d entries after FlushCache, want 1", len(f.Entries))
	}
	(&GitHubSkillResolver{}).FlushCache() // no cache: no-op
}

// TestNewGitHubResolutionCache_DirModeFailureIsNotFatal checks that a cache
// directory whose mode cannot be changed (for example one owned by another
// user) still yields a working cache.
func TestNewGitHubResolutionCache_DirModeFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := chmod
	chmod = func(string, os.FileMode) error { return errors.New("operation not permitted") }
	t.Cleanup(func() { chmod = orig })

	cache, err := NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	cache.putEntry("gh://o/r/s@main", ResolvedSkill{Name: "s"}, true)
	cache.Flush()
	if f := readCacheFile(t, dir); len(f.Entries) != 1 {
		t.Fatalf("file has %d entries, want 1", len(f.Entries))
	}
}
