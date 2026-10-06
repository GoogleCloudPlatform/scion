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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Early warning for unmapped GCP service accounts (ptone/scion#3329 phase 2).

const (
	mappedGSA   = "mapped@p.iam.gserviceaccount.com"
	unmappedGSA = "unmapped@p.iam.gserviceaccount.com"
)

// addProviderBroker creates a broker with profiles and links it to projectID.
func addProviderBroker(t *testing.T, s store.Store, projectID, name string, profiles ...store.BrokerProfile) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:       tid("map-broker-" + name + t.Name()),
		Name:     name,
		Slug:     "map-broker-" + name + "-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
		Profiles: profiles,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status,
	}))
	return b
}

func k8sProfile(name string, reported bool, gsas ...string) store.BrokerProfile {
	p := store.BrokerProfile{Name: name, Type: "kubernetes", Available: true, MappingsReported: reported}
	for _, g := range gsas {
		p.ServiceAccountMappings = append(p.ServiceAccountMappings, store.BrokerProfileSAMapping{GSA: g})
	}
	return p
}

func mappingTestSA(t *testing.T, s store.Store, projectID, email string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID: tid("map-sa-" + email + t.Name()), Scope: store.ScopeProject, ScopeID: projectID,
		Email: email, ProjectID: "p", Verified: true, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func newMappingProject(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)
	return srv, s, projectID
}

func TestProjectSAMappingWarnings_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		profiles [][]store.BrokerProfile // one entry per provider broker
		email    string
		want     []string // substrings; nil means no warning
	}{
		{
			name:     "no providers",
			email:    unmappedGSA,
			profiles: nil,
		},
		{
			name:     "docker-only broker",
			email:    unmappedGSA,
			profiles: [][]store.BrokerProfile{{{Name: "local", Type: "docker", MappingsReported: true}}},
		},
		{
			name:     "broker with no profiles (flat row)",
			email:    unmappedGSA,
			profiles: [][]store.BrokerProfile{{}},
		},
		{
			name:     "kubernetes profile maps it",
			email:    mappedGSA,
			profiles: [][]store.BrokerProfile{{k8sProfile("k8s", true, mappedGSA)}},
		},
		{
			name:     "kubernetes profile maps it, case-insensitive",
			email:    "Mapped@p.iam.gserviceaccount.com",
			profiles: [][]store.BrokerProfile{{k8sProfile("k8s", true, mappedGSA)}},
		},
		{
			name:     "kubernetes profile does not map it",
			email:    unmappedGSA,
			profiles: [][]store.BrokerProfile{{k8sProfile("k8s", true, mappedGSA)}},
			want:     []string{unmappedGSA, "b0/k8s", "kubernetes_service_account_mappings"},
		},
		{
			name:     "reported with nothing mapped",
			email:    unmappedGSA,
			profiles: [][]store.BrokerProfile{{k8sProfile("k8s", true)}},
			want:     []string{unmappedGSA},
		},
		{
			name:     "unreported (older broker) is unknown",
			email:    unmappedGSA,
			profiles: [][]store.BrokerProfile{{k8sProfile("k8s", false)}},
		},
		{
			name:  "mapped on another provider broker",
			email: mappedGSA,
			profiles: [][]store.BrokerProfile{
				{k8sProfile("k8s", true)},
				{k8sProfile("gke", true, mappedGSA)},
			},
		},
		{
			name:  "unmapped with one unreported profile notes it",
			email: unmappedGSA,
			profiles: [][]store.BrokerProfile{
				{k8sProfile("k8s", true, mappedGSA)},
				{k8sProfile("old", false)},
			},
			want: []string{unmappedGSA, "1 Kubernetes profile(s) did not report"},
		},
		{
			name:     "custom-named runtime with reported mappings counts as kubernetes",
			email:    unmappedGSA,
			profiles: [][]store.BrokerProfile{{{Name: "gke", Type: "gke-prod", MappingsReported: true, ServiceAccountMappings: []store.BrokerProfileSAMapping{{GSA: mappedGSA}}}}},
			want:     []string{unmappedGSA},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := newMappingProject(t)
			for i, profiles := range tc.profiles {
				addProviderBroker(t, s, projectID, fmt.Sprintf("b%d", i), profiles...)
			}
			sa := mappingTestSA(t, s, projectID, tc.email)
			got := srv.projectSAMappingWarnings(context.Background(), projectID, sa)
			if tc.want == nil {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			for _, sub := range tc.want {
				assert.Contains(t, got[0], sub)
			}
		})
	}
}

func TestProjectSAMappingWarnings_HubScopedAccountNeverWarned(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b", k8sProfile("k8s", true))
	hubSA := &store.GCPServiceAccount{
		ID: tid("hub-sa"), Scope: store.ScopeHub, ScopeID: "hub", Email: unmappedGSA, ProjectID: "p", Verified: true,
	}
	assert.Empty(t, srv.projectSAMappingWarnings(context.Background(), projectID, hubSA))
}

// The embedded broker runs in the hub process, so its mappings are read live
// from the same settings it resolves them from at dispatch, not from its
// registration record.
func TestProjectSAMappingWarnings_EmbeddedBrokerReadsLiveSettings(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	// The embedded broker's record does not report mappings; live settings
	// map mappedGSA on the profile's runtime entry ("kubernetes", the
	// profile's Type).
	b := addProviderBroker(t, s, projectID, "embedded", k8sProfile("remote", false))
	srv.SetEmbeddedBrokerID(b.ID)
	prev := loadEmbeddedBrokerMappingSettings
	t.Cleanup(func() { loadEmbeddedBrokerMappingSettings = prev })
	loadEmbeddedBrokerMappingSettings = func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{
			Runtimes: map[string]config.V1RuntimeConfig{
				"kubernetes": {KubernetesServiceAccountMappings: map[string]string{mappedGSA: "ksa"}},
			},
		}, nil
	}

	mapped := mappingTestSA(t, s, projectID, mappedGSA)
	unmapped := mappingTestSA(t, s, projectID, unmappedGSA)
	got := srv.projectSAMappingWarnings(context.Background(), projectID, mapped, unmapped)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], unmappedGSA)

	// Unreadable live settings: fall back to the record, which does not
	// report mappings, so unknown and no warning.
	loadEmbeddedBrokerMappingSettings = func() (*config.VersionedSettings, error) {
		return nil, errors.New("boom")
	}
	assert.Empty(t, srv.projectSAMappingWarnings(context.Background(), projectID, unmapped))
}

// --- HTTP surfaces ---

func TestCreateGCPServiceAccount_WarnsWhenUnmapped(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b", k8sProfile("k8s", true, mappedGSA))

	for _, path := range []string{
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", projectID),
	} {
		t.Run(path, func(t *testing.T) {
			email := fmt.Sprintf("u%d@p.iam.gserviceaccount.com", len(path))
			rec := doRequest(t, srv, http.MethodPost, path, map[string]string{"email": email})
			require.Equal(t, http.StatusCreated, rec.Code, "warnings must never fail the request: %s", rec.Body.String())
			var resp struct {
				ID       string   `json:"id"`
				Warnings []string `json:"warnings"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.NotEmpty(t, resp.ID)
			require.Len(t, resp.Warnings, 1)
			assert.Contains(t, resp.Warnings[0], email)
		})
	}

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		map[string]string{"email": mappedGSA})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"warnings"`, "a mapped account gets no warnings field")
}

func TestCreateGCPServiceAccount_NoWarningWithoutKubernetesProviders(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b", store.BrokerProfile{Name: "local", Type: "docker", MappingsReported: true})

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		map[string]string{"email": unmappedGSA})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"warnings"`)
}

func TestHubScopedCreate_NoWarnings(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b", k8sProfile("k8s", true))
	ensureHubMembership(context.Background(), s, DevUserID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/gcp-service-accounts?scope=hub",
		map[string]string{"email": unmappedGSA})
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"warnings"`)
}

func TestListGCPServiceAccounts_Warnings(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	addProviderBroker(t, s, projectID, "b", k8sProfile("k8s", true, mappedGSA))
	mappingTestSA(t, s, projectID, mappedGSA)
	mappingTestSA(t, s, projectID, unmappedGSA)
	hubSA := &store.GCPServiceAccount{
		ID: tid("hub-sa-list"), Scope: store.ScopeHub, ScopeID: "hub",
		Email: "hubwide@p.iam.gserviceaccount.com", ProjectID: "p", Verified: true,
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), hubSA))

	for _, path := range []string{
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts?includeHubScoped=true", projectID),
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s&includeHubScoped=true", projectID),
	} {
		t.Run(path, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp ListGCPServiceAccountsResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Len(t, resp.Items, 3)
			require.Len(t, resp.Warnings, 1, "only the unmapped project-scoped account is warned about")
			assert.Contains(t, resp.Warnings[0], unmappedGSA)
		})
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/gcp-service-accounts?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"warnings"`, "the hub-scope list never carries warnings")
}

func TestMintGCPServiceAccount_WarnsWhenUnmapped(t *testing.T) {
	srv, s, _ := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)
	addProviderBroker(t, s, projectID, "b", k8sProfile("k8s", true, mappedGSA))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID), map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp struct {
		store.GCPServiceAccount
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Managed, "the account fields are unchanged by the wrapper")
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], resp.Email)
}

func TestVerifyGCPServiceAccount_Warnings(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	addProviderBroker(t, s, projectID, "b", k8sProfile("k8s", true, mappedGSA))
	sa := mappingTestSA(t, s, projectID, unmappedGSA)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"warnings"`)
	assert.Contains(t, rec.Body.String(), unmappedGSA)

	// A hub-scoped account verified through the project route gets none.
	hubSA := &store.GCPServiceAccount{
		ID: tid("hub-sa-verify"), Scope: store.ScopeHub, ScopeID: "hub",
		Email: "hubverify@p.iam.gserviceaccount.com", ProjectID: "p",
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), hubSA))
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, hubSA.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"warnings"`)
}
