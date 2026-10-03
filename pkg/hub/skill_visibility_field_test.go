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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func assertVisibilityRejected(t *testing.T, code int, body []byte) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, code, "got: %s", body)
	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), "body: %s", body)
	assert.Equal(t, ErrCodeValidationError, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "visibility")
	assert.Equal(t, "visibility", resp.Error.Details["field"])
}

// TestCreateSkill_RejectsVisibilityField verifies that a create request that
// still sends the removed visibility field fails with 400 and creates nothing,
// instead of the field being silently ignored.
func TestCreateSkill_RejectsVisibilityField(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)

	for _, key := range []string{"visibility", "Visibility"} {
		t.Run(key, func(t *testing.T) {
			name := "vis-create-" + map[string]string{"visibility": "lower", "Visibility": "upper"}[key]
			rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", map[string]interface{}{
				"name":    name,
				"scope":   "project",
				"scopeId": project.ID,
				key:       "private",
			})
			assertVisibilityRejected(t, rec.Code, rec.Body.Bytes())

			list, err := s.ListSkills(context.Background(), store.SkillFilter{Name: name}, store.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, list.Items, "a rejected create must not store a skill")
		})
	}
}

// TestCreateSkill_VisibilityOnlyCheckedAsTopLevelField verifies that the word
// appearing in a value (not as a field name) is still accepted.
func TestCreateSkill_VisibilityOnlyCheckedAsTopLevelField(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", map[string]interface{}{
		"name":        "vis-in-value",
		"description": "visibility",
		"scope":       "project",
		"scopeId":     project.ID,
		"tags":        []string{"visibility"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "got: %s", rec.Body.String())
}

// TestUpdateSkill_RejectsVisibilityField verifies that an update request that
// sends visibility fails with 400 and leaves the skill unchanged, while the
// same update without it is applied.
func TestUpdateSkill_RejectsVisibilityField(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	skill := createTestSkill(t, s, "vis-update", store.SkillScopeProject, project.ID, alice.ID)
	path := "/api/v1/skills/" + skill.ID

	rec := doRequestAsUser(t, srv, alice, http.MethodPatch, path, map[string]interface{}{
		"description": "changed",
		"visibility":  "public",
	})
	assertVisibilityRejected(t, rec.Code, rec.Body.Bytes())

	got, err := s.GetSkill(ctx, skill.ID)
	require.NoError(t, err)
	assert.Equal(t, skill.Description, got.Description, "a rejected update must not modify the skill")

	rec = doRequestAsUser(t, srv, alice, http.MethodPatch, path, map[string]interface{}{
		"description": "changed",
	})
	require.Equal(t, http.StatusOK, rec.Code, "got: %s", rec.Body.String())
	got, err = s.GetSkill(ctx, skill.ID)
	require.NoError(t, err)
	assert.Equal(t, "changed", got.Description)
}

// doRawJSONRequestAsUser sends body verbatim (no JSON marshalling) as user.
func doRawJSONRequestAsUser(t *testing.T, srv *Server, user *store.User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestSkillWriteBody_RejectsTrailingData verifies that a body with data after
// the first JSON value is rejected, so a visibility field cannot be dropped
// silently by appending trailing bytes, and that nothing is stored or
// changed.
func TestSkillWriteBody_RejectsTrailingData(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	createBody := func(name string, extra string) string {
		return `{"name":"` + name + `","scope":"project","scopeId":"` + project.ID + `"` + extra + `}`
	}

	cases := []struct {
		name string
		body string
	}{
		{"visibility then junk", createBody("trail-vis", `,"visibility":"public"`) + ` junk`},
		{"junk without visibility", createBody("trail-junk", "") + ` junk`},
		{"second object", createBody("trail-obj", "") + ` {"visibility":"public"}`},
	}
	for _, tc := range cases {
		t.Run("create "+tc.name, func(t *testing.T) {
			rec := doRawJSONRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "got: %s", rec.Body.String())
		})
	}
	list, err := s.ListSkills(ctx, store.SkillFilter{ScopeID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items, "rejected creates must not store a skill")

	// Trailing whitespace is not data and stays accepted.
	rec := doRawJSONRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", createBody("trail-ws", "")+" \n\t ")
	require.Equal(t, http.StatusCreated, rec.Code, "got: %s", rec.Body.String())

	skill := createTestSkill(t, s, "trail-update", store.SkillScopeProject, project.ID, alice.ID)
	rec = doRawJSONRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/skills/"+skill.ID,
		`{"description":"changed","visibility":"public"} junk`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "got: %s", rec.Body.String())
	got, err := s.GetSkill(ctx, skill.ID)
	require.NoError(t, err)
	assert.Equal(t, skill.Description, got.Description, "a rejected update must not modify the skill")
}

// TestSkillWriteBody_BodyTooLarge verifies the create body is size-limited.
func TestSkillWriteBody_BodyTooLarge(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)

	big := `{"name":"too-big","scope":"project","scopeId":"` + project.ID +
		`","description":"` + strings.Repeat("x", maxSettingsBodySize) + `"}`
	rec := doRawJSONRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", big)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "got: %s", rec.Body.String())
}
