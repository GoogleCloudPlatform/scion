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
	"testing"
	"testing/synctest"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin that the suspend path records the agent_credential_revoke
// mutation audit only after the revoke succeeded (ptone/scion#2403): a
// failed revoke leaves the operational warning and no record claiming the
// credentials were revoked; a successful one leaves exactly one record.
//
// emitMutationAudit writes in a fire-and-forget goroutine. Each call under
// test runs in a synctest bubble, so synctest.Wait returns only once every
// goroutine the call started has finished: a record that was going to be
// written has been written, and its absence is not a matter of timing.
//
// The call runs under a context carrying the suspending user's identity, as
// the suspend handler's request context does: a mutation audit record needs
// an actor (actor_principal_kind and actor_principal_id are NotEmpty in the
// schema), so under a bare context.Background() no record could ever be
// written and the tests would prove nothing.

var errSuspendRevokeFault = errors.New("injected suspend revoke fault")

// suspendRevokeFaultStore makes RevokeAgentCredentialsByAgent fail while its
// switch is armed, without touching the stored credentials. Every other
// method, the mutation audit write included, reaches the real store.
type suspendRevokeFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *suspendRevokeFaultStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, revokedBy, reason string) (int, error) {
	if !f.fault.Active() {
		return f.Store.RevokeAgentCredentialsByAgent(ctx, agentID, revokedBy, reason)
	}
	return 0, errSuspendRevokeFault
}

// suspendRevokeFixture is a test server on the real SQLite store with an
// agent holding live credentials.
type suspendRevokeFixture struct {
	srv     *Server
	s       store.Store // unwrapped store
	user    *store.User // the agent's owner, who suspends it
	agentID string
	tokens  []string
	fault   *storeFaultSwitch
}

// setupSuspendRevokeAuditTest builds a test server on the real SQLite store,
// wrapped before setup in a disarmed suspendRevokeFaultStore, and seeds an
// agent holding n live credentials.
func setupSuspendRevokeAuditTest(t *testing.T, n int) *suspendRevokeFixture {
	t.Helper()
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *suspendRevokeFaultStore {
		return &suspendRevokeFaultStore{Store: inner, fault: fault}
	})
	srv, s, user, project := setupCredentialTestServerOn(t, srv, s)

	agentID := tid("agent-susp-audit")
	createCredTestAgent(t, s, agentID, project.ID, user.ID)
	tokens := make([]string, 0, n)
	for i := 0; i < n; i++ {
		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		tokens = append(tokens, token)
	}
	return &suspendRevokeFixture{srv: srv, s: s, user: user, agentID: agentID, tokens: tokens, fault: fault}
}

// runSuspendRevoke calls revokeSuspendedCredentials as f.user in a synctest
// bubble and returns once every goroutine it started (the audit write) has
// finished. It also returns what was logged through the default slog logger
// meanwhile.
//
// Invariant: no goroutine outside the bubble may use the store while this
// runs. The test store has a single connection (MaxOpenConns 1); if an
// outside goroutine held it, the bubble's wait for it would either panic
// (database/sql hands the connection over on a channel made in the bubble)
// or block durably, letting synctest.Wait return before the audit write.
// Setup is synchronous and the test server starts no background services,
// so nothing outside the bubble touches the store here.
func runSuspendRevoke(t *testing.T, f *suspendRevokeFixture) string {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// The suspending user's identity, as the suspend handler's request
	// context carries it; built outside the bubble.
	identity := NewAuthenticatedUser(f.user.ID, f.user.Email, f.user.DisplayName, f.user.Role, "api")
	ctx := contextWithIdentity(context.Background(), identity)
	synctest.Test(t, func(t *testing.T) {
		f.srv.revokeSuspendedCredentials(ctx, f.agentID)
		synctest.Wait()
	})
	return logs.String()
}

// suspendRevokeAudits lists the stored agent_credential_revoke records for
// agentID.
func suspendRevokeAudits(t *testing.T, s store.Store, agentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: "agent_credential_revoke",
		TargetID:     agentID,
	})
	require.NoError(t, err)
	return recs
}

// assertCredentialsRevoked checks whether every token's stored credential is
// revoked (want true) or still live (want false).
func assertCredentialsRevoked(t *testing.T, srv *Server, s store.Store, tokens []string, want bool) {
	t.Helper()
	for i, token := range tokens {
		claims, err := srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)
		cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)
		if want {
			require.NotNil(t, cred.RevokedAt, "credential %d should be revoked", i)
			require.NotNil(t, cred.RevokeReason)
			assert.Equal(t, "agent_suspended", *cred.RevokeReason)
		} else {
			assert.Nil(t, cred.RevokedAt, "credential %d should still be live", i)
		}
	}
}

// TestSuspendRevokeFailureRecordsNoRevokeAudit: a failed suspend revoke
// keeps its warning and writes no agent_credential_revoke record.
func TestSuspendRevokeFailureRecordsNoRevokeAudit(t *testing.T) {
	f := setupSuspendRevokeAuditTest(t, 2)
	require.Empty(t, suspendRevokeAudits(t, f.s, f.agentID), "setup must not record a revoke")

	f.fault.Arm()
	logs := runSuspendRevoke(t, f)

	assert.Empty(t, suspendRevokeAudits(t, f.s, f.agentID),
		"a failed revoke must not be recorded as a committed agent_credential_revoke")
	assertCredentialsRevoked(t, f.srv, f.s, f.tokens, false)

	var warned bool
	for _, line := range bytes.Split([]byte(logs), []byte("\n")) {
		var l struct {
			Level   string `json:"level"`
			Msg     string `json:"msg"`
			AgentID string `json:"agent_id"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(line, &l) != nil || l.Msg != "Failed to revoke agent credentials on suspend" {
			continue
		}
		warned = true
		assert.Equal(t, "WARN", l.Level)
		assert.Equal(t, f.agentID, l.AgentID)
		assert.Equal(t, errSuspendRevokeFault.Error(), l.Error)
	}
	assert.True(t, warned, "the revoke failure warning must still be logged; logs:\n%s", logs)
	assert.NotContains(t, logs, "failed to emit mutation audit record")
	for i, token := range f.tokens {
		assert.NotContains(t, logs, token, "token %d must not appear in the logs", i)
	}
}

// TestSuspendRevokeSuccessRecordsOneRevokeAudit: a successful suspend revoke
// revokes every credential of the agent and writes exactly one
// agent_credential_revoke record for it, with its existing fields.
func TestSuspendRevokeSuccessRecordsOneRevokeAudit(t *testing.T) {
	f := setupSuspendRevokeAuditTest(t, 2)
	require.Empty(t, suspendRevokeAudits(t, f.s, f.agentID), "setup must not record a revoke")

	logs := runSuspendRevoke(t, f)

	assertCredentialsRevoked(t, f.srv, f.s, f.tokens, true)
	recs := suspendRevokeAudits(t, f.s, f.agentID)
	require.Len(t, recs, 1, "a successful revoke must be recorded exactly once")
	assert.Equal(t, "agent_credential_revoke", recs[0].MutationType)
	assert.Equal(t, "agent_credential", recs[0].TargetType)
	assert.Equal(t, f.agentID, recs[0].TargetID)
	assert.Equal(t, "user", recs[0].ActorPrincipalKind)
	assert.Equal(t, f.user.ID, recs[0].ActorPrincipalID, "the record names the suspending user")
	assert.NotContains(t, logs, "Failed to revoke agent credentials on suspend")
	assert.NotContains(t, logs, "failed to emit mutation audit record",
		"the audit write must not fail silently")
}
