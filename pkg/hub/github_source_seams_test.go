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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
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

	// AppID == 0 must fail closed in both production ingress and seam verifier.
	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 0
	srv.mu.Unlock()

	req := makeReq(validSig, "integration", "4242")
	_, seamErr := srv.GitHubSourceWireVerifier().Verify(req, body, secrets)
	assert.ErrorIs(t, seamErr, triggithub.ErrWebhookTargetMismatch)

	rr := httptest.NewRecorder()
	srv.handleGitHubContentWebhook(rr, req, triggithub.EventIssueComment, "11111111-2222-3333-4444-555555555555", body)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
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

func TestServerGitHubSourceSeams_MinterAndCommentActionParity(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setGitHubTriggersExperimentForRouting(t, srv, true)

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	srv.githubTokenCache.now = func() time.Time { return now }
	srv.githubWebhookIngress.nowFn = func() time.Time { return now }
	srv.githubCommentLimiter.now = func() time.Time { return now }

	var commentPosted atomic.Int64
	var lastCommentBody atomic.Value
	var lastListSince atomic.Value

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
	mux.HandleFunc("/repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			lastListSince.Store(r.URL.Query().Get("since"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			lastCommentBody.Store(body.Body)
			n := commentPosted.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       9875 + n,
				"html_url": "https://github.com/acme/widgets/issues/7#issuecomment-9876",
			})
			return
		}
		http.NotFound(w, r)
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

	// 3. Comment ProviderAction parity against registered githubCommentAction / s.fireAction.
	ev, err := srv.GitHubSourceNormalizer().Normalize(ctx, delivery, body, targets[0])
	require.NoError(t, err)
	ev.ID = "evt-seam-1"

	userTrigger := &store.Trigger{
		ID:            "11111111-1111-4111-8111-111111111111",
		ProjectID:     projA.ID,
		Name:          "seam-user-comment",
		Status:        store.TriggerStatusActive,
		SourceKind:    store.TriggerSourceKindGitHub,
		ActionType:    store.TriggerActionGitHubComment,
		ActionPayload: `{"body":"Event ${event.id} at ${event.received_at}"}`,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    admin.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{
			Kind: store.EffectCeilingPrincipal,
		},
	}
	require.NoError(t, st.CreateTrigger(ctx, userTrigger))

	agentTrigger := &store.Trigger{
		ID:            "22222222-2222-4222-8222-222222222222",
		ProjectID:     projA.ID,
		Name:          "seam-agent-comment",
		Status:        store.TriggerStatusActive,
		SourceKind:    store.TriggerSourceKindGitHub,
		ActionType:    store.TriggerActionGitHubComment,
		ActionPayload: `{"body":"From agent"}`,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    "33333333-3333-4333-8333-333333333333",
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{
			Kind: store.EffectCeilingPrincipal,
		},
	}
	require.NoError(t, st.CreateTrigger(ctx, agentTrigger))

	action := srv.GitHubSourceCommentAction()
	assert.Equal(t, store.TriggerActionGitHubComment, action.Kind())

	normPayload, err := action.Validate(ctx, source.ActionValidateRequest{
		ProjectID: projA.ID,
		Payload:   userTrigger.ActionPayload,
	})
	require.NoError(t, err)

	// 3a. Missing TriggerID refuses (fail closed, never posts without trigger authority).
	noTrigRes, err := action.Fire(ctx, source.ActionFireRequest{
		ProjectID: projA.ID,
		FireID:    "fire-no-trig",
		Payload:   normPayload,
		Event:     ev,
		Target:    targets[0],
		Attempts:  1,
	})
	require.NoError(t, err)
	assert.Equal(t, string(hubfire.FireStatusRefused), noTrigRes.Status)
	assert.Equal(t, "github.comment refused", noTrigRes.Error)
	assert.Equal(t, int64(0), commentPosted.Load())

	// 3b. Posted (with Attempts=2 so marker scan verifies Since == ev.ReceivedAt and template vars).
	fireRes, err := action.Fire(ctx, source.ActionFireRequest{
		ProjectID: projA.ID,
		TriggerID: userTrigger.ID,
		FireID:    "fire-ok-1",
		Payload:   normPayload,
		Event:     ev,
		Target:    targets[0],
		Attempts:  2,
	})
	require.NoError(t, err)
	assert.Equal(t, string(hubfire.FireStatusFired), fireRes.Status)
	assert.Equal(t, "9876", fireRes.ResultRef)
	assert.Equal(t, int64(1), commentPosted.Load())
	assert.Contains(t, lastCommentBody.Load().(string), "Event evt-seam-1 at 2026-11-21T12:00:00Z")
	assert.Equal(t, now.Format(time.RFC3339), lastListSince.Load())

	// Helper to compare seam outcome against direct s.fireAction on the same trigger & event.
	fireDirect := func(tr *store.Trigger, fireID string, srcEv source.Event) hubfire.FireOutcome {
		return srv.fireAction(ctx, hubfire.FireRequest{
			ProjectID: tr.ProjectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    tr.InitiatorAttribution,
				Ceiling:        tr.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tr.ID,
				FireID:         fireID,
			},
			ActionType: store.TriggerActionGitHubComment,
			Payload:    normPayload,
			Event: &hubfire.FireEvent{
				ID:         srcEv.ID,
				ProjectID:  tr.ProjectID,
				SourceKind: srcEv.SourceKind,
				SourceID:   srcEv.SourceID,
				Type:       srcEv.Type,
				Subject:    srcEv.Subject,
				OccurredAt: srcEv.OccurredAt,
				ReceivedAt: srcEv.ReceivedAt,
				Attributes: srcEv.Attributes,
				Lists:      srcEv.Lists,
				Payload:    srcEv.Payload,
				Trigger:    &hubfire.FireTriggerRef{ID: tr.ID, Name: tr.Name},
			},
			FireAt:   srcEv.ReceivedAt,
			Attempts: 1,
		})
	}

	// 3c. Stale event parity: both refuse with "stale event".
	staleEv := ev
	staleEv.Attributes = map[string]string{
		"repo":       "acme/widgets",
		"repo_owner": "acme",
		"repo_name":  "widgets",
		"number":     "7",
		"stale":      "true",
	}
	wantStale := fireDirect(userTrigger, "fire-stale-prod", staleEv)
	gotStale, err := action.Fire(ctx, source.ActionFireRequest{
		ProjectID: projA.ID,
		TriggerID: userTrigger.ID,
		FireID:    "fire-stale-seam",
		Payload:   normPayload,
		Event:     staleEv,
		Target:    targets[0],
		Attempts:  1,
	})
	require.NoError(t, err)
	assert.Equal(t, wantStale.Status.String(), gotStale.Status)
	assert.Equal(t, wantStale.Error, gotStale.Error)
	assert.Equal(t, "stale event", gotStale.Error)

	// 3d. Agent principal parity: both refuse with "github.comment refused".
	wantAgent := fireDirect(agentTrigger, "fire-agent-prod", ev)
	gotAgent, err := action.Fire(ctx, source.ActionFireRequest{
		ProjectID: projA.ID,
		TriggerID: agentTrigger.ID,
		FireID:    "fire-agent-seam",
		Payload:   normPayload,
		Event:     ev,
		Target:    targets[0],
		Attempts:  1,
	})
	require.NoError(t, err)
	assert.Equal(t, wantAgent.Status.String(), gotAgent.Status)
	assert.Equal(t, wantAgent.Error, gotAgent.Error)
	assert.Equal(t, "github.comment refused", gotAgent.Error)

	// 3e. Rate-limited parity: exhaust per-project bucket, both return skipped / "comment rate limited".
	for srv.AllowGitHubComment(projA.ID) == nil {
	}
	wantRL := fireDirect(userTrigger, "fire-rl-prod", ev)
	gotRL, err := action.Fire(ctx, source.ActionFireRequest{
		ProjectID: projA.ID,
		TriggerID: userTrigger.ID,
		FireID:    "fire-rl-seam",
		Payload:   normPayload,
		Event:     ev,
		Target:    targets[0],
		Attempts:  1,
	})
	require.NoError(t, err)
	assert.Equal(t, wantRL.Status.String(), gotRL.Status)
	assert.Equal(t, wantRL.Error, gotRL.Error)
	assert.Equal(t, string(hubfire.FireStatusSkipped), gotRL.Status)
	assert.Equal(t, "comment rate limited", gotRL.Error)

	// 3f. Mint refused (project permission limit denies issues:write) parity.
	now = now.Add(time.Minute) // refill comment bucket
	projA.GitHubPermissions = &store.GitHubTokenPermissions{Contents: "read", Metadata: "read"}
	require.NoError(t, st.UpdateProject(ctx, projA))
	wantMintRefused := fireDirect(userTrigger, "fire-perm-prod", ev)
	gotMintRefused, err := action.Fire(ctx, source.ActionFireRequest{
		ProjectID: projA.ID,
		TriggerID: userTrigger.ID,
		FireID:    "fire-perm-seam",
		Payload:   normPayload,
		Event:     ev,
		Target:    targets[0],
		Attempts:  1,
	})
	require.NoError(t, err)
	assert.Equal(t, wantMintRefused.Status.String(), gotMintRefused.Status)
	assert.Equal(t, wantMintRefused.Error, gotMintRefused.Error)
	assert.Equal(t, string(hubfire.FireStatusRefused), gotMintRefused.Status)
	assert.Equal(t, "github permission not permitted", gotMintRefused.Error)

	// 4. Presets.
	presets := srv.GitHubSourcePresets()
	list := presets.List()
	require.NotEmpty(t, list)
	match, suggested, err := presets.Compile(list[0], map[string]any{"command": "review"})
	require.NoError(t, err)
	assert.NotEmpty(t, match.Types)
	assert.NotEmpty(t, match.Rules)
	assert.Equal(t, triggithub.ActionKindDispatchAgent, suggested.Type)
}
