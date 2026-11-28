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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type recordedDriftReq struct {
	method string
	path   string
	query  string
	auth   string
	accept string
}

// httptestDriftGitHub runs a real httptest.Server speaking the GitHub REST
// endpoints used by githubfetch (repo metadata, commits, matching-refs,
// non-recursive git/trees) and adapts it to githubfetch.Doer.
type httptestDriftGitHub struct {
	srv *httptest.Server

	mu            sync.Mutex
	defaultBranch string
	// branches maps branch name -> commit SHA
	branches map[string]string
	// tags maps tag name -> commit SHA
	tags map[string]string
	// trees maps "<commitSHA>" or "<commitSHA>:<subPath>" -> tree SHA
	trees map[string]string
	// override, if non-nil, handles requests before the default mux
	override func(w http.ResponseWriter, r *http.Request) bool

	reqs []recordedDriftReq
}

func newHTTPTestDriftGitHub(t *testing.T) *httptestDriftGitHub {
	t.Helper()
	fg := &httptestDriftGitHub{
		defaultBranch: "main",
		branches:      make(map[string]string),
		tags:          make(map[string]string),
		trees:         make(map[string]string),
	}
	fg.srv = httptest.NewServer(http.HandlerFunc(fg.serveHTTP))
	t.Cleanup(fg.srv.Close)
	return fg
}

func (fg *httptestDriftGitHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	fg.mu.Lock()
	fg.reqs = append(fg.reqs, recordedDriftReq{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		auth:   r.Header.Get("Authorization"),
		accept: r.Header.Get("Accept"),
	})
	override := fg.override
	defaultBranch := fg.defaultBranch
	branches := make(map[string]string, len(fg.branches))
	for k, v := range fg.branches {
		branches[k] = v
	}
	tags := make(map[string]string, len(fg.tags))
	for k, v := range fg.tags {
		tags[k] = v
	}
	trees := make(map[string]string, len(fg.trees))
	for k, v := range fg.trees {
		trees[k] = v
	}
	fg.mu.Unlock()

	if override != nil && override(w, r) {
		return
	}

	// GET /repos/{o}/{r}/git/matching-refs/heads/{first}
	if idx := strings.Index(r.URL.Path, "/git/matching-refs/heads/"); idx >= 0 {
		first := r.URL.Path[idx+len("/git/matching-refs/heads/"):]
		var entries []map[string]any
		for name, sha := range branches {
			if name == first || strings.HasPrefix(name, first+"/") {
				entries = append(entries, map[string]any{
					"ref":    "refs/heads/" + name,
					"object": map[string]string{"type": "commit", "sha": sha},
				})
			}
		}
		if entries == nil {
			entries = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
		return
	}

	// GET /repos/{o}/{r}/git/matching-refs/tags/{first}
	if idx := strings.Index(r.URL.Path, "/git/matching-refs/tags/"); idx >= 0 {
		first := r.URL.Path[idx+len("/git/matching-refs/tags/"):]
		var entries []map[string]any
		for name, sha := range tags {
			if name == first || strings.HasPrefix(name, first+"/") {
				entries = append(entries, map[string]any{
					"ref":    "refs/tags/" + name,
					"object": map[string]string{"type": "commit", "sha": sha},
				})
			}
		}
		if entries == nil {
			entries = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
		return
	}

	// GET /repos/{o}/{r}/commits/{ref}
	if idx := strings.Index(r.URL.Path, "/commits/"); idx >= 0 {
		ref := r.URL.Path[idx+len("/commits/"):]
		if unesc, err := url.PathUnescape(ref); err == nil {
			ref = unesc
		}
		sha, ok := branches[ref]
		if !ok {
			sha, ok = tags[ref]
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.sha")
		_, _ = io.WriteString(w, sha+"\n")
		return
	}

	// GET /repos/{o}/{r}/git/trees/{commitSHA}[:{subPath}]
	if idx := strings.Index(r.URL.Path, "/git/trees/"); idx >= 0 {
		spec := r.URL.Path[idx+len("/git/trees/"):]
		if unesc, err := url.PathUnescape(spec); err == nil {
			spec = unesc
		}
		treeSHA, ok := trees[spec]
		if !ok {
			http.Error(w, "tree not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha":       treeSHA,
			"truncated": false,
			"tree": []map[string]any{
				{"path": "scion-agent.yaml", "mode": "100644", "type": "blob", "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "size": 10},
			},
		})
		return
	}

	// GET /repos/{o}/{r}
	if strings.HasPrefix(r.URL.Path, "/repos/") && strings.Count(strings.Trim(r.URL.Path, "/"), "/") == 2 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"default_branch": defaultBranch,
		})
		return
	}

	http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
}

func (fg *httptestDriftGitHub) Open(ctx context.Context, r githubfetch.Request) (*githubfetch.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	if r.Authorization != "" {
		req.Header.Set("Authorization", r.Authorization)
	}
	for k, vals := range r.Header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	resp, err := fg.srv.Client().Do(req)
	if err != nil {
		return nil, err
	}
	return &githubfetch.Response{
		Status: resp.StatusCode,
		Header: resp.Header.Clone(),
		Body:   resp.Body,
	}, nil
}

func (fg *httptestDriftGitHub) newClient(cd *githubfetch.Cooldown, clock func() time.Time) *githubfetch.Client {
	return githubfetch.New(githubfetch.Options{
		Endpoint: githubfetch.Endpoint{
			APIBase:  fg.srv.URL,
			RawBase:  fg.srv.URL,
			WebHost:  "github.com",
			Codeload: "codeload.github.com",
		},
		Doer:       fg,
		Cooldown:   cd,
		Clock:      clock,
		MaxRetries: 0,
	})
}

func (fg *httptestDriftGitHub) snapshotReqs() []recordedDriftReq {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	out := make([]recordedDriftReq, len(fg.reqs))
	copy(out, fg.reqs)
	return out
}

func (fg *httptestDriftGitHub) callCount() int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return len(fg.reqs)
}

func (fg *httptestDriftGitHub) assertNoContentsOrRecursiveCalls(t *testing.T) {
	t.Helper()
	for _, r := range fg.snapshotReqs() {
		assert.NotContains(t, r.path, "/contents", "drift must not call /contents API")
		if strings.Contains(r.path, "/git/trees/") {
			assert.NotContains(t, r.query, "recursive=1", "drift must use non-recursive ListTreeShallow")
		}
	}
}

func TestTemplateDrift_Caches(t *testing.T) {
	now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	c := &templateDriftChecker{now: func() time.Time { return now }}

	// Commit LRU cache scoped by credential scope
	ck := templateDriftKey{owner: "o", repo: "r", ref: "1111111111111111111111111111111111111111", path: "p", scope: githubfetch.AnonScope}
	_, ok := c.getCommitTree(ck)
	assert.False(t, ok)
	c.putCommitTree(ck, "tree-1")
	got, ok := c.getCommitTree(ck)
	assert.True(t, ok)
	assert.Equal(t, "tree-1", got)

	// Different credential scope must not hit the same entry
	ckOther := ck
	ckOther.scope = "secret:deadbeef"
	_, ok = c.getCommitTree(ckOther)
	assert.False(t, ok)

	// Branch TTL cache (60s)
	bk := templateDriftKey{owner: "o", repo: "r", ref: "main", path: "p", scope: githubfetch.AnonScope}
	c.putBranchTree(bk, templateDriftBranchEntry{
		resolvedRef:  "main",
		resolvedPath: "p",
		commitSHA:    "2222222222222222222222222222222222222222",
		treeSHA:      "tree-b1",
	}, now)
	gotEntry, ok := c.getBranchTree(bk, now.Add(59*time.Second))
	assert.True(t, ok)
	assert.Equal(t, "tree-b1", gotEntry.treeSHA)
	_, ok = c.getBranchTree(bk, now.Add(60*time.Second))
	assert.False(t, ok, "branch cache must expire at 60s")
}

func seedDriftTemplate(t *testing.T, f *flatHubFixture, slug, sourceURL, sourceCommit string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:           api.NewUUID(),
		Name:         slug,
		Slug:         slug,
		Scope:        store.TemplateScopeProject,
		ScopeID:      f.project.ID,
		Status:       store.TemplateStatusActive,
		ContentHash:  "sha256:test-" + slug,
		SourceURL:    sourceURL,
		SourceCommit: sourceCommit,
		Files: []store.TemplateFile{
			{Path: "scion-agent.yaml", Size: 10, Hash: "sha256:abc"},
		},
	}
	require.NoError(t, f.s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

func TestCreateAgent_TemplateDriftWarning(t *testing.T) {
	const (
		commitOld = "abc1234567890abcdef1234567890abcdef12345"
		commitNew = "def678901234567890abcdef1234567890abcdef"
		treeOld   = "1111111111111111111111111111111111111111"
		treeNew   = "2222222222222222222222222222222222222222"
	)

	newFixture := func(t *testing.T, gateOn bool, gitRemote string, labels map[string]string, fg *httptestDriftGitHub, clock func() time.Time) *flatHubFixture {
		t.Helper()
		f := newFlatHubFixture(t, flatHubOpts{})
		f.project.GitRemote = gitRemote
		if labels != nil {
			f.project.Labels = labels
		}
		require.NoError(t, f.s.UpdateProject(context.Background(), f.project))
		setExperimentOverrides(t, f.srv, map[string]bool{experiments.TemplateDriftWarning: gateOn})
		cd := githubfetch.NewCooldown(clock)
		f.srv.ghCooldown = cd
		f.srv.templateDrift.now = clock
		if fg != nil {
			f.srv.templateDrift.client = fg.newClient(cd, clock)
		}
		return f
	}

	t.Run("experiment off makes no outbound calls", func(t *testing.T) {
		fg := newHTTPTestDriftGitHub(t)
		f := newFixture(t, false, "https://github.com/o/r.git", nil, fg, nil)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		rec := f.create(t, map[string]any{"name": "ag-off", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		assert.Zero(t, fg.callCount())
	})

	t.Run("main branch: drift detected and no /contents or recursive calls", func(t *testing.T) {
		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		rec := f.create(t, map[string]any{"name": "ag-drift", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		want := `template "my-tmpl" may be out of date: it was imported from github.com/o/r/.scion/templates/my-tmpl at abc1234, and that folder has changed on "main" since. Run "scion templates update my-tmpl" (or Refresh from Source) to update.`
		assert.Equal(t, []string{want}, resp.Warnings)
		fg.assertNoContentsOrRecursiveCalls(t)
	})

	t.Run("main branch: same commit skips tree calls and produces no warning", func(t *testing.T) {
		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitOld

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		rec := f.create(t, map[string]any{"name": "ag-same-commit", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		// Only matching-refs call; no git/trees calls needed when commit SHA matches
		assert.Equal(t, 1, fg.callCount())
		fg.assertNoContentsOrRecursiveCalls(t)
	})

	t.Run("main branch: newer commit with same folder tree SHA produces no warning", func(t *testing.T) {
		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeOld

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		rec := f.create(t, map[string]any{"name": "ag-fresh", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		fg.assertNoContentsOrRecursiveCalls(t)
	})

	t.Run("slashed branch feat/x/y: drift detected and not detected", func(t *testing.T) {
		t.Run("drift detected", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.branches["feat/x/y"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/feat/x/y/.scion/templates/my-tmpl", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-slash-drift", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			want := `template "my-tmpl" may be out of date: it was imported from github.com/o/r/.scion/templates/my-tmpl at abc1234, and that folder has changed on "feat/x/y" since. Run "scion templates update my-tmpl" (or Refresh from Source) to update.`
			assert.Equal(t, []string{want}, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})

		t.Run("not detected", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.branches["feat/x/y"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeOld

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/feat/x/y/.scion/templates/my-tmpl", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-slash-ok", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Empty(t, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})
	})

	t.Run("tag ref: drift detected and not detected", func(t *testing.T) {
		t.Run("drift detected", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.tags["v1.2.3"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/v1.2.3/.scion/templates/my-tmpl", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-tag-drift", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			want := `template "my-tmpl" may be out of date: it was imported from github.com/o/r/.scion/templates/my-tmpl at abc1234, and that folder has changed on "v1.2.3" since. Run "scion templates update my-tmpl" (or Refresh from Source) to update.`
			assert.Equal(t, []string{want}, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})

		t.Run("not detected", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.tags["v1.2.3"] = commitOld

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/v1.2.3/.scion/templates/my-tmpl", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-tag-ok", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Empty(t, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})
	})

	t.Run("SHA ref never drifts and makes zero outbound calls", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			url  string
		}{
			{"subfolder", "https://github.com/o/r/tree/" + commitOld + "/.scion/templates/my-tmpl"},
			{"repo-root", "https://github.com/o/r/tree/" + commitOld},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fg := newHTTPTestDriftGitHub(t)
				f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
				seedDriftTemplate(t, f, "sha-tmpl", tc.url, commitOld)

				rec := f.create(t, map[string]any{"name": "ag-sha", "task": "t", "template": "sha-tmpl"})
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Empty(t, resp.Warnings)
				assert.Zero(t, fg.callCount(), "SHA-pinned source must not make outbound calls")
			})
		}
	})

	t.Run("repo-root bare URL uses repo default branch (not hardcoded main)", func(t *testing.T) {
		t.Run("drift detected on default branch develop", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.defaultBranch = "develop"
			fg.branches["develop"] = commitNew
			fg.trees[commitOld] = treeOld
			fg.trees[commitNew] = treeNew

			f := newFixture(t, true, "git@github.com:o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "root-tmpl", "https://github.com/o/r", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-root-drift", "task": "t", "template": "root-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			want := `template "root-tmpl" may be out of date: it was imported from github.com/o/r at abc1234, and that folder has changed on "develop" since. Run "scion templates update root-tmpl" (or Refresh from Source) to update.`
			assert.Equal(t, []string{want}, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})

		t.Run("not detected on default branch develop", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.defaultBranch = "develop"
			fg.branches["develop"] = commitOld

			f := newFixture(t, true, "git@github.com:o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "root-tmpl", "https://github.com/o/r", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-root-ok", "task": "t", "template": "root-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Empty(t, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})
	})

	t.Run("bare-path URL stays on main: drift detected and not detected", func(t *testing.T) {
		t.Run("drift detected", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.defaultBranch = "develop" // bare-path must still resolve main, not default_branch
			fg.branches["main"] = commitNew
			fg.trees[commitOld+":templates/my-template"] = treeOld
			fg.trees[commitNew+":templates/my-template"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "bare-tmpl", "https://github.com/o/r/templates/my-template", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-bare-drift", "task": "t", "template": "bare-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			want := `template "bare-tmpl" may be out of date: it was imported from github.com/o/r/templates/my-template at abc1234, and that folder has changed on "main" since. Run "scion templates update bare-tmpl" (or Refresh from Source) to update.`
			assert.Equal(t, []string{want}, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})

		t.Run("not detected", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.branches["main"] = commitNew
			fg.trees[commitOld+":templates/my-template"] = treeOld
			fg.trees[commitNew+":templates/my-template"] = treeOld

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "bare-tmpl", "https://github.com/o/r/templates/my-template", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-bare-ok", "task": "t", "template": "bare-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Empty(t, resp.Warnings)
			fg.assertNoContentsOrRecursiveCalls(t)
		})
	})

	t.Run("non-matching repo makes no outbound calls", func(t *testing.T) {
		fg := newHTTPTestDriftGitHub(t)
		f := newFixture(t, true, "https://github.com/other/repo.git", nil, fg, nil)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		rec := f.create(t, map[string]any{"name": "ag-other", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		assert.Zero(t, fg.callCount())
	})

	t.Run("LabelCloneURL fallback and case-insensitive repo match", func(t *testing.T) {
		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

		f := newFixture(t, true, "", map[string]string{store.LabelCloneURL: "https://github.com/O/R.git"}, fg, nil)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		rec := f.create(t, map[string]any{"name": "ag-label", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Len(t, resp.Warnings, 1)
		fg.assertNoContentsOrRecursiveCalls(t)
	})

	t.Run("empty source_commit makes no outbound calls", func(t *testing.T) {
		fg := newHTTPTestDriftGitHub(t)
		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
		seedDriftTemplate(t, f, "no-sha", "https://github.com/o/r/tree/main/.scion/templates/no-sha", "")

		rec := f.create(t, map[string]any{"name": "ag-nosha", "task": "t", "template": "no-sha"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		assert.Zero(t, fg.callCount())
	})

	t.Run("cache hit avoids repeat calls and branch TTL expiry re-fetches ref only while commit tree stays cached", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		// First create: cold calls (matching-refs + 2 shallow ListTree calls)
		rec1 := f.create(t, map[string]any{"name": "ag-c1", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Len(t, resp1.Warnings, 1)
		coldCalls := fg.callCount()
		assert.Equal(t, 3, coldCalls)

		// Second create within 60s: 0 new calls, warning still returned
		clockMu.Lock()
		now = now.Add(30 * time.Second)
		clockMu.Unlock()
		rec2 := f.create(t, map[string]any{"name": "ag-c2", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
		var resp2 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Len(t, resp2.Warnings, 1)
		assert.Equal(t, coldCalls, fg.callCount())

		// Third create after 60s TTL: ref re-resolved; both commit trees stay cached
		clockMu.Lock()
		now = now.Add(31 * time.Second)
		clockMu.Unlock()
		rec3 := f.create(t, map[string]any{"name": "ag-c3", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())
		assert.Equal(t, coldCalls+1, fg.callCount())
		reqs := fg.snapshotReqs()
		assert.Contains(t, reqs[len(reqs)-1].path, "/git/matching-refs/heads/main")
		fg.assertNoContentsOrRecursiveCalls(t)
	})

	t.Run("rate limit activates shared cooldown for that credential scope only", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew
		fg.override = func(w http.ResponseWriter, r *http.Request) bool {
			// Rate-limit anonymous requests; allow credentialed requests
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("Retry-After", "120")
				http.Error(w, "API rate limit exceeded", http.StatusForbidden)
				return true
			}
			return false
		}

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		// First anonymous create hits 403 rate-limit -> activates shared cooldown for "anon"
		rec1 := f.create(t, map[string]any{"name": "ag-rl-1", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		assert.Equal(t, 1, fg.callCount())
		_, active := f.srv.githubCooldown().Active(githubfetch.AnonScope)
		assert.True(t, active, "rate limit must activate shared cooldown for anon scope")

		// Second anonymous create during cooldown (at +30s, inside both memo and 120s cooldown) -> skipped with 0 new outbound calls
		clockMu.Lock()
		now = now.Add(30 * time.Second)
		clockMu.Unlock()
		rec2 := f.create(t, map[string]any{"name": "ag-rl-2", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
		assert.Equal(t, 1, fg.callCount(), "drift must skip while credential scope is cooling down")

		// Now advance past 60s scope memo TTL (still inside 120s anon cooldown) and configure a project PAT:
		// must NOT be blocked by anon cooldown
		clockMu.Lock()
		now = now.Add(30 * time.Second)
		clockMu.Unlock()
		f.srv.SetSecretBackend(&importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_other_scope_token"},
		})
		rec3 := f.create(t, map[string]any{"name": "ag-rl-3", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())
		var resp3 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &resp3))
		assert.Len(t, resp3.Warnings, 1, "different credential scope must not be blocked by anon cooldown")
		assert.Greater(t, fg.callCount(), 1)

		// Remove PAT (back to anon) and advance past both 60s memo TTL and 120s Retry-After -> anon retries
		f.srv.SetSecretBackend(nil)
		clockMu.Lock()
		now = now.Add(61 * time.Second)
		clockMu.Unlock()
		callsBefore := fg.callCount()
		rec4 := f.create(t, map[string]any{"name": "ag-rl-4", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec4.Code, rec4.Body.String())
		assert.Equal(t, callsBefore+1, fg.callCount(), "anon scope must resume after cooldown expires")
	})

	t.Run("typed error classification for 404, 401, 403 forbidden, and timeout", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			status       int
			wantKind     githubfetch.Kind
			wantCooldown bool
		}{
			{"404 not_found", http.StatusNotFound, githubfetch.KindNotFound, false},
			{"401 auth_rejected", http.StatusUnauthorized, githubfetch.KindAuthRejected, false},
			{"403 forbidden (non-rate-limit)", http.StatusForbidden, githubfetch.KindForbidden, false},
			{"429 rate_limited", http.StatusTooManyRequests, githubfetch.KindRateLimited, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fg := newHTTPTestDriftGitHub(t)
				fg.override = func(w http.ResponseWriter, _ *http.Request) bool {
					http.Error(w, "simulated error", tc.status)
					return true
				}
				f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
				var logBuf bytes.Buffer
				f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
				seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

				rec := f.create(t, map[string]any{"name": "ag-err", "task": "t", "template": "my-tmpl"})
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Empty(t, resp.Warnings)
				assert.Contains(t, logBuf.String(), "kind="+string(tc.wantKind))
				_, active := f.srv.githubCooldown().Active(githubfetch.AnonScope)
				assert.Equal(t, tc.wantCooldown, active)
			})
		}

		t.Run("timeout within budget fails open", func(t *testing.T) {
			var sawCancel atomic.Bool
			fg := newHTTPTestDriftGitHub(t)
			fg.override = func(_ http.ResponseWriter, r *http.Request) bool {
				<-r.Context().Done()
				sawCancel.Store(true)
				return true
			}
			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			f.srv.templateDrift.budget = 50 * time.Millisecond
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

			start := time.Now()
			rec := f.create(t, map[string]any{"name": "ag-timeout", "task": "t", "template": "my-tmpl"})
			elapsed := time.Since(start)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Empty(t, resp.Warnings)
			assert.True(t, sawCancel.Load())
			assert.Less(t, elapsed, 2*time.Second)
		})
	})

	t.Run("credentialed vs anonymous request headers", func(t *testing.T) {
		t.Run("anonymous sends no Authorization header", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.branches["main"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-anon", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			reqs := fg.snapshotReqs()
			require.NotEmpty(t, reqs)
			for _, r := range reqs {
				assert.Empty(t, r.auth, "anonymous drift request must not send Authorization header")
			}
		})

		t.Run("project credential sends Bearer token and follows GH_OWNER__REPO precedence without Info log spam", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.branches["main"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
			var infoBuf bytes.Buffer
			f.srv.resourceLog = slog.New(slog.NewTextHandler(&infoBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			f.srv.SetSecretBackend(&importCredSecretBackend{
				scope:     secret.ScopeProject,
				projectID: f.project.ID,
				values: map[string]string{
					"GITHUB_TOKEN": "ghp_fallback_token",
					"GH_O__R":      "ghp_repo_specific_token",
				},
			})
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

			rec := f.create(t, map[string]any{"name": "ag-auth", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			reqs := fg.snapshotReqs()
			require.NotEmpty(t, reqs)
			for _, r := range reqs {
				assert.Equal(t, "Bearer ghp_repo_specific_token", r.auth)
			}
			assert.NotContains(t, infoBuf.String(), "using project credential", "drift credential lookup must not log at Info level")
			fg.assertNoContentsOrRecursiveCalls(t)
		})
	})

	t.Run("shared source URL at different commits: fresh then stale and stale then fresh within TTL", func(t *testing.T) {
		const sharedURL = "https://github.com/o/r/tree/main/.scion/templates/my-tmpl"

		t.Run("fresh checked first then stale within TTL warns for stale", func(t *testing.T) {
			now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
			var clockMu sync.Mutex
			clock := func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}

			fg := newHTTPTestDriftGitHub(t)
			fg.branches["main"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
			seedDriftTemplate(t, f, "fresh-tmpl", sharedURL, commitNew)
			seedDriftTemplate(t, f, "stale-tmpl", sharedURL, commitOld)

			// 1. Fresh template checked first: resolves ref (headCommit == sourceCommit), no warning
			rec1 := f.create(t, map[string]any{"name": "ag-fresh-1", "task": "t", "template": "fresh-tmpl"})
			require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
			var resp1 CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
			assert.Empty(t, resp1.Warnings)
			assert.Equal(t, 1, fg.callCount())

			// 2. Stale template checked within 60s TTL: must fetch head + source trees and emit warning
			clockMu.Lock()
			now = now.Add(15 * time.Second)
			clockMu.Unlock()
			rec2 := f.create(t, map[string]any{"name": "ag-stale-2", "task": "t", "template": "stale-tmpl"})
			require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
			var resp2 CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
			want := `template "stale-tmpl" may be out of date: it was imported from github.com/o/r/.scion/templates/my-tmpl at abc1234, and that folder has changed on "main" since. Run "scion templates update stale-tmpl" (or Refresh from Source) to update.`
			assert.Equal(t, []string{want}, resp2.Warnings)
			callsAfterStale := fg.callCount()
			assert.Equal(t, 3, callsAfterStale, "stale check should fetch head and source trees without re-resolving ref")

			// 3. Second stale check within TTL: 0 new calls, warning still returned
			clockMu.Lock()
			now = now.Add(15 * time.Second)
			clockMu.Unlock()
			rec3 := f.create(t, map[string]any{"name": "ag-stale-3", "task": "t", "template": "stale-tmpl"})
			require.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())
			var resp3 CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &resp3))
			assert.Equal(t, []string{want}, resp3.Warnings)
			assert.Equal(t, callsAfterStale, fg.callCount())
			fg.assertNoContentsOrRecursiveCalls(t)
		})

		t.Run("stale checked first then fresh within TTL warns for stale and not for fresh", func(t *testing.T) {
			now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
			var clockMu sync.Mutex
			clock := func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}

			fg := newHTTPTestDriftGitHub(t)
			fg.branches["main"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
			seedDriftTemplate(t, f, "fresh-tmpl", sharedURL, commitNew)
			seedDriftTemplate(t, f, "stale-tmpl", sharedURL, commitOld)

			// 1. Stale template checked first: warns
			rec1 := f.create(t, map[string]any{"name": "ag-stale-1", "task": "t", "template": "stale-tmpl"})
			require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
			var resp1 CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
			assert.Len(t, resp1.Warnings, 1)
			callsAfterStale := fg.callCount()
			assert.Equal(t, 3, callsAfterStale)

			// 2. Fresh template checked within 60s TTL: 0 new calls, no warning
			clockMu.Lock()
			now = now.Add(15 * time.Second)
			clockMu.Unlock()
			rec2 := f.create(t, map[string]any{"name": "ag-fresh-2", "task": "t", "template": "fresh-tmpl"})
			require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
			var resp2 CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
			assert.Empty(t, resp2.Warnings)
			assert.Equal(t, callsAfterStale, fg.callCount())
			fg.assertNoContentsOrRecursiveCalls(t)
		})
	})

	t.Run("cache entry populated under PAT scope is never served to anonymous scope", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew
		fg.override = func(w http.ResponseWriter, r *http.Request) bool {
			// Private repo: anonymous requests receive 404
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return true
			}
			return false
		}

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		f.srv.SetSecretBackend(&importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_private_repo_pat"},
		})
		seedDriftTemplate(t, f, "priv-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		// 1. Create with PAT: succeeds and populates PAT-scoped drift caches
		rec1 := f.create(t, map[string]any{"name": "ag-pat-1", "task": "t", "template": "priv-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Len(t, resp1.Warnings, 1)
		callsWithPAT := fg.callCount()
		assert.Equal(t, 3, callsWithPAT)

		// 2. A second anonymous project on the same repo within the 60s TTL:
		// must NOT reuse PAT-scoped cache; must hit fake anonymously (404 -> fail-open, no warning)
		projB := &store.Project{
			ID:        api.NewUUID(),
			Name:      "anon-proj",
			Slug:      "anon-proj",
			GitRemote: "https://github.com/o/r.git",
		}
		require.NoError(t, f.s.CreateProject(context.Background(), projB))
		tmplB := &store.Template{
			ID:           api.NewUUID(),
			Name:         "priv-tmpl",
			Slug:         "priv-tmpl",
			Scope:        store.TemplateScopeProject,
			ScopeID:      projB.ID,
			SourceURL:    "https://github.com/o/r/tree/main/.scion/templates/my-tmpl",
			SourceCommit: commitOld,
		}
		targetB := f.srv.resolveTemplateDriftTarget(projB, tmplB)
		require.NotNil(t, targetB)
		warnB := f.srv.checkTemplateDrift(context.Background(), projB, tmplB, targetB, "ag-anon-2")
		assert.Empty(t, warnB, "anonymous scope must not reuse PAT-scoped drift cache entry")
		assert.Greater(t, fg.callCount(), callsWithPAT, "anonymous check must miss PAT cache and query GitHub")
		reqs := fg.snapshotReqs()
		assert.Empty(t, reqs[len(reqs)-1].auth, "anonymous project request must be anonymous")
	})

	t.Run("branch-key scope isolation even when anonymous commit tree is pre-cached", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew
		fg.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return true
			}
			return false
		}

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		// Pre-cache the anonymous commit tree for sourceCommit so commit-key
		// scoping cannot mask a missing scope on branchKey.
		f.srv.templateDrift.putCommitTree(templateDriftKey{
			owner: "o",
			repo:  "r",
			ref:   commitOld,
			path:  ".scion/templates/my-tmpl",
			scope: githubfetch.AnonScope,
		}, treeOld)

		f.srv.SetSecretBackend(&importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_branch_scope_pat"},
		})
		seedDriftTemplate(t, f, "branch-iso-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		// 1. PAT check on project A populates PAT-scoped branch entry and warns
		rec1 := f.create(t, map[string]any{"name": "ag-biso-1", "task": "t", "template": "branch-iso-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Len(t, resp1.Warnings, 1)
		callsWithPAT := fg.callCount()

		// 2. Anonymous check on project B must miss PAT's branch entry and re-resolve anonymously
		projB := &store.Project{
			ID:        api.NewUUID(),
			Name:      "biso-anon-proj",
			Slug:      "biso-anon-proj",
			GitRemote: "https://github.com/o/r.git",
		}
		require.NoError(t, f.s.CreateProject(context.Background(), projB))
		tmplB := &store.Template{
			ID:           api.NewUUID(),
			Name:         "branch-iso-tmpl",
			Slug:         "branch-iso-tmpl",
			Scope:        store.TemplateScopeProject,
			ScopeID:      projB.ID,
			SourceURL:    "https://github.com/o/r/tree/main/.scion/templates/my-tmpl",
			SourceCommit: commitOld,
		}
		targetB := f.srv.resolveTemplateDriftTarget(projB, tmplB)
		require.NotNil(t, targetB)
		warnB := f.srv.checkTemplateDrift(context.Background(), projB, tmplB, targetB, "ag-biso-2")
		assert.Empty(t, warnB, "anonymous scope must not reuse PAT branch cache entry")
		assert.Greater(t, fg.callCount(), callsWithPAT, "anonymous check must re-resolve ref anonymously")
		reqs := fg.snapshotReqs()
		assert.Empty(t, reqs[len(reqs)-1].auth)
	})

	t.Run("App-installed project mints once on cold drift cache and zero times on warm cache", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		var mintCount atomic.Int64
		appMux := http.NewServeMux()
		appMux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          1,
				"slug":        "drift-test-app",
				"permissions": map[string]string{"contents": "read", "metadata": "read"},
			})
		})
		appMux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
				http.NotFound(w, r)
				return
			}
			n := mintCount.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      fmt.Sprintf("ghs_drift_app_token_%d", n),
				"expires_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
			})
		})
		appSrv := httptest.NewServer(appMux)
		t.Cleanup(appSrv.Close)

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		// Route api.github.com App token minting calls to appSrv while keeping
		// APIBaseURL empty so githubAppWebHost() remains "github.com".
		appURL, err := url.Parse(appSrv.URL)
		require.NoError(t, err)
		origTransport := http.DefaultTransport
		http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "api.github.com" {
				clone := req.Clone(req.Context())
				clone.URL.Scheme = appURL.Scheme
				clone.URL.Host = appURL.Host
				return origTransport.RoundTrip(clone)
			}
			return origTransport.RoundTrip(req)
		})
		t.Cleanup(func() { http.DefaultTransport = origTransport })

		f.srv.mu.Lock()
		f.srv.config.GitHubAppConfig.AppID = 1
		f.srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
		f.srv.mu.Unlock()

		const installID int64 = 7201
		admin := createAssocAdmin(t, f.s, "drift-app-admin")
		createAssocInstallation(t, f.s, installID, "o", "o/r")
		setAdminBinding(t, f.s, f.project, installID, normalizedRemoteForTest("https://github.com/o/r.git"), admin.ID)
		f.project.GitHubInstallationID = ptrInt64(installID)
		require.NoError(t, f.s.UpdateProject(context.Background(), f.project))

		seedDriftTemplate(t, f, "app-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		// 1. Cold drift cache: exactly 1 App token mint and drift warning returned
		rec1 := f.create(t, map[string]any{"name": "ag-app-1", "task": "t", "template": "app-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Len(t, resp1.Warnings, 1)
		assert.Equal(t, int64(1), mintCount.Load(), "cold drift check must mint App token once")
		reqs := fg.snapshotReqs()
		require.NotEmpty(t, reqs)
		assert.Equal(t, "Bearer ghs_drift_app_token_1", reqs[0].auth)
		callsAfterFirst := fg.callCount()

		// 2. Warm drift cache within 60s TTL: 0 additional App token mints and 0 GitHub API calls
		clockMu.Lock()
		now = now.Add(20 * time.Second)
		clockMu.Unlock()
		rec2 := f.create(t, map[string]any{"name": "ag-app-2", "task": "t", "template": "app-tmpl"})
		require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
		var resp2 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Len(t, resp2.Warnings, 1)
		assert.Equal(t, int64(1), mintCount.Load(), "warm drift cache hit must not mint another App token")
		assert.Equal(t, callsAfterFirst, fg.callCount(), "warm drift cache hit must not make GitHub API calls")

		// 3. Refused project B on the same installation (7201) and repo (o/r)
		// must NEVER read project A's app:7201 cache entries.
		fg.mu.Lock()
		fg.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return true
			}
			return false
		}
		fg.mu.Unlock()

		projB := &store.Project{
			ID:                   api.NewUUID(),
			Name:                 "refused-app-proj",
			Slug:                 "refused-app-proj",
			GitRemote:            "https://github.com/o/r.git",
			GitHubInstallationID: ptrInt64(installID),
		}
		require.NoError(t, f.s.CreateProject(context.Background(), projB))
		tmplB := &store.Template{
			ID:           api.NewUUID(),
			Name:         "app-tmpl",
			Slug:         "app-tmpl",
			Scope:        store.TemplateScopeProject,
			ScopeID:      projB.ID,
			SourceURL:    "https://github.com/o/r/tree/main/.scion/templates/my-tmpl",
			SourceCommit: commitOld,
		}
		targetB := f.srv.resolveTemplateDriftTarget(projB, tmplB)
		require.NotNil(t, targetB)
		warnB := f.srv.checkTemplateDrift(context.Background(), projB, tmplB, targetB, "ag-refused-b")
		assert.Empty(t, warnB, "refused project B must never read authorized project A's app:7201 cache entries")
		assert.Greater(t, fg.callCount(), callsAfterFirst, "refused project B must miss app:7201 cache and query anonymously")
		reqsAfterB := fg.snapshotReqs()
		assert.Empty(t, reqsAfterB[len(reqsAfterB)-1].auth)
	})

	t.Run("App unconfigured with installation ID and GITHUB_TOKEN caches under secret scope with zero extra lookups", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		// Installation ID set on project, but AppID is 0 (App not configured).
		f.project.GitHubInstallationID = ptrInt64(8801)
		require.NoError(t, f.s.UpdateProject(context.Background(), f.project))

		credBackend := &importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_fallback_when_app_unconfigured"},
		}
		f.srv.SetSecretBackend(credBackend)
		seedDriftTemplate(t, f, "unconf-app-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		// 1. Cold create resolves GITHUB_TOKEN, caches under secret scope (not app:8801), and warns
		rec1 := f.create(t, map[string]any{"name": "ag-unconf-1", "task": "t", "template": "unconf-app-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Len(t, resp1.Warnings, 1)
		callsAfter1 := fg.callCount()
		secretGetsAfter1 := credBackend.gets
		assert.Greater(t, secretGetsAfter1, 0)

		// Verify no cache entry was stored under app:8801
		f.srv.templateDrift.mu.Lock()
		for k := range f.srv.templateDrift.branchItems {
			assert.NotEqual(t, "app:8801", k.scope, "fallback credential result must never be stored under app:<id>")
		}
		for k := range f.srv.templateDrift.commitItems {
			assert.NotEqual(t, "app:8801", k.scope, "fallback credential commit tree must never be stored under app:<id>")
		}
		f.srv.templateDrift.mu.Unlock()

		// 2. Second create within 60s TTL: 0 API calls and 0 extra credential resolutions
		clockMu.Lock()
		now = now.Add(20 * time.Second)
		clockMu.Unlock()
		rec2 := f.create(t, map[string]any{"name": "ag-unconf-2", "task": "t", "template": "unconf-app-tmpl"})
		require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
		var resp2 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Len(t, resp2.Warnings, 1)
		assert.Equal(t, callsAfter1, fg.callCount(), "second create within TTL must make 0 GitHub API calls")
		assert.Equal(t, secretGetsAfter1, credBackend.gets, "second create within TTL must make 0 extra credential resolutions")
	})

	t.Run("App association refused or mint error memoizes fallback scope with zero extra mints, API calls, or Warns", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		var resourceBuf bytes.Buffer
		f.srv.resourceLog = slog.New(slog.NewTextHandler(&resourceBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

		// App configured with AppID != 0, installation ID set, no valid association -> mint fails and falls back to GITHUB_TOKEN
		f.srv.mu.Lock()
		f.srv.config.GitHubAppConfig.AppID = 999
		f.srv.mu.Unlock()
		f.project.GitHubInstallationID = ptrInt64(9901)
		require.NoError(t, f.s.UpdateProject(context.Background(), f.project))
		f.srv.SetSecretBackend(&importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_fallback_after_refusal"},
		})
		seedDriftTemplate(t, f, "refused-app-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)

		// 1. Cold create: mint fails once (1 Warn), falls back to GITHUB_TOKEN, caches under secret scope, warns
		rec1 := f.create(t, map[string]any{"name": "ag-ref-1", "task": "t", "template": "refused-app-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Len(t, resp1.Warnings, 1)
		assert.Equal(t, 1, strings.Count(resourceBuf.String(), "GitHub App token mint failed"))
		callsAfter1 := fg.callCount()

		// Verify fallback result is never stored under app:9901
		f.srv.templateDrift.mu.Lock()
		for k := range f.srv.templateDrift.branchItems {
			assert.NotEqual(t, "app:9901", k.scope)
		}
		for k := range f.srv.templateDrift.commitItems {
			assert.NotEqual(t, "app:9901", k.scope)
		}
		f.srv.templateDrift.mu.Unlock()

		// 2. Warm create within 60s TTL: 0 extra mints, 0 API calls, 0 additional Warns
		clockMu.Lock()
		now = now.Add(20 * time.Second)
		clockMu.Unlock()
		rec2 := f.create(t, map[string]any{"name": "ag-ref-2", "task": "t", "template": "refused-app-tmpl"})
		require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
		var resp2 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Len(t, resp2.Warnings, 1)
		assert.Equal(t, callsAfter1, fg.callCount(), "warm create within TTL must make 0 GitHub API calls")
		assert.Equal(t, 1, strings.Count(resourceBuf.String(), "GitHub App token mint failed"), "warm create within TTL must not log additional mint-failure Warns")
	})

	t.Run("memo hit then cache miss re-resolves credential and re-scopes on PAT rotation", func(t *testing.T) {
		const (
			commitMid  = "3333333333333333333333333333333333333333"
			commitMid2 = "4444444444444444444444444444444444444444"
		)
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew
		fg.trees[commitMid+":.scion/templates/my-tmpl"] = "5555555555555555555555555555555555555555"
		fg.trees[commitMid2+":.scion/templates/my-tmpl"] = "6666666666666666666666666666666666666666"
		fg.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return true
			}
			return false
		}

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		f.srv.SetSecretBackend(&importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_pat_one"},
		})
		seedDriftTemplate(t, f, "tmpl-a", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitOld)
		seedDriftTemplate(t, f, "tmpl-b", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitMid)
		seedDriftTemplate(t, f, "tmpl-c", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitMid2)

		rec := f.create(t, map[string]any{"name": "p-a", "task": "t", "template": "tmpl-a"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		// Memo hit, branch cache hit, commit-tree miss for commitMid: must re-resolve credential.
		clockMu.Lock()
		now = now.Add(10 * time.Second)
		clockMu.Unlock()
		before := fg.callCount()
		rec = f.create(t, map[string]any{"name": "p-b", "task": "t", "template": "tmpl-b"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		reqs := fg.snapshotReqs()
		require.Greater(t, len(reqs), before)
		for _, r := range reqs[before:] {
			assert.Equal(t, "Bearer ghp_pat_one", r.auth, "memo-hit cache miss must use the resolved credential")
		}
		assert.Len(t, resp.Warnings, 1, "memo-hit cache miss must still detect drift on a private repo")

		// Rotate PAT within the memo TTL; cache miss for commitMid2 must re-scope to the new PAT.
		f.srv.SetSecretBackend(&importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_pat_two"},
		})
		oldScope := f.srv.secretCredentialScope("ghp_pat_one")
		newScope := f.srv.secretCredentialScope("ghp_pat_two")
		require.NotEqual(t, oldScope, newScope)
		clockMu.Lock()
		now = now.Add(10 * time.Second)
		clockMu.Unlock()
		rec = f.create(t, map[string]any{"name": "p-c", "task": "t", "template": "tmpl-c"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		f.srv.templateDrift.mu.Lock()
		var gotScopes []string
		for k := range f.srv.templateDrift.commitItems {
			if k.ref == commitMid2 {
				gotScopes = append(gotScopes, k.scope)
			}
		}
		f.srv.templateDrift.mu.Unlock()
		assert.Equal(t, []string{newScope}, gotScopes, "rotated PAT results must be cached under the new PAT scope only")
	})

	t.Run("empty-tree branch entry with cached source tree after head moves still warns", func(t *testing.T) {
		const commitH2 = "7777777777777777777777777777777777777777"
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		fg := newHTTPTestDriftGitHub(t)
		fg.branches["main"] = commitNew
		fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
		fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew
		fg.trees[commitH2+":.scion/templates/my-tmpl"] = "8888888888888888888888888888888888888888"

		f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, clock)
		u := "https://github.com/o/r/tree/main/.scion/templates/my-tmpl"
		seedDriftTemplate(t, f, "stale", u, commitOld)
		seedDriftTemplate(t, f, "fresh2", u, commitH2)

		rec := f.create(t, map[string]any{"name": "e1", "task": "t", "template": "stale"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		fg.mu.Lock()
		fg.branches["main"] = commitH2
		fg.mu.Unlock()
		clockMu.Lock()
		now = now.Add(61 * time.Second)
		clockMu.Unlock()

		rec = f.create(t, map[string]any{"name": "e2", "task": "t", "template": "fresh2"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		clockMu.Lock()
		now = now.Add(5 * time.Second)
		clockMu.Unlock()
		rec = f.create(t, map[string]any{"name": "e3", "task": "t", "template": "stale"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Len(t, resp.Warnings, 1, "empty-tree branch entry must not suppress the stale template warning")
	})
}

func TestTemplateDrift_HostedUsesNetworkVetted(t *testing.T) {
	var hostedLogs bytes.Buffer
	hostedSrv := &Server{
		workstation: false,
		resourceLog: slog.New(slog.NewTextHandler(&hostedLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	assert.Equal(t, remotefetch.NetworkVetted, hostedSrv.templateReimportNetworkMode())

	// Build the drift client through getTemplateDriftClient with an endpoint
	// pointing at a loopback IP on implicit port 443. Only NetworkVetted runs
	// the IP denylist and rejects this with remotefetch.ReasonDeniedAddress;
	// NetworkSystem skips IP vetting and attempts a TCP connect instead.
	loopbackEP := &githubfetch.Endpoint{
		APIBase:  "https://127.0.0.1",
		RawBase:  "https://127.0.0.1",
		WebHost:  "127.0.0.1",
		Codeload: "127.0.0.1",
	}
	hostedSrv.templateDrift.endpoint = loopbackEP
	hostedClient := hostedSrv.getTemplateDriftClient()

	_, _, resolveErr := hostedClient.ResolveRef(context.Background(), githubfetch.Source{
		Owner: "o",
		Repo:  "r",
		Ref:   "main",
	}, githubfetch.Credential{})
	require.Error(t, resolveErr)
	var ghe *githubfetch.Error
	require.ErrorAs(t, resolveErr, &ghe)
	assert.Equal(t, githubfetch.KindUnreachable, ghe.Kind)
	assert.Contains(t, hostedLogs.String(), "reason="+string(remotefetch.ReasonDeniedAddress),
		"hosted drift client built via getTemplateDriftClient must enforce NetworkVetted IP denylist")

	// Contrast: workstation mode uses NetworkSystem (no IP denylist refusal).
	var wsLogs bytes.Buffer
	wsSrv := &Server{
		workstation: true,
		resourceLog: slog.New(slog.NewTextHandler(&wsLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	assert.Equal(t, remotefetch.NetworkSystem, wsSrv.templateReimportNetworkMode())
	wsSrv.templateDrift.endpoint = loopbackEP
	wsSrv.templateDrift.budget = 250 * time.Millisecond
	wsClient := wsSrv.getTemplateDriftClient()
	_, _, _ = wsClient.ResolveRef(context.Background(), githubfetch.Source{
		Owner: "o",
		Repo:  "r",
		Ref:   "main",
	}, githubfetch.Credential{})
	assert.NotContains(t, wsLogs.String(), "reason="+string(remotefetch.ReasonDeniedAddress),
		"workstation drift client must use NetworkSystem rather than NetworkVetted")
}

func TestTemplateDriftWarning_AdminPolicyClassifiedDeny(t *testing.T) {
	pol, ok := testAdminExperimentPolicy[experiments.TemplateDriftWarning]
	require.True(t, ok, "hub.template_drift_warning must be classified in testAdminExperimentPolicy")
	assert.Equal(t, testAdminPolicyDeny, pol)
}

func TestTemplateDrift_BarePathSourceURL(t *testing.T) {
	const commitSHA = "1111111111111111111111111111111111111111"
	srv, _, project, _ := setupWorkspaceProject(t, "drift-bare-path")
	setExperimentOverrides(t, srv, map[string]bool{experiments.TemplateDriftWarning: true})
	project.GitRemote = "https://github.com/acme/repo.git"
	tmpl := &store.Template{
		SourceURL:    "https://github.com/acme/repo/templates/my-template",
		SourceCommit: commitSHA,
	}
	target := srv.resolveTemplateDriftTarget(project, tmpl)
	require.NotNil(t, target, "bare-path GitHub source URL must resolve for drift")
	assert.Equal(t, "acme", target.src.Owner)
	assert.Equal(t, "repo", target.src.Repo)
	assert.Equal(t, "main", target.src.Ref)
	assert.Equal(t, "templates/my-template", target.src.Path)
	assert.Equal(t, commitSHA, target.sourceCommit)
}
