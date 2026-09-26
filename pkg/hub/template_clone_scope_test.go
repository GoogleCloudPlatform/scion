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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1977: template clone destination-scope validation and legacy
// scope-name normalization.
//
// handleTemplateClone's destination-scope switch had no default case, so a
// scope value it did not recognize (including the legacy "grove" name for
// what is now the "project" scope) fell through the switch without ever
// being authorized. The clone record itself was then built from the raw,
// unnormalized request scope rather than the value the switch validated, so
// a legacy-named clone could be stored under a scope value that the storage
// path resolver (storage.ResourceStoragePath) still treats as an alias for
// "project". This suite pins both fixes: an explicit default-403, mirroring
// handleHarnessConfigClone, and normalizing the legacy scope name via
// pkg/projectcompat before authorization, storage path resolution, and the
// record lookup/create all happen — so they can never resolve a clone
// request to different targets.
// ============================================================================

func TestTemplateClone_UnsupportedScope_Rejected(t *testing.T) {
	cases := []string{"bogus", "GROVE", "Project", "arbitrary", "global "}
	for _, scope := range cases {
		t.Run(scope, func(t *testing.T) {
			srv, s, alice, _, project := setupTemplateAuthzTest(t)
			tpl := createAuthzTestTemplate(t, s, "unsupported-scope-source-"+scope, store.TemplateScopeProject, project.ID, alice.ID)

			rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
				Name:  "unsupported-scope-clone-" + scope,
				Scope: scope,
			})
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"an unrecognized destination scope must be rejected; got: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "Cloning into this resource scope is not supported")
		})
	}
}

// TestTemplateClone_UnsupportedScope_MatchesHarnessConfig pins template
// clone and harness-config clone to identical behavior for a destination
// scope neither recognizes, so the two cannot drift apart again.
func TestTemplateClone_UnsupportedScope_MatchesHarnessConfig(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "parity-scope-source", store.TemplateScopeProject, project.ID, alice.ID)

	ctx := context.Background()
	hc := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "parity-scope-hc-source", Name: "Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	tplRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
		Name:  "parity-scope-clone",
		Scope: "bogus",
	})
	hcRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/clone", map[string]interface{}{
		"name":  "parity-scope-clone",
		"scope": "bogus",
	})

	assert.Equal(t, hcRec.Code, tplRec.Code, "template and harness-config clone must reject an unknown scope identically")
	assert.Equal(t, hcRec.Body.String(), tplRec.Body.String(), "template and harness-config clone must reject an unknown scope with the same message")
}

// TestTemplateClone_LegacyScopeName_AuthorizedAsItsCanonicalTarget asserts
// that the legacy "grove" scope name is normalized to "project" before
// authorization runs: a caller without create rights on the destination
// project is denied exactly as they would be under the canonical "project"
// scope name, and an authorized project member succeeds and gets back a
// record stored under the canonical scope name.
func TestTemplateClone_LegacyScopeName_AuthorizedAsItsCanonicalTarget(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "legacy-scope-source", store.TemplateScopeGlobal, "", alice.ID)

	outsider := createNamedTestUser(t, s, "legacy-scope-outsider", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, outsider.ID)

	deniedRec := doRequestAsUser(t, srv, outsider, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
		Name:    "legacy-scope-denied-clone",
		Scope:   "grove",
		ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusForbidden, deniedRec.Code,
		"a caller without create rights on the target project must be denied under the legacy scope name too; got: %s", deniedRec.Body.String())

	member := createNamedTestUser(t, s, "legacy-scope-member", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, member.ID)
	createTestUserWithProjectRole(t, s, member.ID, member.Email, project.ID, store.ProjectRoleMember)

	allowedRec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
		Name:    "legacy-scope-allowed-clone",
		Scope:   "grove",
		ScopeID: project.ID,
	})
	require.Equal(t, http.StatusCreated, allowedRec.Code, "a project member must still be able to clone using the legacy scope name; got: %s", allowedRec.Body.String())

	var clone store.Template
	require.NoError(t, json.NewDecoder(allowedRec.Body).Decode(&clone))
	assert.Equal(t, store.TemplateScopeProject, clone.Scope,
		"the legacy scope name must be normalized to its canonical form before the record is stored")
	assert.Equal(t, project.ID, clone.ScopeID)
}

// TestTemplateClone_LegacyScopeName_CollisionDetected is the normalization
// counterpart to TestTemplateClone_CollisionLeavesExistingTemplateIntact: a
// clone request naming the legacy scope must resolve to the same (scope,
// slug, scopeId) target as a request naming the canonical scope, so an
// existing template at that target is found and reported as a conflict
// rather than missed by the lookup.
func TestTemplateClone_LegacyScopeName_CollisionDetected(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)

	// The existing template is stored canonically ("project"), at the same
	// physical path storage.TemplateStoragePath computes for the legacy
	// "grove" alias — this is the path the two must agree on.
	existingPath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeProject, project.ID, "legacy-collide")
	existingContent := []byte("scion-agent-config: existing\n")
	stor.seedObject(existingPath+"/scion-agent.yaml", existingContent)
	existing := &store.Template{
		ID: api.NewUUID(), Slug: "legacy-collide", Name: "Existing", Harness: "claude",
		Scope: store.TemplateScopeProject, ScopeID: project.ID, OwnerID: alice.ID,
		Status: store.TemplateStatusActive, StoragePath: existingPath, StorageBucket: "test-bucket",
		Files: []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len(existingContent))}},
	}
	existing.ContentHash = computeContentHash(existing.Files)
	require.NoError(t, s.CreateTemplate(ctx, existing))

	sourcePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "legacy-collide-source")
	sourceContent := []byte("scion-agent-config: source\n")
	stor.seedObject(sourcePath+"/scion-agent.yaml", sourceContent)
	source := &store.Template{
		ID: api.NewUUID(), Slug: "legacy-collide-source", Name: "Source", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket",
		Files: []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len(sourceContent))}},
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateTemplate(ctx, source))

	member := createNamedTestUser(t, s, "legacy-collide-member", store.UserRoleMember)
	ensureHubMembership(ctx, s, member.ID)
	createTestUserWithProjectRole(t, s, member.ID, member.Email, project.ID, store.ProjectRoleMember)

	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/templates/"+source.ID+"/clone", CloneTemplateRequest{
		Name:    "legacy-collide", // same slug as the existing project-scoped template
		Scope:   "grove",
		ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusConflict, rec.Code,
		"a legacy-scope clone that targets an already-occupied (scope, slug) pair must conflict, not silently succeed; got: %s", rec.Body.String())

	reloaded, err := s.GetTemplate(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, existing.ContentHash, reloaded.ContentHash, "the existing template's record must be unaffected by a colliding legacy-scope clone attempt")
	assert.Equal(t, existingContent, stor.content[existingPath+"/scion-agent.yaml"],
		"the existing template's file content must survive a colliding legacy-scope clone attempt")
}

// TestCreateTemplateV2_LegacyScopeName_GetsProjectParent verifies that
// createTemplateV2 (the generic POST /api/v1/templates handler) also
// normalizes the legacy "grove" scope name via
// projectcompat.CanonicalResourceScope, so a create request naming it
// authorizes against the project parent and the stored record ends up
// scoped canonically, matching handleTemplateClone's normalization.
func TestCreateTemplateV2_LegacyScopeName_GetsProjectParent(t *testing.T) {
	srv, s, _, _, project := setupTemplateAuthzTest(t)

	member := createNamedTestUser(t, s, "create-legacy-scope-member", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, member.ID)
	createTestUserWithProjectRole(t, s, member.ID, member.Email, project.ID, store.ProjectRoleMember)

	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name:    "create-legacy-scope-template",
		Harness: "claude",
		Scope:   "grove",
		ScopeID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code,
		"a project member must be able to create using the legacy scope name; got: %s", rec.Body.String())

	var resp CreateTemplateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, store.TemplateScopeProject, resp.Template.Scope,
		"the legacy scope name must be normalized to its canonical form before the record is stored")
	assert.Equal(t, project.ID, resp.Template.ScopeID)
}

// TestListTemplatesV2_LegacyScopeName_FindsProjectTemplates verifies that
// listTemplatesV2 (the generic GET /api/v1/templates handler) normalizes the
// legacy "grove" scope name before filtering, so a list request naming it
// finds templates stored under the canonical "project" scope rather than
// returning nothing.
func TestListTemplatesV2_LegacyScopeName_FindsProjectTemplates(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "list-legacy-scope-target", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates?scope=grove&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListTemplatesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	var found bool
	for _, item := range resp.Templates {
		if item.ID == tpl.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "listing with the legacy scope name must find templates stored under the canonical project scope")
}
