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
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-account status view (ptone/scion#4018).

func statusPath(projectID, ref string) string {
	return fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/status", projectID, url.PathEscape(ref))
}

func getSAStatus(t *testing.T, srv *Server, projectID, ref string) GCPServiceAccountStatus {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, statusPath(projectID, ref), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var st GCPServiceAccountStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	return st
}

func createSAStatusAgent(t *testing.T, s store.Store, projectID, name, saID string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("sa-status-agent-" + name + t.Name()), Slug: name, Name: name, ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Created: time.Now(), Updated: time.Now(),
	}
	if saID != "" {
		a.AppliedConfig = &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: saID,
		}}
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

// authoritativeProfile is a reported Kubernetes profile whose report is
// complete and fresh, so an account it does not map shows as not mapped.
func authoritativeProfile(name string, gsas ...string) store.BrokerProfile {
	p := k8sProfile(name, true, gsas...)
	p.MappingsComplete = true
	at := time.Now().Add(-time.Minute)
	p.MappingsReportedAt = &at
	return p
}

func TestGCPSAStatus_AllSections(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	ctx := context.Background()
	addProviderBroker(t, s, projectID, "b",
		authoritativeProfile("k8s-a", mappedGSA),
		authoritativeProfile("k8s-b"),
		k8sProfile("k8s-c", false),
		store.BrokerProfile{Name: "local", Type: "docker"})
	sa := mappingTestSA(t, s, projectID, mappedGSA)
	sa.DisplayName = "Worker"
	sa.VerificationStatus = store.GCPVerificationVerified
	sa.VerifiedAt = time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.UpdateGCPServiceAccount(ctx, sa))

	project, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	project.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	setProfileDefaultSAIDsAnnotation(project.Annotations, map[string]string{"k8s-b": sa.ID, "other": "x"})
	require.NoError(t, s.UpdateProject(ctx, project))
	srv.config.AgentDefaults.DefaultGCPIdentityMode = store.GCPMetadataModeAssign
	srv.config.AgentDefaults.DefaultGCPIdentityServiceAccountID = sa.ID

	createSAStatusAgent(t, s, projectID, "uses-one", sa.ID)
	createSAStatusAgent(t, s, projectID, "uses-two", sa.ID)
	createSAStatusAgent(t, s, projectID, "uses-other", tid("some-other-sa"))
	createSAStatusAgent(t, s, projectID, "uses-none", "")

	st := getSAStatus(t, srv, projectID, sa.ID)

	assert.Equal(t, GCPServiceAccountStatusIdentity{ID: sa.ID, DisplayName: "Worker", Scope: store.ScopeProject, Email: mappedGSA}, st.Account)

	assert.True(t, st.Verification.Verified)
	assert.Equal(t, store.GCPVerificationVerified, st.Verification.Status)
	require.NotNil(t, st.Verification.VerifiedAt)
	assert.True(t, st.Verification.VerifiedAt.Equal(sa.VerifiedAt))

	require.Len(t, st.Mappings, 3, "only Kubernetes profiles are listed")
	states := map[string]string{}
	for _, m := range st.Mappings {
		states[m.Profile] = m.State
		assert.Equal(t, "b", m.BrokerName)
		assert.Empty(t, m.KubernetesServiceAccount, "a report entry without a KSA shows none")
		assert.Empty(t, m.Source)
		assert.False(t, m.Incomplete)
		assert.False(t, m.Ambiguous)
	}
	assert.Equal(t, map[string]string{
		"k8s-a": GCPSAMappingMapped, "k8s-b": GCPSAMappingNotMapped, "k8s-c": GCPSAMappingNotReported,
	}, states)

	assert.Equal(t, GCPServiceAccountBinding{State: GCPSABindingUnknown, Reason: "not checked"}, st.WorkloadIdentityBinding,
		"the binding is never reported as bound without evidence")

	assert.Equal(t, []GCPServiceAccountDefault{
		{Kind: GCPSADefaultProject},
		{Kind: GCPSADefaultProfile, Profile: "k8s-b"},
		{Kind: GCPSADefaultHub},
	}, st.DefaultFor)

	assert.Equal(t, 2, st.Agents.Count)
	assert.Equal(t, []string{"uses-one", "uses-two"}, st.Agents.Names)

	assert.Equal(t, GCPSANextStepNotMapped, st.NextStep.Code,
		"k8s-b defaults to this account but does not map it")
	assert.Equal(t, "k8s-b", st.NextStep.Profile)
	assert.Equal(t, "b", st.NextStep.BrokerName)
}

// Phase 4 broker reports (ptone/scion#3329) carry the Kubernetes
// ServiceAccount, namespace and source per mapping, plus per-profile
// completeness, ambiguity and the report time. The view shows them per
// profile.
func TestGCPSAStatus_MappingDetails(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	reportedAt := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)

	explicit := store.BrokerProfile{
		Name: "k8s-explicit", Type: "kubernetes", Available: true,
		MappingsReported: true, MappingsComplete: true, MappingsReportedAt: &reportedAt,
		ServiceAccountMappings: []store.BrokerProfileSAMapping{
			{GSA: mappedGSA, KSA: "worker-ksa", Namespace: "agents", Source: "mapped"},
		},
	}
	discovered := store.BrokerProfile{
		Name: "k8s-discovered", Type: "kubernetes", Available: true,
		MappingsReported: true, MappingsComplete: true, MappingsReportedAt: &reportedAt,
		ServiceAccountMappings: []store.BrokerProfileSAMapping{
			// Stored as reported; matched case-insensitively.
			{GSA: "MAPPED@p.iam.gserviceaccount.com", KSA: "found-ksa", Namespace: "team", Source: "discovered"},
		},
	}
	incomplete := store.BrokerProfile{
		Name: "k8s-incomplete", Type: "kubernetes", Available: true,
		MappingsReported: true, MappingsIncompleteReason: "list_failed", MappingsReportedAt: &reportedAt,
	}
	staleAt := time.Now().Add(-gcpSAReportFreshFor - time.Minute)
	stale := store.BrokerProfile{
		Name: "k8s-stale", Type: "kubernetes", Available: true,
		MappingsReported: true, MappingsComplete: true, MappingsReportedAt: &staleAt,
	}
	// A broker that predates completeness reporting: reported, never complete,
	// no reason.
	older := store.BrokerProfile{
		Name: "k8s-older", Type: "kubernetes", Available: true,
		MappingsReported: true, MappingsReportedAt: &reportedAt,
	}
	authoritative := authoritativeProfile("k8s-authoritative")
	ambiguous := store.BrokerProfile{
		Name: "k8s-ambiguous", Type: "kubernetes", Available: true,
		MappingsReported: true, MappingsComplete: true, MappingsReportedAt: &reportedAt,
		AmbiguousGSAs: []string{mappedGSA},
	}
	addProviderBroker(t, s, projectID, "b", explicit, discovered, incomplete, ambiguous, stale, older, authoritative,
		k8sProfile("k8s-silent", false))
	sa := mappingTestSA(t, s, projectID, mappedGSA)

	st := getSAStatus(t, srv, projectID, sa.ID)
	byProfile := map[string]GCPServiceAccountProfileMapping{}
	for _, m := range st.Mappings {
		byProfile[m.Profile] = m
	}
	require.Len(t, byProfile, 8)

	got := byProfile["k8s-explicit"]
	assert.Equal(t, GCPSAMappingMapped, got.State)
	assert.Equal(t, "worker-ksa", got.KubernetesServiceAccount)
	assert.Equal(t, "agents", got.Namespace)
	assert.Equal(t, "mapped", got.Source)
	require.NotNil(t, got.ReportedAt)
	assert.True(t, got.ReportedAt.Equal(reportedAt))
	assert.False(t, got.Incomplete)
	assert.False(t, got.Ambiguous)

	got = byProfile["k8s-discovered"]
	assert.Equal(t, GCPSAMappingMapped, got.State)
	assert.Equal(t, "found-ksa", got.KubernetesServiceAccount)
	assert.Equal(t, "team", got.Namespace)
	assert.Equal(t, "discovered", got.Source)

	// "not mapped" only on an authoritative (complete and fresh) report.
	got = byProfile["k8s-authoritative"]
	assert.Equal(t, GCPSAMappingNotMapped, got.State)
	assert.Empty(t, got.UnknownReason)

	got = byProfile["k8s-incomplete"]
	assert.Equal(t, GCPSAMappingUnknown, got.State, "an incomplete report cannot show absence")
	assert.Equal(t, GCPSAUnknownIncomplete, got.UnknownReason)
	assert.True(t, got.Incomplete)
	assert.Equal(t, "list_failed", got.IncompleteReason)
	assert.Empty(t, got.KubernetesServiceAccount)

	got = byProfile["k8s-stale"]
	assert.Equal(t, GCPSAMappingUnknown, got.State)
	assert.Equal(t, GCPSAUnknownStale, got.UnknownReason)
	require.NotNil(t, got.ReportedAt, "the age is shown with a stale report")

	got = byProfile["k8s-older"]
	assert.Equal(t, GCPSAMappingUnknown, got.State)
	assert.Equal(t, GCPSAUnknownReportUnsupported, got.UnknownReason)
	assert.False(t, got.Incomplete, "an older broker's report has no incomplete reason to show")

	got = byProfile["k8s-ambiguous"]
	assert.Equal(t, GCPSAMappingNotMapped, got.State, "the broker refuses an ambiguous account")
	assert.True(t, got.Ambiguous)

	got = byProfile["k8s-silent"]
	assert.Equal(t, GCPServiceAccountProfileMapping{
		BrokerID: got.BrokerID, BrokerName: "b", Profile: "k8s-silent", State: GCPSAMappingNotReported,
	}, got, "an unreported profile carries no details")

	// The JSON stays additive: an older-broker entry has none of the new keys.
	raw, err := json.Marshal(byProfile["k8s-silent"])
	require.NoError(t, err)
	for _, key := range []string{"source", "reportedAt", "incomplete", "incompleteReason", "ambiguous", "unknownReason", "kubernetesServiceAccount", "namespace"} {
		assert.NotContains(t, string(raw), `"`+key+`"`)
	}
}

func TestGCPSAStatus_EmptySectionsAreEmptyNotNull(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	sa := mappingTestSA(t, s, projectID, unmappedGSA)

	rec := doRequest(t, srv, http.MethodGet, statusPath(projectID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `"mappings":[]`)
	assert.Contains(t, body, `"defaultFor":[]`)
	assert.Contains(t, body, `"names":[]`)
}

func TestGCPSAStatus_ResolvesEmailAndName(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	sa := mappingTestSA(t, s, projectID, mappedGSA)
	sa.DisplayName = "Worker SA"
	require.NoError(t, s.UpdateGCPServiceAccount(context.Background(), sa))

	assert.Equal(t, sa.ID, getSAStatus(t, srv, projectID, "MAPPED@p.iam.gserviceaccount.com").Account.ID,
		"email matches ignoring case")
	assert.Equal(t, sa.ID, getSAStatus(t, srv, projectID, "Worker SA").Account.ID)
}

func TestGCPSAStatus_NotFound(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	ctx := context.Background()
	other := &store.Project{ID: tid("sa-status-other-project"), Name: "other", Slug: "sa-status-other"}
	require.NoError(t, s.CreateProject(ctx, other))
	foreign := mappingTestSA(t, s, other.ID, "foreign@p.iam.gserviceaccount.com")

	for name, ref := range map[string]string{
		"unknown id":    tid("no-such-sa"),
		"unknown email": "nobody@p.iam.gserviceaccount.com",
		"other project": foreign.ID,
	} {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodGet, statusPath(projectID, ref), nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		})
	}
}

func TestGCPSAStatus_AgentOnlyInOwnProject(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	ctx := context.Background()
	sa := mappingTestSA(t, s, projectID, mappedGSA)
	agent := createSAStatusAgent(t, s, projectID, "reader", sa.ID)

	other := &store.Project{ID: tid("sa-status-agent-other"), Name: "other", Slug: "sa-status-agent-other"}
	require.NoError(t, s.CreateProject(ctx, other))
	otherAgent := createSAStatusAgent(t, s, other.ID, "outsider", "")

	ownToken, err := srv.agentTokenService.GenerateAgentToken(agent.ID, projectID, nil, nil)
	require.NoError(t, err)
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, statusPath(projectID, sa.ID), nil, ownToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), mappedGSA, "project agents may see the account email")

	otherToken, err := srv.agentTokenService.GenerateAgentToken(otherAgent.ID, other.ID, nil, nil)
	require.NoError(t, err)
	rec = doRequestWithAgentToken(t, srv, http.MethodGet, statusPath(projectID, sa.ID), nil, otherToken)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestGCPSANextStep(t *testing.T) {
	m := func(profile, state string) GCPServiceAccountProfileMapping {
		return GCPServiceAccountProfileMapping{BrokerName: "b", Profile: profile, State: state}
	}
	cases := []struct {
		name        string
		verified    bool
		mappings    []GCPServiceAccountProfileMapping
		defaults    []GCPServiceAccountDefault
		wantCode    string
		wantProfile string
		wantMessage string
	}{
		{name: "not verified wins", verified: false,
			mappings: []GCPServiceAccountProfileMapping{m("a", GCPSAMappingNotMapped)}, wantCode: GCPSANextStepNotVerified},
		{name: "no kubernetes profiles", verified: true, wantCode: GCPSANextStepNone},
		{name: "none map it", verified: true,
			mappings: []GCPServiceAccountProfileMapping{m("a", GCPSAMappingNotReported), m("b", GCPSAMappingNotMapped), m("c", GCPSAMappingNotMapped)},
			wantCode: GCPSANextStepNotMapped, wantProfile: "b"},
		{name: "mapped on one profile", verified: true,
			mappings: []GCPServiceAccountProfileMapping{m("a", GCPSAMappingMapped), m("b", GCPSAMappingNotMapped)},
			wantCode: GCPSANextStepNone},
		{name: "profile default not mapped", verified: true,
			mappings: []GCPServiceAccountProfileMapping{m("a", GCPSAMappingMapped), m("b", GCPSAMappingNotMapped)},
			defaults: []GCPServiceAccountDefault{{Kind: GCPSADefaultProfile, Profile: "b"}},
			wantCode: GCPSANextStepNotMapped, wantProfile: "b"},
		{name: "only unreported", verified: true,
			mappings: []GCPServiceAccountProfileMapping{m("a", GCPSAMappingNotReported)}, wantCode: GCPSANextStepNone},
		{name: "ambiguous on the only profile", verified: true,
			mappings: []GCPServiceAccountProfileMapping{{BrokerName: "b", Profile: "a", State: GCPSAMappingNotMapped, Ambiguous: true}},
			wantCode: GCPSANextStepNotMapped, wantProfile: "a", wantMessage: "annotated"},
		{name: "unknown is not a known missing link", verified: true,
			mappings: []GCPServiceAccountProfileMapping{m("a", GCPSAMappingUnknown)},
			defaults: []GCPServiceAccountDefault{{Kind: GCPSADefaultProfile, Profile: "a"}},
			wantCode: GCPSANextStepNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gcpSANextStep(tc.verified, tc.mappings, tc.defaults)
			assert.Equal(t, tc.wantCode, got.Code)
			assert.Equal(t, tc.wantProfile, got.Profile)
			assert.NotEmpty(t, got.Message)
			assert.Contains(t, got.Message, tc.wantMessage)
		})
	}
}

func TestListGCPServiceAccounts_MappingSummary(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b",
		k8sProfile("k8s-a", true, mappedGSA), k8sProfile("k8s-b", true, mappedGSA), k8sProfile("k8s-c", false))
	mappingTestSA(t, s, projectID, mappedGSA)
	mappingTestSA(t, s, projectID, unmappedGSA)

	for _, path := range []string{
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", projectID),
	} {
		t.Run(path, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp ListGCPServiceAccountsResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Len(t, resp.Items, 2)
			got := map[string]GCPServiceAccountMappingSummary{}
			for _, it := range resp.Items {
				require.NotNil(t, it.Mapping)
				got[it.Email] = *it.Mapping
			}
			assert.Equal(t, GCPServiceAccountMappingSummary{MappedProfiles: 2, ReportedProfiles: 2, UnreportedProfiles: 1}, got[mappedGSA])
			assert.Equal(t, GCPServiceAccountMappingSummary{MappedProfiles: 0, ReportedProfiles: 2, UnreportedProfiles: 1}, got[unmappedGSA])
		})
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/gcp-service-accounts?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"mapping"`, "the hub-scope list carries no mapping summary")
}

// An agent token without project:read sees no agents in the view, the same
// gate listAgents applies (checkAgentReadScope).
func TestGCPSAStatus_AgentWithoutProjectReadSeesNoAgents(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	sa := mappingTestSA(t, s, projectID, mappedGSA)
	agent := createSAStatusAgent(t, s, projectID, "no-read", sa.ID)
	createSAStatusAgent(t, s, projectID, "sibling", sa.ID)

	tok, err := srv.agentTokenService.GenerateAgentToken(agent.ID, projectID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	// Section level: the gate itself, independent of the route's project
	// read check.
	claims, err := srv.agentTokenService.ValidateAgentToken(tok)
	require.NoError(t, err)
	ctx := contextWithIdentity(context.Background(), &agentIdentityWrapper{claims})
	got := srv.gcpServiceAccountAgents(ctx, projectID, sa.ID)
	assert.Equal(t, GCPServiceAccountAgents{Names: []string{}}, got)

	// Route level: the rest of the view is served, the agents section is
	// empty, and no sibling agent name leaks.
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, statusPath(projectID, sa.ID), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "sibling")
	var st GCPServiceAccountStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.Equal(t, 0, st.Agents.Count)
	assert.Equal(t, sa.ID, st.Account.ID)
}

// A user who may not list the project's agents gets the other sections but
// no agents.
func TestGCPSAStatus_UserWithoutAgentListSeesNoAgents(t *testing.T) {
	srv, s, owner, _, outsider, project := setupGCPAuthzTest(t)
	sa := mappingTestSA(t, s, project.ID, mappedGSA)
	createSAStatusAgent(t, s, project.ID, "hidden-agent", sa.ID)

	// Positive control: the project owner sees the agent.
	rec := doRequestAsUser(t, srv, owner, http.MethodGet, statusPath(project.ID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ownerView GCPServiceAccountStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ownerView))
	require.Equal(t, 1, ownerView.Agents.Count)

	rec = doRequestAsUser(t, srv, outsider, http.MethodGet, statusPath(project.ID, sa.ID), nil)
	assert.NotContains(t, rec.Body.String(), "hidden-agent")
	if rec.Code == http.StatusOK {
		var st GCPServiceAccountStatus
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		assert.Equal(t, 0, st.Agents.Count)
		assert.Equal(t, sa.ID, st.Account.ID, "the other sections still return")
	} else {
		assert.Equal(t, http.StatusForbidden, rec.Code, "the project read gate refuses: %s", rec.Body.String())
	}
}

func TestGCPSAStatus_HubScopedAccount(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b", k8sProfile("k8s", true, "hubwide@p.iam.gserviceaccount.com"))
	hubSA := &store.GCPServiceAccount{
		ID: tid("sa-status-hub"), Scope: store.ScopeHub, ScopeID: "hub",
		Email: "hubwide@p.iam.gserviceaccount.com", ProjectID: "p",
		Verified: true, VerificationStatus: store.GCPVerificationVerified, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), hubSA))

	st := getSAStatus(t, srv, projectID, hubSA.ID)
	assert.Equal(t, store.ScopeHub, st.Account.Scope)
	require.Len(t, st.Mappings, 1)
	assert.Equal(t, GCPSAMappingMapped, st.Mappings[0].State)
	assert.Equal(t, GCPSANextStepNone, st.NextStep.Code)

	// Same read gate as the plain nested GET: a caller with no user identity
	// (an agent) is refused hub-scoped accounts on both routes alike.
	agent := createSAStatusAgent(t, s, projectID, "hub-reader", "")
	tok, err := srv.agentTokenService.GenerateAgentToken(agent.ID, projectID, nil, nil)
	require.NoError(t, err)
	plain := doRequestWithAgentToken(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", projectID, hubSA.ID), nil, tok)
	status := doRequestWithAgentToken(t, srv, http.MethodGet, statusPath(projectID, hubSA.ID), nil, tok)
	assert.Equal(t, plain.Code, status.Code, "plain %s / status %s", plain.Body.String(), status.Body.String())
}

func TestGCPSAStatus_AmbiguousName(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	ctx := context.Background()
	a := mappingTestSA(t, s, projectID, "a@p.iam.gserviceaccount.com")
	b := mappingTestSA(t, s, projectID, "b@p.iam.gserviceaccount.com")
	for _, sa := range []*store.GCPServiceAccount{a, b} {
		sa.DisplayName = "Shared Name"
		require.NoError(t, s.UpdateGCPServiceAccount(ctx, sa))
	}

	rec := doRequest(t, srv, http.MethodGet, statusPath(projectID, "Shared Name"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeIdentityAmbiguous)
	assert.Contains(t, rec.Body.String(), a.ID)
	assert.Contains(t, rec.Body.String(), b.ID)
}

// On the embedded broker the live settings decide explicit mappings; they
// name only the GSA, so the KSA, namespace and source come from the stored
// report for the same GSA. A stored explicit mapping the settings no longer
// have does not count; a discovered one does (discovery is not in settings).
func TestGCPSAStatus_EmbeddedBrokerDetailsFromRecord(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	reportedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	b := addProviderBroker(t, s, projectID, "embedded", store.BrokerProfile{
		Name: "gke", Type: "kubernetes", MappingsReported: true, MappingsComplete: true, MappingsReportedAt: &reportedAt,
		ServiceAccountMappings: []store.BrokerProfileSAMapping{
			{GSA: mappedGSA, KSA: "worker-ksa", Namespace: "agents", Source: "mapped"},
			{GSA: unmappedGSA, KSA: "stale-ksa", Namespace: "agents", Source: "mapped"},
			{GSA: "found@p.iam.gserviceaccount.com", KSA: "found-ksa", Namespace: "agents", Source: "discovered"},
		},
	})
	srv.SetEmbeddedBrokerID(b.ID)
	prev := loadEmbeddedBrokerMappingSettings
	t.Cleanup(func() { loadEmbeddedBrokerMappingSettings = prev })
	loadEmbeddedBrokerMappingSettings = func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{
			Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "kubernetes"}},
			Runtimes: map[string]config.V1RuntimeConfig{
				"kubernetes": {KubernetesServiceAccountMappings: map[string]string{mappedGSA: "worker-ksa"}},
			},
		}, nil
	}

	st := getSAStatus(t, srv, projectID, mappingTestSA(t, s, projectID, mappedGSA).ID)
	require.Len(t, st.Mappings, 1)
	assert.Equal(t, GCPSAMappingMapped, st.Mappings[0].State)
	assert.Equal(t, "worker-ksa", st.Mappings[0].KubernetesServiceAccount)
	assert.Equal(t, "agents", st.Mappings[0].Namespace)
	assert.Equal(t, "mapped", st.Mappings[0].Source)
	require.NotNil(t, st.Mappings[0].ReportedAt, "the report time is the stored report's")

	st = getSAStatus(t, srv, projectID, mappingTestSA(t, s, projectID, unmappedGSA).ID)
	require.Len(t, st.Mappings, 1)
	assert.Equal(t, GCPSAMappingNotMapped, st.Mappings[0].State,
		"the live settings win over the record's explicit mappings; the record is complete and fresh")
	assert.Empty(t, st.Mappings[0].KubernetesServiceAccount)

	// Discovery is not in the settings: a discovered mapping comes from the
	// stored report.
	st = getSAStatus(t, srv, projectID, mappingTestSA(t, s, projectID, "found@p.iam.gserviceaccount.com").ID)
	require.Len(t, st.Mappings, 1)
	assert.Equal(t, GCPSAMappingMapped, st.Mappings[0].State)
	assert.Equal(t, "found-ksa", st.Mappings[0].KubernetesServiceAccount)
	assert.Equal(t, "discovered", st.Mappings[0].Source)
}
