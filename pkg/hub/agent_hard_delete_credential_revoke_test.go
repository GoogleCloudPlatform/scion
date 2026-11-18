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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errInjectedHardDeleteRevokeFault = errors.New("injected hard-delete credential revoke fault")

// txRevokeFaultStore wraps a store.Store and propagates the wrapper into WithTx
// and FinalizeAgentDeletion's hook so an armed fault fails
// RevokeAgentCredentialsByAgent inside the hard-delete transaction too.
type txRevokeFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *txRevokeFaultStore) wrap(tx store.Store) *txRevokeFaultStore {
	return &txRevokeFaultStore{Store: tx, fault: s.fault}
}

func (s *txRevokeFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(s.wrap(tx))
	})
}

func (s *txRevokeFaultStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	var wrapped store.DeletionFinalizeHook
	if hook != nil {
		wrapped = func(ctx context.Context, tx store.Store, a *store.Agent, m store.DeletionFinalizeMode) error {
			return hook(ctx, s.wrap(tx), a, m)
		}
	}
	return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, wrapped)
}

func (s *txRevokeFaultStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, revokedBy, reason string) (int, error) {
	if s.fault.Active() {
		return 0, errInjectedHardDeleteRevokeFault
	}
	return s.Store.RevokeAgentCredentialsByAgent(ctx, agentID, revokedBy, reason)
}

func setupHardDeleteRevokeAgent(t *testing.T, srv *Server, st store.Store, suffix string) (*store.Agent, string, string) {
	t.Helper()
	ctx := context.Background()

	userID := tid("user-hdrev-" + suffix)
	require.NoError(t, st.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "hdrev-" + suffix + "@example.com",
		DisplayName: "Hard Delete Revoke User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}))

	projectID := tid("proj-hdrev-" + suffix)
	require.NoError(t, st.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "hdrev-" + suffix,
		Slug: "hdrev-" + suffix,
	}))

	agent := &store.Agent{
		ID:        tid("agent-hdrev-" + suffix),
		Slug:      "agent-hdrev-" + suffix,
		Name:      "agent-hdrev-" + suffix,
		ProjectID: projectID,
		Template:  "default",
		Phase:     string(state.PhaseCreated),
		Ancestry:  []string{userID},
		OwnerID:   userID,
		CreatedBy: userID,
	}
	require.NoError(t, st.CreateAgent(ctx, agent))
	addProjectEdge(t, st, store.DelegationPrincipalUser, userID, agent.ID, projectID)

	token, err := srv.GenerateAgentToken(agent.ID, projectID, []string{userID}, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	return agent, token, claims.ID
}

func assertTokenRefusedByMiddleware(t *testing.T, srv *Server, agentID, token string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "revoked credential must be refused at authentication: %s", rec.Body.String())
}

func TestHardDeleteAgentTx_RevokesCredentialsAtomically(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	agent, token, jti := setupHardDeleteRevokeAgent(t, srv, st, "hardtx-ok")

	n, err := st.FinalizeAgentDeletion(ctx, agent.ID, createRowCompensable(), store.DeletionFinalizeHard, store.DeletionFields{},
		createRowHeldCheck(func(tx store.Store) error {
			return srv.hardDeleteAgentTx(ctx, tx, agent, AuditActor{})
		}))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, err = st.GetAgent(ctx, agent.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "hardDeleteAgentTx finalize removes the agent row")

	cred := getTestAgentCredential(t, st, jti)
	require.NotNil(t, cred.RevokedAt, "hardDeleteAgentTx must revoke active credentials in the hard-delete transaction")
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonDeleted, *cred.RevokeReason)
	assertTokenRefusedByMiddleware(t, srv, agent.ID, token)
}

func TestCompensateAgentCreate_RevokesCredentialsAtomically(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	agent, token, jti := setupHardDeleteRevokeAgent(t, srv, st, "comp-ok")

	err := srv.compensateAgentCreate(ctx, createCompensation{
		Agent: agent,
		Stage: createStageDispatch,
		Cause: errors.New("dispatch failed"),
	})
	require.NoError(t, err)

	_, err = st.GetAgent(ctx, agent.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "compensation removes the agent row")

	cred := getTestAgentCredential(t, st, jti)
	require.NotNil(t, cred.RevokedAt, "compensateAgentCreate must revoke active credentials in the hard-delete transaction")
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
	assertTokenRefusedByMiddleware(t, srv, agent.ID, token)
}

func TestCompensateAgentCreate_RevokeFailureRollsBackDelete(t *testing.T) {
	srv, st, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *txRevokeFaultStore {
		return &txRevokeFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	agent, _, jti := setupHardDeleteRevokeAgent(t, srv, st, "comp-fail")

	fault.Arm()
	err := srv.compensateAgentCreate(ctx, createCompensation{
		Agent: agent,
		Stage: createStageDispatch,
		Cause: errors.New("dispatch failed"),
	})
	require.ErrorIs(t, err, errInjectedHardDeleteRevokeFault)

	row, gerr := st.GetAgent(ctx, agent.ID)
	require.NoError(t, gerr, "agent row must survive when credential revocation fails")
	assert.True(t, row.DeletedAt.IsZero())
	assert.Len(t, activeEdgesFor(t, st, agent.ID), 1, "delegation edge deactivation must roll back with the row delete")

	cred := getTestAgentCredential(t, st, jti)
	assert.Nil(t, cred.RevokedAt)
}

func TestDeleteFailedCreateRow_RevokesCredentialsAtomically(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	agent, token, jti := setupHardDeleteRevokeAgent(t, srv, st, "fallback-ok")

	err := srv.deleteFailedCreateRow(ctx, agent, "op-fallback")
	require.NoError(t, err)

	_, err = st.GetAgent(ctx, agent.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "fallback removes the agent row")

	cred := getTestAgentCredential(t, st, jti)
	require.NotNil(t, cred.RevokedAt, "deleteFailedCreateRow must revoke active credentials in the hard-delete transaction")
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
	assertTokenRefusedByMiddleware(t, srv, agent.ID, token)
}

func TestCleanupFailedCreate_RevokeFailureRollsBackRowRemoval(t *testing.T) {
	srv, st, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *txRevokeFaultStore {
		return &txRevokeFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	agent, _, _ := setupHardDeleteRevokeAgent(t, srv, st, "cleanup-fail")

	fault.Arm()
	corrID := srv.cleanupFailedCreate(ctx, createRollback{
		Agent:             agent,
		Stage:             createStageDispatch,
		Cause:             errors.New("dispatch failed"),
		RevokeCredentials: true,
	})
	assert.NotEmpty(t, corrID, "failed compensation must return a correlation ID")

	row, err := st.GetAgent(ctx, agent.ID)
	require.NoError(t, err, "agent row must not be deleted when credential revocation fails across compensation and fallback")
	assert.Equal(t, string(state.PhaseError), row.Phase)
	assert.Len(t, activeEdgesFor(t, st, agent.ID), 1, "surviving row keeps its active delegation edge")
}

func TestCreateAgent_EnvGatherRecreate_RevokeFailureRollsBackHardDelete(t *testing.T) {
	srv, st, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *txRevokeFaultStore {
		return &txRevokeFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	projectID := tid("project-recreate-revoke-fail")
	require.NoError(t, st.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "recreate-revoke-fail", Slug: "recreate-revoke-fail",
	}))
	brokerID := tid("broker-recreate-revoke-fail")
	require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "recreate-revoke-fail-broker", Slug: "recreate-revoke-fail-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "test-broker", LocalPath: "/tmp/test-project",
	}))

	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"GEMINI_API_KEY"},
			Needs:    []string{"GEMINI_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	gen := &fakeMintingTokenGenerator{store: st}
	dispatcher.SetTokenGenerator(gen)
	srv.SetDispatcher(dispatcher)

	reqBody := map[string]any{
		"name":      "recreate-revoke-fail-agent",
		"projectId": projectID,
		"template":  "claude",
		"gatherEnv": true,
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
	var resp1 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	oldAgentID := resp1.Agent.ID
	require.Len(t, gen.jtis, 1)

	// Arm the revoke fault: tearing down the existing provisioning agent must
	// roll back the hard delete and fail rather than removing the row with an
	// active credential or recreating over it.
	fault.Arm()
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)
	require.Equal(t, http.StatusInternalServerError, rec2.Code, rec2.Body.String())

	surviving, err := st.GetAgent(ctx, oldAgentID)
	require.NoError(t, err, "existing provisioning agent row must survive when credential revocation fails")
	assert.Equal(t, string(state.PhaseProvisioning), surviving.Phase)
	assert.Len(t, gen.jtis, 1, "recreate must not mint a replacement agent credential after rolling back")
}
