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

//go:build !no_sqlite && (!hubshard || hubshard_6)

package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedHoldStore answers HasActiveAgentHold with held or err.
type fixedHoldStore struct {
	store.AgentHoldStore
	held bool
	err  error
}

func (f fixedHoldStore) HasActiveAgentHold(context.Context, string) (bool, error) {
	return f.held, f.err
}

// TestAgentAuthCredentialFaultBeforeHold pins the order of the agent
// token checks: the credential status is evaluated before the hold, so a
// credential-store fault answers 503 even for a held agent.
func TestAgentAuthCredentialFaultBeforeHold(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	agentID := tid("agent-auth-credfault-held")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	srv.authConfig.CredentialStore = &erroringCredentialStore{Store: s, err: errors.New("credential store unavailable for testing")}
	srv.authConfig.HoldStore = fixedHoldStore{AgentHoldStore: s, held: true}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeUnavailable, decodeAPIError(t, rec.Body.Bytes()).Error.Code)
}

// TestAgentAuthRevokedBeforeHoldFault: a revoked credential answers 401
// before the hold is looked up, so a hold-lookup fault does not turn it
// into a 503.
func TestAgentAuthRevokedBeforeHoldFault(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()
	agentID := tid("agent-auth-revoked-holdfault")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
	require.NoError(t, err)
	require.NoError(t, s.RevokeAgentCredential(ctx, cred.ID, "test", "explicit"))

	srv.authConfig.HoldStore = fixedHoldStore{AgentHoldStore: s, err: errors.New("hold lookup unavailable for testing")}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeUnauthorized, decodeAPIError(t, rec.Body.Bytes()).Error.Code)
}
