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

package hub

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E.2a (ptone/scion#2127, plan §3.3): the hub.audit slog-event credential
// group. These assert on emitted output (see audit_emission_test.go's
// rationale for that convention), through the real LogAuditLogger and a real
// credential decoration on ctx.
// ---------------------------------------------------------------------------

func ctxWithTestDecoration(tokenID, tokenName string) context.Context {
	decoration := &CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   tokenID,
		TokenName: tokenName,
		Boundary:  decorationBoundary{Kind: "project", ProjectID: tid("audit-credential-project")},
	}
	identity := NewAuthenticatedUser(tid("audit-credential-user"), "u@test.com", "U", "member", "api")
	scoped := NewScopedUserIdentityWithDecoration(identity, tid("audit-credential-project"), []string{"project:read"}, tokenID, decoration)
	ctx := contextWithIdentity(context.Background(), scoped)
	return contextWithCredentialContext(ctx, credentialContextForIdentity(scoped))
}

func TestLogBrokerAuthEvent_CarriesCredentialWhenPresent(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	ctx := ctxWithTestDecoration("brokerauth-token", "ci-token")
	err := logger.LogBrokerAuthEvent(ctx, &BrokerAuthEvent{
		EventType: BrokerAuthEventRegister,
		BrokerID:  "broker-1",
		Success:   true,
	})
	require.NoError(t, err)

	rec := auditRecordWithMsg(t, buf, "Broker auth audit event")
	require.NotNil(t, rec, "no broker auth audit record was emitted")
	cred, ok := rec["credential"].(map[string]any)
	require.True(t, ok, "expected a credential group in the record: %v", rec)
	require.Equal(t, "brokerauth-token", cred["id"])
}

func TestLogInviteAuditEvent_CarriesCredentialWhenPresent(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	ctx := ctxWithTestDecoration("invite-token", "ci-token")
	err := logger.LogInviteAuditEvent(ctx, &InviteAuditEvent{
		EventType: InviteAuditUserInvited,
		Email:     "invitee@test.com",
		Success:   true,
	})
	require.NoError(t, err)

	rec := auditRecordWithMsg(t, buf, "authz: "+string(InviteAuditUserInvited))
	require.NotNil(t, rec, "no invite audit record was emitted")
	cred, ok := rec["credential"].(map[string]any)
	require.True(t, ok, "expected a credential group in the record: %v", rec)
	require.Equal(t, "invite-token", cred["id"])
}

func TestLogLifecycleHookEvent_CarriesCredentialWhenPresent(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	ctx := ctxWithTestDecoration("hook-token", "ci-token")
	err := logger.LogLifecycleHookEvent(ctx, &LifecycleHookEvent{
		EventType: LifecycleHookEventCreate,
		HookID:    "hook-1",
		HookName:  "on-start",
		Actor:     "u@test.com",
		Success:   true,
	})
	require.NoError(t, err)

	rec := auditRecordWithMsg(t, buf, "lifecycle hook audit event")
	require.NotNil(t, rec, "no lifecycle hook audit record was emitted")
	cred, ok := rec["credential"].(map[string]any)
	require.True(t, ok, "expected a credential group in the record: %v", rec)
	require.Equal(t, "hook-token", cred["id"])
}

func TestLogAgentSecretReadEvent_CarriesCredentialWhenPresent(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	ctx := ctxWithTestDecoration("secretread-token", "ci-token")
	err := logger.LogAgentSecretReadEvent(ctx, &AgentSecretReadEvent{
		AgentID:   "agent-1",
		ProjectID: "project-1",
		SecretKey: "MY_SECRET",
		Success:   true,
	})
	require.NoError(t, err)

	rec := auditRecordWithMsg(t, buf, "agent secret read event")
	require.NotNil(t, rec, "no agent secret read record was emitted")
	cred, ok := rec["credential"].(map[string]any)
	require.True(t, ok, "expected a credential group in the record: %v", rec)
	require.Equal(t, "secretread-token", cred["id"])
}

// TestLogAuditEvents_NoCredentialGroupWhenAbsent proves the helper adds
// nothing (not even an empty group) when ctx carries no decoration — the
// existing schema for every event type is unaffected for non-UAT credentials
// and for calls with no request context at all (e.g. from a background job).
func TestLogAuditEvents_NoCredentialGroupWhenAbsent(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	err := logger.LogAgentSecretReadEvent(context.Background(), &AgentSecretReadEvent{
		AgentID:   "agent-1",
		ProjectID: "project-1",
		SecretKey: "MY_SECRET",
		Success:   true,
	})
	require.NoError(t, err)

	rec := auditRecordWithMsg(t, buf, "agent secret read event")
	require.NotNil(t, rec)
	_, ok := rec["credential"]
	require.False(t, ok, "no credential group should be emitted when ctx carries no decoration: %v", rec)
}
