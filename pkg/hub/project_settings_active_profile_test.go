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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The project settings PUT is a full replace for most fields, but
// activeProfile is kept when absent and cleared only by an explicit empty
// string (ptone/scion#3383). Before this, the web settings page, which never
// sends activeProfile, cleared the project's active profile on every save.
// The bodies below are raw JSON maps so the tests control exactly whether the
// key is present.

// putActiveProfileTestSettings sends body as the project settings PUT and
// returns the stored scion.io/active-profile annotation and whether it is set.
func putActiveProfileTestSettings(t *testing.T, srv *Server, s store.Store, projectID string, body map[string]any) (string, bool) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+projectID+"/settings", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	stored, err := s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	v, ok := stored.Annotations[projectSettingActiveProfile]
	return v, ok
}

// getActiveProfileTestSettings returns the activeProfile the settings GET
// reports.
func getActiveProfileTestSettings(t *testing.T, srv *Server, projectID string) *string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var settings hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&settings))
	return settings.ActiveProfile
}

func TestProjectSettings_ActiveProfile_PutSetsValue(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	v, ok := putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{"activeProfile": "k8s-prod"})
	require.True(t, ok, "a PUT with activeProfile must store the annotation")
	assert.Equal(t, "k8s-prod", v)

	got := getActiveProfileTestSettings(t, srv, project.ID)
	require.NotNil(t, got)
	assert.Equal(t, "k8s-prod", *got)

	// A second value replaces the first.
	v, ok = putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{"activeProfile": "docker-local"})
	require.True(t, ok)
	assert.Equal(t, "docker-local", v)
}

func TestProjectSettings_ActiveProfile_AbsentKeepsValue(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	_, ok := putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{"activeProfile": "k8s-prod"})
	require.True(t, ok)

	// Empty body: activeProfile absent.
	v, ok := putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{})
	require.True(t, ok, "a PUT without activeProfile must keep the stored annotation")
	assert.Equal(t, "k8s-prod", v)

	// JSON null is treated as absent, matching the per-profile default
	// service account map.
	v, ok = putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{"activeProfile": nil})
	require.True(t, ok, "activeProfile: null must keep the stored annotation")
	assert.Equal(t, "k8s-prod", v)

	got := getActiveProfileTestSettings(t, srv, project.ID)
	require.NotNil(t, got)
	assert.Equal(t, "k8s-prod", *got)
}

func TestProjectSettings_ActiveProfile_ExplicitEmptyClears(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	_, ok := putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{"activeProfile": "k8s-prod"})
	require.True(t, ok)

	_, ok = putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{"activeProfile": ""})
	assert.False(t, ok, "an explicit empty activeProfile must remove the annotation")

	assert.Nil(t, getActiveProfileTestSettings(t, srv, project.ID),
		"a cleared active profile is omitted from the GET response")
}

// TestProjectSettings_ActiveProfile_WebShapedSaveKeepsValue reproduces
// ptone/scion#3383: the web project settings page sends a full settings body
// built from its form fields, with no activeProfile key. Changing an
// unrelated field there must not clear the active profile.
func TestProjectSettings_ActiveProfile_WebShapedSaveKeepsValue(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	_, ok := putActiveProfileTestSettings(t, srv, s, project.ID, map[string]any{
		"activeProfile":   "k8s-prod",
		"defaultTemplate": "old-template",
	})
	require.True(t, ok)

	// Mirrors handleSaveConfig in web/src/components/pages/project-settings.ts:
	// JSON.stringify drops undefined members, so unset form fields and
	// activeProfile (which the page never sets) are absent.
	webBody := map[string]any{
		"defaultTemplate":        "new-template",
		"telemetryEnabled":       true,
		"autoExposePortsEnabled": false,
		"defaultMaxTurns":        25,
		"defaultThinkingLevel":   nil,
	}
	v, ok := putActiveProfileTestSettings(t, srv, s, project.ID, webBody)
	require.True(t, ok, "a web settings save must keep the active profile")
	assert.Equal(t, "k8s-prod", v)

	stored, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, "new-template", stored.Annotations[projectSettingDefaultTemplate],
		"the unrelated field the page changed is still applied")

	// And the kept profile still reaches new agents.
	ac := &store.AgentAppliedConfig{}
	applyProjectDefaults(ac, stored)
	assert.Equal(t, "k8s-prod", ac.Profile)
}

// TestApplyProjectSettingsToAnnotations_ActiveProfile covers the three
// cases at the function level, independent of the HTTP decoder.
func TestApplyProjectSettingsToAnnotations_ActiveProfile(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	tests := []struct {
		name      string
		in        *string
		wantValue string
		wantSet   bool
	}{
		{name: "nil keeps the stored value", in: nil, wantValue: "k8s-prod", wantSet: true},
		{name: "empty clears", in: strPtr(""), wantSet: false},
		{name: "value sets", in: strPtr("docker-local"), wantValue: "docker-local", wantSet: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project := &store.Project{Annotations: map[string]string{projectSettingActiveProfile: "k8s-prod"}}
			applyProjectSettingsToAnnotations(project, &hubclient.ProjectSettings{ActiveProfile: tc.in})
			v, ok := project.Annotations[projectSettingActiveProfile]
			assert.Equal(t, tc.wantSet, ok)
			assert.Equal(t, tc.wantValue, v)
		})
	}

	t.Run("nil on a project with no annotation leaves it unset", func(t *testing.T) {
		project := &store.Project{}
		applyProjectSettingsToAnnotations(project, &hubclient.ProjectSettings{})
		assert.NotContains(t, project.Annotations, projectSettingActiveProfile)
	})
}
