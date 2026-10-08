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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransportMinter is a TransportTokenMinter for tests.
type fakeTransportMinter struct {
	token string
	err   error
	calls int
}

func (m *fakeTransportMinter) MintIDToken(_ context.Context, _ string) (string, time.Time, error) {
	m.calls++
	if m.err != nil {
		return "", time.Time{}, m.err
	}
	return m.token, time.Now().Add(time.Hour), nil
}

type refreshResponseForTest struct {
	Token          string              `json:"token"`
	Tokens         []RefreshTokenEntry `json:"tokens"`
	TransportError string              `json:"transportError"`
}

func runTransportRefresh(t *testing.T, minter TransportTokenMinter, audience string) refreshResponseForTest {
	t.Helper()
	srv, s, _, project := setupCredentialTestServer(t)
	srv.transportMinter = minter
	srv.transportAudience = audience

	agentID := tid("agent-refresh-transport")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, buildAgentRefreshRequest(agentID, claims, "", false), agentID)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp refreshResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func transportEntries(resp refreshResponseForTest) []RefreshTokenEntry {
	var out []RefreshTokenEntry
	for _, e := range resp.Tokens {
		if e.Layer == "transport" {
			out = append(out, e)
		}
	}
	return out
}

func TestAgentRefresh_TransportMinted(t *testing.T) {
	minter := &fakeTransportMinter{token: "fake-transport-value"}
	resp := runTransportRefresh(t, minter, "https://hub.example.test")

	assert.Equal(t, 1, minter.calls)
	assert.Empty(t, resp.TransportError)
	entries := transportEntries(resp)
	require.Len(t, entries, 1)
	assert.Equal(t, "https://hub.example.test", entries[0].Audience)
}

func TestAgentRefresh_TransportMintFailureIsReported(t *testing.T) {
	minter := &fakeTransportMinter{err: errors.New("iam: permission denied on internal-sa@example")}
	resp := runTransportRefresh(t, minter, "https://hub.example.test")

	// The refresh still succeeds with an app token...
	assert.NotEmpty(t, resp.Token)
	assert.Empty(t, transportEntries(resp))
	// ...and the agent is told the transport mint failed, with a fixed
	// message that does not carry the underlying error.
	assert.Equal(t, TransportMintFailedMessage, resp.TransportError)
	assert.NotContains(t, resp.TransportError, "internal-sa")
}

func TestAgentRefresh_TransportMintEmptyIsReported(t *testing.T) {
	resp := runTransportRefresh(t, &fakeTransportMinter{token: ""}, "https://hub.example.test")
	assert.Empty(t, transportEntries(resp))
	assert.Equal(t, TransportMintFailedMessage, resp.TransportError)
}

func TestAgentRefresh_NoTransportMinterOmitsField(t *testing.T) {
	resp := runTransportRefresh(t, nil, "")
	assert.Empty(t, transportEntries(resp))
	assert.Empty(t, resp.TransportError)
}

type staticTokenGenerator struct{ token string }

func (g staticTokenGenerator) GenerateAgentToken(string, string, []string, AgentRole, []AgentTokenScope) (string, error) {
	return g.token, nil
}

func (g staticTokenGenerator) AuthorizeAgentToken(_ context.Context, agent *store.Agent) (AgentTokenGrant, error) {
	return AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID}, nil
}

func (g staticTokenGenerator) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	return g.token, &store.AgentCredential{AgentID: grant.AgentID, ProjectID: grant.ProjectID, TokenJTIHash: hashJTI(uuid.NewString()), RunID: runID}, nil
}

func newResetAuthDispatcher(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("host-1"), Name: "test-host", Slug: "test-host",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	d.SetTokenGenerator(staticTokenGenerator{token: "fake-app-value"})
	agent := &store.Agent{
		ID: tid("agent-1"), Name: "test-agent", Slug: "test-agent",
		ProjectID: tid("project-1"), RuntimeBrokerID: tid("host-1"),
	}
	return d, mockClient, agent
}

func TestDispatchAgentResetAuth_PushesTransportToken(t *testing.T) {
	d, mockClient, agent := newResetAuthDispatcher(t)
	d.SetTransportMinter(&fakeTransportMinter{token: "fake-transport-value"}, "https://hub.example.test", "iap")

	require.NoError(t, d.DispatchAgentResetAuth(context.Background(), agent))
	assert.True(t, mockClient.resetAuthCalled)
	assert.Equal(t, "fake-app-value", mockClient.lastResetToken)
	assert.Equal(t, "fake-transport-value", mockClient.lastResetTransportToken)
}

func TestDispatchAgentResetAuth_MintFailureStillResetsAppToken(t *testing.T) {
	d, mockClient, agent := newResetAuthDispatcher(t)
	d.SetTransportMinter(&fakeTransportMinter{err: errors.New("mint failed")}, "https://hub.example.test", "iap")

	require.NoError(t, d.DispatchAgentResetAuth(context.Background(), agent))
	assert.Equal(t, "fake-app-value", mockClient.lastResetToken)
	assert.Empty(t, mockClient.lastResetTransportToken)
}

func TestDispatchAgentResetAuth_NoMinterNoTransportToken(t *testing.T) {
	d, mockClient, agent := newResetAuthDispatcher(t)

	require.NoError(t, d.DispatchAgentResetAuth(context.Background(), agent))
	assert.Equal(t, "fake-app-value", mockClient.lastResetToken)
	assert.Empty(t, mockClient.lastResetTransportToken)
}

func TestResetAuthBody(t *testing.T) {
	assert.Equal(t, map[string]string{"token": "a"}, resetAuthBody("a", ""))
	assert.Equal(t, map[string]string{"token": "a", "transportToken": "b"}, resetAuthBody("a", "b"))
}
