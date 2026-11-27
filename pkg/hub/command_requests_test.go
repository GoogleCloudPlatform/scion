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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type commandRequestsFixture struct {
	srv       *Server
	store     store.Store
	project   *store.Project
	agent     *store.Agent
	execAgent *store.Agent
	alice     *store.User
	bob       *store.User
	audit     *srAuditLogger
	logBuf    *bytes.Buffer
	launched  []*store.CommandRun
	launchMu  sync.Mutex
}

func newCommandRequestsFixture(t *testing.T, enableExp bool) *commandRequestsFixture {
	t.Helper()
	srv, st := testServer(t)
	ctx := context.Background()

	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(testLogger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	audit := &srAuditLogger{LogAuditLogger: &LogAuditLogger{prefix: "[Audit]", log: testLogger}}
	srv.auditLogger = audit
	srv.messageLog = testLogger

	if enableExp {
		raw := json.RawMessage(`{"overrides":{"hub.command_requests":true,"hub.exec_containers":true}}`)
		_, err := st.UpsertHubSetting(ctx, "experiments", raw, "test", -1, "managed")
		require.NoError(t, err)
		ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		srv.SetOperationalSettings(ops)
	}

	proj := &store.Project{
		ID:   api.NewUUID(),
		Slug: "cr-proj-" + api.NewUUID()[:8],
		Name: "Command Project",
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
	grantProjectAccessOnly(t, st, alice.ID, proj.ID)

	bob := &store.User{
		ID:          api.NewUUID(),
		Email:       "bob@example.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, bob))

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "cr-asker",
		Name:      "cr-asker",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
	}
	require.NoError(t, st.CreateAgent(ctx, agent))

	execAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "alice-shell",
		Name:      "alice-shell",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "shell",
		},
	}
	require.NoError(t, st.CreateAgent(ctx, execAgent))

	f := &commandRequestsFixture{
		srv:       srv,
		store:     st,
		project:   proj,
		agent:     agent,
		execAgent: execAgent,
		alice:     alice,
		bob:       bob,
		audit:     audit,
		logBuf:    &logBuf,
	}
	srv.SetCommandRunLauncher(CommandRunLauncherFunc(func(_ *store.CommandRequest, run *store.CommandRun) {
		f.launchMu.Lock()
		f.launched = append(f.launched, run)
		f.launchMu.Unlock()
	}))
	return f
}

func (f *commandRequestsFixture) doAgentJSON(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rr, req)
	return rr
}

func (f *commandRequestsFixture) doUserJSON(t *testing.T, method, path string, u *store.User, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	ident := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")
	ctx := contextWithCredentialContext(
		contextWithIdentity(req.Context(), ident),
		CredentialContext{Kind: CredentialKindInteractive, ID: u.ID},
	)
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rr, req)
	return rr
}

func (f *commandRequestsFixture) doBearerJSON(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestCommandRequests_ExperimentOff(t *testing.T) {
	f := newCommandRequestsFixture(t, false)

	rrSend := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		CommandRequest: &messages.CommandRequestInput{
			Key:    "k1",
			Script: "echo hi",
		},
	})
	assert.Equal(t, http.StatusBadRequest, rrSend.Code)
	assert.Contains(t, rrSend.Body.String(), ErrCodeExperimentDisabled)

	rrGet := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+api.NewUUID(), f.alice, nil)
	assert.Equal(t, http.StatusNotFound, rrGet.Code)

	rrApprove := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+api.NewUUID()+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: messages.CommandRequestCanonicalHash("echo hi", "", 600),
	})
	assert.Equal(t, http.StatusNotFound, rrApprove.Code)

	rrOutput := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+api.NewUUID()+"/output", f.alice, nil)
	assert.Equal(t, http.StatusNotFound, rrOutput.Code)
	rrDismiss := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+api.NewUUID()+"/dismiss", f.alice, messages.DismissCommandRequestBody{
		Reason: "no",
	})
	assert.Equal(t, http.StatusNotFound, rrDismiss.Code)

	rrList := f.doAgentJSON(t, http.MethodGet, "/api/v1/agent/command-requests", nil)
	assert.Equal(t, http.StatusNotFound, rrList.Code)

	rrWithdraw := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", messages.WithdrawCommandRequestBody{
		Key: "k1",
	})
	assert.Equal(t, http.StatusNotFound, rrWithdraw.Code)
}

func TestCommandRequests_EndToEnd(t *testing.T) {
	f := newCommandRequestsFixture(t, true)
	ctx := context.Background()

	// 1. Bidi control character rejected at send (S3).
	rrBidi := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		CommandRequest: &messages.CommandRequestInput{
			Key:    "bad-bidi",
			Script: "echo \u202ehi",
		},
	})
	assert.Equal(t, http.StatusBadRequest, rrBidi.Code)

	// 2. Conversation target refused in P1 (DM only).
	rrConv := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient:       "user:" + f.alice.Email,
		ConversationRef: "conv:" + api.NewUUID(),
		CommandRequest: &messages.CommandRequestInput{
			Key:    "conv-not-allowed",
			Script: "echo hi",
		},
	})
	assert.Equal(t, http.StatusBadRequest, rrConv.Code)

	// 3. Valid DM send v1 (F1).
	secretMarker := "UNIQUE_SCRIPT_MARKER_NEVER_IN_LOGS_9281"
	rrSend1 := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Please run this check",
		CommandRequest: &messages.CommandRequestInput{
			Key:         "gcloud-check",
			Description: "Inspect active gcloud config",
			Script:      "echo " + secretMarker,
		},
	})
	require.Equal(t, http.StatusOK, rrSend1.Code, rrSend1.Body.String())
	var sendResp1 struct {
		MessageID      string                             `json:"message_id"`
		CommandRequest *messages.CommandRequestSendResult `json:"command_request"`
	}
	require.NoError(t, json.Unmarshal(rrSend1.Body.Bytes(), &sendResp1))
	require.NotNil(t, sendResp1.CommandRequest)
	assert.Equal(t, "gcloud-check", sendResp1.CommandRequest.Key)
	assert.Equal(t, 1, sendResp1.CommandRequest.Version)
	v1ID := sendResp1.CommandRequest.RowID
	v1Hash := sendResp1.CommandRequest.CommandSHA256

	// 4. Repeat send without --update -> 409 command_request_key_pending.
	rrDup := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		CommandRequest: &messages.CommandRequestInput{
			Key:    "gcloud-check",
			Script: "echo v2",
		},
	})
	assert.Equal(t, http.StatusConflict, rrDup.Code)
	assert.Contains(t, rrDup.Body.String(), msgaction.ConflictCmdKeyPending)

	// 5. --update creates v2 and replaces v1 (S3).
	rrSend2 := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient:            "user:" + f.alice.Email,
		CommandRequestUpdate: true,
		CommandRequest: &messages.CommandRequestInput{
			Key:         "gcloud-check",
			Description: "Inspect active gcloud config v2",
			Script:      "echo " + secretMarker + " v2",
		},
	})
	require.Equal(t, http.StatusOK, rrSend2.Code, rrSend2.Body.String())
	var sendResp2 struct {
		CommandRequest *messages.CommandRequestSendResult `json:"command_request"`
	}
	require.NoError(t, json.Unmarshal(rrSend2.Body.Bytes(), &sendResp2))
	require.NotNil(t, sendResp2.CommandRequest)
	assert.Equal(t, 2, sendResp2.CommandRequest.Version)
	v2ID := sendResp2.CommandRequest.RowID
	v2Hash := sendResp2.CommandRequest.CommandSHA256

	// 6. Security S1 + S8 (R3): real agent JWT through Handler(), real PAT
	// through Handler(), visitor identity, federated identity, and non-approver
	// on BOTH GET and POST /approve -> 403 + refused audit event with expected reason.
	assertRefusedReason := func(t *testing.T, label, wantReason string) {
		t.Helper()
		found := false
		for _, ev := range f.audit.snapshot() {
			if ev.Kind == msgaction.KindCommandRequest && ev.EventType == MessageActionEventRefused && ev.Reason == wantReason {
				found = true
				break
			}
		}
		assert.True(t, found, "%s: expected refused audit event with reason %q", label, wantReason)
	}

	// 6a. Real agent JWT through Handler() on GET and POST /approve.
	agentJWT, err := f.srv.GenerateAgentToken(f.agent.ID, f.project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/command-requests/" + v2ID, nil},
		{http.MethodPost, "/api/v1/command-requests/" + v2ID + "/approve", messages.ApproveCommandRequestBody{CommandSHA256: v2Hash}},
	} {
		f.audit.reset()
		rr := f.doBearerJSON(t, tc.method, tc.path, agentJWT, tc.body)
		assert.Equal(t, http.StatusForbidden, rr.Code, "agent JWT %s %s: %s", tc.method, tc.path, rr.Body.String())
		assertRefusedReason(t, "agent JWT "+tc.method, ErrCodeUsersOnly)
	}

	// 6b. Real UAT/PAT through Handler() on GET and POST /approve.
	patPresented := "scion_pat_cmdreq-s1-test-token"
	patHash := sha256.Sum256([]byte(patPresented))
	patExpiry := time.Now().Add(time.Hour)
	require.NoError(t, f.store.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID:        api.NewUUID(),
		UserID:    f.alice.ID,
		Name:      "cmdreq-s1-pat",
		Prefix:    "scion_pat_cmd1",
		KeyHash:   hex.EncodeToString(patHash[:]),
		ProjectID: f.project.ID,
		Scopes:    []string{"project:read", "agent:attach"},
		ExpiresAt: &patExpiry,
		Created:   time.Now(),
	}))
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/command-requests/" + v2ID, nil},
		{http.MethodPost, "/api/v1/command-requests/" + v2ID + "/approve", messages.ApproveCommandRequestBody{CommandSHA256: v2Hash}},
	} {
		f.audit.reset()
		rr := f.doBearerJSON(t, tc.method, tc.path, patPresented, tc.body)
		assert.Equal(t, http.StatusForbidden, rr.Code, "real PAT %s %s: %s", tc.method, tc.path, rr.Body.String())
		assertRefusedReason(t, "real PAT "+tc.method, "session_required")
	}

	// 6c. Visitor identity on GET and POST /approve: refused with 403 by the
	// route guard (mux) and, directly on the handler, by messageActionHumanSessionCaller
	// with a refused audit event (reason: visitor_refused).
	visitorIdent := NewVisitorIdentity(api.NewUUID(), "https://issuer.cmdreq.test", "cmdreq-visitor", "visitor@cmdreq.test", "Visitor")
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/command-requests/" + v2ID, ""},
		{http.MethodPost, "/api/v1/command-requests/" + v2ID + "/approve", `{"command_sha256":"` + v2Hash + `"}`},
	} {
		reqMux := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		reqMux = reqMux.WithContext(contextWithCredentialContext(
			contextWithIdentity(reqMux.Context(), visitorIdent),
			CredentialContext{Kind: CredentialKindInteractive, ID: visitorIdent.ID()},
		))
		rrMux := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rrMux, reqMux)
		assert.Equal(t, http.StatusForbidden, rrMux.Code, "visitor mux %s %s: %s", tc.method, tc.path, rrMux.Body.String())

		f.audit.reset()
		reqDirect := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		reqDirect = reqDirect.WithContext(contextWithCredentialContext(
			contextWithIdentity(reqDirect.Context(), visitorIdent),
			CredentialContext{Kind: CredentialKindInteractive, ID: visitorIdent.ID()},
		))
		rrDirect := httptest.NewRecorder()
		f.srv.handleCommandRequestRoutes(rrDirect, reqDirect)
		assert.Equal(t, http.StatusForbidden, rrDirect.Code, "visitor direct %s %s: %s", tc.method, tc.path, rrDirect.Body.String())
		assertRefusedReason(t, "visitor "+tc.method, "visitor_refused")
	}

	// 6d. Federated user identity on GET and POST /approve.
	fedIdent := NewFederatedUserIdentity("https://issuer.cmdreq.test", "cmdreq-fed", f.alice.Email, "Fed Alice", store.UserRoleMember, nil)
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/command-requests/" + v2ID, ""},
		{http.MethodPost, "/api/v1/command-requests/" + v2ID + "/approve", `{"command_sha256":"` + v2Hash + `"}`},
	} {
		f.audit.reset()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req = req.WithContext(contextWithCredentialContext(
			contextWithIdentity(req.Context(), fedIdent),
			CredentialContext{Kind: CredentialKindFederation, ID: fedIdent.ID()},
		))
		rr := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code, "federated %s %s: %s", tc.method, tc.path, rr.Body.String())
		assertRefusedReason(t, "federated "+tc.method, "session_required")
	}

	// 6e. Non-recipient / non-approver (bob) on GET (403 forbidden) and POST /approve (403 not_approver).
	f.audit.reset()
	rrBobGet := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+v2ID, f.bob, nil)
	assert.Equal(t, http.StatusForbidden, rrBobGet.Code)
	assertRefusedReason(t, "non-recipient GET", ErrCodeForbidden)

	f.audit.reset()
	rrBob := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", f.bob, messages.ApproveCommandRequestBody{
		CommandSHA256: v2Hash,
	})
	assert.Equal(t, http.StatusForbidden, rrBob.Code)
	assert.Contains(t, rrBob.Body.String(), msgaction.ConflictCmdNotApprover)
	assertRefusedReason(t, "non-approver approve", msgaction.ConflictCmdNotApprover)

	// 7. Unconfigured exec container -> 409 exec_container_unconfigured.
	rrUnconf := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: v2Hash,
	})
	assert.Equal(t, http.StatusConflict, rrUnconf.Code)
	assert.Contains(t, rrUnconf.Body.String(), msgaction.ConflictCmdExecUnconfigured)

	// Register alice's execution container.
	_, err = f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
	require.NoError(t, err)

	// 8. Stopped execution container -> 409 exec_container_stopped, row stays pending (D5).
	f.execAgent.Phase = string(state.PhaseStopped)
	require.NoError(t, f.store.UpdateAgent(ctx, f.execAgent))
	rrStopped := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: v2Hash,
	})
	assert.Equal(t, http.StatusConflict, rrStopped.Code)
	assert.Contains(t, rrStopped.Body.String(), msgaction.ConflictCmdExecStopped)
	v2Row, err := f.store.GetCommandRequest(ctx, v2ID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRequestStatusPending, v2Row.Status)

	// Start container -> ready.
	f.execAgent.Phase = string(state.PhaseRunning)
	require.NoError(t, f.store.UpdateAgent(ctx, f.execAgent))

	// 9. Approving replaced v1 -> 409 replaced (S3).
	rrOld := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+v1ID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: v1Hash,
	})
	assert.Equal(t, http.StatusConflict, rrOld.Code)
	assert.Contains(t, rrOld.Body.String(), msgaction.ConflictReplaced)

	// 10. Mismatched hash -> 409 command_hash_mismatch (S3).
	rrBadHash := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: strings.Repeat("0", 64),
	})
	assert.Equal(t, http.StatusConflict, rrBadHash.Code)
	assert.Contains(t, rrBadHash.Body.String(), msgaction.ConflictCmdHashMismatch)

	// 11. §12 Q5 proof: CLI login token (ClientTypeCLI) classifies as CredentialKindInteractive
	// and succeeds on GET and POST /approve through the full auth middleware!
	cliToken, _, _, err := f.srv.userTokenService.GenerateTokenPair(
		f.alice.ID, f.alice.Email, f.alice.DisplayName, f.alice.Role, ClientTypeCLI,
	)
	require.NoError(t, err)

	rrGetCLI := f.doBearerJSON(t, http.MethodGet, "/api/v1/command-requests/"+v2ID, cliToken, nil)
	require.Equal(t, http.StatusOK, rrGetCLI.Code, rrGetCLI.Body.String())
	var detail messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrGetCLI.Body.Bytes(), &detail))
	assert.True(t, detail.CanApprove)
	require.NotNil(t, detail.ExecContainer)
	assert.Equal(t, "ready", detail.ExecContainer.State)
	assert.Equal(t, f.execAgent.ID, detail.ExecContainer.AgentID)

	rrApproveCLI := f.doBearerJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", cliToken, messages.ApproveCommandRequestBody{
		RowID:          v2ID,
		CommandSHA256:  v2Hash,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "cli-idem-1",
	})
	require.Equal(t, http.StatusAccepted, rrApproveCLI.Code, rrApproveCLI.Body.String())
	var appResp messages.ApproveCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrApproveCLI.Body.Bytes(), &appResp))
	assert.Equal(t, store.CommandRequestStatusApproved, appResp.Request.Status)
	assert.Equal(t, store.CommandRunStatusQueued, appResp.Run.Status)
	assert.False(t, appResp.Run.ShareOutput)

	f.launchMu.Lock()
	require.Len(t, f.launched, 1)
	assert.Equal(t, appResp.Run.RunID, f.launched[0].ID)
	launchedRun := f.launched[0]
	f.launchMu.Unlock()

	// I3: approver GET retains execAgentId/execProjectId, while SSE-published view strips them.
	rrGetAfterApprove := f.doBearerJSON(t, http.MethodGet, "/api/v1/command-requests/"+v2ID, cliToken, nil)
	require.Equal(t, http.StatusOK, rrGetAfterApprove.Code, rrGetAfterApprove.Body.String())
	var detailAfter messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrGetAfterApprove.Body.Bytes(), &detailAfter))
	require.NotNil(t, detailAfter.Run)
	assert.Equal(t, f.execAgent.ID, detailAfter.Run.ExecAgentID)
	assert.Equal(t, f.project.ID, detailAfter.Run.ExecProjectID)

	storedReq, err := f.store.GetCommandRequest(ctx, v2ID)
	require.NoError(t, err)
	sseView := commandRequestSSEView(storedReq, nil, launchedRun)
	require.NotNil(t, sseView.Run)
	assert.Empty(t, sseView.Run.ExecAgentID, "SSE view must omit execAgentId (I3)")
	assert.Empty(t, sseView.Run.ExecProjectID, "SSE view must omit execProjectId (I3)")

	// 12. Idempotent replay returns 200 and does NOT re-launch.
	rrReplay := f.doBearerJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", cliToken, messages.ApproveCommandRequestBody{
		RowID:          v2ID,
		CommandSHA256:  v2Hash,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "cli-idem-1",
	})
	require.Equal(t, http.StatusOK, rrReplay.Code)
	var replayResp messages.ApproveCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrReplay.Body.Bytes(), &replayResp))
	assert.True(t, replayResp.Replayed)
	f.launchMu.Lock()
	assert.Len(t, f.launched, 1)
	f.launchMu.Unlock()

	// 13. Second approve with different idempotency key -> 409 already_resolved.
	rrSecond := f.doBearerJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", cliToken, messages.ApproveCommandRequestBody{
		RowID:          v2ID,
		CommandSHA256:  v2Hash,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "cli-idem-2",
	})
	assert.Equal(t, http.StatusConflict, rrSecond.Code)
	assert.Contains(t, rrSecond.Body.String(), msgaction.ConflictAlreadyResolved)

	// 14. Agent list returns its own requests with run status; human user gets 403 agents_only.
	rrList := f.doAgentJSON(t, http.MethodGet, "/api/v1/agent/command-requests?key=gcloud-check", nil)
	require.Equal(t, http.StatusOK, rrList.Code)
	var listResp messages.AgentCommandRequestListResponse
	require.NoError(t, json.Unmarshal(rrList.Body.Bytes(), &listResp))
	require.Len(t, listResp.Items, 2)
	assert.Equal(t, v2ID, listResp.Items[0].RowID)
	require.NotNil(t, listResp.Items[0].Run)
	assert.Equal(t, store.CommandRunStatusQueued, listResp.Items[0].Run.Status)
	// N2: agent view omits the human's execution container IDs.
	assert.Empty(t, listResp.Items[0].Run.ExecAgentID)
	assert.Empty(t, listResp.Items[0].Run.ExecProjectID)

	rrUserList := f.doUserJSON(t, http.MethodGet, "/api/v1/agent/command-requests", f.alice, nil)
	assert.Equal(t, http.StatusForbidden, rrUserList.Code)

	// 15. S12: approved audit event has allow-listed details and script text never appears in audit or slog.
	var foundApproved bool
	for _, ev := range f.audit.snapshot() {
		if ev.Kind == msgaction.KindCommandRequest && ev.EventType == msgaction.CommandRequestResolvedStatus {
			foundApproved = true
			assert.Equal(t, v2Hash, ev.Details["command_sha256"])
			assert.Equal(t, f.execAgent.ID, ev.Details["exec_agent_id"])
			assert.Equal(t, appResp.Run.RunID, ev.Details["run_id"])
		}
	}
	assert.True(t, foundApproved, "expected approved audit event")
	assert.NotContains(t, f.logBuf.String(), secretMarker, "script text must never appear in audit or slog")

	// 16. S11: CSRF protection rejects cross-site browser POST to /approve with 403.
	csrfReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve",
		strings.NewReader(`{"command_sha256":"`+v2Hash+`"}`))
	csrfReq.Header.Set("Content-Type", "application/json")
	csrfReq.Header.Set("Authorization", "Bearer "+cliToken)
	csrfReq.Header.Set("Origin", "https://evil.example")
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	rrCSRF := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rrCSRF, csrfReq)
	assert.Equal(t, http.StatusForbidden, rrCSRF.Code)
}

// TestCommandRequests_S12_NoScriptInLogsAcrossDeliveryPaths verifies Review 2
// R1 (G7, T15, AC-S12): script text never appears in slog.Default() or the
// dedicated message log across the broker stored-first path, the broker
// early-return/persist-failure path, and the direct path, while the stored
// message retains the Markdown fallback body.
func TestCommandRequests_S12_NoScriptInLogsAcrossDeliveryPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("broker stored-first path", func(t *testing.T) {
		f := newCommandRequestsFixture(t, true)
		var msgLogBuf bytes.Buffer
		msgLogger := slog.New(slog.NewJSONHandler(&msgLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		f.srv.messageLog = msgLogger

		bus := eventbus.NewInProcessEventBus(slog.Default())
		t.Cleanup(func() { _ = bus.Close() })
		events := NewChannelEventPublisher()
		bp := NewMessageBrokerProxy(bus, f.store, events, func() AgentDispatcher { return f.srv.GetDispatcher() }, slog.Default())
		bp.messageLog = msgLogger
		f.srv.SetMessageBrokerProxy(bp)

		marker := "BROKER_STORED_FIRST_SCRIPT_MARKER_9281"
		rr := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Broker path intro",
			CommandRequest: &messages.CommandRequestInput{
				Key:         "broker-s12",
				Description: "Broker path script check",
				Script:      "echo " + marker,
			},
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var sendResp struct {
			MessageID string `json:"message_id"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &sendResp))
		require.NotEmpty(t, sendResp.MessageID)

		// Wait briefly for the async in-process broker subscriber to log delivery.
		require.Eventually(t, func() bool {
			return strings.Contains(msgLogBuf.String(), "user message delivered via broker")
		}, 2*time.Second, 10*time.Millisecond, "expected broker delivery log line; got: %s", msgLogBuf.String())

		assert.NotContains(t, f.logBuf.String(), marker, "script text must never appear in slog.Default() on broker path")
		assert.NotContains(t, msgLogBuf.String(), marker, "script text must never appear in message log on broker path")
		assert.Contains(t, msgLogBuf.String(), `"message_content_omitted":true`)

		// Stored message keeps the Markdown fallback body with the script.
		stored, err := f.store.GetMessage(ctx, sendResp.MessageID)
		require.NoError(t, err)
		assert.Contains(t, stored.Msg, marker)
	})

	t.Run("broker early-return persist-failure path", func(t *testing.T) {
		f := newCommandRequestsFixture(t, true)
		var msgLogBuf bytes.Buffer
		msgLogger := slog.New(slog.NewJSONHandler(&msgLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		bus := eventbus.NewInProcessEventBus(slog.Default())
		t.Cleanup(func() { _ = bus.Close() })
		events := NewChannelEventPublisher()
		// Real store with a canceled context forces deliverToUser through the
		// early-return/persist-failure log path (messagebroker.go:705).
		bp := NewMessageBrokerProxy(bus, f.store, events, func() AgentDispatcher { return nil }, slog.Default())
		bp.messageLog = msgLogger

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()

		marker := "BROKER_EARLY_RETURN_SCRIPT_MARKER_9281"
		bp.deliverToUser(canceledCtx, f.project.ID, f.alice.ID, &messages.StructuredMessage{
			Version:     messages.Version,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Sender:      "agent:" + f.agent.Slug,
			SenderID:    f.agent.ID,
			Recipient:   "user:" + f.alice.Email,
			RecipientID: f.alice.ID,
			Type:        messages.TypeCommandRequest,
			Msg:         "```sh\necho " + marker + "\n```",
		})

		assert.Contains(t, msgLogBuf.String(), "user message handed to delivery")
		assert.Contains(t, msgLogBuf.String(), `"message_content_omitted":true`)
		assert.NotContains(t, f.logBuf.String(), marker, "script text must never appear in slog.Default() on early-return path")
		assert.NotContains(t, msgLogBuf.String(), marker, "script text must never appear in message log on early-return path")
	})

	t.Run("direct path", func(t *testing.T) {
		f := newCommandRequestsFixture(t, true)
		var msgLogBuf bytes.Buffer
		f.srv.messageLog = slog.New(slog.NewJSONHandler(&msgLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		marker := "DIRECT_PATH_SCRIPT_MARKER_9281"
		rr := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			CommandRequest: &messages.CommandRequestInput{
				Key:    "direct-s12",
				Script: "echo " + marker,
			},
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var sendResp struct {
			MessageID string `json:"message_id"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &sendResp))

		assert.NotContains(t, f.logBuf.String(), marker, "script text must never appear in slog.Default() on direct path")
		assert.NotContains(t, msgLogBuf.String(), marker, "script text must never appear in message log on direct path")

		stored, err := f.store.GetMessage(ctx, sendResp.MessageID)
		require.NoError(t, err)
		assert.Contains(t, stored.Msg, marker)
	})
}

func TestCommandRequestGet_MessageIDFallbackAndDetailEnrichment(t *testing.T) {
	f := newCommandRequestsFixture(t, true)
	ctx := context.Background()

	// Register Alice's execution container.
	_, err := f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
	require.NoError(t, err)

	// Send v1 with an intro and a /workspace workdir.
	rrSend1 := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Please run this **deploy**",
		CommandRequest: &messages.CommandRequestInput{
			Key:         "deploy-p3",
			Description: "Deploy preview",
			Script:      "echo v1\n",
			Workdir:     "/workspace/app",
		},
	})
	require.Equal(t, http.StatusOK, rrSend1.Code, rrSend1.Body.String())
	var send1 struct {
		MessageID      string                            `json:"message_id"`
		CommandRequest messages.CommandRequestSendResult `json:"command_request"`
	}
	require.NoError(t, json.Unmarshal(rrSend1.Body.Bytes(), &send1))
	require.NotEmpty(t, send1.MessageID)
	require.NotEmpty(t, send1.CommandRequest.RowID)

	// GET by message_id falls back to GetCommandRequestByMessageID and returns
	// enriched metadata (workdirKind, intro, projectName, approverName).
	rrGetByMsg := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+send1.MessageID, f.alice, nil)
	require.Equal(t, http.StatusOK, rrGetByMsg.Code, rrGetByMsg.Body.String())
	var detail1 messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrGetByMsg.Body.Bytes(), &detail1))
	assert.Equal(t, send1.CommandRequest.RowID, detail1.Request.RowID)
	assert.Equal(t, messages.CommandWorkdirKindWorkspace, detail1.Request.WorkdirKind)
	assert.Equal(t, "Please run this **deploy**", detail1.Intro)
	assert.Equal(t, "Command Project", detail1.ProjectName)
	assert.Equal(t, "Alice", detail1.ApproverName)
	assert.Nil(t, detail1.PreviousVersionUnknown)
	require.NotNil(t, detail1.ExecContainer)
	assert.False(t, detail1.ExecContainer.CanWake, "canWake is false when execution container is not suspended")

	// O3: negative authorization checks on the message-ID lookup path.
	// 1. Non-recipient (bob) GET by messageId -> 403 with no request/script/exec_container fields.
	rrNonRecip := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+send1.MessageID, f.bob, nil)
	assert.Equal(t, http.StatusForbidden, rrNonRecip.Code)
	assert.NotContains(t, rrNonRecip.Body.String(), "echo v1")
	assert.NotContains(t, rrNonRecip.Body.String(), "exec_container")
	assert.NotContains(t, rrNonRecip.Body.String(), send1.CommandRequest.RowID)

	// 2. POST approve with messageId (not rowId) -> 404 (approve never falls back to messageId).
	rrApproveByMsg := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+send1.MessageID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256:  send1.CommandRequest.CommandSHA256,
		Surface:        msgaction.CommandRequestSurfaceNative,
		IdempotencyKey: "p3-o3-msg-approve",
	})
	assert.Equal(t, http.StatusNotFound, rrApproveByMsg.Code)

	// 3. Recipient non-approver GET by messageId -> 200 with canApprove=false, reason=not_approver, no exec_container.
	projID := f.project.ID
	conv := &store.Conversation{
		ID:         api.NewUUID(),
		ProjectID:  &projID,
		Kind:       "group",
		Surface:    "native",
		DriftState: "active",
	}
	require.NoError(t, f.store.CreateConversation(ctx, conv))
	for _, p := range []*store.ConversationParticipant{
		{ID: api.NewUUID(), ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: f.alice.ID, Role: "member", JoinedAt: time.Now().UTC()},
		{ID: api.NewUUID(), ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: f.bob.ID, Role: "member", JoinedAt: time.Now().UTC()},
		{ID: api.NewUUID(), ConversationID: conv.ID, PrincipalKind: "agent", PrincipalID: f.agent.ID, Role: "member", JoinedAt: time.Now().UTC()},
	} {
		require.NoError(t, f.store.AddParticipant(ctx, p))
	}
	groupMsg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: conv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + conv.ID,
		Type:           messages.TypeCommandRequest,
		CreatedAt:      time.Now().UTC(),
	}
	normReq, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:    "group-o3",
		Script: "echo group\n",
	})
	require.NoError(t, err)
	groupPrompt, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message:     groupMsg,
		ProjectName: f.project.Name,
		AgentID:     f.agent.ID,
		AgentName:   f.agent.Name,
		ApproverID:  f.alice.ID,
		Request:     normReq,
	})
	require.NoError(t, err)
	rrRecipNonApprover := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+groupPrompt.Message.ID, f.bob, nil)
	require.Equal(t, http.StatusOK, rrRecipNonApprover.Code, rrRecipNonApprover.Body.String())
	var detailBob messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrRecipNonApprover.Body.Bytes(), &detailBob))
	assert.Equal(t, groupPrompt.Request.ID, detailBob.Request.RowID)
	assert.False(t, detailBob.CanApprove)
	assert.Equal(t, msgaction.ConflictCmdNotApprover, detailBob.Reason)
	assert.Nil(t, detailBob.ExecContainer, "O3: recipient non-approver must never receive exec_container")

	// Approve v1 and mark its run as unknown, then re-propose v2 -> D7 banner field populated.
	rrApprove := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+send1.CommandRequest.RowID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		RowID:          send1.CommandRequest.RowID,
		CommandSHA256:  send1.CommandRequest.CommandSHA256,
		Surface:        msgaction.CommandRequestSurfaceNative,
		IdempotencyKey: "p3-d7-test-1",
	})
	require.Equal(t, http.StatusAccepted, rrApprove.Code, rrApprove.Body.String())
	var approveResp messages.ApproveCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrApprove.Body.Bytes(), &approveResp))

	won, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:      approveResp.Run.RunID,
		Status:     store.CommandRunStatusUnknown,
		Reason:     "timeout",
		FinishedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, won)

	// Re-propose v2 (RepostTerminalOK=true).
	rrSend2 := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		CommandRequest: &messages.CommandRequestInput{
			Key:         "deploy-p3",
			Description: "Retry deploy",
			Script:      "echo v2\n",
		},
	})
	require.Equal(t, http.StatusOK, rrSend2.Code, rrSend2.Body.String())
	var send2 struct {
		CommandRequest messages.CommandRequestSendResult `json:"command_request"`
	}
	require.NoError(t, json.Unmarshal(rrSend2.Body.Bytes(), &send2))
	require.Equal(t, 2, send2.CommandRequest.Version)

	rrGet2 := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+send2.CommandRequest.RowID, f.alice, nil)
	require.Equal(t, http.StatusOK, rrGet2.Code, rrGet2.Body.String())
	var detail2 messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrGet2.Body.Bytes(), &detail2))
	require.NotNil(t, detail2.PreviousVersionUnknown, "D7: v2 after unknown v1 must populate previousVersionUnknown")
	assert.Equal(t, 1, detail2.PreviousVersionUnknown.Version)
	assert.Equal(t, send1.CommandRequest.RowID, detail2.PreviousVersionUnknown.RowID)
	assert.Equal(t, approveResp.Run.RunID, detail2.PreviousVersionUnknown.RunID)
}

func TestCommandRequests_FromMessage_EndToEnd(t *testing.T) {
	ctx := context.Background()

	// 1. 404 while hub.command_requests is off.
	fOff := newCommandRequestsFixture(t, false)
	rrOff := fOff.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", fOff.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     api.NewUUID(),
		Script:        "echo hi\n",
		CommandSHA256: messages.CommandRequestCanonicalHash("echo hi\n", "", messages.DefaultCommandRequestTimeoutS),
	})
	assert.Equal(t, http.StatusNotFound, rrOff.Code)

	// 2. Experiment ON fixture.
	f := newCommandRequestsFixture(t, true)
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.alice.ID)
	require.NoError(t, err)

	script := "gcloud config list\n"
	hash := messages.CommandRequestCanonicalHash(script, "", messages.DefaultCommandRequestTimeoutS)

	dmMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     f.project.ID,
		ThreadID:      dmKey,
		Sender:        "agent:" + f.agent.Slug,
		SenderID:      f.agent.ID,
		Recipient:     "user:" + f.alice.Email,
		RecipientID:   f.alice.ID,
		Msg:           "Please run this:\n\n```sh\n" + script + "```\n",
		Type:          "instruction",
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, dmMsg))

	// 3. Non-session callers (agent JWT, real PAT, visitor, federated) -> 403 + refused audit (R3).
	assertFromMsgRefusedReason := func(t *testing.T, label, wantReason string) {
		t.Helper()
		for _, ev := range f.audit.snapshot() {
			if ev.Kind == msgaction.KindCommandRequest && ev.EventType == MessageActionEventRefused && ev.Reason == wantReason {
				return
			}
		}
		t.Errorf("%s: expected refused audit event with reason %q, got %+v", label, wantReason, f.audit.snapshot())
	}
	fromMsgBody := messages.CreateCommandRequestFromMessageBody{
		MessageID:     dmMsg.ID,
		Script:        script,
		CommandSHA256: hash,
	}

	// 3a. Real agent JWT through Handler().
	agentJWT, agentCred, err := f.srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID:   f.agent.ID,
		ProjectID: f.project.ID,
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentTokenRefresh},
	}, "run-frommsg-r3")
	require.NoError(t, err)
	_, err = f.store.SetAgentRunID(ctx, f.agent.ID, "run-frommsg-r3", agentCred)
	require.NoError(t, err)
	f.audit.reset()
	rrAgentJWT := f.doBearerJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", agentJWT, fromMsgBody)
	assert.Equal(t, http.StatusForbidden, rrAgentJWT.Code, "agent JWT: %s", rrAgentJWT.Body.String())
	assertFromMsgRefusedReason(t, "agent JWT from-message", ErrCodeUsersOnly)

	// 3b. Real PAT through Handler().
	patPresented := "scion_pat_cmdreq-frommsg-r3-token"
	patHash := sha256.Sum256([]byte(patPresented))
	patExpiry := time.Now().Add(time.Hour)
	require.NoError(t, f.store.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID:        api.NewUUID(),
		UserID:    f.alice.ID,
		Name:      "cmdreq-frommsg-r3-pat",
		Prefix:    "scion_pat_frm1",
		KeyHash:   hex.EncodeToString(patHash[:]),
		ProjectID: f.project.ID,
		Scopes:    []string{"project:read", "agent:attach"},
		ExpiresAt: &patExpiry,
		Created:   time.Now(),
	}))
	f.audit.reset()
	rrPAT := f.doBearerJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", patPresented, fromMsgBody)
	assert.Equal(t, http.StatusForbidden, rrPAT.Code, "real PAT: %s", rrPAT.Body.String())
	assertFromMsgRefusedReason(t, "real PAT from-message", "session_required")

	// 3c. Visitor identity (mux + direct handler with visitor_refused audit).
	visitorIdent := NewVisitorIdentity(api.NewUUID(), "https://issuer.cmdreq.test", "cmdreq-visitor-r3", "v@example.com", "Visitor")
	visitorBytes, _ := json.Marshal(fromMsgBody)
	visitorReqMux := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/from-message", bytes.NewReader(visitorBytes))
	visitorReqMux = visitorReqMux.WithContext(contextWithCredentialContext(
		contextWithIdentity(visitorReqMux.Context(), visitorIdent),
		CredentialContext{Kind: CredentialKindInteractive, ID: visitorIdent.ID()},
	))
	visitorReqMux.Header.Set("Content-Type", "application/json")
	visitorRRMux := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(visitorRRMux, visitorReqMux)
	assert.Equal(t, http.StatusForbidden, visitorRRMux.Code)

	f.audit.reset()
	visitorReqDirect := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/from-message", bytes.NewReader(visitorBytes))
	visitorReqDirect = visitorReqDirect.WithContext(contextWithCredentialContext(
		contextWithIdentity(visitorReqDirect.Context(), visitorIdent),
		CredentialContext{Kind: CredentialKindInteractive, ID: visitorIdent.ID()},
	))
	visitorReqDirect.Header.Set("Content-Type", "application/json")
	visitorRRDirect := httptest.NewRecorder()
	f.srv.handleCommandRequestRoutes(visitorRRDirect, visitorReqDirect)
	assert.Equal(t, http.StatusForbidden, visitorRRDirect.Code)
	assertFromMsgRefusedReason(t, "visitor from-message", "visitor_refused")

	// 3d. Federated identity (403 + session_required audit).
	fedIdent := NewFederatedUserIdentity("https://issuer.cmdreq.test", "cmdreq-fed-r3", f.alice.Email, "Fed Alice", store.UserRoleMember, nil)
	f.audit.reset()
	fedReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/from-message", bytes.NewReader(visitorBytes))
	fedReq = fedReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(fedReq.Context(), fedIdent),
		CredentialContext{Kind: CredentialKindFederation, ID: fedIdent.ID()},
	))
	fedReq.Header.Set("Content-Type", "application/json")
	fedRR := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(fedRR, fedReq)
	assert.Equal(t, http.StatusForbidden, fedRR.Code)
	assertFromMsgRefusedReason(t, "federated from-message", "session_required")

	// 3e. Script validation 400 emits a refused audit event (O8).
	badScript := "echo \u202ehi\n"
	f.audit.reset()
	rrBadScript := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     dmMsg.ID,
		Script:        badScript,
		CommandSHA256: messages.CommandRequestCanonicalHash(badScript, "", messages.DefaultCommandRequestTimeoutS),
	})
	assert.Equal(t, http.StatusBadRequest, rrBadScript.Code)
	assertFromMsgRefusedReason(t, "script validation 400", ErrCodeValidationError)

	// 4. Non-recipient (bob) on alice's DM -> 403 not_approver.
	rrBob := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.bob, messages.CreateCommandRequestFromMessageBody{
		MessageID:     dmMsg.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrBob.Code)
	assert.Contains(t, rrBob.Body.String(), msgaction.ConflictCmdNotApprover)

	// 5. Edited bytes -> 422 markdown_block_not_found.
	editedScript := "gcloud config list --all\n"
	rrEdited := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     dmMsg.ID,
		Script:        editedScript,
		CommandSHA256: messages.CommandRequestCanonicalHash(editedScript, "", messages.DefaultCommandRequestTimeoutS),
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rrEdited.Code)
	assert.Contains(t, rrEdited.Body.String(), msgaction.ConflictCmdMarkdownBlock)

	// 6. Hash mismatch -> 409 command_hash_mismatch.
	rrBadHash := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     dmMsg.ID,
		Script:        script,
		CommandSHA256: strings.Repeat("0", 64),
	})
	assert.Equal(t, http.StatusConflict, rrBadHash.Code)
	assert.Contains(t, rrBadHash.Body.String(), msgaction.ConflictCmdHashMismatch)

	// 7. Human-authored message -> 422 markdown_block_not_found.
	humanMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     f.project.ID,
		ThreadID:      dmKey,
		Sender:        "user:" + f.alice.Email,
		SenderID:      f.alice.ID,
		Recipient:     "agent:" + f.agent.Slug,
		RecipientID:   f.agent.ID,
		Msg:           "```sh\n" + script + "```\n",
		Type:          "instruction",
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, humanMsg))
	rrHuman := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     humanMsg.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rrHuman.Code)
	assert.Contains(t, rrHuman.Body.String(), msgaction.ConflictCmdMarkdownBlock)

	// 8. no-execute block, nested quote, indented fence -> 422 markdown_block_not_found.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no_execute", "```sh no-execute\n" + script + "```\n"},
		{"nested_quote", "> ```sh\n> " + script + "> ```\n"},
		{"indented_fence", "  ```sh\n  " + script + "  ```\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &store.Message{
				ID:            api.NewUUID(),
				ProjectID:     f.project.ID,
				ThreadID:      dmKey,
				Sender:        "agent:" + f.agent.Slug,
				SenderID:      f.agent.ID,
				Recipient:     "user:" + f.alice.Email,
				RecipientID:   f.alice.ID,
				Msg:           tc.body,
				Type:          "instruction",
				DispatchState: store.MessageDispatchDispatched,
				CreatedAt:     time.Now().UTC(),
			}
			require.NoError(t, f.store.CreateMessage(ctx, m))
			rr := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
				MessageID:     m.ID,
				Script:        script,
				CommandSHA256: hash,
			})
			assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
			assert.Contains(t, rr.Body.String(), msgaction.ConflictCmdMarkdownBlock)
		})
	}

	// 9. Message older than 24 h -> 409 expired.
	oldMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     f.project.ID,
		ThreadID:      dmKey,
		Sender:        "agent:" + f.agent.Slug,
		SenderID:      f.agent.ID,
		Recipient:     "user:" + f.alice.Email,
		RecipientID:   f.alice.ID,
		Msg:           "```sh\n" + script + "```\n",
		Type:          "instruction",
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now().UTC().Add(-25 * time.Hour),
	}
	require.NoError(t, f.store.CreateMessage(ctx, oldMsg))
	rrOld := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     oldMsg.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusConflict, rrOld.Code)
	assert.Contains(t, rrOld.Body.String(), msgaction.ConflictExpired)

	// 10. Structured command-request message -> 422 markdown_block_not_found.
	cardMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     f.project.ID,
		ThreadID:      dmKey,
		Sender:        "agent:" + f.agent.Slug,
		SenderID:      f.agent.ID,
		Recipient:     "user:" + f.alice.Email,
		RecipientID:   f.alice.ID,
		Msg:           "```sh\n" + script + "```\n",
		Type:          messages.TypeCommandRequest,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, cardMsg))
	rrCard := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     cardMsg.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rrCard.Code)

	// 11. Unconfigured exec container -> 409 exec_container_unconfigured.
	rrUnconf := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     dmMsg.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusConflict, rrUnconf.Code)
	assert.Contains(t, rrUnconf.Body.String(), msgaction.ConflictCmdExecUnconfigured)

	// Configure alice's execution container (ready).
	_, err = f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
	require.NoError(t, err)

	// 12. Happy path DM Execute -> 202 Accepted, v1 approved, surface=markdown_block, run queued.
	rrOK := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:      dmMsg.ID,
		Script:         script,
		CommandSHA256:  hash,
		IdempotencyKey: "idem-p6-1",
	})
	require.Equal(t, http.StatusAccepted, rrOK.Code, rrOK.Body.String())
	var okResp messages.ApproveCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrOK.Body.Bytes(), &okResp))
	assert.Equal(t, messages.MarkdownCommandRequestKey(dmMsg.ID, hash), okResp.Request.Key)
	assert.Equal(t, 1, okResp.Request.Version)
	assert.Equal(t, store.CommandRequestStatusApproved, okResp.Request.Status)
	require.NotNil(t, okResp.Request.Approved)
	assert.Equal(t, msgaction.CommandRequestSurfaceMarkdownBlock, okResp.Request.Approved.Surface)
	assert.False(t, okResp.Request.ShareOutputRequested)
	assert.Equal(t, store.CommandRunStatusQueued, okResp.Run.Status)
	launchedCount := func() int {
		f.launchMu.Lock()
		defer f.launchMu.Unlock()
		return len(f.launched)
	}

	assert.Equal(t, 1, launchedCount())

	// 13. Same idempotency_key -> 200 replayed, no second launch.
	rrReplay := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:      dmMsg.ID,
		Script:         script,
		CommandSHA256:  hash,
		IdempotencyKey: "idem-p6-1",
	})
	require.Equal(t, http.StatusOK, rrReplay.Code)
	assert.Equal(t, 1, launchedCount())

	// 14. Second Execute (different idempotency_key) -> 409 already_resolved with existing run.
	rrDup := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:      dmMsg.ID,
		Script:         script,
		CommandSHA256:  hash,
		IdempotencyKey: "idem-p6-2",
	})
	require.Equal(t, http.StatusConflict, rrDup.Code)
	assert.Contains(t, rrDup.Body.String(), msgaction.ConflictAlreadyResolved)
	assert.Contains(t, rrDup.Body.String(), okResp.Run.RunID)
	assert.Equal(t, 1, launchedCount())

	// 15. Group conversation: unaddressed -> 403; multi-human @-mention -> 403; single human @-mention -> 202.
	grantProjectAccessOnly(t, f.store, f.bob.ID, f.project.ID)
	groupProjID := f.project.ID
	groupConv := &store.Conversation{
		ID:         api.NewUUID(),
		ProjectID:  &groupProjID,
		Kind:       "group",
		Surface:    "native",
		DriftState: "active",
	}
	require.NoError(t, f.store.CreateConversation(ctx, groupConv))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: groupConv.ID,
		PrincipalKind:  "user",
		PrincipalID:    f.alice.ID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: groupConv.ID,
		PrincipalKind:  "user",
		PrincipalID:    f.bob.ID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: groupConv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    f.agent.ID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}))

	groupUnaddressed := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: groupConv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + groupConv.ID,
		Msg:            "Anyone can run:\n```sh\n" + script + "```\n",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, groupUnaddressed))
	rrGrpNone := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupUnaddressed.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrGrpNone.Code)

	groupBoth := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: groupConv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + groupConv.ID,
		Msg:            "@alice@example.com @bob@example.com please check:\n```sh\n" + script + "```\n",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, groupBoth))
	rrGrpBoth := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupBoth.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrGrpBoth.Code)

	groupAliceOnly := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: groupConv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + groupConv.ID,
		Msg:            "@alice@example.com please run:\n```sh\n" + script + "```\n",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, groupAliceOnly))
	// Bob is not the addressed human -> 403.
	rrGrpBob := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.bob, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupAliceOnly.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrGrpBob.Code)
	// Alice is the single addressed human -> 202.
	rrGrpAlice := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupAliceOnly.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusAccepted, rrGrpAlice.Code, rrGrpAlice.Body.String())

	// 16. R2: canonical mention resolution — email local part, display name,
	// 2-human refusal when 2nd human is mentioned by local part only, unknown
	// email fail-closed, and ambiguous local part refusal.
	groupLocalPartBoth := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: groupConv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + groupConv.ID,
		Msg:            "@alice @bob please run:\n```sh\n" + script + "```\n",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, groupLocalPartBoth))
	rrGrpLocalBoth := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupLocalPartBoth.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrGrpLocalBoth.Code, "R2: 2-human mention via email local parts must refuse")

	groupUnknownEmail := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: groupConv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + groupConv.ID,
		Msg:            "@alice@example.com @stranger@other.org please run:\n```sh\n" + script + "```\n",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, groupUnknownEmail))
	rrGrpUnknown := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupUnknownEmail.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrGrpUnknown.Code, "R2: unrecognized email mention must fail closed")

	groupLocalPartAliceOnly := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: groupConv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + groupConv.ID,
		Msg:            "@alice please run:\n```sh\n" + script + "```\n",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, groupLocalPartAliceOnly))
	rrGrpLocalAlice := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupLocalPartAliceOnly.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusAccepted, rrGrpLocalAlice.Code, rrGrpLocalAlice.Body.String())

	// Ambiguous email local part: second user with same local part ("alice@other.example").
	alice2 := &store.User{
		ID:          api.NewUUID(),
		Email:       "alice@other.example",
		DisplayName: "Alice Two",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, alice2))
	grantProjectAccessOnly(t, f.store, alice2.ID, f.project.ID)
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: groupConv.ID,
		PrincipalKind:  "user",
		PrincipalID:    alice2.ID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}))
	groupAmbiguous := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		ConversationID: groupConv.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "conv:" + groupConv.ID,
		Msg:            "@alice please run:\n```sh\n" + script + "```\n",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, groupAmbiguous))
	rrGrpAmbiguous := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupAmbiguous.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrGrpAmbiguous.Code, "R2/V1: ambiguous @alice local-part mention must refuse alice1")
	rrGrpAmbiguous2 := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", alice2, messages.CreateCommandRequestFromMessageBody{
		MessageID:     groupAmbiguous.ID,
		Script:        script,
		CommandSHA256: hash,
	})
	assert.Equal(t, http.StatusForbidden, rrGrpAmbiguous2.Code, "R2/V1: ambiguous @alice local-part mention must refuse alice2")

	// 17. L1: Non-chat and hub-composed message types (notification, state-change,
	// input-needed, decision-prompt, secret-request, command-request) must be refused
	// with 422 command_markdown_block_not_executable + refused audit.
	for _, disallowedType := range []string{
		"notification",
		messages.TypeStateChange,
		messages.TypeInputNeeded,
		messages.TypeDecisionPrompt,
		messages.TypeSecretRequest,
		messages.TypeCommandRequest,
	} {
		nonChatMsg := &store.Message{
			ID:            api.NewUUID(),
			ProjectID:     f.project.ID,
			ThreadID:      dmKey,
			Sender:        "agent:" + f.agent.Slug,
			SenderID:      f.agent.ID,
			Recipient:     "user:" + f.alice.Email,
			RecipientID:   f.alice.ID,
			Msg:           "```sh\n" + script + "```\n",
			Type:          disallowedType,
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     time.Now().UTC(),
		}
		require.NoError(t, f.store.CreateMessage(ctx, nonChatMsg))
		f.audit.reset()
		rrNonChat := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
			MessageID:     nonChatMsg.ID,
			Script:        script,
			CommandSHA256: hash,
		})
		assert.Equal(t, http.StatusUnprocessableEntity, rrNonChat.Code, "L1: type %q must be refused", disallowedType)
		assert.Contains(t, rrNonChat.Body.String(), msgaction.ConflictCmdMarkdownBlock)
		assertFromMsgRefusedReason(t, "L1 disallowed type "+disallowedType, msgaction.ConflictCmdMarkdownBlock)
	}

	// 18. I1: Cross-site (CSRF) browser POST to /from-message is refused with 403,
	// and non-JSON browser POST with Origin is refused with 415.
	csrfPayload, err := json.Marshal(fromMsgBody)
	require.NoError(t, err)
	aliceIdent := NewAuthenticatedUser(f.alice.ID, f.alice.Email, f.alice.DisplayName, f.alice.Role, "web")
	aliceSessionCtx := func(base context.Context) context.Context {
		return contextWithCredentialContext(
			contextWithIdentity(base, aliceIdent),
			CredentialContext{Kind: CredentialKindInteractive, ID: f.alice.ID},
		)
	}
	csrfReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/from-message", bytes.NewReader(csrfPayload))
	csrfReq = csrfReq.WithContext(aliceSessionCtx(csrfReq.Context()))
	csrfReq.Header.Set("Content-Type", "application/json")
	csrfReq.Header.Set("Origin", "https://evil.example")
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	rrCSRF := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rrCSRF, csrfReq)
	assert.Equal(t, http.StatusForbidden, rrCSRF.Code, "I1: cross-site POST to from-message must be refused")

	nonJSONReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/from-message", bytes.NewReader(csrfPayload))
	nonJSONReq = nonJSONReq.WithContext(aliceSessionCtx(nonJSONReq.Context()))
	nonJSONReq.Header.Set("Content-Type", "text/plain")
	nonJSONReq.Header.Set("Origin", "http://example.com")
	rrNonJSON := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rrNonJSON, nonJSONReq)
	assert.Equal(t, http.StatusUnsupportedMediaType, rrNonJSON.Code, "I1: non-JSON browser POST to from-message must be refused with 415")
}

func TestCommandRequests_FromMessage_ConcurrentExecOnce(t *testing.T) {
	ctx := context.Background()
	f := newCommandRequestsFixture(t, true)
	_, err := f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
	require.NoError(t, err)

	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.alice.ID)
	require.NoError(t, err)

	script := "echo concurrent-once\n"
	hash := messages.CommandRequestCanonicalHash(script, "", messages.DefaultCommandRequestTimeoutS)
	dmMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     f.project.ID,
		ThreadID:      dmKey,
		Sender:        "agent:" + f.agent.Slug,
		SenderID:      f.agent.ID,
		Recipient:     "user:" + f.alice.Email,
		RecipientID:   f.alice.ID,
		Msg:           "```bash\n" + script + "```\n",
		Type:          "instruction",
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, dmMsg))

	const workers = 4
	codes := make([]int, workers)
	bodies := make([]string, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			rr := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/from-message", f.alice, messages.CreateCommandRequestFromMessageBody{
				MessageID:      dmMsg.ID,
				Script:         script,
				CommandSHA256:  hash,
				IdempotencyKey: "conc-" + api.NewUUID(),
			})
			codes[idx] = rr.Code
			bodies[idx] = rr.Body.String()
		}(i)
	}
	close(start)
	wg.Wait()

	accepted := 0
	conflicts := 0
	for i, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicts++
			assert.Contains(t, bodies[i], msgaction.ConflictAlreadyResolved)
		default:
			t.Fatalf("worker %d unexpected status %d: %s", i, c, bodies[i])
		}
	}
	assert.Equal(t, 1, accepted, "exactly one concurrent Execute must win")
	assert.Equal(t, workers-1, conflicts, "all losing concurrent Executes must get 409 already_resolved")
	f.launchMu.Lock()
	gotLaunched := len(f.launched)
	f.launchMu.Unlock()
	assert.Equal(t, 1, gotLaunched, "executor must be launched exactly once")
}

func TestCommandRequests_DismissAndWithdraw(t *testing.T) {
	f := newCommandRequestsFixture(t, true)
	ctx := context.Background()

	// Helper to send a command request DM to Alice and return (rowID, hash).
	sendDM := func(key, script string) (string, string) {
		rr := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			CommandRequest: &messages.CommandRequestInput{
				Key:    key,
				Script: script,
			},
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var resp struct {
			CommandRequest *messages.CommandRequestSendResult `json:"command_request"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		require.NotNil(t, resp.CommandRequest)
		return resp.CommandRequest.RowID, resp.CommandRequest.CommandSHA256
	}

	row1ID, _ := sendDM("dismiss-me", "echo dismiss")

	// P3/P5 feature-detect hook: GET detail sets CanDismiss=true for approver while pending.
	rrGetPending := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+row1ID, f.alice, nil)
	require.Equal(t, http.StatusOK, rrGetPending.Code, rrGetPending.Body.String())
	var pendingDetail messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrGetPending.Body.Bytes(), &pendingDetail))
	assert.True(t, pendingDetail.CanApprove)
	assert.True(t, pendingDetail.CanDismiss, "approver GET must set canDismiss=true while pending")

	// 1. S1: PAT, agent JWT, and visitor refused on /dismiss with 403.
	patPresented := "scion_pat_cmdreq-dismiss-s1-token"
	patHash := sha256.Sum256([]byte(patPresented))
	patExpiry := time.Now().Add(time.Hour)
	require.NoError(t, f.store.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID:        api.NewUUID(),
		UserID:    f.alice.ID,
		Name:      "dismiss-pat",
		Prefix:    "scion_pat_dism",
		KeyHash:   hex.EncodeToString(patHash[:]),
		ProjectID: f.project.ID,
		Scopes:    []string{"project:read", "agent:attach"},
		ExpiresAt: &patExpiry,
		Created:   time.Now(),
	}))
	rrPAT := f.doBearerJSON(t, http.MethodPost, "/api/v1/command-requests/"+row1ID+"/dismiss", patPresented, messages.DismissCommandRequestBody{Reason: "pat"})
	assert.Equal(t, http.StatusForbidden, rrPAT.Code)

	agentToken, err := f.srv.GenerateAgentToken(f.agent.ID, f.project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	rrAgent := f.doBearerJSON(t, http.MethodPost, "/api/v1/command-requests/"+row1ID+"/dismiss", agentToken, messages.DismissCommandRequestBody{Reason: "agent"})
	assert.Equal(t, http.StatusForbidden, rrAgent.Code)

	visitorIdent := NewVisitorIdentity(api.NewUUID(), "https://issuer.cmdreq.test", "cmdreq-dismiss-visitor", "visitor@cmdreq.test", "Visitor")
	visReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/"+row1ID+"/dismiss", strings.NewReader(`{"reason":"vis"}`))
	visReq.Header.Set("Content-Type", "application/json")
	visReq = visReq.WithContext(contextWithCredentialContext(contextWithIdentity(visReq.Context(), visitorIdent), CredentialContext{Kind: CredentialKindInteractive, ID: visitorIdent.ID()}))
	rrVis := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rrVis, visReq)
	assert.Equal(t, http.StatusForbidden, rrVis.Code)

	// 2. Non-approver (Bob) refused with 403 not_approver.
	rrBob := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+row1ID+"/dismiss", f.bob, messages.DismissCommandRequestBody{Reason: "bob"})
	assert.Equal(t, http.StatusForbidden, rrBob.Code)
	assert.Contains(t, rrBob.Body.String(), msgaction.ConflictCmdNotApprover)

	// 3. Reason > 500 runes refused with 400.
	rrLong := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+row1ID+"/dismiss", f.alice, messages.DismissCommandRequestBody{
		Reason: strings.Repeat("x", msgaction.MaxReasonRunes+1),
	})
	assert.Equal(t, http.StatusBadRequest, rrLong.Code)

	// 4. Valid dismiss by Alice -> 200, status=dismissed, idempotent repeat -> 200 already_dismissed=true (AC-F6).
	rrDis := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+row1ID+"/dismiss", f.alice, messages.DismissCommandRequestBody{
		Reason:         "not needed",
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "dis-idem-1",
	})
	require.Equal(t, http.StatusOK, rrDis.Code, rrDis.Body.String())
	var disResp messages.DismissCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrDis.Body.Bytes(), &disResp))
	assert.Equal(t, store.CommandRequestStatusDismissed, disResp.Request.Status)
	assert.False(t, disResp.AlreadyDismissed)
	require.NotNil(t, disResp.Request.Dismissed)
	assert.Equal(t, "not needed", disResp.Request.Dismissed.Reason)

	rrDisRepeat := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+row1ID+"/dismiss", f.alice, messages.DismissCommandRequestBody{
		Reason: "again",
	})
	require.Equal(t, http.StatusOK, rrDisRepeat.Code)
	var disResp2 messages.DismissCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrDisRepeat.Body.Bytes(), &disResp2))
	assert.True(t, disResp2.AlreadyDismissed)

	// After dismiss, GET detail sets CanDismiss=false.
	rrGetDismissed := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+row1ID, f.alice, nil)
	require.Equal(t, http.StatusOK, rrGetDismissed.Code, rrGetDismissed.Body.String())
	var dismissedDetail messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrGetDismissed.Body.Bytes(), &dismissedDetail))
	assert.False(t, dismissedDetail.CanApprove)
	assert.False(t, dismissedDetail.CanDismiss, "approver GET must set canDismiss=false once dismissed")

	// 5. Withdraw by agent -> 200, idempotent repeat -> 200 already_withdrawn=true, user caller -> 403.
	sendDM("withdraw-me", "echo wd")
	rrUserWd := f.doUserJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", f.alice, messages.WithdrawCommandRequestBody{Key: "withdraw-me"})
	assert.Equal(t, http.StatusForbidden, rrUserWd.Code)

	rrNotFoundWd := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", messages.WithdrawCommandRequestBody{Key: "does-not-exist"})
	assert.Equal(t, http.StatusNotFound, rrNotFoundWd.Code)

	rrWd := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", messages.WithdrawCommandRequestBody{
		Key:    "withdraw-me",
		Reason: "no longer needed",
	})
	require.Equal(t, http.StatusOK, rrWd.Code, rrWd.Body.String())
	var wdResp messages.WithdrawCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrWd.Body.Bytes(), &wdResp))
	assert.Equal(t, store.CommandRequestStatusWithdrawn, wdResp.Request.Status)
	assert.False(t, wdResp.AlreadyWithdrawn)
	require.NotNil(t, wdResp.Request.Withdrawn)
	assert.Equal(t, store.CommandRequestWithdrawnByAgent, wdResp.Request.Withdrawn.By)
	assert.Equal(t, "no longer needed", wdResp.Request.Withdrawn.Reason)

	rrWdRepeat := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", messages.WithdrawCommandRequestBody{Key: "withdraw-me"})
	require.Equal(t, http.StatusOK, rrWdRepeat.Code)
	var wdResp2 messages.WithdrawCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrWdRepeat.Body.Bytes(), &wdResp2))
	assert.True(t, wdResp2.AlreadyWithdrawn)

	// Withdraw on already-dismissed key -> 409 dismissed.
	rrWdDis := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", messages.WithdrawCommandRequestBody{Key: "dismiss-me"})
	assert.Equal(t, http.StatusConflict, rrWdDis.Code)
	assert.Contains(t, rrWdDis.Body.String(), msgaction.ConflictDismissed)

	// 6. Race tests through real HTTP handlers: approve vs dismiss, and approve vs withdraw (AC-F6).
	_, err = f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
	require.NoError(t, err)

	raceID1, raceHash1 := sendDM("race-dis", "echo race1")
	start1 := make(chan struct{})
	var rrApp1, rrDis1 *httptest.ResponseRecorder
	var wg1 sync.WaitGroup
	wg1.Add(2)
	go func() {
		defer wg1.Done()
		<-start1
		rrApp1 = f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+raceID1+"/approve", f.alice, messages.ApproveCommandRequestBody{
			CommandSHA256: raceHash1,
			Surface:       msgaction.CommandRequestSurfaceCLI,
		})
	}()
	go func() {
		defer wg1.Done()
		<-start1
		rrDis1 = f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+raceID1+"/dismiss", f.alice, messages.DismissCommandRequestBody{
			Reason: "race",
		})
	}()
	close(start1)
	wg1.Wait()
	appWon1 := rrApp1.Code == http.StatusAccepted
	disWon1 := rrDis1.Code == http.StatusOK
	assert.NotEqual(t, appWon1, disWon1, "exactly one of approve(%d) or dismiss(%d) must win", rrApp1.Code, rrDis1.Code)
	// O7: a command_runs row exists iff approve won.
	run1, rerr1 := f.store.GetCommandRunByRequestID(ctx, raceID1)
	if appWon1 {
		require.NoError(t, rerr1)
		require.NotNil(t, run1)
	} else {
		assert.ErrorIs(t, rerr1, store.ErrNotFound)
		assert.Nil(t, run1)
	}

	raceID2, raceHash2 := sendDM("race-wd", "echo race2")
	start2 := make(chan struct{})
	var rrApp2, rrWd2 *httptest.ResponseRecorder
	var wg2 sync.WaitGroup
	wg2.Add(2)
	go func() {
		defer wg2.Done()
		<-start2
		rrApp2 = f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+raceID2+"/approve", f.alice, messages.ApproveCommandRequestBody{
			CommandSHA256: raceHash2,
			Surface:       msgaction.CommandRequestSurfaceCLI,
		})
	}()
	go func() {
		defer wg2.Done()
		<-start2
		rrWd2 = f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", messages.WithdrawCommandRequestBody{
			Key: "race-wd",
		})
	}()
	close(start2)
	wg2.Wait()
	appWon2 := rrApp2.Code == http.StatusAccepted
	wdWon2 := rrWd2.Code == http.StatusOK
	assert.NotEqual(t, appWon2, wdWon2, "exactly one of approve(%d) or withdraw(%d) must win", rrApp2.Code, rrWd2.Code)
	// O7: a command_runs row exists iff approve won.
	run2, rerr2 := f.store.GetCommandRunByRequestID(ctx, raceID2)
	if appWon2 {
		require.NoError(t, rerr2)
		require.NotNil(t, run2)
	} else {
		assert.ErrorIs(t, rerr2, store.ErrNotFound)
		assert.Nil(t, run2)
	}

	// 9. P6 Markdown Execute rows are created already-approved; dismiss and withdraw
	//    must refuse them with 409 already_resolved without mutating status.
	mdScript := "echo from-markdown\n"
	mdHash := messages.CommandRequestCanonicalHash(mdScript, "", messages.DefaultCommandRequestTimeoutS)
	mdMsg := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   f.project.ID,
		AgentID:     f.agent.ID,
		Sender:      "agent:" + f.agent.Slug,
		SenderID:    f.agent.ID,
		Recipient:   "user:" + f.alice.Email,
		RecipientID: f.alice.ID,
		Type:        messages.TypeInstruction,
		Msg:         "```sh\n" + mdScript + "```",
		CreatedAt:   time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, mdMsg))
	mdRes, err := f.store.CreateAndApproveMarkdownCommandRequest(ctx, &store.CreateMarkdownCommandRequestInput{
		ProjectID:     f.project.ID,
		AgentID:       f.agent.ID,
		AgentName:     f.agent.Slug,
		MessageID:     mdMsg.ID,
		ChatKey:       "dm:agent:" + f.agent.ID + ":user:" + f.alice.ID,
		Script:        mdScript,
		CommandSHA256: mdHash,
		UserID:        f.alice.ID,
		UserName:      f.alice.DisplayName,
		UserEmail:     f.alice.Email,
		ExecAgentID:   f.execAgent.ID,
		ExecProjectID: f.project.ID,
		LeaseUntil:    time.Now().UTC().Add(6 * time.Minute),
		Now:           time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NotNil(t, mdRes.Request)

	rrMdDis := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+mdRes.Request.ID+"/dismiss", f.alice, messages.DismissCommandRequestBody{
		Reason:  "nope",
		Surface: msgaction.CommandRequestSurfaceCLI,
	})
	assert.Equal(t, http.StatusConflict, rrMdDis.Code, rrMdDis.Body.String())
	assert.Contains(t, rrMdDis.Body.String(), msgaction.ConflictAlreadyResolved)

	rrMdWd := f.doAgentJSON(t, http.MethodPost, "/api/v1/agent/command-requests/withdraw", messages.WithdrawCommandRequestBody{
		Key: mdRes.Request.Key,
	})
	assert.Equal(t, http.StatusConflict, rrMdWd.Code, rrMdWd.Body.String())
	assert.Contains(t, rrMdWd.Body.String(), msgaction.ConflictAlreadyResolved)

	mdAfter, err := f.store.GetCommandRequest(ctx, mdRes.Request.ID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRequestStatusApproved, mdAfter.Status, "markdown row must remain approved")
}

func TestCommandRequests_ConversationApproverAndPendingCap(t *testing.T) {
	f := newCommandRequestsFixture(t, true)
	ctx := context.Background()

	// Grant Bob project access and add Alice, Bob, and the agent to a group conversation.
	grantProjectAccessOnly(t, f.store, f.bob.ID, f.project.ID)
	charlie := &store.User{
		ID:          api.NewUUID(),
		Email:       "charlie@example.com",
		DisplayName: "Charlie",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, charlie))

	projID := f.project.ID
	conv := &store.Conversation{
		ID:          api.NewUUID(),
		ProjectID:   &projID,
		Kind:        "group",
		Surface:     "native",
		DisplayName: "ops-thread",
	}
	require.NoError(t, f.store.CreateConversation(ctx, conv))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: conv.ID, PrincipalKind: "agent", PrincipalID: f.agent.ID}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: f.alice.ID}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: f.bob.ID}))

	// 1. Conversation target without --approver -> 400 (D6).
	rrNoApprover := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		CommandRequest: &messages.CommandRequestInput{
			Key:    "conv-no-app",
			Script: "echo hi",
		},
	})
	assert.Equal(t, http.StatusBadRequest, rrNoApprover.Code)

	// 2. Conversation target with non-participant (Charlie) -> 400.
	rrNonPart := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		CommandRequest: &messages.CommandRequestInput{
			Key:      "conv-charlie",
			Script:   "echo hi",
			Approver: "@" + charlie.Email,
		},
	})
	assert.Equal(t, http.StatusBadRequest, rrNonPart.Code)

	// 3. Conversation target with agent as approver -> 400.
	rrAgentApp := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		CommandRequest: &messages.CommandRequestInput{
			Key:      "conv-agent",
			Script:   "echo hi",
			Approver: "agent:" + f.agent.Slug,
		},
	})
	assert.Equal(t, http.StatusBadRequest, rrAgentApp.Code)

	// 3b. R1: Group conversation_ref combined with a caller-supplied dm: thread_id -> 400 refused at send.
	// Wire a FanOutEventBus registering the "web" channel so the request passes channel validation
	// and reaches resolveCommandRequestApprover (matching the reviewer's end-to-end reproducer).
	spoofInproc := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = spoofInproc.Close() })
	spoofFanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: spoofInproc},
		{Name: "web", Bus: spoofInproc},
	}, slog.Default())
	spoofEvents := NewChannelEventPublisher()
	t.Cleanup(spoofEvents.Close)
	spoofProxy := NewMessageBrokerProxy(spoofFanout, f.store, spoofEvents, func() AgentDispatcher { return f.srv.GetDispatcher() }, slog.Default())
	f.srv.SetMessageBrokerProxy(spoofProxy)
	rrSpoofDMThread := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		ThreadID:        "dm:agent:" + f.agent.ID + ":user:" + f.alice.ID,
		Channel:         "web",
		CommandRequest: &messages.CommandRequestInput{
			Key:                  "conv-dm-thread-spoof",
			Script:               "echo spoof",
			Approver:             "@" + f.alice.Email,
			ShareOutputRequested: true,
		},
	})
	f.srv.SetMessageBrokerProxy(nil)
	assert.Equal(t, http.StatusBadRequest, rrSpoofDMThread.Code, "dm: thread_id on group conversation must be refused at send: %s", rrSpoofDMThread.Body.String())
	assert.Contains(t, rrSpoofDMThread.Body.String(), "dm:", "expected dm: thread_id refusal, got: %s", rrSpoofDMThread.Body.String())

	// 4. Conversation target with human participant Alice as --approver -> 200.
	rrConvOK := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		CommandRequest: &messages.CommandRequestInput{
			Key:      "conv-alice",
			Script:   "echo conv ok",
			Approver: "@" + f.alice.Email,
		},
	})
	require.Equal(t, http.StatusOK, rrConvOK.Code, rrConvOK.Body.String())
	var sendResp struct {
		CommandRequest *messages.CommandRequestSendResult `json:"command_request"`
	}
	require.NoError(t, json.Unmarshal(rrConvOK.Body.Bytes(), &sendResp))
	require.NotNil(t, sendResp.CommandRequest)
	rowID := sendResp.CommandRequest.RowID
	rowHash := sendResp.CommandRequest.CommandSHA256

	// 5. AC-S7: Bob (other participant) gets read-only GET (canApprove=false, reason=not_approver, no exec_container),
	// and 403 not_approver on approve and dismiss. Charlie (non-participant) gets 403 on GET.
	rrBobGet := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+rowID, f.bob, nil)
	require.Equal(t, http.StatusOK, rrBobGet.Code, rrBobGet.Body.String())
	var bobDetail messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrBobGet.Body.Bytes(), &bobDetail))
	assert.False(t, bobDetail.CanApprove)
	assert.False(t, bobDetail.CanDismiss, "non-approver GET must set canDismiss=false")
	assert.Equal(t, msgaction.ConflictCmdNotApprover, bobDetail.Reason)
	assert.Nil(t, bobDetail.ExecContainer)

	rrBobApprove := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+rowID+"/approve", f.bob, messages.ApproveCommandRequestBody{
		CommandSHA256: rowHash,
	})
	assert.Equal(t, http.StatusForbidden, rrBobApprove.Code)
	assert.Contains(t, rrBobApprove.Body.String(), msgaction.ConflictCmdNotApprover)

	rrBobDismiss := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+rowID+"/dismiss", f.bob, messages.DismissCommandRequestBody{
		Reason: "not mine",
	})
	assert.Equal(t, http.StatusForbidden, rrBobDismiss.Code)
	assert.Contains(t, rrBobDismiss.Body.String(), msgaction.ConflictCmdNotApprover)

	rrCharlieGet := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+rowID, charlie, nil)
	assert.Equal(t, http.StatusForbidden, rrCharlieGet.Code)

	// 5b. R2: Federated/visitor, suspended, and expired users refused as --approver and DM recipient.
	fedEmail := "visitor@ext.example.com"
	fedIssuer := "https://issuer.ext.test"
	fedUser := &store.User{
		ID:             api.NewUUID(),
		DisplayName:    "Fed Visitor",
		Role:           store.UserRoleViewer,
		Status:         store.UserStatusActive,
		Kind:           store.UserKindFederated,
		HomeIssuer:     &fedIssuer,
		FederatedEmail: &fedEmail,
	}
	require.NoError(t, f.store.CreateFederatedUser(ctx, fedUser))
	require.NotEmpty(t, fedUser.Email)
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: fedUser.ID}))

	suspendedUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "suspended@example.com",
		DisplayName: "Suspended User",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusSuspended,
	}
	require.NoError(t, f.store.CreateUser(ctx, suspendedUser))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: suspendedUser.ID}))

	pastExp := time.Now().UTC().Add(-time.Hour)
	issuerID := f.alice.ID
	expiredUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "expired@" + store.TestFixtureEmailDomain,
		DisplayName: "Expired User",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestFixture,
		ExpiresAt:   &pastExp,
		IssuedBy:    &issuerID,
	}
	require.NoError(t, f.store.CreateTestFixtureUser(ctx, expiredUser))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: expiredUser.ID}))

	// O1: Participant whose display name looks like an absent user's email must not match email-shaped --approver.
	spoofUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "spoof@example.com",
		DisplayName: "ghost@example.com",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, f.store.CreateUser(ctx, spoofUser))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: spoofUser.ID}))

	for _, tc := range []struct {
		name     string
		approver string
		wantMsg  string
	}{
		{"federated participant", "@" + fedEmail, ""},
		{"suspended participant", "@" + suspendedUser.Email, ""},
		{"expired participant", "@" + expiredUser.Email, ""},
		{"O1 email-shaped display-name spoof", "@ghost@example.com", ""},
		{"L2 bare display name refused", f.alice.DisplayName, "specify the approver's email address"},
	} {
		rrBadApp := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
			ConversationRef: "conv:" + conv.ID,
			CommandRequest: &messages.CommandRequestInput{
				Key:      "bad-app-" + api.NewUUID()[:6],
				Script:   "echo no",
				Approver: tc.approver,
			},
		})
		assert.Equal(t, http.StatusBadRequest, rrBadApp.Code, "%s: %s", tc.name, rrBadApp.Body.String())
		if tc.wantMsg != "" {
			assert.Contains(t, rrBadApp.Body.String(), tc.wantMsg, "%s", tc.name)
		}
	}

	// R2: DM to federated visitor, suspended, or expired user refused with 400.
	for _, u := range []*store.User{fedUser, suspendedUser, expiredUser} {
		rrBadDM := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
			Recipient: "user:" + u.Email,
			CommandRequest: &messages.CommandRequestInput{
				Key:    "bad-dm-" + api.NewUUID()[:6],
				Script: "echo no",
			},
		})
		assert.Equal(t, http.StatusBadRequest, rrBadDM.Code, "DM to %s: %s", u.Email, rrBadDM.Body.String())
	}

	// 5c. P3-audit I3 + P5-audit I1: after Alice approves and the run finishes with
	// wake/reason/outputBytes/shareOutput populated, Alice's GET includes them,
	// while non-approver participant Bob's GET and SSE view keep status, exit code,
	// and timestamps only (stripping execAgentId, execProjectId, reason, wake,
	// outputBytes, shareOutput).
	_, err := f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
	require.NoError(t, err)
	shareTrue := true
	rrAliceApprove := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+rowID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: rowHash,
		Surface:       msgaction.CommandRequestSurfaceCLI,
		ShareOutput:   &shareTrue,
	})
	require.Equal(t, http.StatusAccepted, rrAliceApprove.Code, rrAliceApprove.Body.String())
	var appConvResp messages.ApproveCommandRequestResponse
	require.NoError(t, json.Unmarshal(rrAliceApprove.Body.Bytes(), &appConvResp))
	require.NotNil(t, appConvResp.Run)

	_, err = f.store.TransitionCommandRunToWaking(ctx, appConvResp.Run.RunID, "resume")
	require.NoError(t, err)
	exitZero := 0
	outBytes := int64(42)
	finAt := time.Now().UTC().Truncate(time.Microsecond)
	wonFin, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:        appConvResp.Run.RunID,
		Status:       store.CommandRunStatusNotStarted,
		Reason:       "wake_failed",
		ExitCode:     &exitZero,
		OutputBytes:  &outBytes,
		OutputStatus: "captured",
		FinishedAt:   finAt,
	})
	require.NoError(t, err)
	require.True(t, wonFin)

	rrAliceGetApproved := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+rowID, f.alice, nil)
	require.Equal(t, http.StatusOK, rrAliceGetApproved.Code)
	var aliceApprovedDetail messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrAliceGetApproved.Body.Bytes(), &aliceApprovedDetail))
	require.NotNil(t, aliceApprovedDetail.Run)
	assert.Equal(t, f.execAgent.ID, aliceApprovedDetail.Run.ExecAgentID)
	assert.Equal(t, f.project.ID, aliceApprovedDetail.Run.ExecProjectID)
	assert.Equal(t, "resume", aliceApprovedDetail.Run.Wake)
	assert.Equal(t, "wake_failed", aliceApprovedDetail.Run.Reason)
	require.NotNil(t, aliceApprovedDetail.Run.OutputBytes)
	assert.Equal(t, int64(42), *aliceApprovedDetail.Run.OutputBytes)

	rrBobGetApproved := f.doUserJSON(t, http.MethodGet, "/api/v1/command-requests/"+rowID, f.bob, nil)
	require.Equal(t, http.StatusOK, rrBobGetApproved.Code)
	var bobApprovedDetail messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(rrBobGetApproved.Body.Bytes(), &bobApprovedDetail))
	require.NotNil(t, bobApprovedDetail.Run)
	assert.Equal(t, store.CommandRunStatusNotStarted, bobApprovedDetail.Run.Status)
	require.NotNil(t, bobApprovedDetail.Run.ExitCode)
	assert.Equal(t, 0, *bobApprovedDetail.Run.ExitCode)
	assert.False(t, bobApprovedDetail.Run.CreatedAt.IsZero())
	assert.NotNil(t, bobApprovedDetail.Run.FinishedAt)
	for _, rv := range []*messages.CommandRequestRunView{bobApprovedDetail.Run, bobApprovedDetail.Request.Run} {
		require.NotNil(t, rv)
		assert.Empty(t, rv.ExecAgentID, "non-approver view must strip execAgentId")
		assert.Empty(t, rv.ExecProjectID, "non-approver view must strip execProjectId")
		assert.Empty(t, rv.Reason, "non-approver view must strip reason (I1)")
		assert.Empty(t, rv.Wake, "non-approver view must strip wake (I1)")
		assert.Nil(t, rv.OutputBytes, "non-approver view must strip outputBytes (I1)")
		assert.False(t, rv.ShareOutput, "non-approver view must strip shareOutput (I1)")
	}

	// Verify commandRequestSSEView strips ShareOutput=true as well (for P4 opt-in runs).
	storedConvReq, err := f.store.GetCommandRequest(ctx, rowID)
	require.NoError(t, err)
	storedConvRun, err := f.store.GetCommandRunByRequestID(ctx, rowID)
	require.NoError(t, err)
	storedConvRun.ShareOutput = true
	sseConvView := commandRequestSSEView(storedConvReq, nil, storedConvRun)
	require.NotNil(t, sseConvView.Run)
	assert.Equal(t, store.CommandRunStatusNotStarted, sseConvView.Run.Status)
	require.NotNil(t, sseConvView.Run.ExitCode)
	assert.Equal(t, 0, *sseConvView.Run.ExitCode)
	assert.Empty(t, sseConvView.Run.ExecAgentID)
	assert.Empty(t, sseConvView.Run.ExecProjectID)
	assert.Empty(t, sseConvView.Run.Reason)
	assert.Empty(t, sseConvView.Run.Wake)
	assert.Nil(t, sseConvView.Run.OutputBytes)
	assert.False(t, sseConvView.Run.ShareOutput, "SSE view must strip shareOutput (I1)")

	// O8: Asking agent's own list strips only execAgentId/execProjectId, keeping
	// status, exitCode, reason, wake, outputBytes, shareOutput, and timestamps.
	rrAgentList := f.doAgentJSON(t, http.MethodGet, "/api/v1/agent/command-requests?key=conv-alice", nil)
	require.Equal(t, http.StatusOK, rrAgentList.Code, rrAgentList.Body.String())
	var agentListResp messages.AgentCommandRequestListResponse
	require.NoError(t, json.Unmarshal(rrAgentList.Body.Bytes(), &agentListResp))
	require.Len(t, agentListResp.Items, 1)
	require.NotNil(t, agentListResp.Items[0].Run)
	agentRun := agentListResp.Items[0].Run
	assert.Empty(t, agentRun.ExecAgentID, "agent list must strip execAgentId")
	assert.Empty(t, agentRun.ExecProjectID, "agent list must strip execProjectId")
	assert.Equal(t, store.CommandRunStatusNotStarted, agentRun.Status)
	require.NotNil(t, agentRun.ExitCode)
	assert.Equal(t, 0, *agentRun.ExitCode)
	assert.Equal(t, "wake_failed", agentRun.Reason, "agent list must retain run.reason (O8)")
	assert.Equal(t, "resume", agentRun.Wake, "agent list must retain run.wake (O8)")
	require.NotNil(t, agentRun.OutputBytes)
	assert.Equal(t, int64(42), *agentRun.OutputBytes, "agent list must retain run.outputBytes (O8)")
	assert.False(t, agentRun.CreatedAt.IsZero())
	assert.NotNil(t, agentRun.FinishedAt)

	agentViewWithShare := commandRequestAgentView(storedConvReq, nil, storedConvRun)
	require.NotNil(t, agentViewWithShare.Run)
	assert.Empty(t, agentViewWithShare.Run.ExecAgentID)
	assert.Empty(t, agentViewWithShare.Run.ExecProjectID)
	assert.Equal(t, "wake_failed", agentViewWithShare.Run.Reason)
	assert.Equal(t, "resume", agentViewWithShare.Run.Wake)
	require.NotNil(t, agentViewWithShare.Run.OutputBytes)
	assert.Equal(t, int64(42), *agentViewWithShare.Run.OutputBytes)
	assert.True(t, agentViewWithShare.Run.ShareOutput, "agent view must retain shareOutput=true (O8)")

	// 5d. O7: Approver removed from conversation after send -> 403 on approve and dismiss.
	rrConv2 := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		CommandRequest: &messages.CommandRequestInput{
			Key:      "conv-alice-removed",
			Script:   "echo removed",
			Approver: "@" + f.alice.Email,
		},
	})
	require.Equal(t, http.StatusOK, rrConv2.Code, rrConv2.Body.String())
	var sendResp2 struct {
		CommandRequest *messages.CommandRequestSendResult `json:"command_request"`
	}
	require.NoError(t, json.Unmarshal(rrConv2.Body.Bytes(), &sendResp2))
	require.NoError(t, f.store.RemoveParticipant(ctx, conv.ID, "user", f.alice.ID))

	rrRemovedApp := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+sendResp2.CommandRequest.RowID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: sendResp2.CommandRequest.CommandSHA256,
	})
	assert.Equal(t, http.StatusForbidden, rrRemovedApp.Code)
	rrRemovedDis := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+sendResp2.CommandRequest.RowID+"/dismiss", f.alice, messages.DismissCommandRequestBody{
		Reason: "removed",
	})
	assert.Equal(t, http.StatusForbidden, rrRemovedDis.Code)

	// 6. AC-F7: Pending cap 10 per agent -> 11th returns 429 command_request_limit.
	// We already have 1 pending (conv-alice-removed); create 9 more to hit 10.
	for i := 1; i < messages.MaxPendingCommandRequestsPerAgent; i++ {
		rr := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			CommandRequest: &messages.CommandRequestInput{
				Key:    "cap-key-" + string(rune('a'+i)),
				Script: "echo cap",
			},
		})
		require.Equal(t, http.StatusOK, rr.Code, "i=%d body=%s", i, rr.Body.String())
	}
	rrCap := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		CommandRequest: &messages.CommandRequestInput{
			Key:    "cap-overflow",
			Script: "echo over",
		},
	})
	assert.Equal(t, http.StatusTooManyRequests, rrCap.Code, rrCap.Body.String())
	assert.Contains(t, rrCap.Body.String(), msgaction.ConflictCmdRequestLimit)
}

func TestCommandRequests_AskerHardDeleteHook(t *testing.T) {
	f := newCommandRequestsFixture(t, true)
	ctx := context.Background()
	_, err := f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
	require.NoError(t, err)

	// Create 2 requests: one stays pending, one is approved.
	sendReq := func(key string) (string, string) {
		rr := f.doAgentJSON(t, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			CommandRequest: &messages.CommandRequestInput{
				Key:    key,
				Script: "echo " + key,
			},
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var resp struct {
			CommandRequest *messages.CommandRequestSendResult `json:"command_request"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		return resp.CommandRequest.RowID, resp.CommandRequest.CommandSHA256
	}

	pendingID, _ := sendReq("del-pending")
	approvedID, approvedHash := sendReq("del-approved")

	rrApp := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+approvedID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: approvedHash,
	})
	require.Equal(t, http.StatusAccepted, rrApp.Code, rrApp.Body.String())

	// Execute real agent hard-delete work inside withAgentSecretCleanup.
	nowDel := time.Now().UTC()
	_, err = f.srv.withAgentSecretCleanup(ctx, f.agent.ID, func(txCtx context.Context) (int, error) {
		if _, _, werr := f.srv.agentHardDeleteWork(txCtx, f.store, f.agent, AuditActor{PrincipalKind: "user", PrincipalID: f.alice.ID}, store.Deactivation{
			Cause: store.EdgeDeactivationAgentHardDelete,
			At:    &nowDel,
			OpID:  api.NewUUID(),
		}, "test hard delete"); werr != nil {
			return 0, werr
		}
		return 1, nil
	})
	require.NoError(t, err)

	// Pending row -> withdrawn (withdrawn_by=system, withdrawn_reason="agent deleted").
	gotPending, err := f.store.GetCommandRequest(ctx, pendingID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRequestStatusWithdrawn, gotPending.Status)
	assert.Equal(t, store.CommandRequestWithdrawnBySystem, gotPending.WithdrawnBy)
	assert.Equal(t, "agent deleted", gotPending.WithdrawnReason)

	// Approved row and its command_run remain untouched (§4.12).
	gotApproved, err := f.store.GetCommandRequest(ctx, approvedID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRequestStatusApproved, gotApproved.Status)
	gotRun, err := f.store.GetCommandRunByRequestID(ctx, approvedID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRunStatusQueued, gotRun.Status)

	// O7: Race approve vs asker hard-delete hook on a fresh pending request.
	raceID, raceHash := sendReq("del-race")
	start := make(chan struct{})
	var rrRaceApp *httptest.ResponseRecorder
	var delErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		rrRaceApp = f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+raceID+"/approve", f.alice, messages.ApproveCommandRequestBody{
			CommandSHA256: raceHash,
			Surface:       msgaction.CommandRequestSurfaceCLI,
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, delErr = f.store.WithdrawAgentCommandRequestsOnDelete(ctx, f.agent.ID, time.Now().UTC())
	}()
	close(start)
	wg.Wait()
	require.NoError(t, delErr)

	raceRow, err := f.store.GetCommandRequest(ctx, raceID)
	require.NoError(t, err)
	raceRun, raceRunErr := f.store.GetCommandRunByRequestID(ctx, raceID)
	if rrRaceApp.Code == http.StatusAccepted {
		assert.Equal(t, store.CommandRequestStatusApproved, raceRow.Status)
		require.NoError(t, raceRunErr)
		require.NotNil(t, raceRun)
	} else {
		assert.Equal(t, http.StatusConflict, rrRaceApp.Code)
		assert.Equal(t, store.CommandRequestStatusWithdrawn, raceRow.Status)
		assert.Equal(t, store.CommandRequestWithdrawnBySystem, raceRow.WithdrawnBy)
		assert.ErrorIs(t, raceRunErr, store.ErrNotFound)
		assert.Nil(t, raceRun)
	}

	// L1 + L1a: Soft-delete finalize withdraws pending command requests while leaving
	// approved rows untouched, and emits both the withdrawn audit event and SSE update.
	softPendingID, _ := sendReq("soft-del-pending")
	softApprovedID, softApprovedHash := sendReq("soft-del-approved")
	rrSoftApp := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+softApprovedID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: softApprovedHash,
	})
	require.Equal(t, http.StatusAccepted, rrSoftApp.Code, rrSoftApp.Body.String())

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.events = pub
	sseCh, unsub := pub.Subscribe("user." + f.alice.ID + ".>")
	defer unsub()

	nowSoft := time.Now().UTC()
	nSoft, err := f.srv.finalizeAgentDeletion(ctx, f.agent.ID, store.DeletionPredicate{}, store.DeletionFinalizeSoft, store.DeletionFields{
		DeletedAt: &nowSoft,
	})
	require.NoError(t, err)
	require.Equal(t, 1, nSoft)

	gotSoftPending, err := f.store.GetCommandRequest(ctx, softPendingID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRequestStatusWithdrawn, gotSoftPending.Status)
	assert.Equal(t, store.CommandRequestWithdrawnBySystem, gotSoftPending.WithdrawnBy)
	assert.Equal(t, "agent deleted", gotSoftPending.WithdrawnReason)

	gotSoftApproved, err := f.store.GetCommandRequest(ctx, softApprovedID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRequestStatusApproved, gotSoftApproved.Status)
	gotSoftRun, err := f.store.GetCommandRunByRequestID(ctx, softApprovedID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRunStatusQueued, gotSoftRun.Status)

	var sawSoftWithdrawnAudit bool
	for _, ev := range f.audit.snapshot() {
		if ev.Kind == msgaction.KindCommandRequest && ev.EventType == MessageActionEventWithdrawn && ev.RowID == softPendingID && ev.Reason == "agent deleted" {
			sawSoftWithdrawnAudit = true
		}
	}
	assert.True(t, sawSoftWithdrawnAudit, "soft-delete finalize must emit withdrawn audit event for pending command request (L1a)")

	var sawSoftSSE bool
	drainDone := false
	for !drainDone {
		select {
		case ev := <-sseCh:
			if strings.HasSuffix(ev.Subject, ".chat."+msgaction.KindCommandRequest+".updated") &&
				strings.Contains(string(ev.Data), softPendingID) &&
				strings.Contains(string(ev.Data), store.CommandRequestStatusWithdrawn) {
				sawSoftSSE = true
				drainDone = true
			}
		default:
			drainDone = true
		}
	}
	assert.True(t, sawSoftSSE, "soft-delete finalize must publish SSE update for withdrawn command request (L1a)")
}
