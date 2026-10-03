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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-profile broker capacity (ptone/scion#2728).

// profileLimitSettings installs settings for srv's max_agents lookups and
// returns a setter to change them mid-test.
func profileLimitSettings(srv *Server, vs *config.VersionedSettings) func(*config.VersionedSettings) {
	var mu sync.Mutex
	cur := vs
	srv.agentLimitSettingsFn = func() *config.VersionedSettings {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	return func(next *config.VersionedSettings) {
		mu.Lock()
		defer mu.Unlock()
		cur = next
	}
}

// pqSettings is a settings fixture: profile gke (max 2) on runtime docker,
// profiles a and b on runtime k8s (max 2, shared), profile open with no
// limit, and profile zero with max_agents 0.
func pqSettings() *config.VersionedSettings {
	return &config.VersionedSettings{
		Runtimes: map[string]config.V1RuntimeConfig{
			"docker": {Type: "docker"},
			"k8s":    {Type: "kubernetes", MaxAgents: 2},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"gke":  {Runtime: "docker", MaxAgents: 2},
			"a":    {Runtime: "k8s"},
			"b":    {Runtime: "k8s"},
			"open": {Runtime: "docker"},
			"zero": {Runtime: "docker", MaxAgents: 0},
		},
	}
}

func profileScopeID(brokerID, kind, name string) string {
	return brokerID + "/" + kind + "/" + name
}

func profileReservationCount(t *testing.T, s store.Store, brokerID, scopeID string) int64 {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	n, err := s.CountActiveReservations(context.Background(), def.ID, brokerID, store.QuotaScopeBrokerProfile, scopeID)
	require.NoError(t, err)
	return n
}

func hasBrokerReservation(t *testing.T, s store.Store, agentID string) bool {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	has, err := s.HasActiveReservation(context.Background(), def.ID, agentID)
	require.NoError(t, err)
	return has
}

// newProfileQuotaAgent is newQuotaTestAgent with a recorded quota profile.
func newProfileQuotaAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase, cfg *store.AgentAppliedConfig) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(phase),
		AppliedConfig:   cfg,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

func reserveProfileSlot(t *testing.T, s store.Store, broker *store.RuntimeBroker, scopeID, agentID string) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	_, err = s.CreateUsageReservation(context.Background(), &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         broker.ID,
		ScopeType:         store.QuotaScopeBrokerProfile,
		ScopeID:           scopeID,
		ResourceID:        agentID,
		Reserved:          1,
	})
	require.NoError(t, err)
}

func quotaErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	assert.Equal(t, ErrCodeQuotaExceeded, resp.Error.Code)
	return resp.Error.Message
}

// Agents created with a capped profile reserve in the profile's own scope,
// count only against it, and leave the broker-wide total untouched; the
// cap refusal names the profile and its limit. Agents without an own limit
// still count toward max_agents_per_broker.
func TestBrokerProfileQuota_CreateCountsOnlyAgainstProfile(t *testing.T) {
	disp := &quotaLifecycleDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	profileLimitSettings(srv, pqSettings())
	brokerID := project.DefaultRuntimeBrokerID
	gkeScope := profileScopeID(brokerID, "profiles", "gke")

	var firstID string
	for i, name := range []string{"pq-gke-1", "pq-gke-2"} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: name, ProjectID: project.ID, Profile: "gke"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if i == 0 {
			var created CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
			firstID = created.Agent.ID
		}
	}
	assert.EqualValues(t, 2, profileReservationCount(t, s, brokerID, gkeScope))
	assert.EqualValues(t, 0, brokerReservationCount(t, s, brokerID), "profile-limited agents must not count toward the broker total")

	got, err := s.GetAgent(context.Background(), firstID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Equal(t, "gke", got.AppliedConfig.QuotaProfile, "the dispatched profile is recorded on the agent")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "pq-gke-3", ProjectID: project.ID, Profile: "gke"})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "quota exceeded: max_agents for profile gke on this broker (limit 2)", quotaErrorMessage(t, rec.Body.Bytes()))

	// An agent with no own limit fills the broker-wide slot...
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "pq-open-1", ProjectID: project.ID, Profile: "open"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerID))
	// ...and the next one gets the generic broker-wide refusal.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "pq-open-2", ProjectID: project.ID})
	assertBrokerQuotaExceeded(t, rec)
}

// max_agents: 0 means unset: the agent counts toward the broker total.
func TestBrokerProfileQuota_ZeroMeansBrokerScope(t *testing.T) {
	disp := &quotaLifecycleDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	profileLimitSettings(srv, pqSettings())
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "pq-zero-1", ProjectID: project.ID, Profile: "zero"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerID))
	assert.EqualValues(t, 0, profileReservationCount(t, s, brokerID, profileScopeID(brokerID, "profiles", "zero")))

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "pq-zero-2", ProjectID: project.ID, Profile: "zero"})
	assertBrokerQuotaExceeded(t, rec)
}

// A runtime entry's max_agents is one limit shared by every profile on it,
// and the refusal names the runtime entry.
func TestBrokerProfileQuota_RuntimeEntryLimitShared(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "rtshared")
	k8sScope := profileScopeID(broker.ID, "runtimes", "k8s")

	a1 := newProfileQuotaAgent(t, s, broker, project, "rt-a1", state.PhaseStopped, &store.AgentAppliedConfig{QuotaProfile: "a"})
	b1 := newProfileQuotaAgent(t, s, broker, project, "rt-b1", state.PhaseStopped, &store.AgentAppliedConfig{QuotaProfile: "b"})
	a2 := newProfileQuotaAgent(t, s, broker, project, "rt-a2", state.PhaseStopped, &store.AgentAppliedConfig{QuotaProfile: "a"})

	for _, a := range []*store.Agent{a1, b1} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assert.EqualValues(t, 2, profileReservationCount(t, s, broker.ID, k8sScope))
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a2.ID+"/start", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "quota exceeded: max_agents for runtime entry k8s on this broker (limit 2)", quotaErrorMessage(t, rec.Body.Bytes()))
}

// An agent created without a profile records the broker's default profile,
// and keeps counting against it after the broker's default changes.
func TestBrokerProfileQuota_RecordedDefaultProfileIsStable(t *testing.T) {
	disp := &quotaLifecycleDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	profileLimitSettings(srv, pqSettings())
	ctx := context.Background()
	brokerID := project.DefaultRuntimeBrokerID
	gkeScope := profileScopeID(brokerID, "profiles", "gke")

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	broker.DefaultProfile = "gke"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "pq-default", ProjectID: project.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	id := created.Agent.ID
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Equal(t, "gke", got.AppliedConfig.QuotaProfile)
	assert.EqualValues(t, 1, profileReservationCount(t, s, brokerID, gkeScope))

	broker, err = s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	broker.DefaultProfile = "open"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, hasBrokerReservation(t, s, id))
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, profileReservationCount(t, s, brokerID, gkeScope), "restart keys on the recorded profile, not the broker's new default")
	assert.EqualValues(t, 0, brokerReservationCount(t, s, brokerID))
}

// Agents created before the quota profile was recorded fall back to their
// applied profile.
func TestBrokerProfileQuota_LegacyAgentUsesAppliedProfile(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "legacyprof")

	a := newProfileQuotaAgent(t, s, broker, project, "legacy-prof", state.PhaseStopped, &store.AgentAppliedConfig{Profile: "gke"})
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, profileReservationCount(t, s, broker.ID, profileScopeID(broker.ID, "profiles", "gke")))
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))
}

// Release finds the reservation by agent ID whatever its scope: reserve
// under a profile limit, remove the limit, stop, and the reservation is
// released.
func TestBrokerProfileQuota_ReleaseAfterLimitRemoved(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	set := profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "relremoved")
	gkeScope := profileScopeID(broker.ID, "profiles", "gke")

	a := newProfileQuotaAgent(t, s, broker, project, "rel-removed", state.PhaseStopped, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.EqualValues(t, 1, profileReservationCount(t, s, broker.ID, gkeScope))

	set(&config.VersionedSettings{})

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, hasBrokerReservation(t, s, a.ID), "stop must release the reservation made under the removed limit")
	assert.EqualValues(t, 0, profileReservationCount(t, s, broker.ID, gkeScope))
}

// Suspend and delete release a profile-scope reservation too.
func TestBrokerProfileQuota_SuspendAndDeleteRelease(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "reldel")
	gkeScope := profileScopeID(broker.ID, "profiles", "gke")

	sus := newProfileQuotaAgent(t, s, broker, project, "rel-suspend", state.PhaseRunning, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	reserveProfileSlot(t, s, broker, gkeScope, sus.ID)
	del := newProfileQuotaAgent(t, s, broker, project, "rel-delete", state.PhaseRunning, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	reserveProfileSlot(t, s, broker, gkeScope, del.ID)
	require.EqualValues(t, 2, profileReservationCount(t, s, broker.ID, gkeScope))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+sus.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, hasBrokerReservation(t, s, sus.ID), "suspend must release")

	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+del.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, hasBrokerReservation(t, s, del.ID), "delete must release")
	assert.EqualValues(t, 0, profileReservationCount(t, s, broker.ID, gkeScope))
}

// Reconcile backfills an unreserved counted agent into its profile scope
// and releases a stale profile-scope reservation of a stopped agent.
func TestBrokerProfileQuota_ReconcileBackfillAndRelease(t *testing.T) {
	srv, s := testServer(t)
	profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "recbackfill")
	gkeScope := profileScopeID(broker.ID, "profiles", "gke")

	running := newProfileQuotaAgent(t, s, broker, project, "rec-running", state.PhaseRunning, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	stopped := newProfileQuotaAgent(t, s, broker, project, "rec-stopped", state.PhaseStopped, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	_, err = s.CreateUsageReservation(context.Background(), &store.UsageReservation{
		LimitDefinitionID: def.ID, SubjectID: broker.ID,
		ScopeType: store.QuotaScopeBrokerProfile, ScopeID: gkeScope,
		ResourceID: stopped.ID, Reserved: 1,
		CreatedAt: time.Now().Add(-2 * reconcileMinReservationAge),
	})
	require.NoError(t, err)

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	assert.True(t, hasBrokerReservation(t, s, running.ID))
	assert.False(t, hasBrokerReservation(t, s, stopped.ID))
	assert.EqualValues(t, 1, profileReservationCount(t, s, broker.ID, gkeScope))
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))
}

// Reconcile moves a running agent's reservation when its limit is added or
// removed, logging each move, and never moves one with a launch in flight.
func TestBrokerProfileQuota_ReconcileMovesScope(t *testing.T) {
	srv, s := testServer(t)
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&lockedWriter{w: &logBuf, mu: &logMu}, nil))
	set := profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "recmove")
	gkeScope := profileScopeID(broker.ID, "profiles", "gke")

	running := newProfileQuotaAgent(t, s, broker, project, "move-running", state.PhaseRunning, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	reserveBrokerSlot(t, s, broker, running.ID)
	starting := newProfileQuotaAgent(t, s, broker, project, "move-starting", state.PhaseStarting, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	reserveBrokerSlot(t, s, broker, starting.ID)

	// The limit was added after the reservations were made.
	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	assert.EqualValues(t, 1, profileReservationCount(t, s, broker.ID, gkeScope), "the running agent moves to the profile scope")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "the agent with a launch in flight is not moved")
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	assert.Contains(t, logged, "moved agent reservation to its current capacity scope")
	assert.Contains(t, logged, running.ID)
	assert.NotContains(t, logged, "agent_id="+starting.ID)

	// The limit is removed: the running agent moves back.
	set(&config.VersionedSettings{})
	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	assert.EqualValues(t, 0, profileReservationCount(t, s, broker.ID, gkeScope))
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID))
	assert.True(t, hasBrokerReservation(t, s, running.ID))
}

// Reconcile does not move an agent whose create launch is still active.
func TestBrokerProfileQuota_ReconcileSkipsActiveLaunch(t *testing.T) {
	srv, s := testServer(t)
	profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "reclaunch")
	gkeScope := profileScopeID(broker.ID, "profiles", "gke")

	a := newProfileQuotaAgent(t, s, broker, project, "launch-active", state.PhaseCreated, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	reserveBrokerSlot(t, s, broker, a.ID)
	_, err := s.BeginLaunch(context.Background(), a.ID, "create", time.Hour)
	require.NoError(t, err)
	got, err := s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "active", got.LaunchState)

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
	assert.EqualValues(t, 0, profileReservationCount(t, s, broker.ID, gkeScope))
}

func TestBrokerProfileQuota_LaunchInFlightPredicate(t *testing.T) {
	assert.True(t, brokerQuotaLaunchInFlight(&store.Agent{Phase: "running", LaunchState: "active"}))
	assert.True(t, brokerQuotaLaunchInFlight(&store.Agent{Phase: "starting"}))
	assert.False(t, brokerQuotaLaunchInFlight(&store.Agent{Phase: "running", LaunchState: "ended"}))
}

// The admin usage view lists profile-scope reservations with the limit
// they count against.
func TestBrokerProfileQuota_AdminUsageListsProfileRows(t *testing.T) {
	srv, s := testServer(t)
	profileLimitSettings(srv, pqSettings())
	broker, project := newQuotaTestBrokerAndProject(t, s, "usage")
	gkeScope := profileScopeID(broker.ID, "profiles", "gke")
	k8sScope := profileScopeID(broker.ID, "runtimes", "k8s")

	a := newProfileQuotaAgent(t, s, broker, project, "usage-gke", state.PhaseRunning, &store.AgentAppliedConfig{QuotaProfile: "gke"})
	reserveProfileSlot(t, s, broker, gkeScope, a.ID)
	b := newProfileQuotaAgent(t, s, broker, project, "usage-k8s", state.PhaseRunning, &store.AgentAppliedConfig{QuotaProfile: "a"})
	reserveProfileSlot(t, s, broker, k8sScope, b.ID)
	c := newProfileQuotaAgent(t, s, broker, project, "usage-broker", state.PhaseRunning, nil)
	reserveBrokerSlot(t, s, broker, c.ID)

	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.TotalActive)

	gke := reservationByScopeID(resp.Reservations, gkeScope)
	require.NotNil(t, gke)
	require.NotNil(t, gke.EntryAgentLimit)
	assert.EqualValues(t, 2, *gke.EntryAgentLimit)
	assert.Equal(t, "profiles.gke.max_agents", gke.EntryAgentLimitKey)
	assert.Nil(t, gke.BrokerAgentLimit)

	k8s := reservationByScopeID(resp.Reservations, k8sScope)
	require.NotNil(t, k8s)
	require.NotNil(t, k8s.EntryAgentLimit)
	assert.EqualValues(t, 2, *k8s.EntryAgentLimit)
	assert.Equal(t, "runtimes.k8s.max_agents", k8s.EntryAgentLimitKey)

	brokerRow := reservationByScopeID(resp.Reservations, broker.ID)
	require.NotNil(t, brokerRow)
	assert.Nil(t, brokerRow.EntryAgentLimit)
}

func TestBrokerQuotaScopeFor(t *testing.T) {
	vs := pqSettings()
	tests := []struct {
		profile, wantType, wantID string
		wantLimit                 int64
	}{
		{"gke", store.QuotaScopeBrokerProfile, "B/profiles/gke", 2},
		{"a", store.QuotaScopeBrokerProfile, "B/runtimes/k8s", 2},
		{"b", store.QuotaScopeBrokerProfile, "B/runtimes/k8s", 2},
		{"open", store.QuotaScopeBroker, "B", 0},
		{"zero", store.QuotaScopeBroker, "B", 0},
		{"", store.QuotaScopeBroker, "B", 0},
	}
	for _, tt := range tests {
		sc := brokerQuotaScopeFor(vs, "B", tt.profile)
		assert.Equal(t, tt.wantType, sc.ScopeType, tt.profile)
		assert.Equal(t, tt.wantID, sc.ScopeID, tt.profile)
		assert.Equal(t, tt.wantLimit, sc.Limit, tt.profile)
	}
	sc := brokerQuotaScopeFor(nil, "B", "gke")
	assert.Equal(t, store.QuotaScopeBroker, sc.ScopeType)
	assert.True(t, strings.HasPrefix(brokerQuotaExceededMessage(&brokerQuotaExceededError{scope: brokerQuotaScopeFor(vs, "B", "a")}), "quota exceeded: max_agents for runtime entry k8s"))
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
