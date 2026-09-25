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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectWorkspaceAuthz_ExternalBearerSameDecisionAsHubToken covers the
// interaction between this branch's external-bearer authentication path and
// upstream's project-workspace authorization gate: a request authenticated
// via a Google bearer token must reach exactly the same authz decision as
// the same identity authenticated with a normal Hub token (JWT). Nothing
// about the external-bearer path may special-case AuthTypeExternalBearer to
// skip or shortcut the workspace membership check.
//
// A pre-existing (provider, issuer, subject) binding is used to attach each
// test identity directly, rather than relying on the resolver's email-domain
// bootstrap policy (Gmail/Workspace/service-account only) — the point of
// this test is the authz decision after resolution, not the resolution
// policy itself, which is already covered elsewhere.
func TestProjectWorkspaceAuthz_ExternalBearerSameDecisionAsHubToken(t *testing.T) {
	srv, s, alice, bob, _ := setupTemplateAuthzTest(t)

	// Alice creates and owns this project via a normal Hub token, exactly as
	// createTestHubManagedProject does, but with a real (non-dev) owner so
	// the same identity can be reused as the "member" case below.
	projRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects",
		CreateProjectRequest{Name: "EB Workspace Authz Test"})
	require.Equal(t, http.StatusCreated, projRec.Code, "body: %s", projRec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(projRec.Body).Decode(&project))
	ws, err := hubManagedProjectPath(project.Slug)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(ws) })
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "SECRET.md"), []byte(workspaceSecret), 0o644))

	// Bind alice and bob to Google identities via a pre-existing external
	// identity binding (Resolve's Step 1 lookup), bypassing the
	// authoritative-email-domain bootstrap gate that alice/bob's
	// @test.com addresses would otherwise fail.
	now := time.Now()
	aliceIdentity := &ValidatedGoogleIdentity{
		Subject: "eb-authz-alice", Email: alice.Email, EmailVerified: true,
		Issuer: googleCanonicalIssuer, Audience: externalBearerTestAudience,
		UpstreamExpiry: now.Add(time.Hour),
	}
	bobIdentity := &ValidatedGoogleIdentity{
		Subject: "eb-authz-bob", Email: bob.Email, EmailVerified: true,
		Issuer: googleCanonicalIssuer, Audience: externalBearerTestAudience,
		UpstreamExpiry: now.Add(time.Hour),
	}
	require.NoError(t, s.CreateExternalIdentity(context.Background(), &store.ExternalIdentityBinding{
		ID: api.NewUUID(), Provider: "google", Issuer: googleCanonicalIssuer,
		Subject: aliceIdentity.Subject, UserID: alice.ID, Email: alice.Email,
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, s.CreateExternalIdentity(context.Background(), &store.ExternalIdentityBinding{
		ID: api.NewUUID(), Provider: "google", Issuer: googleCanonicalIssuer,
		Subject: bobIdentity.Subject, UserID: bob.ID, Email: bob.Email,
		CreatedAt: now, UpdatedAt: now,
	}))

	// Wire Google trust so a bearer token is classified as applicable to
	// this path at all (authenticateExternalBearer's first gate).
	srv.authConfig.FederationAuth.Store(newGoogleTrustFederationAuth(t, externalBearerTestAudience))

	doAsGoogleBearer := func(identity *ValidatedGoogleIdentity, method, url string) *httptest.ResponseRecorder {
		srv.authConfig.GoogleValidator = &fakeGoogleValidator{accessTokenResult: identity}
		req := httptest.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer opaque-google-access-token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	base := "/api/v1/projects/" + project.ID

	t.Run("non-member: Google-bearer decision matches Hub-token decision", func(t *testing.T) {
		hubTokenRec := doRequestAsUser(t, srv, bob, http.MethodGet, base+"/workspace/files", nil)
		require.Equal(t, http.StatusForbidden, hubTokenRec.Code,
			"sanity check: bob must be refused via a normal Hub token; body: %s", hubTokenRec.Body.String())

		googleBearerRec := doAsGoogleBearer(bobIdentity, http.MethodGet, base+"/workspace/files")
		assert.Equal(t, hubTokenRec.Code, googleBearerRec.Code,
			"external-bearer decision (%d) must match the Hub-token decision (%d) for the same non-member identity; body: %s",
			googleBearerRec.Code, hubTokenRec.Code, googleBearerRec.Body.String())
		assert.Equal(t, http.StatusForbidden, googleBearerRec.Code, "body: %s", googleBearerRec.Body.String())
		assert.NotContains(t, googleBearerRec.Body.String(), workspaceSecret,
			"response to a non-member leaked workspace content")
	})

	t.Run("member: Google-bearer decision matches Hub-token decision", func(t *testing.T) {
		hubTokenRec := doRequestAsUser(t, srv, alice, http.MethodGet, base+"/workspace/files/SECRET.md", nil)
		require.Equal(t, http.StatusOK, hubTokenRec.Code,
			"sanity check: alice (owner) must be admitted via a normal Hub token; body: %s", hubTokenRec.Body.String())

		googleBearerRec := doAsGoogleBearer(aliceIdentity, http.MethodGet, base+"/workspace/files/SECRET.md")
		assert.Equal(t, hubTokenRec.Code, googleBearerRec.Code,
			"external-bearer decision (%d) must match the Hub-token decision (%d) for the same member identity; body: %s",
			googleBearerRec.Code, hubTokenRec.Code, googleBearerRec.Body.String())
		assert.Equal(t, http.StatusOK, googleBearerRec.Code, "body: %s", googleBearerRec.Body.String())
		assert.Contains(t, googleBearerRec.Body.String(), workspaceSecret,
			"the project owner, authenticated via Google bearer, must still read their own workspace")
	})
}
