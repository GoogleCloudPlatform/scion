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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// User access tokens on the user-scope injected-skills routes
// (/api/v1/users/me/injected-skills). The writes need the
// user_skill_injection:update scope on a hub boundary; the list does not.

const userInjectedSkillsPath = "/api/v1/users/me/injected-skills"

// callWithKey sends a request with a bearer credential (a token key)
// through the full server handler.
func callWithKey(t *testing.T, srv *Server, key, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// callUserInjectedSkillsAs runs the user-scope injected-skills routes for
// identity, set directly on the request context.
func callUserInjectedSkillsAs(t *testing.T, srv *Server, identity Identity, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	if entryID, ok := strings.CutPrefix(path, userInjectedSkillsPath+"/"); ok {
		srv.handleUserMeInjectedSkillByID(rec, req, entryID)
	} else {
		srv.handleUserMeInjectedSkills(rec, req)
	}
	return rec
}

// seedUserInjectedSkill adds one user-scope entry for userID.
func seedUserInjectedSkill(t *testing.T, s store.Store, userID, uri string) *store.SkillInjection {
	t.Helper()
	si := &store.SkillInjection{Scope: store.SkillInjectionScopeUser, ScopeID: userID, SkillURI: uri, SortOrder: 1, CreatedBy: userID}
	require.NoError(t, s.AddSkillInjection(context.Background(), si))
	return si
}

// userInjectedSkillURIs returns the skill URIs stored for userID, sorted.
func userInjectedSkillURIs(t *testing.T, s store.Store, userID string) []string {
	t.Helper()
	sis, err := s.ListSkillInjections(context.Background(), store.SkillInjectionScopeUser, userID)
	require.NoError(t, err)
	uris := make([]string, 0, len(sis))
	for _, si := range sis {
		uris = append(uris, si.SkillURI)
	}
	sort.Strings(uris)
	return uris
}

// userInjectedSkillWrites lists the three write requests, against the
// seeded entry seededID.
func userInjectedSkillWrites(seededID string) []struct {
	method, path string
	body         interface{}
} {
	return []struct {
		method, path string
		body         interface{}
	}{
		{http.MethodPost, userInjectedSkillsPath, api.SkillInjectionEntry{SkillURI: "skill://scion/added@1.0"}},
		{http.MethodPut, userInjectedSkillsPath, api.SkillInjectionList{Entries: []api.SkillInjectionEntry{{SkillURI: "skill://scion/replaced@1.0"}}}},
		{http.MethodDelete, userInjectedSkillsPath + "/" + seededID, nil},
	}
}

// TestUserInjectedSkillsWrite_TokenNeedsUpdateScope pins that a user access
// token without user_skill_injection:update gets 403 on each write route
// and changes nothing, that the same 403 is returned for an entry ID that
// does not exist, that the list stays readable with that token, and that a
// hub token with the scope can add, replace and remove entries.
func TestUserInjectedSkillsWrite_TokenNeedsUpdateScope(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	seeded := seedUserInjectedSkill(t, s, alice.ID, "skill://scion/seeded@1.0")
	before := userInjectedSkillURIs(t, s, alice.ID)

	without := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "inbox:read", "inbox:write")
	var deleteBody string
	for _, w := range userInjectedSkillWrites(seeded.ID) {
		rec := callWithKey(t, srv, without, w.method, w.path, w.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s without the scope: %s", w.method, w.path, rec.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
		assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
		if w.method == http.MethodDelete {
			deleteBody = rec.Body.String()
		}
	}
	rec := callWithKey(t, srv, without, http.MethodDelete, userInjectedSkillsPath+"/"+tid("no-such-entry"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "an unknown entry ID gets the same status: %s", rec.Body.String())
	assert.Equal(t, deleteBody, rec.Body.String(), "an unknown entry ID gets the same response body")
	assert.Equal(t, before, userInjectedSkillURIs(t, s, alice.ID), "a refused write changes nothing")

	rec = callWithKey(t, srv, without, http.MethodGet, userInjectedSkillsPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list api.SkillInjectionList
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Entries, 1)
	assert.Equal(t, seeded.SkillURI, list.Entries[0].SkillURI)

	with := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "user_skill_injection:update")
	writes := userInjectedSkillWrites(seeded.ID)
	// DELETE first, while the seeded entry is still there.
	rec = callWithKey(t, srv, with, writes[2].method, writes[2].path, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, userInjectedSkillURIs(t, s, alice.ID))
	rec = callWithKey(t, srv, with, writes[0].method, writes[0].path, writes[0].body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"skill://scion/added@1.0"}, userInjectedSkillURIs(t, s, alice.ID))
	rec = callWithKey(t, srv, with, writes[1].method, writes[1].path, writes[1].body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"skill://scion/replaced@1.0"}, userInjectedSkillURIs(t, s, alice.ID))
}

// TestUserInjectedSkillsWrite_ProjectBoundaryTokenDenied pins that
// user_skill_injection:update is hub-only: a project-boundary token with
// the scope cannot be minted, and a project-boundary token identity that
// carries it gets 403 on each write route and changes nothing.
func TestUserInjectedSkillsWrite_ProjectBoundaryTokenDenied(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(alice.ID), CreateTokenParams{
		UserID: alice.ID, Name: "si-" + tid("tok"), Boundary: projectBoundary(project.ID), Scopes: []string{"user_skill_injection:update"},
	})
	require.Error(t, err, "user_skill_injection:update is not mintable on a project boundary")

	seeded := seedUserInjectedSkill(t, s, alice.ID, "skill://scion/seeded@1.0")
	before := userInjectedSkillURIs(t, s, alice.ID)
	token := selfToken(t, alice.ID, projectBoundary(project.ID), "user_skill_injection:update")
	for _, w := range userInjectedSkillWrites(seeded.ID) {
		rec := callUserInjectedSkillsAs(t, srv, token, w.method, w.path, w.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s with a project-boundary token: %s", w.method, w.path, rec.Body.String())
	}
	assert.Equal(t, before, userInjectedSkillURIs(t, s, alice.ID))
}

// TestUserInjectedSkillsWrite_SessionAndDevUnchanged pins that an
// interactive session and a dev credential add, replace and remove their
// own entries without a token scope.
func TestUserInjectedSkillsWrite_SessionAndDevUnchanged(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)

	seeded := seedUserInjectedSkill(t, s, alice.ID, "skill://scion/seeded@1.0")
	writes := userInjectedSkillWrites(seeded.ID)
	rec := doRequestAsUser(t, srv, alice, writes[2].method, writes[2].path, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, writes[0].method, writes[0].path, writes[0].body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, writes[1].method, writes[1].path, writes[1].body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"skill://scion/replaced@1.0"}, userInjectedSkillURIs(t, s, alice.ID))

	dev := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})
	devSeeded := seedUserInjectedSkill(t, s, dev.ID(), "skill://scion/seeded@1.0")
	writes = userInjectedSkillWrites(devSeeded.ID)
	rec = callUserInjectedSkillsAs(t, srv, dev, writes[2].method, writes[2].path, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, userInjectedSkillURIs(t, s, dev.ID()))
	rec = callUserInjectedSkillsAs(t, srv, dev, writes[0].method, writes[0].path, writes[0].body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"skill://scion/added@1.0"}, userInjectedSkillURIs(t, s, dev.ID()))
	rec = callUserInjectedSkillsAs(t, srv, dev, writes[1].method, writes[1].path, writes[1].body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"skill://scion/replaced@1.0"}, userInjectedSkillURIs(t, s, dev.ID()))
}

// TestUserInjectedSkillsWrite_FederatedUserDenied pins that a federated
// user identity gets 403 on each write route and changes nothing, as on the
// inbox routes, and still reads its list.
func TestUserInjectedSkillsWrite_FederatedUserDenied(t *testing.T) {
	srv, s, _, _, _ := setupInjectedSkillsTest(t)
	fed := NewFederatedUserIdentity("https://issuer.si.test", "si-fed", "si-fed@test.com", "Fed", "member", nil)
	seeded := seedUserInjectedSkill(t, s, fed.ID(), "skill://scion/seeded@1.0")
	before := userInjectedSkillURIs(t, s, fed.ID())
	for _, w := range userInjectedSkillWrites(seeded.ID) {
		rec := callUserInjectedSkillsAs(t, srv, fed, w.method, w.path, w.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s as a federated user: %s", w.method, w.path, rec.Body.String())
	}
	assert.Equal(t, before, userInjectedSkillURIs(t, s, fed.ID()), "a refused write changes nothing")

	rec := callUserInjectedSkillsAs(t, srv, fed, http.MethodGet, userInjectedSkillsPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list api.SkillInjectionList
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Entries, 1)
	assert.Equal(t, seeded.SkillURI, list.Entries[0].SkillURI)
}

// TestProjectInjectedSkillsWrite_TokenNeedsProjectUpdate records that the
// project-scope write routes apply user access token scopes through
// CheckAccess: a project token without project:update gets 403 and changes
// nothing, and a project token with it adds an entry.
func TestProjectInjectedSkillsWrite_TokenNeedsProjectUpdate(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	path := "/api/v1/projects/" + project.ID + "/injected-skills"
	body := api.SkillInjectionEntry{SkillURI: "skill://scion/project-skill@1.0"}

	without := mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "project:read")
	rec := callWithKey(t, srv, without, http.MethodPost, path, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	sis, err := s.ListSkillInjections(context.Background(), store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis)

	with := mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "project:update")
	rec = callWithKey(t, srv, with, http.MethodPost, path, body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestUserInjectedSkillsWrite_TokenCredentialNeedsTokenIdentity pins that a
// request whose credential record names a user access token is checked as
// a token request: without a token identity each write route gets 403 and
// changes nothing.
func TestUserInjectedSkillsWrite_TokenCredentialNeedsTokenIdentity(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	seeded := seedUserInjectedSkill(t, s, alice.ID, "skill://scion/seeded@1.0")
	before := userInjectedSkillURIs(t, s, alice.ID)
	user := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, alice.Role, string(ClientTypeWeb))
	for _, w := range userInjectedSkillWrites(seeded.ID) {
		var raw []byte
		if w.body != nil {
			var err error
			raw, err = json.Marshal(w.body)
			require.NoError(t, err)
		}
		req := httptest.NewRequest(w.method, w.path, bytes.NewReader(raw))
		ctx := contextWithIdentity(req.Context(), user)
		ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindUAT})
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		if entryID, ok := strings.CutPrefix(w.path, userInjectedSkillsPath+"/"); ok {
			srv.handleUserMeInjectedSkillByID(rec, req, entryID)
		} else {
			srv.handleUserMeInjectedSkills(rec, req)
		}
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", w.method, w.path, rec.Body.String())
	}
	assert.Equal(t, before, userInjectedSkillURIs(t, s, alice.ID))
}
