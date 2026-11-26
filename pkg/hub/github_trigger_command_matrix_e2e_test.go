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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	githubtrigger "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// fakeGitHubCommandMatrixAPI implements the subset of GitHub's REST API used by
// the §9.4 command-matrix end-to-end test: GET /app, installation token mints,
// GET /repos/{owner}/{repo}/pulls/{number} (PR enrichment), and issue comments.
type fakeGitHubCommandMatrixAPI struct {
	srv *httptest.Server

	mu            sync.Mutex
	appID         int64
	appSlug       string
	mintCount     int
	nextCommentID int64
	prResponses   map[int]fakePRResponse
	comments      []fakeGitHubCommentRecord
}

type fakePRResponse struct {
	status int
	body   string
}

func newFakeGitHubCommandMatrixAPI(t *testing.T, appID int64, appSlug string) *fakeGitHubCommandMatrixAPI {
	t.Helper()
	api := &fakeGitHubCommandMatrixAPI{
		appID:         appID,
		appSlug:       appSlug,
		nextCommentID: 2000,
		prResponses:   map[int]fakePRResponse{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   api.appID,
			"slug": api.appSlug,
			"name": "Scion Bot",
			"permissions": map[string]string{
				"contents":      "write",
				"pull_requests": "write",
				"issues":        "write",
				"metadata":      "read",
			},
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       fmt.Sprintf("ghs_cmd_matrix_tok_%d", n),
			"expires_at":  time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			"permissions": req.Permissions,
		})
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()

		// PR enrichment: GET /repos/{owner}/{repo}/pulls/{number}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/") && !strings.HasSuffix(r.URL.Path, "/comments") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
			if len(parts) == 5 && parts[3] == "pulls" {
				prNum, err := strconv.Atoi(parts[4])
				if err == nil {
					resp, ok := api.prResponses[prNum]
					if !ok {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					status := resp.status
					if status == 0 {
						status = http.StatusOK
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(resp.body))
					return
				}
			}
		}

		// List comments for marker idempotency: GET /repos/{owner}/{repo}/issues/{n}/comments
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments") {
			out := make([]githubapp.IssueComment, 0, len(api.comments))
			for _, c := range api.comments {
				if c.Path == r.URL.Path {
					out = append(out, githubapp.IssueComment{
						ID:        c.ID,
						Body:      c.Body,
						HTMLURL:   c.HTMLURL,
						User:      githubapp.CommentUser{Login: c.Author, Type: "Bot"},
						CreatedAt: c.Created,
						UpdatedAt: c.Created,
					})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		// Create issue/PR comment: POST /repos/{owner}/{repo}/issues/{n}/comments
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
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
				ID:      id,
				Method:  r.Method,
				Path:    r.URL.Path,
				Body:    body.Body,
				HTMLURL: fmt.Sprintf("https://github.com/acme/widgets/issues/1#issuecomment-%d", id),
				Author:  api.appSlug + "[bot]",
				Created: time.Now().UTC(),
			}
			api.comments = append(api.comments, rec)
			w.Header().Set("Content-Type", "application/json")
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

func (a *fakeGitHubCommandMatrixAPI) setPR(number, status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prResponses[number] = fakePRResponse{status: status, body: body}
}

func (a *fakeGitHubCommandMatrixAPI) snapshotComments() []fakeGitHubCommentRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]fakeGitHubCommentRecord, len(a.comments))
	copy(out, a.comments)
	return out
}

func (a *fakeGitHubCommandMatrixAPI) commentsOnPath(path string) []fakeGitHubCommentRecord {
	all := a.snapshotComments()
	var out []fakeGitHubCommentRecord
	for _, c := range all {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// compileAndBuildPresetTrigger calls POST /api/v1/trigger-presets/compile and
// translates the compiled output into a CreateTriggerRequest, applying the
// caller's literal template and any explicit subject-mode or ephemeral overrides.
func compileAndBuildPresetTrigger(
	t *testing.T,
	f *triggerDispatchAgentFixture,
	authCtx context.Context,
	presetID string,
	fields map[string]any,
	name string,
	literalTemplate string,
	subjectModeOverride string,
	ephemeralOverride *bool,
) (*githubtrigger.CompiledPreset, CreateTriggerRequest) {
	t.Helper()

	compileRec := f.doJSON(t, authCtx, http.MethodPost, "/api/v1/trigger-presets/compile", githubtrigger.CompilePresetRequest{
		Preset: presetID,
		Fields: fields,
	})
	require.Equal(t, http.StatusOK, compileRec.Code, "compile preset %s (%s): %s", presetID, name, compileRec.Body.String())

	var compiled githubtrigger.CompiledPreset
	require.NoError(t, json.Unmarshal(compileRec.Body.Bytes(), &compiled))
	require.NotNil(t, compiled.SuggestedAction, "preset %s must emit suggested_action", presetID)

	formRaw, err := json.Marshal(compiled.Form)
	require.NoError(t, err)

	rules := make([]TriggerMatchRuleSpec, len(compiled.Match.Rules))
	for i, r := range compiled.Match.Rules {
		rules[i] = TriggerMatchRuleSpec{Dialect: r.Dialect, Expr: r.Expr}
	}

	subjectMode := compiled.Subject.Mode
	if subjectModeOverride != "" {
		subjectMode = subjectModeOverride
	}

	sa := compiled.SuggestedAction
	ephemeral := sa.Ephemeral
	if ephemeralOverride != nil {
		ephemeral = *ephemeralOverride
	}
	var ack *DispatchAgentAckComment
	if sa.AckComment != nil {
		ack = &DispatchAgentAckComment{Body: sa.AckComment.Body}
	}

	req := CreateTriggerRequest{
		Name: name,
		Source: TriggerSourceSpec{
			Kind: store.TriggerSourceKindGitHub,
			ID:   "",
		},
		Match: &TriggerMatchSpec{
			Types: append([]string(nil), compiled.Match.Types...),
			Rules: rules,
			Form:  json.RawMessage(formRaw),
		},
		Subject: &TriggerSubjectSpec{
			Mode:           subjectMode,
			CoalesceWindow: compiled.Subject.CoalesceWindow,
			Expr:           compiled.Subject.Expr,
		},
		Action: TriggerActionSpec{
			Type:          sa.Type,
			AgentName:     sa.AgentName,
			Template:      literalTemplate,
			Task:          sa.Task,
			Branch:        sa.Branch,
			Labels:        sa.Labels,
			Ephemeral:     ephemeral,
			GithubToken:   sa.GitHubToken,
			ReuseMessage:  sa.ReuseMessage,
			AckComment:    ack,
			UntrustedVars: append([]string(nil), sa.UntrustedVars...),
		},
	}
	return &compiled, req
}

func syntheticIssueCommentPayload(
	t *testing.T,
	instID int64,
	number int,
	isPR bool,
	commentID int,
	senderLogin, senderType, authorAssoc, body string,
	ts time.Time,
) []byte {
	t.Helper()
	issueObj := map[string]any{
		"number":             number,
		"title":              fmt.Sprintf("Item #%d", number),
		"state":              "open",
		"html_url":           fmt.Sprintf("https://github.com/acme/widgets/issues/%d", number),
		"author_association": authorAssoc,
		"user":               map[string]any{"login": senderLogin, "id": 100, "type": senderType},
		"created_at":         ts.Format(time.RFC3339),
		"updated_at":         ts.Format(time.RFC3339),
	}
	if isPR {
		issueObj["pull_request"] = map[string]any{
			"url":      fmt.Sprintf("https://api.github.com/repos/acme/widgets/pulls/%d", number),
			"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		}
	}
	payload := map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": instID},
		"repository": map[string]any{
			"id":             555001,
			"full_name":      "acme/widgets",
			"name":           "widgets",
			"private":        false,
			"default_branch": "main",
			"html_url":       "https://github.com/acme/widgets",
			"owner":          map[string]any{"login": "acme", "id": 10, "type": "Organization"},
		},
		"sender": map[string]any{
			"login": senderLogin,
			"id":    100,
			"type":  senderType,
		},
		"issue": issueObj,
		"comment": map[string]any{
			"id":                 commentID,
			"body":               body,
			"html_url":           fmt.Sprintf("https://github.com/acme/widgets/issues/%d#issuecomment-%d", number, commentID),
			"author_association": authorAssoc,
			"user":               map[string]any{"login": senderLogin, "id": 100, "type": senderType},
			"created_at":         ts.Format(time.RFC3339),
			"updated_at":         ts.Format(time.RFC3339),
		},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return raw
}

func syntheticPRSynchronizePayload(
	t *testing.T,
	instID int64,
	number int,
	headRef, headSHA, headRepoFullName string,
	senderLogin, authorAssoc string,
	ts time.Time,
) []byte {
	t.Helper()
	payload := map[string]any{
		"action":       "synchronize",
		"number":       number,
		"before":       "0000000000000000000000000000000000000000",
		"after":        headSHA,
		"installation": map[string]any{"id": instID},
		"repository": map[string]any{
			"id":             555001,
			"full_name":      "acme/widgets",
			"name":           "widgets",
			"private":        false,
			"default_branch": "main",
			"html_url":       "https://github.com/acme/widgets",
			"owner":          map[string]any{"login": "acme", "id": 10, "type": "Organization"},
		},
		"sender": map[string]any{
			"login": senderLogin,
			"id":    101,
			"type":  "User",
		},
		"pull_request": map[string]any{
			"number":             number,
			"title":              fmt.Sprintf("PR #%d", number),
			"state":              "open",
			"draft":              false,
			"merged":             false,
			"html_url":           fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			"author_association": authorAssoc,
			"user":               map[string]any{"login": senderLogin, "id": 101, "type": "User"},
			"created_at":         ts.Format(time.RFC3339),
			"updated_at":         ts.Format(time.RFC3339),
			"base": map[string]any{
				"ref": "main",
				"sha": "base111111111111111111111111111111111111",
				"repo": map[string]any{
					"id":             555001,
					"full_name":      "acme/widgets",
					"name":           "widgets",
					"default_branch": "main",
					"html_url":       "https://github.com/acme/widgets",
					"owner":          map[string]any{"login": "acme", "id": 10, "type": "Organization"},
				},
			},
			"head": map[string]any{
				"ref": headRef,
				"sha": headSHA,
				"repo": map[string]any{
					"id":             555002,
					"full_name":      headRepoFullName,
					"name":           "widgets",
					"default_branch": "main",
					"html_url":       "https://github.com/" + headRepoFullName,
					"owner":          map[string]any{"login": strings.Split(headRepoFullName, "/")[0], "id": 11, "type": "User"},
				},
			},
		},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return raw
}

// TestGitHubTriggerCommandMatrix_E2E exercises the Stage 3 §9.4 / §15.2 command
// matrix end-to-end through the real preset compile/apply API, signed GitHub
// webhook ingress, durable queue, trigger dispatcher, PR enrichment, subject
// reuse/coalescing, dispatch_agent, ackComment, and unmatched-command replies.
func TestGitHubTriggerCommandMatrix_E2E(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g6e-cmd-matrix")

	const (
		appID         int64 = 778899
		appSlug             = "scion-bot"
		instID        int64 = 9401
		webhookSecret       = "g6e-cmd-matrix-webhook-secret"
	)

	gh := newFakeGitHubCommandMatrixAPI(t, appID, appSlug)
	// Same-repo PR #42.
	gh.setPR(42, http.StatusOK, `{
		"number": 42,
		"state": "open",
		"draft": false,
		"merged": false,
		"base": {"ref": "main", "sha": "base1111"},
		"head": {"ref": "feat/widget-42", "sha": "head1111", "repo": {"full_name": "acme/widgets"}},
		"labels": [{"name": "enhancement"}]
	}`)
	// Fork PR #55.
	gh.setPR(55, http.StatusOK, `{
		"number": 55,
		"state": "open",
		"draft": false,
		"merged": false,
		"base": {"ref": "main", "sha": "base5555"},
		"head": {"ref": "fork-fix-55", "sha": "head5555", "repo": {"full_name": "external-contrib/widgets"}},
		"labels": []
	}`)
	// PR #77 whose enrichment fails (HTTP 500) so fail-closed CEL records a skipped fire.
	gh.setPR(77, http.StatusInternalServerError, `{"message":"upstream unavailable"}`)

	f.sched.srv.mu.Lock()
	f.sched.srv.config.GitHubAppConfig.APIBaseURL = gh.srv.URL
	f.sched.srv.config.GitHubAppConfig.AppID = appID
	f.sched.srv.config.GitHubAppConfig.AppSlug = appSlug
	f.sched.srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	f.sched.srv.config.GitHubAppConfig.WebhookSecret = webhookSecret
	f.sched.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.sched.srv.config.HubEndpoint = "https://hub.example.test"
	f.sched.srv.config.HubEndpoints = &HubEndpoints{Public: "https://hub.example.test"}
	f.sched.srv.mu.Unlock()

	setTriggerExperimentsForG1bTest(t, f.sched.srv, true, true)

	createAssocInstallation(t, f.sched.store, instID, "acme", "acme/widgets")
	sysAdmin := createAssocAdmin(t, f.sched.store, "g6e-sys-admin")
	proj, err := f.sched.store.GetProject(ctx, f.sched.proj.ID)
	require.NoError(t, err)
	remote := "https://" + strings.TrimPrefix(gh.srv.URL, "http://") + "/acme/widgets"
	proj.GitRemote = remote
	instCopy := instID
	proj.GitHubInstallationID = &instCopy
	proj.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:       store.GitHubAppStateOK,
		LastChecked: time.Now().UTC(),
	}
	require.NoError(t, f.sched.store.UpdateProject(ctx, proj))
	setAdminBinding(t, f.sched.store, proj, instID, util.NormalizeGitRemote(remote), sysAdmin.ID)
	f.sched.srv.invalidateGitHubAssociation(proj.ID)

	// Wire reuseTrackingBrokerClient over the headless gather broker client so
	// both CreateAgentWithGather requests and reuse MessageAgent calls are recorded.
	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
	}
	f.sched.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default()))

	owner := &store.User{
		ID:          uuid.NewString(),
		Email:       "g6e-owner@example.com",
		DisplayName: "G6e Project Owner",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now().UTC(),
	}
	require.NoError(t, f.sched.store.CreateUser(ctx, owner))
	require.NoError(t, f.sched.srv.createProjectOwnerRoleBinding(ctx, proj.ID, owner.ID))
	ownerCtx := f.userAuth(t, owner)
	triggersPath := fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID)

	// Enable project setting unmatched_command_reply: on via the real settings API (§8.3).
	settingsRec := f.doJSON(t, ownerCtx, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/trigger-sources/github", proj.ID),
		map[string]any{"unmatched_command_reply": true})
	require.Equal(t, http.StatusOK, settingsRec.Code, settingsRec.Body.String())

	// -------------------------------------------------------------------------
	// Build the §9.4 command matrix through the real preset compile/apply path,
	// plus the §7.2 github.pr_updated preset to exercise synchronize coalescing.
	// -------------------------------------------------------------------------

	// Current behaviour check (ptone/scion#194): presets emit ephemeral=false and
	// authoring dispatch_agent with ephemeral=true is refused 400 ephemeral_unsupported.
	ephemeralTrue := true
	compiledReview, reviewEphemeralReq := compileAndBuildPresetTrigger(
		t, f, ownerCtx,
		githubtrigger.PresetCommand,
		map[string]any{"command": "review", "on": "pull_request"},
		"cmd-review", "review", "", &ephemeralTrue,
	)
	assert.False(t, compiledReview.SuggestedAction.Ephemeral, "preset must emit ephemeral=false until ephemeral agents land")
	recEphReview := f.doJSON(t, ownerCtx, http.MethodPost, triggersPath, reviewEphemeralReq)
	require.Equal(t, http.StatusBadRequest, recEphReview.Code, recEphReview.Body.String())
	assert.Equal(t, "ephemeral_unsupported", decodeAPIErrorCode(t, recEphReview))

	compiledValidate, validateEphemeralReq := compileAndBuildPresetTrigger(
		t, f, ownerCtx,
		githubtrigger.PresetCommand,
		map[string]any{"command": "validate", "on": "pull_request"},
		"cmd-validate", "validate", "", &ephemeralTrue,
	)
	assert.False(t, compiledValidate.SuggestedAction.Ephemeral, "preset must emit ephemeral=false until ephemeral agents land")
	recEphValidate := f.doJSON(t, ownerCtx, http.MethodPost, triggersPath, validateEphemeralReq)
	require.Equal(t, http.StatusBadRequest, recEphValidate.Code, recEphValidate.Body.String())
	assert.Equal(t, "ephemeral_unsupported", decodeAPIErrorCode(t, recEphValidate))

	applyPreset := func(
		presetID string,
		fields map[string]any,
		name, literalTemplate, subjectModeOverride string,
	) TriggerResponse {
		t.Helper()
		_, req := compileAndBuildPresetTrigger(t, f, ownerCtx, presetID, fields, name, literalTemplate, subjectModeOverride, nil)
		rec := f.doJSON(t, ownerCtx, http.MethodPost, triggersPath, req)
		require.Equal(t, http.StatusCreated, rec.Code, "apply preset %s (%s): %s", presetID, name, rec.Body.String())
		var out TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		assert.NotEmpty(t, out.Match.Form, "preset rule %s must persist Match.Form", name)
		return out
	}

	// 1. /review: on=pull_request, mode=new, ack_comment=yes, branch=head_ref (non-fork)
	trReview := applyPreset(githubtrigger.PresetCommand,
		map[string]any{"command": "review", "on": "pull_request"},
		"cmd-review", "review", "")
	assert.Equal(t, store.TriggerSubjectModeNew, trReview.Subject.Mode)

	// 2. /validate: on=pull_request, mode=new, ack_comment=yes, branch=head_ref
	trValidate := applyPreset(githubtrigger.PresetCommand,
		map[string]any{"command": "validate", "on": "pull_request"},
		"cmd-validate", "validate", "")
	assert.Equal(t, store.TriggerSubjectModeNew, trValidate.Subject.Mode)

	// 3. /fix: on=pull_request, mode=reuse, ack_comment=yes on dispatch / none on reuse, branch=head_ref.
	// The github.command preset cannot express subject.mode=reuse today, so the test sets it on the create request.
	trFix := applyPreset(githubtrigger.PresetCommand,
		map[string]any{"command": "fix", "on": "pull_request"},
		"cmd-fix", "fix", store.TriggerSubjectModeReuse)
	assert.Equal(t, store.TriggerSubjectModeReuse, trFix.Subject.Mode)

	// 4. /plan: on=any (issue or PR), mode=reuse, ack_comment=yes, branch=none.
	// The github.command preset cannot express subject.mode=reuse today, so the test sets it on the create request.
	trPlan := applyPreset(githubtrigger.PresetCommand,
		map[string]any{"command": "plan", "on": "any", "include_body": true},
		"cmd-plan", "plan", store.TriggerSubjectModeReuse)
	assert.Equal(t, store.TriggerSubjectModeReuse, trPlan.Subject.Mode)

	// 5. /implement: on=any, mode=new, ack_comment=yes, branch=none (include_forks=true; on=any never sets branch).
	trImplement := applyPreset(githubtrigger.PresetCommand,
		map[string]any{"command": "implement", "on": "any", "include_body": true, "include_forks": true},
		"cmd-implement", "implement", "")
	assert.Equal(t, store.TriggerSubjectModeNew, trImplement.Subject.Mode)

	// 5b. PR-scoped fork-enabled command rule (on=pull_request, include_forks=true) to exercise
	// non-vacuous branch omission and `gh pr checkout` task guidance on fork PRs.
	trForkReview := applyPreset(githubtrigger.PresetCommand,
		map[string]any{"command": "review-fork", "on": "pull_request", "include_forks": true},
		"cmd-review-fork", "fork-review", "")
	assert.Equal(t, store.TriggerSubjectModeNew, trForkReview.Subject.Mode)

	// 6. Preset PR-updated rule (§7.2) to exercise synchronize coalescing within the same project.
	trPRUpdated := applyPreset(githubtrigger.PresetPRUpdated,
		map[string]any{"base_branches": []string{"default"}, "coalesce_window": "30s"},
		"pr-updated-coalesce", "pr-sync", "")
	assert.Equal(t, store.TriggerSubjectModeReuse, trPRUpdated.Subject.Mode)
	assert.Equal(t, "30s", trPRUpdated.Subject.CoalesceWindow)

	// Controllable clock + real trigger.Dispatcher backed by the real durable queue.
	var clockMu sync.Mutex
	simNow := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return simNow
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		simNow = simNow.Add(d)
		clockMu.Unlock()
	}

	// Wire both GitHub comment rate limiters to the simulated clock so advancing
	// simulated time refills the per-project comment bucket deterministically.
	f.sched.srv.githubCommentLimiter = githubCommentRateLimiter{now: clock}
	f.sched.srv.unmatchedCommandLimiter = unmatchedCommandRateLimiter{now: clock}

	claims := trigger.NewSubjectClaims(f.sched.store, f.sched.store, clock)
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: claims,
		Rules:         f.sched.srv.TriggerRules(),
		Now:           clock,
	})
	require.NoError(t, err)

	dqStore, ok := f.sched.store.(store.DurableQueueStore)
	require.True(t, ok, "store must implement DurableQueueStore")

	// drainDue claims all due trigger.event items at the current simulated clock
	// and runs each through disp.HandleItem, acking done items and re-scheduling
	// deferred/coalescing items via FailDurableItem(NextAttemptAt=res.RetryAt).
	drainDue := func(t *testing.T) int {
		t.Helper()
		handled := 0
		for {
			now := clock()
			// handleGitHubWebhook enqueues durable_queue_items using wall time for
			// next_attempt_at, so snap simNow past wall time when needed to claim
			// newly enqueued deliveries.
			if wall := time.Now().UTC(); wall.After(now) {
				clockMu.Lock()
				simNow = wall.Add(time.Second).Truncate(time.Second)
				now = simNow
				clockMu.Unlock()
			}
			rows, cErr := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "g6e-drainer", now, now.Add(time.Minute), 20)
			require.NoError(t, cErr)
			if len(rows) == 0 {
				return handled
			}
			for _, r := range rows {
				item := durablequeue.Item{
					Seq:       r.Seq,
					Kind:      r.Kind,
					DedupKey:  r.DedupKey,
					ProjectID: r.ProjectID,
					Payload:   []byte(r.Payload),
					Attempts:  r.Attempts + 1,
				}
				res := disp.HandleItem(ctx, item, nil)
				handled++
				switch {
				case res.Done:
					held, ackErr := dqStore.CompleteDurableItem(ctx, r.Seq, "g6e-drainer")
					require.NoError(t, ackErr)
					require.True(t, held)
				case !res.RetryAt.IsZero():
					held, failErr := dqStore.FailDurableItem(ctx, r.Seq, "g6e-drainer", store.DurableQueueFailure{
						CountAttempt:  false,
						NextAttemptAt: res.RetryAt,
					})
					require.NoError(t, failErr)
					require.True(t, held)
				default:
					t.Fatalf("unexpected non-done HandleItem result: err=%v retryAt=%v", res.Err, res.RetryAt)
				}
			}
		}
	}

	postAndDrain := func(t *testing.T, eventName string, body []byte) string {
		t.Helper()
		rec := postSignedGitHubDelivery(t, f.sched.srv, webhookSecret, appID, eventName, uuid.NewString(), body)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		var resp GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Events, 1, "expected 1 queued event: %s", rec.Body.String())
		require.GreaterOrEqual(t, drainDue(t), 1)
		return resp.Events[0].EventID
	}

	dispatchCalls := func() []*RemoteCreateAgentRequest {
		f.client.mu.Lock()
		defer f.client.mu.Unlock()
		out := make([]*RemoteCreateAgentRequest, len(f.client.calls))
		copy(out, f.client.calls)
		return out
	}

	eventTS := time.Now().UTC().Truncate(time.Second)

	// -------------------------------------------------------------------------
	// 1. Fires per rule: /review, /validate, /fix, /plan, /implement
	// -------------------------------------------------------------------------

	// 1a. /review on PR #42 -> template="review", branch="feat/widget-42", ack comment with agent link.
	evReview := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 42, true, 1001, "alice", "User", "OWNER", "@scion-bot /review --thorough", eventTS,
	))
	calls := dispatchCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "review", calls[0].Config.Template)
	assert.Equal(t, "feat/widget-42", calls[0].Config.Branch)
	assert.Contains(t, calls[0].Name, "pr-42-review")

	firesReview, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evReview}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesReview.Items, 1)
	assert.Equal(t, trReview.ID, firesReview.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesReview.Items[0].Status)

	pr42Comments := gh.commentsOnPath("/repos/acme/widgets/issues/42/comments")
	require.Len(t, pr42Comments, 1, "/review must post 1 ack comment")
	assert.Contains(t, pr42Comments[0].Body, calls[0].Name)
	assert.Contains(t, pr42Comments[0].Body, "https://hub.example.test/")
	assert.Contains(t, pr42Comments[0].Body, "`/review`")

	// 1b. /validate on PR #42 -> template="validate", branch="feat/widget-42", ack comment.
	evValidate := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 42, true, 1002, "bob", "User", "MEMBER", "@scion-bot /validate", eventTS,
	))
	calls = dispatchCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, "validate", calls[1].Config.Template)
	assert.Equal(t, "feat/widget-42", calls[1].Config.Branch)
	firesValidate, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evValidate}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesValidate.Items, 1)
	assert.Equal(t, trValidate.ID, firesValidate.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesValidate.Items[0].Status)
	require.Len(t, gh.commentsOnPath("/repos/acme/widgets/issues/42/comments"), 2, "/validate must post 1 ack comment")

	// 1c. /fix (first time) on PR #42 -> template="fix", branch="feat/widget-42", ack comment.
	evFix1 := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 42, true, 1003, "carol", "User", "COLLABORATOR", "@scion-bot /fix flaky test", eventTS,
	))
	calls = dispatchCalls()
	require.Len(t, calls, 3)
	assert.Equal(t, "fix", calls[2].Config.Template)
	assert.Equal(t, "feat/widget-42", calls[2].Config.Branch)
	firesFix1, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evFix1}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesFix1.Items, 1)
	assert.Equal(t, trFix.ID, firesFix1.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesFix1.Items[0].Status)
	fixAgentID := firesFix1.Items[0].TargetAgentID
	require.NotEmpty(t, fixAgentID)
	require.Len(t, gh.commentsOnPath("/repos/acme/widgets/issues/42/comments"), 3, "first /fix must post 1 ack comment")

	// Mark the /fix agent running so the second /fix takes the reuse wake-message path.
	fixAgent, err := f.sched.store.GetAgent(ctx, fixAgentID)
	require.NoError(t, err)
	fixAgent.Phase = string(state.PhaseRunning)
	fixAgent.ContainerStatus = "running"
	require.NoError(t, f.sched.store.UpdateAgent(ctx, fixAgent))

	// 1d. /plan on Issue #101 -> template="plan", branch="" (none), ack comment.
	evPlan := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 101, false, 1004, "alice", "User", "OWNER", "@scion-bot /plan break down milestones", eventTS,
	))
	calls = dispatchCalls()
	require.Len(t, calls, 4)
	assert.Equal(t, "plan", calls[3].Config.Template)
	assert.Empty(t, calls[3].Config.Branch, "/plan must not pass a branch")
	firesPlan, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evPlan}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesPlan.Items, 1)
	assert.Equal(t, trPlan.ID, firesPlan.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesPlan.Items[0].Status)
	require.Len(t, gh.commentsOnPath("/repos/acme/widgets/issues/101/comments"), 1, "/plan must post 1 ack comment")

	// 1e. /implement on Issue #102 -> template="implement", branch="" (none), ack comment.
	evImpl := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 102, false, 1005, "bob", "User", "MEMBER", "@scion-bot /implement step 1", eventTS,
	))
	calls = dispatchCalls()
	require.Len(t, calls, 5)
	assert.Equal(t, "implement", calls[4].Config.Template)
	assert.Empty(t, calls[4].Config.Branch, "/implement must not pass a branch")
	firesImpl, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evImpl}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesImpl.Items, 1)
	assert.Equal(t, trImplement.ID, firesImpl.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesImpl.Items[0].Status)
	require.Len(t, gh.commentsOnPath("/repos/acme/widgets/issues/102/comments"), 1, "/implement must post 1 ack comment")

	// -------------------------------------------------------------------------
	// 2. /fix second time -> reuse wake message, no second agent, no second ack
	// -------------------------------------------------------------------------
	evFix2 := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 42, true, 1006, "alice", "User", "OWNER", "@scion-bot /fix also lint", eventTS,
	))
	assert.Len(t, dispatchCalls(), 5, "second /fix must reuse existing agent, not create a second agent")
	assert.Len(t, gh.commentsOnPath("/repos/acme/widgets/issues/42/comments"), 3, "reuse delivery must not post a second ack comment")

	msgs := tracker.messages()
	require.Len(t, msgs, 1, "second /fix must deliver exactly 1 reuse wake message")
	assert.Equal(t, fixAgent.Slug, msgs[0].AgentID)
	assert.Contains(t, msgs[0].Message, "also lint")
	assert.Contains(t, msgs[0].Message, "@alice")

	firesFix2, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evFix2}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesFix2.Items, 1)
	assert.Equal(t, trFix.ID, firesFix2.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesFix2.Items[0].Status)
	assert.Equal(t, fixAgentID, firesFix2.Items[0].TargetAgentID)

	// -------------------------------------------------------------------------
	// 3. Coalesced synchronize pair -> one dispatch with the latest SHA
	// -------------------------------------------------------------------------
	const (
		syncSHA1 = "aaaa111111111111111111111111111111111111"
		syncSHA2 = "bbbb222222222222222222222222222222222222"
	)
	evSync1 := postAndDrain(t, "pull_request", syntheticPRSynchronizePayload(
		t, instID, 42, "feat/widget-42", syncSHA1, "acme/widgets", "alice", "OWNER", eventTS,
	))
	assert.Len(t, dispatchCalls(), 5, "first synchronize inside coalesce window must not dispatch immediately")

	advanceClock(5 * time.Second)
	evSync2 := postAndDrain(t, "pull_request", syntheticPRSynchronizePayload(
		t, instID, 42, "feat/widget-42", syncSHA2, "acme/widgets", "alice", "OWNER", eventTS.Add(5*time.Second),
	))
	assert.Len(t, dispatchCalls(), 5, "second synchronize inside coalesce window must still defer until not_before")

	// First synchronize fire must now be marked skipped "coalesced".
	firesSync1, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evSync1, TriggerID: trPRUpdated.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesSync1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, firesSync1.Items[0].Status)
	assert.Equal(t, trigger.ErrFireCoalesced, firesSync1.Items[0].Error)

	// Advance clock past the 30s coalesce window and drain -> exactly 1 dispatch with latest SHA.
	advanceClock(35 * time.Second)
	require.GreaterOrEqual(t, drainDue(t), 1)

	calls = dispatchCalls()
	require.Len(t, calls, 6, "coalesced synchronize pair must produce exactly one dispatch")
	assert.Equal(t, "pr-sync", calls[5].Config.Template)
	assert.Equal(t, "feat/widget-42", calls[5].Config.Branch)
	assert.Contains(t, calls[5].Config.Task, syncSHA2, "coalesced dispatch must carry the latest SHA")
	assert.NotContains(t, calls[5].Config.Task, syncSHA1, "coalesced dispatch must not carry the superseded SHA")

	firesSync2, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evSync2, TriggerID: trPRUpdated.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesSync2.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, firesSync2.Items[0].Status)

	// -------------------------------------------------------------------------
	// 4. /frobnicate -> exactly one unmatched guidance reply
	// -------------------------------------------------------------------------
	advanceClock(5 * time.Second)
	commentsBeforeFrob := len(gh.commentsOnPath("/repos/acme/widgets/issues/42/comments"))
	evFrob := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 42, true, 1007, "alice", "User", "OWNER", "@scion-bot /frobnicate", eventTS,
	))
	assert.Len(t, dispatchCalls(), 6, "/frobnicate must not dispatch any agent")
	firesFrob, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evFrob}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, firesFrob.Items, "/frobnicate must record no fires")

	pr42AfterFrob := gh.commentsOnPath("/repos/acme/widgets/issues/42/comments")
	require.Len(t, pr42AfterFrob, commentsBeforeFrob+1, "/frobnicate must post exactly one unmatched guidance reply")
	assert.Contains(t, pr42AfterFrob[len(pr42AfterFrob)-1].Body, "`/frobnicate`")

	// -------------------------------------------------------------------------
	// 5. App's own comment -> no event row
	// -------------------------------------------------------------------------
	eventsBeforeSelf, err := f.sched.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)

	selfRec := postSignedGitHubDelivery(t, f.sched.srv, webhookSecret, appID, "issue_comment", uuid.NewString(),
		syntheticIssueCommentPayload(t, instID, 42, true, 1008, appSlug+"[bot]", "Bot", "NONE", "@scion-bot /review", eventTS))
	require.Equal(t, http.StatusAccepted, selfRec.Code, selfRec.Body.String())
	var selfResp map[string]any
	require.NoError(t, json.Unmarshal(selfRec.Body.Bytes(), &selfResp))
	assert.Equal(t, "ignored", selfResp["status"])
	assert.Equal(t, "self", selfResp["reason"])

	eventsAfterSelf, err := f.sched.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, eventsAfterSelf.Items, len(eventsBeforeSelf.Items), "App's own comment must create no trigger_events row")
	assert.Len(t, dispatchCalls(), 6)

	// -------------------------------------------------------------------------
	// 6. NONE-association commenter -> no fire under presets
	// -------------------------------------------------------------------------
	evNone := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 42, true, 1009, "outside-user", "User", "NONE", "@scion-bot /review", eventTS,
	))
	assert.Len(t, dispatchCalls(), 6, "NONE-association commenter must not dispatch any agent")
	firesNone, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evNone}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, firesNone.Items, "NONE-association commenter must record no fires under presets")

	// -------------------------------------------------------------------------
	// 7. Fork PR -> fork-guarded /review does not fire; PR-scoped fork-enabled
	//    /review-fork dispatches with no branch and `gh pr checkout 55` guidance
	//    (and /implement on=any also dispatches with no branch)
	// -------------------------------------------------------------------------
	evForkReview := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 55, true, 1010, "alice", "User", "OWNER", "@scion-bot /review", eventTS,
	))
	assert.Len(t, dispatchCalls(), 6, "fork-guarded /review must not fire on a fork PR")
	firesForkReview, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evForkReview}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, firesForkReview.Items)

	evForkImpl := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 55, true, 1011, "alice", "User", "OWNER", "@scion-bot /implement", eventTS,
	))
	calls = dispatchCalls()
	require.Len(t, calls, 7, "fork-enabled /implement must dispatch on fork PR")
	assert.Equal(t, "implement", calls[6].Config.Template)
	assert.Empty(t, calls[6].Config.Branch, "/implement (on=any) must pass no branch")
	firesForkImpl, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evForkImpl}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesForkImpl.Items, 1)
	assert.Equal(t, trImplement.ID, firesForkImpl.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesForkImpl.Items[0].Status)

	advanceClock(2 * time.Second)
	evForkPRScoped := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 55, true, 1014, "alice", "User", "OWNER", "@scion-bot /review-fork", eventTS,
	))
	calls = dispatchCalls()
	require.Len(t, calls, 8, "PR-scoped fork-enabled /review-fork must dispatch on fork PR")
	assert.Equal(t, "fork-review", calls[7].Config.Template)
	assert.Empty(t, calls[7].Config.Branch, "PR-scoped fork-enabled rule must omit branch on fork PR")
	assert.Contains(t, calls[7].Config.Task, "gh pr checkout 55", "PR-scoped fork-enabled rule task must instruct gh pr checkout 55")
	firesForkPRScoped, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evForkPRScoped}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesForkPRScoped.Items, 1)
	assert.Equal(t, trForkReview.ID, firesForkPRScoped.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusFired, firesForkPRScoped.Items[0].Status)

	// -------------------------------------------------------------------------
	// 8. Carried §8.3 item A: unmatched-command reply when CEL did not match
	//    (/review is a match_types candidate on issue_comment.created, but CEL
	//    is_pull_request == 'true' evaluates false on plain Issue #103 -> reply posted)
	// -------------------------------------------------------------------------
	advanceClock(5 * time.Second)
	evCELNoMatch := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 103, false, 1012, "alice", "User", "OWNER", "@scion-bot /review", eventTS,
	))
	assert.Len(t, dispatchCalls(), 8, "CEL-unmatched /review on an issue must not dispatch")
	firesCELNoMatch, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evCELNoMatch}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, firesCELNoMatch.Items)

	issue103Comments := gh.commentsOnPath("/repos/acme/widgets/issues/103/comments")
	require.Len(t, issue103Comments, 1, "unmatched-command reply must post when CEL did not match (match_count=0, no fires)")
	assert.Contains(t, issue103Comments[0].Body, "`/review`")

	// -------------------------------------------------------------------------
	// 9. Carried §8.3 item B: NO unmatched-command reply when the only candidate
	//    fire was skipped (PR #77 enrichment fails -> missing is_fork -> fail-closed
	//    CEL eval error records a skipped fire -> §8.3 "a rule was there")
	// -------------------------------------------------------------------------
	evSkippedOnly := postAndDrain(t, "issue_comment", syntheticIssueCommentPayload(
		t, instID, 77, true, 1013, "alice", "User", "OWNER", "@scion-bot /review", eventTS,
	))
	assert.Len(t, dispatchCalls(), 8, "skipped /review must not dispatch")
	firesSkippedOnly, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evSkippedOnly}, store.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, firesSkippedOnly.Items, "enrichment failure on PR #77 must record a skipped fire")
	for _, fr := range firesSkippedOnly.Items {
		assert.Equal(t, store.TriggerFireStatusSkipped, fr.Status, "every candidate fire on PR #77 must be skipped")
	}

	pr77Comments := gh.commentsOnPath("/repos/acme/widgets/issues/77/comments")
	assert.Empty(t, pr77Comments, "unmatched-command reply must NOT post when a candidate fire was recorded as skipped (§8.3)")
}
