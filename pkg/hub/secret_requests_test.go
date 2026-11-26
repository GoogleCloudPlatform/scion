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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type secretRequestsFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	agent   *store.Agent
	alice   *store.User // owner / member with agent.attach
	bob     *store.User // outsider (not recipient, no attach)
	carol   *store.User // project viewer (agent.read, NO agent.attach)
	audit   *srAuditLogger
	logBuf  *bytes.Buffer
	prevLog *slog.Logger
	backend *failingSecretBackend
}

type srAuditLogger struct {
	*LogAuditLogger
	mu     sync.Mutex
	events []*MessageActionEvent
}

func (c *srAuditLogger) LogMessageActionEvent(ctx context.Context, event *MessageActionEvent) error {
	c.mu.Lock()
	if event != nil {
		cp := *event
		c.events = append(c.events, &cp)
	}
	c.mu.Unlock()
	return c.LogAuditLogger.LogMessageActionEvent(ctx, event)
}

func (c *srAuditLogger) snapshot() []*MessageActionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*MessageActionEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *srAuditLogger) reset() {
	c.mu.Lock()
	c.events = nil
	c.mu.Unlock()
}

type failingSecretBackend struct {
	secret.SecretBackend
	mu                sync.Mutex
	failOnKey         string
	failErr           error
	getMetaErrKey     string
	getMetaErr        error
	getMetaFailOnCall int // 0 or 1 = fail on first matching call; N = fail on Nth matching call
	getMetaCalls      int
	setCalls          int
	afterSet          func(ctx context.Context, input *secret.SetSecretInput)
}

func (f *failingSecretBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	f.mu.Lock()
	errKey := f.getMetaErrKey
	metaErr := f.getMetaErr
	failOn := f.getMetaFailOnCall
	matched := errKey != "" && strings.EqualFold(name, errKey)
	callNum := 0
	if matched {
		f.getMetaCalls++
		callNum = f.getMetaCalls
	}
	f.mu.Unlock()
	if matched && (failOn <= 1 || callNum >= failOn) {
		return nil, metaErr
	}
	return f.SecretBackend.GetMeta(ctx, name, scope, scopeID)
}

func (f *failingSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	f.mu.Lock()
	f.setCalls++
	failKey := f.failOnKey
	failErr := f.failErr
	afterSet := f.afterSet
	f.mu.Unlock()
	if failKey != "" && input != nil && strings.EqualFold(input.Name, failKey) {
		return false, nil, failErr
	}
	created, meta, err := f.SecretBackend.Set(ctx, input)
	if err == nil && afterSet != nil {
		afterSet(ctx, input)
	}
	return created, meta, err
}

func newSecretRequestsFixture(t *testing.T, enableExp bool) *secretRequestsFixture {
	t.Helper()
	srv, st := testServer(t)
	if srv.GetSecretBackend() == nil {
		srv.SetSecretBackend(secret.NewLocalBackend(st, "test-hub", "test-signing-key-32-bytes-long!!"))
	}
	ctx := context.Background()

	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(testLogger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	audit := &srAuditLogger{LogAuditLogger: &LogAuditLogger{prefix: "[Audit]", log: testLogger}}
	srv.auditLogger = audit
	srv.messageLog = testLogger

	fb := &failingSecretBackend{SecretBackend: srv.GetSecretBackend()}
	srv.SetSecretBackend(fb)

	if enableExp {
		raw := json.RawMessage(`{"overrides":{"hub.agent_secrets":true}}`)
		_, err := st.UpsertHubSetting(ctx, "experiments", raw, "test", -1, "managed")
		require.NoError(t, err)
		ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		srv.SetOperationalSettings(ops)
		require.True(t, srv.experimentEnabled(experiments.AgentSecrets))
	}

	proj := &store.Project{
		ID:   api.NewUUID(),
		Slug: "sr-proj-" + api.NewUUID()[:8],
		Name: "Secret Project",
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	alice := &store.User{
		ID:          api.NewUUID(),
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, alice))

	bob := &store.User{
		ID:          api.NewUUID(),
		Email:       "bob@example.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, bob))

	carol := &store.User{
		ID:          api.NewUUID(),
		Email:       "carol@example.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, carol))

	// Give alice project-owner role (agent.read + agent.attach) and carol project-viewer role (agent.read, NO agent.attach).
	ownerRD, err := st.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      alice.ID,
	})
	require.NoError(t, err)
	viewerRD, err := st.GetRoleDefinitionByName(ctx, "project-viewer", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: viewerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      carol.ID,
	})
	require.NoError(t, err)

	ag := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "sr-agent",
		Name:      "sr-agent",
		ProjectID: proj.ID,
		OwnerID:   alice.ID,
		Phase:     "running",
	}
	require.NoError(t, st.CreateAgent(ctx, ag))

	return &secretRequestsFixture{
		srv:     srv,
		store:   st,
		project: proj,
		agent:   ag,
		alice:   alice,
		bob:     bob,
		carol:   carol,
		audit:   audit,
		logBuf:  &logBuf,
		prevLog: prev,
		backend: fb,
	}
}

func (f *secretRequestsFixture) sessionToken(t *testing.T, u *store.User) string {
	t.Helper()
	tok, _, _, err := f.srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

func (f *secretRequestsFixture) sendSecretRequest(t *testing.T, recipient string, reqs []messages.SecretRequestInput, update bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(OutboundMessageRequest{
		Recipient:            recipient,
		Msg:                  "Please provide the requested secret.",
		SecretRequests:       reqs,
		SecretRequestsUpdate: update,
	})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(agentCtx(r.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rr, r)
	return rr
}

func (f *secretRequestsFixture) doUserJSON(t *testing.T, method, path string, u *store.User, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader)
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+f.sessionToken(t, u))
	rr := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rr, r)
	return rr
}

func (f *secretRequestsFixture) doAgentJSON(t *testing.T, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader)
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r = r.WithContext(agentCtx(r.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rr, r)
	return rr
}

// TestSecretRequests_ExperimentOff verifies every new route and send flag is
// refused when hub.agent_secrets is disabled.
func TestSecretRequests_ExperimentOff(t *testing.T) {
	f := newSecretRequestsFixture(t, false)

	rr := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{{Key: "GH_TOKEN", Description: "PAT"}}, false)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "experiment_disabled")

	rrGet := f.doUserJSON(t, http.MethodGet, "/api/v1/secret-requests/"+api.NewUUID(), f.alice, nil)
	require.Equal(t, http.StatusNotFound, rrGet.Code)

	rrList := f.doAgentJSON(t, http.MethodGet, "/api/v1/agent/secret-requests", nil)
	require.Equal(t, http.StatusNotFound, rrList.Code)

	rrWd := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/secret-requests/withdraw", WithdrawSecretRequestsRequest{Keys: []string{"GH_TOKEN"}})
	require.Equal(t, http.StatusNotFound, rrWd.Code)
}

// TestSecretRequests_EndToEnd covers send, versioning (--update), GET card,
// 403 authorization (non-recipient, missing agent.attach, UAT, agent caller),
// fulfill + value canary across rows/messages/audit/logs, idempotent replay,
// 409 conflicts, 503 rollback, dismiss, agent withdraw & list, sweeper,
// hard-delete withdraw with row retention, and project-delete removal.
func TestSecretRequests_EndToEnd(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// 1. Send v1 with 2 keys.
	rr1 := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "GITHUB_TOKEN", Description: "PAT for release"},
		{Key: "DEPLOY_KEY", Description: "SSH key", Type: "file", Target: "~/.ssh/id_ed25519"},
	}, false)
	require.Equal(t, http.StatusOK, rr1.Code, rr1.Body.String())

	var send1 struct {
		MessageID      string                    `json:"message_id"`
		SecretRequests []SecretRequestSendResult `json:"secret_requests"`
	}
	require.NoError(t, json.Unmarshal(rr1.Body.Bytes(), &send1))
	require.Len(t, send1.SecretRequests, 2)
	require.Equal(t, 1, send1.SecretRequests[0].Version)
	require.Nil(t, send1.SecretRequests[0].Replaced)

	// Re-sending pending key without --update returns 409 secret_request_key_pending with hint.
	rrDup := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "GITHUB_TOKEN", Description: "dup"},
	}, false)
	require.Equal(t, http.StatusConflict, rrDup.Code)
	require.Contains(t, rrDup.Body.String(), ErrCodeSecretRequestKeyPending)
	require.Contains(t, rrDup.Body.String(), "--update")

	// --update replaces ONLY GITHUB_TOKEN (v1 -> replaced, v2 created); DEPLOY_KEY stays pending.
	rrUpd := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "GITHUB_TOKEN", Description: "PAT v2"},
	}, true)
	require.Equal(t, http.StatusOK, rrUpd.Code, rrUpd.Body.String())
	var send2 struct {
		MessageID      string                    `json:"message_id"`
		SecretRequests []SecretRequestSendResult `json:"secret_requests"`
	}
	require.NoError(t, json.Unmarshal(rrUpd.Body.Bytes(), &send2))
	require.Len(t, send2.SecretRequests, 1)
	require.Equal(t, 2, send2.SecretRequests[0].Version)
	require.NotNil(t, send2.SecretRequests[0].Replaced)
	require.Equal(t, 1, send2.SecretRequests[0].Replaced.Version)

	// AC13: Send a 1-key card v1, replace it with --update v2, and assert both
	// fulfill and dismiss on the replaced v1 card return 409 replaced.
	rrAC13v1 := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "AC13_KEY", Description: "v1 to be replaced"},
	}, false)
	require.Equal(t, http.StatusOK, rrAC13v1.Code, rrAC13v1.Body.String())
	var ac13v1 struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrAC13v1.Body.Bytes(), &ac13v1))
	rrAC13v2 := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "AC13_KEY", Description: "v2 replacement"},
	}, true)
	require.Equal(t, http.StatusOK, rrAC13v2.Code, rrAC13v2.Body.String())

	rrReplFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+ac13v1.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{"AC13_KEY": "stale-v1-val"},
	})
	require.Equal(t, http.StatusConflict, rrReplFulfill.Code, rrReplFulfill.Body.String())
	require.Contains(t, rrReplFulfill.Body.String(), msgaction.ConflictReplaced)

	rrReplDismiss := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+ac13v1.MessageID+"/dismiss", f.alice, DismissSecretRequestBody{
		Reason: "stale v1 dismiss",
	})
	require.Equal(t, http.StatusConflict, rrReplDismiss.Code, rrReplDismiss.Body.String())
	require.Contains(t, rrReplDismiss.Body.String(), msgaction.ConflictReplaced)

	// Clean up AC13_KEY v2 by withdrawing it so step 9's expiry/audit counts stay exact.
	rrAC13WD := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/secret-requests/withdraw", WithdrawSecretRequestsRequest{
		Keys:   []string{"AC13_KEY"},
		Reason: "cleanup ac13 test",
	})
	require.Equal(t, http.StatusOK, rrAC13WD.Code, rrAC13WD.Body.String())

	// 2. GET card as recipient Alice (has agent.attach -> canRespond=true).
	rrCard := f.doUserJSON(t, http.MethodGet, "/api/v1/secret-requests/"+send2.MessageID, f.alice, nil)
	require.Equal(t, http.StatusOK, rrCard.Code, "body=%s audit=%+v", rrCard.Body.String(), f.audit.snapshot())
	var card SecretRequestCardResponse
	require.NoError(t, json.Unmarshal(rrCard.Body.Bytes(), &card))
	require.True(t, card.CanRespond)
	require.Len(t, card.Rows, 1)
	require.Equal(t, "GITHUB_TOKEN", card.Rows[0].Key)
	require.Equal(t, 2, card.Rows[0].Version)

	// 3. Authorization refusals (403):
	// 3a. Non-recipient Bob gets 403 on GET and fulfill.
	rrBobGet := f.doUserJSON(t, http.MethodGet, "/api/v1/secret-requests/"+send2.MessageID, f.bob, nil)
	require.Equal(t, http.StatusForbidden, rrBobGet.Code)
	rrBobFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/fulfill", f.bob, FulfillSecretRequestBody{
		Values: map[string]string{"GITHUB_TOKEN": "nope"},
	})
	require.Equal(t, http.StatusForbidden, rrBobFulfill.Code)

	// 3a2. Dave holds agent.attach (admin) on proj, but is NOT a
	// recipient of Alice's DM -> 403 + refused audit event with reason "not_recipient"
	// on BOTH fulfill and dismiss (R5).
	dave := &store.User{
		ID:          api.NewUUID(),
		Email:       "dave@example.com",
		DisplayName: "Dave",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	createTestUserWithRole(t, f.store, dave.ID, dave.Email, store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	require.True(t, f.srv.authzService.CheckAccess(ctx, NewAuthenticatedUser(dave.ID, dave.Email, dave.DisplayName, dave.Role, string(ClientTypeWeb)), agentResource(f.agent), ActionAttach).Allowed,
		"test invariant: Dave must hold agent.attach so the 403 proves the recipient guard")

	f.audit.reset()
	rrDaveFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/fulfill", dave, FulfillSecretRequestBody{
		Values: map[string]string{"GITHUB_TOKEN": "should-fail"},
	})
	require.Equal(t, http.StatusForbidden, rrDaveFulfill.Code)
	rrDaveDismiss := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/dismiss", dave, DismissSecretRequestBody{
		Reason: "nope",
	})
	require.Equal(t, http.StatusForbidden, rrDaveDismiss.Code)
	daveRefused := 0
	for _, ev := range f.audit.snapshot() {
		if ev.EventType == MessageActionEventRefused && ev.ActorID == dave.ID && ev.Reason == "not_recipient" {
			daveRefused++
		}
	}
	require.Equal(t, 2, daveRefused, "expected 2 not_recipient refused audit events for non-DM-party with agent.attach")

	// 3b. Agent caller gets 403 users_only on responder route.
	rrAgentFulfill := f.doAgentJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/fulfill", FulfillSecretRequestBody{
		Values: map[string]string{"GITHUB_TOKEN": "nope"},
	})
	require.Equal(t, http.StatusForbidden, rrAgentFulfill.Code)
	require.Contains(t, rrAgentFulfill.Body.String(), ErrCodeUsersOnly)

	// 3b2. User access token (UAT) is refused on GET, fulfill, and dismiss (session-only).
	uatIdent := NewScopedUserIdentity(
		NewAuthenticatedUser(f.alice.ID, f.alice.Email, f.alice.DisplayName, f.alice.Role, ""),
		f.project.ID,
		[]string{"agent:read", "agent:attach"},
	)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/secret-requests/" + send2.MessageID},
		{http.MethodPost, "/api/v1/secret-requests/" + send2.MessageID + "/fulfill"},
		{http.MethodPost, "/api/v1/secret-requests/" + send2.MessageID + "/dismiss"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(contextWithIdentity(r.Context(), uatIdent))
		rr := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rr, r)
		require.Equal(t, http.StatusForbidden, rr.Code, "UAT must be refused on %s %s: %s", tc.method, tc.path, rr.Body.String())
		require.Contains(t, rr.Body.String(), "session_required")
	}

	// 3c. Non-agent user gets 403 agents_only on agent route.
	rrUserAgentList := f.doUserJSON(t, http.MethodGet, "/api/v1/agent/secret-requests", f.alice, nil)
	require.Equal(t, http.StatusForbidden, rrUserAgentList.Code)
	require.Contains(t, rrUserAgentList.Body.String(), ErrCodeAgentsOnly)

	// 3d. Recipient Carol with agent.read but NO agent.attach sees canRespond=false on GET and gets 403 on fulfill/dismiss.
	rrCarolSend := f.sendSecretRequest(t, "user:"+f.carol.Email, []messages.SecretRequestInput{
		{Key: "CAROL_KEY", Description: "test"},
	}, false)
	require.Equal(t, http.StatusOK, rrCarolSend.Code, rrCarolSend.Body.String())
	var carolCard struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrCarolSend.Body.Bytes(), &carolCard))
	rrCarolGet := f.doUserJSON(t, http.MethodGet, "/api/v1/secret-requests/"+carolCard.MessageID, f.carol, nil)
	require.Equal(t, http.StatusOK, rrCarolGet.Code, "body=%s audit=%+v", rrCarolGet.Body.String(), f.audit.snapshot())
	var carolCardResp SecretRequestCardResponse
	require.NoError(t, json.Unmarshal(rrCarolGet.Body.Bytes(), &carolCardResp))
	require.False(t, carolCardResp.CanRespond)
	require.NotEmpty(t, carolCardResp.Reason)
	rrCarolFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+carolCard.MessageID+"/fulfill", f.carol, FulfillSecretRequestBody{
		Values: map[string]string{"CAROL_KEY": "val"},
	})
	require.Equal(t, http.StatusForbidden, rrCarolFulfill.Code)

	// 4. 503 rollback on a two-key card failing on the SECOND key (R5):
	// first key MUST be deleted from the backend (compensation) and both rows
	// MUST return to pending.
	rrRbSend := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "ROLLBACK_A", Description: "first key"},
		{Key: "ROLLBACK_B", Description: "second key (fails)"},
	}, false)
	require.Equal(t, http.StatusOK, rrRbSend.Code, rrRbSend.Body.String())
	var rbCard struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrRbSend.Body.Bytes(), &rbCard))

	f.backend.mu.Lock()
	f.backend.failOnKey = "ROLLBACK_B"
	f.backend.failErr = errors.New("simulated KMS outage on second key")
	f.backend.mu.Unlock()

	rrFail := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+rbCard.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{
			"ROLLBACK_A": "partial-value-must-be-deleted",
			"ROLLBACK_B": "fails-here",
		},
	})
	require.Equal(t, http.StatusServiceUnavailable, rrFail.Code)
	rowsAfterFail, err := f.store.GetSecretRequestsByMessageID(ctx, rbCard.MessageID)
	require.NoError(t, err)
	require.Len(t, rowsAfterFail, 2)
	require.Equal(t, store.SecretRequestStatusPending, rowsAfterFail[0].Status)
	require.Equal(t, store.SecretRequestStatusPending, rowsAfterFail[1].Status)
	_, getAErr := f.backend.Get(ctx, "ROLLBACK_A", store.ScopeAgent, f.agent.ID)
	require.ErrorIs(t, getAErr, store.ErrNotFound, "first key written before second-key failure must be compensated/deleted")

	f.backend.mu.Lock()
	f.backend.failOnKey = ""
	f.backend.failErr = nil
	f.backend.mu.Unlock()

	// 5. Fulfill with VALUE CANARY and verify canary appears ONLY in secret backend!
	const canary = "CANARY_SECRET_VALUE_9f8a7b6c5d4e3f2a1b"
	f.audit.reset()
	f.logBuf.Reset()

	rrFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values:         map[string]string{"GITHUB_TOKEN": canary},
		Surface:        msgaction.SecretRequestSurfaceStandalonePage,
		IdempotencyKey: "fulfill-idem-1",
	})
	require.Equal(t, http.StatusOK, rrFulfill.Code, rrFulfill.Body.String())
	require.NotContains(t, rrFulfill.Body.String(), canary, "fulfill response must never echo value")

	var fulResp FulfillSecretRequestResponse
	require.NoError(t, json.Unmarshal(rrFulfill.Body.Bytes(), &fulResp))
	require.Len(t, fulResp.Rows, 1)
	require.Equal(t, store.SecretRequestStatusFulfilled, fulResp.Rows[0].Status)
	require.False(t, fulResp.Replayed)

	// Canary MUST be stored in backend at agent scope.
	storedVal, err := f.backend.Get(ctx, "GITHUB_TOKEN", store.ScopeAgent, f.agent.ID)
	require.NoError(t, err)
	require.Equal(t, canary, storedVal.Value)

	// Canary MUST NOT appear in: secret_requests rows, messages, audit events, or hub logs.
	allRows, err := f.store.ListAgentSecretRequests(ctx, &store.ListAgentSecretRequestsOptions{
		AgentID: f.agent.ID,
		State:   store.SecretRequestListStateAll,
	})
	require.NoError(t, err)
	rowsJSON, _ := json.Marshal(allRows)
	require.NotContains(t, string(rowsJSON), canary, "canary leaked into secret_requests rows")

	require.NotEmpty(t, fulResp.ResponseMessageID)
	respMsg, merr := f.store.GetMessage(ctx, fulResp.ResponseMessageID)
	require.NoError(t, merr)
	require.Equal(t, "user:"+f.alice.Email, respMsg.Sender, "fulfill secret-response message must use userSenderAddress(user)")
	msgJSON, _ := json.Marshal(respMsg)
	require.NotContains(t, string(msgJSON), canary, "canary leaked into secret-response message")
	auditJSON, _ := json.Marshal(f.audit.snapshot())
	require.NotContains(t, string(auditJSON), canary, "canary leaked into audit events")
	require.NotContains(t, f.logBuf.String(), canary, "canary leaked into hub logs")

	// Canary MUST NOT appear in any notifications for the user or asking agent.
	userNotifs, nerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.alice.ID, false)
	require.NoError(t, nerr)
	userNotifsJSON, _ := json.Marshal(userNotifs)
	require.NotContains(t, string(userNotifsJSON), canary, "canary leaked into user notifications")
	agentNotifs, nerr := f.store.GetNotifications(ctx, store.SubscriberTypeAgent, f.agent.ID, false)
	require.NoError(t, nerr)
	agentNotifsJSON, _ := json.Marshal(agentNotifs)
	require.NotContains(t, string(agentNotifsJSON), canary, "canary leaked into agent notifications")

	// Also verify FulfillSecretRequestBody.LogValue() redacts Values.
	var lvBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&lvBuf, nil)).Info("probe", "body", FulfillSecretRequestBody{
		Values: map[string]string{"GITHUB_TOKEN": canary},
	})
	require.NotContains(t, lvBuf.String(), canary)
	require.Contains(t, lvBuf.String(), "[REDACTED]")

	// 6. Idempotent replay returns 200 replayed=true; second submit without key returns 409 already_fulfilled.
	rrReplay := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values:         map[string]string{"GITHUB_TOKEN": canary},
		Surface:        msgaction.SecretRequestSurfaceStandalonePage,
		IdempotencyKey: "fulfill-idem-1",
	})
	require.Equal(t, http.StatusOK, rrReplay.Code)
	var replayResp FulfillSecretRequestResponse
	require.NoError(t, json.Unmarshal(rrReplay.Body.Bytes(), &replayResp))
	require.True(t, replayResp.Replayed)

	rrSecond := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{"GITHUB_TOKEN": "other"},
	})
	require.Equal(t, http.StatusConflict, rrSecond.Code)
	require.Contains(t, rrSecond.Body.String(), msgaction.ConflictAlreadyFulfilled)

	// 7. Dismiss remaining pending DEPLOY_KEY on send1 with reason.
	rrDismiss := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send1.MessageID+"/dismiss", f.alice, DismissSecretRequestBody{
		Reason:  "use shared deploy key",
		Surface: msgaction.SecretRequestSurfaceNative,
	})
	require.Equal(t, http.StatusOK, rrDismiss.Code, rrDismiss.Body.String())
	var dsmResp DismissSecretRequestResponse
	require.NoError(t, json.Unmarshal(rrDismiss.Body.Bytes(), &dsmResp))
	require.Len(t, dsmResp.Dismissed, 1)
	require.Equal(t, "DEPLOY_KEY", dsmResp.Dismissed[0].Key)
	require.NotEmpty(t, dsmResp.ResponseMessageID)
	dsmMsg, dmerr := f.store.GetMessage(ctx, dsmResp.ResponseMessageID)
	require.NoError(t, dmerr)
	require.Equal(t, "user:"+f.alice.Email, dsmMsg.Sender, "dismiss secret-response message must use userSenderAddress(user)")

	// 8. Agent withdraw & list.
	rrWd := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/secret-requests/withdraw", WithdrawSecretRequestsRequest{
		Keys:   []string{"CAROL_KEY"},
		Reason: "no longer needed",
	})
	require.Equal(t, http.StatusOK, rrWd.Code, rrWd.Body.String())
	var wdResp WithdrawSecretRequestsResponse
	require.NoError(t, json.Unmarshal(rrWd.Body.Bytes(), &wdResp))
	require.Len(t, wdResp.Withdrawn, 1)

	// Repeat withdraw -> alreadyWithdrawn.
	rrWd2 := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/secret-requests/withdraw", WithdrawSecretRequestsRequest{
		Keys: []string{"CAROL_KEY"},
	})
	require.Equal(t, http.StatusOK, rrWd2.Code)
	var wdResp2 WithdrawSecretRequestsResponse
	require.NoError(t, json.Unmarshal(rrWd2.Body.Bytes(), &wdResp2))
	require.Len(t, wdResp2.AlreadyWithdrawn, 1)

	// List --all returns all rows with no values.
	rrListAll := f.doAgentJSON(t, http.MethodGet, "/api/v1/agent/secret-requests?state=all", nil)
	require.Equal(t, http.StatusOK, rrListAll.Code)
	var listResp AgentSecretRequestListResponse
	require.NoError(t, json.Unmarshal(rrListAll.Body.Bytes(), &listResp))
	require.GreaterOrEqual(t, len(listResp.Items), 4)

	// 9. Sweeper: stale claim recovery + expiry (with no agent message).
	rrExpSend := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "SWEEP_KEY", Description: "for sweep"},
	}, false)
	require.Equal(t, http.StatusOK, rrExpSend.Code)
	var sweepCard struct {
		MessageID      string                    `json:"message_id"`
		SecretRequests []SecretRequestSendResult `json:"secret_requests"`
	}
	require.NoError(t, json.Unmarshal(rrExpSend.Body.Bytes(), &sweepCard))
	claimNow := time.Now().UTC()
	_, err = f.store.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID: sweepCard.MessageID,
		RowIDs:    []string{sweepCard.SecretRequests[0].RowID},
		UserID:    f.alice.ID,
		Now:       claimNow,
	})
	require.NoError(t, err)

	expN, recN, err := f.srv.sweepSecretRequests(ctx, claimNow.Add(msgaction.SecretRequestClaimTTL+time.Second))
	require.NoError(t, err)
	require.Equal(t, 0, expN)
	require.Equal(t, 1, recN)

	expN, recN, err = f.srv.sweepSecretRequests(ctx, claimNow.Add(msgaction.SecretRequestTTL+time.Hour))
	require.NoError(t, err)
	require.Equal(t, 3, expN) // SWEEP_KEY + ROLLBACK_A + ROLLBACK_B from step 4
	require.Equal(t, 0, recN)

	// Assert audit event counts for fulfilled, dismissed, withdrawn, claim_recovered, expired (§10.15).
	evCounts := map[string]int{}
	for _, ev := range f.audit.snapshot() {
		evCounts[ev.EventType]++
	}
	require.Equal(t, 1, evCounts[msgaction.EventFulfilled], "expected 1 fulfilled audit event")
	require.Equal(t, 1, evCounts[MessageActionEventDismissed], "expected 1 dismissed audit event")
	require.Equal(t, 1, evCounts[MessageActionEventWithdrawn], "expected 1 agent-withdrawn audit event")
	require.Equal(t, 1, evCounts[msgaction.EventClaimRecovered], "expected 1 claim_recovered audit event")
	require.Equal(t, 3, evCounts[MessageActionEventExpired], "expected 3 expired audit events")

	// 10. Hard-delete withdraw via withAgentSecretCleanup retains rows and emits withdrawn audit after commit; project delete removes them.
	rrHD := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "HD_KEY", Description: "survives delete"},
	}, false)
	require.Equal(t, http.StatusOK, rrHD.Code)
	var hdCard struct {
		SecretRequests []SecretRequestSendResult `json:"secret_requests"`
	}
	require.NoError(t, json.Unmarshal(rrHD.Body.Bytes(), &hdCard))

	f.audit.reset()
	_, err = f.srv.withAgentSecretCleanup(ctx, f.agent.ID, func(txCtx context.Context) (int, error) {
		if txErr := f.store.WithTx(txCtx, func(tx store.Store) error {
			_, _, err := f.srv.agentHardDeleteWork(txCtx, tx, f.agent, AuditActor{}, store.Deactivation{
				Cause: store.EdgeDeactivationAgentHardDelete,
				OpID:  api.NewUUID(),
			}, "test hard delete")
			return err
		}); txErr != nil {
			return 0, txErr
		}
		return 1, nil
	})
	require.NoError(t, err)
	hdRow, err := f.store.GetSecretRequest(ctx, hdCard.SecretRequests[0].RowID)
	require.NoError(t, err, "secret request row must survive agent hard delete")
	require.Equal(t, store.SecretRequestStatusWithdrawn, hdRow.Status)
	require.Equal(t, "agent deleted", hdRow.WithdrawnReason)
	require.Equal(t, store.SecretRequestWithdrawnByHub, hdRow.WithdrawnBy)
	hdWithdrawn := 0
	for _, ev := range f.audit.snapshot() {
		if ev.EventType == MessageActionEventWithdrawn && ev.ActorKind == MessageActionActorHub && ev.Reason == "agent deleted" {
			hdWithdrawn++
		}
	}
	require.GreaterOrEqual(t, hdWithdrawn, 1, "expected post-commit withdrawn audit event on hard delete")

	// Project delete removes all secret request rows.
	require.NoError(t, f.store.DeleteProject(ctx, f.project.ID))
	_, err = f.store.GetSecretRequest(ctx, hdCard.SecretRequests[0].RowID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestSecretRequests_WriteValidationGuards verifies R3: send and fulfill
// enforce all agent-scope secret write constraints (System One reserved keys
// including _ -> - folding, file target absolute/~/ with no '..', reserved env
// targets via secret.IsReservedEnvTarget, internal secret mutation refusal, and
// 64 KiB value cap on fulfill).
func TestSecretRequests_WriteValidationGuards(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// 1. System One reserved key with _ -> - folding.
	rrSysOne := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "SystemOne_Instance_abc123", Description: "reserved"},
	}, false)
	require.Equal(t, http.StatusBadRequest, rrSysOne.Code, rrSysOne.Body.String())

	// 2. File target with '..'.
	rrDotDot := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "SSH_KEY", Description: "bad path", Type: "file", Target: "~/.ssh/../id_rsa"},
	}, false)
	require.Equal(t, http.StatusBadRequest, rrDotDot.Code, rrDotDot.Body.String())

	// 3. Relative file target and absolute target outside ~ (/etc/x) refused at send.
	for _, badTarget := range []string{"relative/id_rsa", "/etc/x"} {
		rrRel := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
			{Key: "SSH_KEY2", Description: "bad target", Type: "file", Target: badTarget},
		}, false)
		require.Equal(t, http.StatusBadRequest, rrRel.Code, "target=%s body=%s", badTarget, rrRel.Body.String())
	}

	// 4. Reserved env targets (SCION_* and GCE_METADATA_*).
	for _, reserved := range []string{"SCION_AUTH_TOKEN", "GCE_METADATA_HOST"} {
		rrRes := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
			{Key: reserved, Description: "reserved env", Type: messages.SecretRequestTypeEnvironment},
		}, false)
		require.Equal(t, http.StatusBadRequest, rrRes.Code, "key=%s body=%s", reserved, rrRes.Body.String())
	}

	// 5. Internal secret mutation refused (409 internal_secret).
	_, _, err := f.backend.Set(ctx, &secret.SetSecretInput{
		Name:       "INTERNAL_ONLY_KEY",
		Value:      "internal-val",
		SecretType: store.SecretTypeInternal,
		Target:     "INTERNAL_ONLY_KEY",
		Scope:      store.ScopeAgent,
		ScopeID:    f.agent.ID,
		CreatedBy:  f.alice.ID,
		ProjectID:  f.project.ID,
	})
	require.NoError(t, err)
	rrInternal := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "INTERNAL_ONLY_KEY", Description: "should refuse"},
	}, false)
	require.Equal(t, http.StatusConflict, rrInternal.Code, rrInternal.Body.String())
	require.Contains(t, rrInternal.Body.String(), "internal_secret")

	// 6. Fulfill enforces 64 KiB max value size.
	rrValid := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "BIG_KEY", Description: "size check"},
	}, false)
	require.Equal(t, http.StatusOK, rrValid.Code, rrValid.Body.String())
	var card struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrValid.Body.Bytes(), &card))

	rrBig := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+card.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{"BIG_KEY": strings.Repeat("x", maxSecretRequestValueBytes+1)},
	})
	require.Equal(t, http.StatusBadRequest, rrBig.Code, rrBig.Body.String())
	require.Contains(t, rrBig.Body.String(), "64KB")
}

// TestSecretRequests_DeletePurgeAndRollbackAudit verifies R4: withdrawn audit
// events for agent hard delete and retention purge are emitted strictly AFTER
// commit (exactly one per pending row), and ZERO are emitted on rollback.
func TestSecretRequests_DeletePurgeAndRollbackAudit(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// Create 2 pending requests on f.agent.
	rr := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "DEL_K1", Description: "k1"},
		{Key: "DEL_K2", Description: "k2"},
	}, false)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// 1. Rollback: tx withdraws rows then returns error -> 0 withdrawn events emitted, rows remain pending.
	f.audit.reset()
	_, err := f.srv.withAgentSecretCleanup(ctx, f.agent.ID, func(txCtx context.Context) (int, error) {
		txErr := f.store.WithTx(txCtx, func(tx store.Store) error {
			if _, werr := tx.WithdrawAgentSecretRequestsOnDelete(txCtx, f.agent.ID, time.Now().UTC()); werr != nil {
				return werr
			}
			return errors.New("simulated rollback")
		})
		return 0, txErr
	})
	require.Error(t, err)
	for _, ev := range f.audit.snapshot() {
		require.NotEqual(t, MessageActionEventWithdrawn, ev.EventType, "no withdrawn audit event may be emitted on rollback")
	}

	// 2. Hard delete commit: exactly 2 withdrawn events (one per pending row).
	f.audit.reset()
	_, err = f.srv.withAgentSecretCleanup(ctx, f.agent.ID, func(txCtx context.Context) (int, error) {
		if txErr := f.store.WithTx(txCtx, func(tx store.Store) error {
			_, _, herr := f.srv.agentHardDeleteWork(txCtx, tx, f.agent, AuditActor{}, store.Deactivation{
				Cause: store.EdgeDeactivationAgentHardDelete,
				OpID:  api.NewUUID(),
			}, "hard delete test")
			return herr
		}); txErr != nil {
			return 0, txErr
		}
		return 1, nil
	})
	require.NoError(t, err)
	wdCount := 0
	for _, ev := range f.audit.snapshot() {
		if ev.EventType == MessageActionEventWithdrawn && ev.ActorKind == MessageActionActorHub && ev.Reason == "agent deleted" {
			wdCount++
		}
	}
	require.Equal(t, 2, wdCount, "expected exactly 1 withdrawn audit event per pending row on hard delete")

	// 3. Retention purge commit: soft-deleted agent purged via PurgeDeletedAgentsByID -> exactly 1 withdrawn event per pending row.
	ag2 := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "sr-purge-agent",
		Name:      "sr-purge-agent",
		ProjectID: f.project.ID,
		OwnerID:   f.alice.ID,
		Phase:     "stopped",
	}
	require.NoError(t, f.store.CreateAgent(ctx, ag2))
	_, err = f.store.CreateSecretRequestPrompt(ctx, &store.SecretRequestPromptInput{
		Message: &store.Message{
			ID:          api.NewUUID(),
			ProjectID:   f.project.ID,
			Sender:      "agent:" + ag2.Slug,
			SenderID:    ag2.ID,
			Recipient:   "user:" + f.alice.Email,
			RecipientID: f.alice.ID,
		},
		AgentID:   ag2.ID,
		AgentName: ag2.Name,
		Requests: []messages.SecretRequest{
			{Key: "PURGE_K1", KeyNorm: "PURGE_K1", SecretType: "environment"},
		},
		Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	ag2.DeletedAt = time.Now().UTC().Add(-48 * time.Hour)
	require.NoError(t, f.store.UpdateAgent(ctx, ag2))

	f.audit.reset()
	n, err := f.srv.withAgentSecretCleanup(ctx, ag2.ID, func(txCtx context.Context) (int, error) {
		return f.store.PurgeDeletedAgentsByID(txCtx, []string{ag2.ID}, time.Now().UTC().Add(time.Minute))
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	purgeWD := 0
	for _, ev := range f.audit.snapshot() {
		if ev.EventType == MessageActionEventWithdrawn && ev.AgentID == ag2.ID && ev.ActorKind == MessageActionActorHub && ev.Reason == "agent deleted" {
			purgeWD++
		}
	}
	require.Equal(t, 1, purgeWD, "expected exactly 1 withdrawn audit event per pending row on retention purge")
}

// TestSecretRequests_StaleClaimRecoveryDoesNotClobberSecondFulfill verifies R1:
// when a first responder's backend.Set blocks past SecretRequestClaimTTL, the
// sweeper recovers the claim, a second responder claims and finishes, and the
// first responder's Set then returns and hits a finish CAS conflict, the first
// responder MUST NOT delete the second responder's stored secret.
func TestSecretRequests_StaleClaimRecoveryDoesNotClobberSecondFulfill(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// Create a second user (dave) with agent.attach and a shared group
	// conversation with alice, dave, and the asking agent so both users are
	// eligible responders.
	dave := &store.User{
		ID:          api.NewUUID(),
		Email:       "dave-race@example.com",
		DisplayName: "Dave Race",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	createTestUserWithRole(t, f.store, dave.ID, dave.Email, store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	require.True(t, f.srv.authzService.CheckAccess(ctx, NewAuthenticatedUser(dave.ID, dave.Email, dave.DisplayName, dave.Role, string(ClientTypeWeb)), agentResource(f.agent), ActionAttach).Allowed,
		"test invariant: Dave must hold agent.attach")

	projID := f.project.ID
	conv := &store.Conversation{
		ID:         api.NewUUID(),
		ProjectID:  &projID,
		Kind:       "group",
		Surface:    "native",
		DriftState: "active",
	}
	require.NoError(t, f.store.CreateConversation(ctx, conv))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    f.alice.ID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    dave.ID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    f.agent.ID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}))

	// Send a 1-key secret request into the shared group conversation.
	body, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		Msg:             "Need RACE_KEY",
		SecretRequests: []messages.SecretRequestInput{
			{Key: "RACE_KEY", Description: "shared race test"},
		},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), f.agent))
	rrSend := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rrSend, req)
	require.Equal(t, http.StatusOK, rrSend.Code, rrSend.Body.String())

	var card struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrSend.Body.Bytes(), &card))
	require.NotEmpty(t, card.MessageID)

	// Block Alice's first backend.Set right after it stores, before FinishSecretRequestFulfill.
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	f.backend.mu.Lock()
	f.backend.afterSet = func(_ context.Context, input *secret.SetSecretInput) {
		if input != nil && input.Value == "alice-stale-value" {
			firstOnce.Do(func() {
				close(firstEntered)
				<-releaseFirst
			})
		}
	}
	f.backend.mu.Unlock()

	aliceDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		aliceDone <- f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+card.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
			Values: map[string]string{"RACE_KEY": "alice-stale-value"},
		})
	}()

	// Wait until Alice has claimed and completed backend.Set (and is now blocked before Finish).
	select {
	case <-firstEntered:
	case rr := <-aliceDone:
		t.Fatalf("Alice returned before backend.Set: status=%d body=%s", rr.Code, rr.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Alice's backend.Set")
	}

	// Run sweeper past SecretRequestClaimTTL to recover Alice's stale claim.
	expN, recN, err := f.srv.sweepSecretRequests(ctx, time.Now().UTC().Add(msgaction.SecretRequestClaimTTL+time.Second))
	require.NoError(t, err)
	require.Equal(t, 0, expN)
	require.Equal(t, 1, recN)

	// Second responder (Dave) claims and finishes with his value.
	rrDave := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+card.MessageID+"/fulfill", dave, FulfillSecretRequestBody{
		Values: map[string]string{"RACE_KEY": "dave-winning-value"},
	})
	require.Equal(t, http.StatusOK, rrDave.Code, rrDave.Body.String())

	// Release Alice's blocked Set so Alice hits FinishSecretRequestFulfill and conflicts.
	close(releaseFirst)
	var rrAlice *httptest.ResponseRecorder
	select {
	case rrAlice = <-aliceDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Alice's fulfill response")
	}
	require.Equal(t, http.StatusConflict, rrAlice.Code, rrAlice.Body.String())
	require.Contains(t, rrAlice.Body.String(), msgaction.ConflictAlreadyFulfilled)

	// Critical R1 assertion: Dave's stored secret MUST still be present in the backend.
	got, err := f.backend.Get(ctx, "RACE_KEY", store.ScopeAgent, f.agent.ID)
	require.NoError(t, err, "first responder's finish-conflict handler must not delete second responder's stored secret")
	require.Equal(t, "dave-winning-value", got.Value)
}

// TestSecretRequests_FulfillRollbackPreservesExistingSecrets verifies design
// item 3: when a multi-key fulfill fails on a later key (503), rollback deletes
// only keys newly created by that call and NEVER deletes a secret that already
// existed in the backend before the call (protecting working secrets on rotation).
func TestSecretRequests_FulfillRollbackPreservesExistingSecrets(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// Pre-populate existing secret K1 at agent scope.
	_, _, err := f.backend.Set(ctx, &secret.SetSecretInput{
		Name:          "ROT_K1",
		Value:         "existing-working-secret",
		SecretType:    store.SecretTypeEnvironment,
		Target:        "ROT_K1",
		Scope:         store.ScopeAgent,
		ScopeID:       f.agent.ID,
		InjectionMode: store.InjectionModeAsNeeded,
		CreatedBy:     f.alice.ID,
		UpdatedBy:     f.alice.ID,
		ProjectID:     f.project.ID,
	})
	require.NoError(t, err)

	// Send a 3-key card: ROT_K1 (existing), ROT_K2 (new), ROT_K3 (will fail).
	rrSend := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "ROT_K1", Description: "existing key being rotated"},
		{Key: "ROT_K2", Description: "brand new key"},
		{Key: "ROT_K3", Description: "key that fails backend write"},
	}, false)
	require.Equal(t, http.StatusOK, rrSend.Code, rrSend.Body.String())
	var card struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrSend.Body.Bytes(), &card))

	f.backend.mu.Lock()
	f.backend.failOnKey = "ROT_K3"
	f.backend.failErr = errors.New("simulated third-key failure")
	f.backend.mu.Unlock()

	rrFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+card.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{
			"ROT_K1": "rotated-value",
			"ROT_K2": "new-value-must-be-deleted",
			"ROT_K3": "fails-here",
		},
	})
	require.Equal(t, http.StatusServiceUnavailable, rrFulfill.Code, rrFulfill.Body.String())

	// Pre-existing ROT_K1 MUST still exist in the backend (never wiped on rollback).
	k1, err := f.backend.Get(ctx, "ROT_K1", store.ScopeAgent, f.agent.ID)
	require.NoError(t, err, "pre-existing secret ROT_K1 must not be deleted by 503 rollback")
	require.NotEmpty(t, k1.Value)

	// Newly created ROT_K2 MUST be gone (deleted by 503 rollback).
	_, err = f.backend.Get(ctx, "ROT_K2", store.ScopeAgent, f.agent.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "newly created secret ROT_K2 must be deleted by 503 rollback")

	// All 3 rows must be rolled back to pending.
	rows, err := f.store.GetSecretRequestsByMessageID(ctx, card.MessageID)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for _, r := range rows {
		require.Equal(t, store.SecretRequestStatusPending, r.Status)
	}
}

// TestSecretRequests_HardDeleteRacingFulfillDeletesRotatedKey verifies AC7:
// when an agent hard-delete withdraws a fulfilling row while fulfill is in
// backend.Set, the hub-withdrawn finish conflict deletes EVERY key written in
// that call — including keys that already existed before the rotation — because
// the agent is gone.
func TestSecretRequests_HardDeleteRacingFulfillDeletesRotatedKey(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// Pre-populate an existing secret at agent scope (rotation scenario).
	_, _, err := f.backend.Set(ctx, &secret.SetSecretInput{
		Name:          "HD_RACE_K1",
		Value:         "old-value",
		SecretType:    store.SecretTypeEnvironment,
		Target:        "HD_RACE_K1",
		Scope:         store.ScopeAgent,
		ScopeID:       f.agent.ID,
		InjectionMode: store.InjectionModeAsNeeded,
		CreatedBy:     f.alice.ID,
		UpdatedBy:     f.alice.ID,
		ProjectID:     f.project.ID,
	})
	require.NoError(t, err)

	rrSend := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "HD_RACE_K1", Description: "rotating key raced by hard delete"},
	}, false)
	require.Equal(t, http.StatusOK, rrSend.Code, rrSend.Body.String())
	var card struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrSend.Body.Bytes(), &card))

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.backend.mu.Lock()
	f.backend.afterSet = func(_ context.Context, input *secret.SetSecretInput) {
		if input != nil && input.Name == "HD_RACE_K1" {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	f.backend.mu.Unlock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+card.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
			Values: map[string]string{"HD_RACE_K1": "rotated-value"},
		})
	}()

	select {
	case <-entered:
	case rr := <-done:
		t.Fatalf("fulfill returned before backend.Set: status=%d body=%s", rr.Code, rr.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backend.Set")
	}

	// Simulate agent hard-delete withdrawing the fulfilling row as hub.
	wd, err := f.store.WithdrawAgentSecretRequestsOnDelete(ctx, f.agent.ID, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, wd, 1)

	close(release)
	var rrFulfill *httptest.ResponseRecorder
	select {
	case rrFulfill = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fulfill response")
	}
	require.Equal(t, http.StatusConflict, rrFulfill.Code, rrFulfill.Body.String())
	require.Contains(t, rrFulfill.Body.String(), msgaction.ConflictWithdrawn)

	// AC7: even though HD_RACE_K1 existed before the rotation, the agent is gone
	// so hub-withdrawn conflict compensation MUST delete it from the backend.
	_, err = f.backend.Get(ctx, "HD_RACE_K1", store.ScopeAgent, f.agent.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "hub-withdrawn finish conflict must delete every key written in the call")
}

// TestSecretRequests_FulfillAbortsOnGetMetaError verifies that a non-ErrNotFound
// backend.GetMeta error aborts fulfill with 503 BEFORE calling backend.Set and
// rolls back the claim to pending.
func TestSecretRequests_FulfillAbortsOnGetMetaError(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	rrSend := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "META_ERR_KEY", Description: "getmeta failure"},
	}, false)
	require.Equal(t, http.StatusOK, rrSend.Code, rrSend.Body.String())
	var card struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrSend.Body.Bytes(), &card))

	f.backend.mu.Lock()
	f.backend.getMetaErrKey = "META_ERR_KEY"
	f.backend.getMetaErr = errors.New("backend unavailable")
	// Call 1 is refuseInternalSecretMutation during pre-claim validation; fail on
	// call 2 (inside Step 4 right before backend.Set) to verify 503 + claim rollback.
	f.backend.getMetaFailOnCall = 2
	f.backend.getMetaCalls = 0
	f.backend.setCalls = 0
	f.backend.mu.Unlock()

	rrFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+card.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{"META_ERR_KEY": "must-not-be-written"},
	})
	require.Equal(t, http.StatusServiceUnavailable, rrFulfill.Code, rrFulfill.Body.String())

	f.backend.mu.Lock()
	calls := f.backend.setCalls
	f.backend.mu.Unlock()
	require.Equal(t, 0, calls, "backend.Set must not be called when GetMeta returns a non-ErrNotFound error")

	rows, err := f.store.GetSecretRequestsByMessageID(ctx, card.MessageID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, store.SecretRequestStatusPending, rows[0].Status)
}

// TestSecretRequests_FulfillRevalidatesWriteConstraints verifies that
// handleSecretRequestFulfill re-runs validateAgentSecretWriteConstraints before
// claiming or writing to the backend: if an internal secret with the same key
// is created after the request was sent, fulfill must refuse it (403) without
// overwriting the internal secret. Removing the validateAgentSecretWriteConstraints
// call in handleSecretRequestFulfill makes this test fail.
func TestSecretRequests_FulfillRevalidatesWriteConstraints(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// 1. Send a valid request while no internal secret with this key exists.
	rrSend := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "REVAL_INTERNAL_KEY", Description: "valid at send time"},
	}, false)
	require.Equal(t, http.StatusOK, rrSend.Code, rrSend.Body.String())
	var card struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrSend.Body.Bytes(), &card))

	// 2. Create an internal agent-scoped secret with the same key AFTER send.
	_, _, err := f.backend.Set(ctx, &secret.SetSecretInput{
		Name:          "REVAL_INTERNAL_KEY",
		Value:         "internal-do-not-overwrite",
		SecretType:    store.SecretTypeInternal,
		Target:        "REVAL_INTERNAL_KEY",
		Scope:         store.ScopeAgent,
		ScopeID:       f.agent.ID,
		InjectionMode: store.InjectionModeAsNeeded,
		CreatedBy:     "system",
		UpdatedBy:     "system",
		ProjectID:     f.project.ID,
	})
	require.NoError(t, err)

	// 3. Fulfill must be refused by fulfill-side re-validation (409 internal_secret)
	// and must not overwrite the internal secret or transition the row.
	rrFulfill := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+card.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{"REVAL_INTERNAL_KEY": "attempted-overwrite"},
	})
	require.Equal(t, http.StatusConflict, rrFulfill.Code, rrFulfill.Body.String())
	require.Contains(t, rrFulfill.Body.String(), "internal_secret")

	got, err := f.backend.Get(ctx, "REVAL_INTERNAL_KEY", store.ScopeAgent, f.agent.ID)
	require.NoError(t, err)
	require.Equal(t, "internal-do-not-overwrite", got.Value, "internal secret must not be overwritten")

	rows, err := f.store.GetSecretRequestsByMessageID(ctx, card.MessageID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, store.SecretRequestStatusPending, rows[0].Status)
}
