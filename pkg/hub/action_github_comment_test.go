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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type fakeGitHubCommentRecord struct {
	ID       int64
	Method   string
	Path     string
	Body     string
	HTMLURL  string
	Author   string
	Created  time.Time
	InThread bool
}

type fakeGitHubCommentAPI struct {
	srv *httptest.Server

	mu            sync.Mutex
	mintCount     int
	postCount     int
	listCount     int
	nextCommentID int64
	comments      []fakeGitHubCommentRecord
	postStatus    int
	postHeaders   map[string]string
	postErrMsg    string
	botLogin      string
}

func newFakeGitHubCommentAPI(t *testing.T) *fakeGitHubCommentAPI {
	t.Helper()
	api := &fakeGitHubCommentAPI{
		nextCommentID: 1000,
		botLogin:      "scion-bot[bot]",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   778899,
			"slug": "scion-bot",
			"name": "Scion Bot",
		})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		var req fakeMintRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		api.mu.Lock()
		api.mintCount++
		n := api.mintCount
		api.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       fmt.Sprintf("ghs_comment_test_token_%d", n),
			"expires_at":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": req.Permissions,
		})
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()

		// List comments: GET /repos/{o}/{r}/issues/{n}/comments or /pulls/{n}/comments
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments") {
			api.listCount++
			out := make([]githubapp.IssueComment, 0, len(api.comments))
			for _, c := range api.comments {
				out = append(out, githubapp.IssueComment{
					ID:        c.ID,
					Body:      c.Body,
					HTMLURL:   c.HTMLURL,
					User:      githubapp.CommentUser{Login: c.Author, Type: "Bot"},
					CreatedAt: c.Created,
					UpdatedAt: c.Created,
				})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		// Create comment or review reply: POST .../issues/{n}/comments or .../pulls/{n}/comments/{id}/replies
		if r.Method == http.MethodPost &&
			(strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/replies")) {
			api.postCount++
			if api.postStatus != 0 && api.postStatus != http.StatusCreated && api.postStatus != http.StatusOK {
				for k, v := range api.postHeaders {
					w.Header().Set(k, v)
				}
				w.WriteHeader(api.postStatus)
				msg := api.postErrMsg
				if msg == "" {
					msg = "error"
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			api.nextCommentID++
			id := api.nextCommentID
			rec := fakeGitHubCommentRecord{
				ID:       id,
				Method:   r.Method,
				Path:     r.URL.Path,
				Body:     body.Body,
				HTMLURL:  fmt.Sprintf("https://github.com/acme/widgets/issues/42#issuecomment-%d", id),
				Author:   api.botLogin,
				Created:  time.Now().UTC(),
				InThread: strings.HasSuffix(r.URL.Path, "/replies"),
			}
			api.comments = append(api.comments, rec)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(githubapp.IssueComment{
				ID:        rec.ID,
				Body:      rec.Body,
				HTMLURL:   rec.HTMLURL,
				User:      githubapp.CommentUser{Login: rec.Author, Type: "Bot"},
				CreatedAt: rec.Created,
				UpdatedAt: rec.Created,
			})
			return
		}

		http.NotFound(w, r)
	})
	api.srv = httptest.NewServer(mux)
	t.Cleanup(api.srv.Close)
	return api
}

func (a *fakeGitHubCommentAPI) snapshotComments() []fakeGitHubCommentRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]fakeGitHubCommentRecord, len(a.comments))
	copy(out, a.comments)
	return out
}

func setupGitHubCommentFixture(t *testing.T) (*triggersTestFixture, *fakeGitHubCommentAPI, *store.Project, *store.User, int64) {
	t.Helper()
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	gh := newFakeGitHubCommentAPI(t)
	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.APIBaseURL = gh.srv.URL
	f.srv.config.GitHubAppConfig.AppID = 778899
	f.srv.config.GitHubAppConfig.AppSlug = "scion-bot"
	f.srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	f.srv.config.GitHubAppConfig.WebhookSecret = "g3a-webhook-secret"
	f.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.srv.mu.Unlock()

	setTriggerExperimentsForG1bTest(t, f.srv, true, true)

	sysAdmin := createAssocAdmin(t, f.store, "g3a-sys-admin")
	const instID int64 = 8090
	createAssocInstallation(t, f.store, instID, "acme", "acme/widgets")

	proj, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	remote := gh.srv.URL + "/acme/widgets"
	proj.GitRemote = remote
	idCopy := instID
	proj.GitHubInstallationID = &idCopy
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	setAdminBinding(t, f.store, proj, instID, util.NormalizeGitRemote(remote), sysAdmin.ID)

	proj, err = f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	return f, gh, proj, sysAdmin, instID
}

func TestGitHubCommentTrigger_AuthoringAndAuthzMatrix(t *testing.T) {
	f, _, proj, _, _ := setupGitHubCommentFixture(t)
	ctx := context.Background()

	validReq := func(name string) CreateTriggerRequest {
		return CreateTriggerRequest{
			Name:   name,
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionGitHubComment,
				Body:          "Thanks @${event.attributes.sender} in ${project.name}",
				Target:        "event",
				UntrustedVars: []string{"event.attributes.sender"},
			},
		}
	}

	// 1. Role matrix: owner, admin, member allowed; viewer denied (403).
	for _, tc := range []struct {
		name string
		auth Identity
		want int
	}{
		{"owner", f.ownerAuth, http.StatusCreated},
		{"admin", f.adminAuth, http.StatusCreated},
		{"member", f.memberAuth, http.StatusCreated},
		{"viewer", f.viewerAuth, http.StatusForbidden},
	} {
		t.Run("role_"+tc.name, func(t *testing.T) {
			rec := f.doTrigger(t, tc.auth, "", http.MethodPost, validReq("gh-comment-"+tc.name))
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			if tc.want == http.StatusCreated {
				var resp TriggerResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Equal(t, store.TriggerActionGitHubComment, resp.Action.Type)
				assert.Equal(t, "event", resp.Action.Target)
				assert.Equal(t, []string{"event.attributes.sender"}, resp.Action.UntrustedVars)
			}
		})
	}

	// 2. Scoped UAT: requires both trigger:create and github_trigger:comment.
	mkUAT := func(scopes []string) Identity {
		_, tok, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.member.ID), CreateTokenParams{
			UserID:   f.member.ID,
			Name:     "uat-" + uuid.NewString()[:8],
			Boundary: TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID},
			Scopes:   scopes,
		})
		require.NoError(t, err)
		return NewScopedUserIdentityWithBoundary(
			f.memberAuth,
			TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID},
			tok.Scopes,
			tok.ID,
			tok.NormalizedCeiling(),
		)
	}

	uatNoComment := mkUAT([]string{"trigger:create"})
	rec := f.doTrigger(t, uatNoComment, "", http.MethodPost, validReq("gh-comment-uat-denied"))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	uatWithComment := mkUAT([]string{"trigger:create", "github_trigger:comment"})
	rec = f.doTrigger(t, uatWithComment, "", http.MethodPost, validReq("gh-comment-uat-allowed"))
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// 3. Payload & slot validation.
	// 3a. Non-github source -> 400.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "comment-wh-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))
	whComment := validReq("gh-comment-on-webhook")
	whComment.Source = TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID}
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, whComment)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 3b. Empty body -> 400.
	emptyBody := validReq("gh-comment-empty")
	emptyBody.Action.Body = ""
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, emptyBody)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 3c. Post-fire result.* placeholder in PhasePre slot -> 400.
	postVar := validReq("gh-comment-post-var")
	postVar.Action.Body = "Agent: ${result.agent_url}"
	postVar.Action.UntrustedVars = nil
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, postVar)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 3d. Unlisted untrusted variable -> 400.
	unlistedVar := validReq("gh-comment-unlisted")
	unlistedVar.Action.UntrustedVars = nil
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, unlistedVar)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 3e. replyInThread on non-review-comment match type -> 400.
	badThread := validReq("gh-comment-bad-thread")
	badThread.Action.ReplyInThread = true
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, badThread)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 3f. replyInThread on pull_request_review_comment match type -> 201,
	// then PATCH changing match.types to issue_comment -> 400.
	goodThread := validReq("gh-comment-good-thread")
	goodThread.Match = &TriggerMatchSpec{Types: []string{"github.pull_request_review_comment.created"}}
	goodThread.Action.ReplyInThread = true
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, goodThread)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var threadTr TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &threadTr))
	assert.True(t, threadTr.Action.ReplyInThread)

	nonReviewTypes := []string{"github.issue_comment.created"}
	patchRec := f.doTrigger(t, f.ownerAuth, threadTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Types: &nonReviewTypes},
	})
	assert.Equal(t, http.StatusBadRequest, patchRec.Code, patchRec.Body.String())

	// 4. Project without authorized association -> 409 github_not_associated.
	proj.GitHubInstallationID = nil
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	f.srv.invalidateGitHubAssociation(proj.ID)
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, validReq("gh-comment-unassoc"))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeGitHubNotAssociated, decodeAPIErrorCode(t, rec))
}

func TestGitHubCommentTrigger_E2E_PostRetryRefusalRateLimitStaleAndAudit(t *testing.T) {
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	f, gh, proj, _, instID := setupGitHubCommentFixture(t)
	ctx := context.Background()

	// Create a github.comment trigger as member.
	secretBodySentinel := "SUPER_UNIQUE_COMMENT_BODY_SENTINEL_8090"
	createRec := f.doTrigger(t, f.memberAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-comment-ack",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionGitHubComment,
			Body:          secretBodySentinel + " @${event.attributes.sender} /${event.attributes.command}",
			Target:        "event",
			UntrustedVars: []string{"event.attributes.sender", "event.attributes.command"},
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &trResp))

	trRow, err := f.store.GetTrigger(ctx, trResp.ID)
	require.NoError(t, err)

	mkReq := func(fireID string, attempts int, stale bool) hubfire.FireRequest {
		attrs := map[string]string{
			"repo":       "acme/widgets",
			"repo_owner": "acme",
			"repo_name":  "widgets",
			"number":     "42",
			"sender":     "alice",
			"command":    "hello",
		}
		if stale {
			attrs["stale"] = "true"
		}
		return hubfire.FireRequest{
			ProjectID: f.projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    trRow.InitiatorAttribution,
				Ceiling:        trRow.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   trRow.ID,
				FireID:         fireID,
			},
			ActionType: trRow.ActionType,
			Payload:    trRow.ActionPayload,
			Event: &hubfire.FireEvent{
				ID:         uuid.NewString(),
				ProjectID:  f.projectID,
				SourceKind: store.TriggerSourceKindGitHub,
				SourceID:   strconv.FormatInt(instID, 10),
				Type:       "github.issue_comment.created",
				Subject:    "acme/widgets#42",
				OccurredAt: time.Now().UTC(),
				ReceivedAt: time.Now().UTC().Add(-time.Second),
				Attributes: attrs,
				Trigger:    &hubfire.FireTriggerRef{ID: trRow.ID, Name: trRow.Name},
			},
			Executor: hubfire.ExecutorContext{
				Kind: trigger.ExecutorKindTriggerDrainer,
				ID:   trigger.ExecutorIDPrefixTriggerEvent + "ev-1",
			},
			FireAt:   time.Now().UTC(),
			Attempts: attempts,
		}
	}

	// 1. Happy-path post + marker + result_ref + audit without body.
	fireID1 := "fire-e2e-1"
	out1 := f.srv.fireAction(ctx, mkReq(fireID1, 1, false))
	require.Equal(t, hubfire.FireStatusFired, out1.Status, "err=%s", out1.Error)
	assert.Equal(t, "1001", out1.ResultRef)

	posted := gh.snapshotComments()
	require.Len(t, posted, 1)
	assert.Equal(t, "/repos/acme/widgets/issues/42/comments", posted[0].Path)
	assert.Contains(t, posted[0].Body, secretBodySentinel+" @alice /hello")
	assert.True(t, githubapp.HasFireMarker(posted[0].Body, fireID1))

	logs := logBuf.String()
	assert.Contains(t, logs, githubCommentAuditEventType)
	assert.Contains(t, logs, `"comment_id":1001`)
	assert.NotContains(t, logs, secretBodySentinel, "comment body must NEVER appear in logs or audit")

	// 2. Retry with Attempts=2 and same FireID finds existing marker and does NOT post a duplicate.
	outRetry := f.srv.fireAction(ctx, mkReq(fireID1, 2, false))
	require.Equal(t, hubfire.FireStatusFired, outRetry.Status, "err=%s", outRetry.Error)
	assert.Equal(t, "1001", outRetry.ResultRef)
	assert.Len(t, gh.snapshotComments(), 1, "retry with marker must not create a duplicate comment")

	// 3. Stale event (§4.4) is refused without calling GitHub.
	outStale := f.srv.fireAction(ctx, mkReq("fire-stale", 1, true))
	assert.Equal(t, hubfire.FireStatusRefused, outStale.Status)
	assert.Equal(t, "stale event", outStale.Error)
	assert.Len(t, gh.snapshotComments(), 1)

	// 4. Review-comment replyInThread posts to /pulls/{n}/comments/{comment_id}/replies.
	threadRec := f.doTrigger(t, f.memberAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-comment-thread",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.pull_request_review_comment.created"}},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionGitHubComment,
			Body:          "In-thread reply",
			Target:        "event",
			ReplyInThread: true,
		},
	})
	require.Equal(t, http.StatusCreated, threadRec.Code, threadRec.Body.String())
	var threadResp TriggerResponse
	require.NoError(t, json.Unmarshal(threadRec.Body.Bytes(), &threadResp))
	threadRow, err := f.store.GetTrigger(ctx, threadResp.ID)
	require.NoError(t, err)

	threadReq := mkReq("fire-thread-1", 1, false)
	threadReq.Authority.DefinitionID = threadRow.ID
	threadReq.Payload = threadRow.ActionPayload
	threadReq.Event.Type = "github.pull_request_review_comment.created"
	threadReq.Event.Attributes["comment_id"] = "9001"
	outThread := f.srv.fireAction(ctx, threadReq)
	require.Equal(t, hubfire.FireStatusFired, outThread.Status, "err=%s", outThread.Error)
	posted = gh.snapshotComments()
	require.Len(t, posted, 2)
	assert.Equal(t, "/repos/acme/widgets/pulls/42/comments/9001/replies", posted[1].Path)
	assert.True(t, posted[1].InThread)

	// 5. G3 S3: project github-permissions limit excluding issues:write refuses the fire.
	p, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	p.GitHubPermissions = &store.GitHubTokenPermissions{Contents: "read", Issues: "read", Metadata: "read"}
	require.NoError(t, f.store.UpdateProject(ctx, p))
	f.srv.githubTokenCache.invalidateProject(p.ID)

	outPermLimit := f.srv.fireAction(ctx, mkReq("fire-perm-limit", 1, false))
	assert.Equal(t, hubfire.FireStatusRefused, outPermLimit.Status)
	assert.Equal(t, "github permission not permitted", outPermLimit.Error)

	// Restore default project permissions.
	p.GitHubPermissions = nil
	require.NoError(t, f.store.UpdateProject(ctx, p))
	f.srv.githubTokenCache.invalidateProject(p.ID)

	// 5b. Pull-only creator proof (WriteAllowed == false) refuses with "github write not permitted".
	projPullOnly := createAssocProject(t, f.store, "g3a-pullonly-proj", gh.srv.URL+"/acme/widgets", f.owner.ID, ptrInt64(instID))
	memberRoleDef, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRoleDef.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projPullOnly.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.member.ID,
		CreatedBy:        f.owner.ID,
		CreatedAt:        time.Now().UTC(),
	})
	require.NoError(t, err)
	_, _, err = f.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GITHUB_TOKEN",
		Value:      "ghp_pull_only_creator_token",
		SecretType: secret.TypeEnvironment,
		Scope:      secret.ScopeUser,
		ScopeID:    f.owner.ID,
	})
	require.NoError(t, err)
	origRepoCheck := f.srv.githubRepoAccessCheck
	f.srv.githubRepoAccessCheck = func(_ context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		return githubapp.RepoAccess{Pull: true, Push: false}, nil
	}
	t.Cleanup(func() { f.srv.githubRepoAccessCheck = origRepoCheck })
	f.srv.invalidateGitHubAssociation(projPullOnly.ID)

	pullOnlyReq := mkReq("fire-pull-only", 1, false)
	pullOnlyReq.ProjectID = projPullOnly.ID
	pullOnlyReq.Event.ProjectID = projPullOnly.ID
	outPullOnly := f.srv.fireAction(ctx, pullOnlyReq)
	assert.Equal(t, hubfire.FireStatusRefused, outPullOnly.Status)
	assert.Equal(t, "github write not permitted", outPullOnly.Error)
	f.srv.githubRepoAccessCheck = origRepoCheck

	// Reset per-project comment bucket before testing GitHub 403/429 responses.
	f.srv.githubCommentLimiter = githubCommentRateLimiter{}

	// 6. GitHub 403 refusal -> failed "github comment refused" + project status degraded/permission_denied.
	gh.mu.Lock()
	gh.postStatus = http.StatusForbidden
	gh.postErrMsg = "Resource not accessible by integration"
	gh.mu.Unlock()
	out403 := f.srv.fireAction(ctx, mkReq("fire-403", 1, false))
	assert.Equal(t, hubfire.FireStatusFailed, out403.Status)
	assert.Equal(t, "github comment refused", out403.Error)
	after403, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	require.NotNil(t, after403.GitHubAppStatus)
	assert.Equal(t, store.GitHubAppStateDegraded, after403.GitHubAppStatus.State)
	assert.Equal(t, githubapp.ErrCodePermissionDenied, after403.GitHubAppStatus.ErrorCode)
	assert.NotContains(t, after403.GitHubAppStatus.ErrorMessage, "Resource not accessible by integration")

	// 7. GitHub 429 -> failed "github rate limited".
	gh.mu.Lock()
	gh.postStatus = http.StatusTooManyRequests
	gh.postErrMsg = "API rate limit exceeded"
	gh.mu.Unlock()
	out429 := f.srv.fireAction(ctx, mkReq("fire-429", 1, false))
	assert.Equal(t, hubfire.FireStatusFailed, out429.Status)
	assert.Equal(t, "github rate limited", out429.Error)

	// Reset GitHub API to 201 and test per-project comment rate bucket (burst 5).
	gh.mu.Lock()
	gh.postStatus = 0
	gh.mu.Unlock()
	fixedNow := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	f.srv.githubCommentLimiter = githubCommentRateLimiter{now: func() time.Time { return fixedNow }}
	for i := 0; i < 5; i++ {
		out := f.srv.fireAction(ctx, mkReq(fmt.Sprintf("fire-burst-%d", i), 1, false))
		require.Equal(t, hubfire.FireStatusFired, out.Status, "burst %d err=%s", i, out.Error)
	}
	outBucket := f.srv.fireAction(ctx, mkReq("fire-burst-6", 1, false))
	assert.Equal(t, hubfire.FireStatusSkipped, outBucket.Status)
	assert.Equal(t, "comment rate limited", outBucket.Error)

	// 8. Live revocation: downgrading member to viewer refuses subsequent fires.
	f.srv.githubCommentLimiter = githubCommentRateLimiter{}
	viewerRole, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.member.ID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projectID {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
		}
	}
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: viewerRole.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.member.ID,
		CreatedBy:        f.owner.ID,
		CreatedAt:        time.Now().UTC(),
	})
	require.NoError(t, err)
	outRevoked := f.srv.fireAction(ctx, mkReq("fire-revoked", 1, false))
	assert.Equal(t, hubfire.FireStatusRefused, outRevoked.Status)
	assert.Equal(t, "github.comment refused", outRevoked.Error)

	// Keep unused proj referenced cleanly.
	_ = proj
}

func TestGitHubUnmatchedCommandReply_E2E(t *testing.T) {
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	f, gh, proj, _, instID := setupGitHubCommentFixture(t)
	ctx := context.Background()

	fixedNow := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	f.srv.unmatchedCommandLimiter.now = func() time.Time { return fixedNow }

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store: f.store,
		Core:  serverTriggerCore{srv: f.srv},
		Rules: f.srv.TriggerRules(),
		Now:   func() time.Time { return fixedNow },
	})
	require.NoError(t, err)

	// Helper using real durablequeue.Item via Dispatcher.
	runEvent := func(evID, subject, command, senderType string) {
		ev := &store.TriggerEvent{
			ID:         evID,
			ProjectID:  f.projectID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   strconv.FormatInt(instID, 10),
			Type:       "github.issue_comment.created",
			Subject:    subject,
			DedupKey:   "dedup-" + evID,
			OccurredAt: fixedNow,
			ReceivedAt: fixedNow,
			Attributes: map[string]string{
				"repo":        "acme/widgets",
				"repo_owner":  "acme",
				"repo_name":   "widgets",
				"number":      "42",
				"sender":      "bob",
				"sender_type": senderType,
				"command":     command,
			},
			Status: store.TriggerEventStatusPending,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))

		payload, mErr := json.Marshal(trigger.EventQueuePayload{
			Version: trigger.EventQueuePayloadVersion,
			EventID: ev.ID,
		})
		require.NoError(t, mErr)
		item := durablequeue.Item{
			Kind:     trigger.EventQueueKind,
			DedupKey: ev.ID,
			Payload:  payload,
			Attempts: 1,
		}
		res := disp.HandleItem(ctx, item, nil)
		require.True(t, res.Done, "HandleItem err=%v", res.Err)
	}

	// 1. Default OFF: unmatched command posts nothing.
	runEvent(uuid.NewString(), "acme/widgets#42", "unknown", "User")
	assert.Empty(t, gh.snapshotComments(), "unmatched_command_reply must be off by default")

	// 2. Enable unmatched_command_reply via PUT /api/v1/projects/{id}/trigger-sources/github as owner.
	putRR := doRequestAsUser(t, f.srv, f.owner, http.MethodPut,
		"/api/v1/projects/"+f.projectID+"/trigger-sources/github",
		ProjectGitHubTriggerSettingsUpdateRequest{UnmatchedCommandReply: true})
	require.Equal(t, http.StatusOK, putRR.Code, putRR.Body.String())

	// 3. Unmatched command posts exactly one guidance comment with default text + marker + audit.
	ev1ID := uuid.NewString()
	runEvent(ev1ID, "acme/widgets#42", "deploy", "User")
	comments := gh.snapshotComments()
	require.Len(t, comments, 1)
	assert.Contains(t, comments[0].Body, fmt.Sprintf("No automation in project **%s** handles `/deploy` here. Available commands are listed in the project's Triggers page.", proj.Name))
	assert.True(t, githubapp.HasFireMarker(comments[0].Body, "unmatched-"+ev1ID))

	logs := logBuf.String()
	assert.Contains(t, logs, githubCommentAuditEventType)
	assert.Contains(t, logs, `"unmatched_command":true`)
	assert.Contains(t, logs, `"actor_id":"`+f.owner.ID+`"`)
	assert.NotContains(t, logs, "No automation in project", "unmatched reply body must not appear in logs/audit")

	// 4. Rate limited 1/min per (project, subject): second unmatched command on same subject posts nothing.
	runEvent(uuid.NewString(), "acme/widgets#42", "typo", "User")
	assert.Len(t, gh.snapshotComments(), 1, "second unmatched command within 1m on same subject must be rate-limited")

	// 5. Different subject within 1m still posts.
	runEvent(uuid.NewString(), "acme/widgets#43", "typo2", "User")
	assert.Len(t, gh.snapshotComments(), 2)

	// 6. Silent for Bot senders (even after 1m window elapses).
	fixedNow = fixedNow.Add(2 * time.Minute)
	runEvent(uuid.NewString(), "acme/widgets#42", "botcmd", "Bot")
	assert.Len(t, gh.snapshotComments(), 2, "unmatched reply must never post for Bot senders")

	// 7. Idempotent on retry (attempts=2): finds existing marker and does not post duplicate.
	ev1, err := f.store.GetTriggerEvent(ctx, ev1ID)
	require.NoError(t, err)
	f.srv.handleUnmatchedGitHubCommand(ctx, ev1, 2)
	assert.Len(t, gh.snapshotComments(), 2, "retry with marker must not post duplicate unmatched reply")

	// 8. Silent when a rule matched (even if the rule fire was skipped/refused).
	// Create a rule matching github.issue_comment.created that skips via rate limit / refused action.
	createRec := f.doTrigger(t, f.memberAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-comment-matches-cmd",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match: &TriggerMatchSpec{
			Types: []string{"github.issue_comment.created"},
			Rules: []TriggerMatchRuleSpec{{Dialect: "cel", Expr: `event.attributes.command == "handled"`}},
		},
		Action: TriggerActionSpec{
			Type:   store.TriggerActionGitHubComment,
			Body:   "Handled by rule",
			Target: "event",
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	fixedNow = fixedNow.Add(2 * time.Minute)
	runEvent(uuid.NewString(), "acme/widgets#44", "handled", "User")
	comments = gh.snapshotComments()
	require.Len(t, comments, 3, "matched rule posts its own comment, not an unmatched-command reply")
	assert.Contains(t, comments[2].Body, "Handled by rule")
	assert.NotContains(t, comments[2].Body, "No automation in project")
}

func TestGitHubAppStatusWriters_NarrowUpdateAndContentFreeErrors(t *testing.T) {
	f, gh, proj, _, instID := setupGitHubCommentFixture(t)
	ctx := context.Background()

	// Take a stale snapshot of proj before a concurrent admin unbind + remote change.
	staleSnap, err := f.store.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	require.NotNil(t, staleSnap.GitHubInstallationID)

	// Simulate concurrent admin unbind and remote update in the store.
	concurrent, err := f.store.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	concurrent.GitHubInstallationID = nil
	concurrent.GitRemote = gh.srv.URL + "/acme/other-repo"
	require.NoError(t, f.store.UpdateProject(ctx, concurrent))

	// Write status via updateProjectGitHubAppStatus using the stale snapshot.
	f.srv.updateProjectGitHubAppStatus(ctx, staleSnap, store.GitHubAppStateError,
		githubAppErrAssociationUnauthorized, githubAppStatusMessage(githubAppErrAssociationUnauthorized))

	got, err := f.store.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	assert.Nil(t, got.GitHubInstallationID, "status writer must not revert concurrent admin unbind")
	assert.Equal(t, gh.srv.URL+"/acme/other-repo", got.GitRemote, "status writer must not revert concurrent remote change")
	require.NotNil(t, got.GitHubAppStatus)
	assert.Equal(t, githubAppErrAssociationUnauthorized, got.GitHubAppStatus.ErrorCode)
	assert.Equal(t, githubAppStatusMessage(githubAppErrAssociationUnauthorized), got.GitHubAppStatus.ErrorMessage)

	// Also verify MintGitHubAppTokenWithPermissions authorization failure stores
	// content-free fixed text rather than raw authzErr.Error().
	badInst := instID + 999
	got.GitHubInstallationID = &badInst
	require.NoError(t, f.store.UpdateProject(ctx, got))
	f.srv.invalidateGitHubAssociation(got.ID)

	_, _, mintErr := f.srv.MintGitHubAppTokenWithPermissions(ctx, got, githubapp.TokenPermissions{Issues: "write", Metadata: "read"})
	require.Error(t, mintErr)

	afterMint, err := f.store.GetProject(ctx, got.ID)
	require.NoError(t, err)
	require.NotNil(t, afterMint.GitHubAppStatus)
	assert.Equal(t, githubAppErrAssociationUnauthorized, afterMint.GitHubAppStatus.ErrorCode)
	assert.Equal(t, githubAppStatusMessage(githubAppErrAssociationUnauthorized), afterMint.GitHubAppStatus.ErrorMessage)
	assert.NotContains(t, afterMint.GitHubAppStatus.ErrorMessage, strconv.FormatInt(badInst, 10),
		"stored status error message must be content-free and never echo raw internal error details")

	// Also verify github_trigger.comment permission registry row exists.
	var hasCommentPerm atomic.Bool
	for _, p := range permissions.Registry {
		if p.ID == githubTriggerCommentPermission {
			hasCommentPerm.Store(true)
			break
		}
	}
	assert.True(t, hasCommentPerm.Load())
}
