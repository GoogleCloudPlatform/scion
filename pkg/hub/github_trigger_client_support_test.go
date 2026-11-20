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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestGitHubInstallationTokenCache_HitExpiryInvalidationPermissionsAndNoLogs(t *testing.T) {
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	srv, st := testServer(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	srv.githubTokenCache.now = func() time.Time { return now }

	var mintCount atomic.Int64
	var mintedTokens []string

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":          1,
			"slug":        "tokcache-app",
			"permissions": map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"},
		})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		var req fakeMintRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		n := mintCount.Add(1)
		tok := fmt.Sprintf("ghs_SECRET_CACHE_TOKEN_%d", n)
		mintedTokens = append(mintedTokens, tok)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"token":       tok,
			"expires_at":  now.Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": req.Permissions,
		})
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.AppID = 1
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	srv.mu.Unlock()

	admin := createAssocAdmin(t, st, "tokcache-admin")
	createAssocInstallation(t, st, 7101, "acme", "acme/widgets", "acme/other")

	remoteA := gh.URL + "/acme/widgets"
	projA := createAssocProject(t, st, "tokcache-proj-a", remoteA, "nobody", ptrInt64(7101))
	setAdminBinding(t, st, projA, 7101, normalizedRemoteForTest(remoteA), admin.ID)

	remoteB := gh.URL + "/acme/other"
	projB := createAssocProject(t, st, "tokcache-proj-b", remoteB, "nobody", ptrInt64(7101))
	setAdminBinding(t, st, projB, 7101, normalizedRemoteForTest(remoteB), admin.ID)

	// 1. Hub-internal MintGitHubAppTokenWithPermissions caches by (project_id, permission_set),
	// whereas MintGitHubAppTokenForProject (agent delivery / refresh) and
	// POST /api/v1/projects/{id}/github-status (status check) always mint fresh.
	defaultLikePerms := githubapp.TokenPermissions{Contents: "write", PullRequests: "write", Metadata: "read"}
	tokHubPerm, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), defaultLikePerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_1", tokHubPerm)
	assert.Equal(t, int64(1), mintCount.Load())

	tokHubPermHit, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), defaultLikePerms)
	require.NoError(t, err)
	assert.Equal(t, tokHubPerm, tokHubPermHit)
	assert.Equal(t, int64(1), mintCount.Load(), "MintGitHubAppTokenWithPermissions must reuse cached token before expires_at - 5m")

	// Agent delivery / refresh leg (MintGitHubAppTokenForProject) must stay uncached
	// and mint a fresh token on every call even when defaultLikePerms is warm in cache.
	tokAgent1, _, err := srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, projA.ID))
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_2", tokAgent1)
	assert.Equal(t, int64(2), mintCount.Load(), "agent delivery leg must mint fresh rather than reading cache")

	tokAgent2, _, err := srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, projA.ID))
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_3", tokAgent2)
	assert.NotEqual(t, tokAgent1, tokAgent2)
	assert.Equal(t, int64(3), mintCount.Load(), "second agent delivery call must also mint fresh")

	// Status-check leg (POST /api/v1/projects/{id}/github-status -> handleCheckProjectGitHubStatus)
	// must actively contact GitHub and clear a stale degraded status rather than hitting cache.
	degradedProj := reloadProject(t, st, projA.ID)
	degradedProj.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:     store.GitHubAppStateDegraded,
		ErrorCode: githubapp.ErrCodePermissionDenied,
	}
	require.NoError(t, st.UpdateProject(ctx, degradedProj))

	statusRR := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+projA.ID+"/github-status", nil)
	require.Equal(t, http.StatusOK, statusRR.Code, statusRR.Body.String())
	assert.Equal(t, int64(4), mintCount.Load(), "handleCheckProjectGitHubStatus must contact GitHub even with warm cache")
	checkedProj := reloadProject(t, st, projA.ID)
	require.NotNil(t, checkedProj.GitHubAppStatus)
	assert.Equal(t, store.GitHubAppStateOK, checkedProj.GitHubAppStatus.State)

	// 2. Distinct permission sets on the same project do not collide in cache.
	commentPerms := githubapp.TokenPermissions{Issues: "write", Metadata: "read"}
	enrichPerms := githubapp.TokenPermissions{PullRequests: "read", Metadata: "read"}

	tokComment, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_5", tokComment)
	assert.Equal(t, int64(5), mintCount.Load())

	tokEnrich, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), enrichPerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_6", tokEnrich)
	assert.Equal(t, int64(6), mintCount.Load())

	// Re-reading both explicit permission sets hits their respective cache entries.
	tokCommentHit, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, tokComment, tokCommentHit)
	tokEnrichHit, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), enrichPerms)
	require.NoError(t, err)
	assert.Equal(t, tokEnrich, tokEnrichHit)
	assert.Equal(t, int64(6), mintCount.Load())

	// Mint projB as well so we can verify per-project invalidation leaves projB untouched.
	tokB, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projB.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_7", tokB)
	assert.Equal(t, int64(7), mintCount.Load())

	// 3. Expiry at expires_at - 5m boundary: at +54m59s still valid; at +55m00s refreshes.
	now = now.Add(54*time.Minute + 59*time.Second)
	tokBeforeMargin, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, tokComment, tokBeforeMargin)
	assert.Equal(t, int64(7), mintCount.Load())

	now = now.Add(time.Second) // exactly expires_at - 5m
	tokRefreshed, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_8", tokRefreshed)
	assert.Equal(t, int64(8), mintCount.Load(), "token at expires_at - 5m must mint fresh")

	// 4. invalidateGitHubAssociation(projA.ID) drops projA's cached tokens while
	// keeping projB's fresh token (if still before projB's margin).
	// Mint projB fresh at current `now` first so projB has a full 1h window.
	tokBFresh, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projB.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_9", tokBFresh)
	assert.Equal(t, int64(9), mintCount.Load())

	srv.invalidateGitHubAssociation(projA.ID)

	// projB still hits cache; projA misses and mints fresh.
	tokBHit, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projB.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, tokBFresh, tokBHit)
	assert.Equal(t, int64(9), mintCount.Load())

	tokAAfterInvalidate, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_10", tokAAfterInvalidate)
	assert.Equal(t, int64(10), mintCount.Load())

	// 5. Under a read-only grant (pull-only creator proof):
	//    - any write permission request is refused with ErrGitHubWriteNotPermitted (never downgraded);
	//    - a read-only permission request succeeds and returns a repository-scoped token.
	fakeAssoc := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, fakeAssoc)
	createAssocInstallation(t, st, 7102, "acme", "acme/public")
	giveUserGitHubToken(t, srv, st, "pullonly-user", "ghp_pullonly")
	fakeAssoc.grant("ghp_pullonly", "acme/public", githubapp.RepoAccess{Pull: true})
	projPullOnly := createAssocProject(t, st, "tokcache-pullonly", fakeAssoc.remote("acme", "public"), "pullonly-user", ptrInt64(7102))

	_, _, err = srv.MintGitHubAppTokenWithPermissions(ctx, projPullOnly, commentPerms)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGitHubWriteNotPermitted), "got %v, want ErrGitHubWriteNotPermitted", err)
	fakeAssoc.mu.Lock()
	assert.Empty(t, fakeAssoc.mints, "write request under read-only grant must not mint a downgraded token")
	fakeAssoc.mu.Unlock()

	tokReadOnly, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, projPullOnly, enrichPerms)
	require.NoError(t, err)
	assert.Equal(t, "ghs_minted_installation_token", tokReadOnly)
	fakeAssoc.mu.Lock()
	require.Len(t, fakeAssoc.mints, 1)
	assert.Equal(t, []string{"public"}, fakeAssoc.mints[0].Repositories)
	assert.Equal(t, map[string]string{"metadata": "read", "pull_requests": "read"}, fakeAssoc.mints[0].Permissions)
	fakeAssoc.mu.Unlock()

	// 6. No minted installation token (ghs_...) ever reaches any slog line.
	logged := logBuf.String()
	assert.NotContains(t, logged, "ghs_")
	for _, tok := range mintedTokens {
		assert.NotContains(t, logged, tok)
	}
}

func TestServerPostGitHubCommentIdempotent_UsesIdentityBotLoginAndFallback(t *testing.T) {
	ctx := context.Background()
	since := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)

	t.Run("uses G5 identity cache bot login to reject non-App marker spoof", func(t *testing.T) {
		srv, _ := testServer(t)
		var postCalls atomic.Int32

		mux := http.NewServeMux()
		mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":77,"slug":"scion-helper"}`))
		})
		mux.HandleFunc("/repos/acme/widgets/issues/12/comments", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				// Untrusted commenter planted the exact suffix marker first.
				_ = json.NewEncoder(w).Encode([]githubapp.IssueComment{
					{
						ID:      900,
						Body:    "Spoofed\n<!-- scion fire:fire-srv-1 -->",
						HTMLURL: "https://github.com/acme/widgets/issues/12#issuecomment-900",
						User:    githubapp.CommentUser{Login: "mallory", Type: "User"},
					},
				})
			case http.MethodPost:
				postCalls.Add(1)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(githubapp.IssueComment{
					ID:      901,
					Body:    "Real bot reply\n<!-- scion fire:fire-srv-1 -->",
					HTMLURL: "https://github.com/acme/widgets/issues/12#issuecomment-901",
					User:    githubapp.CommentUser{Login: "scion-helper[bot]", Type: "Bot"},
				})
			default:
				http.NotFound(w, r)
			}
		})
		gh := httptest.NewServer(mux)
		t.Cleanup(gh.Close)

		srv.mu.Lock()
		srv.config.GitHubAppConfig.APIBaseURL = gh.URL
		srv.config.GitHubAppConfig.AppID = 77
		srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
		srv.mu.Unlock()

		got, err := srv.PostGitHubCommentIdempotent(ctx, "ghs_install_tok", githubapp.PostCommentOptions{
			Owner:       "acme",
			Repo:        "widgets",
			IssueNumber: 12,
			Body:        "Real bot reply",
			FireID:      "fire-srv-1",
			Attempts:    2,
			Since:       since,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), postCalls.Load(), "spoofed marker by mallory must not suppress bot retry")
		assert.Equal(t, int64(901), got.ID)
	})

	t.Run("falls back to suffix match when App identity is unknown", func(t *testing.T) {
		srv, _ := testServer(t)
		var postCalls atomic.Int32

		mux := http.NewServeMux()
		mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		})
		mux.HandleFunc("/repos/acme/widgets/issues/12/comments", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]githubapp.IssueComment{
					{
						ID:      950,
						Body:    "Earlier delivery\n<!-- scion fire:fire-srv-unknown -->",
						HTMLURL: "https://github.com/acme/widgets/issues/12#issuecomment-950",
						User:    githubapp.CommentUser{Login: "unverified-app[bot]", Type: "Bot"},
					},
				})
			case http.MethodPost:
				postCalls.Add(1)
				w.WriteHeader(http.StatusCreated)
			default:
				http.NotFound(w, r)
			}
		})
		gh := httptest.NewServer(mux)
		t.Cleanup(gh.Close)

		srv.mu.Lock()
		srv.config.GitHubAppConfig.APIBaseURL = gh.URL
		srv.config.GitHubAppConfig.AppID = 77
		srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
		srv.mu.Unlock()

		got, err := srv.PostGitHubCommentIdempotent(ctx, "ghs_install_tok", githubapp.PostCommentOptions{
			Owner:       "acme",
			Repo:        "widgets",
			IssueNumber: 12,
			Body:        "Earlier delivery",
			FireID:      "fire-srv-unknown",
			Attempts:    2,
			Since:       since,
		})
		require.NoError(t, err)
		assert.Zero(t, postCalls.Load(), "when App identity is unknown, suffix match must still deduplicate")
		assert.Equal(t, int64(950), got.ID)
	})
}
