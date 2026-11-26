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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doRawAgentTokenRequest(t *testing.T, srv *Server, method, rawURL, token, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	req := httptest.NewRequest(method, u.RequestURI(), bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestAgentSkillPublish_OwnProjectSucceeds verifies the full end-to-end
// project-scope skill publish lifecycle for an agent with project:skill:write
// delegated by a project owner: create skill, publish v1.0.0 via two-phase
// upload + finalize, publish v1.1.0, and publish v2.0.0 via multipart
// (ptone/scion#8734).
func TestAgentSkillPublish_OwnProjectSucceeds(t *testing.T) {
	f := setupAgentSkillFixture(t)
	stor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "test-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	f.srv.SetStorage(stor)

	owner := createNamedTestUser(t, f.s, "skillpub-owner", store.UserRoleMember)
	createTestUserWithProjectRole(t, f.s, owner.ID, owner.Email, f.p.ID, store.ProjectRoleOwner)
	token := f.newAgentWithRole(t, "skillpub-agent-full", f.p.ID, owner.ID, AgentRoleFull)

	// 1. Create a new project-scoped skill in the agent's own project.
	createRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:        "agent-built-skill",
		Description: "Published by an agent",
		Scope:       store.SkillScopeProject,
		ScopeID:     f.p.ID,
	}, token)
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateSkillResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Skill)
	skillID := createResp.Skill.ID
	assert.Equal(t, store.SkillScopeProject, createResp.Skill.Scope)
	assert.Equal(t, f.p.ID, createResp.Skill.ScopeID)

	// 2. Publish v1.0.0 via two-phase flow: create draft -> upload file -> finalize.
	skillMD := []byte("---\nname: agent-built-skill\ndescription: v1\n---\n# Agent Skill v1\n")
	pubRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+skillID+"/versions", PublishVersionRequest{
		Version: "1.0.0",
		Files:   []FileUploadRequest{{Path: "SKILL.md", Size: int64(len(skillMD))}},
	}, token)
	require.Equal(t, http.StatusCreated, pubRec.Code, pubRec.Body.String())
	var pubResp PublishVersionResponse
	require.NoError(t, json.Unmarshal(pubRec.Body.Bytes(), &pubResp))
	require.Len(t, pubResp.UploadURLs, 1)

	putRec := doRawAgentTokenRequest(t, f.srv, http.MethodPut, pubResp.UploadURLs[0].URL, token, "application/octet-stream", skillMD)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	finRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+skillID+"/finalize", FinalizeSkillVersionRequest{
		Version: "1.0.0",
		Manifest: &SkillManifest{
			Files: []store.TemplateFile{{Path: "SKILL.md", Size: int64(len(skillMD)), Hash: sha256Hex(skillMD)}},
		},
	}, token)
	require.Equal(t, http.StatusOK, finRec.Code, finRec.Body.String())
	var sv store.SkillVersion
	require.NoError(t, json.Unmarshal(finRec.Body.Bytes(), &sv))
	assert.Equal(t, "1.0.0", sv.Version)
	assert.Equal(t, store.SkillVersionStatusPublished, sv.Status)

	// 3. Publish v2.0.0 via inline multipart upload.
	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	require.NoError(t, mw.WriteField("version", "2.0.0"))
	fw, err := mw.CreateFormFile("file", "SKILL.md")
	require.NoError(t, err)
	_, err = fw.Write([]byte("---\nname: agent-built-skill\ndescription: v2\n---\n# Agent Skill v2\n"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	mpRec := doRawAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+skillID+"/versions", token, mw.FormDataContentType(), mpBuf.Bytes())
	require.Equal(t, http.StatusCreated, mpRec.Code, mpRec.Body.String())

	// 4. Delete remains denied: project:skill:write does not include skill.delete.
	delRec := doAgentTokenRequest(t, f.srv, http.MethodDelete, "/api/v1/skills/"+skillID, nil, token)
	assert.Equal(t, http.StatusForbidden, delRec.Code, delRec.Body.String())
}

// TestAgentSkillPublish_OutOfScopeDenied verifies that an agent with
// project:skill:write cannot create or publish versions for another project's
// skill, a global skill, or a user-scoped skill (even its own creator's user
// skill) (ptone/scion#8734).
func TestAgentSkillPublish_OutOfScopeDenied(t *testing.T) {
	f := setupAgentSkillFixture(t)
	stor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "test-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	f.srv.SetStorage(stor)

	owner := createNamedTestUser(t, f.s, "skillpub-deny-owner", store.UserRoleMember)
	ensureHubMembership(context.Background(), f.s, owner.ID)
	createTestUserWithProjectRole(t, f.s, owner.ID, owner.Email, f.p.ID, store.ProjectRoleOwner)
	// Even when the creator is also owner of project Q, an agent scoped to P
	// must never write to Q.
	createTestUserWithProjectRole(t, f.s, owner.ID, owner.Email, f.q.ID, store.ProjectRoleOwner)
	ownerUserSkill := createTestSkill(t, f.s, "owner-user-skill", store.SkillScopeUser, owner.ID, owner.ID)

	token := f.newAgentWithRole(t, "skillpub-deny-agent", f.p.ID, owner.ID, AgentRoleFull)

	// Create in other project, global, and user scopes must all be denied.
	for _, tc := range []struct {
		name    string
		scope   string
		scopeID string
	}{
		{"other-project", store.SkillScopeProject, f.q.ID},
		{"global", store.SkillScopeGlobal, ""},
		{"default-global", "", ""},
		{"user", store.SkillScopeUser, owner.ID},
	} {
		t.Run("create-"+tc.name, func(t *testing.T) {
			rec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
				Name:    "denied-" + tc.name,
				Scope:   tc.scope,
				ScopeID: tc.scopeID,
			}, token)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}

	// Publishing versions, uploading files, and finalizing against out-of-scope
	// skills (other project, global, creator's own user skill) must be denied.
	skillMD := []byte("# denied\n")
	for _, tc := range []struct {
		name  string
		skill *store.Skill
	}{
		{"other-project", f.sq},
		{"global", f.sg},
		{"creator-user-skill", ownerUserSkill},
		{"other-user-skill", f.sv},
	} {
		t.Run("publish-"+tc.name, func(t *testing.T) {
			pubRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+tc.skill.ID+"/versions", PublishVersionRequest{
				Version: "9.0.0",
				Files:   []FileUploadRequest{{Path: "SKILL.md", Size: int64(len(skillMD))}},
			}, token)
			assert.Equal(t, http.StatusForbidden, pubRec.Code, pubRec.Body.String())

			upRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+tc.skill.ID+"/upload", map[string]any{
				"version": "9.0.0",
				"files":   []FileUploadRequest{{Path: "SKILL.md", Size: int64(len(skillMD))}},
			}, token)
			assert.Equal(t, http.StatusForbidden, upRec.Code, upRec.Body.String())

			putRec := doRawAgentTokenRequest(t, f.srv, http.MethodPut, "/api/v1/skills/"+tc.skill.ID+"/files/SKILL.md?version=9.0.0", token, "application/octet-stream", skillMD)
			assert.Equal(t, http.StatusForbidden, putRec.Code, putRec.Body.String())

			finRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+tc.skill.ID+"/finalize", FinalizeSkillVersionRequest{
				Version:  "9.0.0",
				Manifest: &SkillManifest{Files: []store.TemplateFile{{Path: "SKILL.md", Size: int64(len(skillMD)), Hash: sha256Hex(skillMD)}}},
			}, token)
			assert.Equal(t, http.StatusForbidden, finRec.Code, finRec.Body.String())
		})
	}
}

// TestAgentSkillPublish_RoleAndCeilingEnforced verifies that agents without
// project:skill:write (baseline/readonly), agents whose creator lacks
// skill.create/skill.update (project-member), and project-less agents are all
// denied.
func TestAgentSkillPublish_RoleAndCeilingEnforced(t *testing.T) {
	f := setupAgentSkillFixture(t)
	owner := createNamedTestUser(t, f.s, "skillpub-role-owner", store.UserRoleMember)
	createTestUserWithProjectRole(t, f.s, owner.ID, owner.Email, f.p.ID, store.ProjectRoleOwner)

	// Baseline and readonly roles do not carry project:skill:write.
	for _, role := range []AgentRole{AgentRoleBaseline, AgentRoleReadOnly} {
		t.Run("role-"+string(role), func(t *testing.T) {
			token := f.newAgentWithRole(t, "skillpub-role-"+string(role), f.p.ID, owner.ID, role)
			createRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
				Name:    "role-denied-" + string(role),
				Scope:   store.SkillScopeProject,
				ScopeID: f.p.ID,
			}, token)
			assert.Equal(t, http.StatusForbidden, createRec.Code, createRec.Body.String())
			assertSkillMissingScopeResponse(t, createRec)

			pubRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+f.sp.ID+"/versions", PublishVersionRequest{
				Version: "2.0.0",
			}, token)
			assert.Equal(t, http.StatusForbidden, pubRec.Code, pubRec.Body.String())
			assertSkillMissingScopeResponse(t, pubRec)
		})
	}

	// Delegation ceiling: an AgentRoleFull agent created by a project-member
	// (f.u, who lacks skill.create and skill.update) is denied.
	t.Run("member-creator-ceiling", func(t *testing.T) {
		token := f.newAgentWithRole(t, "skillpub-member-ceiling", f.p.ID, f.u.ID, AgentRoleFull)
		createRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
			Name:    "ceiling-denied-skill",
			Scope:   store.SkillScopeProject,
			ScopeID: f.p.ID,
		}, token)
		assert.Equal(t, http.StatusForbidden, createRec.Code, createRec.Body.String())
		assert.Contains(t, createRec.Body.String(), `"code":"forbidden"`)

		pubRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+f.sp.ID+"/versions", PublishVersionRequest{
			Version: "2.0.0",
		}, token)
		assert.Equal(t, http.StatusForbidden, pubRec.Code, pubRec.Body.String())
		assert.Contains(t, pubRec.Body.String(), `"code":"forbidden"`)
	})

	// Real mint path: an AgentRoleFull agent whose token was minted through
	// AuthorizeAgentToken under a creator ceiling lacking skill.create/update
	// has project:skill:write dropped from its JWT, and gets 403 missing_scope
	// naming project:skill:write on own-project skill writes (ptone/scion#9452),
	// while cross-project and global denials stay plain 403 forbidden.
	t.Run("ceiling-dropped-scope-missing-scope", func(t *testing.T) {
		ctx := context.Background()
		agent := &store.Agent{
			ID: tid("skillpub-dropped-scope"), Slug: tid("skillpub-dropped-scope"), Name: "skillpub-dropped-scope",
			ProjectID: f.p.ID, Phase: string(state.PhaseRunning),
			CreatedBy: f.u.ID, OwnerID: f.u.ID, Ancestry: []string{f.u.ID},
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, f.s.CreateAgent(ctx, agent))
		require.NoError(t, f.s.CreateDelegationEdge(ctx, &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.u.ID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: agent.ID,
			ScopeType: store.RoleScopeProject, ScopeID: f.p.ID,
			Role: string(AgentRoleFull), Active: true,
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
		grant, err := f.srv.AuthorizeAgentToken(ctx, agent)
		require.NoError(t, err)
		assert.NotContains(t, grant.Scopes, ScopeProjectSkillWrite)
		assert.Contains(t, grant.DroppedScopes, ScopeProjectSkillWrite)

		token := f.srv.generateRecordedAgentToken(t, agent.ID, f.p.ID, grant.Scopes, grant.Ancestry)

		createRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
			Name:    "dropped-scope-skill",
			Scope:   store.SkillScopeProject,
			ScopeID: f.p.ID,
		}, token)
		assert.Equal(t, http.StatusForbidden, createRec.Code, createRec.Body.String())
		assertSkillMissingScopeResponse(t, createRec)

		pubRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+f.sp.ID+"/versions", PublishVersionRequest{
			Version: "2.0.0",
		}, token)
		assert.Equal(t, http.StatusForbidden, pubRec.Code, pubRec.Body.String())
		assertSkillMissingScopeResponse(t, pubRec)

		otherRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/"+f.sq.ID+"/versions", PublishVersionRequest{
			Version: "2.0.0",
		}, token)
		assert.Equal(t, http.StatusForbidden, otherRec.Code, otherRec.Body.String())
		assert.Contains(t, otherRec.Body.String(), `"code":"forbidden"`)

		globalRec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
			Name:  "dropped-scope-global",
			Scope: store.SkillScopeGlobal,
		}, token)
		assert.Equal(t, http.StatusForbidden, globalRec.Code, globalRec.Body.String())
		assert.Contains(t, globalRec.Body.String(), `"code":"forbidden"`)
	})

	// A project-less agent with project:skill:write cannot create or update any
	// project's skill.
	t.Run("projectless-agent", func(t *testing.T) {
		ident := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:   jwt.Claims{Subject: tid("skillpub-projectless")},
			Scopes:   ScopesForRole(AgentRoleFull),
			Ancestry: []string{owner.ID},
		}}
		ctx := context.Background()
		assert.False(t, f.srv.authzService.CheckAccess(ctx, ident, skillScopeResource(store.SkillScopeProject, f.p.ID), ActionCreate).Allowed)
		assert.False(t, f.srv.authzService.CheckAccess(ctx, ident, skillResource(f.sp), ActionUpdate).Allowed)
	})
}

func assertSkillMissingScopeResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.Equal(t, "missing_scope", body.Error.Code, rec.Body.String())
	assert.Contains(t, body.Error.Message, string(ScopeProjectSkillWrite), rec.Body.String())
	assert.Equal(t, string(ScopeProjectSkillWrite), body.Error.Details["scope"], rec.Body.String())
	assert.Equal(t, []any{string(ScopeProjectSkillWrite)}, body.Error.Details["scopes"], rec.Body.String())
}
