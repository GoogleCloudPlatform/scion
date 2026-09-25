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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1901 uat finding F2a/F2c: a skill that exists but the caller
// cannot read must return a byte-identical 404 body to a skill ID that does
// not exist at all. Before this fix, a missing skill fell through to
// writeErrorFromErr's generic {"code":"not_found","message":"Resource not
// found"}, while the CheckAccess-denied branch a few lines below used
// NotFound(w, "Skill") -> {"code":"not_found","message":"Skill not found"}.
// Same HTTP status (404) and same error code, but different message text —
// an unauthorized caller could distinguish "doesn't exist" from "exists,
// denied" from the response body alone. writeSkillLookupError closes this
// for every read surface that looks a skill up by ID.
// ============================================================================

func TestSkillExistenceOracle_GetSkill_IdenticalNotFoundBody(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-oracle-get", store.SkillScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	missing := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+api.NewUUID(), nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	assert.Equal(t, missing.Body.String(), rec.Body.String(),
		"a forbidden-but-existing skill and a nonexistent one must return byte-identical 404 bodies")
}

func TestSkillExistenceOracle_VersionsList_IdenticalNotFoundBody(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-oracle-versions", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/versions", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	missing := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+api.NewUUID()+"/versions", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	assert.Equal(t, missing.Body.String(), rec.Body.String())
}

func TestSkillExistenceOracle_VersionByID_IdenticalNotFoundBody(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-oracle-version-by-id", store.SkillScopeUser, alice.ID, alice.ID)
	sv := createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/versions/"+sv.ID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	missing := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+api.NewUUID()+"/versions/"+sv.ID, nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	assert.Equal(t, missing.Body.String(), rec.Body.String())
}

func TestSkillExistenceOracle_ResolveSingle_IdenticalNotFoundBody(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-oracle-resolve-single", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/resolve?version=1.0.0", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	missing := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+api.NewUUID()+"/resolve?version=1.0.0", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	assert.Equal(t, missing.Body.String(), rec.Body.String())
}

func TestSkillExistenceOracle_Download_IdenticalNotFoundBody(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-oracle-download", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/download?version=1.0.0", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	missing := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+api.NewUUID()+"/download?version=1.0.0", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	assert.Equal(t, missing.Body.String(), rec.Body.String())
}

func TestSkillExistenceOracle_FileRead_IdenticalNotFoundBody(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-oracle-file-read", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/files/SKILL.md?version=1.0.0", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	missing := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+api.NewUUID()+"/files/SKILL.md?version=1.0.0", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	assert.Equal(t, missing.Body.String(), rec.Body.String())
}

// TestSkillExistenceOracle_BatchResolve_IdenticalCodeAndMessageShape covers
// uat F2b: resolveSkill must run authorization before any version-specific
// detail (F2's "found but version X could not be resolved" oracle), and a
// forbidden skill's batch-resolve error must have the same code and the same
// message shape as resolving a name that does not exist at all — not a
// distinguishable "forbidden".
func TestSkillExistenceOracle_BatchResolve_IdenticalCodeAndMessageShape(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-oracle-batch-resolve", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	// Forbidden: exists, wrong version too (9.9.9 was never published) — the
	// exact shape of the uat "found but version X could not be resolved"
	// oracle. Authorization must deny this before version resolution ever
	// runs, so the fact that 9.9.9 doesn't exist must never surface.
	forbiddenResp := decodeResolveResponse(t, srv, carol,
		"skill://scion/user/"+alice.ID+"/alice-oracle-batch-resolve@9.9.9")
	require.Len(t, forbiddenResp.Errors, 1)
	assert.Equal(t, "not_found", forbiddenResp.Errors[0].Code)
	assert.NotContains(t, forbiddenResp.Errors[0].Message, "found but version",
		"an unauthorized caller must not learn that the skill exists but the requested version does not")

	// Truly nonexistent name, same scope.
	missingResp := decodeResolveResponse(t, srv, carol,
		"skill://scion/user/"+alice.ID+"/alice-oracle-batch-resolve-does-not-exist@9.9.9")
	require.Len(t, missingResp.Errors, 1)
	assert.Equal(t, "not_found", missingResp.Errors[0].Code)

	assert.Equal(t, missingResp.Errors[0].Code, forbiddenResp.Errors[0].Code,
		"forbidden and nonexistent must report the same code")
}

func decodeResolveResponse(t *testing.T, srv *Server, user *store.User, uri string) ResolveSkillsResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: uri}},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}
