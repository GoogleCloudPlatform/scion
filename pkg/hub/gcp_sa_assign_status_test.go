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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Assign picker status on the project-scope SA lists (ptone/scion#3329
// phase 4c).

// reportedK8sProfile is a Kubernetes profile with a recent, complete,
// current-version report mapping gsas in namespace "agents".
func reportedK8sProfile(name string, gsas ...string) store.BrokerProfile {
	p := k8sProfile(name, true)
	for _, g := range gsas {
		p.ServiceAccountMappings = append(p.ServiceAccountMappings, store.BrokerProfileSAMapping{GSA: g, KSA: "ksa", Namespace: "agents"})
	}
	at := time.Now().Add(-time.Minute)
	p.MappingsComplete = true
	p.MappingsReportedAt = &at
	p.MappingsReportVersion = api.BrokerSAReportVersion
	return p
}

func assignListPaths(projectID, query string) []string {
	return []string{
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts?includeHubScoped=true%s", projectID, query),
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s&includeHubScoped=true%s", projectID, query),
	}
}

func listAssignStatus(t *testing.T, srv *Server, path string) map[string]*GCPServiceAccountAssignStatus {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListGCPServiceAccountsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	out := map[string]*GCPServiceAccountAssignStatus{}
	for _, it := range resp.Items {
		out[it.Email] = it.AssignStatus
	}
	return out
}

func TestListGCPServiceAccounts_AssignStatus(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	stale := reportedK8sProfile("stale", mappedGSA)
	old := time.Now().Add(-profileSAReportFreshFor - time.Minute)
	stale.MappingsReportedAt = &old
	b := addProviderBroker(t, s, projectID, "b",
		reportedK8sProfile("gke", mappedGSA),
		stale,
		store.BrokerProfile{Name: "local", Type: "docker"},
		store.BrokerProfile{Name: "custom", Type: "gke-custom"},
	)
	mappingTestSA(t, s, projectID, mappedGSA)
	mappingTestSA(t, s, projectID, unmappedGSA)
	hubSA := &store.GCPServiceAccount{
		ID: tid("hub-sa-assign"), Scope: store.ScopeHub, ScopeID: "hub",
		Email: "hubwide@p.iam.gserviceaccount.com", ProjectID: "p", Verified: true,
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), hubSA))

	type want struct{ state, reason string }
	tests := []struct {
		name  string
		query string
		want  map[string]want
	}{
		{name: "mapped and not mapped, single provider resolved", query: "&profile=gke", want: map[string]want{
			mappedGSA:                           {SAMappingStateMapped, ""},
			unmappedGSA:                         {SAMappingStateNotMapped, ""},
			"hubwide@p.iam.gserviceaccount.com": {SAMappingStateNotMapped, ""},
		}},
		{name: "explicit broker by name", query: "&profile=gke&broker=b", want: map[string]want{
			mappedGSA: {SAMappingStateMapped, ""}, unmappedGSA: {SAMappingStateNotMapped, ""},
		}},
		{name: "explicit broker by id", query: "&profile=gke&broker=" + b.ID, want: map[string]want{
			mappedGSA: {SAMappingStateMapped, ""},
		}},
		{name: "stale report is unknown", query: "&profile=stale", want: map[string]want{
			mappedGSA:   {SAMappingStateUnknown, SAMappingReasonReportStale},
			unmappedGSA: {SAMappingStateUnknown, SAMappingReasonReportStale},
		}},
		{name: "docker needs no mapping", query: "&profile=local", want: map[string]want{
			unmappedGSA: {SAMappingStateNotRequired, ""},
		}},
		{name: "custom runtime key is unknown", query: "&profile=custom", want: map[string]want{
			unmappedGSA: {SAMappingStateUnknown, SAMappingReasonRuntimeUnrecognized},
		}},
		{name: "no profile", query: "&assignStatus=true", want: map[string]want{
			unmappedGSA: {SAMappingStateUnknown, SAMappingReasonNoProfile},
		}},
		{name: "profile not on broker", query: "&profile=nope", want: map[string]want{
			unmappedGSA: {SAMappingStateUnknown, SAMappingReasonProfileNotOnBroker},
		}},
		{name: "broker not a provider", query: "&profile=gke&broker=elsewhere", want: map[string]want{
			mappedGSA: {SAMappingStateUnknown, SAMappingReasonNoBroker},
		}},
	}
	for _, tt := range tests {
		for _, path := range assignListPaths(projectID, tt.query) {
			t.Run(tt.name+" "+path, func(t *testing.T) {
				got := listAssignStatus(t, srv, path)
				require.Len(t, got, 3)
				for email, w := range tt.want {
					st := got[email]
					require.NotNil(t, st, email)
					assert.Equal(t, w.state, st.State, email)
					assert.Equal(t, w.reason, st.Reason, email)
					assert.NotEmpty(t, st.Message)
					assert.NotContains(t, st.Message, "@", "the message names no account")
					assert.NotContains(t, strings.ToLower(st.Message), "ready")
					if w.reason != SAMappingReasonNoBroker {
						assert.Equal(t, b.ID, st.BrokerID)
						assert.Equal(t, "b", st.BrokerName)
					}
				}
			})
		}
	}

	// The mapped row carries the namespace from the report.
	got := listAssignStatus(t, srv, assignListPaths(projectID, "&profile=gke")[0])
	assert.Equal(t, "agents", got[mappedGSA].Namespace)
	assert.Equal(t, "gke", got[mappedGSA].Profile)
}

func TestListGCPServiceAccounts_AssignStatusNoBrokerKnown(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b1", reportedK8sProfile("gke", mappedGSA))
	addProviderBroker(t, s, projectID, "b2", reportedK8sProfile("gke", mappedGSA))
	mappingTestSA(t, s, projectID, mappedGSA)
	for _, path := range assignListPaths(projectID, "&profile=gke") {
		st := listAssignStatus(t, srv, path)[mappedGSA]
		require.NotNil(t, st)
		assert.Equal(t, SAMappingStateUnknown, st.State)
		assert.Equal(t, SAMappingReasonNoBroker, st.Reason)
		assert.Contains(t, st.Message, "No broker chosen")
	}

	// The project's default broker resolves it.
	project, err := s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	project.DefaultRuntimeBrokerID = tid("map-broker-b2" + t.Name())
	require.NoError(t, s.UpdateProject(context.Background(), project))
	for _, path := range assignListPaths(projectID, "&profile=gke") {
		st := listAssignStatus(t, srv, path)[mappedGSA]
		require.NotNil(t, st)
		assert.Equal(t, SAMappingStateMapped, st.State)
		assert.Equal(t, "b2", st.BrokerName)
	}
}

// Without the opt-in parameters the response is unchanged: no assignStatus
// key, and the body is identical to the one with the field removed.
func TestListGCPServiceAccounts_AssignStatusOptIn(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b", reportedK8sProfile("gke", mappedGSA))
	mappingTestSA(t, s, projectID, mappedGSA)
	for i, base := range assignListPaths(projectID, "") {
		plain := doRequest(t, srv, http.MethodGet, base, nil)
		require.Equal(t, http.StatusOK, plain.Code)
		assert.NotContains(t, plain.Body.String(), "assignStatus")

		with := doRequest(t, srv, http.MethodGet, assignListPaths(projectID, "&profile=gke")[i], nil)
		require.Equal(t, http.StatusOK, with.Code)
		require.Contains(t, with.Body.String(), `"assignStatus"`)
		var withResp map[string]any
		require.NoError(t, json.Unmarshal(with.Body.Bytes(), &withResp))
		for _, it := range withResp["items"].([]any) {
			delete(it.(map[string]any), "assignStatus")
		}
		stripped, err := json.Marshal(withResp)
		require.NoError(t, err)
		var plainResp map[string]any
		require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &plainResp))
		plainNorm, err := json.Marshal(plainResp)
		require.NoError(t, err)
		assert.JSONEq(t, string(plainNorm), string(stripped))
	}

	// The hub-scope list never carries it.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/gcp-service-accounts?scope=hub&profile=gke", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "assignStatus")
}
