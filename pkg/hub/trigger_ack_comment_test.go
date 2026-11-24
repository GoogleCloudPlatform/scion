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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func setupAckCommentFixture(t *testing.T, tag string) (*triggerDispatchAgentFixture, *fakeGitHubCommentAPI, *store.User, *store.Project) {
	t.Helper()
	f := newTriggerDispatchAgentFixture(t, tag)
	ctx := context.Background()

	gh := newFakeGitHubCommentAPI(t)
	f.sched.srv.mu.Lock()
	f.sched.srv.config.GitHubAppConfig.APIBaseURL = gh.srv.URL
	f.sched.srv.config.GitHubAppConfig.AppID = 778899
	f.sched.srv.config.GitHubAppConfig.AppSlug = "scion-bot"
	f.sched.srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	f.sched.srv.config.GitHubAppConfig.WebhookSecret = "test-webhook-secret"
	f.sched.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.sched.srv.config.HubEndpoint = "https://hub.example.test"
	f.sched.srv.config.HubEndpoints = &HubEndpoints{Public: "https://hub.example.test"}
	f.sched.srv.mu.Unlock()

	setTriggerExperimentsForG1bTest(t, f.sched.srv, true, true)

	const instID int64 = 8301
	createAssocInstallation(t, f.sched.store, instID, "acme", "acme/widgets")

	admin := createAssocAdmin(t, f.sched.store, "ack-admin-"+tag)
	proj, err := f.sched.store.GetProject(ctx, f.sched.proj.ID)
	require.NoError(t, err)
	// Use https:// with the fake GitHub server's host so both githubOwnerRepo
	// (host-pinned against APIBaseURL) and clone-per-agent URL validation accept it.
	remote := "https://" + strings.TrimPrefix(gh.srv.URL, "http://") + "/acme/widgets"
	proj.GitRemote = remote
	idCopy := instID
	proj.GitHubInstallationID = &idCopy
	proj.GitHubAppStatus = &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now().UTC()}
	require.NoError(t, f.sched.store.UpdateProject(ctx, proj))
	setAdminBinding(t, f.sched.store, proj, instID, util.NormalizeGitRemote(remote), admin.ID)
	f.sched.srv.invalidateGitHubAssociation(proj.ID)

	// Make f.sched.creator an owner so they can author github triggers and dispatch agents.
	ownerCtx := f.userAuth(t, f.sched.creator)
	_ = ownerCtx
	return f, gh, admin, proj
}

func TestDispatchAgentAckComment_AuthoringValidation(t *testing.T) {
	f, _, admin, proj := setupAckCommentFixture(t, "ack-authoring")
	creatorCtx := f.userAuth(t, f.sched.creator)
	adminCtx := f.userAuth(t, admin)
	ep := f.createEndpoint(t, creatorCtx, "ack-wh-ep")

	// 1. Non-github trigger with ackComment -> 400 ack_comment_requires_github_source.
	recWH := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "wh-ack-rejected",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{
				Type:       store.TriggerActionDispatchAgent,
				Task:       "run",
				AckComment: &DispatchAgentAckComment{Body: "Started ${result.agent_name}"},
			},
		})
	require.Equal(t, http.StatusBadRequest, recWH.Code, recWH.Body.String())
	assert.Equal(t, "ack_comment_requires_github_source", decodeAPIErrorCode(t, recWH))

	// 2. Empty ackComment.body -> 400.
	recEmpty := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-ack-empty",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:       store.TriggerActionDispatchAgent,
				Task:       "run",
				AckComment: &DispatchAgentAckComment{Body: ""},
			},
		})
	require.Equal(t, http.StatusBadRequest, recEmpty.Code, recEmpty.Body.String())

	// 3. Un-opted-in untrusted variable in ackComment.body -> 400 untrusted_var_not_allowed.
	recUntrusted := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-ack-untrusted",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:       store.TriggerActionDispatchAgent,
				Task:       "run",
				AckComment: &DispatchAgentAckComment{Body: "Requested by @${event.attributes.sender}"},
			},
		})
	require.Equal(t, http.StatusBadRequest, recUntrusted.Code, recUntrusted.Body.String())
	assert.Equal(t, "untrusted_var_not_allowed", decodeAPIErrorCode(t, recUntrusted))

	// 4. Valid github trigger with PhasePost result.* and opted-in untrusted var -> 201 Created, round-trips.
	ackBody := "Started **${result.agent_name}** (${result.agent_id}) — [open](${result.agent_url}) [fire ${result.fire_id}] for @${event.attributes.sender}"
	recOK := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-ack-ok",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				Task:          "review PR",
				AckComment:    &DispatchAgentAckComment{Body: ackBody},
				UntrustedVars: []string{"event.attributes.sender"},
			},
		})
	require.Equal(t, http.StatusCreated, recOK.Code, recOK.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(recOK.Body.Bytes(), &created))
	require.NotNil(t, created.Action.AckComment)
	assert.Equal(t, ackBody, created.Action.AckComment.Body)

	// 5. PATCH changing source from github to webhook while keeping ackComment -> 400 ack_comment_requires_github_source.
	recPatch := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", proj.ID, created.ID),
		map[string]any{
			"source": TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			"match":  map[string]any{"types": []string{"webhook.received"}},
		})
	require.Equal(t, http.StatusBadRequest, recPatch.Code, recPatch.Body.String())
	assert.Equal(t, "ack_comment_requires_github_source", decodeAPIErrorCode(t, recPatch))

	// 6. Schedule payload with ackComment is rejected as trigger-only.
	assert.Equal(t, "ackComment", unsupportedScheduledDispatchField(DispatchAgentEventPayload{
		AckComment: &DispatchAgentAckComment{Body: "hi"},
	}))

	// 6b. N1: ackComment is rejected on message and github.comment actions (not silently dropped).
	recCommentWithAck := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-comment-with-ack",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:       store.TriggerActionGitHubComment,
				Body:       "hello",
				AckComment: &DispatchAgentAckComment{Body: "ack"},
			},
		})
	require.Equal(t, http.StatusBadRequest, recCommentWithAck.Code, recCommentWithAck.Body.String())
	assert.Contains(t, recCommentWithAck.Body.String(), "action.ackComment")

	recMsgWithAck := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "msg-with-ack",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:       store.TriggerActionMessage,
				AgentID:    uuid.NewString(),
				Message:    "hi",
				AckComment: &DispatchAgentAckComment{Body: "ack"},
			},
		})
	require.Equal(t, http.StatusBadRequest, recMsgWithAck.Code, recMsgWithAck.Body.String())
	assert.Contains(t, recMsgWithAck.Body.String(), "action.ackComment")

	// 7. O2: UAT scoped to trigger:create + agent:create (without github_trigger:comment)
	// is 403 Forbidden with ackComment, and 201 Created for the identical payload without it.
	ctx := context.Background()
	uatExp := time.Now().UTC().Add(time.Hour)
	uat := &store.UserAccessToken{
		ID:                   uuid.NewString(),
		UserID:               f.sched.creator.ID,
		ProjectID:            proj.ID,
		Name:                 "ack-perm-isolation-uat",
		Prefix:               "scion_pat_iso",
		KeyHash:              "hash-" + uuid.NewString(),
		Scopes:               []string{"trigger:create", "agent:create", "project:read"},
		BoundaryKind:         "project",
		CeilingVersion:       permissions.CeilingVersionV1,
		CeilingPermissionIDs: []string{"trigger.create", "agent.create", "project.read"},
		ExpiresAt:            &uatExp,
	}
	require.NoError(t, f.sched.store.CreateUserAccessToken(ctx, uat))
	baseUser := NewAuthenticatedUser(f.sched.creator.ID, f.sched.creator.Email, f.sched.creator.DisplayName, f.sched.creator.Role, "api")
	scopedIdent := NewScopedUserIdentityWithBoundary(
		baseUser,
		TokenBoundary{Kind: BoundaryKindProject, ProjectID: proj.ID},
		uat.Scopes,
		uat.ID,
		uat.NormalizedCeiling(),
	)
	uatCtx := contextWithIdentity(context.Background(), scopedIdent)
	uatCtx = contextWithCredentialContext(uatCtx, credentialContextForIdentity(scopedIdent))

	recUATWithAck := f.doJSON(t, uatCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-uat-with-ack",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:       store.TriggerActionDispatchAgent,
				Task:       "review PR",
				NoAuth:     true,
				AckComment: &DispatchAgentAckComment{Body: "Started ${result.agent_name}"},
			},
		})
	assert.Equal(t, http.StatusForbidden, recUATWithAck.Code, recUATWithAck.Body.String())

	recUATNoAck := f.doJSON(t, uatCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-uat-no-ack",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:   store.TriggerActionDispatchAgent,
				Task:   "review PR",
				NoAuth: true,
			},
		})
	assert.Equal(t, http.StatusCreated, recUATNoAck.Code, recUATNoAck.Body.String())
}

func TestDispatchAgentAckComment_FireLeaseLossOutcomeIsolationAndNoBodyLog(t *testing.T) {
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	f, gh, _, proj := setupAckCommentFixture(t, "ack-fire")
	ctx := context.Background()

	const secretBodyMarker = "ACK_SECRET_BODY_DO_NOT_LOG_98765"
	ackBody := secretBodyMarker + " Started **${result.agent_name}** (${result.agent_id}) — [open](${result.agent_url}) [fire ${result.fire_id}] for @${event.attributes.sender}"

	triggerID := uuid.NewString()
	fireID := uuid.NewString()
	eventID := uuid.NewString()
	receivedAt := time.Now().UTC().Add(-time.Minute)

	makeReq := func(fid string, attempts int, nameConflict string, attrs map[string]string) hubfire.FireRequest {
		p := DispatchAgentEventPayload{
			AgentName:     "pr-${event.attributes.number}-review",
			NameConflict:  nameConflict,
			Task:          "review PR ${event.attributes.number}",
			GithubToken:   DispatchAgentGitHubTokenInstallation,
			AckComment:    &DispatchAgentAckComment{Body: ackBody},
			UntrustedVars: []string{"event.attributes.number", "event.attributes.sender"},
		}
		raw, mErr := json.Marshal(p)
		require.NoError(t, mErr)
		return hubfire.FireRequest{
			ProjectID: proj.ID,
			Authority: hubfire.RecordedAuthority{
				Attribution: store.InitiatorAttribution{
					InitiatorPrincipalKind:  store.DelegationPrincipalUser,
					InitiatorPrincipalID:    f.sched.creator.ID,
					InitiatorCredentialKind: store.InitiatorCredentialKindSession,
					AttributionVersion:      1,
					AuthorizationRevision:   1,
				},
				Ceiling:        store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   triggerID,
				FireID:         fid,
			},
			ActionType: store.TriggerActionDispatchAgent,
			Payload:    string(raw),
			FireAt:     time.Now().UTC(),
			Attempts:   attempts,
			Event: &hubfire.FireEvent{
				ID:         eventID,
				SourceKind: store.TriggerSourceKindGitHub,
				SourceID:   "8301",
				Type:       "github.issue_comment.created",
				Subject:    "github:acme/widgets#42",
				ReceivedAt: receivedAt,
				Attributes: attrs,
				Trigger: &hubfire.FireTriggerRef{
					ID:          triggerID,
					Name:        "gh-ack-trigger",
					SubjectMode: store.TriggerSubjectModeNew,
					FireSubject: "github:acme/widgets#42",
				},
			},
		}
	}

	baseAttrs := map[string]string{
		"repo":       "acme/widgets",
		"repo_owner": "acme",
		"repo_name":  "widgets",
		"number":     "42",
		"sender":     "alice",
		"command":    "review",
	}

	// 1. Fired dispatch renders PhasePost result.* and posts exactly one ack comment with marker.
	req1 := makeReq(fireID, 1, DispatchAgentNameConflictSuffix, baseAttrs)
	out1 := f.sched.srv.fireAction(ctx, req1)
	require.Equal(t, hubfire.FireStatusFired, out1.Status, out1.Error)
	require.NotEmpty(t, out1.TargetAgentID)
	assert.Equal(t, out1.TargetAgentID, out1.ResultRef)

	gh.mu.Lock()
	require.Len(t, gh.comments, 1)
	posted := gh.comments[0]
	assert.Equal(t, 1, gh.postCount)
	gh.mu.Unlock()

	expectedURL := "https://hub.example.test/agents/" + out1.TargetAgentID
	assert.Contains(t, posted.Body, "Started **pr-42-review**")
	assert.Contains(t, posted.Body, "("+out1.TargetAgentID+")")
	assert.Contains(t, posted.Body, "[open]("+expectedURL+")")
	assert.Contains(t, posted.Body, "[fire "+fireID+"]")
	assert.Contains(t, posted.Body, "@alice")
	assert.True(t, strings.HasSuffix(posted.Body, "<!-- scion fire:"+fireID+" -->"), "body: %s", posted.Body)

	// 2. Lease-loss retry (Attempts > 1 with same fireID) scans comments for marker and does NOT double-post.
	reqLeaseRetry := makeReq(fireID, 2, DispatchAgentNameConflictSuffix, baseAttrs)
	outRetry := f.sched.srv.fireAction(ctx, reqLeaseRetry)
	require.Equal(t, hubfire.FireStatusFired, outRetry.Status, outRetry.Error)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "lease-loss retry must not post a duplicate ack comment")
	assert.GreaterOrEqual(t, gh.listCount, 1, "lease-loss retry must scan existing comments for marker")
	gh.mu.Unlock()

	// 3. Skipped outcome (nameConflict: skip on existing slug) must NOT post an ack comment.
	reqSkipped := makeReq(uuid.NewString(), 1, DispatchAgentNameConflictSkip, baseAttrs)
	outSkipped := f.sched.srv.fireAction(ctx, reqSkipped)
	require.Equal(t, hubfire.FireStatusSkipped, outSkipped.Status)
	assert.Equal(t, "agent name in use", outSkipped.Error)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "skipped dispatch must not post ack comment")
	gh.mu.Unlock()

	// 4. Refused outcome (missing required untrusted attr for template render) must NOT post an ack comment.
	reqRefused := makeReq(uuid.NewString(), 1, DispatchAgentNameConflictSuffix, map[string]string{
		"repo":       "acme/widgets",
		"repo_owner": "acme",
		"repo_name":  "widgets",
		"sender":     "alice",
		// number missing -> Name/Task template render refuses
	})
	outRefused := f.sched.srv.fireAction(ctx, reqRefused)
	require.Equal(t, hubfire.FireStatusRefused, outRefused.Status)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "refused dispatch must not post ack comment")
	gh.mu.Unlock()

	// 5. Ack failure (project github-permissions excludes issues:write) must NOT change dispatch outcome,
	// and must record the failure visibly without logging the comment body.
	projUpdated, err := f.sched.store.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	projUpdated.GitHubPermissions = &store.GitHubTokenPermissions{Contents: "read", Metadata: "read"}
	require.NoError(t, f.sched.store.UpdateProject(ctx, projUpdated))
	f.sched.srv.githubTokenCache.invalidateProject(proj.ID)

	logBuf.Reset()

	failFireID := uuid.NewString()
	reqPermDenied := makeReq(failFireID, 1, DispatchAgentNameConflictSuffix, baseAttrs)
	outPermDenied := f.sched.srv.fireAction(ctx, reqPermDenied)
	require.Equal(t, hubfire.FireStatusFired, outPermDenied.Status, "ack failure must not change dispatch outcome")
	require.NotEmpty(t, outPermDenied.TargetAgentID)
	assert.Empty(t, outPermDenied.Error)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "ack without issues:write permission must be refused")
	gh.mu.Unlock()

	logged := logBuf.String()
	assert.Contains(t, logged, githubAckCommentFailedAuditEventType)
	assert.Contains(t, logged, "github permission not permitted")
	assert.NotContains(t, logged, secretBodyMarker, "ackComment body must never appear in logs or audit")

	// 6. Fire-time github_trigger.comment authorization revocation (frozen ceiling
	// omits github_trigger.comment) refuses only the ack and keeps dispatch fired.
	projRestore, err := f.sched.store.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	projRestore.GitHubPermissions = nil
	require.NoError(t, f.sched.store.UpdateProject(ctx, projRestore))
	f.sched.srv.githubTokenCache.invalidateProject(proj.ID)

	logBuf.Reset()
	// Fire-time github_trigger.comment authorization revocation via a UAT whose
	// frozen ceiling permits agent.create (with noAuth) but omits github_trigger.comment:
	// dispatch_agent succeeds (fired), while only ackComment is refused.
	uatExp := time.Now().UTC().Add(time.Hour)
	uat := &store.UserAccessToken{
		ID:                   uuid.NewString(),
		UserID:               f.sched.creator.ID,
		ProjectID:            proj.ID,
		Name:                 "ack-ceiling-uat",
		Prefix:               "scion_pat_ack",
		KeyHash:              "hash-" + uuid.NewString(),
		Scopes:               []string{"agent:create"},
		BoundaryKind:         "project",
		CeilingVersion:       permissions.CeilingVersionV1,
		CeilingPermissionIDs: []string{"agent.create"},
		ExpiresAt:            &uatExp,
	}
	require.NoError(t, f.sched.store.CreateUserAccessToken(ctx, uat))

	ceilingPayload, mErr := json.Marshal(DispatchAgentEventPayload{
		AgentName:     "pr-${event.attributes.number}-ceiling",
		Task:          "review PR ${event.attributes.number}",
		NoAuth:        true,
		GithubToken:   DispatchAgentGitHubTokenInstallation,
		AckComment:    &DispatchAgentAckComment{Body: ackBody},
		UntrustedVars: []string{"event.attributes.number", "event.attributes.sender"},
	})
	require.NoError(t, mErr)
	reqCeilingDenied := makeReq(uuid.NewString(), 1, DispatchAgentNameConflictSuffix, baseAttrs)
	reqCeilingDenied.Payload = string(ceilingPayload)
	reqCeilingDenied.Authority.Attribution.InitiatorCredentialKind = store.InitiatorCredentialKindUAT
	reqCeilingDenied.Authority.Attribution.InitiatorCredentialID = uat.ID
	reqCeilingDenied.Authority.Ceiling = store.EffectCeiling{
		Kind:              store.EffectCeilingBounded,
		Version:           permissions.CeilingVersionV1,
		PermissionIDs:     []string{"agent.create"},
		BoundaryKind:      "project",
		BoundaryProjectID: proj.ID,
	}
	outCeilingDenied := f.sched.srv.fireAction(ctx, reqCeilingDenied)
	require.Equal(t, hubfire.FireStatusFired, outCeilingDenied.Status, "revoked github_trigger.comment must not fail dispatch (err=%s, log=%s)", outCeilingDenied.Error, logBuf.String())
	assert.Empty(t, outCeilingDenied.Error)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "ack without github_trigger.comment in ceiling must not post")
	gh.mu.Unlock()
	assert.Contains(t, logBuf.String(), githubAckCommentFailedAuditEventType)
	assert.Contains(t, logBuf.String(), "github.comment refused")
	assert.NotContains(t, logBuf.String(), secretBodyMarker)

	// 7. O1: when no public hub endpoint is configured, Server.AgentWebURL
	// returns "" so result.agent_url renders empty rather than a broken relative
	// /agents/<id> link.
	f.sched.srv.mu.Lock()
	f.sched.srv.config.HubEndpoint = ""
	f.sched.srv.config.HubEndpoints = nil
	f.sched.srv.mu.Unlock()

	noURLFireID := uuid.NewString()
	noURLPayload, mErr := json.Marshal(DispatchAgentEventPayload{
		AgentName:     "pr-${event.attributes.number}-nourl",
		Task:          "review PR ${event.attributes.number}",
		GithubToken:   DispatchAgentGitHubTokenInstallation,
		AckComment:    &DispatchAgentAckComment{Body: "Started **${result.agent_name}** url=[${result.agent_url}]"},
		UntrustedVars: []string{"event.attributes.number"},
	})
	require.NoError(t, mErr)
	reqNoURL := makeReq(noURLFireID, 1, DispatchAgentNameConflictSuffix, baseAttrs)
	reqNoURL.Payload = string(noURLPayload)
	outNoURL := f.sched.srv.fireAction(ctx, reqNoURL)
	require.Equal(t, hubfire.FireStatusFired, outNoURL.Status, outNoURL.Error)

	gh.mu.Lock()
	require.Len(t, gh.comments, 2)
	noURLBody := gh.comments[1].Body
	gh.mu.Unlock()
	assert.Contains(t, noURLBody, "url=[]", "result.agent_url must render empty when no public endpoint is configured")
	assert.NotContains(t, noURLBody, "/agents/", "must not emit broken relative /agents/ link")
}

func TestDispatchAgentAckComment_AgentCallerForbidden(t *testing.T) {
	f, _, _, proj := setupAckCommentFixture(t, "ack-agent-forbid")
	ctx := context.Background()

	// Seed a full-role agent in the project and give it agent-JWT auth.
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "full-author-agent",
		Name:      "full-author-agent",
		ProjectID: proj.ID,
		OwnerID:   f.sched.creator.ID,
		Phase:     string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleFull),
		},
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, ag))
	agentCtx := f.agentAuth(t, ag, AgentRoleFull)

	// Agent callers cannot author dispatch_agent with ackComment (design rev 1.12 §8.1:
	// github_trigger.comment has no agent scope).
	rec := f.doJSON(t, agentCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "agent-ack-denied",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:       store.TriggerActionDispatchAgent,
				Task:       "run",
				AckComment: &DispatchAgentAckComment{Body: "Started ${result.agent_name}"},
			},
		})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "ackComment")
}

func TestGitHubOwnAppUnknownIdentity_BotRefusal(t *testing.T) {
	f := newTriggerDispatchAgentFixture(t, "own-app-unknown-id")
	ctx := context.Background()

	var appFail atomic.Bool
	appFail.Store(true)

	var commentPostCount atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		if appFail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "github down"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          778899,
			"slug":        "recovered-app",
			"name":        "Recovered App",
			"permissions": map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"},
		})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       "ghs_ownapp_test_tok",
			"expires_at":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": map[string]string{"issues": "write", "metadata": "read"},
		})
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			commentPostCount.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(githubapp.IssueComment{
				ID:      501,
				HTMLURL: "https://github.com/acme/widgets/issues/7#issuecomment-501",
			})
			return
		}
		http.NotFound(w, r)
	})
	ghSrv := httptest.NewServer(mux)
	t.Cleanup(ghSrv.Close)

	f.sched.srv.mu.Lock()
	f.sched.srv.config.GitHubAppConfig.APIBaseURL = ghSrv.URL
	f.sched.srv.config.GitHubAppConfig.AppID = 778899
	f.sched.srv.config.GitHubAppConfig.AppSlug = "" // unknown until GET /app succeeds
	f.sched.srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	f.sched.srv.config.GitHubAppConfig.WebhookSecret = "test-webhook-secret"
	f.sched.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.sched.srv.mu.Unlock()
	f.sched.srv.githubAppIdentity.reset()

	setTriggerExperimentsForG1bTest(t, f.sched.srv, true, true)

	const instID int64 = 8401
	createAssocInstallation(t, f.sched.store, instID, "acme", "acme/widgets")
	admin := createAssocAdmin(t, f.sched.store, "ownapp-admin")
	proj, err := f.sched.store.GetProject(ctx, f.sched.proj.ID)
	require.NoError(t, err)
	remote := "https://" + strings.TrimPrefix(ghSrv.URL, "http://") + "/acme/widgets"
	proj.GitRemote = remote
	idCopy := instID
	proj.GitHubInstallationID = &idCopy
	// Explicit project permissions so github.comment mint doesn't require GET /app for defaults.
	proj.GitHubPermissions = &store.GitHubTokenPermissions{Issues: "write", Metadata: "read"}
	proj.GitHubAppStatus = &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now().UTC()}
	require.NoError(t, f.sched.store.UpdateProject(ctx, proj))
	setAdminBinding(t, f.sched.store, proj, instID, util.NormalizeGitRemote(remote), admin.ID)
	f.sched.srv.invalidateGitHubAssociation(proj.ID)

	// Running target agent for message action test.
	targetAgent := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "msg-target-agent",
		Name:            "msg-target-agent",
		ProjectID:       proj.ID,
		OwnerID:         f.sched.creator.ID,
		RuntimeBrokerID: f.sched.broker.ID,
		Phase:           string(state.PhaseRunning),
		ContainerStatus: "running",
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, targetAgent))

	makeFireReq := func(actionType, payload, senderType, sender string) hubfire.FireRequest {
		return hubfire.FireRequest{
			ProjectID: proj.ID,
			Authority: hubfire.RecordedAuthority{
				Attribution: store.InitiatorAttribution{
					InitiatorPrincipalKind:  store.DelegationPrincipalUser,
					InitiatorPrincipalID:    f.sched.creator.ID,
					InitiatorCredentialKind: store.InitiatorCredentialKindSession,
					AttributionVersion:      1,
					AuthorizationRevision:   1,
				},
				Ceiling:        store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   uuid.NewString(),
				FireID:         uuid.NewString(),
			},
			ActionType: actionType,
			Payload:    payload,
			FireAt:     time.Now().UTC(),
			Event: &hubfire.FireEvent{
				ID:         uuid.NewString(),
				SourceKind: store.TriggerSourceKindGitHub,
				SourceID:   "8401",
				Type:       "github.pull_request.synchronize",
				Subject:    "github:acme/widgets#7",
				ReceivedAt: time.Now().UTC(),
				Attributes: map[string]string{
					"repo":        "acme/widgets",
					"repo_owner":  "acme",
					"repo_name":   "widgets",
					"number":      "7",
					"sender_type": senderType,
					"sender":      sender,
				},
			},
		}
	}

	dispatchPayload := `{"task":"check PR","githubToken":"installation"}`
	commentPayload := `{"body":"hello from trigger"}`

	// 1. Unknown identity + Bot sender -> dispatch_agent and github.comment refused.
	outDispatchBot := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionDispatchAgent, dispatchPayload, "Bot", "dependabot[bot]"))
	assert.Equal(t, hubfire.FireStatusRefused, outDispatchBot.Status)
	assert.Equal(t, "github app identity unknown", outDispatchBot.Error)

	outCommentBot := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionGitHubComment, commentPayload, "Bot", "dependabot[bot]"))
	assert.Equal(t, hubfire.FireStatusRefused, outCommentBot.Status)
	assert.Equal(t, "github app identity unknown", outCommentBot.Error)
	assert.Zero(t, commentPostCount.Load())

	// 2. Unknown identity + User sender -> dispatch_agent and github.comment fire normally.
	outDispatchUser := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionDispatchAgent, dispatchPayload, "User", "alice"))
	assert.Equal(t, hubfire.FireStatusFired, outDispatchUser.Status, outDispatchUser.Error)

	outCommentUser := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionGitHubComment, commentPayload, "User", "alice"))
	assert.Equal(t, hubfire.FireStatusFired, outCommentUser.Status, outCommentUser.Error)
	assert.Equal(t, int64(1), commentPostCount.Load())

	// 3. Unknown identity + Bot sender on message action -> unaffected (fires normally).
	msgPayload := fmt.Sprintf(`{"agentId":%q,"message":"bot event"}`, targetAgent.ID)
	outMsgBot := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionMessage, msgPayload, "Bot", "dependabot[bot]"))
	assert.Equal(t, hubfire.FireStatusFired, outMsgBot.Status, outMsgBot.Error)

	// 4. Known identity via AppSlug override:
	//    - own App login (configured-app[bot]) is refused as "github own app event";
	//    - different bot (dependabot[bot]) fires normally.
	f.sched.srv.mu.Lock()
	f.sched.srv.config.GitHubAppConfig.AppSlug = "configured-app"
	f.sched.srv.mu.Unlock()

	outDispatchOwnSlug := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionDispatchAgent, dispatchPayload, "Bot", "configured-app[bot]"))
	assert.Equal(t, hubfire.FireStatusRefused, outDispatchOwnSlug.Status)
	assert.Equal(t, "github own app event", outDispatchOwnSlug.Error)

	outCommentOwnSlug := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionGitHubComment, commentPayload, "Bot", "configured-app[bot]"))
	assert.Equal(t, hubfire.FireStatusRefused, outCommentOwnSlug.Status)
	assert.Equal(t, "github own app event", outCommentOwnSlug.Error)
	assert.Equal(t, int64(1), commentPostCount.Load())

	outDispatchKnown := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionDispatchAgent, dispatchPayload, "Bot", "dependabot[bot]"))
	assert.Equal(t, hubfire.FireStatusFired, outDispatchKnown.Status, outDispatchKnown.Error)

	outCommentKnown := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionGitHubComment, commentPayload, "Bot", "dependabot[bot]"))
	assert.Equal(t, hubfire.FireStatusFired, outCommentKnown.Status, outCommentKnown.Error)
	assert.Equal(t, int64(2), commentPostCount.Load())

	// 5. Identity recovered via GET /app (AppSlug unset, GET /app now succeeds):
	//    - own App login (recovered-app[bot]) enqueued during unknown window is refused as "github own app event";
	//    - different bot (dependabot[bot]) fires normally;
	//    - message action stays unaffected.
	f.sched.srv.mu.Lock()
	f.sched.srv.config.GitHubAppConfig.AppSlug = ""
	f.sched.srv.mu.Unlock()
	appFail.Store(false)
	f.sched.srv.githubAppIdentity.reset()

	outDispatchOwnRecovered := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionDispatchAgent, dispatchPayload, "Bot", "recovered-app[bot]"))
	assert.Equal(t, hubfire.FireStatusRefused, outDispatchOwnRecovered.Status)
	assert.Equal(t, "github own app event", outDispatchOwnRecovered.Error)

	outCommentOwnRecovered := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionGitHubComment, commentPayload, "Bot", "recovered-app[bot]"))
	assert.Equal(t, hubfire.FireStatusRefused, outCommentOwnRecovered.Status)
	assert.Equal(t, "github own app event", outCommentOwnRecovered.Error)
	assert.Equal(t, int64(2), commentPostCount.Load())

	outMsgOwnRecovered := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionMessage, msgPayload, "Bot", "recovered-app[bot]"))
	assert.Equal(t, hubfire.FireStatusFired, outMsgOwnRecovered.Status, outMsgOwnRecovered.Error)

	outDispatchRecovered := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionDispatchAgent, dispatchPayload, "Bot", "dependabot[bot]"))
	assert.Equal(t, hubfire.FireStatusFired, outDispatchRecovered.Status, outDispatchRecovered.Error)

	outCommentRecovered := f.sched.srv.fireAction(ctx, makeFireReq(store.TriggerActionGitHubComment, commentPayload, "Bot", "dependabot[bot]"))
	assert.Equal(t, hubfire.FireStatusFired, outCommentRecovered.Status, outCommentRecovered.Error)
	assert.Equal(t, int64(3), commentPostCount.Load())
}

// TestG4aG4b_AckCommentReuseIsolationAndStaleRefusalOrder verifies the G4a×G4b
// interactions through the real trigger.Dispatcher:
//   - (a) ackComment posts on a dispatch_agent create fire, and does NOT post
//     on a subsequent subject_mode="reuse" wake-message delivery;
//   - (b) stale refusal ("stale event") deterministically takes precedence over
//     the unknown-identity / own-App bot guard for both dispatch_agent and
//     github.comment, and non-stale Bot events with unknown identity still
//     refuse as "github app identity unknown" without posting any comment.
func TestG4aG4b_AckCommentReuseIsolationAndStaleRefusalOrder(t *testing.T) {
	f, gh, _, proj := setupAckCommentFixture(t, "g4a-g4b-reuse-stale")
	ctx := context.Background()
	creatorCtx := f.userAuth(t, f.sched.creator)

	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
	}
	f.sched.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default()))

	// 1. Create a GitHub dispatch_agent trigger with subjectMode="reuse" and ackComment.
	recDisp := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:    "gh-reuse-ack",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:   &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				Task:          "Review PR #${event.attributes.number}",
				ReuseMessage:  "Follow-up on ${event.subject} by ${event.attributes.sender}",
				GithubToken:   DispatchAgentGitHubTokenInstallation,
				AckComment:    &DispatchAgentAckComment{Body: "Started **${result.agent_name}** (${result.agent_id})"},
				UntrustedVars: []string{"event.attributes.number", "event.subject", "event.attributes.sender"},
			},
		})
	require.Equal(t, http.StatusCreated, recDisp.Code, recDisp.Body.String())
	var dispTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recDisp.Body.Bytes(), &dispTrig))

	// 2. Create a GitHub github.comment trigger for stale vs bot-guard ordering.
	recComment := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-comment-stale-order",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.pull_request.synchronize"}},
			Action: TriggerActionSpec{
				Type: store.TriggerActionGitHubComment,
				Body: "Synced PR",
			},
		})
	require.Equal(t, http.StatusCreated, recComment.Code, recComment.Body.String())
	var commentTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recComment.Body.Bytes(), &commentTrig))

	// Also add a dispatch_agent trigger on pull_request.synchronize for step 3/4.
	recDispSync := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-dispatch-stale-order",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.pull_request.synchronize"}},
			Action: TriggerActionSpec{
				Type:        store.TriggerActionDispatchAgent,
				Task:        "Check sync",
				GithubToken: DispatchAgentGitHubTokenInstallation,
				AckComment:  &DispatchAgentAckComment{Body: "Sync started ${result.agent_name}"},
			},
		})
	require.Equal(t, http.StatusCreated, recDispSync.Code, recDispSync.Body.String())
	var dispSyncTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recDispSync.Body.Bytes(), &dispSyncTrig))

	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(f.sched.store, f.sched.store, clock)
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	enqueueAndHandle := func(evType, subject string, attrs map[string]string) string {
		t.Helper()
		evID := uuid.NewString()
		ev := &store.TriggerEvent{
			ID:         evID,
			ProjectID:  proj.ID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "8301",
			Type:       evType,
			Subject:    subject,
			DedupKey:   "dedup-" + evID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: attrs,
			Status:     store.TriggerEventStatusPending,
		}
		require.NoError(t, f.sched.store.CreateTriggerEvent(ctx, ev))
		rawPayload, mErr := json.Marshal(trigger.EventQueuePayload{
			Version: trigger.EventQueuePayloadVersion,
			EventID: ev.ID,
		})
		require.NoError(t, mErr)
		res := disp.HandleItem(ctx, durablequeue.Item{
			Kind:     trigger.EventQueueKind,
			DedupKey: ev.ID,
			Payload:  rawPayload,
			Attempts: 1,
		}, nil)
		require.True(t, res.Done, "HandleItem err=%v", res.Err)
		return ev.ID
	}

	// Step A1: First event creates the agent and posts exactly 1 ackComment.
	ev1ID := enqueueAndHandle("github.issue_comment.created", "github:acme/widgets#42", map[string]string{
		"repo":        "acme/widgets",
		"repo_owner":  "acme",
		"repo_name":   "widgets",
		"number":      "42",
		"sender":      "alice",
		"sender_type": "User",
	})
	fires1, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: ev1ID, TriggerID: dispTrig.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	require.Equal(t, store.TriggerFireStatusFired, fires1.Items[0].Status, fires1.Items[0].Error)
	agentID := fires1.Items[0].TargetAgentID
	require.NotEmpty(t, agentID)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "initial create fire must post 1 ackComment")
	gh.mu.Unlock()

	// Mark the created agent running so the second event takes the reuse wake-message path.
	ag, err := f.sched.store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	ag.Phase = string(state.PhaseRunning)
	ag.ContainerStatus = "running"
	require.NoError(t, f.sched.store.UpdateAgent(ctx, ag))

	// Step A2: Second event on same subject takes subject_mode="reuse" wake-message path.
	// It must deliver a wake message to the existing agent and must NOT post another ackComment.
	ev2ID := enqueueAndHandle("github.issue_comment.created", "github:acme/widgets#42", map[string]string{
		"repo":        "acme/widgets",
		"repo_owner":  "acme",
		"repo_name":   "widgets",
		"number":      "42",
		"sender":      "bob",
		"sender_type": "User",
	})
	fires2, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: ev2ID, TriggerID: dispTrig.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 1)
	require.Equal(t, store.TriggerFireStatusFired, fires2.Items[0].Status, fires2.Items[0].Error)
	assert.Equal(t, agentID, fires2.Items[0].TargetAgentID)

	sentMsgs := tracker.messages()
	require.Len(t, sentMsgs, 1, "reuse delivery must send wake message to claimed agent")
	assert.Equal(t, ag.Slug, sentMsgs[0].AgentID)
	assert.Equal(t, "Follow-up on github:acme/widgets#42 by bob", sentMsgs[0].Message)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "reuse wake-message delivery must NOT post ackComment")
	gh.mu.Unlock()

	// Step A3: Reuse delivery whose reuseMessage template fails to render
	// (renderDispatchReuseMessage -> refuseTemplateRender) refuses the fire
	// without sending a wake message or posting an ackComment.
	ev3ID := enqueueAndHandle("github.issue_comment.created", "github:acme/widgets#42", map[string]string{
		"repo":        "acme/widgets",
		"repo_owner":  "acme",
		"repo_name":   "widgets",
		"number":      "42",
		"sender_type": "User",
		// sender intentionally omitted -> KindValueMissing in reuseMessage
	})
	fires3, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: ev3ID, TriggerID: dispTrig.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires3.Items, 1)
	assert.Equal(t, store.TriggerFireStatusRefused, fires3.Items[0].Status)
	assert.Equal(t, string(triggertemplate.KindValueMissing), fires3.Items[0].Error)
	assert.Len(t, tracker.messages(), 1, "refused reuseMessage render must not send wake message")

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "refused reuseMessage render must NOT post ackComment")
	gh.mu.Unlock()

	// Step B1: Stale + Bot + unknown App identity -> "stale event" deterministically wins
	// over "github app identity unknown" for both dispatch_agent and github.comment.
	// Clear AppSlug and PrivateKey (keeping APIBaseURL host-pinned to gh.srv.URL)
	// so AuthorizeEventSource still passes while GitHubAppBotLogin returns "".
	f.sched.srv.mu.Lock()
	f.sched.srv.config.GitHubAppConfig.AppSlug = ""
	f.sched.srv.config.GitHubAppConfig.PrivateKey = ""
	f.sched.srv.mu.Unlock()
	f.sched.srv.githubAppIdentity.reset()

	evStaleBotID := enqueueAndHandle("github.pull_request.synchronize", "github:acme/widgets#55", map[string]string{
		"repo":        "acme/widgets",
		"repo_owner":  "acme",
		"repo_name":   "widgets",
		"number":      "55",
		"sender":      "dependabot[bot]",
		"sender_type": "Bot",
		"stale":       "true",
	})
	firesStale, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evStaleBotID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesStale.Items, 2)
	byTrig := map[string]store.TriggerFire{}
	for _, fr := range firesStale.Items {
		byTrig[fr.TriggerID] = fr
	}
	assert.Equal(t, store.TriggerFireStatusRefused, byTrig[dispSyncTrig.ID].Status)
	assert.Equal(t, trigger.ErrFireStaleEvent, byTrig[dispSyncTrig.ID].Error)
	assert.Equal(t, store.TriggerFireStatusRefused, byTrig[commentTrig.ID].Status)
	assert.Equal(t, trigger.ErrFireStaleEvent, byTrig[commentTrig.ID].Error)

	// Step B2: Non-stale + Bot + unknown App identity -> "github app identity unknown" refuses both.
	evFreshBotID := enqueueAndHandle("github.pull_request.synchronize", "github:acme/widgets#56", map[string]string{
		"repo":        "acme/widgets",
		"repo_owner":  "acme",
		"repo_name":   "widgets",
		"number":      "56",
		"sender":      "dependabot[bot]",
		"sender_type": "Bot",
	})
	firesFresh, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evFreshBotID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesFresh.Items, 2)
	byTrigFresh := map[string]store.TriggerFire{}
	for _, fr := range firesFresh.Items {
		byTrigFresh[fr.TriggerID] = fr
	}
	assert.Equal(t, store.TriggerFireStatusRefused, byTrigFresh[dispSyncTrig.ID].Status)
	assert.Equal(t, "github app identity unknown", byTrigFresh[dispSyncTrig.ID].Error)
	assert.Equal(t, store.TriggerFireStatusRefused, byTrigFresh[commentTrig.ID].Status)
	assert.Equal(t, "github app identity unknown", byTrigFresh[commentTrig.ID].Error)

	gh.mu.Lock()
	assert.Equal(t, 1, gh.postCount, "refused stale/unknown-identity fires must NOT post any comment")
	gh.mu.Unlock()
}

// TestG4aG4b_RepoRemovalReleasesClaimAndCancelsDeferredFire verifies G4a×G4b
// interaction (c): when a repository is removed from the GitHub App
// installation while a subject_mode="reuse" fire is deferred waiting on an
// in-flight agent launch, repo removal releases the bound claim with
// release_reason="association_lost" and pauses the trigger with
// status_reason="source_deleted", and the subsequent drainer re-handle cleanly
// cancels the deferred fire ("defer cancelled") and marks the event processed
// with nothing stranded.
func TestG4aG4b_RepoRemovalReleasesClaimAndCancelsDeferredFire(t *testing.T) {
	f, _, _, proj := setupAckCommentFixture(t, "g4a-g4b-repo-remove-defer")
	ctx := context.Background()
	creatorCtx := f.userAuth(t, f.sched.creator)

	rec := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:    "gh-reuse-defer-remove",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:   &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:        store.TriggerActionDispatchAgent,
				Task:        "Review PR",
				GithubToken: DispatchAgentGitHubTokenInstallation,
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))

	// Seed an in-flight (PhaseStarting) agent and bind an active subject claim to it.
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(f.sched.store, f.sched.store, clock)

	inFlightAgent := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "launching-pr-agent",
		Name:            "launching-pr-agent",
		ProjectID:       proj.ID,
		OwnerID:         f.sched.creator.ID,
		RuntimeBrokerID: f.sched.broker.ID,
		Phase:           string(state.PhaseStarting),
		LaunchDeadline:  now.Add(5 * time.Minute),
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, inFlightAgent))

	const subject = "github:acme/widgets#77"
	seedClaim, existing, err := claims.Claim(ctx, trigResp.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, existing)
	require.NoError(t, claims.Bind(ctx, seedClaim, inFlightAgent.ID))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	// 1. Incoming event for the claimed in-flight subject defers non-terminally.
	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  proj.ID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "8301",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "dedup-defer-" + uuid.NewString(),
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"repo":        "acme/widgets",
			"repo_owner":  "acme",
			"repo_name":   "widgets",
			"number":      "77",
			"sender":      "alice",
			"sender_type": "User",
		},
		Status: store.TriggerEventStatusPending,
	}
	require.NoError(t, f.sched.store.CreateTriggerEvent(ctx, ev))

	rawPayload, err := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: ev.ID,
	})
	require.NoError(t, err)
	item := durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: ev.ID,
		Payload:  rawPayload,
		Attempts: 1,
	}

	res1 := disp.HandleItem(ctx, item, nil)
	require.False(t, res1.Done, "in-flight launch must defer reuse fire")
	require.False(t, res1.RetryAt.IsZero())

	fires1, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: ev.ID, TriggerID: trigResp.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	require.Equal(t, store.TriggerFireStatusDeferred, fires1.Items[0].Status)

	// 2. Repository is removed from the GitHub App installation.
	f.sched.srv.checkProjectsForRemovedRepos(ctx, 8301, map[string]bool{"acme/widgets": true})

	// Claim must be released (GetActiveTriggerSubjectClaim returns ErrNotFound).
	activeClaim, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subject)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Nil(t, activeClaim, "bound claim must be released on repo removal")

	// Trigger must be paused with source_deleted.
	pausedTrig, err := f.sched.store.GetTrigger(ctx, trigResp.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, pausedTrig.Status)
	assert.Equal(t, store.TriggerStatusReasonSourceDeleted, pausedTrig.StatusReason)

	// 3. Drainer re-handles the deferred item at RetryAt -> cleanly cancels deferred fire and completes event.
	now = res1.RetryAt.Add(time.Second)
	res2 := disp.HandleItem(ctx, item, nil)
	require.True(t, res2.Done, "re-handle after repo removal must complete event, err=%v", res2.Err)

	fires2, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: ev.ID, TriggerID: trigResp.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires2.Items[0].Status)
	assert.Equal(t, trigger.ErrFireDeferCancelled, fires2.Items[0].Error)

	evAfter, err := f.sched.store.GetTriggerEvent(ctx, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, evAfter.Status)
}

// TestG4aG4b_PreFireBotGuardCoversReuseDeferAndCoalesce verifies R1: the
// dispatcher-level PreFireRefuser hook enforces the unknown-identity and
// own-App bot guard before claiming, coalescing, deferral, or subject reuse so
// reuse deliveries, deferred re-handles, and coalescing promotes on
// dispatch_agent triggers are refused, while message-action triggers and
// non-bot User senders remain unaffected.
func TestG4aG4b_PreFireBotGuardCoversReuseDeferAndCoalesce(t *testing.T) {
	f, _, _, proj := setupAckCommentFixture(t, "g4a-g4b-prefire-bot-guard")
	ctx := context.Background()
	creatorCtx := f.userAuth(t, f.sched.creator)

	var createCalls atomic.Int32
	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
		onBeforeCreate: func(_ *RemoteCreateAgentRequest) error {
			createCalls.Add(1)
			return nil
		},
	}
	f.sched.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default()))

	// Target agent for the message-action trigger (must stay unaffected).
	msgTarget := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "prefire-msg-target",
		Name:            "prefire-msg-target",
		ProjectID:       proj.ID,
		OwnerID:         f.sched.creator.ID,
		RuntimeBrokerID: f.sched.broker.ID,
		Phase:           string(state.PhaseRunning),
		ContainerStatus: "running",
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, msgTarget))

	// Create 3 GitHub triggers: reuse dispatch_agent, coalescing dispatch_agent, and message.
	recReuse := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:    "gh-prefire-reuse",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:   &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:         store.TriggerActionDispatchAgent,
				Task:         "Review PR",
				ReuseMessage: "Wake on event",
				GithubToken:  DispatchAgentGitHubTokenInstallation,
			},
		})
	require.Equal(t, http.StatusCreated, recReuse.Code, recReuse.Body.String())
	var reuseTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recReuse.Body.Bytes(), &reuseTrig))

	recCoalesce := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:    "gh-prefire-coalesce",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:   &TriggerMatchSpec{Types: []string{"github.pull_request.synchronize"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeNew, CoalesceWindow: "30s"},
			Action: TriggerActionSpec{
				Type:        store.TriggerActionDispatchAgent,
				Task:        "Check sync",
				GithubToken: DispatchAgentGitHubTokenInstallation,
			},
		})
	require.Equal(t, http.StatusCreated, recCoalesce.Code, recCoalesce.Body.String())
	var coalesceTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recCoalesce.Body.Bytes(), &coalesceTrig))

	recMsg := f.doJSON(t, creatorCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", proj.ID),
		CreateTriggerRequest{
			Name:   "gh-prefire-msg",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: msgTarget.ID,
				Message: "Observe bot event",
			},
		})
	require.Equal(t, http.StatusCreated, recMsg.Code, recMsg.Body.String())
	var msgTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recMsg.Body.Bytes(), &msgTrig))

	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(f.sched.store, f.sched.store, clock)
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	// Seed a running claimed agent for reuse delivery tests.
	liveAgent := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "prefire-reuse-live",
		Name:            "prefire-reuse-live",
		ProjectID:       proj.ID,
		OwnerID:         f.sched.creator.ID,
		RuntimeBrokerID: f.sched.broker.ID,
		Phase:           string(state.PhaseRunning),
		ContainerStatus: "running",
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, liveAgent))
	const liveSubject = "github:acme/widgets#101"
	liveClaim, ex, err := claims.Claim(ctx, reuseTrig.ID, liveSubject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, ex)
	require.NoError(t, claims.Bind(ctx, liveClaim, liveAgent.ID))

	// Seed an in-flight (PhaseStarting) claimed agent for deferred re-handle tests.
	deferAgent := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "prefire-reuse-defer",
		Name:            "prefire-reuse-defer",
		ProjectID:       proj.ID,
		OwnerID:         f.sched.creator.ID,
		RuntimeBrokerID: f.sched.broker.ID,
		Phase:           string(state.PhaseStarting),
		LaunchDeadline:  now.Add(10 * time.Minute),
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, deferAgent))
	const deferSubject = "github:acme/widgets#102"
	deferClaim, ex, err := claims.Claim(ctx, reuseTrig.ID, deferSubject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, ex)
	require.NoError(t, claims.Bind(ctx, deferClaim, deferAgent.ID))

	makeItem := func(evType, subject, senderType, sender string) (string, durablequeue.Item) {
		t.Helper()
		evID := uuid.NewString()
		ev := &store.TriggerEvent{
			ID:         evID,
			ProjectID:  proj.ID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "8301",
			Type:       evType,
			Subject:    subject,
			DedupKey:   "dedup-r1-" + evID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{
				"repo":        "acme/widgets",
				"repo_owner":  "acme",
				"repo_name":   "widgets",
				"number":      "101",
				"sender":      sender,
				"sender_type": senderType,
			},
			Status: store.TriggerEventStatusPending,
		}
		require.NoError(t, f.sched.store.CreateTriggerEvent(ctx, ev))
		rawPayload, mErr := json.Marshal(trigger.EventQueuePayload{
			Version: trigger.EventQueuePayloadVersion,
			EventID: ev.ID,
		})
		require.NoError(t, mErr)
		return ev.ID, durablequeue.Item{
			Kind:     trigger.EventQueueKind,
			DedupKey: ev.ID,
			Payload:  rawPayload,
			Attempts: 1,
		}
	}

	origPrivKey := f.sched.srv.config.GitHubAppConfig.PrivateKey

	modes := []struct {
		name       string
		sender     string
		wantReason string
		// allowSetup sets App identity so the event can initially enter deferred/coalescing.
		allowSetup func()
		// blockSetup sets App identity so the guard must refuse the event.
		blockSetup func()
	}{
		{
			name:       "unknown_identity",
			sender:     "dependabot[bot]",
			wantReason: "github app identity unknown",
			allowSetup: func() {
				f.sched.srv.mu.Lock()
				f.sched.srv.config.GitHubAppConfig.AppSlug = "scion-bot"
				f.sched.srv.config.GitHubAppConfig.PrivateKey = origPrivKey
				f.sched.srv.mu.Unlock()
				f.sched.srv.githubAppIdentity.reset()
			},
			blockSetup: func() {
				f.sched.srv.mu.Lock()
				f.sched.srv.config.GitHubAppConfig.AppSlug = ""
				f.sched.srv.config.GitHubAppConfig.PrivateKey = ""
				f.sched.srv.mu.Unlock()
				f.sched.srv.githubAppIdentity.reset()
			},
		},
		{
			name:       "own_app_login",
			sender:     "scion-bot[bot]",
			wantReason: "github own app event",
			allowSetup: func() {
				f.sched.srv.mu.Lock()
				f.sched.srv.config.GitHubAppConfig.AppSlug = "other-app"
				f.sched.srv.config.GitHubAppConfig.PrivateKey = origPrivKey
				f.sched.srv.mu.Unlock()
				f.sched.srv.githubAppIdentity.reset()
			},
			blockSetup: func() {
				f.sched.srv.mu.Lock()
				f.sched.srv.config.GitHubAppConfig.AppSlug = "scion-bot"
				f.sched.srv.config.GitHubAppConfig.PrivateKey = origPrivKey
				f.sched.srv.mu.Unlock()
				f.sched.srv.githubAppIdentity.reset()
			},
		},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			// 1. Reuse delivery to live claimed agent -> refused, no wake message;
			//    message-action trigger on same event -> still delivered.
			tc.blockSetup()
			msgsBefore := len(tracker.messages())
			evReuseID, itReuse := makeItem("github.issue_comment.created", liveSubject, "Bot", tc.sender)
			resReuse := disp.HandleItem(ctx, itReuse, nil)
			require.True(t, resReuse.Done, "reuse HandleItem err=%v", resReuse.Err)

			firesReuse, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evReuseID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			byTrig := map[string]store.TriggerFire{}
			for _, fr := range firesReuse.Items {
				byTrig[fr.TriggerID] = fr
			}
			assert.Equal(t, store.TriggerFireStatusRefused, byTrig[reuseTrig.ID].Status)
			assert.Equal(t, tc.wantReason, byTrig[reuseTrig.ID].Error)
			assert.Equal(t, store.TriggerFireStatusFired, byTrig[msgTrig.ID].Status)

			// Only the message-action trigger sent a message; liveAgent got NO wake message.
			newMsgs := tracker.messages()[msgsBefore:]
			require.Len(t, newMsgs, 1)
			assert.Equal(t, msgTarget.Slug, newMsgs[0].AgentID)

			// 2. Deferred re-handle -> transitions deferred fire to refused.
			tc.allowSetup()
			evDeferID, itDefer := makeItem("github.issue_comment.created", deferSubject, "Bot", tc.sender)
			resDefer1 := disp.HandleItem(ctx, itDefer, nil)
			require.False(t, resDefer1.Done, "in-flight agent must defer fire")
			require.False(t, resDefer1.RetryAt.IsZero())

			tc.blockSetup()
			now = resDefer1.RetryAt.Add(time.Second)
			resDefer2 := disp.HandleItem(ctx, itDefer, nil)
			require.True(t, resDefer2.Done, "deferred re-handle must complete, err=%v", resDefer2.Err)

			firesDefer, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evDeferID, TriggerID: reuseTrig.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, firesDefer.Items, 1)
			assert.Equal(t, store.TriggerFireStatusRefused, firesDefer.Items[0].Status)
			assert.Equal(t, tc.wantReason, firesDefer.Items[0].Error)

			// 3. Coalescing promote -> transitions coalescing fire to refused without creating an agent.
			tc.allowSetup()
			createsBefore := createCalls.Load()
			evCoalID, itCoal := makeItem("github.pull_request.synchronize", "github:acme/widgets#"+tc.name, "Bot", tc.sender)
			resCoal1 := disp.HandleItem(ctx, itCoal, nil)
			require.False(t, resCoal1.Done, "coalesce window must hold fire")
			require.False(t, resCoal1.RetryAt.IsZero())

			tc.blockSetup()
			now = resCoal1.RetryAt.Add(time.Second)
			resCoal2 := disp.HandleItem(ctx, itCoal, nil)
			require.True(t, resCoal2.Done, "coalescing promote must complete, err=%v", resCoal2.Err)

			firesCoal, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evCoalID, TriggerID: coalesceTrig.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, firesCoal.Items, 1)
			assert.Equal(t, store.TriggerFireStatusRefused, firesCoal.Items[0].Status)
			assert.Equal(t, tc.wantReason, firesCoal.Items[0].Error)
			assert.Equal(t, createsBefore, createCalls.Load(), "refused coalescing promote must not create agent")

			// 4. Non-bot User sender -> unaffected (reuse wake message delivered normally).
			msgsBeforeUser := len(tracker.messages())
			evUserID, itUser := makeItem("github.issue_comment.created", liveSubject, "User", "alice")
			resUser := disp.HandleItem(ctx, itUser, nil)
			require.True(t, resUser.Done, "user HandleItem err=%v", resUser.Err)

			firesUser, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evUserID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			byTrigUser := map[string]store.TriggerFire{}
			for _, fr := range firesUser.Items {
				byTrigUser[fr.TriggerID] = fr
			}
			assert.Equal(t, store.TriggerFireStatusFired, byTrigUser[reuseTrig.ID].Status, byTrigUser[reuseTrig.ID].Error)
			assert.Equal(t, store.TriggerFireStatusFired, byTrigUser[msgTrig.ID].Status, byTrigUser[msgTrig.ID].Error)
			assert.Len(t, tracker.messages()[msgsBeforeUser:], 2, "both reuse wake and message trigger must fire for User sender")
		})
	}
}
