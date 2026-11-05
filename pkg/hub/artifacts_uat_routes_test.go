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
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded: a user
// access token's ceiling and project boundary bound every manage route
// (PATCH, grants, share links, DELETE) and every version route of an
// artifact its user owns (ptone/scion#4574). A token without artifact
// read, or bounded to another project, gets 404 as for a missing artifact;
// a token that can read but lacks artifact manage (manage routes) or
// artifact create (version writes) gets 403; a token with them is allowed.
// It is the pin of the catalog's bearer disposition for these routes.
func TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	p1 := artifactProject(t, s, "uatr-p1")
	p2 := artifactProject(t, s, "uatr-p2")
	createTestUserWithProjectRole(t, s, tid("uatr-user"), "uatr-user@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("uatr-user"), "uatr-user@test.com", p2.ID, store.ProjectRoleMember)
	user, err := s.GetUser(t.Context(), tid("uatr-user"))
	require.NoError(t, err)
	session := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")

	publish := func(name string, body []byte) string {
		t.Helper()
		rec := identityArtifactRequest(t, srv, session, http.MethodPost, "/api/v1/artifacts?name="+name+"&scope="+p1.ID, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		return decodeArtifactID(t, rec)
	}
	// Both owned by the session user, homed in p1: one for every route
	// but DELETE, one to delete.
	id := publish("index.html", []byte("<p>v1</p>"))
	doomed := publish("doomed.md", []byte("# doomed\n"))
	base := "/api/v1/artifacts/" + id

	// A grant and a share link for the delete and revoke routes, made by
	// the session user.
	rec := identityArtifactRequest(t, srv, session, http.MethodPost, base+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+tid("uatr-friend")+`","permission":"read"}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant artifacts.GrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	rec = identityArtifactRequest(t, srv, session, http.MethodPost, base+"/links", []byte(`{"ttlHours":1}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var link artifacts.CreateLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &link))

	v2 := []byte("<p>v2</p>")
	sum := sha256.Sum256(v2)
	manifest, err := json.Marshal(artifacts.CreateVersionRequest{Entry: "index.html",
		Files: []artifacts.ManifestFile{{Path: "index.html", Size: int64(len(v2)), SHA256: hex.EncodeToString(sum[:])}}})
	require.NoError(t, err)

	noRead := artifactTestUAT(t, session, p1.ID, "agent:read")
	readOnly := artifactTestUAT(t, session, p1.ID, "artifact:read")
	elsewhere := artifactTestUAT(t, session, p2.ID, "artifact:read", "artifact:manage", "artifact:create")
	manage := artifactTestUAT(t, session, p1.ID, "artifact:read", "artifact:manage")
	create := artifactTestUAT(t, session, p1.ID, "artifact:read", "artifact:create")

	check := func(name string, identity Identity, method, path string, body []byte, want int) {
		t.Helper()
		rec := identityArtifactRequest(t, srv, identity, method, path, body)
		assert.Equal(t, want, rec.Code, "%s: %s %s: %s", name, method, path, rec.Body.String())
	}
	refused := func(method, path string, body []byte, lacking bool) {
		t.Helper()
		check("without artifact:read", noRead, method, path, body, http.StatusNotFound)
		check("bounded to another project", elsewhere, method, path, body, http.StatusNotFound)
		if lacking {
			check("read only", readOnly, method, path, body, http.StatusForbidden)
		}
	}

	// Manage routes: refused, then allowed with artifact:manage. The
	// allowed requests are chosen to change nothing the later rows need
	// (PATCH {} and POST grants {} are refused as malformed after the
	// authorization check).
	manageRoutes := []struct {
		method, path string
		body         []byte
		allowed      int
	}{
		{http.MethodPatch, base, []byte(`{}`), http.StatusBadRequest},
		{http.MethodGet, base + "/grants", nil, http.StatusOK},
		{http.MethodPost, base + "/grants", []byte(`{}`), http.StatusBadRequest},
		{http.MethodGet, base + "/links", nil, http.StatusOK},
		{http.MethodPost, base + "/links", []byte(`{"ttlHours":1}`), http.StatusCreated},
		{http.MethodDelete, base + "/grants/" + grant.Grant.ID, nil, http.StatusNoContent},
		{http.MethodDelete, base + "/links/" + link.Link.ID, nil, http.StatusNoContent},
		{http.MethodDelete, "/api/v1/artifacts/" + doomed, nil, http.StatusNoContent},
	}
	for _, r := range manageRoutes {
		refused(r.method, r.path, r.body, true)
		check("artifact:manage", manage, r.method, r.path, r.body, r.allowed)
	}

	// Version writes: refused, then allowed with artifact:create, in the
	// order a publish runs.
	refused(http.MethodPost, base+"/versions", manifest, true)
	rec = identityArtifactRequest(t, srv, create, http.MethodPost, base+"/versions", manifest)
	require.Equal(t, http.StatusCreated, rec.Code, "artifact:create: create version: %s", rec.Body.String())
	var pend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pend))
	vbase := fmt.Sprintf("%s/versions/%d", base, pend.Version.Seq)
	refused(http.MethodPut, vbase+"/files/index.html", v2, true)
	check("artifact:create", create, http.MethodPut, vbase+"/files/index.html", v2, http.StatusNoContent)
	refused(http.MethodPost, vbase+"/finalize", nil, true)
	check("artifact:create", create, http.MethodPost, vbase+"/finalize", nil, http.StatusOK)

	// The archive route answers 404 to everyone while hub.artifact_archive
	// is off, the owner's session included.
	check("archive, experiment off", session, http.MethodGet, base+"/versions/1/archive", nil, http.StatusNotFound)
	enableArtifactArchiveForTest(t, srv)

	// Version reads, the archive and the view mint need artifact:read
	// only, and are refused exactly like a file read.
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, base + "/versions"},
		{http.MethodGet, base + "/versions/1"},
		{http.MethodGet, base + "/versions/1/files/index.html"},
		{http.MethodGet, base + "/versions/1/archive"},
		{http.MethodHead, base + "/versions/1/archive"},
		{http.MethodPost, base + "/versions/1/view"},
	} {
		refused(r.method, r.path, nil, false)
		check("artifact:read", readOnly, r.method, r.path, nil, http.StatusOK)
	}
}

// enableArtifactArchiveForTest turns hub.artifact_archive on for srv,
// keeping every other default of its current registry (hub.artifacts
// included, after enableArtifactsForTest).
func enableArtifactArchiveForTest(t *testing.T, srv *Server) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == experiments.ArtifactArchive {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.True(t, srv.experimentEnabled(experiments.ArtifactArchive))
	require.True(t, srv.experimentEnabled(experiments.Artifacts))
}

// TestArtifactArchiveRoute: through the real hub, the archive route of
// ptone/scion#7106. While hub.artifact_archive is off it answers 404 and
// the version response has no archive field; while it is on, the owner
// downloads the version's files as a zip and the response says it is
// available. A share link never reaches it: neither the share-link route
// nor the view route serves an archive (404), and the link token is not a
// credential for the archive route (the same answer as for a file read
// with it).
func TestArtifactArchiveRoute(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	p1 := artifactProject(t, s, "arch-p1")
	createTestUserWithProjectRole(t, s, tid("arch-owner"), "arch-owner@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(t.Context(), tid("arch-owner"))
	require.NoError(t, err)

	body := []byte("# Archived report\n")
	rec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=report.md&title=Q3+Report&scope="+p1.ID, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	base := "/api/v1/artifacts/" + id
	archive := base + "/versions/1/archive"

	versionResponse := func() artifacts.ArtifactResponse {
		t.Helper()
		rec := userArtifactRequest(t, srv, owner, http.MethodGet, base+"/versions/1", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp artifacts.ArtifactResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	// Off: 404, as for a missing route, and no archive field.
	off := userArtifactRequest(t, srv, owner, http.MethodGet, archive, nil)
	assert.Equal(t, http.StatusNotFound, off.Code, off.Body.String())
	assert.Equal(t, userArtifactRequest(t, srv, owner, http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000000", nil).Body.String(),
		off.Body.String(), "the experiment-off answer is the not-found answer")
	assert.Nil(t, versionResponse().Archive)

	enableArtifactArchiveForTest(t, srv)
	resp := versionResponse()
	require.NotNil(t, resp.Archive)
	assert.True(t, resp.Archive.Available)

	rec = userArtifactRequest(t, srv, owner, http.MethodGet, archive, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	assert.Equal(t, `attachment; filename=q3-report-v1.zip`, rec.Header().Get("Content-Disposition"))
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	require.Len(t, zr.File, 1)
	assert.Equal(t, "report.md", zr.File[0].Name)
	rc, err := zr.File[0].Open()
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	// A share link of the artifact never yields an archive.
	rec = userArtifactRequest(t, srv, owner, http.MethodPost, base+"/links", []byte(`{"ttlHours": 1}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created artifacts.CreateLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	token := strings.TrimPrefix(created.URL, artifacts.RouteShared)
	shared := anonymous(srv, http.MethodGet, created.URL)
	require.Equal(t, http.StatusSeeOther, shared.Code, shared.Body.String())
	loc := shared.Header().Get("Location")
	capability, _, ok := strings.Cut(strings.TrimPrefix(loc, artifacts.RouteView), "/")
	require.True(t, ok, loc)
	for _, target := range []string{
		created.URL + "/archive",
		created.URL + "/versions/1/archive",
		artifacts.RouteView + capability + "/archive",
		artifacts.RouteView + capability + "/versions/1/archive",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := anonymous(srv, method, target)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", method, target)
			assert.NotEqual(t, "application/zip", rec.Header().Get("Content-Type"), "%s %s", method, target)
		}
	}
	withToken := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	fileRead := withToken(base + "/versions/1/files/report.md")
	archiveRead := withToken(archive)
	assert.NotEqual(t, http.StatusOK, archiveRead.Code)
	assert.Equal(t, fileRead.Code, archiveRead.Code, "the link token is refused on the archive route as on a file read")
	assert.NotEqual(t, "application/zip", archiveRead.Header().Get("Content-Type"))
}
