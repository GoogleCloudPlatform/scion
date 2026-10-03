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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusGitHub is a fake GitHub API whose commits and contents endpoints
// answer with the configured status (200 serves a valid response).
type statusGitHub struct {
	*httptest.Server
	commitStatus   atomic.Int64
	contentsStatus atomic.Int64
	commitCalls    atomic.Int64
	contentsCalls  atomic.Int64
}

func newStatusGitHub(t *testing.T, owner, repo, skillPath, commitSHA string) *statusGitHub {
	t.Helper()
	f := &statusGitHub{}
	f.commitStatus.Store(http.StatusOK)
	f.contentsStatus.Store(http.StatusOK)
	commitsPrefix := "/repos/" + owner + "/" + repo + "/commits/"
	contentsPrefix := "/repos/" + owner + "/" + repo + "/contents/"
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, commitsPrefix):
			f.commitCalls.Add(1)
			if st := int(f.commitStatus.Load()); st != http.StatusOK {
				http.Error(w, `{"message":"No commit found"}`, st)
				return
			}
			_, _ = w.Write([]byte(commitSHA))
		case strings.HasPrefix(r.URL.Path, contentsPrefix):
			f.contentsCalls.Add(1)
			if st := int(f.contentsStatus.Load()); st != http.StatusOK {
				http.Error(w, `{"message":"Not Found"}`, st)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
		default:
			t.Errorf("unexpected GitHub API request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *statusGitHub) calls() int64 { return f.commitCalls.Load() + f.contentsCalls.Load() }

// A ref GitHub reports as not found is asked for once: a second resolve
// request within the TTL gets the same per-URI error without a GitHub call.
// Another ref (another cache key) is still resolved against GitHub.
func TestSkillsResolve_GHNotFoundIsRemembered(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "missing-repo"
		skillPath = "skills/gone"
		uri       = "gh://" + owner + "/" + repo + "/gone@no-such-branch"
		otherURI  = "gh://" + owner + "/" + repo + "/gone@other-branch"
		commitSHA = "abababababababababababababababababababab"
	)

	srv, _, alice, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	gh := newStatusGitHub(t, owner, repo, skillPath, commitSHA)
	gh.commitStatus.Store(http.StatusNotFound)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	resolveErr := func(t *testing.T, u string) ResolveSkillError {
		t.Helper()
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve",
			ResolveSkillsRequest{Skills: []ResolveSkillRef{{URI: u}}, ProjectID: project.ID})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp ResolveSkillsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.Empty(t, resp.Resolved)
		require.Len(t, resp.Errors, 1)
		return resp.Errors[0]
	}

	first := resolveErr(t, uri)
	require.Equal(t, int64(1), gh.calls(), "the first resolve asks GitHub")
	assert.Contains(t, first.Message, "GitHub API error 404")

	second := resolveErr(t, uri)
	assert.Equal(t, int64(1), gh.calls(), "the remembered not found must be served without a GitHub call")
	assert.Equal(t, first, second, "the remembered failure must be reported as the original one")

	resolveErr(t, otherURI)
	assert.Equal(t, int64(2), gh.calls(), "another ref must still be resolved against GitHub")
}

// A skill path that is missing at an existing ref is remembered too.
func TestResolveGitHubSkill_MissingSkillPathIsRemembered(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "path-repo"
		skillPath = "skills/absent"
		uri       = "gh://" + owner + "/" + repo + "/absent@main"
		commitSHA = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	gh := newStatusGitHub(t, owner, repo, skillPath, commitSHA)
	gh.contentsStatus.Store(http.StatusNotFound)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	_, err := srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
	require.Error(t, err)
	require.Equal(t, int64(1), gh.contentsCalls.Load())

	_, err2 := srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
	require.Error(t, err2)
	assert.Equal(t, err.Error(), err2.Error())
	assert.Equal(t, int64(1), gh.commitCalls.Load(), "no new commit lookup")
	assert.Equal(t, int64(1), gh.contentsCalls.Load(), "no new contents lookup")
}

// Statuses other than 404 may succeed on the next attempt, so they are never
// remembered: every resolve asks GitHub again.
func TestResolveGitHubSkill_OtherFailuresAreNotRemembered(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			const (
				owner     = "acme"
				repo      = "flaky-repo"
				skillPath = "skills/flaky"
				uri       = "gh://" + owner + "/" + repo + "/flaky@main"
				commitSHA = "efefefefefefefefefefefefefefefefefefefef"
			)

			srv, _, _, _, project := setupSkillAuthzTest(t)
			srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
			gh := newStatusGitHub(t, owner, repo, skillPath, commitSHA)
			gh.commitStatus.Store(int64(status))
			srv.config.GitHubAppConfig.APIBaseURL = gh.URL
			srv.config.GitHubAppConfig.RawBaseURL = gh.URL

			for i := 0; i < 2; i++ {
				_, err := srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
				require.Error(t, err)
			}
			assert.Equal(t, int64(2), gh.commitCalls.Load(), "each resolve must ask GitHub")
			assert.Empty(t, srv.ghFailures.failures, "nothing may be remembered")
		})
	}
}

// A successful fetch drops a remembered failure for its key.
func TestFetchAndCacheGitHubSkill_SuccessClearsRememberedFailure(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "fixed-repo"
		skillPath = "skills/fixed"
		uri       = "gh://" + owner + "/" + repo + "/fixed@main"
		commitSHA = "1212121212121212121212121212121212121212"
		cacheKey  = "fixed-key"
	)

	srv, _, _, _, _ := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	gh := newStatusGitHub(t, owner, repo, skillPath, commitSHA)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	srv.ghFailures.record(cacheKey, errors.New("remembered"))
	require.Error(t, srv.ghFailures.recent(cacheKey))

	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	entry, err := srv.fetchAndCacheGitHubSkill(context.Background(), cacheKey, uri, ghRef, "", "", true, newGHSHAMemo())
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.NoError(t, srv.ghFailures.recent(cacheKey), "a success must clear the remembered failure")
}

func TestGHFailureCache_RecordSetsTTL(t *testing.T) {
	var c ghFailureCache
	before := time.Now()
	c.record("k", errors.New("not found"))
	after := time.Now()

	got := c.failures["k"].expiresAt
	assert.False(t, got.Before(before.Add(ghFailureCacheTTL)), "expiry %v is earlier than now+TTL", got)
	assert.False(t, got.After(after.Add(ghFailureCacheTTL)), "expiry %v is later than now+TTL", got)
}

func TestGHFailureCache_ExpiredFailureIsNotServed(t *testing.T) {
	var c ghFailureCache
	c.record("k", errors.New("not found"))
	c.failures["k"] = ghRememberedFailure{err: c.failures["k"].err, expiresAt: time.Now().Add(-time.Second)}

	assert.NoError(t, c.recent("k"))
	assert.NotContains(t, c.failures, "k", "an expired entry is dropped when looked up")
}

func TestGHFailureCache_RememberedFailuresAreCapped(t *testing.T) {
	var c ghFailureCache
	for i := 0; i < ghMaxRememberedFailures; i++ {
		c.record(fmt.Sprintf("k%d", i), errors.New("not found"))
	}
	require.Len(t, c.failures, ghMaxRememberedFailures)

	c.record("one-more", errors.New("not found"))
	assert.Len(t, c.failures, ghMaxRememberedFailures)
	assert.NoError(t, c.recent("one-more"), "a new key is not remembered at the cap")

	// An existing key can still be refreshed at the cap.
	c.record("k0", errors.New("again"))
	assert.EqualError(t, c.recent("k0"), "again")

	// Once entries expire, there is room again.
	for k, f := range c.failures {
		c.failures[k] = ghRememberedFailure{err: f.err, expiresAt: time.Now().Add(-time.Second)}
	}
	c.record("after-expiry", errors.New("not found"))
	assert.Error(t, c.recent("after-expiry"))
	assert.Len(t, c.failures, 1)
}

func TestIsGHNotFound(t *testing.T) {
	notFound := &ghStatusError{status: http.StatusNotFound, msg: "GitHub API error 404 resolving r@x: {}"}
	assert.True(t, isGHNotFound(notFound))
	assert.True(t, isGHNotFound(fmt.Errorf("failed to resolve commit SHA: %w", notFound)))
	assert.Equal(t, "GitHub API error 404 resolving r@x: {}", notFound.Error())

	assert.False(t, isGHNotFound(&ghStatusError{status: http.StatusInternalServerError}))
	assert.False(t, isGHNotFound(errors.New("GitHub API error 404")))
	assert.False(t, isGHNotFound(&agent.GitHubRateLimitError{}))
	assert.False(t, isGHNotFound(nil))
}

// A caller that passed the pre-check before a concurrent flight recorded a
// 404 must get the remembered failure from the re-check inside its own
// flight, not ask GitHub again.
func TestResolveGitHubSkill_FlightRecheckServesRememberedFailure(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "race-repo"
		skillPath = "skills/gone"
		uri       = "gh://" + owner + "/" + repo + "/gone@no-such-branch"
		commitSHA = "3434343434343434343434343434343434343434"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	gh := newStatusGitHub(t, owner, repo, skillPath, commitSHA)
	gh.commitStatus.Store(http.StatusNotFound)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	// The first caller to reach the join point (B) is held there; it has
	// already passed the pre-check. Later callers pass straight through.
	held := make(chan struct{})
	release := make(chan struct{})
	var joins atomic.Int64
	hook := func(string) {
		if joins.Add(1) == 1 {
			close(held)
			<-release
		}
	}
	ghFlightJoinHook.Store(&hook)
	t.Cleanup(func() { ghFlightJoinHook.Store(nil) })

	doneB := make(chan error, 1)
	go func() {
		_, err := srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
		doneB <- err
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("caller B never reached the join point")
	}

	// A's flight gets the 404, records it and finishes.
	_, errA := srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
	require.Error(t, errA)
	require.Equal(t, int64(1), gh.commitCalls.Load())

	close(release)
	var errB error
	select {
	case errB = <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("caller B did not return")
	}
	require.Error(t, errB)
	assert.Equal(t, errA.Error(), errB.Error())
	assert.Equal(t, int64(1), gh.commitCalls.Load(), "B's flight must not ask GitHub again")
}

// A stale entry is served ahead of a remembered failure for the same key:
// a background refresh that gets a 404 records it while the stale row is
// still in the store.
func TestResolveGitHubSkill_StaleEntryWinsOverRememberedFailure(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "stale-fail-repo"
		skillPath = "skills/widget"
		uri       = "gh://" + owner + "/" + repo + "/widget@main"
		staleSHA  = "5656565656565656565656565656565656565656"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	gh := newStatusGitHub(t, owner, repo, skillPath, staleSHA)
	gh.commitStatus.Store(http.StatusNotFound)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")

	ctx := context.Background()
	require.NoError(t, srv.ghResolutionStore.Put(ctx, cacheKey, GitHubCacheEntry{
		CommitSHA:   staleSHA,
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.invalid/SKILL.md", Hash: "x", Size: 1}},
		BundleHash:  "sha256:stale",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(-time.Minute),
		OriginalURI: uri,
	}))
	srv.ghFailures.record(cacheKey, errors.New("remembered not found"))

	resp, err := srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.NoError(t, err, "the stale entry must be served, not the remembered failure")
	assert.Equal(t, safeShortSHA(staleSHA), resp.ResolvedVersion)

	// Let the background refresh finish before the test returns.
	_, _, _ = srv.ghResolveFlight.Do(cacheKey, func() (interface{}, error) { return nil, nil })
}

// A 404 remembered for a project with no GitHub App installation (the
// "public" scope) is not served to the same project once it is backed by an
// installation: that is another cache key, so GitHub is asked again.
func TestResolveGitHubSkill_PublicNotFoundNotServedToInstallation(t *testing.T) {
	const (
		instID = int64(515151)
		uri    = "gh://acme/private/s@main"
	)
	srv, s, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	srv.ghCooldown = agent.NewGitHubCooldown(time.Now)

	var repoCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "ghs_test_value",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		repoCalls.Add(1)
		http.NotFound(w, r)
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL
	srv.config.GitHubAppConfig.AppID = 1
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)

	ctx := context.Background()
	_, err := srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.Error(t, err)
	require.Equal(t, int64(1), repoCalls.Load())
	_, err = srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.Error(t, err)
	require.Equal(t, int64(1), repoCalls.Load(), "the public 404 is remembered")

	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))
	id := instID
	project.GitHubInstallationID = &id
	require.NoError(t, s.UpdateProject(ctx, project))

	_, err = srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.Error(t, err)
	assert.Equal(t, int64(2), repoCalls.Load(),
		"an installation-backed resolution must not be served the public scope's remembered 404")
}

// A large 404 body is cut, so each remembered failure stays small.
func TestResolveGitHubSkill_RememberedErrorBodyIsBounded(t *testing.T) {
	const uri = "gh://acme/big-page/s@main"
	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	page := strings.Repeat("<p>not found</p>", 64*1024) // about 1 MiB
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(gh.Close)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	_, err := srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
	require.Error(t, err)
	require.Len(t, srv.ghFailures.failures, 1)
	for _, f := range srv.ghFailures.failures {
		assert.LessOrEqual(t, len(f.err.Error()), maxGHErrorBody+256,
			"a remembered error must not carry the whole response body")
		assert.Contains(t, f.err.Error(), "...")
	}
}

func TestGHErrorBody(t *testing.T) {
	assert.Equal(t, "short", ghErrorBody([]byte("short")))
	exact := strings.Repeat("a", maxGHErrorBody)
	assert.Equal(t, exact, ghErrorBody([]byte(exact)))
	assert.Equal(t, exact+"...", ghErrorBody([]byte(exact+"b")))
}
