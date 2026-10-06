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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-profile default GCP service account (ptone/scion#3329 phase 1): a
// project can set a default registered service account per broker profile.
// When an agent is created without an explicit GCP identity, the entry for
// the profile the agent runs under wins over the project-wide default.

// profileDefaultFixture is a bypassAgents fixture whose broker reports the
// stock two-profile set (local=docker, remote=kubernetes) with the given
// default profile, plus two verified project-scoped SAs: broad (the
// project-wide default) and k8s (the per-profile default for "remote").
type profileDefaultFixture struct {
	*bypassAgentsFixture
	broad *store.GCPServiceAccount
	k8s   *store.GCPServiceAccount
}

func newProfileDefaultFixture(t *testing.T, brokerDefaultProfile string) *profileDefaultFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	markBrokerStockProfiles(t, f, brokerDefaultProfile)
	pf := &profileDefaultFixture{
		bypassAgentsFixture: f,
		broad:               bypassAgentsCreateSA(t, f, f.proj.ID, true),
		k8s:                 bypassAgentsCreateSA(t, f, f.proj.ID, true),
	}
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().
		AllowTarget(pf.broad.Email).AllowTarget(pf.k8s.Email))
	return pf
}

// setAnnotations writes project annotations straight to the store.
func (pf *profileDefaultFixture) setAnnotations(t *testing.T, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	proj, err := pf.store.GetProject(ctx, pf.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	for k, v := range kv {
		proj.Annotations[k] = v
	}
	require.NoError(t, pf.store.UpdateProject(ctx, proj))
}

func (pf *profileDefaultFixture) setProjectDefaultAssignBroad(t *testing.T) {
	t.Helper()
	pf.setAnnotations(t, map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID: pf.broad.ID,
	})
}

func (pf *profileDefaultFixture) setProfileDefaults(t *testing.T, byProfile map[string]string) {
	t.Helper()
	b, err := json.Marshal(byProfile)
	require.NoError(t, err)
	pf.setAnnotations(t, map[string]string{projectSettingDefaultGCPIdentitySAIDByProfile: string(b)})
}

func assertAssigned(t *testing.T, agent *store.Agent, sa *store.GCPServiceAccount, msg string) {
	t.Helper()
	require.NotNil(t, agent.AppliedConfig)
	require.NotNil(t, agent.AppliedConfig.GCPIdentity, msg)
	assert.Equal(t, store.GCPMetadataModeAssign, agent.AppliedConfig.GCPIdentity.MetadataMode, msg)
	assert.Equal(t, sa.ID, agent.AppliedConfig.GCPIdentity.ServiceAccountID, msg)
	assert.Equal(t, sa.Email, agent.AppliedConfig.GCPIdentity.ServiceAccountEmail, msg)
}

// The request names no profile and the project has no active profile, so the
// agent runs under the broker's own default profile ("remote"). Its entry
// wins over the project-wide default, and the profile is pinned so the broker
// dispatches under exactly the profile the SA was chosen for.
func TestProfileDefaultSA_BrokerDefaultProfileApplies(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})

	logs := captureDefaultSlog(t)
	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-broker-default"})

	assertAssigned(t, agent, pf.k8s, "the broker default profile's entry must win over the project default")
	assert.Equal(t, "remote", agent.AppliedConfig.Profile, "the resolved profile must be pinned")
	require.NotNil(t, agent.AppliedConfig.CreateInputs)
	assert.Equal(t, "remote", agent.AppliedConfig.CreateInputs.Profile,
		"the pin must reach CreateInputs so reincarnate replays the same profile")

	recs := logs.recordsContaining("GCP identity chosen by default")
	require.Len(t, recs, 1)
	assert.Equal(t, slog.LevelDebug, recs[0].Level)
	assert.Equal(t, "project-profile-default", logSource(recs[0]))
}

// An explicit request profile selects its entry over the broker default.
func TestProfileDefaultSA_RequestProfileSelectsEntry(t *testing.T) {
	pf := newProfileDefaultFixture(t, "local")
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})

	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-request", Profile: "remote"})
	assertAssigned(t, agent, pf.k8s, "the request profile's entry must apply")
	assert.Equal(t, "remote", agent.AppliedConfig.Profile)
}

// The project's active profile is the profile the agent runs under when the
// request names none, ahead of the broker default.
func TestProfileDefaultSA_ProjectActiveProfileSelectsEntry(t *testing.T) {
	pf := newProfileDefaultFixture(t, "local")
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
	pf.setAnnotations(t, map[string]string{projectSettingActiveProfile: "remote"})

	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-active"})
	assertAssigned(t, agent, pf.k8s, "the project active profile's entry must apply")
	assert.Equal(t, "remote", agent.AppliedConfig.Profile)
}

// No entry for the profile the agent runs under: the project-wide default
// applies unchanged, and nothing is pinned by the per-profile rung.
func TestProfileDefaultSA_NoEntryFallsBackToProjectDefault(t *testing.T) {
	pf := newProfileDefaultFixture(t, "local")
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})

	logs := captureDefaultSlog(t)
	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-miss"})
	assertAssigned(t, agent, pf.broad, "with no entry for local, the project default must apply")

	recs := logs.recordsContaining("GCP identity chosen by default")
	require.Len(t, recs, 1)
	assert.Equal(t, "project-default", logSource(recs[0]))
}

// No per-profile setting at all: today's behaviour (project default).
func TestProfileDefaultSA_UnsetKeepsProjectDefault(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setProjectDefaultAssignBroad(t)

	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-unset"})
	assertAssigned(t, agent, pf.broad, "without a per-profile setting the project default must apply")
}

// The per-profile default is more specific than a project-wide "block" and
// wins over it.
func TestProfileDefaultSA_WinsOverProjectBlock(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setAnnotations(t, map[string]string{projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeBlock})
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})

	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-over-block"})
	assertAssigned(t, agent, pf.k8s, "the per-profile default must win over an explicit project block")
}

// An explicit request identity still wins over every default.
func TestProfileDefaultSA_ExplicitRequestIdentityWins(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})

	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{
		Name: "profile-default-explicit",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: pf.broad.ID,
		},
	})
	assertAssigned(t, agent, pf.broad, "an explicit request identity must win over the per-profile default")
}

// A stale entry (the SA was un-verified after it was saved) fails the create,
// naming the per-profile tier, exactly like a stale project default does.
func TestProfileDefaultSA_StaleUnverifiedEntryFailsCreate(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	unverified := bypassAgentsCreateSA(t, pf.bypassAgentsFixture, pf.proj.ID, false)
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": unverified.ID})

	rec := createAgentAsOwner(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-stale"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `project default for profile \"remote\" GCP service account is not verified`)
}

// The authorization gate applies to the per-profile default as it does to
// the project default.
func TestProfileDefaultSA_CreatorWithoutActAsDenied(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
	enforceSAAssign(pf.srv, store.NewFakeCallerPermissionChecker().DenyTarget(pf.k8s.Email, "no actAs grant"))

	rec := createAgentAsOwner(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-default-denied"})
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

// The scheduled-dispatch twin applies the same precedence and pin.
func TestScheduledDispatch_ProfileDefaultSAWinsOverProjectDefault(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})

	require.NoError(t, fireScheduledDispatchAsOwner(t, pf.bypassAgentsFixture, "sched-profile-default"))
	got, err := pf.store.GetAgentBySlug(context.Background(), pf.proj.ID, "sched-profile-default")
	require.NoError(t, err)
	assertAssigned(t, got, pf.k8s, "the scheduled path must apply the per-profile default")
	assert.Equal(t, "remote", got.AppliedConfig.Profile)
	require.NotNil(t, got.AppliedConfig.CreateInputs)
	assert.Equal(t, "remote", got.AppliedConfig.CreateInputs.Profile,
		"the scheduled pin must reach CreateInputs so reincarnate replays the same profile")
}

func TestScheduledDispatch_ProfileDefaultNoEntryKeepsProjectDefault(t *testing.T) {
	pf := newProfileDefaultFixture(t, "local")
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})

	require.NoError(t, fireScheduledDispatchAsOwner(t, pf.bypassAgentsFixture, "sched-profile-miss"))
	got, err := pf.store.GetAgentBySlug(context.Background(), pf.proj.ID, "sched-profile-miss")
	require.NoError(t, err)
	assertAssigned(t, got, pf.broad, "with no entry for local, the scheduled path must apply the project default")
}

// --- settings API ---

func getProjectSettings(t *testing.T, srv *Server, projectID string) hubclient.ProjectSettings {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	return got
}

func TestProjectSettings_ProfileDefaultSA_RoundTripPreserveAndClear(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-profile")
	url := "/api/v1/projects/" + project.ID + "/settings"

	rec := doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByProfile: map[string]string{"k8s": sa.ID},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, map[string]string{"k8s": sa.ID},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile)

	// A full-body PUT that does not carry the field (the web settings page)
	// must keep the stored map.
	rec = doRequest(t, srv, http.MethodPut, url, map[string]interface{}{"defaultModel": "m"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, map[string]string{"k8s": sa.ID},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile,
		"an absent field must keep the stored map")

	// An empty object clears it.
	rec = doRequest(t, srv, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByProfile": map[string]string{},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Nil(t, getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile)
	stored, err := s.GetProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.NotContains(t, stored.Annotations, projectSettingDefaultGCPIdentitySAIDByProfile)
}

func TestProjectSettings_ProfileDefaultSA_Rejections(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	good := newSettingsTestSA(t, s, project.ID, "sa-good")
	unverified := &store.GCPServiceAccount{
		ID: tid("sa-unverified-profile"), Scope: store.ScopeProject, ScopeID: project.ID,
		Email: "unverified-profile@proj.iam.gserviceaccount.com", ProjectID: "gcp-proj",
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), unverified))
	other := &store.Project{ID: tid("other-profile-project"), Name: "Other", Slug: "other-profile-project"}
	require.NoError(t, s.CreateProject(t.Context(), other))
	otherSA := newSettingsTestSA(t, s, other.ID, "sa-other")

	cases := []struct {
		name      string
		byProfile map[string]string
		wantMsg   string
	}{
		{"unknown SA", map[string]string{"k8s": "no-such-sa"}, msgSANotAvailableInProject},
		{"other project's SA", map[string]string{"k8s": otherSA.ID}, msgSANotAvailableInProject},
		{"unverified SA", map[string]string{"k8s": unverified.ID}, `as the default for profile \"k8s\"`},
		{"empty SA ID", map[string]string{"k8s": ""}, "a service account ID is required"},
		{"empty profile", map[string]string{"": good.ID}, "is invalid"},
		{"padded profile", map[string]string{" k8s": good.ID}, "is invalid"},
		{"dotted profile", map[string]string{"k8s.prod": good.ID}, "is invalid"},
		{"leading dash", map[string]string{"-k8s": good.ID}, "is invalid"},
		{"profile name too long", map[string]string{strings.Repeat("a", 64): good.ID}, "is invalid"},
		{"too many entries", manyProfileEntries(65, good.ID), "at most 64 entries"},
		{"one bad entry among good", map[string]string{"a": good.ID, "b": "no-such-sa"}, msgSANotAvailableInProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
				hubclient.ProjectSettings{DefaultGCPIdentityServiceAccountIDByProfile: tc.byProfile})
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantMsg)
			assert.Nil(t, getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile,
				"a rejected map must not be persisted")
		})
	}
}

func TestProfileDefaultSAIDsFromAnnotations_MalformedIgnored(t *testing.T) {
	assert.Nil(t, profileDefaultSAIDsFromAnnotations(map[string]string{
		projectSettingDefaultGCPIdentitySAIDByProfile: "not json",
	}))
	assert.Nil(t, profileDefaultSAIDsFromAnnotations(map[string]string{
		projectSettingDefaultGCPIdentitySAIDByProfile: "{}",
	}))
	assert.Equal(t, map[string]string{"a": "x"}, profileDefaultSAIDsFromAnnotations(map[string]string{
		projectSettingDefaultGCPIdentitySAIDByProfile: `{"a":"x"}`,
	}))
}

// --- clone ---

func TestProjectClone_ProfileDefaultSARemapped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srcID := api.NewUUID()
	projSA := &store.GCPServiceAccount{
		ID: api.NewUUID(), Scope: store.ScopeProject, ScopeID: srcID,
		Email: "k8s@proj.iam.gserviceaccount.com", ProjectID: "gcp-proj", Verified: true, CreatedBy: DevUserID,
	}
	const hubSAID = "hub-scoped-sa-id"
	byProfile, err := json.Marshal(map[string]string{"remote": projSA.ID, "other": hubSAID})
	require.NoError(t, err)
	src := &store.Project{
		ID: srcID, Name: "Profile SA Source", Slug: "profile-sa-source", OwnerID: DevUserID, CreatedBy: DevUserID,
		Annotations: map[string]string{projectSettingDefaultGCPIdentitySAIDByProfile: string(byProfile)},
	}
	require.NoError(t, s.CreateProject(ctx, src))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, projSA))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Profile SA Clone"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	cloneSAs, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Scope: store.ScopeProject, ScopeID: clone.ID})
	require.NoError(t, err)
	require.Len(t, cloneSAs, 1)

	persisted, err := s.GetProject(ctx, clone.ID)
	require.NoError(t, err)
	got := profileDefaultSAIDsFromAnnotations(persisted.Annotations)
	assert.Equal(t, cloneSAs[0].ID, got["remote"], "a project-scoped entry must point at the cloned SA")
	assert.Equal(t, hubSAID, got["other"], "an entry naming another account must be kept")
}

func logSource(r slog.Record) string {
	v, _ := recordAttr(r, "source")
	return v.String()
}

func manyProfileEntries(n int, saID string) map[string]string {
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("p%d", i)] = saID
	}
	return m
}

// The bounds accept their maximums.
func TestProjectSettings_ProfileDefaultSA_BoundsAcceptMaximums(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-bounds")
	m := manyProfileEntries(62, sa.ID)
	m[strings.Repeat("a", 63)] = sa.ID // a 63-character name
	m["Gke_prod-1"] = sa.ID            // mixed case, '_' and '-'; 64 entries in total
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityServiceAccountIDByProfile: m})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Len(t, getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile, 64)
}

// F2: the scheduled per-profile rung runs the same authorization gate.
func TestScheduledDispatch_ProfileDefaultSA_ActAsDenied(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
	enforceSAAssign(pf.srv, store.NewFakeCallerPermissionChecker().
		AllowTarget(pf.broad.Email).DenyTarget(pf.k8s.Email, "no actAs grant"))

	err := fireScheduledDispatchAsOwner(t, pf.bypassAgentsFixture, "sched-profile-denied")
	require.Error(t, err, "a creator without actAs on the per-profile SA must not get a scheduled agent")
	assert.Contains(t, err.Error(), store.PermissionActAs)
	_, getErr := pf.store.GetAgentBySlug(context.Background(), pf.proj.ID, "sched-profile-denied")
	assert.ErrorIs(t, getErr, store.ErrNotFound, "denied dispatch must not create the agent record")
}

func TestScheduledDispatch_ProfileDefaultSA_StaleUnverifiedFails(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	unverified := bypassAgentsCreateSA(t, pf.bypassAgentsFixture, pf.proj.ID, false)
	pf.setProjectDefaultAssignBroad(t)
	pf.setProfileDefaults(t, map[string]string{"remote": unverified.ID})

	err := fireScheduledDispatchAsOwner(t, pf.bypassAgentsFixture, "sched-profile-stale")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `project default for profile "remote" GCP service account is not verified`)
	_, getErr := pf.store.GetAgentBySlug(context.Background(), pf.proj.ID, "sched-profile-stale")
	assert.ErrorIs(t, getErr, store.ErrNotFound)
}

// F8: the per-profile default wins over a hub-default assign when the
// project has no default of its own, on both paths.
func TestProfileDefaultSA_WinsOverHubDefaultAssign(t *testing.T) {
	pf := newProfileDefaultFixture(t, "remote")
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
	setHubAgentDefaults(pf.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: pf.broad.ID,
	})

	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-over-hub"})
	assertAssigned(t, agent, pf.k8s, "the per-profile default must win over the hub default")

	require.NoError(t, fireScheduledDispatchAsOwner(t, pf.bypassAgentsFixture, "sched-profile-over-hub"))
	got, err := pf.store.GetAgentBySlug(context.Background(), pf.proj.ID, "sched-profile-over-hub")
	require.NoError(t, err)
	assertAssigned(t, got, pf.k8s, "the scheduled per-profile default must win over the hub default")
}

// With no entry for the agent's profile, the hub default still applies.
func TestProfileDefaultSA_NoEntryKeepsHubDefaultAssign(t *testing.T) {
	pf := newProfileDefaultFixture(t, "local")
	pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
	setHubAgentDefaults(pf.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: pf.broad.ID,
	})
	agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "profile-miss-hub"})
	assertAssigned(t, agent, pf.broad, "with no entry for local, the hub default must apply")
}

// F3: projectProfileDefaultSA's broker-resolution branches, directly.
func TestProjectProfileDefaultSA_BrokerResolution(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()
	byProfile, err := json.Marshal(map[string]string{"remote": "sa-remote", "local": "sa-local"})
	require.NoError(t, err)
	project := &store.Project{ID: f.proj.ID, Annotations: map[string]string{
		projectSettingDefaultGCPIdentitySAIDByProfile: string(byProfile),
	}}

	t.Run("unknown broker with a request profile uses that profile", func(t *testing.T) {
		profile, saID := f.srv.projectProfileDefaultSA(ctx, "no-such-broker", project, "remote")
		assert.Equal(t, "remote", profile)
		assert.Equal(t, "sa-remote", saID)
	})
	t.Run("unknown broker and no request or active profile is empty", func(t *testing.T) {
		profile, saID := f.srv.projectProfileDefaultSA(ctx, "no-such-broker", project, "")
		assert.Empty(t, profile)
		assert.Empty(t, saID)
	})
	t.Run("no broker ID with an active profile uses it", func(t *testing.T) {
		withActive := &store.Project{ID: f.proj.ID, Annotations: map[string]string{
			projectSettingDefaultGCPIdentitySAIDByProfile: string(byProfile),
			projectSettingActiveProfile:                   "local",
		}}
		profile, saID := f.srv.projectProfileDefaultSA(ctx, "", withActive, "")
		assert.Equal(t, "local", profile)
		assert.Equal(t, "sa-local", saID)
	})
	t.Run("broker with two profiles and no default is empty", func(t *testing.T) {
		markBrokerStockProfiles(t, f, "")
		profile, saID := f.srv.projectProfileDefaultSA(ctx, f.broker.ID, project, "")
		assert.Empty(t, profile)
		assert.Empty(t, saID)
	})
	t.Run("broker with a single unnamed-default profile resolves it", func(t *testing.T) {
		b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
		require.NoError(t, err)
		b.Profiles = []store.BrokerProfile{{Name: "remote", Type: "kubernetes", Available: true}}
		b.DefaultProfile = ""
		require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
		profile, saID := f.srv.projectProfileDefaultSA(ctx, f.broker.ID, project, "")
		assert.Equal(t, "remote", profile)
		assert.Equal(t, "sa-remote", saID)
	})
	t.Run("no map is empty", func(t *testing.T) {
		profile, saID := f.srv.projectProfileDefaultSA(ctx, f.broker.ID, &store.Project{ID: f.proj.ID}, "remote")
		assert.Empty(t, profile)
		assert.Empty(t, saID)
	})
}
