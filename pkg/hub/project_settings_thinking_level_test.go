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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The project settings PUT follows the hub server-config rule for the
// default thinking level (ptone/scion#4052): 1-100, 0 refused with a
// validation error, and an explicit null clears.

func putProjectSettingsRaw(t *testing.T, srv *Server, projectID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+projectID+"/settings", body)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestProjectSettings_ThinkingLevel_OutOfRangeRefused(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	// A stored value the refused PUTs must leave alone.
	code, body := putProjectSettingsRaw(t, srv, project.ID, map[string]any{"defaultThinkingLevel": 40})
	require.Equal(t, http.StatusOK, code, "body: %v", body)

	for _, v := range []int{0, -1, 101} {
		code, body := putProjectSettingsRaw(t, srv, project.ID, map[string]any{
			"defaultThinkingLevel": v,
			"defaultModel":         "model-x",
		})
		assert.Equal(t, http.StatusUnprocessableEntity, code, "level %d: %v", v, body)
		errObj, _ := body["error"].(map[string]any)
		require.NotNil(t, errObj, "level %d: structured error expected, got %v", v, body)
		assert.Equal(t, ErrCodeValidationError, errObj["code"], "level %d", v)
		assert.Contains(t, errObj["message"], "between 1 and 100", "level %d", v)
		details, _ := errObj["details"].(map[string]any)
		assert.Equal(t, projectSettingsFieldThinkingLevel, details["field"], "level %d", v)
	}

	stored, err := s.GetProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, "40", stored.Annotations[projectSettingDefaultThinkingLevel], "a refused PUT stores nothing")
	assert.NotContains(t, stored.Annotations, projectSettingDefaultModel, "a refused PUT stores nothing")
}

func TestProjectSettings_ThinkingLevel_BoundsAndNullClear(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	for _, v := range []int{1, 100} {
		code, body := putProjectSettingsRaw(t, srv, project.ID, map[string]any{"defaultThinkingLevel": v})
		require.Equal(t, http.StatusOK, code, "level %d: %v", v, body)
		assert.EqualValues(t, v, body["defaultThinkingLevel"])
	}

	code, body := putProjectSettingsRaw(t, srv, project.ID, map[string]any{"defaultThinkingLevel": nil})
	require.Equal(t, http.StatusOK, code, "body: %v", body)
	assert.NotContains(t, body, "defaultThinkingLevel", "null clears the level")

	stored, err := s.GetProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.NotContains(t, stored.Annotations, projectSettingDefaultThinkingLevel)
}

// A level of 0 stored before ptone/scion#4052 reads as unset: GET reports
// no level, a new agent does not get 0, and an unrelated save succeeds and
// removes the stale annotation.
func TestProjectSettings_ThinkingLevel_StoredZeroReadsAsUnset(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	project.Annotations = map[string]string{projectSettingDefaultThinkingLevel: "0"}
	require.NoError(t, s.UpdateProject(t.Context(), project))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got hubclient.ProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Nil(t, got.DefaultThinkingLevel, "a stored 0 is not reported")

	ac := &store.AgentAppliedConfig{}
	applyProjectDefaults(ac, project)
	assert.Nil(t, ac.ThinkingLevel, "a stored 0 is not applied to a new agent")

	code, body := putProjectSettingsRaw(t, srv, project.ID, map[string]any{"defaultModel": "model-y"})
	require.Equal(t, http.StatusOK, code, "an unrelated save is not blocked: %v", body)
	stored, err := s.GetProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, "model-y", stored.Annotations[projectSettingDefaultModel])
	assert.NotContains(t, stored.Annotations, projectSettingDefaultThinkingLevel, "the save removes the stale 0")
}

func TestProjectSettingsFromAnnotations_ThinkingLevelRange(t *testing.T) {
	for val, want := range map[string]*int{
		"0": nil, "-5": nil, "101": nil, "x": nil,
		"1": thinkingLevelPtr(1), "55": thinkingLevelPtr(55), "100": thinkingLevelPtr(100),
	} {
		p := &store.Project{Annotations: map[string]string{projectSettingDefaultThinkingLevel: val}}
		assert.Equal(t, want, projectSettingsFromAnnotations(p).DefaultThinkingLevel, "stored %q", val)
	}
}

func thinkingLevelPtr(n int) *int { return &n }
