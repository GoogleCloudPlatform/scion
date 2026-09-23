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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publishTestSkillVersion adds a published 1.0.0 version to skill.
func publishTestSkillVersion(t *testing.T, s store.Store, skill *store.Skill) {
	t.Helper()
	require.NoError(t, s.CreateSkillVersion(context.Background(), &store.SkillVersion{
		ID:          api.NewUUID(),
		SkillID:     skill.ID,
		Version:     "1.0.0",
		ContentHash: "sha256:test",
		Status:      store.SkillVersionStatusPublished,
		Created:     time.Now(),
	}))
}

// dispatchTestAgent returns an agent created by creatorID whose inline config
// declares refs as required skills.
func dispatchTestAgent(creatorID, projectID string, refs ...string) *store.Agent {
	skills := make([]api.SkillReference, len(refs))
	for i, r := range refs {
		skills[i] = api.SkillReference{URI: r, Scope: "template"}
	}
	return &store.Agent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		CreatedBy: creatorID,
		OwnerID:   creatorID,
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{Skills: skills},
		},
	}
}

// The #1784 scenario: a private (default visibility) global skill referenced
// by an agent's config. The broker cannot read it, but the Hub resolves it at
// dispatch as the creating member, who can.
func TestPreResolveAgentSkills_PrivateGlobalSkill_CreatorAllowed(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "private-global", store.SkillScopeGlobal, "", alice.ID)
	publishTestSkillVersion(t, s, skill)

	uri := "skill://scion/global/private-global@latest"
	agent := dispatchTestAgent(alice.ID, project.ID, uri)

	resp := srv.preResolveAgentSkills(context.Background(), agent)
	require.NotNil(t, resp)
	assert.Empty(t, resp.Errors)
	require.Len(t, resp.Resolved, 1)
	assert.Equal(t, uri, resp.Resolved[0].URI)
	assert.Equal(t, "1.0.0", resp.Resolved[0].ResolvedVersion)
	assert.Equal(t, "sha256:test", resp.Resolved[0].ContentHash)
}

func TestPreResolveAgentSkills_PrivateProjectSkill_CreatorAllowed(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "private-proj", store.SkillScopeProject, project.ID, alice.ID)
	publishTestSkillVersion(t, s, skill)

	uri := "skill://scion/project/" + project.ID + "/private-proj@latest"
	resp := srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(alice.ID, project.ID, uri))
	require.NotNil(t, resp)
	assert.Empty(t, resp.Errors)
	require.Len(t, resp.Resolved, 1)
	assert.Equal(t, uri, resp.Resolved[0].URI)
}

// A creator without read access gets a per-skill forbidden error that names
// the creator's permissions, not the broker's.
func TestPreResolveAgentSkills_CreatorWithoutAccess_Forbidden(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "alice-only", store.SkillScopeProject, project.ID, alice.ID)
	publishTestSkillVersion(t, s, skill)

	uri := "skill://scion/project/" + project.ID + "/alice-only"
	resp := srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(bob.ID, project.ID, uri))
	require.NotNil(t, resp)
	assert.Empty(t, resp.Resolved)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, uri, resp.Errors[0].URI)
	assert.Equal(t, "forbidden", resp.Errors[0].Code)
	assert.Equal(t, dispatchSkillForbiddenMessage, resp.Errors[0].Message)
}

func TestPreResolveAgentSkills_PublicSkill_Unchanged(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "public-one", store.SkillScopeGlobal, "", alice.ID)
	skill.Visibility = store.VisibilityPublic
	require.NoError(t, s.UpdateSkill(context.Background(), skill))
	publishTestSkillVersion(t, s, skill)

	resp := srv.preResolveAgentSkills(context.Background(),
		dispatchTestAgent(bob.ID, project.ID, "skill://scion/global/public-one"))
	require.NotNil(t, resp)
	assert.Empty(t, resp.Errors)
	require.Len(t, resp.Resolved, 1)
}

// The dispatching request's identity wins when it is a user. A broker
// identity in context never qualifies; the recorded creator is used instead.
func TestPreResolveAgentSkills_IdentitySelection(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "sel-skill", store.SkillScopeProject, project.ID, alice.ID)
	publishTestSkillVersion(t, s, skill)
	uri := "skill://scion/project/" + project.ID + "/sel-skill"

	t.Run("broker identity in context falls back to creator", func(t *testing.T) {
		ctx := contextWithIdentity(context.Background(), NewBrokerIdentity("broker-1"))
		resp := srv.preResolveAgentSkills(ctx, dispatchTestAgent(alice.ID, project.ID, uri))
		require.NotNil(t, resp)
		assert.Empty(t, resp.Errors)
		assert.Len(t, resp.Resolved, 1)
	})

	t.Run("user identity in context is used", func(t *testing.T) {
		bobIdent := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "api")
		ctx := contextWithIdentity(context.Background(), bobIdent)
		// Recorded creator is alice, but bob is the one dispatching.
		resp := srv.preResolveAgentSkills(ctx, dispatchTestAgent(alice.ID, project.ID, uri))
		require.NotNil(t, resp)
		assert.Empty(t, resp.Resolved)
		require.Len(t, resp.Errors, 1)
		assert.Equal(t, "forbidden", resp.Errors[0].Code)
	})

	t.Run("unknown creator skips pre-resolution", func(t *testing.T) {
		resp := srv.preResolveAgentSkills(context.Background(), dispatchTestAgent("no-such-principal", project.ID, uri))
		assert.Nil(t, resp, "without a principal the broker keeps its previous behaviour")
	})
}

// gh://, gcp-skill:// and federated registries stay with the broker's router.
func TestPreResolveAgentSkills_SkipsNonRegistrySchemes(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)
	agent := dispatchTestAgent(alice.ID, project.ID,
		"gh://owner/repo/skills/x@main",
		"gcp-skill://registry/x",
		"skill://other-registry/global/x",
	)
	assert.Nil(t, srv.preResolveAgentSkills(context.Background(), agent))
}

func TestPreResolveAgentSkills_NotFoundReported(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)
	uri := "skill://scion/global/does-not-exist"
	resp := srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(alice.ID, project.ID, uri))
	require.NotNil(t, resp)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, "not_found", resp.Errors[0].Code)
}

// Skills declared only in the Hub template's scion-agent.yaml (not in the
// agent's inline config) are discovered from storage and pre-resolved. This
// is the UAT reproduction path: a template referencing a private skill.
// createDispatchSkillTemplate stores a global Hub template whose
// scion-agent.yaml declares skillURI, and returns it.
func createDispatchSkillTemplate(t *testing.T, s store.Store, stor *contentMockStorage, slug, skillURI string) *store.Template {
	t.Helper()
	ctx := context.Background()
	yaml := []byte("harness: claude\nskills:\n  - uri: " + skillURI + "\n")
	tmpl := &store.Template{
		ID:          tid(slug),
		Name:        slug,
		Slug:        slug,
		Scope:       store.TemplateScopeGlobal,
		Harness:     "claude",
		Status:      store.TemplateStatusActive,
		StoragePath: "templates/global/" + slug,
		Files:       []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len(yaml))}},
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateTemplate(ctx, tmpl))
	_, err := stor.Upload(ctx, tmpl.StoragePath+"/scion-agent.yaml", bytes.NewReader(yaml), storage.UploadOptions{})
	require.NoError(t, err)
	return tmpl
}

// Skills declared in the Hub template's scion-agent.yaml are pre-resolved.
func TestPreResolveAgentSkills_TemplateConfigSkills(t *testing.T) {
	const skillURI = "skill://scion/global/tmpl-private@latest"
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	skill := createTestSkill(t, s, "tmpl-private", store.SkillScopeGlobal, "", alice.ID)
	publishTestSkillVersion(t, s, skill)
	tmpl := createDispatchSkillTemplate(t, s, stor, "dispatch-skill-tmpl", skillURI)

	agent := dispatchTestAgent(alice.ID, project.ID)
	agent.AppliedConfig.TemplateID = tmpl.ID

	resp := srv.preResolveAgentSkills(ctx, agent)
	require.NotNil(t, resp)
	assert.Empty(t, resp.Errors)
	require.Len(t, resp.Resolved, 1)
	assert.Equal(t, skillURI, resp.Resolved[0].URI)
}

// preResolvingDispatcher runs the Hub's pre-resolver on the agent exactly as
// HTTPAgentDispatcher.buildCreateRequest does, capturing the result.
type preResolvingDispatcher struct {
	createAgentDispatcher
	srv *Server
	pre *ResolveSkillsResponse
}

func (d *preResolvingDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) error {
	d.pre = d.srv.preResolveAgentSkills(ctx, agent)
	return d.createAgentDispatcher.DispatchAgentCreate(ctx, agent)
}

// End to end through the scheduler: a dispatch_agent event created by a
// regular member, naming a Hub template whose scion-agent.yaml requires a
// private skill, stamps the template ID (#1795) and pre-resolves that skill as
// the event's creator.
func TestSchedulerDispatch_PreResolvesTemplatePrivateSkill(t *testing.T) {
	const skillURI = "skill://scion/global/sched-private@latest"
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	skill := createTestSkill(t, s, "sched-private", store.SkillScopeGlobal, "", alice.ID)
	publishTestSkillVersion(t, s, skill)
	tmpl := createDispatchSkillTemplate(t, s, stor, "sched-skill-tmpl", skillURI)

	disp := &preResolvingDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}, srv: srv}
	srv.SetDispatcher(disp)

	payload, err := json.Marshal(DispatchAgentEventPayload{AgentName: "sched-skill-agent", Template: "sched-skill-tmpl"})
	require.NoError(t, err)
	require.NoError(t, srv.dispatchAgentEventHandler()(ctx, store.ScheduledEvent{
		ID:        tid("sched-skill-event"),
		ProjectID: project.ID,
		EventType: "dispatch_agent",
		Payload:   string(payload),
		CreatedBy: alice.ID,
	}))

	// #1795: the scheduled path stamps the resolved template's ID and hash,
	// as the agent-create path does, so the broker can hydrate it and the
	// Hub can read its skills.
	agent, err := s.GetAgentBySlug(ctx, project.ID, "sched-skill-agent")
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, agent.AppliedConfig.TemplateID)
	assert.Equal(t, tmpl.ContentHash, agent.AppliedConfig.TemplateHash)

	require.NotNil(t, disp.pre, "scheduled dispatch must pre-resolve the template's Hub skills")
	assert.Empty(t, disp.pre.Errors)
	require.Len(t, disp.pre.Resolved, 1)
	assert.Equal(t, skillURI, disp.pre.Resolved[0].URI)
}

// The dispatcher attaches the pre-resolved set to every create request, and
// it survives the JSON hop to the broker under "preResolvedSkills".
func TestBuildCreateRequest_AttachesPreResolvedSkills(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "wired-skill", store.SkillScopeGlobal, "", alice.ID)
	publishTestSkillVersion(t, s, skill)

	d := NewHTTPAgentDispatcherWithClient(s, nil, false, nil)
	d.SetSkillPreResolver(srv.preResolveAgentSkills)

	agent := dispatchTestAgent(alice.ID, project.ID, "skill://scion/global/wired-skill")
	req, err := d.buildCreateRequest(context.Background(), agent, "test")
	require.NoError(t, err)
	require.NotNil(t, req.PreResolvedSkills)
	require.Len(t, req.PreResolvedSkills.Resolved, 1)

	data, err := json.Marshal(req)
	require.NoError(t, err)
	var wire struct {
		PreResolvedSkills struct {
			Resolved []struct {
				URI string `json:"uri"`
			} `json:"resolved"`
		} `json:"preResolvedSkills"`
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Len(t, wire.PreResolvedSkills.Resolved, 1)
	assert.Equal(t, "skill://scion/global/wired-skill", wire.PreResolvedSkills.Resolved[0].URI)
}

func TestRewriteLocalDownloadURLsRelative(t *testing.T) {
	urls := rewriteLocalDownloadURLsRelative([]DownloadURLInfo{
		{Path: "SKILL.md", URL: "file:///var/storage/skills/x/1.0.0/SKILL.md"},
		{Path: "b.md", URL: "https://storage.example.com/signed"},
	}, "skills", "skill-id")
	assert.Equal(t, "/api/v1/skills/skill-id/files/SKILL.md?raw=1", urls[0].URL)
	assert.Equal(t, "https://storage.example.com/signed", urls[1].URL)
}
