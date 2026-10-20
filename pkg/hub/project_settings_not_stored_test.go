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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectSettings_PutRejectsNotStoredFields checks that a project
// settings PUT setting bucket, runtimes, harnesses or profiles is refused
// with 400, names the field, does not echo the value, and changes nothing
// in the store, even when the body also carries a valid setting.
func TestProjectSettings_PutRejectsNotStoredFields(t *testing.T) {
	const secret = "s3cr3t-value-must-not-leak"
	bodies := map[string]string{
		"bucket":           `{"bucket":{"provider":"gcs","bucket":"` + secret + `"}}`,
		"runtimes":         `{"runtimes":{"docker":{"type":"` + secret + `"}}}`,
		"harnesses":        `{"harnesses":{"claude":{"image":"` + secret + `"}}}`,
		"profiles":         `{"profiles":{"dev":{"runtime":"` + secret + `"}}}`,
		"profiles-folded":  `{"Profiles":{"dev":{"runtime":"` + secret + `"}}}`,
		"empty-object":     `{"profiles":{}}`,
		"with-valid-field": `{"defaultTemplate":"changed","runtimes":{"x":"` + secret + `"}}`,
	}
	wantField := map[string]string{
		"bucket": "bucket", "runtimes": "runtimes", "harnesses": "harnesses",
		"profiles": "profiles", "profiles-folded": "profiles", "empty-object": "profiles",
		"with-valid-field": "runtimes",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			project := createTestProjectForSettings(t, s)
			path := "/api/v1/projects/" + project.ID + "/settings"

			rec := doRequest(t, srv, http.MethodPut, path, json.RawMessage(`{"defaultTemplate":"stored"}`))
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			before, err := s.GetProject(t.Context(), project.ID)
			require.NoError(t, err)

			rec = doRequest(t, srv, http.MethodPut, path, json.RawMessage(body))
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), `\"`+wantField[name]+`\"`)
			assert.Contains(t, rec.Body.String(), "not a project setting")
			assert.NotContains(t, rec.Body.String(), secret)

			after, err := s.GetProject(t.Context(), project.ID)
			require.NoError(t, err)
			assert.Equal(t, before.Annotations, after.Annotations, "a refused PUT changed the stored settings")
			assert.Equal(t, before.Updated, after.Updated, "a refused PUT wrote the project")
		})
	}
}

// TestProjectSettings_PutAllowsNullNotStoredFields checks that null for
// these fields is still accepted: null means "keep" on this endpoint, so it
// asks for no change.
func TestProjectSettings_PutAllowsNullNotStoredFields(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	path := "/api/v1/projects/" + project.ID + "/settings"

	rec := doRequest(t, srv, http.MethodPut, path,
		json.RawMessage(`{"defaultTemplate":"tpl","bucket":null,"runtimes":null,"harnesses":null,"profiles":null}`))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := s.GetProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, "tpl", got.Annotations[projectSettingDefaultTemplate])
}

// TestProjectSettingsNotStoredFieldError covers the body check directly.
func TestProjectSettingsNotStoredFieldError(t *testing.T) {
	for body, want := range map[string]string{
		`{}`:                                 "",
		`{"defaultTemplate":"x"}`:            "",
		`{"profiles":null}`:                  "",
		`not json`:                           "",
		`{"profiles":{"a":{}},"bucket":{}}`:  `"bucket", "profiles": not a project setting`,
		`{"Runtimes":{},"runtimes":{}}`:      `"runtimes": not a project setting`,
		"{\"harnesse\u017f\":{}}":            `"harnesses": not a project setting`,
		`{"runtimes":[],"harnesses":"text"}`: `"harnesses", "runtimes": not a project setting`,
	} {
		got := projectSettingsNotStoredFieldError([]byte(body))
		if want == "" {
			assert.Empty(t, got, body)
			continue
		}
		assert.Contains(t, got, want, body)
	}
}
