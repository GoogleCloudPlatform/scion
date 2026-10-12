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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-profile default GCP service accounts on the settings PUT
// (ptone/scion#4630): warnings for an unreported profile or an unmapped
// account, and validation of added or changed entries only.

// profileDefaultsPutResponse is the part of a settings PUT response these
// tests read.
type profileDefaultsPutResponse struct {
	DefaultGCPIdentityServiceAccountIDByProfile map[string]string `json:"defaultGCPIdentityServiceAccountIDByProfile"`
	Warnings                                    []string          `json:"warnings"`
}

// putProfileDefaults PUTs byProfile as the project's per-profile default
// map and returns the recorder.
func putProfileDefaults(t *testing.T, srv *Server, projectID string, byProfile map[string]string) *http.Response {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+projectID+"/settings",
		map[string]any{"defaultGCPIdentityServiceAccountIDByProfile": byProfile})
	return rec.Result()
}

// putProfileDefaultsOK PUTs byProfile, requires a 200 and decodes the body.
// The raw body is returned too, for checks on the absence of a field.
func putProfileDefaultsOK(t *testing.T, srv *Server, projectID string, byProfile map[string]string) (profileDefaultsPutResponse, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+projectID+"/settings",
		map[string]any{"defaultGCPIdentityServiceAccountIDByProfile": byProfile})
	require.Equal(t, http.StatusOK, rec.Code, "warnings must never fail the save: %s", rec.Body.String())
	var resp profileDefaultsPutResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp, rec.Body.String()
}

// storeProfileDefaults writes the per-profile default annotation directly,
// bypassing validation, to stand for an entry saved earlier that has since
// gone stale.
func storeProfileDefaults(t *testing.T, s store.Store, projectID string, byProfile map[string]string) {
	t.Helper()
	project, err := s.GetProject(t.Context(), projectID)
	require.NoError(t, err)
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	b, err := json.Marshal(byProfile)
	require.NoError(t, err)
	project.Annotations[projectSettingDefaultGCPIdentitySAIDByProfile] = string(b)
	require.NoError(t, s.UpdateProject(t.Context(), project))
}

func TestProjectSettings_ProfileDefaultSA_WarnsOnUnreportedProfile(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-unreported")
	addProviderBroker(t, s, project.ID, "b", authoritativeProfile("k8s", sa.Email))

	resp, _ := putProfileDefaultsOK(t, srv, project.ID, map[string]string{"gke": sa.ID})
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], `no broker providing this project reports profile "gke"`)
	assert.Equal(t, map[string]string{"gke": sa.ID}, resp.DefaultGCPIdentityServiceAccountIDByProfile,
		"the settings fields must be returned unchanged next to the warnings")
	assert.Equal(t, map[string]string{"gke": sa.ID},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile,
		"the entry must be saved despite the warning")
}

func TestProjectSettings_ProfileDefaultSA_WarnsOnUnmappedAccount(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-unmapped")
	addProviderBroker(t, s, project.ID, "b", authoritativeProfile("k8s", mappedGSA))

	resp, _ := putProfileDefaultsOK(t, srv, project.ID, map[string]string{"k8s": sa.ID})
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], sa.Email)
	assert.Contains(t, resp.Warnings[0], `is not mapped to a Kubernetes ServiceAccount on profile "k8s"`)
	assert.Contains(t, resp.Warnings[0], "checked: b/k8s")
	assert.Equal(t, map[string]string{"k8s": sa.ID},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile)
}

// One provider maps the account on the profile: no warning, even though
// another provider's profile of the same name does not.
func TestProjectSettings_ProfileDefaultSA_MappedAccountNoWarning(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-mapped")
	addProviderBroker(t, s, project.ID, "b1", authoritativeProfile("k8s", sa.Email))
	addProviderBroker(t, s, project.ID, "b2", authoritativeProfile("k8s", mappedGSA))

	resp, body := putProfileDefaultsOK(t, srv, project.ID, map[string]string{"k8s": sa.ID})
	assert.Empty(t, resp.Warnings)
	assert.NotContains(t, body, `"warnings"`, "a response with nothing to warn about has no warnings field")
}

// unknown and not_required decisions, and a provider set whose profiles are
// not all known, never warn.
func TestProjectSettings_ProfileDefaultSA_UnknownStateNoWarning(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		brokers [][]store.BrokerProfile
	}{
		{"report missing", "k8s", [][]store.BrokerProfile{{k8sProfile("k8s", false)}}},
		{"report incomplete", "k8s", [][]store.BrokerProfile{{k8sProfile("k8s", true)}}},
		{"local runtime profile (not_required)", "local", [][]store.BrokerProfile{{{Name: "local", Type: "docker"}}}},
		{"custom runtime profile", "gke", [][]store.BrokerProfile{{{Name: "gke", Type: "gke-custom"}}}},
		{"a provider without a stored profile list", "gke", [][]store.BrokerProfile{
			{authoritativeProfile("k8s", mappedGSA)},
			nil,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			project := createTestProjectForSettings(t, s)
			sa := newSettingsTestSA(t, s, project.ID, "sa-unknown")
			for i, profiles := range tc.brokers {
				addProviderBroker(t, s, project.ID, string(rune('a'+i)), profiles...)
			}
			resp, body := putProfileDefaultsOK(t, srv, project.ID, map[string]string{tc.profile: sa.ID})
			assert.Empty(t, resp.Warnings)
			assert.NotContains(t, body, `"warnings"`)
		})
	}
}

// Only added or changed entries are warned about: an entry the request
// resends as stored is not warned about again.
func TestProjectSettings_ProfileDefaultSA_WarnsOnChangedEntriesOnly(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	kept := newSettingsTestSA(t, s, project.ID, "sa-kept")
	added := newSettingsTestSA(t, s, project.ID, "sa-added")
	addProviderBroker(t, s, project.ID, "b", authoritativeProfile("k8s", mappedGSA))
	storeProfileDefaults(t, s, project.ID, map[string]string{"gke": kept.ID})

	resp, _ := putProfileDefaultsOK(t, srv, project.ID, map[string]string{"gke": kept.ID, "k8s": added.ID})
	require.Len(t, resp.Warnings, 1, "the unchanged gke entry must not be warned about: %v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "defaultGCPIdentityServiceAccountIDByProfile[k8s]")

	// A PUT that does not carry the map warns about nothing.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		map[string]any{"defaultModel": "m"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"warnings"`)
}

// A stale stored entry for one profile no longer blocks setting, changing
// or removing a different profile.
func TestProjectSettings_ProfileDefaultSA_StaleOtherEntryDoesNotBlock(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	good := newSettingsTestSA(t, s, project.ID, "sa-good")
	other := newSettingsTestSA(t, s, project.ID, "sa-other")
	storeProfileDefaults(t, s, project.ID, map[string]string{"stale": "deleted-sa-id"})

	// Add a profile next to the stale entry.
	putProfileDefaultsOK(t, srv, project.ID, map[string]string{"stale": "deleted-sa-id", "k8s": good.ID})
	// Change it.
	putProfileDefaultsOK(t, srv, project.ID, map[string]string{"stale": "deleted-sa-id", "k8s": other.ID})
	// Remove it, keeping the stale entry.
	resp, _ := putProfileDefaultsOK(t, srv, project.ID, map[string]string{"stale": "deleted-sa-id"})
	assert.Equal(t, map[string]string{"stale": "deleted-sa-id"}, resp.DefaultGCPIdentityServiceAccountIDByProfile)

	// Changing the stale entry itself to another bad value is still refused.
	res := putProfileDefaults(t, srv, project.ID, map[string]string{"stale": "another-missing-sa"})
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	assert.Equal(t, map[string]string{"stale": "deleted-sa-id"},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile,
		"a rejected change must not be persisted")
}

// Removing an entry is always allowed, whatever state the removed or the
// remaining entries are in.
func TestProjectSettings_ProfileDefaultSA_RemovalAlwaysAllowed(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	unverified := &store.GCPServiceAccount{
		ID: tid("sa-unverified-removal"), Scope: store.ScopeProject, ScopeID: project.ID,
		Email: "unverified-removal@example.com", ProjectID: "gcp-proj",
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), unverified))
	storeProfileDefaults(t, s, project.ID, map[string]string{"a": unverified.ID, "b": "deleted-sa-id", "c": "gone-sa-id"})

	// Remove one stale entry, keeping two.
	resp, _ := putProfileDefaultsOK(t, srv, project.ID, map[string]string{"a": unverified.ID, "c": "gone-sa-id"})
	assert.Equal(t, map[string]string{"a": unverified.ID, "c": "gone-sa-id"}, resp.DefaultGCPIdentityServiceAccountIDByProfile)
	// Clear the rest.
	putProfileDefaultsOK(t, srv, project.ID, map[string]string{})
	assert.Nil(t, getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByProfile)
}

// A stored map over the entry bound (for example after the bound was
// lowered) can still shrink.
func TestProjectSettings_ProfileDefaultSA_OverBoundMapCanShrink(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-bound")
	stored := manyProfileEntries(maxProfileDefaultSAEntries+2, sa.ID)
	storeProfileDefaults(t, s, project.ID, stored)

	smaller := manyProfileEntries(maxProfileDefaultSAEntries+1, sa.ID)
	resp, _ := putProfileDefaultsOK(t, srv, project.ID, smaller)
	assert.Len(t, resp.DefaultGCPIdentityServiceAccountIDByProfile, maxProfileDefaultSAEntries+1)

	// Growing past the bound is still refused.
	res := putProfileDefaults(t, srv, project.ID, manyProfileEntries(maxProfileDefaultSAEntries+3, sa.ID))
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
}

func TestChangedProfileDefaultSAIDs(t *testing.T) {
	stored := map[string]string{"same": "x", "changed": "old", "removed": "y"}
	next := map[string]string{"same": "x", "changed": "new", "added": "z"}
	assert.Equal(t, map[string]string{"changed": "new", "added": "z"}, changedProfileDefaultSAIDs(stored, next))
	assert.Empty(t, changedProfileDefaultSAIDs(stored, nil))
	assert.Equal(t, next, changedProfileDefaultSAIDs(nil, next))
}

func TestProfileReportedBy(t *testing.T) {
	withK8s := &store.RuntimeBroker{Profiles: []store.BrokerProfile{{Name: "k8s"}}}
	flat := &store.RuntimeBroker{}

	reported, known := profileReportedBy([]*store.RuntimeBroker{withK8s}, true, "k8s")
	assert.True(t, reported)
	assert.True(t, known)

	reported, known = profileReportedBy([]*store.RuntimeBroker{withK8s}, true, "gke")
	assert.False(t, reported)
	assert.True(t, known, "every provider listed its profiles, so gke is known to be unreported")

	_, known = profileReportedBy([]*store.RuntimeBroker{withK8s, flat}, true, "gke")
	assert.False(t, known, "a provider without a profile list makes the answer unknown")

	_, known = profileReportedBy([]*store.RuntimeBroker{withK8s}, false, "gke")
	assert.False(t, known, "an incomplete provider list makes the answer unknown")

	reported, known = profileReportedBy([]*store.RuntimeBroker{flat, withK8s}, false, "k8s")
	assert.True(t, reported, "a provider that reports the profile is enough")
	assert.True(t, known)

	_, known = profileReportedBy(nil, true, "k8s")
	assert.True(t, known, "a project with no providers reports no profile")
}
