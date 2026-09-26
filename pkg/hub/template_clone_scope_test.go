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
// ptone/scion#1977: template clone destination-scope validation.
//
// handleTemplateClone's destination-scope switch had no default case, so a
// scope value it did not recognize fell through the switch without ever
// being authorized. This suite originally pinned that fix together with
// normalizing the legacy "grove" scope name to "project" so legacy callers
// kept working.
//
// GCP#1968 (upstream 9385d07b, "stop accepting legacy grove request input")
// superseded the normalization half: createTemplateV2 and handleTemplateClone
// now call isValidTemplateScope up front and reject anything other than "",
// "global", "project" or "user" with 400 — including "grove" — before
// authorization or normalization ever runs. Accepting "grove" again would
// reintroduce the legacy input GCP#1968 removed, so this suite now pins
// rejection instead of normalization for create and clone. Only
// listTemplatesV2 (a read-only filter, not a write path) still normalizes
// "grove" so a caller can find templates stored under the canonical
// "project" scope before the legacy name is retired entirely; see
// TestListTemplatesV2_LegacyScopeName_FindsProjectTemplates below.
//
// The default case in handleTemplateClone's switch (mirroring
// handleHarnessConfigClone) is kept as a defense-in-depth safety net, but it
// is no longer reachable through the HTTP API: isValidTemplateScope already
// rejects every value the switch does not otherwise handle.
// ============================================================================

func TestTemplateClone_UnsupportedScope_Rejected(t *testing.T) {
	cases := []string{"bogus", "GROVE", "Project", "arbitrary", "global ", "grove"}
	for _, scope := range cases {
		t.Run(scope, func(t *testing.T) {
			srv, s, alice, _, project := setupTemplateAuthzTest(t)
			tpl := createAuthzTestTemplate(t, s, "unsupported-scope-source-"+scope, store.TemplateScopeProject, project.ID, alice.ID)

			rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
				Name:  "unsupported-scope-clone-" + scope,
				Scope: scope,
			})
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"an unrecognized destination scope, including the legacy \"grove\" name, must be rejected; got: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid scope")
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

// TestTemplateClone_LegacyScopeName_RejectedBeforeAuthorization asserts that
// the legacy "grove" scope name is rejected by isValidTemplateScope before
// authorization ever runs: an outsider with no rights on the destination
// project and a member with full create rights on it both get the same 400
// rejection, rather than the member succeeding. Accepting "grove" for one of
// them (normalizing it to "project") would reintroduce the legacy input
// GCP#1968 removed.
func TestTemplateClone_LegacyScopeName_RejectedBeforeAuthorization(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "legacy-scope-source", store.TemplateScopeGlobal, "", alice.ID)

	outsider := createNamedTestUser(t, s, "legacy-scope-outsider", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, outsider.ID)

	outsiderRec := doRequestAsUser(t, srv, outsider, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
		Name:    "legacy-scope-outsider-clone",
		Scope:   "grove",
		ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusBadRequest, outsiderRec.Code,
		"the legacy scope name must be rejected regardless of the caller's rights; got: %s", outsiderRec.Body.String())
	assert.Contains(t, outsiderRec.Body.String(), "invalid scope")

	member := createNamedTestUser(t, s, "legacy-scope-member", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, member.ID)
	createTestUserWithProjectRole(t, s, member.ID, member.Email, project.ID, store.ProjectRoleMember)

	memberRec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
		Name:    "legacy-scope-member-clone",
		Scope:   "grove",
		ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusBadRequest, memberRec.Code,
		"a project member must not be able to clone using the legacy scope name; got: %s", memberRec.Body.String())
	assert.Contains(t, memberRec.Body.String(), "invalid scope")
}

// TestTemplateClone_LegacyScopeName_RejectedBeforeCollisionCheck asserts that
// a clone request naming the legacy "grove" scope is rejected before the
// destination (scope, slug, scopeId) lookup ever runs, even when a template
// already occupies the canonical "project" target the legacy name would have
// resolved to. It must fail the same way regardless of what already exists
// at that target, and must leave the existing template untouched.
func TestTemplateClone_LegacyScopeName_RejectedBeforeCollisionCheck(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)

	// Stored canonically ("project"), at the same physical path
	// storage.TemplateStoragePath would compute for the legacy "grove" name.
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
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"the legacy scope name must be rejected before the collision lookup runs, not surfaced as a conflict; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid scope")

	reloaded, err := s.GetTemplate(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, existing.ContentHash, reloaded.ContentHash, "the existing template's record must be unaffected by a rejected legacy-scope clone attempt")
	assert.Equal(t, existingContent, stor.content[existingPath+"/scion-agent.yaml"],
		"the existing template's file content must survive a rejected legacy-scope clone attempt")
}

// TestCreateTemplateV2_LegacyScopeName_Rejected verifies that createTemplateV2
// (the generic POST /api/v1/templates handler) rejects the legacy "grove"
// scope name with 400 via isValidTemplateScope, the same as any other
// unrecognized scope, even for a caller with full create rights on the named
// project. See the package comment above TestTemplateClone_UnsupportedScope_Rejected
// for why this suite pins rejection rather than normalization here.
func TestCreateTemplateV2_LegacyScopeName_Rejected(t *testing.T) {
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
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a create request using the legacy scope name must be rejected even for a caller with full project rights; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid scope")
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
