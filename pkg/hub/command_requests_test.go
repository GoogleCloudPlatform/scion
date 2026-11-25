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

	rrList := f.doAgentJSON(t, http.MethodGet, "/api/v1/agent/command-requests", nil)
	assert.Equal(t, http.StatusNotFound, rrList.Code)
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

	// 6. Security S1 + S8: agent, UAT, visitor, and non-approver all get 403 + refused audit event.
	f.audit.reset()
	rrAgentApprove := f.doAgentJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", messages.ApproveCommandRequestBody{
		CommandSHA256: v2Hash,
	})
	assert.Equal(t, http.StatusForbidden, rrAgentApprove.Code)

	// UAT caller -> 403 INTERACTIVE_STATE + refused audit event.
	uatReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve",
		strings.NewReader(`{"command_sha256":"`+v2Hash+`"}`))
	uatIdent := NewScopedUserIdentity(
		NewAuthenticatedUser(f.alice.ID, f.alice.Email, f.alice.DisplayName, f.alice.Role, "uat"),
		f.project.ID, []string{"agent:attach"},
	)
	uatCtx := contextWithCredentialContext(
		contextWithIdentity(uatReq.Context(), uatIdent),
		CredentialContext{Kind: CredentialKindUAT, ID: "uat-1"},
	)
	rrUAT := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rrUAT, uatReq.WithContext(uatCtx))
	assert.Equal(t, http.StatusForbidden, rrUAT.Code)

	// Non-approver (bob) -> 403 not_approver.
	rrBob := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", f.bob, messages.ApproveCommandRequestBody{
		CommandSHA256: v2Hash,
	})
	assert.Equal(t, http.StatusForbidden, rrBob.Code)
	assert.Contains(t, rrBob.Body.String(), msgaction.ConflictCmdNotApprover)

	// Verify refused audit events were emitted (S8).
	refusedCount := 0
	for _, ev := range f.audit.snapshot() {
		if ev.Kind == msgaction.KindCommandRequest && ev.EventType == MessageActionEventRefused {
			refusedCount++
		}
	}
	assert.GreaterOrEqual(t, refusedCount, 3, "expected refused audit events for agent, UAT, and non-approver")

	// 7. Unconfigured exec container -> 409 exec_container_unconfigured.
	rrUnconf := f.doUserJSON(t, http.MethodPost, "/api/v1/command-requests/"+v2ID+"/approve", f.alice, messages.ApproveCommandRequestBody{
		CommandSHA256: v2Hash,
	})
	assert.Equal(t, http.StatusConflict, rrUnconf.Code)
	assert.Contains(t, rrUnconf.Body.String(), msgaction.ConflictCmdExecUnconfigured)

	// Register alice's execution container.
	_, err := f.store.PutUserExecContainer(ctx, f.alice.ID, f.execAgent.ID)
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
	f.launchMu.Unlock()

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

	// Avoid unused time import warning.
	_ = time.Second
}
