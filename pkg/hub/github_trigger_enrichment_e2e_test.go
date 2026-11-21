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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	githubtrigger "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func TestGitHubTriggerE2E_PREnrichmentHappyForkFailureUnauthorizedAndNoCandidate(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableWriteDenySwitch(t, f.srv)
	ctx := context.Background()

	const (
		appID         int64  = 778899
		webhookSecret string = "g2w-e2e-webhook-secret"
		fixtureInstID int64  = 987654 // matches issue_comment__created_pr.payload.json
	)

	fixtureBytes, err := os.ReadFile(filepath.Join("trigger", "github", "testdata", "issue_comment__created_pr.payload.json"))
	require.NoError(t, err)

	pemData := generateTestGitHubAppKey(t)

	var (
		mu             sync.Mutex
		prResponseCode = http.StatusOK
		prResponseBody string
		mintPermsSeen  []map[string]string
		prCallCount    atomic.Int64
		mintCallCount  atomic.Int64
	)

	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          appID,
				"slug":        "octo-app",
				"permissions": map[string]string{"contents": "write", "pull_requests": "read", "issues": "read", "metadata": "read"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			mintCallCount.Add(1)
			var req struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			mintPermsSeen = append(mintPermsSeen, req.Permissions)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":       "ghs_enrich_e2e_token",
				"expires_at":  time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339),
				"permissions": req.Permissions,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo-org/octo-repo/pulls/42":
			prCallCount.Add(1)
			mu.Lock()
			code, body := prResponseCode, prResponseBody
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fakeGH.Close()

	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.AppID = appID
	f.srv.config.GitHubAppConfig.AppSlug = "octo-app"
	f.srv.config.GitHubAppConfig.PrivateKey = pemData
	f.srv.config.GitHubAppConfig.APIBaseURL = fakeGH.URL
	f.srv.config.GitHubAppConfig.WebhookSecret = webhookSecret
	f.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.srv.mu.Unlock()

	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)

	done := f.srv.startTriggerDrainer(ctx)
	require.NotNil(t, done)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.srv.stopTriggerDrainer(stopCtx)
	})

	sysAdmin := createAssocAdmin(t, f.store, "g2w-e2e-sys-admin")
	createAssocInstallation(t, f.store, fixtureInstID, "octo-org", "octo-org/octo-repo")

	host := githubapp.WebHostForAPIBaseURL(fakeGH.URL)
	remote := "https://" + host + "/octo-org/octo-repo.git"
	proj, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	instCopy := fixtureInstID
	proj.GitRemote = remote
	proj.GitHubInstallationID = &instCopy
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	setAdminBinding(t, f.store, proj, fixtureInstID, util.NormalizeGitRemote(remote), sysAdmin.ID)
	f.srv.invalidateGitHubAssociation(proj.ID)
	setTriggerExperimentsForG1bTest(t, f.srv, true, true)

	mutateCommentID := func(commentID int) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal(fixtureBytes, &m))
		m["comment"].(map[string]any)["id"] = commentID
		out, err := json.Marshal(m)
		require.NoError(t, err)
		return out
	}

	// 1. No candidate trigger matching issue_comment.created -> 0 token mints, 0 PR API calls.
	recNoCand := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), mutateCommentID(1001))
	require.Equal(t, http.StatusAccepted, recNoCand.Code, recNoCand.Body.String())
	var respNoCand GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recNoCand.Body.Bytes(), &respNoCand))
	require.Len(t, respNoCand.Events, 1)
	require.Eventually(t, func() bool {
		ev, gErr := f.store.GetTriggerEvent(ctx, respNoCand.Events[0].EventID)
		return gErr == nil && ev != nil && ev.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)
	assert.Zero(t, mintCallCount.Load(), "no candidate trigger must mint zero tokens")
	assert.Zero(t, prCallCount.Load(), "no candidate trigger must make zero PR API calls")

	// 2. Author a preset-style fork-guarded + event.lists.labels trigger.
	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "pr-review-enriched",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match: &TriggerMatchSpec{
			Types: []string{"github.issue_comment.created"},
			Rules: []TriggerMatchRuleSpec{
				{Dialect: "cel", Expr: "event.attributes.command == 'review' && event.attributes.is_fork == 'false'"},
				{Dialect: "cel", Expr: "event.lists.labels.exists(l, l == 'needs-review')"},
			},
		},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: f.targetAgent.ID,
			Message: "Review enriched PR",
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var trGuard TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &trGuard))

	// Happy path: same-repo PR with label "needs-review".
	mu.Lock()
	prResponseCode = http.StatusOK
	prResponseBody = `{
		"number": 42,
		"state": "open",
		"draft": false,
		"merged": false,
		"base": {"ref": "main", "sha": "baseaaa"},
		"head": {"ref": "feat/enrich", "sha": "headbbb", "repo": {"full_name": "octo-org/octo-repo"}},
		"labels": [{"name": "needs-review"}],
		"requested_reviewers": [{"login": "reviewer-1"}]
	}`
	mu.Unlock()

	recOK := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), mutateCommentID(1002))
	require.Equal(t, http.StatusAccepted, recOK.Code, recOK.Body.String())
	var respOK GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recOK.Body.Bytes(), &respOK))
	evOKID := respOK.Events[0].EventID

	require.Eventually(t, func() bool {
		ev, gErr := f.store.GetTriggerEvent(ctx, evOKID)
		return gErr == nil && ev != nil && ev.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, int64(1), prCallCount.Load())
	assert.Equal(t, int64(1), mintCallCount.Load())
	mu.Lock()
	require.Len(t, mintPermsSeen, 1)
	assert.Equal(t, map[string]string{"pull_requests": "read", "metadata": "read"}, mintPermsSeen[0])
	mu.Unlock()

	storedOK, err := f.store.GetTriggerEvent(ctx, evOKID)
	require.NoError(t, err)
	assert.Equal(t, githubtrigger.EnrichedTrue, storedOK.Attributes[githubtrigger.AttrEnriched])
	assert.Equal(t, "false", storedOK.Attributes["is_fork"])
	assert.Equal(t, "feat/enrich", storedOK.Attributes["head_ref"])
	assert.Equal(t, "headbbb", storedOK.Attributes["head_sha"])
	assert.Equal(t, "main", storedOK.Attributes["base_ref"])
	assert.Equal(t, "baseaaa", storedOK.Attributes["base_sha"])
	assert.Equal(t, []string{"needs-review"}, storedOK.Lists["labels"])
	assert.Contains(t, storedOK.Lists[githubtrigger.ListEnriched], "is_fork")
	assert.Contains(t, storedOK.Lists[githubtrigger.ListEnriched], "labels")
	assert.Contains(t, storedOK.Lists[githubtrigger.ListEnriched], "requested_reviewers")

	calls := disp.getCalls()
	require.Len(t, calls, 1)
	env := extractEnvelopeJSONFromDelivery(t, calls[0].StructuredMessage.DeliveryText)
	require.NotNil(t, env.Event)
	assert.Equal(t, githubtrigger.EnrichedTrue, env.Event.Attributes[githubtrigger.AttrEnriched])
	assert.Equal(t, "feat/enrich", env.Event.Attributes["head_ref"])
	assert.Equal(t, "false", env.Event.Attributes["is_fork"])
	assert.Equal(t, []string{"needs-review"}, env.Event.Lists["labels"])

	// 3. Fork PR happy path: head.repo differs from repo -> is_fork="true", enriched="true",
	//    fork-guarded trigger evaluates false (does not fire).
	mu.Lock()
	prResponseCode = http.StatusOK
	prResponseBody = `{
		"number": 42,
		"state": "open",
		"draft": false,
		"merged": false,
		"base": {"ref": "main", "sha": "baseaaa"},
		"head": {"ref": "contrib-branch", "sha": "headfork", "repo": {"full_name": "contrib/octo-repo"}},
		"labels": [{"name": "needs-review"}]
	}`
	mu.Unlock()

	recFork := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), mutateCommentID(10025))
	require.Equal(t, http.StatusAccepted, recFork.Code, recFork.Body.String())
	var respFork GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recFork.Body.Bytes(), &respFork))
	evForkID := respFork.Events[0].EventID

	require.Eventually(t, func() bool {
		ev, gErr := f.store.GetTriggerEvent(ctx, evForkID)
		return gErr == nil && ev != nil && ev.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	storedFork, err := f.store.GetTriggerEvent(ctx, evForkID)
	require.NoError(t, err)
	assert.Equal(t, githubtrigger.EnrichedTrue, storedFork.Attributes[githubtrigger.AttrEnriched])
	assert.Equal(t, "true", storedFork.Attributes["is_fork"])
	assert.Equal(t, "contrib/octo-repo", storedFork.Attributes["head_repo"])
	assert.Len(t, disp.getCalls(), 1, "fork PR must not fire fork-guarded trigger")

	// 4. Failure path: GitHub returns 500 -> enriched="failed", fork guard fails closed (skipped),
	//    while a second Types-only trigger still fires.
	trTypesRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "pr-comment-types-only",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: f.targetAgent.ID,
			Message: "Always notify",
		},
	})
	require.Equal(t, http.StatusCreated, trTypesRec.Code, trTypesRec.Body.String())

	mu.Lock()
	prResponseCode = http.StatusInternalServerError
	prResponseBody = `{"message":"INTERNAL-SECRET-MARKER-500"}`
	mu.Unlock()

	recFail := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), mutateCommentID(1003))
	require.Equal(t, http.StatusAccepted, recFail.Code, recFail.Body.String())
	var respFail GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recFail.Body.Bytes(), &respFail))
	evFailID := respFail.Events[0].EventID

	require.Eventually(t, func() bool {
		ev, gErr := f.store.GetTriggerEvent(ctx, evFailID)
		return gErr == nil && ev != nil && ev.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	// Cached installation token was reused (still 1 mint), PR API called once per PR comment (total 3).
	assert.Equal(t, int64(1), mintCallCount.Load(), "cached read-only token must be reused")
	assert.Equal(t, int64(3), prCallCount.Load())

	storedFail, err := f.store.GetTriggerEvent(ctx, evFailID)
	require.NoError(t, err)
	assert.Equal(t, githubtrigger.EnrichedFailed, storedFail.Attributes[githubtrigger.AttrEnriched])
	assert.Empty(t, storedFail.Attributes["is_fork"])

	// Fork-guarded rule skipped; Types-only rule fired (total 2 calls to disp).
	firesFail, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evFailID}, store.ListOptions{})
	require.NoError(t, err)
	statusByTr := map[string]string{}
	for _, fr := range firesFail.Items {
		statusByTr[fr.TriggerID] = fr.Status
	}
	assert.Equal(t, store.TriggerFireStatusSkipped, statusByTr[trGuard.ID])
	require.Len(t, disp.getCalls(), 2)

	// 5. Direct enricher ceiling test while association is still valid:
	//    a fetched PR whose protected head_ref exceeds 8 KiB triggers
	//    ErrAttributesCeilingExceeded and returns enriched="failed" within the 8 KiB bound.
	mu.Lock()
	prResponseCode = http.StatusOK
	prResponseBody = `{
		"number": 42,
		"state": "open",
		"draft": false,
		"merged": false,
		"base": {"ref": "main", "sha": "baseaaa"},
		"head": {"ref": "` + strings.Repeat("r", githubtrigger.DefaultMaxAttributesBytes) + `", "sha": "headbbb", "repo": {"full_name": "octo-org/octo-repo"}}
	}`
	mu.Unlock()
	ceilingEv := &hubfire.FireEvent{
		ID:         uuid.NewString(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindGitHub,
		Attributes: map[string]string{
			"repo":            "octo-org/octo-repo",
			"event":           "issue_comment",
			"is_pull_request": "true",
			"number":          "42",
			"body":            strings.Repeat("b", 1024),
		},
	}
	gotAttrs, _, enrichErr := (serverTriggerCore{srv: f.srv}).Enrich(ctx, ceilingEv)
	assert.ErrorIs(t, enrichErr, githubtrigger.ErrAttributesCeilingExceeded)
	require.NotNil(t, gotAttrs)
	assert.Equal(t, githubtrigger.EnrichedFailed, gotAttrs[githubtrigger.AttrEnriched])
	rawGotAttrs, mErr := json.Marshal(gotAttrs)
	require.NoError(t, mErr)
	assert.LessOrEqual(t, len(rawGotAttrs), githubtrigger.DefaultMaxAttributesBytes)
	assert.Equal(t, int64(4), prCallCount.Load())

	// 6. Unauthorized association -> 0 additional mints, 0 additional PR API calls, fires refused.
	proj, err = f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	delete(proj.Annotations, githubInstallationBindingAnnotation)
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	f.srv.invalidateGitHubAssociation(proj.ID)

	recUnauth := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), mutateCommentID(1004))
	require.Equal(t, http.StatusAccepted, recUnauth.Code, recUnauth.Body.String())
	var respUnauth GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recUnauth.Body.Bytes(), &respUnauth))
	evUnauthID := respUnauth.Events[0].EventID

	require.Eventually(t, func() bool {
		ev, gErr := f.store.GetTriggerEvent(ctx, evUnauthID)
		return gErr == nil && ev != nil && ev.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, int64(1), mintCallCount.Load(), "unauthorized association must never mint")
	assert.Equal(t, int64(4), prCallCount.Load(), "unauthorized association must never call PR API")

	firesUnauth, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evUnauthID}, store.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, firesUnauth.Items)
	for _, fr := range firesUnauth.Items {
		assert.Equal(t, store.TriggerFireStatusRefused, fr.Status)
	}
}
