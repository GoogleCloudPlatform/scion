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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func decodePendingAPIError(t *testing.T, body []byte) (code string, details map[string]interface{}) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &env))
	return env.Error.Code, env.Error.Details
}

// seedPendingRows stores n live pending secrets for ownerID from agentID,
// with keys prefix_0 .. prefix_{n-1}.
func seedPendingRows(t *testing.T, f *pendingFixture, ownerID, agentID, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := f.Store.CreateOrReplacePendingSecret(context.Background(), &store.PendingSecret{
			Key: fmt.Sprintf("%s_%d", prefix, i), SecretType: store.SecretTypeEnvironment,
			Scope: store.ScopeUser, ScopeID: ownerID,
			ValueRef: mustHold(t, f.Server, "test-value-seed"), ValueLen: 15,
			AgentID: agentID, AgentSlug: "seed", ProjectID: f.ProjectID, ProjectName: f.ProjectName,
		}, time.Now())
		require.NoError(t, err)
	}
}

// TestAgentPendingSecret_AgentCap: an agent with PendingSecretAgentCap
// pending secrets gets 429 secret_pending_cap for a new key, nothing is
// stored and its held value is released; a replacement of its own key is
// accepted; another agent is not affected.
func TestAgentPendingSecret_AgentCap(t *testing.T) {
	f := newPendingFixture(t, "pend-cap-agent")
	audit := recordPendingAudit(f.Server)
	for i := 0; i < store.PendingSecretAgentCap; i++ {
		decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, fmt.Sprintf("CAP_%d", i), userScopeWrite("test-value-1", false)))
	}
	dropsBefore := len(f.drops.dropped)

	rec := agentPut(t, f.Server, f.AgentID, f.Token, "CAP_OVER", userScopeWrite("test-value-over", true))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	code, details := decodePendingAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeSecretPendingCap, code)
	assert.Equal(t, store.PendingSecretCapAgent, details["cap"])
	assert.EqualValues(t, store.PendingSecretAgentCap, details["limit"])
	assert.NotContains(t, rec.Body.String(), "test-value")
	assert.Len(t, f.listPending(t, f.owner()), store.PendingSecretAgentCap)
	assert.Len(t, f.drops.dropped, dropsBefore+1, "the held value of the refused write is released")

	refused := audit.ofType(PendingSecretEventRefused)
	require.Len(t, refused, 1)
	assert.Equal(t, "cap_agent", refused[0].Reason)
	assert.Equal(t, "CAP_OVER", refused[0].Key)
	assert.Equal(t, PendingSecretActorAgent, refused[0].ActorKind)
	assert.Equal(t, f.AgentID, refused[0].ActorID)

	// A replacement of one of its own keys does not count again.
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "CAP_3", userScopeWrite("test-value-2", false)))
	assert.Equal(t, "pending", resp.State)

	// Another agent of the same owner has its own allowance.
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentB, f.TokenB, "CAP_B", userScopeWrite("test-value-b", false)))
}

// TestAgentPendingSecret_OwnerCap: an owner with PendingSecretOwnerCap
// pending secrets refuses a new key from any agent with 429.
func TestAgentPendingSecret_OwnerCap(t *testing.T) {
	f := newPendingFixture(t, "pend-cap-owner")
	for a := 0; a < store.PendingSecretOwnerCap/store.PendingSecretAgentCap; a++ {
		seedPendingRows(t, f, f.UserID, tid(fmt.Sprintf("pend-cap-seed-%d", a)), fmt.Sprintf("SEED%d", a), store.PendingSecretAgentCap)
	}
	rec := agentPut(t, f.Server, f.AgentID, f.Token, "OWNER_OVER", userScopeWrite("test-value-over", false))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	code, details := decodePendingAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeSecretPendingCap, code)
	assert.Equal(t, store.PendingSecretCapOwner, details["cap"])
	assert.EqualValues(t, store.PendingSecretOwnerCap, details["limit"])
	assert.Len(t, f.listPending(t, f.owner()), store.PendingSecretOwnerCap)
}

// TestPendingSecretAudit_Lifecycle: create, replace, refused (another
// agent), approve and reject each log one event with the writer, the
// owner and the actor, and never the value.
func TestPendingSecretAudit_Lifecycle(t *testing.T) {
	f := newPendingFixture(t, "pend-audit")
	audit := recordPendingAudit(f.Server)

	first := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "AUD_A", userScopeWrite("test-value-a1", false)))
	second := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "AUD_A", userScopeWrite("test-value-a2", false)))
	rec := agentPut(t, f.Server, f.AgentB, f.TokenB, "AUD_A", userScopeWrite("test-value-b", true))
	require.Equal(t, http.StatusConflict, rec.Code)
	toReject := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "AUD_R", userScopeWrite("test-value-r", false)))

	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+second.PendingID+"/approve", ApprovePendingSecretRequest{AllowProgeny: boolPtr(false)})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+toReject.PendingID+"/reject", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	creates := audit.ofType(PendingSecretEventCreate)
	require.Len(t, creates, 2)
	assert.Equal(t, first.PendingID, creates[0].PendingID)
	assert.Equal(t, "AUD_A", creates[0].Key)
	assert.Equal(t, store.SecretTypeFile, creates[0].SecretType)
	assert.Equal(t, "~/.config/tool/creds.json", creates[0].Target)
	assert.Equal(t, store.ScopeUser, creates[0].Scope)
	assert.Equal(t, f.UserID, creates[0].ScopeID)
	assert.Equal(t, f.AgentID, creates[0].AgentID)
	assert.Equal(t, f.ProjectID, creates[0].ProjectID)
	assert.Equal(t, PendingSecretActorAgent, creates[0].ActorKind)
	assert.Equal(t, f.AgentID, creates[0].ActorID)
	assert.True(t, creates[0].AllowProgeny)

	replaces := audit.ofType(PendingSecretEventReplace)
	require.Len(t, replaces, 1)
	assert.Equal(t, second.PendingID, replaces[0].PendingID)

	refused := audit.ofType(PendingSecretEventRefused)
	require.Len(t, refused, 1)
	assert.Equal(t, pendingSecretRefusedHeldByOtherAgent, refused[0].Reason)
	assert.Equal(t, f.AgentB, refused[0].AgentID)
	assert.Equal(t, f.ProjectB, refused[0].ProjectID)
	assert.Empty(t, refused[0].PendingID, "the refused write names no pending secret")

	approves := audit.ofType(PendingSecretEventApprove)
	require.Len(t, approves, 1)
	assert.Equal(t, second.PendingID, approves[0].PendingID)
	assert.Equal(t, PendingSecretActorUser, approves[0].ActorKind)
	assert.Equal(t, f.UserID, approves[0].ActorID)
	assert.False(t, approves[0].AllowProgeny, "the approved progeny setting is logged")

	rejects := audit.ofType(PendingSecretEventReject)
	require.Len(t, rejects, 1)
	assert.Equal(t, toReject.PendingID, rejects[0].PendingID)
	assert.Equal(t, PendingSecretActorUser, rejects[0].ActorKind)
}

// TestLogPendingSecretEvent_NoValue: the structured log record carries the
// event fields and nothing derived from the value.
func TestLogPendingSecretEvent_NoValue(t *testing.T) {
	var buf bytes.Buffer
	logger := &LogAuditLogger{log: slog.New(slog.NewJSONHandler(&buf, nil))}
	require.NoError(t, logger.LogPendingSecretEvent(context.Background(), &PendingSecretEvent{
		EventType: PendingSecretEventCreate, PendingID: "pend-1", Key: "TOOL_TOKEN",
		SecretType: store.SecretTypeEnvironment, Target: "TOOL_TOKEN", Scope: store.ScopeUser, ScopeID: "user-1",
		AgentID: "agent-1", ProjectID: "project-1", ActorKind: PendingSecretActorAgent, ActorID: "agent-1",
		ReplacesSecretID: "secret-1", AllowProgeny: true,
	}))
	var rec map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "pending secret event", rec["msg"])
	for k, want := range map[string]interface{}{
		"event_type": PendingSecretEventCreate, "pending_id": "pend-1", "key": "TOOL_TOKEN",
		"secret_type": store.SecretTypeEnvironment, "target": "TOOL_TOKEN", "scope": store.ScopeUser,
		"scope_id": "user-1", "agent_id": "agent-1", "project_id": "project-1",
		"actor_kind": PendingSecretActorAgent, "actor_id": "agent-1", "replaces_secret_id": "secret-1",
		"allow_progeny": true,
	} {
		assert.Equal(t, want, rec[k], k)
	}
	assert.Contains(t, rec, "timestamp")
	for k := range rec {
		assert.NotContains(t, strings.ToLower(k), "hash")
		assert.NotContains(t, strings.ToLower(k), "value", "no value and no value length")
	}
}

// TestPendingSecretAudit_NilLoggerSafe: with no audit logger configured,
// writes and decisions work and log nothing.
func TestPendingSecretAudit_NilLoggerSafe(t *testing.T) {
	f := newPendingFixture(t, "pend-audit-nil")
	f.Server.auditLogger = nil
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "NIL_A", userScopeWrite("test-value-1", false)))
	rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+resp.PendingID+"/reveal", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+resp.PendingID+"/reject", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	n, err := f.Server.expirePendingSecrets(context.Background(), time.Now().Add(store.PendingSecretTTL+time.Hour))
	require.NoError(t, err)
	assert.Zero(t, n)
}

// pendingNotifications returns the owner's secret_pending notifications.
func pendingNotifications(t *testing.T, f *pendingFixture) []store.Notification {
	t.Helper()
	all, err := f.Store.GetNotifications(context.Background(), store.SubscriberTypeUser, f.UserID, false)
	require.NoError(t, err)
	var out []store.Notification
	for _, n := range all {
		if n.Kind == store.NotificationKindSecretPending {
			out = append(out, n)
		}
	}
	return out
}

// TestPendingSecretNotification_Aggregated: the first pending write of an
// agent notifies the owner with the agent, its project, the key, the type
// and the time, never the value; more writes by the same agent within the
// hour add nothing; another agent gets its own notification.
func TestPendingSecretNotification_Aggregated(t *testing.T) {
	f := newPendingFixture(t, "pend-notify")
	first := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "NOTE_A", userScopeWrite("test-value-n1", false)))
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "NOTE_B", userScopeWrite("test-value-n2", false)))
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "NOTE_A", userScopeWrite("test-value-n3", false)))

	notes := pendingNotifications(t, f)
	require.Len(t, notes, 1, "one notification per owner and writing agent per hour")
	n := notes[0]
	agent, err := f.Store.GetAgent(context.Background(), f.AgentID)
	require.NoError(t, err)
	assert.Equal(t, store.SubscriberTypeUser, n.SubscriberType)
	assert.Equal(t, f.UserID, n.SubscriberID)
	assert.Equal(t, f.AgentID, n.AgentID)
	assert.Equal(t, f.ProjectID, n.ProjectID)
	assert.Equal(t, store.NotificationStatusSecretPending, n.Status)
	assert.Equal(t, store.NotificationResourcePendingSecret, n.ResourceType)
	assert.Equal(t, first.PendingID, n.ResourceID)
	assert.Contains(t, n.Message, "Agent "+agent.Slug)
	assert.Contains(t, n.Message, "in project "+f.ProjectName)
	assert.Contains(t, n.Message, "saved secret NOTE_A (file) for your account at ")
	assert.Contains(t, n.Message, "Approve or reject in Profile > Secrets.")
	assert.NotContains(t, n.Message, "test-value")
	assert.Empty(t, n.Excerpt)

	decodePendingResponse(t, agentPut(t, f.Server, f.AgentB, f.TokenB, "NOTE_C", userScopeWrite("test-value-n4", false)))
	assert.Len(t, pendingNotifications(t, f), 2, "another writing agent is notified separately")
}

// TestPendingSecretNotification_CountsWaitingSecrets: a notification made
// after the hour reports how many of the agent's secrets await approval.
func TestPendingSecretNotification_CountsWaitingSecrets(t *testing.T) {
	f := newPendingFixture(t, "pend-notify-count")
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "CNT_A", userScopeWrite("test-value-1", false)))
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "CNT_B", userScopeWrite("test-value-2", false)))

	p := &store.PendingSecret{
		ID: "pend-later", Key: "CNT_C", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: f.UserID,
		AgentID: f.AgentID, AgentSlug: "slug\u202eevil", ProjectID: f.ProjectID, ProjectName: f.ProjectName,
	}
	base := time.Date(2026, 11, 10, 10, 15, 0, 0, time.UTC)
	n := buildPendingSecretNotification(p, 3, base.Add(2*time.Hour))
	assert.Contains(t, n.Message, "3 secrets from this agent await your approval.")
	assert.Contains(t, n.Message, "at 2026-11-10T12:15:00Z.")
	assert.NotContains(t, n.Message, "\u202e", "agent-chosen names are cleaned")
	earlier := buildPendingSecretNotification(p, 1, base)
	assert.NotContains(t, earlier.Message, "await your approval.", "a single secret gets no count")
	assert.NotEqual(t, earlier.ID, n.ID, "a later hour gets its own notification id")
	assert.Equal(t, earlier.ID, buildPendingSecretNotification(p, 2, base.Add(30*time.Minute)).ID,
		"the same hour maps to one notification id")
}
