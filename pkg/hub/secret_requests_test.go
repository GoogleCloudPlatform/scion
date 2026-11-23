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
	mu        sync.Mutex
	failOnKey string
	failErr   error
}

func (f *failingSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	f.mu.Lock()
	failKey := f.failOnKey
	failErr := f.failErr
	f.mu.Unlock()
	if failKey != "" && input != nil && strings.EqualFold(input.Name, failKey) {
		return false, nil, failErr
	}
	return f.SecretBackend.Set(ctx, input)
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

	// 4. 503 rollback: backend Set failure leaves row pending and no partial keys stored.
	f.backend.mu.Lock()
	f.backend.failOnKey = "GITHUB_TOKEN"
	f.backend.failErr = errors.New("simulated KMS outage")
	f.backend.mu.Unlock()

	rrFail := f.doUserJSON(t, http.MethodPost, "/api/v1/secret-requests/"+send2.MessageID+"/fulfill", f.alice, FulfillSecretRequestBody{
		Values: map[string]string{"GITHUB_TOKEN": "should-not-stick"},
	})
	require.Equal(t, http.StatusServiceUnavailable, rrFail.Code)
	rowsAfterFail, err := f.store.GetSecretRequestsByMessageID(ctx, send2.MessageID)
	require.NoError(t, err)
	require.Equal(t, store.SecretRequestStatusPending, rowsAfterFail[0].Status)

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

	if fulResp.ResponseMessageID != "" {
		respMsg, merr := f.store.GetMessage(ctx, fulResp.ResponseMessageID)
		require.NoError(t, merr)
		msgJSON, _ := json.Marshal(respMsg)
		require.NotContains(t, string(msgJSON), canary, "canary leaked into secret-response message")
	}
	auditJSON, _ := json.Marshal(f.audit.snapshot())
	require.NotContains(t, string(auditJSON), canary, "canary leaked into audit events")
	require.NotContains(t, f.logBuf.String(), canary, "canary leaked into hub logs")

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
	require.Equal(t, 1, expN)
	require.Equal(t, 0, recN)

	// 10. Hard-delete withdraw retains rows; project delete removes them.
	rrHD := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "HD_KEY", Description: "survives delete"},
	}, false)
	require.Equal(t, http.StatusOK, rrHD.Code)
	var hdCard struct {
		SecretRequests []SecretRequestSendResult `json:"secret_requests"`
	}
	require.NoError(t, json.Unmarshal(rrHD.Body.Bytes(), &hdCard))

	require.NoError(t, f.store.WithTx(ctx, func(tx store.Store) error {
		_, _, err := f.srv.agentHardDeleteWork(ctx, tx, f.agent, AuditActor{}, store.Deactivation{
			Cause: store.EdgeDeactivationAgentHardDelete,
			OpID:  api.NewUUID(),
		}, "test hard delete")
		return err
	}))
	hdRow, err := f.store.GetSecretRequest(ctx, hdCard.SecretRequests[0].RowID)
	require.NoError(t, err, "secret request row must survive agent hard delete")
	require.Equal(t, store.SecretRequestStatusWithdrawn, hdRow.Status)
	require.Equal(t, "agent deleted", hdRow.WithdrawnReason)
	require.Equal(t, store.SecretRequestWithdrawnByHub, hdRow.WithdrawnBy)

	// Project delete removes all secret request rows.
	require.NoError(t, f.store.DeleteProject(ctx, f.project.ID))
	_, err = f.store.GetSecretRequest(ctx, hdCard.SecretRequests[0].RowID)
	require.ErrorIs(t, err, store.ErrNotFound)
}
