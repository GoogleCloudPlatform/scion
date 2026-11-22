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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	triggithub "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func signSeamTestWebhook(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestServerGitHubSourceSeams_ReadinessParity(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	var appResp atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(appResp.Load())
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)

	assertParity := func(t *testing.T, label string) {
		t.Helper()
		cfg, _, hasSecret := srv.githubAppCredentialPresence(ctx)
		want := srv.computeGitHubTriggersReadiness(ctx, cfg, hasSecret)
		got, err := srv.GitHubSourceReadiness().Check(ctx)
		require.NoError(t, err, label)
		assert.Equal(t, want.Ready, got.Ready, label+": Ready")
		assert.Equal(t, want.WebhooksEnabled, got.WebhooksEnabled, label+": WebhooksEnabled")
		assert.Equal(t, want.HasWebhookSecret, got.HasWebhookSecret, label+": HasWebhookSecret")
		assert.Equal(t, want.MissingEvents, got.MissingEvents, label+": MissingEvents")
		assert.Equal(t, want.MissingPermissions, got.MissingPermissions, label+": MissingPermissions")
		assert.Equal(t, want.AppSlug, got.AppSlug, label+": AppSlug")
		assert.Equal(t, want.Error, got.Error, label+": Error")
	}

	// 1. Unconfigured (AppID == 0).
	assertParity(t, "unconfigured")

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 4242
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.WebhooksEnabled = true
	srv.config.GitHubAppConfig.WebhookSecret = "seam-secret"
	srv.mu.Unlock()

	// 2. Fully ready.
	appResp.Store(map[string]any{
		"id":   4242,
		"slug": "seam-app",
		"events": []string{
			"issue_comment",
			"issues",
			"pull_request",
			"pull_request_review",
			"pull_request_review_comment",
		},
		"permissions": map[string]string{
			"contents":      "write",
			"issues":        "write",
			"metadata":      "read",
			"pull_requests": "write",
		},
	})
	srv.githubAppIdentity.reset()
	assertParity(t, "fully ready")

	// 3. Partial events & permissions.
	appResp.Store(map[string]any{
		"id":          4242,
		"slug":        "seam-app",
		"events":      []string{"pull_request"},
		"permissions": map[string]string{"issues": "read", "metadata": "read"},
	})
	srv.githubAppIdentity.reset()
	assertParity(t, "partial")

	// 4. Non-nil App identity with empty events and empty permissions (must list every requirement as missing).
	appResp.Store(map[string]any{
		"id":          4242,
		"slug":        "seam-app",
		"events":      []string{},
		"permissions": map[string]string{},
	})
	srv.githubAppIdentity.reset()
	rep, err := srv.GitHubSourceReadiness().Check(ctx)
	require.NoError(t, err)
	assert.False(t, rep.Ready)
	assert.Len(t, rep.MissingEvents, len(githubTriggersRequiredEvents))
	assert.Len(t, rep.MissingPermissions, len(githubTriggersRequiredPermissions))
	assertParity(t, "empty events and permissions")
}

func TestServerGitHubSourceSeams_WireVerifierParity(t *testing.T) {
	srv, _ := testServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)

	secret := "seam-wire-secret"
	body := []byte(`{"action":"created"}`)
	validSig := signSeamTestWebhook(body, secret)
	upperSig := "sha256=" + strings.ToUpper(strings.TrimPrefix(validSig, "sha256="))
	secrets := source.SecretFunc(func(context.Context) (string, error) { return secret, nil })

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 4242
	srv.config.GitHubAppConfig.WebhookSecret = secret
	srv.mu.Unlock()

	makeReq := func(sig, targetType, targetID string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", nil)
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		req.Header.Set("X-GitHub-Event", triggithub.EventIssueComment)
		req.Header.Set("X-GitHub-Delivery", "11111111-2222-3333-4444-555555555555")
		if targetType != "" {
			req.Header.Set("X-GitHub-Hook-Installation-Target-Type", targetType)
		}
		if targetID != "" {
			req.Header.Set("X-GitHub-Hook-Installation-Target-ID", targetID)
		}
		return req
	}

	// Parity table against githubapp.VerifyWebhookSignature.
	sigCases := []struct {
		name      string
		sig       string
		wantValid bool
	}{
		{"valid lowercase", validSig, true},
		{"invalid hex", "sha256=0000", false},
		{"uppercase hex rejected", upperSig, false},
		{"missing prefix", strings.TrimPrefix(validSig, "sha256="), false},
		{"empty signature", "", false},
	}
	v := srv.GitHubSourceWireVerifier()
	for _, tc := range sigCases {
		t.Run(tc.name, func(t *testing.T) {
			prodOK := githubapp.VerifyWebhookSignature(body, tc.sig, secret)
			_, seamErr := v.Verify(makeReq(tc.sig, "integration", "4242"), body, secrets)
			assert.Equal(t, tc.wantValid, prodOK, "production verifier")
			assert.Equal(t, prodOK, seamErr == nil, "seam verifier must match production verifier")
		})
	}

	// Step 1–2 parity table against production handleGitHubContentWebhook.
	step12Cases := []struct {
		name           string
		appID          int64
		mutateHeaders  func(h http.Header)
		eventArg       string
		deliveryArg    string
		wantSeamErr    error
		wantSupported  bool
		wantProdStatus int
		wantProdBody   string
	}{
		{
			name:           "valid supported event passes steps 1-2",
			appID:          4242,
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    nil,
			wantSupported:  true,
			wantProdStatus: http.StatusBadRequest, // advances past steps 1-2 to step 4 payload envelope check
			wantProdBody:   "invalid payload",
		},
		{
			name:           "AppID 0 fails closed",
			appID:          0,
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrWebhookTargetMismatch,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "wrong target",
		},
		{
			name:  "wrong target type",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Set("X-GitHub-Hook-Installation-Target-Type", "repository")
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrWebhookTargetMismatch,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "wrong target",
		},
		{
			name:  "wrong target ID",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Set("X-GitHub-Hook-Installation-Target-ID", "9999")
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrWebhookTargetMismatch,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "wrong target",
		},
		{
			name:  "duplicate target type header",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Add("X-GitHub-Hook-Installation-Target-Type", "integration")
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrWebhookTargetMismatch,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "wrong target",
		},
		{
			name:  "duplicate target ID header",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Add("X-GitHub-Hook-Installation-Target-ID", "4242")
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrWebhookTargetMismatch,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "wrong target",
		},
		{
			name:  "duplicate event header",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Add("X-GitHub-Event", triggithub.EventIssues)
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrInvalidWebhookHeaders,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "invalid event header",
		},
		{
			name:  "missing event header",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Del("X-GitHub-Event")
			},
			eventArg:       "",
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrInvalidWebhookHeaders,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "invalid event header",
		},
		{
			name:  "single empty event header ignored",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Set("X-GitHub-Event", "")
				h.Del("X-GitHub-Delivery")
			},
			eventArg:       "",
			deliveryArg:    "",
			wantSeamErr:    nil,
			wantSupported:  false,
			wantProdStatus: http.StatusOK,
			wantProdBody:   `"status":"ignored"`,
		},
		{
			name:  "unsupported event family ignored",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Set("X-GitHub-Event", "push")
				h.Del("X-GitHub-Delivery")
			},
			eventArg:       "push",
			deliveryArg:    "",
			wantSeamErr:    nil,
			wantSupported:  false,
			wantProdStatus: http.StatusOK,
			wantProdBody:   `"status":"ignored"`,
		},
		{
			name:  "missing delivery header",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Del("X-GitHub-Delivery")
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "",
			wantSeamErr:    triggithub.ErrInvalidDeliveryID,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "invalid delivery id",
		},
		{
			name:  "duplicate delivery header",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Add("X-GitHub-Delivery", "22222222-2222-3333-4444-555555555555")
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "11111111-2222-3333-4444-555555555555",
			wantSeamErr:    triggithub.ErrInvalidDeliveryID,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "invalid delivery id",
		},
		{
			name:  "malformed delivery GUID",
			appID: 4242,
			mutateHeaders: func(h http.Header) {
				h.Set("X-GitHub-Delivery", "not-a-guid")
			},
			eventArg:       triggithub.EventIssueComment,
			deliveryArg:    "not-a-guid",
			wantSeamErr:    triggithub.ErrInvalidDeliveryID,
			wantProdStatus: http.StatusBadRequest,
			wantProdBody:   "invalid delivery id",
		},
	}

	// Held verifier must observe live AppID changes across subtests (O1).
	heldVerifier := srv.GitHubSourceWireVerifier()
	for _, tc := range step12Cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.mu.Lock()
			srv.config.GitHubAppConfig.AppID = tc.appID
			srv.mu.Unlock()

			req := makeReq(validSig, "integration", "4242")
			if tc.mutateHeaders != nil {
				tc.mutateHeaders(req.Header)
			}

			d, seamErr := heldVerifier.Verify(req, body, secrets)
			if tc.wantSeamErr != nil {
				assert.ErrorIs(t, seamErr, tc.wantSeamErr)
			} else {
				require.NoError(t, seamErr)
				assert.Equal(t, tc.wantSupported, triggithub.IsSupportedEvent(d.Event))
			}

			rr := httptest.NewRecorder()
			srv.handleGitHubContentWebhook(rr, req, tc.eventArg, tc.deliveryArg, body)
			assert.Equal(t, tc.wantProdStatus, rr.Code)
			assert.Contains(t, rr.Body.String(), tc.wantProdBody)
		})
	}
}

func TestServerGitHubSourceSeams_NormalizerPeekNoIO(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	var appFetches atomic.Int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appFetches.Add(1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(gh.Close)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 4242
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.mu.Unlock()
	srv.githubAppIdentity.reset()

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	body := []byte(`{
		"action": "created",
		"installation": {"id": 8101},
		"repository": {"id": 1, "full_name": "acme/widgets", "name": "widgets", "private": false, "default_branch": "main", "owner": {"login": "acme", "id": 2, "type": "Organization"}},
		"sender": {"login": "alice", "id": 10, "type": "User"},
		"issue": {"number": 7, "title": "Peek test", "state": "open", "html_url": "https://github.com/acme/widgets/issues/7", "user": {"login": "bob", "id": 11, "type": "User"}},
		"comment": {"id": 99, "body": "@seam-app /review", "author_association": "MEMBER", "html_url": "https://github.com/acme/widgets/issues/7#issuecomment-99", "created_at": "2026-11-21T11:55:00Z", "user": {"login": "alice", "id": 10, "type": "User"}}
	}`)

	ev, err := srv.GitHubSourceNormalizer().Normalize(ctx, source.Delivery{
		ID:         "11111111-2222-3333-4444-555555555555",
		Event:      triggithub.EventIssueComment,
		ReceivedAt: now,
	}, body, source.Target{ProjectID: "proj-1"})
	require.NoError(t, err)
	assert.True(t, ev.Handled)
	assert.Equal(t, int64(0), appFetches.Load(), "GitHubSourceNormalizer must use peek (no GET /app network I/O)")
	assert.Equal(t, "unavailable", ev.Attributes["mention_detection"])
}

func TestServerGitHubSourceSeams_ResolverMinterIdentityAndPresetsParity(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setGitHubTriggersExperimentForRouting(t, srv, true)

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	srv.githubTokenCache.now = func() time.Time { return now }
	srv.githubWebhookIngress.nowFn = func() time.Time { return now }

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          4242,
			"slug":        "seam-app",
			"events":      []string{"issue_comment", "issues", "pull_request", "pull_request_review", "pull_request_review_comment"},
			"permissions": map[string]string{"contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write"},
		})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_seam_minted_token",
			"expires_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
			// Return distinct granted permissions to verify Credential.Scope carries granted permissions.
			"permissions": map[string]string{"issues": "write", "metadata": "read"},
		})
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 4242
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.WebhooksEnabled = true
	srv.config.GitHubAppConfig.WebhookSecret = "seam-secret"
	srv.mu.Unlock()

	admin := createAssocAdmin(t, st, "seam-admin")
	createAssocInstallation(t, st, 8101, "acme", "acme/widgets")

	remote := gh.URL + "/acme/widgets"
	projA := createAssocProject(t, st, "seam-proj-a", remote, "nobody", ptrInt64(8101))
	projA.GitHubPermissions = &store.GitHubTokenPermissions{
		Contents:     "write",
		PullRequests: "write",
		Issues:       "write",
		Metadata:     "read",
	}
	require.NoError(t, st.UpdateProject(ctx, projA))
	setAdminBinding(t, st, projA, 8101, normalizedRemoteForTest(remote), admin.ID)

	projB := createAssocProject(t, st, "seam-proj-b", remote, "nobody", ptrInt64(8101))
	setAdminBinding(t, st, projB, 8101, normalizedRemoteForTest(remote), admin.ID)

	// Set primary routing to projA.
	require.NoError(t, st.SetGitHubInstallationTriggerRoute(ctx, 8101, "acme/widgets", store.GitHubTriggerRoute{
		PrimaryProjectID: projA.ID,
		SetBy:            admin.ID,
		SetAt:            now,
	}))

	// 1. ActorIdentity + InstallationResolver (lookup + primary routing + lifecycle) + Presets.
	actor := srv.GitHubSourceActorIdentity()
	assert.True(t, actor.IsSelf(ctx, source.Principal{Type: "Bot", Login: "seam-app[bot]"}))
	assert.True(t, actor.IsSelf(ctx, source.Principal{Type: "User", Login: "alice", PerformedViaAppID: "4242"}))
	assert.False(t, actor.IsSelf(ctx, source.Principal{Type: "User", Login: "alice"}))

	body := []byte(`{
		"action": "created",
		"installation": {"id": 8101},
		"repository": {"id": 1, "full_name": "acme/widgets", "name": "widgets", "private": false, "default_branch": "main", "owner": {"login": "acme", "id": 2, "type": "Organization"}},
		"sender": {"login": "alice", "id": 10, "type": "User"},
		"issue": {"number": 7, "title": "Seam test", "state": "open", "html_url": "https://github.com/acme/widgets/issues/7", "user": {"login": "bob", "id": 11, "type": "User"}},
		"comment": {"id": 99, "body": "@seam-app /review", "author_association": "MEMBER", "html_url": "https://github.com/acme/widgets/issues/7#issuecomment-99", "created_at": "2026-11-21T11:55:00Z", "user": {"login": "alice", "id": 10, "type": "User"}}
	}`)
	delivery := source.Delivery{
		ID:         "11111111-2222-3333-4444-555555555555",
		Event:      triggithub.EventIssueComment,
		ReceivedAt: now,
	}

	resolver := srv.GitHubSourceInstallationResolver()
	inst, err := resolver.Installation(ctx, delivery, body)
	require.NoError(t, err)
	assert.Equal(t, "8101", inst.ID)
	assert.True(t, inst.Active)

	targets, err := resolver.Targets(ctx, inst, body)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, projA.ID, targets[0].ProjectID)
	assert.Equal(t, GitHubTriggerRoutingStatusOK, targets[0].RoutingStatus)
	assert.Equal(t, []string{projB.ID}, targets[0].BypassedProjectIDs)

	handled, err := resolver.OnLifecycle(ctx, delivery, body)
	require.NoError(t, err)
	assert.False(t, handled)

	handled, err = resolver.OnLifecycle(ctx, source.Delivery{Event: triggithub.EventInstallation}, []byte(`{"action":"suspend","installation":{"id":8101}}`))
	require.NoError(t, err)
	assert.True(t, handled)
	updatedInst, err := st.GetGitHubInstallation(ctx, 8101)
	require.NoError(t, err)
	assert.Equal(t, store.GitHubInstallationStatusSuspended, updatedInst.Status)

	handled, err = resolver.OnLifecycle(ctx, source.Delivery{Event: triggithub.EventInstallation}, []byte(`{"action":"unsuspend","installation":{"id":8101}}`))
	require.NoError(t, err)
	assert.True(t, handled)

	// 2. IdentityMinter parity: granted permissions + expiry + permission limit refusal.
	minter := srv.GitHubSourceIdentityMinter()
	cred, err := minter.Mint(ctx, targets[0], source.CredentialScope{
		Permissions: map[string]string{"issues": "write"},
	})
	require.NoError(t, err)
	assert.Equal(t, "ghs_seam_minted_token", cred.Token)
	assert.Equal(t, now.Add(time.Hour), cred.ExpiresAt)
	// Granted scope includes metadata:read returned by GitHub even though only issues:write was requested.
	assert.Equal(t, map[string]string{"issues": "write", "metadata": "read"}, cred.Scope)

	// Cached hit still carries granted scope.
	credCached, err := minter.Mint(ctx, targets[0], source.CredentialScope{
		Permissions: map[string]string{"issues": "write"},
	})
	require.NoError(t, err)
	assert.Equal(t, cred.Token, credCached.Token)
	assert.Equal(t, map[string]string{"issues": "write", "metadata": "read"}, credCached.Scope)

	// Exceeding project's permission limit returns ErrGitHubPermissionNotPermitted, matching production.
	_, _, prodPermErr := srv.MintGitHubAppTokenWithPermissions(ctx, reloadProject(t, st, projA.ID), githubapp.TokenPermissions{Checks: "write"})
	_, seamPermErr := minter.Mint(ctx, targets[0], source.CredentialScope{Permissions: map[string]string{"checks": "write"}})
	assert.ErrorIs(t, prodPermErr, ErrGitHubPermissionNotPermitted)
	assert.ErrorIs(t, seamPermErr, ErrGitHubPermissionNotPermitted)

	// 3. Presets.
	presets := srv.GitHubSourcePresets()
	list := presets.List()
	require.NotEmpty(t, list)
	match, suggested, err := presets.Compile(list[0], map[string]any{"command": "review"})
	require.NoError(t, err)
	assert.NotEmpty(t, match.Types)
	assert.NotEmpty(t, match.Rules)
	assert.Equal(t, triggithub.ActionKindDispatchAgent, suggested.Type)
}
