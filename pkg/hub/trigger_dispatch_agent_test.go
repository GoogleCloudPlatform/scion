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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type triggerDispatchAgentFixture struct {
	sched  *schedFire
	client *headlessGatherBrokerClient
}

func newTriggerDispatchAgentFixture(t *testing.T, tag string) *triggerDispatchAgentFixture {
	t.Helper()
	f := newSchedFire(t, tag)
	client := installHeadlessGatherDispatcher(t, f)
	enableTriggersExperimentForTest(t, f.srv, true)

	// Advertise HomeFiles capability on the fake Runtime Broker.
	ctx := context.Background()
	broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	broker.Capabilities = &store.BrokerCapabilities{HomeFiles: true, GitHubTokenInstallationOnly: true}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, broker))

	return &triggerDispatchAgentFixture{
		sched:  f,
		client: client,
	}
}

func (f *triggerDispatchAgentFixture) doJSON(t *testing.T, authCtx context.Context, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if authCtx != nil {
		req = req.WithContext(authCtx)
	}
	rec := httptest.NewRecorder()
	f.sched.srv.mux.ServeHTTP(rec, req)
	return rec
}

func (f *triggerDispatchAgentFixture) userAuth(t *testing.T, u *store.User) context.Context {
	t.Helper()
	ctx := context.Background()
	ident := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")
	ctx = contextWithIdentity(ctx, ident)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(ident))
	return ctx
}

func (f *triggerDispatchAgentFixture) agentAuth(t *testing.T, ag *store.Agent, role AgentRole) context.Context {
	t.Helper()
	ctx := context.Background()
	scopes := ScopesForRole(role)
	ident := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ag.ID},
		ProjectID: ag.ProjectID,
		Scopes:    scopes,
	}}
	ctx = contextWithIdentity(ctx, ident)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindAgentJWT})
	return ctx
}

func (f *triggerDispatchAgentFixture) createEndpoint(t *testing.T, authCtx context.Context, name string) WebhookEndpointCreatedResponse {
	t.Helper()
	rec := f.doJSON(t, authCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints", f.sched.proj.ID),
		map[string]any{"name": name})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (f *triggerDispatchAgentFixture) postSignedWebhook(t *testing.T, ep WebhookEndpointCreatedResponse, delivery, eventHeader, subject string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	now := time.Now().UTC().Unix()
	sig := trigger.SignScionV1(ep.Secret, now, delivery, eventHeader, subject, body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(trigger.HeaderSignature, sig)
	if delivery != "" {
		req.Header.Set(trigger.HeaderDelivery, delivery)
	}
	if eventHeader != "" {
		req.Header.Set(trigger.HeaderEvent, eventHeader)
	}
	if subject != "" {
		req.Header.Set(trigger.HeaderSubject, subject)
	}
	rec := httptest.NewRecorder()
	f.sched.srv.mux.ServeHTTP(rec, req)
	return rec
}

func (f *triggerDispatchAgentFixture) drainOneTriggerEvent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: trigger.NewSubjectClaims(f.sched.store, f.sched.store, nil),
		Rules:         f.sched.srv.TriggerRules(),
	})
	require.NoError(t, err)

	dqStore, ok := f.sched.store.(store.DurableQueueStore)
	require.True(t, ok, "store must implement DurableQueueStore")
	now := time.Now().UTC()
	rows, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "test-owner", now, now.Add(time.Minute), 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "expected at least one queued trigger.event item")
	for _, r := range rows {
		it := durablequeue.Item{
			Seq:       r.Seq,
			Kind:      r.Kind,
			DedupKey:  r.DedupKey,
			ProjectID: r.ProjectID,
			Payload:   []byte(r.Payload),
			Attempts:  r.Attempts,
		}
		res := disp.HandleItem(ctx, it, nil)
		require.True(t, res.Done, "expected HandleItem Done=true, got err=%v", res.Err)
		completed, cErr := dqStore.CompleteDurableItem(ctx, r.Seq, "test-owner")
		require.NoError(t, cErr)
		require.True(t, completed)
	}
}

func TestTriggerDispatchAgent_EndToEndWebhookToAgent(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "e2e-dispatch")
	userCtx := f.userAuth(t, f.sched.creator)

	ep := f.createEndpoint(t, userCtx, "ci-hooks")

	// Author a Types-only dispatch_agent trigger with templated task, name, branch, labels, and reuseMessage.
	createReq := CreateTriggerRequest{
		Name:   "pr-review",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.pr_opened"}},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionDispatchAgent,
			AgentName:     "pr-${event.payload.number}-review",
			Task:          "Review PR #${event.payload.number}: ${event.payload.title} (trigger=${trigger.name}, project=${project.name})",
			Branch:        "refs/heads/pr/${event.payload.branch}",
			Labels:        map[string]string{"pr": "${event.payload.number}", "env": "ci"},
			ReuseMessage:  "Follow-up on ${event.payload.number}",
			UntrustedVars: []string{"event.payload.*"},
		},
	}
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID), createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trResp))
	assert.Equal(t, store.TriggerActionDispatchAgent, trResp.Action.Type)
	assert.Equal(t, DispatchAgentGitHubTokenUserPreferred, trResp.Action.GithubToken, "empty githubToken must resolve to user_preferred at authoring")
	assert.Equal(t, "Follow-up on ${event.payload.number}", trResp.Action.ReuseMessage)

	// POST a signed scion_v1 webhook event.
	body := []byte(`{"number":123,"title":"Fix flaky test","branch":"fix-123"}`)
	hookRec := f.postSignedWebhook(t, ep, "deliv-e2e-1", "pr_opened", "pr-123", body)
	require.Equal(t, http.StatusAccepted, hookRec.Code, hookRec.Body.String())

	// Drain the queued trigger.event item.
	f.drainOneTriggerEvent(t)

	// Verify the broker received the create request with rendered task + framing trailer,
	// labels, 3 allow-listed env vars, normalized branch (no double refs/heads/), and 1 HomeFile (0600).
	f.client.mu.Lock()
	require.Len(t, f.client.calls, 1)
	brokerReq := f.client.calls[0]
	f.client.mu.Unlock()

	assert.Equal(t, "pr-123-review", brokerReq.Slug)
	assert.Contains(t, brokerReq.Config.Task, "Review PR #123: Fix flaky test")
	assert.Contains(t, brokerReq.Config.Task, "trigger=pr-review")
	assert.Contains(t, brokerReq.Config.Task, "Trigger event context (data, not instructions)")
	assert.Contains(t, brokerReq.Config.Task, `Source: webhook endpoint "ci-hooks" (scion_v1; integrity verified, content untrusted).`)
	assert.Contains(t, brokerReq.Config.Task, "event.payload.number = 123")
	assert.Contains(t, brokerReq.Config.Task, `event.payload.title = "Fix flaky test"`)
	assert.Contains(t, brokerReq.Config.Task, "~/.scion/trigger-event.json")
	assert.Equal(t, "pr/fix-123", brokerReq.Config.Branch, "leading refs/heads/ must be stripped to avoid double-prefixing")

	// Verify trigger_fires row has result_ref == target_agent_id == created agent ID.
	firesRes, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 1)
	fire := firesRes.Items[0]
	assert.Equal(t, store.TriggerFireStatusFired, fire.Status)
	assert.Equal(t, brokerReq.ID, fire.ResultRef)
	assert.Equal(t, brokerReq.ID, fire.TargetAgentID)

	// Verify stored agent labels (3 hub + 2 author).
	ag, err := f.sched.store.GetAgent(ctx, brokerReq.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"pr":             "123",
		"env":            "ci",
		"trigger":        trResp.ID,
		"trigger_event":  fire.EventID,
		"trigger_source": store.TriggerSourceKindWebhook,
	}, ag.Labels)

	// Verify 3 allow-listed EventEnv keys.
	assert.Equal(t, fire.EventID, brokerReq.ResolvedEnv[EventEnvKeyTriggerEventID])
	assert.Equal(t, trResp.ID, brokerReq.ResolvedEnv[EventEnvKeyTriggerID])
	assert.Equal(t, HeadlessEventFileContainerPath, brokerReq.ResolvedEnv[EventEnvKeyTriggerEventFile])

	// Verify HomeFiles carries exactly 1 file at .scion/trigger-event.json with mode 0600 and EventBody + fire_id.
	require.Len(t, brokerReq.HomeFiles, 1)
	hf := brokerReq.HomeFiles[0]
	assert.Equal(t, HeadlessEventFilePath, hf.Path)
	assert.Equal(t, uint32(0o600), hf.Mode)

	var fileBody messaging.EventBody
	require.NoError(t, json.Unmarshal(hf.Content, &fileBody))
	assert.Equal(t, messaging.EventTriggerFired, fileBody.Type)
	assert.Equal(t, "webhook.pr_opened", fileBody.EventType)
	assert.Equal(t, fire.EventID, fileBody.EventID)
	assert.Equal(t, fire.ID, fileBody.FireID)
	require.NotNil(t, fileBody.Source)
	assert.Equal(t, store.TriggerSourceKindWebhook, fileBody.Source.Kind)
	assert.Equal(t, ep.ID, fileBody.Source.ID)
	assert.Equal(t, "ci-hooks", fileBody.Source.Name)
	require.NotNil(t, fileBody.Trigger)
	assert.Equal(t, trResp.ID, fileBody.Trigger.ID)
	assert.Equal(t, "pr-review", fileBody.Trigger.Name)
	require.NotNil(t, fileBody.External)
	assert.Equal(t, messaging.ExternalVerifiedScionV1, fileBody.External.Verified)
	assert.JSONEq(t, string(body), string(fileBody.External.JSON))

	// Verify the created agent identity can read and list its own trigger event (§5.4).
	dispatchedAgentCtx := f.agentAuth(t, ag, AgentRoleFull)
	getEvRec := f.doJSON(t, dispatchedAgentCtx, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/trigger-events/%s", f.sched.proj.ID, fire.EventID), nil)
	require.Equal(t, http.StatusOK, getEvRec.Code, getEvRec.Body.String())

	listEvRec := f.doJSON(t, dispatchedAgentCtx, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/trigger-events", f.sched.proj.ID), nil)
	require.Equal(t, http.StatusOK, listEvRec.Code, listEvRec.Body.String())
	var listResp ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal(listEvRec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Events, 1)
	assert.Equal(t, fire.EventID, listResp.Events[0].ID)

	// Unrelated agent in the same project cannot read the event (404).
	otherAg := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "other-agent",
		Name:      "other-agent",
		ProjectID: f.sched.proj.ID,
		Phase:     "running",
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, otherAg))
	otherCtx := f.agentAuth(t, otherAg, AgentRoleFull)
	otherRec := f.doJSON(t, otherCtx, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/trigger-events/%s", f.sched.proj.ID, fire.EventID), nil)
	assert.Equal(t, http.StatusNotFound, otherRec.Code)
}

func TestTriggerDispatchAgent_AuthoringValidationAndAuthz(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "authz-val")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "val-ep")

	// Also seed a non-replay-protected body_only endpoint directly in the store.
	bodyOnlyEP := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.sched.proj.ID,
		Name:                "body-only-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.sched.creator.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeBodyOnly,
		Status:              store.WebhookEndpointStatusActive,
		CreatedAt:           time.Now().UTC(),
		UpdatedAt:           time.Now().UTC(),
	}
	require.NoError(t, f.sched.store.CreateWebhookEndpoint(ctx, bodyOnlyEP))

	postTrigger := func(authCtx context.Context, req CreateTriggerRequest) *httptest.ResponseRecorder {
		return f.doJSON(t, authCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID), req)
	}

	assertErrCode := func(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) map[string]any {
		t.Helper()
		require.Equal(t, wantStatus, rec.Code, rec.Body.String())
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		if wantCode != "" {
			assert.Equal(t, wantCode, env.Error.Code, rec.Body.String())
		}
		return env.Error.Details
	}

	baseReq := func(name string) CreateTriggerRequest {
		return CreateTriggerRequest{
			Name:   name,
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "Handle ${event.id}",
			},
		}
	}

	t.Run("template_with_placeholder_refused", func(t *testing.T) {
		req := baseReq("tmpl-ph")
		req.Action.Template = "${event.type}"
		details := assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "invalid_template")
		assert.Equal(t, "template", details["slot"])
	})

	t.Run("invalid_template_syntax_and_post_phase", func(t *testing.T) {
		req := baseReq("bad-syntax")
		req.Action.Task = "hello ${event.id"
		details := assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "invalid_template")
		assert.Equal(t, "task", details["slot"])
		assert.Equal(t, float64(6), details["offset"])
		assert.NotEmpty(t, details["message"])

		req2 := baseReq("post-phase")
		req2.Action.Task = "agent ${result.agent_id}"
		details2 := assertErrCode(t, postTrigger(userCtx, req2), http.StatusBadRequest, "invalid_template")
		assert.Equal(t, "task", details2["slot"])
	})

	t.Run("untrusted_var_not_allowed", func(t *testing.T) {
		req := baseReq("untrusted-uncovered")
		req.Action.Task = "PR ${event.payload.title}"
		details := assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "untrusted_var_not_allowed")
		assert.Equal(t, "task", details["slot"])
		assert.Equal(t, "event.payload.title", details["path"])
	})

	t.Run("untrusted_vars_authoring_validation_dedup_and_cap", func(t *testing.T) {
		for _, bad := range []string{
			"",
			"event.id",
			"trigger.name",
			"project.id",
			"result.agent_id",
			"foo.bar",
			"event.unknown",
			"event.*",
			"*",
			"event.type.*",
			"event.payload.*.x",
		} {
			req := baseReq("bad-uv-" + api.Slugify(bad))
			req.Action.UntrustedVars = []string{bad}
			details := assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "invalid_untrusted_var")
			assert.Equal(t, bad, details["path"])
		}

		// More than 64 entries refused.
		tooMany := make([]string, 65)
		for i := range tooMany {
			tooMany[i] = fmt.Sprintf("event.attributes.k%d", i)
		}
		reqCap := baseReq("uv-over-cap")
		reqCap.Action.UntrustedVars = tooMany
		assertErrCode(t, postTrigger(userCtx, reqCap), http.StatusBadRequest, "invalid_untrusted_var")

		// Valid entries + duplicates deduplicated in order.
		reqOK := baseReq("uv-dedup-ok")
		reqOK.Action.UntrustedVars = []string{
			"event.type",
			"event.attributes.*",
			"event.payload.pr.*",
			"event.type",
			"event.payload.pr.*",
			"event.payload.title",
		}
		rec := postTrigger(userCtx, reqOK)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var got TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, []string{
			"event.type",
			"event.attributes.*",
			"event.payload.pr.*",
			"event.payload.title",
		}, got.Action.UntrustedVars)
	})

	t.Run("reserved_label_keys_and_label_count", func(t *testing.T) {
		for _, k := range []string{"trigger", "trigger_event", "trigger_source", githubTokenSourceProjectLabel} {
			req := baseReq("res-lbl-" + api.Slugify(k))
			req.Action.Labels = map[string]string{k: "val"}
			assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "reserved_label_key")
		}
		tooMany := make(map[string]string, labels.MaxLabels-2)
		for i := range labels.MaxLabels - 2 {
			tooMany[fmt.Sprintf("k%d", i)] = "v"
		}
		req := baseReq("too-many-labels")
		req.Action.Labels = tooMany
		assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, ErrCodeValidationError)
	})

	t.Run("ephemeral_unsupported", func(t *testing.T) {
		req := baseReq("ephem")
		req.Action.Ephemeral = true
		assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "ephemeral_unsupported")
	})

	t.Run("source_not_replay_protected", func(t *testing.T) {
		req := baseReq("no-replay")
		req.Source.ID = bodyOnlyEP.ID
		assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "source_not_replay_protected")
	})

	t.Run("github_token_validation", func(t *testing.T) {
		req := baseReq("gh-inst")
		req.Action.GithubToken = DispatchAgentGitHubTokenInstallation
		rec := postTrigger(userCtx, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.Equal(t, DispatchAgentGitHubTokenInstallation, created.Action.GithubToken)

		reqBad := baseReq("gh-bad")
		reqBad.Action.GithubToken = "magic"
		assertErrCode(t, postTrigger(userCtx, reqBad), http.StatusBadRequest, "invalid_value")
	})

	t.Run("task_exceeds_16kib_before_render", func(t *testing.T) {
		req := baseReq("huge-task")
		req.Action.Task = strings.Repeat("a", 16*1024+1)
		assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, ErrCodeValidationError)
	})

	t.Run("reuse_message_parsed_in_message_slot", func(t *testing.T) {
		req := baseReq("bad-reuse")
		req.Action.ReuseMessage = "hi ${unknown.var}"
		details := assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "invalid_template")
		assert.Equal(t, "message", details["slot"])
	})

	t.Run("subject_mode_skip_allowed_for_dispatch_agent", func(t *testing.T) {
		// dispatch_agent + subject.mode=skip is accepted (P1c-2 subject claims).
		reqSkip := baseReq("skip-mode")
		reqSkip.Subject = &TriggerSubjectSpec{Mode: store.TriggerSubjectModeSkip}
		recSkip := postTrigger(userCtx, reqSkip)
		require.Equal(t, http.StatusCreated, recSkip.Code, recSkip.Body.String())
		var resp TriggerResponse
		require.NoError(t, json.Unmarshal(recSkip.Body.Bytes(), &resp))
		assert.Equal(t, store.TriggerSubjectModeSkip, resp.Subject.Mode)

		// dispatch_agent + subject.mode=reuse is accepted in Stage 3 (when the author holds wake authority).
		reqReuse := baseReq("reuse-mode")
		reqReuse.Subject = &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse}
		recReuse := postTrigger(userCtx, reqReuse)
		require.Equal(t, http.StatusCreated, recReuse.Code, recReuse.Body.String())
		var reuseResp TriggerResponse
		require.NoError(t, json.Unmarshal(recReuse.Body.Bytes(), &reuseResp))
		assert.Equal(t, store.TriggerSubjectModeReuse, reuseResp.Subject.Mode)

		// message trigger + subject.mode=skip (Stage 1 PR 4) remains accepted.
		msgTarget := &store.Agent{
			ID:        api.NewUUID(),
			Slug:      "skip-msg-target",
			Name:      "skip-msg-target",
			ProjectID: f.sched.proj.ID,
			Phase:     "running",
			CreatedBy: f.sched.creator.ID,
			OwnerID:   f.sched.creator.ID,
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, msgTarget))
		recMsgSkip := postTrigger(userCtx, CreateTriggerRequest{
			Name:    "msg-skip-ok",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeSkip},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: msgTarget.ID,
				Message: "hi",
			},
		})
		require.Equal(t, http.StatusCreated, recMsgSkip.Code, recMsgSkip.Body.String())
	})

	t.Run("literal_agent_name_create_path_checks_at_authoring", func(t *testing.T) {
		for _, bad := range []string{"admin", "hub", "UPPER", "bad name!"} {
			req := baseReq("lit-name-" + api.Slugify(bad))
			req.Action.AgentName = bad
			details := assertErrCode(t, postTrigger(userCtx, req), http.StatusBadRequest, "invalid_value")
			assert.Equal(t, "agentName", details["field"])
		}
		// Valid canonical literal succeeds.
		reqOK := baseReq("lit-name-ok")
		reqOK.Action.AgentName = "valid-worker-1"
		rec := postTrigger(userCtx, reqOK)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	t.Run("agent_role_invalid_and_authz_baseline_vs_full_vs_member", func(t *testing.T) {
		reqBadRole := baseReq("bad-role")
		reqBadRole.Action.AgentRole = "superadmin"
		assertErrCode(t, postTrigger(userCtx, reqBadRole), http.StatusBadRequest, "invalid_value")

		// Seed a baseline agent (owns its own endpoint) -> refused 403 for dispatch_agent.
		baselineAg := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "baseline-author",
			Name:          "baseline-author",
			ProjectID:     f.sched.proj.ID,
			Phase:         "running",
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleBaseline)},
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, baselineAg))
		require.NoError(t, f.sched.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
			ID:            api.NewUUID(),
			DelegatorType: store.DelegationPrincipalUser,
			DelegatorID:   f.sched.creator.ID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    baselineAg.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       f.sched.proj.ID,
			Role:          string(AgentRoleBaseline),
			Active:        true,
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
		baselineCtx := f.agentAuth(t, baselineAg, AgentRoleBaseline)
		baseEP := f.createEndpoint(t, baselineCtx, "baseline-ep")
		reqBase := baseReq("from-baseline")
		reqBase.Source.ID = baseEP.ID
		assertErrCode(t, postTrigger(baselineCtx, reqBase), http.StatusForbidden, ErrCodeForbidden)

		// Seed a full agent with full ceiling -> allowed 201!
		fullAg := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "full-author",
			Name:          "full-author",
			ProjectID:     f.sched.proj.ID,
			Phase:         "running",
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, fullAg))
		require.NoError(t, f.sched.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
			ID:            api.NewUUID(),
			DelegatorType: store.DelegationPrincipalUser,
			DelegatorID:   f.sched.creator.ID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    fullAg.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       f.sched.proj.ID,
			Role:          string(AgentRoleFull),
			Active:        true,
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
		fullCtx := f.agentAuth(t, fullAg, AgentRoleFull)
		fullEP := f.createEndpoint(t, fullCtx, "full-ep")
		reqFull := baseReq("from-full")
		reqFull.Source.ID = fullEP.ID
		recFull := postTrigger(fullCtx, reqFull)
		require.Equal(t, http.StatusCreated, recFull.Code, recFull.Body.String())
	})
}

func TestTriggerDispatchAgent_RenderRefusalAndCollisionMatrix(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "render-matrix")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "matrix-ep")

	fireDirect := func(t *testing.T, triggerName string, payload DispatchAgentEventPayload, ev *hubfire.FireEvent) hubfire.FireOutcome {
		t.Helper()
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		return f.sched.srv.fireAction(ctx, hubfire.FireRequest{
			ProjectID: f.sched.proj.ID,
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
				DefinitionID:   api.NewUUID(),
				FireID:         api.NewUUID(),
			},
			ActionType: store.TriggerActionDispatchAgent,
			Payload:    string(raw),
			Event:      ev,
			FireAt:     time.Now().UTC(),
		})
	}

	baseEvent := func(body string, attrs map[string]string) *hubfire.FireEvent {
		now := time.Now().UTC()
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs["endpoint_name"] = ep.Name
		attrs["signature_mode"] = store.WebhookSignatureModeScionV1
		return &hubfire.FireEvent{
			ID:         api.NewUUID(),
			ProjectID:  f.sched.proj.ID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    "webhook:" + ep.ID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: attrs,
			Payload:    []byte(body),
			Trigger:    &hubfire.FireTriggerRef{ID: api.NewUUID(), Name: "matrix-trig"},
		}
	}

	t.Run("missing_required_var_refused", func(t *testing.T) {
		out := fireDirect(t, "missing", DispatchAgentEventPayload{
			Task:          "Check ${event.payload.missing}",
			UntrustedVars: []string{"event.payload.*"},
		}, baseEvent(`{"other":1}`, nil))
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, "template value missing", out.Error)
	})

	t.Run("non_scalar_payload_rejected", func(t *testing.T) {
		out := fireDirect(t, "non-scalar", DispatchAgentEventPayload{
			Task:          "Check ${event.payload.obj}",
			UntrustedVars: []string{"event.payload.*"},
		}, baseEvent(`{"obj":{"a":1}}`, nil))
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, "template value rejected", out.Error)
	})

	t.Run("name_slot_reserved_word_rejected", func(t *testing.T) {
		for _, reserved := range []string{"admin", "hub"} {
			out := fireDirect(t, "reserved-"+reserved, DispatchAgentEventPayload{
				AgentName:     "${event.payload.name}",
				Task:          "hello",
				UntrustedVars: []string{"event.payload.*"},
			}, baseEvent(fmt.Sprintf(`{"name":%q}`, reserved), nil))
			assert.Equal(t, hubfire.FireStatusRefused, out.Status)
			assert.Equal(t, "template value rejected", out.Error)
		}
	})

	t.Run("branch_slot_unsafe_values_rejected", func(t *testing.T) {
		for _, bad := range []string{"--upload-pack=x", "../x", "@{-1}"} {
			out := fireDirect(t, "bad-branch", DispatchAgentEventPayload{
				Branch:        "${event.payload.branch}",
				Task:          "hello",
				UntrustedVars: []string{"event.payload.*"},
			}, baseEvent(fmt.Sprintf(`{"branch":%q}`, bad), nil))
			assert.Equal(t, hubfire.FireStatusRefused, out.Status)
			assert.Equal(t, "template value rejected", out.Error)
		}
	})

	t.Run("label_slot_control_char_rejected", func(t *testing.T) {
		out := fireDirect(t, "bad-label", DispatchAgentEventPayload{
			Task:          "hello",
			Labels:        map[string]string{"k": "${event.payload.v}"},
			UntrustedVars: []string{"event.payload.*"},
		}, baseEvent(`{"v":"bad\nvalue"}`, nil))
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, "template value rejected", out.Error)
	})

	t.Run("oversized_task_including_trailer_rejected", func(t *testing.T) {
		// Task itself is under 16 KiB, but adding the framing trailer pushes it over 16 KiB.
		out := fireDirect(t, "oversized-trailer", DispatchAgentEventPayload{
			Task:          strings.Repeat("a", 16300) + "${event.payload.x}",
			UntrustedVars: []string{"event.payload.*"},
		}, baseEvent(`{"x":"1"}`, nil))
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, "template value rejected", out.Error)
	})

	t.Run("fire_time_non_replay_protected_source_refused", func(t *testing.T) {
		bodyOnlyEP := &store.WebhookEndpoint{
			ID:                  api.NewUUID(),
			ProjectID:           f.sched.proj.ID,
			Name:                "fire-body-only",
			OwnerPrincipalKind:  store.DelegationPrincipalUser,
			OwnerPrincipalID:    f.sched.creator.ID,
			OwnerCredentialKind: store.InitiatorCredentialKindSession,
			SecretVersion:       1,
			SignatureMode:       store.WebhookSignatureModeBodyOnly,
			Status:              store.WebhookEndpointStatusActive,
			CreatedAt:           time.Now().UTC(),
			UpdatedAt:           time.Now().UTC(),
		}
		require.NoError(t, f.sched.store.CreateWebhookEndpoint(ctx, bodyOnlyEP))
		ev := baseEvent(`{}`, nil)
		ev.SourceID = bodyOnlyEP.ID
		ev.Attributes["signature_mode"] = store.WebhookSignatureModeBodyOnly
		out := fireDirect(t, "fire-no-replay", DispatchAgentEventPayload{
			Task: "hello",
		}, ev)
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, "source not replay protected", out.Error)
	})

	t.Run("name_collision_suffix_and_skip_and_default_name", func(t *testing.T) {
		// Default name when agentName is omitted: "<trigger-slug>-<6 base32>".
		outDefault := fireDirect(t, "default-name", DispatchAgentEventPayload{
			Task: "hello default",
		}, baseEvent(`{}`, nil))
		require.Equal(t, hubfire.FireStatusFired, outDefault.Status)
		agDef, err := f.sched.store.GetAgent(ctx, outDefault.ResultRef)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(agDef.Slug, "matrix-trig-"), "got slug %q", agDef.Slug)
		assert.Equal(t, len("matrix-trig-")+6, len(agDef.Slug))

		// First create with explicit name "coll-worker" succeeds.
		out1 := fireDirect(t, "coll-1", DispatchAgentEventPayload{
			AgentName: "coll-worker",
			Task:      "first",
		}, baseEvent(`{}`, nil))
		require.Equal(t, hubfire.FireStatusFired, out1.Status)

		// Second create with nameConflict=suffix (default) appends -<4 hex> and succeeds.
		out2 := fireDirect(t, "coll-2", DispatchAgentEventPayload{
			AgentName: "coll-worker",
			Task:      "second",
		}, baseEvent(`{}`, nil))
		require.Equal(t, hubfire.FireStatusFired, out2.Status)
		ag2, err := f.sched.store.GetAgent(ctx, out2.ResultRef)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(ag2.Slug, "coll-worker-"), "got slug %q", ag2.Slug)
		assert.Equal(t, len("coll-worker-")+4, len(ag2.Slug))

		// Third create with nameConflict=skip returns skipped "agent name in use".
		out3 := fireDirect(t, "coll-3", DispatchAgentEventPayload{
			AgentName:    "coll-worker",
			NameConflict: DispatchAgentNameConflictSkip,
			Task:         "third",
		}, baseEvent(`{}`, nil))
		assert.Equal(t, hubfire.FireStatusSkipped, out3.Status)
		assert.Equal(t, "agent name in use", out3.Error)
	})

	t.Run("name_collision_with_error_and_non_running_phase_holders", func(t *testing.T) {
		for _, phase := range []string{"error", "stopped", "suspended"} {
			slug := "holder-" + phase
			require.NoError(t, f.sched.store.CreateAgent(ctx, &store.Agent{
				ID:        api.NewUUID(),
				Slug:      slug,
				Name:      slug,
				ProjectID: f.sched.proj.ID,
				Phase:     phase,
			}))

			// nameConflict=suffix (default) must suffix and fire, never fail as schedule-blocked.
			outSuffix := fireDirect(t, "coll-"+phase+"-suffix", DispatchAgentEventPayload{
				AgentName: slug,
				Task:      "suffix on " + phase,
			}, baseEvent(`{}`, nil))
			require.Equal(t, hubfire.FireStatusFired, outSuffix.Status, "phase=%s err=%q", phase, outSuffix.Error)
			require.NotEmpty(t, outSuffix.ResultRef)
			ag, err := f.sched.store.GetAgent(ctx, outSuffix.ResultRef)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(ag.Slug, slug+"-"), "got slug %q", ag.Slug)
			assert.Equal(t, len(slug)+1+4, len(ag.Slug))

			// nameConflict=skip must return generic skipped "agent name in use" (no schedule wording, no slug).
			outSkip := fireDirect(t, "coll-"+phase+"-skip", DispatchAgentEventPayload{
				AgentName:    slug,
				NameConflict: DispatchAgentNameConflictSkip,
				Task:         "skip on " + phase,
			}, baseEvent(`{}`, nil))
			assert.Equal(t, hubfire.FireStatusSkipped, outSkip.Status, "phase=%s", phase)
			assert.Equal(t, "agent name in use", outSkip.Error)
			assert.NotContains(t, outSkip.Error, "schedule")
			assert.NotContains(t, outSkip.Error, slug)
		}
	})

	t.Run("event_attributes_cannot_override_hub_source_name_or_signature_mode", func(t *testing.T) {
		// 1. Webhook event with spoofed endpoint_name/signature_mode attributes still uses hub endpoint record.
		ev := baseEvent(`{"x":"ok"}`, map[string]string{
			"endpoint_name":  "spoofed-ep",
			"signature_mode": "spoofed_mode",
		})
		// baseEvent overwrites endpoint_name/signature_mode; explicitly spoof them after.
		ev.Attributes["endpoint_name"] = "spoofed-ep"
		ev.Attributes["signature_mode"] = "spoofed_mode"

		f.client.mu.Lock()
		beforeCalls := len(f.client.calls)
		f.client.mu.Unlock()

		out := fireDirect(t, "spoof-webhook", DispatchAgentEventPayload{
			Task:          "src=${event.source.name} val=${event.payload.x}",
			UntrustedVars: []string{"event.payload.x"},
		}, ev)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		f.client.mu.Lock()
		require.Greater(t, len(f.client.calls), beforeCalls)
		req := f.client.calls[len(f.client.calls)-1]
		f.client.mu.Unlock()

		assert.Contains(t, req.Config.Task, "src="+ep.Name)
		assert.NotContains(t, req.Config.Task, "spoofed-ep")
		assert.Contains(t, req.Config.Task, `Source: webhook endpoint "`+ep.Name+`" (scion_v1; integrity verified, content untrusted).`)
		assert.NotContains(t, req.Config.Task, "spoofed_mode")

		// 2. Non-webhook source (github, no endpoint row) with spoofed attributes:
		// event.source.name is missing (not answered from attributes), so required ${event.source.name} refuses.
		ghEv := &hubfire.FireEvent{
			ID:         api.NewUUID(),
			ProjectID:  f.sched.proj.ID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "inst-1",
			Type:       "github.pull_request.opened",
			Subject:    "github:acme/repo#1",
			OccurredAt: time.Now().UTC(),
			ReceivedAt: time.Now().UTC(),
			Attributes: map[string]string{
				"endpoint_name":  "spoofed-gh",
				"signature_mode": "spoofed_mode",
			},
			Payload: []byte(`{"x":"ok"}`),
			Trigger: &hubfire.FireTriggerRef{ID: api.NewUUID(), Name: "gh-trig"},
		}
		outMissing := fireDirect(t, "spoof-gh-missing", DispatchAgentEventPayload{
			Task: "src=${event.source.name}",
		}, ghEv)
		assert.Equal(t, hubfire.FireStatusRefused, outMissing.Status)
		assert.Equal(t, "template value missing", outMissing.Error)

		// With optional ${event.source.name?}, trailer uses default "verified", never spoofed_mode/spoofed-gh.
		f.client.mu.Lock()
		beforeCalls = len(f.client.calls)
		f.client.mu.Unlock()
		outOpt := fireDirect(t, "spoof-gh-opt", DispatchAgentEventPayload{
			Task:          "src=${event.source.name?} val=${event.payload.x}",
			UntrustedVars: []string{"event.payload.x"},
		}, ghEv)
		require.Equal(t, hubfire.FireStatusFired, outOpt.Status, outOpt.Error)
		f.client.mu.Lock()
		require.Greater(t, len(f.client.calls), beforeCalls)
		reqGH := f.client.calls[len(f.client.calls)-1]
		f.client.mu.Unlock()
		assert.NotContains(t, reqGH.Config.Task, "spoofed-gh")
		assert.NotContains(t, reqGH.Config.Task, "spoofed_mode")
		assert.Contains(t, reqGH.Config.Task, "(verified; integrity verified, content untrusted).")
	})

	t.Run("name_collision_exhausted", func(t *testing.T) {
		fe := newTriggerDispatchAgentFixture(t, "coll-exhaust")
		existing := &store.Agent{
			ID:        api.NewUUID(),
			Slug:      "exhausted-worker",
			Name:      "exhausted-worker",
			ProjectID: fe.sched.proj.ID,
			Phase:     "running",
		}
		_, fault := installStoreFault(t, fe.sched.srv, func(inner store.Store, sw *storeFaultSwitch) *alwaysCollidingSlugStore {
			return &alwaysCollidingSlugStore{Store: inner, fault: sw, prefix: "exhausted-worker", existing: existing}
		})
		epE := fe.createEndpoint(t, fe.userAuth(t, fe.sched.creator), "exhaust-ep")
		fault.Arm()

		raw, err := json.Marshal(DispatchAgentEventPayload{
			AgentName: "exhausted-worker",
			Task:      "will exhaust",
		})
		require.NoError(t, err)
		now := time.Now().UTC()
		out := fe.sched.srv.fireAction(ctx, hubfire.FireRequest{
			ProjectID: fe.sched.proj.ID,
			Authority: hubfire.RecordedAuthority{
				Attribution: store.InitiatorAttribution{
					InitiatorPrincipalKind:  store.DelegationPrincipalUser,
					InitiatorPrincipalID:    fe.sched.creator.ID,
					InitiatorCredentialKind: store.InitiatorCredentialKindSession,
					AttributionVersion:      1,
					AuthorizationRevision:   1,
				},
				Ceiling:        store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   api.NewUUID(),
				FireID:         api.NewUUID(),
			},
			ActionType: store.TriggerActionDispatchAgent,
			Payload:    string(raw),
			Event: &hubfire.FireEvent{
				ID:         api.NewUUID(),
				ProjectID:  fe.sched.proj.ID,
				SourceKind: store.TriggerSourceKindWebhook,
				SourceID:   epE.ID,
				Type:       "webhook.received",
				Subject:    "webhook:" + epE.ID,
				OccurredAt: now,
				ReceivedAt: now,
				Attributes: map[string]string{
					"endpoint_name":  epE.Name,
					"signature_mode": store.WebhookSignatureModeScionV1,
				},
				Payload: []byte(`{}`),
				Trigger: &hubfire.FireTriggerRef{ID: api.NewUUID(), Name: "exhaust-trig"},
			},
			FireAt: now,
		})
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Equal(t, "name exhausted", out.Error)
	})

	// O1: HTML-heavy <=16 KiB JSON payload fires without \u003c expansion overflow;
	// control-byte-heavy 16 KiB text payload fails with specific "event file too large".
	t.Run("event_file_html_chars_do_not_overflow", func(t *testing.T) {
		f.client.mu.Lock()
		before := len(f.client.calls)
		f.client.mu.Unlock()
		htmlBody := `{"data":"` + strings.Repeat("<", 15000) + `"}`
		out := fireDirect(t, "html-heavy", DispatchAgentEventPayload{
			AgentName: "html-heavy-agent",
			Task:      "handle html body",
		}, baseEvent(htmlBody, nil))
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)
		require.Empty(t, out.Error)
		f.client.mu.Lock()
		require.Greater(t, len(f.client.calls), before)
		createReq := f.client.calls[len(f.client.calls)-1]
		f.client.mu.Unlock()
		require.Len(t, createReq.HomeFiles, 1)
		assert.Contains(t, string(createReq.HomeFiles[0].Content), "<<<<")
		assert.NotContains(t, string(createReq.HomeFiles[0].Content), `\u003c`)
	})

	t.Run("event_file_control_bytes_overflow_specific_reason", func(t *testing.T) {
		ctrlBody := strings.Repeat("\x01", 16000)
		out := fireDirect(t, "ctrl-heavy", DispatchAgentEventPayload{
			AgentName: "ctrl-heavy-agent",
			Task:      "handle control bytes",
		}, baseEvent(ctrlBody, nil))
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Equal(t, "event file too large", out.Error)
	})
}

type alwaysCollidingSlugStore struct {
	store.Store
	fault    *storeFaultSwitch
	prefix   string
	existing *store.Agent
}

func (s *alwaysCollidingSlugStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	if s.fault.Active() && strings.HasPrefix(slug, s.prefix) {
		return s.existing, nil
	}
	return s.Store.GetAgentBySlug(ctx, projectID, slug)
}

func TestScheduleHandlers_RefuseTriggerOnlyDispatchAgentFields(t *testing.T) {
	f := newTriggerDispatchAgentFixture(t, "sched-refuse")
	userCtx := f.userAuth(t, f.sched.creator)

	badPayloads := []struct {
		name    string
		payload string
	}{
		{"nameConflict", `{"agentName":"a","nameConflict":"skip"}`},
		{"labels", `{"agentName":"a","labels":{"k":"v"}}`},
		{"ephemeral", `{"agentName":"a","ephemeral":true}`},
		{"githubToken", `{"agentName":"a","githubToken":"user_preferred"}`},
		{"agentRole", `{"agentName":"a","agentRole":"full"}`},
		{"reuseMessage", `{"agentName":"a","reuseMessage":"hi"}`},
		{"untrustedVars", `{"agentName":"a","untrustedVars":["event.type"]}`},
	}

	for _, tc := range badPayloads {
		t.Run("schedule_create_"+tc.name, func(t *testing.T) {
			rec := f.doJSON(t, userCtx, http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%s/schedules", f.sched.proj.ID),
				map[string]any{
					"name":      "sched-" + tc.name,
					"cronExpr":  "0 * * * *",
					"eventType": "dispatch_agent",
					"payload":   tc.payload,
				})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "not supported for schedules")
		})
		t.Run("scheduled_event_create_"+tc.name, func(t *testing.T) {
			rec := f.doJSON(t, userCtx, http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%s/scheduled-events", f.sched.proj.ID),
				map[string]any{
					"eventType": "dispatch_agent",
					"fireAt":    time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
					"payload":   tc.payload,
				})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "not supported for schedules")
		})
	}
}

type flipEndpointModeStore struct {
	store.Store
	active     atomic.Bool
	endpointID string
	mode       string
}

func (s *flipEndpointModeStore) GetWebhookEndpoint(ctx context.Context, id string) (*store.WebhookEndpoint, error) {
	ep, err := s.Store.GetWebhookEndpoint(ctx, id)
	if err == nil && ep != nil && s.active.Load() && id == s.endpointID {
		cp := *ep
		cp.SignatureMode = s.mode
		return &cp, nil
	}
	return ep, err
}

func TestTriggerDispatchAgent_UpdateResumeAndRoleCaps(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "upd-resume-caps")

	// Grant project-admin role via a real RBAC RoleBinding (members lack
	// trigger.update; create a fresh user with hubMemberUser + grantProjectRole).
	adminUser := hubMemberUser(t, f.sched.store, api.NewUUID())
	grantProjectRole(t, f.sched.store, adminUser.ID, f.sched.proj.ID, store.ProjectRoleAdmin)
	adminCtx := f.userAuth(t, adminUser)

	// Install store wrapper before audited trigger mutations so we can flip an
	// endpoint's signature mode to body_only for the resume test.
	flipper, _ := installStoreFault(t, f.sched.srv, func(inner store.Store, _ *storeFaultSwitch) *flipEndpointModeStore {
		return &flipEndpointModeStore{Store: inner, mode: store.WebhookSignatureModeBodyOnly}
	})

	scionEP := f.createEndpoint(t, adminCtx, "upd-scion-ep")
	flipper.endpointID = scionEP.ID

	bodyOnlyEP := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.sched.proj.ID,
		Name:                "upd-body-only-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    adminUser.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeBodyOnly,
		Status:              store.WebhookEndpointStatusActive,
		CreatedAt:           time.Now().UTC(),
		UpdatedAt:           time.Now().UTC(),
	}
	require.NoError(t, f.sched.store.CreateWebhookEndpoint(ctx, bodyOnlyEP))

	// Create a valid dispatch_agent trigger on the scion_v1 endpoint.
	createDispRec := f.doJSON(t, adminCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "disp-trig",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEP.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{Type: store.TriggerActionDispatchAgent, Task: "run"},
		})
	require.Equal(t, http.StatusCreated, createDispRec.Code, createDispRec.Body.String())
	var dispTr TriggerResponse
	require.NoError(t, json.Unmarshal(createDispRec.Body.Bytes(), &dispTr))

	// 1. PATCH source of a dispatch_agent trigger to a non-replay-protected endpoint -> 400 source_not_replay_protected.
	t.Run("patch_source_to_non_replay_protected_refused", func(t *testing.T) {
		rec := f.doJSON(t, adminCtx, http.MethodPatch,
			fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, dispTr.ID),
			UpdateTriggerRequest{
				Source: &TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: bodyOnlyEP.ID},
			})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "source_not_replay_protected")
	})

	// 2. PATCH action of a message trigger on a non-replay-protected endpoint to dispatch_agent -> 400 source_not_replay_protected.
	t.Run("patch_message_trigger_on_body_only_to_dispatch_agent_refused", func(t *testing.T) {
		targetAg := &store.Agent{
			ID:        api.NewUUID(),
			Slug:      "msg-target",
			Name:      "msg-target",
			ProjectID: f.sched.proj.ID,
			Phase:     "running",
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, targetAg))

		msgRec := f.doJSON(t, adminCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "msg-on-body-only",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: bodyOnlyEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "hi"},
			})
		require.Equal(t, http.StatusCreated, msgRec.Code, msgRec.Body.String())
		var msgTr TriggerResponse
		require.NoError(t, json.Unmarshal(msgRec.Body.Bytes(), &msgTr))

		patchRec := f.doJSON(t, adminCtx, http.MethodPatch,
			fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, msgTr.ID),
			UpdateTriggerRequest{
				Action: &TriggerActionSpec{Type: store.TriggerActionDispatchAgent, Task: "escalate"},
			})
		require.Equal(t, http.StatusBadRequest, patchRec.Code, patchRec.Body.String())
		assert.Contains(t, patchRec.Body.String(), "source_not_replay_protected")
	})

	// 3. Pause dispatch_agent trigger, endpoint becomes non-replay-protected, resume -> 400 source_not_replay_protected.
	t.Run("resume_after_endpoint_becomes_non_replay_protected_refused", func(t *testing.T) {
		pauseRec := f.doJSON(t, adminCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers/%s/pause", f.sched.proj.ID, dispTr.ID), nil)
		require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

		flipper.active.Store(true)
		t.Cleanup(func() { flipper.active.Store(false) })

		resumeRec := f.doJSON(t, adminCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers/%s/resume", f.sched.proj.ID, dispTr.ID), nil)
		require.Equal(t, http.StatusBadRequest, resumeRec.Code, resumeRec.Body.String())
		assert.Contains(t, resumeRec.Body.String(), "source_not_replay_protected")
	})

	// 4. Explicit agentRole: full with scion.io/max-agent-role=baseline -> 403.
	t.Run("explicit_agent_role_exceeding_project_max_refused", func(t *testing.T) {
		proj, err := f.sched.store.GetProject(ctx, f.sched.proj.ID)
		require.NoError(t, err)
		prevAnn := proj.Annotations
		if proj.Annotations == nil {
			proj.Annotations = map[string]string{}
		}
		proj.Annotations[projectSettingMaxAgentRole] = string(AgentRoleBaseline)
		require.NoError(t, f.sched.store.UpdateProject(ctx, proj))
		t.Cleanup(func() {
			proj.Annotations = prevAnn
			_ = f.sched.store.UpdateProject(ctx, proj)
		})

		rec := f.doJSON(t, adminCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "over-project-max",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{
					Type:      store.TriggerActionDispatchAgent,
					Task:      "run",
					AgentRole: string(AgentRoleFull),
				},
			})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "project maximum")
	})

	// 5. Frozen-ceiling refusal (DeniedByDelegationCeiling): agent with bounded ceiling
	// that admits baseline but not full requests explicit agentRole: full -> 403 DeniedByDelegationCeiling.
	t.Run("explicit_agent_role_exceeding_frozen_ceiling_refused", func(t *testing.T) {
		boundedAg := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "bounded-ceiling-author",
			Name:          "bounded-ceiling-author",
			ProjectID:     f.sched.proj.ID,
			Phase:         "running",
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, boundedAg))
		require.NoError(t, f.sched.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
			ID:            api.NewUUID(),
			DelegatorType: store.DelegationPrincipalUser,
			DelegatorID:   adminUser.ID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    boundedAg.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       f.sched.proj.ID,
			Role:          string(AgentRoleFull),
			Active:        true,
			EffectCeiling: store.EffectCeiling{
				Kind:              store.EffectCeilingBounded,
				Version:           1,
				PermissionIDs:     []string{"agent.create", "agent.read", "project.read", "trigger.create", "webhook_endpoint.read"},
				BoundaryKind:      "project",
				BoundaryProjectID: f.sched.proj.ID,
			},
		}))
		boundedCtx := f.agentAuth(t, boundedAg, AgentRoleFull)
		boundedEP := f.createEndpoint(t, boundedCtx, "bounded-ep")

		rec := f.doJSON(t, boundedCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "over-ceiling",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: boundedEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{
					Type:      store.TriggerActionDispatchAgent,
					Task:      "run",
					AgentRole: string(AgentRoleFull),
				},
			})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))
	})

	// 6. Agent author whose stored parent role is baseline (with a token carrying ScopeAgentCreate)
	// requesting explicit agentRole: full -> 403 parent-role cap refusal.
	t.Run("agent_author_requesting_role_above_parent_refused", func(t *testing.T) {
		parentAg := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "downgraded-parent",
			Name:          "downgraded-parent",
			ProjectID:     f.sched.proj.ID,
			Phase:         "running",
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleBaseline)},
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, parentAg))
		require.NoError(t, f.sched.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
			ID:            api.NewUUID(),
			DelegatorType: store.DelegationPrincipalUser,
			DelegatorID:   adminUser.ID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    parentAg.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       f.sched.proj.ID,
			Role:          string(AgentRoleBaseline),
			Active:        true,
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
		parentCtx := f.agentAuth(t, parentAg, AgentRoleFull)
		parentEP := f.createEndpoint(t, parentCtx, "parent-cap-ep")

		rec := f.doJSON(t, parentCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "over-parent-role",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: parentEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{
					Type:      store.TriggerActionDispatchAgent,
					Task:      "run",
					AgentRole: string(AgentRoleFull),
				},
			})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "parent agent role")
	})

	// 7. UAT author with a bounded token ceiling: explicit agentRole: full exceeds
	// the UAT's hop ceiling -> 403 DeniedByDelegationCeiling, while readonly fits -> 201.
	t.Run("uat_author_explicit_agent_role_capped_by_uat_ceiling", func(t *testing.T) {
		base := NewAuthenticatedUser(adminUser.ID, adminUser.Email, adminUser.DisplayName, adminUser.Role, "api")
		uatPerms := append(
			agentScopeCoverage(ScopesForRole(AgentRoleReadOnly)),
			"agent.create",
			"trigger.create",
			"webhook_endpoint.read",
		)
		uatIdent := NewScopedUserIdentityWithCeiling(
			base,
			f.sched.proj.ID,
			[]string{"project:read", "agent:create", "agent:read", "agent:list", "trigger:create", "webhook_endpoint:read"},
			api.NewUUID(),
			permissions.FrozenPermissionCeiling{
				Version:       1,
				PermissionIDs: sortedUniqueIDs(uatPerms),
			},
		)
		uatCtx := contextWithIdentity(context.Background(), uatIdent)
		uatCtx = contextWithCredentialContext(uatCtx, credentialContextForIdentity(uatIdent))

		rec := f.doJSON(t, uatCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "uat-over-ceiling",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{
					Type:      store.TriggerActionDispatchAgent,
					Task:      "run",
					AgentRole: string(AgentRoleFull),
				},
			})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))

		recOK := f.doJSON(t, uatCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "uat-within-ceiling",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{
					Type:      store.TriggerActionDispatchAgent,
					Task:      "run",
					AgentRole: string(AgentRoleReadOnly),
				},
			})
		require.Equal(t, http.StatusCreated, recOK.Code, recOK.Body.String())
	})

	// 8. O2: UAT whose ceiling admits agent.create and trigger.create but no
	// agent role (not even readonly) is refused at authoring even when agentRole
	// is omitted (default ladder).
	t.Run("uat_author_omitted_agent_role_no_role_fits_ceiling_refused", func(t *testing.T) {
		base := NewAuthenticatedUser(adminUser.ID, adminUser.Email, adminUser.DisplayName, adminUser.Role, "api")
		uatIdent := NewScopedUserIdentityWithCeiling(
			base,
			f.sched.proj.ID,
			[]string{"project:read", "agent:create", "trigger:create", "webhook_endpoint:read"},
			api.NewUUID(),
			permissions.FrozenPermissionCeiling{
				Version: 1,
				PermissionIDs: []string{
					"agent.create",
					"project.read",
					"trigger.create",
					"webhook_endpoint.read",
				},
			},
		)
		uatCtx := contextWithIdentity(context.Background(), uatIdent)
		uatCtx = contextWithCredentialContext(uatCtx, credentialContextForIdentity(uatIdent))

		rec := f.doJSON(t, uatCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "uat-omitted-role-no-fit",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{
					Type: store.TriggerActionDispatchAgent,
					Task: "run",
				},
			})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))
	})
}

func TestTriggerDispatchAgent_GitHubSourceAuthoring(t *testing.T) {
	f := newTriggerDispatchAgentFixture(t, "gh-unsupported")
	ctx := context.Background()

	// Enable both hub.triggers and hub.github_triggers.
	var active []experiments.Experiment
	for _, e := range f.sched.srv.experimentRegistry().All() {
		if e.Name == experiments.Triggers || e.Name == experiments.GitHubTriggers {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.sched.srv.experiments = reg

	adminUser := hubMemberUser(t, f.sched.store, api.NewUUID())
	adminCtx := f.userAuth(t, adminUser)

	// Set up a project with an authorized GitHub App association (CreatedBy persisted
	// at creation + creator GITHUB_TOKEN + matching active installation).
	const instID int64 = 9101
	require.NoError(t, f.sched.store.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
	}))
	idCopy := instID
	ghProj := &store.Project{
		ID:                   api.NewUUID(),
		Slug:                 "gh-unsupported-proj",
		Name:                 "gh-unsupported-proj",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &idCopy,
		CreatedBy:            adminUser.ID,
	}
	require.NoError(t, f.sched.store.CreateProject(ctx, ghProj))
	grantProjectRole(t, f.sched.store, adminUser.ID, ghProj.ID, store.ProjectRoleAdmin)
	_, _, err = f.sched.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GITHUB_TOKEN",
		Value:      "ghp_test_token",
		SecretType: secret.TypeEnvironment,
		Scope:      secret.ScopeUser,
		ScopeID:    adminUser.ID,
	})
	require.NoError(t, err)
	f.sched.srv.invalidateGitHubAssociation(ghProj.ID)

	// Target agent for a valid github message trigger.
	targetAg := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "gh-msg-target",
		Name:      "gh-msg-target",
		ProjectID: ghProj.ID,
		Phase:     "running",
		CreatedBy: adminUser.ID,
		OwnerID:   adminUser.ID,
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, targetAg))

	// 1. Create github + dispatch_agent with omitted githubToken -> 201 and
	// resolves/stores githubToken="installation" (visible in GET, §5.1a).
	rec := f.doJSON(t, adminCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", ghProj.ID),
		CreateTriggerRequest{
			Name:   "gh-dispatch-ok",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "triage",
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var ghTrig TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ghTrig))
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, ghTrig.Action.GithubToken)

	recGet := f.doJSON(t, adminCtx, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", ghProj.ID, ghTrig.ID), nil)
	require.Equal(t, http.StatusOK, recGet.Code, recGet.Body.String())
	var gotGHTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recGet.Body.Bytes(), &gotGHTrig))
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, gotGHTrig.Action.GithubToken)

	// 2. Create github + message -> 201, then PATCH action to dispatch_agent -> 200
	// with omitted githubToken resolved to "installation".
	recMsg := f.doJSON(t, adminCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", ghProj.ID),
		CreateTriggerRequest{
			Name:   "gh-msg-ok",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: targetAg.ID,
				Message: "hello",
			},
		})
	require.Equal(t, http.StatusCreated, recMsg.Code, recMsg.Body.String())
	var msgTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recMsg.Body.Bytes(), &msgTrig))

	recPatch := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", ghProj.ID, msgTrig.ID),
		map[string]any{
			"action": TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "escalate",
			},
		})
	require.Equal(t, http.StatusOK, recPatch.Code, recPatch.Body.String())
	var patchedMsgTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recPatch.Body.Bytes(), &patchedMsgTrig))
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, patchedMsgTrig.Action.GithubToken)

	// 3. webhook + dispatch_agent remains accepted (201, resolves to user_preferred),
	// PATCHing its source to github succeeds while preserving stored user_preferred,
	// and PATCHing subject.mode=skip is accepted now that dispatch_agent subject
	// claims are implemented.
	recEP := f.doJSON(t, adminCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints", ghProj.ID),
		CreateWebhookEndpointRequest{Name: "wh-still-ok"})
	require.Equal(t, http.StatusCreated, recEP.Code, recEP.Body.String())
	var ghProjEP WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(recEP.Body.Bytes(), &ghProjEP))

	recWH := f.doJSON(t, adminCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", ghProj.ID),
		CreateTriggerRequest{
			Name:   "wh-dispatch-ok",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ghProjEP.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "run",
			},
		})
	require.Equal(t, http.StatusCreated, recWH.Code, recWH.Body.String())
	var whTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recWH.Body.Bytes(), &whTrig))
	assert.Equal(t, DispatchAgentGitHubTokenUserPreferred, whTrig.Action.GithubToken)

	// PATCH webhook dispatch_agent trigger's source to github with req.Action == nil
	// -> 400 github_token_required (cannot silently carry webhook default
	// user_preferred onto a github rule).
	recPatchSrc := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", ghProj.ID, whTrig.ID),
		map[string]any{
			"source": TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			"match":  map[string]any{"types": []string{"github.issue_comment.created"}},
		})
	require.Equal(t, http.StatusBadRequest, recPatchSrc.Code, recPatchSrc.Body.String())
	assert.Contains(t, recPatchSrc.Body.String(), "github_token_required")

	// Same PATCH carrying an action with githubToken == "" -> 200 and re-resolves
	// to the github source default ("installation").
	recPatchSrcEmptyTok := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", ghProj.ID, whTrig.ID),
		map[string]any{
			"source": TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			"match":  map[string]any{"types": []string{"github.issue_comment.created"}},
			"action": TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "run",
			},
		})
	require.Equal(t, http.StatusOK, recPatchSrcEmptyTok.Code, recPatchSrcEmptyTok.Body.String())
	var patchedWHEmptyTok TriggerResponse
	require.NoError(t, json.Unmarshal(recPatchSrcEmptyTok.Body.Bytes(), &patchedWHEmptyTok))
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, patchedWHEmptyTok.Action.GithubToken)

	// A second webhook dispatch_agent trigger PATCHed to github with explicit
	// githubToken="installation" -> 200 and stored.
	recWHInst := f.doJSON(t, adminCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", ghProj.ID),
		CreateTriggerRequest{
			Name:   "wh-dispatch-ok-inst",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ghProjEP.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "run",
			},
		})
	require.Equal(t, http.StatusCreated, recWHInst.Code, recWHInst.Body.String())
	var whTrigInst TriggerResponse
	require.NoError(t, json.Unmarshal(recWHInst.Body.Bytes(), &whTrigInst))

	recPatchSrcInst := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", ghProj.ID, whTrigInst.ID),
		map[string]any{
			"source": TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			"match":  map[string]any{"types": []string{"github.issue_comment.created"}},
			"action": TriggerActionSpec{
				Type:        store.TriggerActionDispatchAgent,
				Task:        "run",
				GithubToken: DispatchAgentGitHubTokenInstallation,
			},
		})
	require.Equal(t, http.StatusOK, recPatchSrcInst.Code, recPatchSrcInst.Body.String())
	var patchedWHInst TriggerResponse
	require.NoError(t, json.Unmarshal(recPatchSrcInst.Body.Bytes(), &patchedWHInst))
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, patchedWHInst.Action.GithubToken)

	// A second webhook dispatch_agent trigger PATCHed to github with explicit
	// githubToken="user_preferred" -> 200 and stored.
	recWH2 := f.doJSON(t, adminCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", ghProj.ID),
		CreateTriggerRequest{
			Name:   "wh-dispatch-ok-2",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ghProjEP.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "run",
			},
		})
	require.Equal(t, http.StatusCreated, recWH2.Code, recWH2.Body.String())
	var whTrig2 TriggerResponse
	require.NoError(t, json.Unmarshal(recWH2.Body.Bytes(), &whTrig2))

	recPatchSrcUserPref := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", ghProj.ID, whTrig2.ID),
		map[string]any{
			"source": TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			"match":  map[string]any{"types": []string{"github.issue_comment.created"}},
			"action": TriggerActionSpec{
				Type:        store.TriggerActionDispatchAgent,
				Task:        "run",
				GithubToken: DispatchAgentGitHubTokenUserPreferred,
			},
		})
	require.Equal(t, http.StatusOK, recPatchSrcUserPref.Code, recPatchSrcUserPref.Body.String())
	var patchedWHUserPref TriggerResponse
	require.NoError(t, json.Unmarshal(recPatchSrcUserPref.Body.Bytes(), &patchedWHUserPref))
	assert.Equal(t, DispatchAgentGitHubTokenUserPreferred, patchedWHUserPref.Action.GithubToken)

	// PATCH webhook dispatch_agent trigger's subject.mode to skip -> 200 OK.
	recPatchSkip := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", ghProj.ID, whTrig.ID),
		map[string]any{
			"subject": map[string]any{"mode": store.TriggerSubjectModeSkip},
		})
	require.Equal(t, http.StatusOK, recPatchSkip.Code, recPatchSkip.Body.String())
	var patchedTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recPatchSkip.Body.Bytes(), &patchedTrig))
	require.NotNil(t, patchedTrig.Subject)
	assert.Equal(t, store.TriggerSubjectModeSkip, patchedTrig.Subject.Mode)
}

func TestScheduleDispatchAgent_IgnoresStrayAgentRoleAndCleanCollisionError(t *testing.T) {
	f := newTriggerDispatchAgentFixture(t, "sched-o3-n1")
	ctx := context.Background()

	// Cap project max role to baseline so an explicit "full" would be refused,
	// whereas a schedule fire (which ignores stray agentRole) caps to baseline.
	proj, err := f.sched.store.GetProject(ctx, f.sched.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingMaxAgentRole] = string(AgentRoleBaseline)
	require.NoError(t, f.sched.store.UpdateProject(ctx, proj))

	raw, err := json.Marshal(DispatchAgentEventPayload{
		AgentName: "sched-stray-role",
		Task:      "scheduled work",
		AgentRole: string(AgentRoleFull),
	})
	require.NoError(t, err)

	req := hubfire.FireRequest{
		ProjectID: f.sched.proj.ID,
		Authority: hubfire.RecordedAuthority{
			Attribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  store.DelegationPrincipalUser,
				InitiatorPrincipalID:    f.sched.creator.ID,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession,
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
			Ceiling:        store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			DefinitionKind: hubfire.DefinitionKindSchedule,
			DefinitionID:   api.NewUUID(),
			FireID:         api.NewUUID(),
		},
		ActionType: store.TriggerActionDispatchAgent,
		Payload:    string(raw),
		FireAt:     time.Now().UTC(),
	}

	// O3: Schedule fire ignores stray "agentRole": "full" and succeeds with baseline.
	out := f.sched.srv.fireAction(ctx, req)
	require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)
	created, err := f.sched.store.GetAgentBySlug(ctx, f.sched.proj.ID, "sched-stray-role")
	require.NoError(t, err)
	require.NotNil(t, created.AppliedConfig)
	assert.Equal(t, string(AgentRoleBaseline), created.AppliedConfig.AgentRole)

	// N1: Second schedule fire with the same slug returns non-duplicated error text.
	req.Authority.FireID = api.NewUUID()
	out2 := f.sched.srv.fireAction(ctx, req)
	assert.Equal(t, hubfire.FireStatusFailed, out2.Status)
	assert.Equal(t, `agent "sched-stray-role" already exists in project`, out2.Error)
}

func TestTriggerDispatchAgent_AuthoringAuthorityAndSelfSufficientValidate(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "authz-consol")
	userCtx := f.userAuth(t, f.sched.creator)
	epResp := f.createEndpoint(t, userCtx, "consol-ep")
	ep, err := f.sched.store.GetWebhookEndpoint(ctx, epResp.ID)
	require.NoError(t, err)

	validPayload, err := json.Marshal(DispatchAgentEventPayload{
		Template: "default",
		Task:     "Run ${event.id}",
	})
	require.NoError(t, err)

	action := &dispatchAgentAction{srv: f.sched.srv}
	webhookCtx := withDispatchAgentAuthoringOpts(ctx, dispatchAgentAuthoringOpts{
		sourceKind: store.TriggerSourceKindWebhook,
		endpoint:   ep,
	})

	t.Run("self_sufficient_validate_requires_replay_protected_source", func(t *testing.T) {
		caller := GetIdentityFromContext(userCtx)
		_, err := action.Validate(ctx, caller, f.sched.proj.ID, string(validPayload))
		var ve *dispatchAgentValidationError
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, http.StatusBadRequest, ve.status)
		assert.Equal(t, "source_not_replay_protected", ve.code)
	})

	t.Run("self_sufficient_validate_enforces_github_experiment_gate", func(t *testing.T) {
		// hub.github_triggers is disabled by default in newTriggerDispatchAgentFixture.
		caller := GetIdentityFromContext(userCtx)
		ghCtx := withDispatchAgentAuthoringOpts(ctx, dispatchAgentAuthoringOpts{
			sourceKind: store.TriggerSourceKindGitHub,
		})
		_, err := action.Validate(ghCtx, caller, f.sched.proj.ID, string(validPayload))
		var ve *dispatchAgentValidationError
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, http.StatusBadRequest, ve.status)
		assert.Equal(t, ErrCodeExperimentDisabled, ve.code)
	})

	t.Run("agent_project_mismatch_refused", func(t *testing.T) {
		otherProj := &store.Project{ID: api.NewUUID(), Name: "other-proj", Slug: "other-proj"}
		require.NoError(t, f.sched.store.CreateProject(ctx, otherProj))
		otherAg := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "other-proj-agent",
			Name:          "other-proj-agent",
			ProjectID:     otherProj.ID,
			Phase:         "running",
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, otherAg))
		otherIdent := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: otherAg.ID},
			ProjectID: otherProj.ID,
			Scopes:    []AgentTokenScope{ScopeAgentCreate},
			Ancestry:  []string{f.sched.creator.ID},
		}}
		_, err := action.Validate(webhookCtx, otherIdent, f.sched.proj.ID, string(validPayload))
		var ve *dispatchAgentValidationError
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, http.StatusForbidden, ve.status)
		assert.Contains(t, ve.message, "own project")
	})

	t.Run("non_agent_non_user_identity_refused", func(t *testing.T) {
		brokerIdent := NewBrokerIdentity("broker-1")
		_, err := action.Validate(webhookCtx, brokerIdent, f.sched.proj.ID, string(validPayload))
		var ve *dispatchAgentValidationError
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, http.StatusForbidden, ve.status)
	})

	t.Run("suspended_agent_author_standing_refused", func(t *testing.T) {
		// Create a dedicated root user for this agent so suspending the root
		// user puts only this agent out of standing without affecting other subtests.
		rootUser := &store.User{
			ID:          api.NewUUID(),
			Email:       "suspended-root@example.com",
			DisplayName: "Suspended Root",
			Role:        store.UserRoleMember,
			Status:      "active",
		}
		require.NoError(t, f.sched.store.CreateUser(ctx, rootUser))
		grantProjectRole(t, f.sched.store, rootUser.ID, f.sched.proj.ID, store.ProjectRoleOwner)

		heldAg := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "held-author",
			Name:          "held-author",
			ProjectID:     f.sched.proj.ID,
			Phase:         "running",
			CreatedBy:     rootUser.ID,
			OwnerID:       rootUser.ID,
			Ancestry:      []string{rootUser.ID},
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, heldAg))
		require.NoError(t, f.sched.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
			ID:            api.NewUUID(),
			DelegatorType: store.DelegationPrincipalUser,
			DelegatorID:   rootUser.ID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    heldAg.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       f.sched.proj.ID,
			Role:          string(AgentRoleFull),
			Active:        true,
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
		heldCtx := f.agentAuth(t, heldAg, AgentRoleFull)
		heldEPResp := f.createEndpoint(t, heldCtx, "held-ep")
		heldEP, err := f.sched.store.GetWebhookEndpoint(ctx, heldEPResp.ID)
		require.NoError(t, err)

		// Place an active hold on the authoring agent while keeping its root
		// user and delegation chain live: agentCreateDecision still allows, so
		// refusal must come from agentStanding (ptone/scion#3433).
		heldIdent := GetIdentityFromContext(heldCtx)
		require.True(t, f.sched.srv.agentCreateDecision(ctx, heldIdent, f.sched.proj.ID).Allowed,
			"precondition: delegation chain must admit before placing hold")
		_, err = f.sched.store.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID:           heldAg.ID,
			ProjectID:         f.sched.proj.ID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   rootUser.ID,
			Trigger:           store.MembershipLossTriggerMemberRemove,
			ActorKind:         "system",
			ActorID:           "hub",
			CorrelationID:     "test-standing-author",
		}})
		require.NoError(t, err)
		require.True(t, f.sched.srv.agentCreateDecision(ctx, heldIdent, f.sched.proj.ID).Allowed,
			"precondition: agentCreateDecision must still allow with live chain so agentStanding is exercised")

		// Direct Validate fails in agentStanding with DeniedByDelegationCeiling.
		heldValidateCtx := withDispatchAgentAuthoringOpts(ctx, dispatchAgentAuthoringOpts{
			sourceKind: store.TriggerSourceKindWebhook,
			endpoint:   heldEP,
		})
		_, err = action.Validate(heldValidateCtx, heldIdent, f.sched.proj.ID, string(validPayload))
		var ve *dispatchAgentValidationError
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, http.StatusForbidden, ve.status)
		assert.Equal(t, DeniedByDelegationCeiling, ve.deniedBy)
		assert.Contains(t, ve.reason, "not in good standing")

		// HTTP POST /triggers also refuses the out-of-standing agent (404 from
		// resolveTriggerWebhookSource's standing/endpoint-probing mask, or 403).
		rec := f.doJSON(t, heldCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "from-held-author",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: heldEP.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: TriggerActionSpec{
					Type:     store.TriggerActionDispatchAgent,
					Template: "default",
					Task:     "Run ${event.id}",
				},
			})
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("self_sufficient_validate_enforces_uat_hop_ceiling_without_precomputed_ceiling", func(t *testing.T) {
		base := NewAuthenticatedUser(f.sched.creator.ID, f.sched.creator.Email, f.sched.creator.DisplayName, f.sched.creator.Role, "user")
		uatPerms := append(
			agentScopeCoverage(ScopesForRole(AgentRoleReadOnly)),
			"agent.create",
			"trigger.create",
			"webhook_endpoint.read",
		)
		uatIdent := NewScopedUserIdentityWithCeiling(
			base,
			f.sched.proj.ID,
			[]string{"project:read", "agent:create", "agent:read", "agent:list", "trigger:create", "webhook_endpoint:read"},
			api.NewUUID(),
			permissions.FrozenPermissionCeiling{
				Version:       1,
				PermissionIDs: sortedUniqueIDs(uatPerms),
			},
		)
		fullPayload, mErr := json.Marshal(DispatchAgentEventPayload{
			Template:  "default",
			Task:      "Run ${event.id}",
			AgentRole: string(AgentRoleFull),
		})
		require.NoError(t, mErr)
		_, err := action.Validate(webhookCtx, uatIdent, f.sched.proj.ID, string(fullPayload))
		var ve *dispatchAgentValidationError
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, http.StatusForbidden, ve.status)
		assert.Equal(t, DeniedByDelegationCeiling, ve.deniedBy)
	})
}

type recordingTemplateRefusalAuditSink struct {
	mu      sync.Mutex
	records []trigger.TemplateRefusalAuditRecord
}

func (s *recordingTemplateRefusalAuditSink) RecordTemplateRefusal(_ context.Context, rec trigger.TemplateRefusalAuditRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

func (s *recordingTemplateRefusalAuditSink) snapshot() []trigger.TemplateRefusalAuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]trigger.TemplateRefusalAuditRecord, len(s.records))
	copy(out, s.records)
	return out
}

func TestTriggerDispatchAgent_TemplateRefusalTypedAudit(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "tmpl-audit")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "audit-ep")

	sink := &recordingTemplateRefusalAuditSink{}
	f.sched.srv.SetTriggerTemplateRefusalAuditSink(sink)
	t.Cleanup(func() { f.sched.srv.SetTriggerTemplateRefusalAuditSink(nil) })

	const secretVal = "SUPER_SECRET_SENTINEL_NEVER_AUDIT_12345"

	raw, err := json.Marshal(DispatchAgentEventPayload{
		Template:      "default",
		Task:          "Do work",
		AgentName:     "pr-${event.payload.bad_name}",
		UntrustedVars: []string{"event.payload.bad_name"},
	})
	require.NoError(t, err)

	trigID := api.NewUUID()
	evID := api.NewUUID()
	fireID := api.NewUUID()
	out := f.sched.srv.fireAction(ctx, hubfire.FireRequest{
		ProjectID: f.sched.proj.ID,
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
			DefinitionID:   trigID,
			FireID:         fireID,
		},
		ActionType: store.TriggerActionDispatchAgent,
		Payload:    string(raw),
		Event: &hubfire.FireEvent{
			ID:         evID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			ReceivedAt: time.Now().UTC(),
			Payload:    []byte(`{"bad_name":"` + secretVal + `"}`),
			Trigger:    &hubfire.FireTriggerRef{ID: trigID, Name: "audit-trig"},
		},
	})
	require.Equal(t, hubfire.FireStatusRefused, out.Status)
	assert.Equal(t, string(triggertemplate.KindValueRejected), out.Error)

	recs := sink.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, f.sched.proj.ID, recs[0].ProjectID)
	assert.Equal(t, trigID, recs[0].TriggerID)
	assert.Equal(t, evID, recs[0].EventID)
	assert.Equal(t, fireID, recs[0].FireID)
	assert.Equal(t, triggertemplate.SlotName, recs[0].Slot)
	assert.Equal(t, "event.payload.bad_name", recs[0].Path)
	assert.Equal(t, string(triggertemplate.KindValueRejected), recs[0].Reason)
	recJSON, _ := json.Marshal(recs[0])
	assert.NotContains(t, string(recJSON), secretVal)
}

func TestTriggerHandlers_RefuseCrossActionFields(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "cross-action")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "cross-ep")

	targetAg := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "cross-target",
		Name:      "cross-target",
		ProjectID: f.sched.proj.ID,
		Phase:     "running",
		CreatedBy: f.sched.creator.ID,
		OwnerID:   f.sched.creator.ID,
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, targetAg))

	postTrig := func(action TriggerActionSpec) *httptest.ResponseRecorder {
		return f.doJSON(t, userCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   "cross-" + api.NewUUID()[:8],
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: action,
			})
	}

	// message action carrying dispatch_agent / github.comment fields -> 400.
	msgBadCases := []struct {
		name      string
		action    TriggerActionSpec
		wantField string
	}{
		{
			name:      "message_with_task",
			action:    TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "hi", Task: "do work"},
			wantField: "action.task",
		},
		{
			name:      "message_with_ackComment",
			action:    TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "hi", AckComment: &DispatchAgentAckComment{Body: "ack"}},
			wantField: "action.ackComment",
		},
		{
			name:      "message_with_labels",
			action:    TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "hi", Labels: map[string]string{"k": "v"}},
			wantField: "action.labels",
		},
		{
			name:      "message_with_agentRole",
			action:    TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "hi", AgentRole: "baseline"},
			wantField: "action.agentRole",
		},
		{
			name:      "message_with_body",
			action:    TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "hi", Body: "comment"},
			wantField: "action.body",
		},
	}
	for _, tc := range msgBadCases {
		t.Run("create_"+tc.name, func(t *testing.T) {
			rec := postTrig(tc.action)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantField)
		})
	}

	// dispatch_agent action carrying message / github.comment fields -> 400.
	dispatchBadCases := []struct {
		name      string
		action    TriggerActionSpec
		wantField string
	}{
		{
			name:      "dispatch_with_message",
			action:    TriggerActionSpec{Type: store.TriggerActionDispatchAgent, Template: "default", Task: "run", Message: "hi"},
			wantField: "action.message",
		},
		{
			name:      "dispatch_with_agentId",
			action:    TriggerActionSpec{Type: store.TriggerActionDispatchAgent, Template: "default", Task: "run", AgentID: targetAg.ID},
			wantField: "action.agentId",
		},
		{
			name:      "dispatch_with_interrupt",
			action:    TriggerActionSpec{Type: store.TriggerActionDispatchAgent, Template: "default", Task: "run", Interrupt: true},
			wantField: "action.interrupt",
		},
		{
			name:      "dispatch_with_wake",
			action:    TriggerActionSpec{Type: store.TriggerActionDispatchAgent, Template: "default", Task: "run", Wake: true},
			wantField: "action.wake",
		},
	}
	for _, tc := range dispatchBadCases {
		t.Run("create_"+tc.name, func(t *testing.T) {
			rec := postTrig(tc.action)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantField)
		})
	}

	// PATCH with cross-action fields -> 400 (requires project-admin role for trigger.update).
	adminUser := hubMemberUser(t, f.sched.store, api.NewUUID())
	grantProjectRole(t, f.sched.store, adminUser.ID, f.sched.proj.ID, store.ProjectRoleAdmin)
	adminCtx := f.userAuth(t, adminUser)

	validRec := postTrig(TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "ok"})
	require.Equal(t, http.StatusCreated, validRec.Code, validRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(validRec.Body.Bytes(), &created))

	patchMsgRec := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, created.ID),
		UpdateTriggerRequest{
			Action: &TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: targetAg.ID,
				Message: "updated",
				Task:    "stray-task",
			},
		})
	require.Equal(t, http.StatusBadRequest, patchMsgRec.Code, patchMsgRec.Body.String())
	assert.Contains(t, patchMsgRec.Body.String(), "action.task")

	patchDispatchRec := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, created.ID),
		UpdateTriggerRequest{
			Action: &TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "run",
				Wake:     true,
			},
		})
	require.Equal(t, http.StatusBadRequest, patchDispatchRec.Code, patchDispatchRec.Body.String())
	assert.Contains(t, patchDispatchRec.Body.String(), "action.wake")
}
