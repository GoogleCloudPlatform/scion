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

	triggithub "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestServerGitHubSourceSeams(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	srv.githubTokenCache.now = func() time.Time { return now }
	srv.githubWebhookIngress.nowFn = func() time.Time { return now }

	var commentPosted atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
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
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       "ghs_seam_minted_token",
			"expires_at":  now.Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": map[string]string{"issues": "write", "metadata": "read"},
		})
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		if r.Method == http.MethodPost {
			commentPosted.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       9876,
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

	// 1. Readiness
	rep, err := srv.githubSourceReadiness().Check(ctx)
	require.NoError(t, err)
	assert.True(t, rep.Ready)
	assert.Equal(t, "seam-app", rep.AppSlug)
	assert.Empty(t, rep.MissingEvents)
	assert.Empty(t, rep.MissingPermissions)

	// 2. ActorIdentity
	actor := srv.githubSourceActorIdentity()
	assert.True(t, actor.IsSelf(ctx, source.Principal{Type: "Bot", Login: "seam-app[bot]"}))
	assert.True(t, actor.IsSelf(ctx, source.Principal{Type: "User", Login: "alice", PerformedViaAppID: "4242"}))
	assert.False(t, actor.IsSelf(ctx, source.Principal{Type: "User", Login: "alice"}))

	// 3. WireVerifier
	body := []byte(`{
		"action": "created",
		"installation": {"id": 8101},
		"repository": {
			"id": 1,
			"full_name": "acme/widgets",
			"name": "widgets",
			"private": false,
			"default_branch": "main",
			"owner": {"login": "acme", "id": 2, "type": "Organization"}
		},
		"sender": {"login": "alice", "id": 10, "type": "User"},
		"issue": {
			"number": 7,
			"title": "Seam test",
			"state": "open",
			"html_url": "https://github.com/acme/widgets/issues/7",
			"user": {"login": "bob", "id": 11, "type": "User"}
		},
		"comment": {
			"id": 99,
			"body": "@seam-app /review",
			"author_association": "MEMBER",
			"html_url": "https://github.com/acme/widgets/issues/7#issuecomment-99",
			"created_at": "2026-11-21T11:55:00Z",
			"user": {"login": "alice", "id": 10, "type": "User"}
		}
	}`)
	mac := hmac.New(sha256.New, []byte("seam-secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", triggithub.EventIssueComment)
	req.Header.Set("X-GitHub-Delivery", "11111111-2222-3333-4444-555555555555")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "4242")

	verifier := srv.githubSourceWireVerifier()
	delivery, err := verifier.Verify(req, body, source.SecretFunc(func(context.Context) (string, error) {
		return "seam-secret", nil
	}))
	require.NoError(t, err)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", delivery.ID)
	assert.NotEmpty(t, verifier.DedupKey(delivery, body))

	// 4. InstallationResolver (lookup + candidate/primary routing + lifecycle)
	resolver := srv.githubSourceInstallationResolver()
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

	lifecycleBody := []byte(`{"action":"suspend","installation":{"id":8101}}`)
	handled, err = resolver.OnLifecycle(ctx, source.Delivery{Event: triggithub.EventInstallation}, lifecycleBody)
	require.NoError(t, err)
	assert.True(t, handled)
	updatedInst, err := st.GetGitHubInstallation(ctx, 8101)
	require.NoError(t, err)
	assert.Equal(t, store.GitHubInstallationStatusSuspended, updatedInst.Status)

	// Unsuspend before minting.
	unsuspendBody := []byte(`{"action":"unsuspend","installation":{"id":8101}}`)
	handled, err = resolver.OnLifecycle(ctx, source.Delivery{Event: triggithub.EventInstallation}, unsuspendBody)
	require.NoError(t, err)
	assert.True(t, handled)

	// 5. Normalizer
	ev, err := srv.githubSourceNormalizer().Normalize(ctx, delivery, body, targets[0])
	require.NoError(t, err)
	assert.True(t, ev.Handled)
	assert.Equal(t, "github.issue_comment.created", ev.Type)
	assert.Equal(t, "review", ev.Attributes["command"])
	assert.Equal(t, "11111111-2222-3333-4444-555555555555/"+projA.ID, ev.DedupKey)

	// 6. IdentityMinter
	minter := srv.githubSourceIdentityMinter()
	cred, err := minter.Mint(ctx, targets[0], source.CredentialScope{
		Permissions: map[string]string{"issues": "write", "metadata": "read"},
	})
	require.NoError(t, err)
	assert.Equal(t, "ghs_seam_minted_token", cred.Token)

	// 7. ProviderAction (github.comment)
	action := srv.githubSourceCommentAction()
	assert.Equal(t, store.TriggerActionGitHubComment, action.Kind())
	normPayload, err := action.Validate(ctx, source.ActionValidateRequest{
		ProjectID: projA.ID,
		Payload:   `{"body":"Hello from seam"}`,
	})
	require.NoError(t, err)
	assert.Contains(t, normPayload, `"body":"Hello from seam"`)

	fireRes, err := action.Fire(ctx, source.ActionFireRequest{
		ProjectID: projA.ID,
		TriggerID: "trig-1",
		FireID:    "fire-1",
		Payload:   normPayload,
		Event:     ev,
		Target:    targets[0],
		Attempts:  1,
	})
	require.NoError(t, err)
	assert.Equal(t, "fired", fireRes.Status)
	assert.Equal(t, "9876", fireRes.ResultRef)
	assert.Equal(t, int64(1), commentPosted.Load())

	// 8. Presets
	presets := srv.githubSourcePresets()
	list := presets.List()
	require.NotEmpty(t, list)
	match, suggested, err := presets.Compile(list[0], map[string]any{"command": "review"})
	require.NoError(t, err)
	assert.NotEmpty(t, match.Types)
	assert.NotEmpty(t, match.Rules)
	assert.Equal(t, triggithub.ActionKindDispatchAgent, suggested.Type)
}
