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

		// Second anonymous create during cooldown -> skipped with 0 new outbound calls
		clockMu.Lock()
		now = now.Add(60 * time.Second)
		clockMu.Unlock()
		rec2 := f.create(t, map[string]any{"name": "ag-rl-2", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
		assert.Equal(t, 1, fg.callCount(), "drift must skip while credential scope is cooling down")

		// Now configure a project PAT (different credential scope): must NOT be blocked by anon cooldown
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

		// Remove PAT (back to anon) and advance past 120s Retry-After -> anon retries
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

		t.Run("project credential sends Bearer token and follows GH_OWNER__REPO precedence", func(t *testing.T) {
			fg := newHTTPTestDriftGitHub(t)
			fg.branches["main"] = commitNew
			fg.trees[commitOld+":.scion/templates/my-tmpl"] = treeOld
			fg.trees[commitNew+":.scion/templates/my-tmpl"] = treeNew

			f := newFixture(t, true, "https://github.com/o/r.git", nil, fg, nil)
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
			fg.assertNoContentsOrRecursiveCalls(t)
		})
	})
}

func TestTemplateDrift_HostedUsesNetworkVetted(t *testing.T) {
	hostedSrv := &Server{workstation: false, resourceLog: slog.Default()}
	assert.Equal(t, remotefetch.NetworkVetted, hostedSrv.templateReimportNetworkMode())

	wsSrv := &Server{workstation: true, resourceLog: slog.Default()}
	assert.Equal(t, remotefetch.NetworkSystem, wsSrv.templateReimportNetworkMode())

	// End-to-end check: a hosted drift doer built via newTemplateGitHubDoer
	// rejects a loopback httptest TLS server because NetworkVetted denies
	// loopback/private IPs.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsSrv.Close)

	u, err := url.Parse(tlsSrv.URL)
	require.NoError(t, err)
	ep := githubfetch.Endpoint{
		APIBase:  tlsSrv.URL,
		RawBase:  tlsSrv.URL,
		WebHost:  u.Hostname(),
		Codeload: u.Hostname(),
	}
	doer := hostedSrv.newTemplateGitHubDoer(ep, hostedSrv.templateDriftLimits())
	client := githubfetch.New(githubfetch.Options{
		Endpoint:   ep,
		Doer:       doer,
		Cooldown:   githubfetch.NewCooldown(nil),
		MaxRetries: 0,
	})
	_, _, resolveErr := client.ResolveRef(context.Background(), githubfetch.Source{
		Owner: "o",
		Repo:  "r",
		Ref:   "main",
	}, githubfetch.Credential{})
	require.Error(t, resolveErr)
	var ghe *githubfetch.Error
	require.ErrorAs(t, resolveErr, &ghe)
	assert.Equal(t, githubfetch.KindUnreachable, ghe.Kind, "hosted drift must enforce NetworkVetted and deny loopback targets")
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
