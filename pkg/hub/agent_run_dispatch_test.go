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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runTokenGenerator signs real agent tokens. A non-empty jtiHash replaces
// the credential's hash, so a test can make the credential insert collide
// with an existing row.
type runTokenGenerator struct {
	svc     *AgentTokenService
	jtiHash string
}

func (g runTokenGenerator) GenerateAgentToken(string, string, []string, AgentRole, []AgentTokenScope) (string, error) {
	panic("runTokenGenerator: dispatch paths must not use GenerateAgentToken")
}

func (g runTokenGenerator) AuthorizeAgentToken(_ context.Context, agent *store.Agent) (AgentTokenGrant, error) {
	return AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID}, nil
}

func (g runTokenGenerator) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	token, cred, err := g.svc.SignAgentToken(grant, runID)
	if err == nil && g.jtiHash != "" {
		cred.TokenJTIHash = g.jtiHash
	}
	return token, cred, err
}

// runDispatchFixture is a dispatcher on a real store holding a project, an
// online broker and a stored agent row whose current run is "run-before".
type runDispatchFixture struct {
	store  store.Store
	client *mockRuntimeBrokerClient
	d      *HTTPAgentDispatcher
	svc    *AgentTokenService
	agent  *store.Agent
}

func newRunDispatchFixture(t *testing.T) *runDispatchFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-run"), Name: "run-project", Slug: "run-project",
		GitRemote: "https://github.com/example/run.git",
	}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("broker-run"), Name: "run-broker", Slug: "run-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: tid("project-run"), BrokerID: tid("broker-run"), BrokerName: "run-broker",
		LocalPath: "/home/user/projects/run/.scion", Status: store.BrokerStatusOnline,
	}))
	agent := &store.Agent{
		ID: tid("agent-run"), Name: "run-agent", Slug: "run-agent",
		ProjectID: tid("project-run"), OwnerID: tid("owner-run"), RuntimeBrokerID: tid("broker-run"),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "task"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-before", nil)
	require.NoError(t, err)
	agent, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	svc, err := NewAgentTokenService(AgentTokenConfig{})
	require.NoError(t, err)
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetTokenGenerator(runTokenGenerator{svc: svc})
	return &runDispatchFixture{store: s, client: client, d: d, svc: svc, agent: agent}
}

// runDispatches are the dispatch paths that begin a run. Each returns the
// token and run id the broker received, and whether the broker was called.
var runDispatches = []struct {
	name     string
	dispatch func(ctx context.Context, f *runDispatchFixture) error
	sent     func(f *runDispatchFixture) (token, runID string, called bool)
}{
	{
		name: "create",
		dispatch: func(ctx context.Context, f *runDispatchFixture) error {
			_, err := f.d.DispatchAgentCreate(ctx, f.agent)
			return err
		},
		sent: func(f *runDispatchFixture) (string, string, bool) {
			if f.client.lastCreateReq == nil {
				return "", "", f.client.createCalled
			}
			return f.client.lastCreateReq.AgentToken, f.client.lastCreateReq.RunID, f.client.createCalled
		},
	},
	{
		name: "start",
		dispatch: func(ctx context.Context, f *runDispatchFixture) error {
			return f.d.DispatchAgentStart(ctx, f.agent, "", false)
		},
		sent: func(f *runDispatchFixture) (string, string, bool) {
			return f.client.lastResolvedEnv["SCION_AUTH_TOKEN"], f.client.lastStartExtras.RunID, f.client.startCalled
		},
	},
	{
		name:     "restart",
		dispatch: func(ctx context.Context, f *runDispatchFixture) error { return f.d.DispatchAgentRestart(ctx, f.agent) },
		sent: func(f *runDispatchFixture) (string, string, bool) {
			return f.client.lastRestartResolvedEnv["SCION_AUTH_TOKEN"], f.client.lastRestartExtras.RunID, f.client.restartCalled
		},
	},
}

// TestDispatchIssuesTokenForTheDispatchedRun: at create, start and restart
// the token the broker receives names the run sent with it, and its
// credential row records the same run.
func TestDispatchIssuesTokenForTheDispatchedRun(t *testing.T) {
	for _, tc := range runDispatches {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRunDispatchFixture(t)
			require.NoError(t, tc.dispatch(ctx, f))

			token, runID, called := tc.sent(f)
			require.True(t, called)
			require.NotEmpty(t, token)
			require.NotEmpty(t, runID)
			assert.NotEqual(t, "run-before", runID)

			claims, err := f.svc.ValidateAgentToken(token)
			require.NoError(t, err)
			assert.Equal(t, runID, claims.RunID, "token run")

			cred, err := f.store.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
			require.NoError(t, err)
			assert.Equal(t, runID, cred.RunID, "credential run")

			got, err := f.store.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			assert.Equal(t, runID, got.RunID, "agent run")
		})
	}
}

// TestDispatchRecordsRunAndCredentialInOneTransaction: when the credential
// cannot be recorded, the agent's run is not changed and the broker is not
// called, on every path that begins a run.
func TestDispatchRecordsRunAndCredentialInOneTransaction(t *testing.T) {
	for _, tc := range runDispatches {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRunDispatchFixture(t)

			// An existing credential row with the hash the next token's
			// credential will carry: the insert violates the unique index.
			taken := hashJTI(uuid.NewString())
			require.NoError(t, f.store.CreateAgentCredential(ctx, &store.AgentCredential{
				AgentID: f.agent.ID, ProjectID: f.agent.ProjectID, TokenJTIHash: taken,
			}))
			f.d.SetTokenGenerator(runTokenGenerator{svc: f.svc, jtiHash: taken})

			require.Error(t, tc.dispatch(ctx, f))

			_, _, called := tc.sent(f)
			assert.False(t, called, "broker must not be called")
			got, err := f.store.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			assert.Equal(t, "run-before", got.RunID, "agent run must be unchanged")
		})
	}
}
