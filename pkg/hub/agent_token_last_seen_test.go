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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lastSeenVerifyFixture is a credential test server whose agent-token
// verification writes last_seen_at through a recorder, throttled on a
// fake clock.
type lastSeenVerifyFixture struct {
	srv   *Server
	rec   *credLastSeenRecorder
	clk   *credLastSeenClock
	token string
	jti   string
}

func newLastSeenVerifyFixture(t *testing.T, name string) *lastSeenVerifyFixture {
	t.Helper()
	srv, s, _, project := setupCredentialTestServer(t)
	agentID := tid("agent-last-seen-" + name)
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	require.NotNil(t, srv.authConfig.LastSeenThrottle, "the server throttles last_seen_at")
	rec := &credLastSeenRecorder{AgentCredentialStore: s}
	srv.authConfig.CredentialStore = rec
	th, clk := testCredLastSeenThrottle(agentCredentialLastSeenMax)
	srv.authConfig.LastSeenThrottle = th
	return &lastSeenVerifyFixture{srv: srv, rec: rec, clk: clk, token: token, jti: claims.ID}
}

// verify sends one agent-token request and requires it authenticated
// (neither the revoked 401 nor the unevaluated 503).
func (f *lastSeenVerifyFixture) verify(t *testing.T) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+tid("agent-last-seen-target"), nil)
	req.Header.Set("X-Scion-Agent-Token", f.token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
	require.NotEqual(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
}

// TestAgentTokenVerify_LastSeenThrottled: the first verify of a token
// writes last_seen_at; a second within the interval does not; one after
// the interval does.
func TestAgentTokenVerify_LastSeenThrottled(t *testing.T) {
	f := newLastSeenVerifyFixture(t, "throttled")
	t0 := f.clk.now()

	f.verify(t)
	writes := f.rec.writes()
	require.Len(t, writes, 1)
	assert.Equal(t, t0, writes[0].at)
	cred, err := f.srv.store.GetAgentCredentialByJTIHash(context.Background(), hashJTI(f.jti))
	require.NoError(t, err)
	assert.Equal(t, cred.ID, writes[0].id)
	require.NotNil(t, cred.LastSeenAt, "the write reached the store")
	assert.WithinDuration(t, t0, *cred.LastSeenAt, time.Second)

	f.clk.advance(agentCredentialLastSeenInterval / 2)
	f.verify(t)
	assert.Len(t, f.rec.writes(), 1, "no write within the interval")

	f.clk.advance(agentCredentialLastSeenInterval / 2)
	f.verify(t)
	writes = f.rec.writes()
	require.Len(t, writes, 2, "a write after the interval")
	assert.Equal(t, t0.Add(agentCredentialLastSeenInterval), writes[1].at)
}

// TestAgentTokenVerify_LastSeenWriteErrorDoesNotFail: a failed
// last_seen_at write does not fail the request.
func TestAgentTokenVerify_LastSeenWriteErrorDoesNotFail(t *testing.T) {
	f := newLastSeenVerifyFixture(t, "write-error")
	f.rec.err = errors.New("last_seen write failed for testing")
	f.verify(t)
	assert.Len(t, f.rec.writes(), 1, "the write was attempted")
}
