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

	// 1. Default mint populates cache; second mint is a cache hit (no second GitHub call).
	tok1, _, err := srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, projA.ID))
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_1", tok1)
	assert.Equal(t, int64(1), mintCount.Load())

	tok1Hit, _, err := srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, projA.ID))
	require.NoError(t, err)
	assert.Equal(t, tok1, tok1Hit)
	assert.Equal(t, int64(1), mintCount.Load(), "cached token must be reused before expires_at - 5m")

	// 2. Distinct permission sets on the same project do not collide in cache.
	commentPerms := githubapp.TokenPermissions{Issues: "write", Metadata: "read"}
	enrichPerms := githubapp.TokenPermissions{PullRequests: "read", Metadata: "read"}

	tokComment, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_2", tokComment)
	assert.Equal(t, int64(2), mintCount.Load())

	tokEnrich, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), enrichPerms, false)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_3", tokEnrich)
	assert.Equal(t, int64(3), mintCount.Load())

	// Re-reading both explicit permission sets hits their respective cache entries.
	tokCommentHit, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, tokComment, tokCommentHit)
	tokEnrichHit, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), enrichPerms, false)
	require.NoError(t, err)
	assert.Equal(t, tokEnrich, tokEnrichHit)
	assert.Equal(t, int64(3), mintCount.Load())

	// Mint projB as well so we can verify per-project invalidation leaves projB untouched.
	tokB, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projB.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_4", tokB)
	assert.Equal(t, int64(4), mintCount.Load())

	// 3. Expiry at expires_at - 5m boundary: at +54m59s still valid; at +55m00s refreshes.
	now = now.Add(54*time.Minute + 59*time.Second)
	tokBeforeMargin, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, tokComment, tokBeforeMargin)
	assert.Equal(t, int64(4), mintCount.Load())

	now = now.Add(time.Second) // exactly expires_at - 5m
	tokRefreshed, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_5", tokRefreshed)
	assert.Equal(t, int64(5), mintCount.Load(), "token at expires_at - 5m must mint fresh")

	// 4. invalidateGitHubAssociation(projA.ID) drops projA's cached tokens while
	// keeping projB's fresh token (if still before projB's margin).
	// Mint projB fresh at current `now` first so projB has a full 1h window.
	tokBFresh, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projB.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_6", tokBFresh)
	assert.Equal(t, int64(6), mintCount.Load())

	srv.invalidateGitHubAssociation(projA.ID)

	// projB still hits cache; projA misses and mints fresh.
	tokBHit, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projB.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, tokBFresh, tokBHit)
	assert.Equal(t, int64(6), mintCount.Load())

	tokAAfterInvalidate, _, err := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), commentPerms, true)
	require.NoError(t, err)
	assert.Equal(t, "ghs_SECRET_CACHE_TOKEN_7", tokAAfterInvalidate)
	assert.Equal(t, int64(7), mintCount.Load())

	// 5. Pull-only creator proof refuses requireWrite=true with ErrGitHubWriteNotPermitted.
	fakeAssoc := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, fakeAssoc)
	createAssocInstallation(t, st, 7102, "acme", "acme/public")
	giveUserGitHubToken(t, srv, st, "pullonly-user", "ghp_pullonly")
	fakeAssoc.grant("ghp_pullonly", "acme/public", githubapp.RepoAccess{Pull: true})
	projPullOnly := createAssocProject(t, st, "tokcache-pullonly", fakeAssoc.remote("acme", "public"), "pullonly-user", ptrInt64(7102))

	_, _, err = srv.MintGitHubAppTokenWithPermissions(ctx, projPullOnly, commentPerms, true)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGitHubWriteNotPermitted), "got %v, want ErrGitHubWriteNotPermitted", err)

	// 6. No minted installation token (ghs_...) ever reaches any slog line.
	logged := logBuf.String()
	assert.NotContains(t, logged, "ghs_")
	for _, tok := range mintedTokens {
		assert.NotContains(t, logged, tok)
	}
}
