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
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type fakeDriftFetcher struct {
	mu       sync.Mutex
	calls    []fakeDriftCall
	handlers func(ctx context.Context, rawURL, auth string) (*remotefetch.Result, error)
}

type fakeDriftCall struct {
	url  string
	auth string
}

func (f *fakeDriftFetcher) FetchBytes(ctx context.Context, rawURL string) (*remotefetch.Result, error) {
	return f.record(ctx, rawURL, "")
}

func (f *fakeDriftFetcher) FetchBytesAuthorized(ctx context.Context, rawURL, authorization string) (*remotefetch.Result, error) {
	return f.record(ctx, rawURL, authorization)
}

func (f *fakeDriftFetcher) record(ctx context.Context, rawURL, auth string) (*remotefetch.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeDriftCall{url: rawURL, auth: auth})
	h := f.handlers
	f.mu.Unlock()
	if h == nil {
		return nil, &remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 404"}
	}
	return h(ctx, rawURL, auth)
}

func (f *fakeDriftFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDriftFetcher) snapshotCalls() []fakeDriftCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeDriftCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func driftContentsJSON(t *testing.T, name, typ, sha string) []byte {
	t.Helper()
	b, err := json.Marshal([]map[string]string{
		{"name": "README.md", "type": "file", "sha": "0000000000000000000000000000000000000000"},
		{"name": name, "type": typ, "sha": sha},
	})
	require.NoError(t, err)
	return b
}

func driftCommitJSON(t *testing.T, treeSHA string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"commit": map[string]any{
			"tree": map[string]any{
				"sha": treeSHA,
			},
		},
	})
	require.NoError(t, err)
	return b
}

func TestTemplateDrift_APIURL(t *testing.T) {
	assert.Equal(t,
		"https://api.github.com/repos/o/r/contents/.scion/templates?ref=main",
		templateDriftAPIURL(&templateGitHubSource{Owner: "o", Repo: "r", Branch: "main", Path: ".scion/templates/my-tmpl"}, "main"),
	)
	assert.Equal(t,
		"https://api.github.com/repos/o/r/contents?ref=main",
		templateDriftAPIURL(&templateGitHubSource{Owner: "o", Repo: "r", Branch: "main", Path: "my-tmpl"}, "main"),
	)
	assert.Equal(t,
		"https://api.github.com/repos/o/r/commits/main",
		templateDriftAPIURL(&templateGitHubSource{Owner: "o", Repo: "r", Branch: "main", Path: ""}, "main"),
	)
	assert.Equal(t,
		"https://api.github.com/repos/o/r/contents/my%20dir/sub+dir?ref=release%2F1",
		templateDriftAPIURL(&templateGitHubSource{Owner: "o", Repo: "r", Branch: "release/1", Path: "my dir/sub+dir/my-tmpl"}, "release/1"),
	)
}

func TestTemplateDrift_CachesAndRateLimit(t *testing.T) {
	now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	c := &templateDriftChecker{now: func() time.Time { return now }}

	// Commit LRU cache
	ck := templateDriftKey{owner: "o", repo: "r", ref: "1111111111111111111111111111111111111111", path: "p"}
	_, ok := c.getCommitTree(ck)
	assert.False(t, ok)
	c.putCommitTree(ck, "tree-1")
	got, ok := c.getCommitTree(ck)
	assert.True(t, ok)
	assert.Equal(t, "tree-1", got)

	// Branch TTL cache (60s)
	bk := templateDriftKey{owner: "o", repo: "r", ref: "main", path: "p"}
	c.putBranchTree(bk, "tree-b1", now)
	got, ok = c.getBranchTree(bk, now.Add(59*time.Second))
	assert.True(t, ok)
	assert.Equal(t, "tree-b1", got)
	_, ok = c.getBranchTree(bk, now.Add(60*time.Second))
	assert.False(t, ok, "branch cache must expire at 60s")

	// Rate-limit negative cache (5m)
	assert.False(t, c.isRateLimited("o/r", now))
	c.recordRateLimit("o/r", now)
	assert.True(t, c.isRateLimited("o/r", now.Add(4*time.Minute+59*time.Second)))
	assert.False(t, c.isRateLimited("o/r", now.Add(5*time.Minute)))

	// Rate-limit error classification
	assert.True(t, isTemplateDriftRateLimited(&remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 403"}))
	assert.True(t, isTemplateDriftRateLimited(&remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 429"}))
	assert.False(t, isTemplateDriftRateLimited(&remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 404"}))
	assert.False(t, isTemplateDriftRateLimited(&remotefetch.Error{Reason: remotefetch.ReasonTimeout, Detail: "deadline"}))
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
		commitSHA = "abc1234567890abcdef1234567890abcdef12345"
		treeOld   = "1111111111111111111111111111111111111111"
		treeNew   = "2222222222222222222222222222222222222222"
	)

	newFixture := func(t *testing.T, gateOn bool, gitRemote string, labels map[string]string, fetcher *fakeDriftFetcher) *flatHubFixture {
		t.Helper()
		f := newFlatHubFixture(t, flatHubOpts{})
		f.project.GitRemote = gitRemote
		if labels != nil {
			f.project.Labels = labels
		}
		require.NoError(t, f.s.UpdateProject(context.Background(), f.project))
		setExperimentOverrides(t, f.srv, map[string]bool{experiments.TemplateDriftWarning: gateOn})
		f.srv.templateDrift.fetcher = fetcher
		return f
	}

	t.Run("experiment off makes no outbound calls", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{}
		f := newFixture(t, false, "https://github.com/o/r.git", nil, fetcher)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

		rec := f.create(t, map[string]any{"name": "ag-off", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		assert.Zero(t, fetcher.callCount())
	})

	t.Run("drift detected appends advisory warning", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{
			handlers: func(_ context.Context, rawURL, _ string) (*remotefetch.Result, error) {
				switch rawURL {
				case "https://api.github.com/repos/o/r/contents/.scion/templates?ref=" + commitSHA:
					return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeOld)}, nil
				case "https://api.github.com/repos/o/r/contents/.scion/templates?ref=main":
					return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeNew)}, nil
				default:
					return nil, &remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 404"}
				}
			},
		}
		f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

		rec := f.create(t, map[string]any{"name": "ag-drift", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		want := `template "my-tmpl" may be out of date: it was imported from github.com/o/r/.scion/templates/my-tmpl at abc1234, and that folder has changed on "main" since. Run "scion templates update my-tmpl" (or Refresh from Source) to update.`
		assert.Equal(t, []string{want}, resp.Warnings)
		assert.Equal(t, 2, fetcher.callCount())
	})

	t.Run("same tree SHA produces no warning", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{
			handlers: func(_ context.Context, rawURL, _ string) (*remotefetch.Result, error) {
				return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeOld)}, nil
			},
		}
		f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

		rec := f.create(t, map[string]any{"name": "ag-fresh", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		assert.Equal(t, 2, fetcher.callCount())
	})

	t.Run("repo-root template compares commit tree SHAs", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{
			handlers: func(_ context.Context, rawURL, _ string) (*remotefetch.Result, error) {
				switch rawURL {
				case "https://api.github.com/repos/o/r/commits/" + commitSHA:
					return &remotefetch.Result{Body: driftCommitJSON(t, treeOld)}, nil
				case "https://api.github.com/repos/o/r/commits/main":
					return &remotefetch.Result{Body: driftCommitJSON(t, treeNew)}, nil
				default:
					return nil, &remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 404"}
				}
			},
		}
		f := newFixture(t, true, "git@github.com:o/r.git", nil, fetcher)
		seedDriftTemplate(t, f, "root-tmpl", "https://github.com/o/r/tree/main", commitSHA)

		rec := f.create(t, map[string]any{"name": "ag-root", "task": "t", "template": "root-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		want := `template "root-tmpl" may be out of date: it was imported from github.com/o/r at abc1234, and that folder has changed on "main" since. Run "scion templates update root-tmpl" (or Refresh from Source) to update.`
		assert.Equal(t, []string{want}, resp.Warnings)
	})

	t.Run("non-matching repo makes no outbound calls", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{}
		f := newFixture(t, true, "https://github.com/other/repo.git", nil, fetcher)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

		rec := f.create(t, map[string]any{"name": "ag-other", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		assert.Zero(t, fetcher.callCount())
	})

	t.Run("LabelCloneURL fallback and case-insensitive repo match", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{
			handlers: func(_ context.Context, rawURL, _ string) (*remotefetch.Result, error) {
				if rawURL == "https://api.github.com/repos/o/r/contents/.scion/templates?ref="+commitSHA {
					return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeOld)}, nil
				}
				return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeNew)}, nil
			},
		}
		f := newFixture(t, true, "", map[string]string{store.LabelCloneURL: "https://github.com/O/R.git"}, fetcher)
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

		rec := f.create(t, map[string]any{"name": "ag-label", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Len(t, resp.Warnings, 1)
	})

	t.Run("empty source_commit makes no outbound calls", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{}
		f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
		seedDriftTemplate(t, f, "no-sha", "https://github.com/o/r/tree/main/.scion/templates/no-sha", "")

		rec := f.create(t, map[string]any{"name": "ag-nosha", "task": "t", "template": "no-sha"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		assert.Zero(t, fetcher.callCount())
	})

	t.Run("cache hit avoids repeat calls and branch TTL expiry re-fetches branch only", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		fetcher := &fakeDriftFetcher{
			handlers: func(_ context.Context, rawURL, _ string) (*remotefetch.Result, error) {
				if rawURL == "https://api.github.com/repos/o/r/contents/.scion/templates?ref="+commitSHA {
					return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeOld)}, nil
				}
				return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeNew)}, nil
			},
		}
		f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
		f.srv.templateDrift.now = func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

		// First create: 2 cold calls (commit + branch)
		rec1 := f.create(t, map[string]any{"name": "ag-c1", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Len(t, resp1.Warnings, 1)
		assert.Equal(t, 2, fetcher.callCount())

		// Second create within 60s: 0 new calls, warning still returned
		clockMu.Lock()
		now = now.Add(30 * time.Second)
		clockMu.Unlock()
		rec2 := f.create(t, map[string]any{"name": "ag-c2", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
		var resp2 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Len(t, resp2.Warnings, 1)
		assert.Equal(t, 2, fetcher.callCount())

		// Third create after 60s TTL: only branch re-fetched (commit stays cached)
		clockMu.Lock()
		now = now.Add(31 * time.Second)
		clockMu.Unlock()
		rec3 := f.create(t, map[string]any{"name": "ag-c3", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())
		assert.Equal(t, 3, fetcher.callCount())
		calls := fetcher.snapshotCalls()
		assert.Equal(t, "https://api.github.com/repos/o/r/contents/.scion/templates?ref=main", calls[2].url)
	})

	t.Run("fail-open on 404, 403/429 with 5m negative cache, and timeout", func(t *testing.T) {
		t.Run("404", func(t *testing.T) {
			fetcher := &fakeDriftFetcher{
				handlers: func(_ context.Context, _, _ string) (*remotefetch.Result, error) {
					return nil, &remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 404"}
				},
			}
			f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

			rec := f.create(t, map[string]any{"name": "ag-404", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Empty(t, resp.Warnings)
		})

		t.Run("403 triggers 5m negative cache", func(t *testing.T) {
			now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
			var clockMu sync.Mutex
			fetcher := &fakeDriftFetcher{
				handlers: func(_ context.Context, _, _ string) (*remotefetch.Result, error) {
					return nil, &remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 403"}
				},
			}
			f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
			f.srv.templateDrift.now = func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

			rec1 := f.create(t, map[string]any{"name": "ag-403-1", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
			assert.Equal(t, 1, fetcher.callCount())

			// Within 5m backoff: 0 calls
			clockMu.Lock()
			now = now.Add(2 * time.Minute)
			clockMu.Unlock()
			rec2 := f.create(t, map[string]any{"name": "ag-403-2", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
			assert.Equal(t, 1, fetcher.callCount())

			// After 5m backoff: retries
			clockMu.Lock()
			now = now.Add(3*time.Minute + time.Second)
			clockMu.Unlock()
			rec3 := f.create(t, map[string]any{"name": "ag-403-3", "task": "t", "template": "my-tmpl"})
			require.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())
			assert.Equal(t, 2, fetcher.callCount())
		})

		t.Run("timeout within budget fails open", func(t *testing.T) {
			var sawCancel atomic.Bool
			fetcher := &fakeDriftFetcher{
				handlers: func(ctx context.Context, _, _ string) (*remotefetch.Result, error) {
					<-ctx.Done()
					sawCancel.Store(true)
					return nil, ctx.Err()
				},
			}
			f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
			f.srv.templateDrift.budget = 50 * time.Millisecond
			seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

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

	t.Run("project credential is sent as Bearer token", func(t *testing.T) {
		fetcher := &fakeDriftFetcher{
			handlers: func(_ context.Context, rawURL, _ string) (*remotefetch.Result, error) {
				if rawURL == "https://api.github.com/repos/o/r/contents/.scion/templates?ref="+commitSHA {
					return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeOld)}, nil
				}
				return &remotefetch.Result{Body: driftContentsJSON(t, "my-tmpl", "dir", treeNew)}, nil
			},
		}
		f := newFixture(t, true, "https://github.com/o/r.git", nil, fetcher)
		f.srv.SetSecretBackend(&importCredSecretBackend{
			scope:     secret.ScopeProject,
			projectID: f.project.ID,
			values:    map[string]string{"GITHUB_TOKEN": "ghp_test_drift_secret"},
		})
		seedDriftTemplate(t, f, "my-tmpl", "https://github.com/o/r/tree/main/.scion/templates/my-tmpl", commitSHA)

		rec := f.create(t, map[string]any{"name": "ag-auth", "task": "t", "template": "my-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		calls := fetcher.snapshotCalls()
		require.Len(t, calls, 2)
		assert.Equal(t, "Bearer ghp_test_drift_secret", calls[0].auth)
		assert.Equal(t, "Bearer ghp_test_drift_secret", calls[1].auth)
	})
}

func TestTemplateDriftWarning_AdminPolicyClassifiedDeny(t *testing.T) {
	pol, ok := testAdminExperimentPolicy[experiments.TemplateDriftWarning]
	require.True(t, ok, "hub.template_drift_warning must be classified in testAdminExperimentPolicy")
	assert.Equal(t, testAdminPolicyDeny, pol)
}
