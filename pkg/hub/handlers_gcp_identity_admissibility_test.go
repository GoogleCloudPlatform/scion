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
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingSAUpdateStore wraps a real store and, while failUpdate is set,
// makes UpdateGCPServiceAccount fail. It exercises the verification-result
// persist path of every verify and auto-verify handler.
type failingSAUpdateStore struct {
	store.Store
	failUpdate atomic.Bool
	updates    atomic.Int32
}

func (f *failingSAUpdateStore) UpdateGCPServiceAccount(ctx context.Context, sa *store.GCPServiceAccount) error {
	f.updates.Add(1)
	if f.failUpdate.Load() {
		return errors.New("injected update failure")
	}
	return f.Store.UpdateGCPServiceAccount(ctx, sa)
}

// DB forwards to the wrapped store's raw *sql.DB so New()'s migrations that
// type-assert for it still run against the real store.
func (f *failingSAUpdateStore) DB() *sql.DB {
	if p, ok := f.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func newFailingSAUpdateServer(t *testing.T) (*Server, *failingSAUpdateStore) {
	t.Helper()
	base, err := newTestStore(":memory:")
	require.NoError(t, err)
	wrapped := &failingSAUpdateStore{Store: base}
	srv, _ := testServerWithStore(t, wrapped)
	return srv, wrapped
}

// TestGCPVerificationResult_PersistFailureSurfaces covers all four
// verify/auto-verify sites (project create, hub-scoped create, nested
// verify, flat by-id verify) for both verification outcomes, with the
// persist succeeding and failing. A persist failure must answer 500 and
// never report a verification outcome the store does not hold.
func TestGCPVerificationResult_PersistFailureSurfaces(t *testing.T) {
	createProject := func(t *testing.T, srv *Server, s *failingSAUpdateStore, failPersist bool) (int, string, string) {
		projectID := createTestProjectForSA(t, srv, nil)
		s.failUpdate.Store(failPersist)
		rec := doRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
			map[string]string{"email": "agent@my-project.iam.gserviceaccount.com", "projectId": "my-project"})
		return rec.Code, rec.Body.String(), ""
	}
	createHub := func(t *testing.T, srv *Server, s *failingSAUpdateStore, failPersist bool) (int, string, string) {
		s.failUpdate.Store(failPersist)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/gcp-service-accounts?scope=hub",
			map[string]string{"email": "hubsa@my-project.iam.gserviceaccount.com", "projectId": "my-project"})
		return rec.Code, rec.Body.String(), ""
	}
	verifyNested := func(t *testing.T, srv *Server, s *failingSAUpdateStore, failPersist bool) (int, string, string) {
		projectID := createTestProjectForSA(t, srv, nil)
		sa := mkSA(t, s.Store, "sa-adm-nested", "nested@p.iam.gserviceaccount.com", store.ScopeProject, projectID, "dev")
		s.failUpdate.Store(failPersist)
		rec := doRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, sa.ID), nil)
		return rec.Code, rec.Body.String(), sa.ID
	}
	verifyFlat := func(t *testing.T, srv *Server, s *failingSAUpdateStore, failPersist bool) (int, string, string) {
		sa := mkSA(t, s.Store, "sa-adm-flat", "flat@p.iam.gserviceaccount.com", store.ScopeHub, srv.HubID(), "dev")
		s.failUpdate.Store(failPersist)
		rec := doRequest(t, srv, http.MethodPost, flatSAPath+sa.ID+"/verify", nil)
		return rec.Code, rec.Body.String(), sa.ID
	}

	sites := []struct {
		name               string
		run                func(*testing.T, *Server, *failingSAUpdateStore, bool) (int, string, string)
		okCode, okFailCode int
	}{
		{"project create", createProject, http.StatusCreated, http.StatusCreated},
		{"hub create", createHub, http.StatusCreated, http.StatusCreated},
		{"nested verify", verifyNested, http.StatusOK, http.StatusBadGateway},
		{"flat verify", verifyFlat, http.StatusOK, http.StatusBadGateway},
	}

	for _, st := range sites {
		for _, verifyFails := range []bool{false, true} {
			for _, persistFails := range []bool{false, true} {
				name := fmt.Sprintf("%s/verifyFails=%v/persistFails=%v", st.name, verifyFails, persistFails)
				t.Run(name, func(t *testing.T) {
					srv, s := newFailingSAUpdateServer(t)
					if verifyFails {
						srv.SetGCPTokenGenerator(&mockGCPTokenGeneratorVerifyFail{
							email: "hub@test.iam.gserviceaccount.com", verifyErr: errors.New("cannot impersonate"),
						})
					} else {
						srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
					}

					code, body, saID := st.run(t, srv, s, persistFails)
					assert.Positive(t, s.updates.Load(), "the verification result must be written")

					if persistFails {
						require.Equal(t, http.StatusInternalServerError, code, body)
						assert.Contains(t, body, ErrCodeInternalError)
						assert.NotContains(t, body, `"verificationStatus"`, "no outcome reported when it was not stored")
						assert.Contains(t, body, `"serviceAccountId"`, "the existing account is named so it can be re-verified")
						assert.Contains(t, body, "re-run verification on the existing account")
						if saID != "" {
							stored, err := s.GetGCPServiceAccount(context.Background(), saID)
							require.NoError(t, err)
							assert.False(t, gcpServiceAccountVerified(stored), "stored row is unchanged")
						}
						return
					}

					want := st.okCode
					if verifyFails {
						want = st.okFailCode
					}
					require.Equal(t, want, code, body)
					if saID != "" {
						stored, err := s.GetGCPServiceAccount(context.Background(), saID)
						require.NoError(t, err)
						assert.Equal(t, !verifyFails, gcpServiceAccountVerified(stored))
						if verifyFails {
							assert.Equal(t, store.GCPVerificationFailed, stored.VerificationStatus)
						}
					} else if verifyFails {
						assert.Contains(t, body, `"verificationStatus":"failed"`)
					} else {
						assert.Contains(t, body, `"verificationStatus":"verified"`)
					}
				})
			}
		}
	}
}

func TestGCPServiceAccountVerified(t *testing.T) {
	assert.False(t, gcpServiceAccountVerified(nil))
	assert.True(t, gcpServiceAccountVerified(&store.GCPServiceAccount{Verified: true, VerificationStatus: store.GCPVerificationVerified}))
	assert.False(t, gcpServiceAccountVerified(&store.GCPServiceAccount{Verified: true, VerificationStatus: store.GCPVerificationFailed}))
	assert.False(t, gcpServiceAccountVerified(&store.GCPServiceAccount{Verified: false, VerificationStatus: store.GCPVerificationVerified}))
	assert.False(t, gcpServiceAccountVerified(&store.GCPServiceAccount{Verified: false, VerificationStatus: store.GCPVerificationUnverified}))
}

// assignAgentGCPSA creates a GCP service account row with the given state
// and points the agent's applied config at it in assign mode.
func assignAgentGCPSA(t *testing.T, s store.Store, agent *store.Agent, suffix string, verified bool, status string) *store.GCPServiceAccount {
	t.Helper()
	ctx := context.Background()
	sa := &store.GCPServiceAccount{
		ID: tid("sa-start-" + suffix), Scope: store.ScopeProject, ScopeID: agent.ProjectID,
		Email: "start-" + suffix + "@p.iam.gserviceaccount.com", ProjectID: "gcp-proj",
		Verified: verified, VerificationStatus: status, CreatedBy: "dev", CreatedAt: time.Now(),
	}
	if verified {
		sa.VerifiedAt = time.Now()
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	if got.AppliedConfig == nil {
		got.AppliedConfig = &store.AgentAppliedConfig{}
	}
	got.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID,
		ServiceAccountEmail: sa.Email, ProjectID: sa.ProjectID,
	}
	require.NoError(t, s.UpdateAgent(ctx, got))
	return sa
}

// Start and restart fail fast with 400 when the assigned service account
// would be refused at token mint; nothing is dispatched and the agent is
// left as it was.
func TestAgentLifecycle_StartRefusedForInadmissibleGCPSA(t *testing.T) {
	cases := []struct {
		name     string
		verified bool
		status   string
		mutate   func(t *testing.T, s store.Store, agent *store.Agent, sa *store.GCPServiceAccount)
		reason   string
	}{
		{name: "unverified", status: store.GCPVerificationUnverified, reason: "not verified"},
		{name: "failed", status: store.GCPVerificationFailed, reason: "not verified"},
		{name: "deleted", verified: true, status: store.GCPVerificationVerified, reason: "no longer available",
			mutate: func(t *testing.T, s store.Store, _ *store.Agent, sa *store.GCPServiceAccount) {
				require.NoError(t, s.DeleteGCPServiceAccount(context.Background(), sa.ID))
			}},
		{name: "email changed", verified: true, status: store.GCPVerificationVerified, reason: "no longer matches",
			mutate: func(t *testing.T, s store.Store, agent *store.Agent, _ *store.GCPServiceAccount) {
				got, err := s.GetAgent(context.Background(), agent.ID)
				require.NoError(t, err)
				got.AppliedConfig.GCPIdentity.ServiceAccountEmail = "other@p.iam.gserviceaccount.com"
				require.NoError(t, s.UpdateAgent(context.Background(), got))
			}},
	}
	for i, tc := range cases {
		for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				srv, s := testServer(t)
				disp := &deleteGuardDispatcher{}
				srv.SetDispatcher(disp)
				suffix := fmt.Sprintf("gcp-%s-%d", action, i)
				agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseStopped)
				sa := assignAgentGCPSA(t, s, agent, suffix, tc.verified, tc.status)
				if tc.mutate != nil {
					tc.mutate(t, s, agent, sa)
				}

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), tc.reason)
				assert.Contains(t, rec.Body.String(), "Cannot "+action+" agent")
				assert.Zero(t, disp.starts, "no start dispatch")
				assert.Zero(t, disp.stops, "restart must not stop the agent before refusing")

				got, err := s.GetAgent(context.Background(), agent.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.PhaseStopped), got.Phase)
			})
		}
	}
}

// Stop and suspend are never gated on the GCP identity: an agent with an
// inadmissible service account must still be stoppable.
func TestAgentLifecycle_StopSuspendNotGatedOnGCPSA(t *testing.T) {
	for _, action := range []string{api.AgentActionStop, api.AgentActionSuspend} {
		t.Run(action, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &deleteGuardDispatcher{}
			srv.SetDispatcher(disp)
			suffix := "gcp-" + action
			agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseRunning)
			assignAgentGCPSA(t, s, agent, suffix, false, store.GCPVerificationFailed)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, 1, disp.stops)
		})
	}
}

// An admissible (verified, reachable, matching) service account starts.
func TestAgentLifecycle_StartAllowedForAdmissibleGCPSA(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &deleteGuardDispatcher{}
			srv.SetDispatcher(disp)
			suffix := "gcp-ok-" + action
			agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseStopped)
			assignAgentGCPSA(t, s, agent, suffix, true, store.GCPVerificationVerified)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, 1, disp.starts)
		})
	}
}

// A hub-scoped assignment is inadmissible while gcpIamCheckMode is not
// enforce -- the same refusal the token-mint gate applies.
func TestAgentLifecycle_StartRefusedForHubScopedSAWithoutEnforce(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteGuardDispatcher{}
	srv.SetDispatcher(disp)
	agent := setupBrokerAgentInPhase(t, s, "gcp-hub-mode", state.PhaseStopped)
	sa := assignAgentGCPSA(t, s, agent, "gcp-hub-mode", true, store.GCPVerificationVerified)

	// Repoint the agent at a hub-scoped copy (Scope cannot be updated in place).
	hubSA := *sa
	hubSA.ID = tid("sa-start-gcp-hub-mode-hub")
	hubSA.Scope, hubSA.ScopeID = store.ScopeHub, srv.HubID()
	hubSA.Email = "hub-" + sa.Email
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), &hubSA))
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	got.AppliedConfig.GCPIdentity.ServiceAccountID = hubSA.ID
	got.AppliedConfig.GCPIdentity.ServiceAccountEmail = hubSA.Email
	require.NoError(t, s.UpdateAgent(context.Background(), got))

	srv.mu.RLock()
	mode := srv.saAssignCheckMode
	srv.mu.RUnlock()
	require.NotEqual(t, SAAssignCheckEnforce, mode, "test assumes the default mode is not enforce")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.True(t, strings.Contains(rec.Body.String(), "gcpIamCheckMode=enforce"), rec.Body.String())
	assert.Zero(t, disp.starts)
}

// A restart refused on a running agent leaves it running: phase and run
// intent are untouched, and nothing is stopped or started.
func TestAgentLifecycle_RunningRestartRefusedLeavesAgentRunning(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteGuardDispatcher{}
	srv.SetDispatcher(disp)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "gcp-running-restart", state.PhaseRunning)
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	assignAgentGCPSA(t, s, agent, "gcp-running-restart", false, store.GCPVerificationFailed)
	before, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, before.RunIntentAt)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Zero(t, disp.stops, "the running agent must not be stopped")
	assert.Zero(t, disp.starts)

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), after.Phase)
	assert.Equal(t, store.RunIntentRunning, after.RunIntent)
	require.NotNil(t, after.RunIntentAt)
	assert.True(t, before.RunIntentAt.Equal(*after.RunIntentAt), "run intent must not be rewritten")
}

// failingSAGetStore makes GetGCPServiceAccount fail with a store error that
// is not ErrNotFound while failGet is set. The error wraps a store sentinel
// (ErrVersionConflict) that writeErrorFromErr would map to 409, so the test
// below pins that this path answers a plain 500 whatever the store returns.
type failingSAGetStore struct {
	store.Store
	failGet atomic.Bool
}

func (f *failingSAGetStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	if f.failGet.Load() {
		return nil, fmt.Errorf("injected lookup failure: %w", store.ErrVersionConflict)
	}
	return f.Store.GetGCPServiceAccount(ctx, id)
}

func (f *failingSAGetStore) DB() *sql.DB {
	if p, ok := f.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

// A store failure while checking admissibility is a 500, not a 400: the
// assignment was not shown to be inadmissible, and the start does not proceed.
func TestAgentLifecycle_StartAdmissibilityStoreErrorIs500(t *testing.T) {
	base, err := newTestStore(":memory:")
	require.NoError(t, err)
	wrapped := &failingSAGetStore{Store: base}
	srv, s := testServerWithStore(t, wrapped)
	disp := &deleteGuardDispatcher{}
	srv.SetDispatcher(disp)
	agent := setupBrokerAgentInPhase(t, s, "gcp-store-err", state.PhaseStopped)
	assignAgentGCPSA(t, s, agent, "gcp-store-err", true, store.GCPVerificationVerified)
	wrapped.failGet.Store(true)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "injected lookup failure", "internal error detail is not echoed")
	assert.Zero(t, disp.starts)
}

// handleExistingAgent is the path the CLI uses to start or resume an
// existing agent (POST /agents with the agent's name). Each branch that
// starts or resumes applies the same refusal as the lifecycle route.
func TestHandleExistingAgent_GCPIdentityStartRefusal(t *testing.T) {
	branches := []struct {
		name   string
		phase  state.Phase
		resume bool
		verb   string
	}{
		{name: "suspended resume", phase: state.PhaseSuspended, verb: "resume"},
		{name: "stopped resume in place", phase: state.PhaseStopped, resume: true, verb: "resume"},
		{name: "created start", phase: state.PhaseCreated, verb: "start"},
	}
	for i, br := range branches {
		for _, admissible := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admissible=%v", br.name, admissible), func(t *testing.T) {
				f := handleExistingAgentAuthzSetup(t)
				disp := &deleteGuardDispatcher{}
				f.srv.SetDispatcher(disp)
				ctx := context.Background()
				name := fmt.Sprintf("hea-gcp-%d-%v", i, admissible)
				agent := f.agent(t, name, string(br.phase))
				before, err := f.store.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				status := store.GCPVerificationFailed
				if admissible {
					status = store.GCPVerificationVerified
				}
				assignAgentGCPSA(t, f.store, agent, name, admissible, status)
				// A ceiling makes the broker quota reservation observable.
				setBrokerAgentCeiling(t, f.store, 5)

				body := map[string]interface{}{"name": agent.Slug, "projectId": f.project.ID, "task": "new task text"}
				if br.resume {
					body["resume"] = true
				}
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", body)

				if admissible {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					assert.Equal(t, 1, disp.starts, "an admissible assignment proceeds to dispatch")
					if br.phase != state.PhaseCreated {
						// The resume branches reserve a broker slot; seeing it
						// here shows the refused-case assertion below is live.
						assert.True(t, hasReservation(t, f.store, store.LimitMaxAgentsPerBroker, agent.ID))
					}
					return
				}
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "Cannot "+br.verb+" agent")
				assert.Contains(t, rec.Body.String(), "not verified")
				assert.Zero(t, disp.starts, "nothing dispatched")
				assert.False(t, disp.deleteCalled)

				got, err := f.store.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				assert.Equal(t, string(br.phase), got.Phase)
				assert.Equal(t, before.RunIntent, got.RunIntent, "no run-intent write before the refusal")
				require.NotNil(t, got.AppliedConfig)
				assert.Equal(t, before.AppliedConfig.Task, got.AppliedConfig.Task, "the request's task is not applied")
				assert.False(t, hasReservation(t, f.store, store.LimitMaxAgentsPerBroker, agent.ID),
					"no broker quota reserved before the refusal")
			})
		}
	}
}
