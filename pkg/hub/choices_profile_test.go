//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const userMeDecisionsPath = "/api/v1/users/me/decisions"

func createTestDecisionTopic(t *testing.T, wcs WebChatStore, projectID, name, createdBy string) string {
	t.Helper()
	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(context.Background(), WebChatTopic{
		ID:        topicID,
		ProjectID: projectID,
		Name:      name,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
	}))
	return topicID
}

func recordTestInterpretedForUser(
	t *testing.T,
	st store.Store,
	projectID, topicID string,
	ag *store.Agent,
	user *store.User,
	rowID, choiceID string,
) {
	t.Helper()
	ctx := context.Background()
	srcID := api.NewUUID()
	require.NoError(t, st.CreateMessage(ctx, &store.Message{
		ID:        srcID,
		ProjectID: projectID,
		Sender:    "user:" + user.ID,
		SenderID:  user.ID,
		Recipient: "chat",
		ThreadID:  topicID,
		Type:      messages.TypeChat,
		Channel:   "web",
		Msg:       "reply",
		CreatedAt: time.Now().UTC(),
	}))
	_, err := st.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: ag.ID,
		SourceMessageID:      srcID,
		AnsweredByUserID:     user.ID,
		AnsweredByName:       user.DisplayName,
		AnsweredByEmail:      user.Email,
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{DecisionRowID: rowID, ChoiceID: choiceID}},
		ResponseMessage: &store.Message{
			ID:        api.NewUUID(),
			ProjectID: projectID,
			Sender:    "agent:" + ag.Slug,
			SenderID:  ag.ID,
			Recipient: "chat",
			ThreadID:  topicID,
			Channel:   "web",
			CreatedAt: time.Now().UTC(),
		},
		FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "recorded" },
	})
	require.NoError(t, err)
}

func decodeProfilePendingDecisions(t *testing.T, rr *httptest.ResponseRecorder) ProfilePendingDecisionsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw), "body: %s", rr.Body.String())
	assert.Contains(t, raw, "prompts")
	assert.NotContains(t, raw, "entries")
	assert.NotContains(t, raw, "groups")

	var out ProfilePendingDecisionsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
	return out
}

func decodeProfileMadeDecisions(t *testing.T, rr *httptest.ResponseRecorder) ProfileMadeDecisionsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw), "body: %s", rr.Body.String())
	assert.Contains(t, raw, "entries")
	assert.NotContains(t, raw, "groups")
	assert.NotContains(t, raw, "prompts")

	var out ProfileMadeDecisionsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
	return out
}

func decodeProfileMadeByProjectDecisions(t *testing.T, rr *httptest.ResponseRecorder) ProfileMadeByProjectDecisionsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw), "body: %s", rr.Body.String())
	assert.Contains(t, raw, "groups")
	assert.NotContains(t, raw, "entries")
	assert.NotContains(t, raw, "nextCursor")
	assert.NotContains(t, raw, "prompts")

	var out ProfileMadeByProjectDecisionsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
	return out
}

func profileProjectSlugs(refs []ProjectDecisionProjectRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Slug)
	}
	return out
}

func TestUserMeDecisions_ExperimentMatrixAndMethods(t *testing.T) {
	f := newChoicesFixture(t)

	// web.decisions off -> GET 404, non-GET 405 before gate, nested suffix 404.
	setChoicesExperiment(t, f.srv, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, userMeDecisionsPath, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"/extra", nil).Code)

	// web.decisions on -> GET 200, unsupported methods 405, nested suffix 404.
	setChoicesExperiment(t, f.srv, true)
	resp := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, ProjectDecisionCounts{}, resp.Counts)
	assert.NotNil(t, resp.Prompts)
	assert.Empty(t, resp.Prompts)
	assert.NotNil(t, resp.Projects)
	assert.Empty(t, resp.Projects)
	assert.False(t, resp.TruncatedProjects)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, userMeDecisionsPath, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, userMeDecisionsPath, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"/extra", nil).Code)
}

// TestUserMeDecisions_TokenAndCallerAuthz pins chat.userdecisions.list across
// session credentials, project-bounded and hub-bounded UATs (with and without
// project:read and inbox:read), unauthenticated callers, agents, visitors, and
// federated users.
func TestUserMeDecisions_TokenAndCallerAuthz(t *testing.T) {
	f := newChatFixture(t)
	setChoicesExperiment(t, f.srv, true)
	ctx := context.Background()

	// Make f.member also a member of f.projB so the profile roll-up covers two
	// member projects for f.member.
	bindProjectMember(t, f.s, f.projB, f.member)

	for _, ag := range []*store.Agent{f.agentA, f.agentB} {
		ag.OwnerID = f.member
		ag.Ancestry = []string{f.member}
		ag.MessageMode = store.MessageModeProject
		require.NoError(t, f.s.UpdateAgent(ctx, ag))
	}

	seedPrompt := func(projID, chatKey, recipient, key string, agent *store.Agent, at time.Time) {
		decs, nerr := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"`+key+`","question":"`+key+`?","choices":["yes","no"]}]`))
		require.NoError(t, nerr)
		_, cerr := f.s.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: projID, Sender: "agent:" + agent.Slug, SenderID: agent.ID,
				Recipient: recipient, ThreadID: chatKey, Channel: "web", CreatedAt: at,
			},
			Intro: key, AskerAgentID: agent.ID, AskerAgentName: agent.Slug, Decisions: decs,
		})
		require.NoError(t, cerr)
	}

	now := time.Now().UTC()
	dmKeyA := f.dm(t, f.member, "agent", f.agentA.ID)
	seedPrompt(f.projA, f.topicA, "chat", "projA-topic", f.agentA, now.Add(-3*time.Minute))
	seedPrompt(f.projA, dmKeyA, "user:"+f.member, "projA-dm", f.agentA, now.Add(-2*time.Minute))
	seedPrompt(f.projB, f.topicB, "chat", "projB-topic", f.agentB, now.Add(-time.Minute))

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)

	// 1. Unauthenticated -> 401.
	unauthReq := httptest.NewRequest(http.MethodGet, userMeDecisionsPath, nil)
	unauthRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(unauthRec, unauthReq)
	assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

	// 2. Agent caller -> 403.
	agentReq := httptest.NewRequest(http.MethodGet, userMeDecisionsPath, nil)
	agentReq = agentReq.WithContext(agentCtx(agentReq.Context(), f.agentA))
	agentRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentRec, agentReq)
	assert.Equal(t, http.StatusForbidden, agentRec.Code)

	// 3. Visitor -> 403.
	visitor := NewVisitorIdentity(tid("user-dec-visitor"), "https://issuer.test", "sub-v", "v@test.com", "Visitor")
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, visitor, http.MethodGet, userMeDecisionsPath, nil).Code)

	// 4. Federated user -> 403.
	fed := NewFederatedUserIdentity("https://issuer.test", "sub-fed", memberUser.Email, "Fed Member", "member", nil)
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, fed, http.MethodGet, userMeDecisionsPath, nil).Code)

	// 5. Session user -> 200, sees projA topic + projA DM + projB topic, canAnswer=true.
	sessResp := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, 3, sessResp.Counts.WaitingOnYou)
	require.Len(t, sessResp.Prompts, 3)
	assert.True(t, sessResp.Prompts[0].CanAnswer)
	require.Len(t, sessResp.Projects, 2)

	// 6. Project-bounded UAT (projA, project:read only) -> sees ONLY projA topic (not projB, not DM), canAnswer=false.
	tokProjReadOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	uatProjResp := decodeProfilePendingDecisions(t, f.call(t, tokProjReadOnly, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, 1, uatProjResp.Counts.WaitingOnYou)
	require.Len(t, uatProjResp.Prompts, 1)
	assert.Equal(t, "projA-topic", uatProjResp.Prompts[0].Decisions[0].Key)
	assert.False(t, uatProjResp.Prompts[0].CanAnswer)
	require.Len(t, uatProjResp.Projects, 1)
	assert.Equal(t, f.projA, uatProjResp.Projects[0].ID)

	// 7. Project-bounded UAT (projA, project:read + inbox:read + agent:read) -> sees projA topic + projA DM, NOT projB.
	tokProjWithInbox := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "inbox:read", "agent:read")
	uatProjDMResp := decodeProfilePendingDecisions(t, f.call(t, tokProjWithInbox, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, 2, uatProjDMResp.Counts.WaitingOnYou)
	assert.ElementsMatch(t, []string{"projA-topic", "projA-dm"}, promptKeys(uatProjDMResp.Prompts))
	require.Len(t, uatProjDMResp.Projects, 1)
	assert.Equal(t, f.projA, uatProjDMResp.Projects[0].ID)

	// 8. Hub-bounded UAT: without inbox:read sees topics across projA & projB; with inbox:read + agent:read sees all 3.
	tokHubReadOnly := f.mint(t, f.member, hubBoundary(), "project:read")
	uatHubResp := decodeProfilePendingDecisions(t, f.call(t, tokHubReadOnly, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, 2, uatHubResp.Counts.WaitingOnYou)
	assert.ElementsMatch(t, []string{"projA-topic", "projB-topic"}, promptKeys(uatHubResp.Prompts))
	require.Len(t, uatHubResp.Projects, 2)

	tokHubWithInbox := f.mint(t, f.member, hubBoundary(), "project:read", "inbox:read", "agent:read")
	uatHubDMResp := decodeProfilePendingDecisions(t, f.call(t, tokHubWithInbox, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, 3, uatHubDMResp.Counts.WaitingOnYou)
	require.Len(t, uatHubDMResp.Prompts, 3)

	// 9. UAT missing project:read selector -> 200 with empty list and zero counts.
	tokNoProjRead := f.mint(t, f.member, hubBoundary(), "inbox:read", "agent:read")
	uatEmptyResp := decodeProfilePendingDecisions(t, f.call(t, tokNoProjRead, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, ProjectDecisionCounts{}, uatEmptyResp.Counts)
	assert.Empty(t, uatEmptyResp.Prompts)
	assert.Empty(t, uatEmptyResp.Projects)
}

// TestUserMeDecisions_DMPrivacyAndMembershipScope verifies R2 and AC 8:
//   - Direct and group-based project members see their member projects.
//   - Non-member projects are excluded even for a Hub super-admin.
//   - A project where the caller holds only project-visitor is excluded.
//   - Another user's private DM decisions never leak in prompts, entries,
//     groups, counts, or any facet (including projects when a project's ONLY
//     decision is another user's private DM).
func TestUserMeDecisions_DMPrivacyAndMembershipScope(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, true)
	ctx := context.Background()

	// Create a Hub super-admin who is NOT a member of f.project.
	superID := tid("profile-superadmin")
	createTestUserWithRole(t, f.store, superID, "profile-super@example.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, f.store, superID)
	superUser, err := f.store.GetUser(ctx, superID)
	require.NoError(t, err)

	// Project 1 (f.project): Alice (owner) and Bob (member).
	// Seed a public topic decision and Bob's private DM decision.
	_, pub1 := f.topicPrompt(t, `[{"id":"p1-pub","question":"P1 public?","choices":["yes","no"]}]`, false)
	require.Len(t, pub1.Decisions, 1)

	dmOnlyAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "p1-dm-bot", Name: "P1 DM Bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, dmOnlyAgent))
	dmBobKey, err := messages.DMConversationKey("agent", dmOnlyAgent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	decsDM, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"p1-bob-dm","question":"Bob secret?","choices":["ok","no"]}]`))
	require.NoError(t, err)
	dmRes, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + dmOnlyAgent.Slug, SenderID: dmOnlyAgent.ID,
			Recipient: "user:" + f.bob.ID, ThreadID: dmBobKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "Private", AskerAgentID: dmOnlyAgent.ID, AskerAgentName: dmOnlyAgent.Name, Decisions: decsDM,
	})
	require.NoError(t, err)

	// Project 2 (projGroup): Alice is a member ONLY via an explicit group!
	projGroup := &store.Project{ID: api.NewUUID(), Name: "Group Project", Slug: "group-proj", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projGroup))
	topicGroupID := createTestDecisionTopic(t, f.wcs, projGroup.ID, "group-topic", f.alice.ID)
	grpID := api.NewUUID()
	require.NoError(t, f.store.CreateGroup(ctx, &store.Group{
		ID: grpID, Name: "Devs", Slug: "devs-" + grpID[:8], GroupType: store.GroupTypeExplicit, Created: time.Now(), Updated: time.Now(),
	}))
	require.NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{
		GroupID: grpID, MemberType: store.GroupMemberTypeUser, MemberID: f.alice.ID, Role: store.GroupMemberRoleMember, AddedAt: time.Now(),
	}))
	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: grpID,
		ScopeType: store.RoleScopeProject, ScopeID: projGroup.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	groupAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "group-bot", Name: "Group Bot",
		ProjectID: projGroup.ID, OwnerID: f.alice.ID, CreatedBy: f.alice.ID, Ancestry: []string{f.alice.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, groupAgent))
	decsGrp, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"p2-grp","question":"Group release?","choices":["go","hold"]}]`))
	require.NoError(t, err)
	_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projGroup.ID, Sender: "agent:" + groupAgent.Slug, SenderID: groupAgent.ID,
			Recipient: "chat", ThreadID: topicGroupID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "Group", AskerAgentID: groupAgent.ID, AskerAgentName: groupAgent.Name, Decisions: decsGrp,
	})
	require.NoError(t, err)

	// Project 3 (projVisitorOnly): a federated account holds ONLY project-visitor
	// via CreateVisitorRoleBinding -> must be excluded from member scopes!
	projVisitorOnly := &store.Project{ID: api.NewUUID(), Name: "Visitor Only", Slug: "visitor-only", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projVisitorOnly))
	topicVisID := createTestDecisionTopic(t, f.wcs, projVisitorOnly.ID, "vis-topic", f.bob.ID)
	visRole, err := f.store.GetRoleDefinitionByName(ctx, "project-visitor", store.RoleScopeProject)
	require.NoError(t, err)
	visIssuer := "https://visitor.example.com"
	visEmail := "vis-only@visitor.example.com"
	fedVisUser := &store.User{
		DisplayName:    "Visitor Account",
		Kind:           store.UserKindFederated,
		Status:         store.UserStatusActive,
		HomeIssuer:     &visIssuer,
		FederatedEmail: &visEmail,
	}
	require.NoError(t, f.store.CreateFederatedUser(ctx, fedVisUser))
	_, err = f.store.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: visRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: fedVisUser.ID,
		ScopeType: store.RoleScopeProject, ScopeID: projVisitorOnly.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	visBotID := api.NewUUID()
	decsVis, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"p3-vis","question":"Visitor see?","choices":["y","n"]}]`))
	require.NoError(t, err)
	_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projVisitorOnly.ID, Sender: "agent:vis-bot", SenderID: visBotID,
			Recipient: "chat", ThreadID: topicVisID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "Visitor", AskerAgentID: visBotID, AskerAgentName: "vis-bot", Decisions: decsVis,
	})
	require.NoError(t, err)

	// Project 4 (projDMOnly): Alice IS a member, but its ONLY decision is Bob's
	// private DM -> must NOT leak even in Alice's projects facet!
	projDMOnly := &store.Project{ID: api.NewUUID(), Name: "DM Only Project", Slug: "dm-only-proj", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projDMOnly))
	bindProjectMember(t, f.store, projDMOnly.ID, f.alice.ID)
	bindProjectMember(t, f.store, projDMOnly.ID, f.bob.ID)
	dm4Bot := &store.Agent{
		ID: api.NewUUID(), Slug: "dm4-bot", Name: "DM4 Bot",
		ProjectID: projDMOnly.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, dm4Bot))
	dm4Key, err := messages.DMConversationKey("agent", dm4Bot.ID, "user", f.bob.ID)
	require.NoError(t, err)
	decsDM4, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"p4-bob-dm","question":"P4 secret?","choices":["y","n"]}]`))
	require.NoError(t, err)
	_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projDMOnly.ID, Sender: "agent:" + dm4Bot.Slug, SenderID: dm4Bot.ID,
			Recipient: "user:" + f.bob.ID, ThreadID: dm4Key, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "DM4", AskerAgentID: dm4Bot.ID, AskerAgentName: dm4Bot.Name, Decisions: decsDM4,
	})
	require.NoError(t, err)

	// 1. Hub super-admin is not a member of ANY of these projects -> sees nothing (AC 8).
	superPend := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, superUser, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, ProjectDecisionCounts{}, superPend.Counts)
	assert.Empty(t, superPend.Prompts)
	assert.Empty(t, superPend.Projects)

	// Even if super-admin passes ?project=<f.project.ID>, still empty 200 (not an oracle, not leaked).
	superNarrow := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, superUser, http.MethodGet, userMeDecisionsPath+"?project="+f.project.ID, nil))
	assert.Empty(t, superNarrow.Prompts)
	assert.Empty(t, superNarrow.Projects)

	// 1b. Visitor-role account is refused at HTTP (403) AND excluded by
	// chatMemberProjectIDs in profileDecisionScopes / waitingOnYouTotal (design R2).
	visIdent := NewVisitorIdentity(fedVisUser.ID, visIssuer, "sub-vis", visEmail, fedVisUser.DisplayName)
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, visIdent, http.MethodGet, userMeDecisionsPath, nil).Code)
	visScopes, visTrunc, err := f.srv.profileDecisionScopes(ctx, NewAuthenticatedUser(fedVisUser.ID, visEmail, fedVisUser.DisplayName, "", ""), nil, f.wcs, "")
	require.NoError(t, err)
	assert.Empty(t, visScopes)
	assert.False(t, visTrunc)
	visTot, visProjs, err := f.srv.waitingOnYouTotal(ctx, fedVisUser.ID)
	require.NoError(t, err)
	assert.Zero(t, visTot)
	assert.Zero(t, visProjs)

	// 2. Alice sees p1-pub (direct member) and p2-grp (group member), NOT p1-bob-dm, NOT p3-vis, NOT dm-only-proj!
	alicePend := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath, nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 2}, alicePend.Counts)
	assert.ElementsMatch(t, []string{"p1-pub", "p2-grp"}, promptKeys(alicePend.Prompts))
	assert.ElementsMatch(t, []string{f.project.Slug, projGroup.Slug}, profileProjectSlugs(alicePend.Projects))
	for _, ag := range alicePend.Agents {
		assert.NotEqual(t, dmOnlyAgent.ID, ag.ID)
		assert.NotEqual(t, dm4Bot.ID, ag.ID)
	}

	// 3. Bob answers his private DM in P1 -> verify tab=made (both sorts) never leaks to Alice.
	require.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodPost,
		"/api/v1/decisions/"+dmRes.Decisions[0].RowID+"/answer", map[string]interface{}{"choice_id": "1"}).Code)

	aliceMadeRecent := decodeProfileMadeDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=recent", nil))
	assert.Empty(t, aliceMadeRecent.Entries)
	for _, ans := range aliceMadeRecent.Answerers {
		assert.NotEqual(t, f.bob.ID, ans.ID)
	}

	aliceMadeProj := decodeProfileMadeByProjectDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=project", nil))
	assert.Empty(t, aliceMadeProj.Groups)

	bobMadeProj := decodeProfileMadeByProjectDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=project", nil))
	require.Len(t, bobMadeProj.Groups, 1)
	assert.Equal(t, "p1-bob-dm", bobMadeProj.Groups[0].Entries[0].Decision.Key)
}

func TestUserMeDecisions_PendingSortsPaginationAndProjectFilter(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, true)
	ctx := context.Background()

	// Rename f.project to Alpha and create Beta (Bob is a member of both) and
	// Gamma (Bob is NOT a member).
	f.project.Name = "Alpha"
	f.project.Slug = "proj-alpha"
	require.NoError(t, f.store.UpdateProject(ctx, f.project))

	projBeta := &store.Project{ID: api.NewUUID(), Name: "Beta", Slug: "proj-beta", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projBeta))
	bindProjectMember(t, f.store, projBeta.ID, f.bob.ID)
	bindProjectMember(t, f.store, projBeta.ID, f.alice.ID)
	topicBetaID := createTestDecisionTopic(t, f.wcs, projBeta.ID, "beta-general", f.bob.ID)
	agentBeta := &store.Agent{
		ID: api.NewUUID(), Slug: "beta-bot", Name: "Beta Bot",
		ProjectID: projBeta.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, agentBeta))

	projGamma := &store.Project{ID: api.NewUUID(), Name: "Gamma", Slug: "proj-gamma", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projGamma))

	base := time.Now().UTC().Truncate(time.Millisecond)
	seed := func(projID, topicID string, ag *store.Agent, key string, at time.Time) *store.ChoicePromptResult {
		decs, nerr := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"`+key+`","question":"Question `+key+`?","choices":["yes","no"]}]`))
		require.NoError(t, nerr)
		res, cerr := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: projID, Sender: "agent:" + ag.Slug, SenderID: ag.ID,
				Recipient: "chat", ThreadID: topicID, Channel: "web", CreatedAt: at,
			},
			Intro: key, AskerAgentID: ag.ID, AskerAgentName: ag.Name, Decisions: decs,
		})
		require.NoError(t, cerr)
		return res
	}

	// WaitingOnYou prompts:
	// - alpha-old at t-3m (Alpha)
	// - beta-mid at t-2m (Beta)
	// - alpha-new at t-1m (Alpha)
	// WaitingOnOthers prompt (section 1 for Bob):
	// - beta-other at t-10s (interpreted answer recorded for Alice -> Bob sees waitingOnOthers)
	seed(f.project.ID, f.topicID, f.agent, "alpha-old", base.Add(-3*time.Minute))
	seed(projBeta.ID, topicBetaID, agentBeta, "beta-mid", base.Add(-2*time.Minute))
	seed(f.project.ID, f.topicID, f.agent, "alpha-new", base.Add(-1*time.Minute))
	otherRes := seed(projBeta.ID, topicBetaID, agentBeta, "beta-other", base.Add(-10*time.Second))
	recordTestInterpretedForUser(t, f.store, projBeta.ID, topicBetaID, agentBeta, f.alice, otherRes.Decisions[0].RowID, "1")

	// 1. Default sort on pending is "project" (R6):
	// Section 0: Alpha's newest waitingOnYou is t-1m > Beta's t-2m -> [alpha-new, alpha-old, beta-mid]
	// Section 1: [beta-other]
	defRaw := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath, nil)
	defResp := decodeProfilePendingDecisions(t, defRaw)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 3, WaitingOnOthers: 1}, defResp.Counts)
	assert.Nil(t, defResp.FilteredCounts)
	assert.Equal(t, []string{"alpha-new", "alpha-old", "beta-mid", "beta-other"}, promptKeys(defResp.Prompts))
	assert.Equal(t, []string{"proj-alpha", "proj-beta"}, profileProjectSlugs(defResp.Projects))

	// Agents facet entries carry their projectId (design D5); answerers omit projectId.
	agentProjByID := make(map[string]string, len(defResp.Agents))
	for _, ag := range defResp.Agents {
		agentProjByID[ag.ID] = ag.ProjectID
	}
	assert.Equal(t, map[string]string{
		f.agent.ID:   f.project.ID,
		agentBeta.ID: projBeta.ID,
	}, agentProjByID)
	require.Len(t, defResp.Answerers, 1)
	assert.Equal(t, f.alice.ID, defResp.Answerers[0].ID)
	assert.Empty(t, defResp.Answerers[0].ProjectID)
	var rawFacets struct {
		Answerers []map[string]any `json:"answerers"`
	}
	require.NoError(t, json.Unmarshal(defRaw.Body.Bytes(), &rawFacets))
	require.Len(t, rawFacets.Answerers, 1)
	assert.NotContains(t, rawFacets.Answerers[0], "projectId", "answerers must omit projectId")

	// 2. Explicit sort=recent:
	// Section 0 chronological: [alpha-new, beta-mid, alpha-old], then Section 1: [beta-other].
	recentResp := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?sort=recent", nil))
	assert.Equal(t, []string{"alpha-new", "beta-mid", "alpha-old", "beta-other"}, promptKeys(recentResp.Prompts))

	// 3. Keyset pagination with limit=1 across all 4 items in BOTH sorts!
	collectPages := func(sortMode string) []string {
		var got []string
		cursor := ""
		for step := 0; step < 10; step++ {
			u := userMeDecisionsPath + "?tab=pending&limit=1&sort=" + sortMode
			if cursor != "" {
				u += "&cursor=" + url.QueryEscape(cursor)
			}
			page := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, u, nil))
			for _, p := range page.Prompts {
				got = append(got, p.Decisions[0].Key)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		return got
	}
	assert.Equal(t, []string{"alpha-new", "alpha-old", "beta-mid", "beta-other"}, collectPages(DecisionsSortProject))
	assert.Equal(t, []string{"alpha-new", "beta-mid", "alpha-old", "beta-other"}, collectPages(DecisionsSortRecent))

	// 4. project=<projBeta.ID> narrows prompts and sets filteredCounts, while unfiltered counts & facets stay whole-scope.
	narrowBeta := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?project="+projBeta.ID, nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 3, WaitingOnOthers: 1}, narrowBeta.Counts)
	require.NotNil(t, narrowBeta.FilteredCounts)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1, WaitingOnOthers: 1}, *narrowBeta.FilteredCounts)
	assert.Equal(t, []string{"beta-mid", "beta-other"}, promptKeys(narrowBeta.Prompts))
	assert.Equal(t, []string{"proj-alpha", "proj-beta"}, profileProjectSlugs(narrowBeta.Projects))

	// 5. Non-member project or unknown project ID -> empty 200 (never 403/404).
	for _, pid := range []string{projGamma.ID, uuid.NewString()} {
		nonMem := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?project="+pid, nil))
		assert.Empty(t, nonMem.Prompts)
		require.NotNil(t, nonMem.FilteredCounts)
		assert.Equal(t, ProjectDecisionCounts{}, *nonMem.FilteredCounts)
		assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 3, WaitingOnOthers: 1}, nonMem.Counts)
	}

	// 6. Invalid sort and cross-sort / cross-tab / cross-filter / cross-endpoint cursor rejections (400).
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?sort=alphabetical", nil).Code)

	p1ByProj := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?limit=1&sort=project", nil))
	require.NotEmpty(t, p1ByProj.NextCursor)

	// Cross-sort cursor -> 400.
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?limit=1&sort=recent&cursor="+url.QueryEscape(p1ByProj.NextCursor), nil).Code)
	// Cross-tab cursor -> 400.
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?tab=made&sort=recent&cursor="+url.QueryEscape(p1ByProj.NextCursor), nil).Code)
	// Cross-filter cursor (adding project= or q=) -> 400.
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?limit=1&sort=project&project="+projBeta.ID+"&cursor="+url.QueryEscape(p1ByProj.NextCursor), nil).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?limit=1&sort=project&q=alpha&cursor="+url.QueryEscape(p1ByProj.NextCursor), nil).Code)
	// Different viewer using Bob's cursor -> 400.
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.alice, http.MethodGet,
		userMeDecisionsPath+"?limit=1&sort=project&cursor="+url.QueryEscape(p1ByProj.NextCursor), nil).Code)

	// Single-project endpoint also populates projectId on agents and rejects profile cursor mixing (400).
	projEndpointPage := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		"/api/v1/projects/"+f.project.ID+"/decisions?limit=1", nil))
	require.Len(t, projEndpointPage.Agents, 1)
	assert.Equal(t, f.project.ID, projEndpointPage.Agents[0].ProjectID)
	require.NotEmpty(t, projEndpointPage.NextCursor)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?limit=1&sort=recent&cursor="+url.QueryEscape(projEndpointPage.NextCursor), nil).Code)
}

func TestUserMeDecisions_MadeSortsOverviewAndSearch(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, true)
	ctx := context.Background()

	f.project.Name = "Alpha"
	f.project.Slug = "proj-alpha"
	require.NoError(t, f.store.UpdateProject(ctx, f.project))

	projBeta := &store.Project{ID: api.NewUUID(), Name: "Beta", Slug: "proj-beta", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projBeta))
	bindProjectMember(t, f.store, projBeta.ID, f.bob.ID)
	topicBetaID := createTestDecisionTopic(t, f.wcs, projBeta.ID, "beta-ops", f.bob.ID)
	agentBeta := &store.Agent{
		ID: api.NewUUID(), Slug: "beta-bot", Name: "Beta Bot",
		ProjectID: projBeta.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, agentBeta))

	seedAndAnswer := func(projID, topicID string, ag *store.Agent, key, label string) {
		decs, nerr := messages.NormalizeDecisions("choices", decisionInputs(t,
			fmt.Sprintf(`[{"id":%q,"question":"Choose for %s?","choices":[{"id":"c1","label":%q},{"id":"c2","label":"skip"}]}]`, key, key, label)))
		require.NoError(t, nerr)
		res, cerr := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: projID, Sender: "agent:" + ag.Slug, SenderID: ag.ID,
				Recipient: "chat", ThreadID: topicID, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			Intro: key, AskerAgentID: ag.ID, AskerAgentName: ag.Name, Decisions: decs,
		})
		require.NoError(t, cerr)
		require.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodPost,
			"/api/v1/decisions/"+res.Decisions[0].RowID+"/answer", map[string]interface{}{"choice_id": "c1"}).Code)
		time.Sleep(2 * time.Millisecond)
	}

	// Seed 6 answers in Alpha (so overview shows 5 + more=true) and 2 answers in Beta (more=false, newest overall).
	for i := 1; i <= 6; i++ {
		seedAndAnswer(f.project.ID, f.topicID, f.agent, fmt.Sprintf("alpha-%d", i), fmt.Sprintf("alpha-ans-%d", i))
	}
	seedAndAnswer(projBeta.ID, topicBetaID, agentBeta, "beta-1", "canary-rollout")
	seedAndAnswer(projBeta.ID, topicBetaID, agentBeta, "beta-2", "full-cutover")

	// 1. Default sort on tab=made is "recent" (R6): chronological across both projects, cursor-paged.
	var pagedKeys []string
	cursor := ""
	for step := 0; step < 10; step++ {
		u := userMeDecisionsPath + "?tab=made&limit=3"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		page := decodeProfileMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, u, nil))
		for _, e := range page.Entries {
			pagedKeys = append(pagedKeys, e.Decision.Key)
		}
		if page.NextCursor == "" {
			break
		}
		// Cross-filter cursor check on Made: adding q= with an existing cursor returns 400.
		if step == 0 {
			assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
				userMeDecisionsPath+"?tab=made&limit=3&q=canary&cursor="+url.QueryEscape(page.NextCursor), nil).Code)
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, []string{"beta-2", "beta-1", "alpha-6", "alpha-5", "alpha-4", "alpha-3", "alpha-2", "alpha-1"}, pagedKeys)

	// 2. tab=made&sort=project overview:
	// Beta's latest answer (beta-2) is newer than Alpha's (alpha-6), so Beta group is first (2 entries, more=false),
	// followed by Alpha group (5 newest entries alpha-6..alpha-2, more=true).
	overview := decodeProfileMadeByProjectDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=project", nil))
	require.Len(t, overview.Groups, 2)

	assert.Equal(t, projBeta.ID, overview.Groups[0].Project.ID)
	assert.False(t, overview.Groups[0].More)
	assert.Equal(t, []string{"beta-2", "beta-1"}, entryKeys(overview.Groups[0].Entries))

	assert.Equal(t, f.project.ID, overview.Groups[1].Project.ID)
	assert.True(t, overview.Groups[1].More)
	assert.Equal(t, []string{"alpha-6", "alpha-5", "alpha-4", "alpha-3", "alpha-2"}, entryKeys(overview.Groups[1].Entries))

	// 3. Passing cursor with tab=made&sort=project returns 400 (R5).
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?tab=made&sort=project&cursor=anything", nil).Code)

	// 4. Search q and project filter work on tab=made in both sorts.
	qRecent := decodeProfileMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?tab=made&q=canary-rollout", nil))
	assert.Equal(t, []string{"beta-1"}, entryKeys(qRecent.Entries))

	qOverview := decodeProfileMadeByProjectDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?tab=made&sort=project&project="+f.project.ID+"&q=alpha-ans-1", nil))
	require.Len(t, qOverview.Groups, 1)
	assert.False(t, qOverview.Groups[0].More)
	assert.Equal(t, []string{"alpha-1"}, entryKeys(qOverview.Groups[0].Entries))
}

func TestUserMeDecisions_ProjectSortPagingStability(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)

	seedPrompt := func(t *testing.T, f *choicesFixture, p *store.Project, topicID, key string, offsetSec int) string {
		t.Helper()
		botID := api.NewUUID()
		decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"`+key+`","question":"`+key+`?","choices":["yes","no"]}]`))
		require.NoError(t, err)
		res, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: p.ID, Sender: "agent:bot", SenderID: botID,
				Recipient: "chat", ThreadID: topicID, Channel: "web",
				CreatedAt: base.Add(time.Duration(offsetSec) * time.Second),
			},
			Intro: key, AskerAgentID: botID, AskerAgentName: "bot-" + key, Decisions: decs,
		})
		require.NoError(t, err)
		return res.Decisions[0].RowID
	}

	promptKeys := func(prompts []ProjectDecisionPromptView) []string {
		out := make([]string, len(prompts))
		for i, p := range prompts {
			out[i] = p.Decisions[0].Key
		}
		return out
	}

	t.Run("new prompt in later group does not skip older items", func(t *testing.T) {
		f := newChoicesFixture(t)
		setChoicesExperiment(t, f.srv, true)

		projB := &store.Project{ID: api.NewUUID(), Name: "Beta", Slug: "beta", Created: base, Updated: base}
		require.NoError(t, f.store.CreateProject(ctx, projB))
		bindProjectMember(t, f.store, projB.ID, f.bob.ID)
		tpB := createTestDecisionTopic(t, f.wcs, projB.ID, "general", f.bob.ID)

		// Seed in chronological order (decisions.created_at uses time.Now()):
		// Group B (projB): B4, B5. Group A (f.project): A9, A10.
		seedPrompt(t, f, projB, tpB, "B4", 4)
		seedPrompt(t, f, projB, tpB, "B5", 5)
		seedPrompt(t, f, f.project, f.topicID, "A9", 9)
		seedPrompt(t, f, f.project, f.topicID, "A10", 10)

		p1 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=2", nil))
		assert.Equal(t, []string{"A10", "A9"}, promptKeys(p1.Prompts))
		require.NotEmpty(t, p1.NextCursor)

		// B11 arrives in group B between Page 1 and Page 2, raising B's groupAt above 10.
		seedPrompt(t, f, projB, tpB, "B11", 11)

		p2 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=2&cursor="+url.QueryEscape(p1.NextCursor), nil))
		assert.Equal(t, []string{"B11", "B5"}, promptKeys(p2.Prompts))
		require.NotEmpty(t, p2.NextCursor)

		p3 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=2&cursor="+url.QueryEscape(p2.NextCursor), nil))
		assert.Equal(t, []string{"B4"}, promptKeys(p3.Prompts))
		assert.Empty(t, p3.NextCursor)
	})

	t.Run("inline answer mid-group finishes active group before later groups", func(t *testing.T) {
		f := newChoicesFixture(t)
		setChoicesExperiment(t, f.srv, true)

		projB := &store.Project{ID: api.NewUUID(), Name: "Beta", Slug: "beta", Created: base, Updated: base}
		require.NoError(t, f.store.CreateProject(ctx, projB))
		bindProjectMember(t, f.store, projB.ID, f.bob.ID)
		tpB := createTestDecisionTopic(t, f.wcs, projB.ID, "general", f.bob.ID)

		seedPrompt(t, f, f.project, f.topicID, "A3", 3)
		seedPrompt(t, f, projB, tpB, "B7", 7)
		seedPrompt(t, f, projB, tpB, "B8", 8)
		rowA9 := seedPrompt(t, f, f.project, f.topicID, "A9", 9)
		rowA10 := seedPrompt(t, f, f.project, f.topicID, "A10", 10)

		p1 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=2", nil))
		assert.Equal(t, []string{"A10", "A9"}, promptKeys(p1.Prompts))
		require.NotEmpty(t, p1.NextCursor)

		// Inline answer settles A10 and A9 so A's live groupAt drops to 3 (< B's 8).
		for _, rid := range []string{rowA10, rowA9} {
			_, _, err := f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
				DecisionRowID: rid, ChoiceID: "1", AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName,
				Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceDecisionsPage,
				ResponseMessage: &store.Message{
					ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "user:" + f.bob.ID, SenderID: f.bob.ID,
					Recipient: "chat", ThreadID: f.topicID, Channel: "web", CreatedAt: base.Add(20 * time.Second),
				},
			})
			require.NoError(t, err)
		}

		p2 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=2&cursor="+url.QueryEscape(p1.NextCursor), nil))
		assert.Equal(t, []string{"A3", "B8"}, promptKeys(p2.Prompts))
		require.NotEmpty(t, p2.NextCursor)

		p3 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=2&cursor="+url.QueryEscape(p2.NextCursor), nil))
		assert.Equal(t, []string{"B7"}, promptKeys(p3.Prompts))
		assert.Empty(t, p3.NextCursor)
	})

	t.Run("inline answer after crossing group does not repeat finished group", func(t *testing.T) {
		f := newChoicesFixture(t)
		setChoicesExperiment(t, f.srv, true)

		projB := &store.Project{ID: api.NewUUID(), Name: "Beta", Slug: "beta", Created: base, Updated: base}
		require.NoError(t, f.store.CreateProject(ctx, projB))
		bindProjectMember(t, f.store, projB.ID, f.bob.ID)
		tpB := createTestDecisionTopic(t, f.wcs, projB.ID, "general", f.bob.ID)

		seedPrompt(t, f, f.project, f.topicID, "A4", 4)
		seedPrompt(t, f, projB, tpB, "B7", 7)
		seedPrompt(t, f, projB, tpB, "B8", 8)
		rowA10 := seedPrompt(t, f, f.project, f.topicID, "A10", 10)

		p1 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=3", nil))
		assert.Equal(t, []string{"A10", "A4", "B8"}, promptKeys(p1.Prompts))
		require.NotEmpty(t, p1.NextCursor)

		// A10 is answered inline; A4 remains open with groupAt=4 (< B's 8),
		// but group A was already finished on Page 1 so A4 must not repeat.
		_, _, err := f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: rowA10, ChoiceID: "1", AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName,
			Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceDecisionsPage,
			ResponseMessage: &store.Message{
				ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "user:" + f.bob.ID, SenderID: f.bob.ID,
				Recipient: "chat", ThreadID: f.topicID, Channel: "web", CreatedAt: base.Add(20 * time.Second),
			},
		})
		require.NoError(t, err)

		p2 := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=3&cursor="+url.QueryEscape(p1.NextCursor), nil))
		assert.Equal(t, []string{"B7"}, promptKeys(p2.Prompts))
		assert.Empty(t, p2.NextCursor)

		// Malformed DoneProjects (containing cur.ProjectID) is rejected with 400.
		rawCur, err := base64.RawURLEncoding.DecodeString(p1.NextCursor)
		require.NoError(t, err)
		var env profileDecisionCursor
		require.NoError(t, json.Unmarshal(rawCur, &env))
		env.Pending.DoneProjects = append(env.Pending.DoneProjects, env.Pending.ProjectID)
		badBytes, err := json.Marshal(env)
		require.NoError(t, err)
		badTok := base64.RawURLEncoding.EncodeToString(badBytes)
		assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			userMeDecisionsPath+"?limit=3&cursor="+url.QueryEscape(badTok), nil).Code)
	})
}

func TestUserMeDecisions_ProjectCap200AndTimeout(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, true)
	ctx := context.Background()

	// Realistic worst-case cap fixture: 205 member projects x 5 topics per project,
	// each topic holding 1 open prompt + 1 settled answer (1,000 open prompts,
	// 1,000 made answers, and 1,000 distinct agents across the 200 capped scopes).
	const (
		totalProjects    = 205
		topicsPerProject = 5
	)
	base := time.Now().UTC().Add(-2 * time.Hour)
	var oldestProjectID string

	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)

	for i := 0; i < totalProjects; i++ {
		p := &store.Project{
			ID:      api.NewUUID(),
			Name:    fmt.Sprintf("Cap Project %03d", i),
			Slug:    fmt.Sprintf("cap-proj-%03d", i),
			Created: base,
			Updated: base,
		}
		require.NoError(t, f.store.CreateProject(ctx, p))
		if i == 0 {
			oldestProjectID = p.ID
		}
		_, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.bob.ID,
			ScopeType: store.RoleScopeProject, ScopeID: p.ID, CreatedBy: "test",
		})
		require.NoError(t, err)
		for tpIdx := 0; tpIdx < topicsPerProject; tpIdx++ {
			tpID := createTestDecisionTopic(t, f.wcs, p.ID, fmt.Sprintf("topic-%d", tpIdx), f.bob.ID)
			botID := api.NewUUID()
			botName := fmt.Sprintf("bot-%03d-%d", i, tpIdx)
			openKey := fmt.Sprintf("cap-k-%03d-%d", i, tpIdx)
			decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"`+openKey+`","question":"`+openKey+`?","choices":["yes","no"]}]`))
			require.NoError(t, err)
			ts := base.Add(time.Duration(i*20+tpIdx*2) * time.Second)
			_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
				Message: &store.Message{
					ID: api.NewUUID(), ProjectID: p.ID, Sender: "agent:" + botName, SenderID: botID,
					Recipient: "chat", ThreadID: tpID, Channel: "web",
					CreatedAt: ts,
				},
				Intro: openKey, AskerAgentID: botID, AskerAgentName: botName, Decisions: decs,
			})
			require.NoError(t, err)

			madeKey := fmt.Sprintf("cap-m-%03d-%d", i, tpIdx)
			madeDecs, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"`+madeKey+`","question":"`+madeKey+`?","choices":["yes","no"]}]`))
			require.NoError(t, err)
			madePrompt, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
				Message: &store.Message{
					ID: api.NewUUID(), ProjectID: p.ID, Sender: "agent:" + botName, SenderID: botID,
					Recipient: "chat", ThreadID: tpID, Channel: "web",
					CreatedAt: ts.Add(time.Second),
				},
				Intro: madeKey, AskerAgentID: botID, AskerAgentName: botName, Decisions: madeDecs,
			})
			require.NoError(t, err)
			_, _, err = f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
				DecisionRowID: madePrompt.Decisions[0].RowID, ChoiceID: "1",
				AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName,
				Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceDecisionsPage,
				ResponseMessage: &store.Message{
					ID: api.NewUUID(), ProjectID: p.ID, Sender: "user:" + f.bob.ID, SenderID: f.bob.ID,
					Recipient: "chat", ThreadID: tpID, Channel: "web", CreatedAt: ts.Add(time.Second),
				},
			})
			require.NoError(t, err)
		}
	}

	// Use a generous ChatSearchTimeout for the correctness half so busy shared CI
	// runners do not flake with a 503 under host CPU contention (timeout mapping is
	// covered explicitly by the 1 ns and interrupting-store cases below). Always log
	// every timing, and enforce the < 1500 ms headroom budget when SCION_PERF_BUDGET=1.
	f.srv.config.ChatSearchTimeout = 30 * time.Second
	const maxShapeDuration = 1500 * time.Millisecond
	checkPerfBudget := os.Getenv("SCION_PERF_BUDGET") == "1"
	assertBudget := func(elapsed time.Duration, label string) {
		if checkPerfBudget {
			assert.Less(t, elapsed, maxShapeDuration, label)
		}
	}

	startPendingProject := time.Now()
	resp := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?limit=50", nil))
	pendingProjectElapsed := time.Since(startPendingProject)
	assertBudget(pendingProjectElapsed, "pending sort=project")
	assert.Len(t, resp.Prompts, 50)

	startPendingRecent := time.Now()
	respRecent := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?sort=recent&limit=50", nil))
	pendingRecentElapsed := time.Since(startPendingRecent)
	assertBudget(pendingRecentElapsed, "pending sort=recent")
	assert.Len(t, respRecent.Prompts, 50)

	startMadeRecent := time.Now()
	respMadeRecent := decodeProfileMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made", nil))
	madeRecentElapsed := time.Since(startMadeRecent)
	assertBudget(madeRecentElapsed, "tab=made sort=recent")
	assert.Len(t, respMadeRecent.Entries, 50)

	startMadeProject := time.Now()
	respMadeProject := decodeProfileMadeByProjectDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made&sort=project", nil))
	madeProjectElapsed := time.Since(startMadeProject)
	assertBudget(madeProjectElapsed, "tab=made sort=project")
	assert.Len(t, respMadeProject.Groups, maxProfileDecisionProjects)

	startLimitZero := time.Now()
	respLimitZero := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?limit=0", nil))
	limitZeroElapsed := time.Since(startLimitZero)
	assertBudget(limitZeroElapsed, "limit=0")
	assert.Empty(t, respLimitZero.Prompts)

	startSearch := time.Now()
	respSearch := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?q=cap-k-1", nil))
	searchElapsed := time.Since(startSearch)
	assertBudget(searchElapsed, "q=cap-k-1")
	assert.NotEmpty(t, respSearch.Prompts)

	startWaitingTotal := time.Now()
	wTot, wProjs, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	waitingTotalElapsed := time.Since(startWaitingTotal)
	require.NoError(t, err)
	assertBudget(waitingTotalElapsed, "waitingOnYouTotal")
	assert.Equal(t, maxProfileDecisionProjects*topicsPerProject, wTot)
	assert.Equal(t, maxProfileDecisionProjects, wProjs)

	t.Logf("200x5 profile roll-up timings: pending/project=%v pending/recent=%v made/recent=%v made/project=%v limit=0=%v q=%v waitingOnYouTotal=%v",
		pendingProjectElapsed, pendingRecentElapsed, madeRecentElapsed, madeProjectElapsed, limitZeroElapsed, searchElapsed, waitingTotalElapsed)

	// Also measure ListDecisionFacets directly across the 200 resolved scopes
	// (1,000 distinct (asker_agent_id, asker_agent_name) pairs, no DB-side LIMIT).
	measScopes, _, err := f.srv.profileDecisionScopes(withAuthzInputMemo(ctx), NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, "web"), nil, f.wcs, "")
	require.NoError(t, err)
	measSet := newDecisionScopeSet(measScopes)
	startFacets := time.Now()
	rawAgents, _, rawConvKeys, err := f.store.ListDecisionFacets(ctx, measSet.projectIDs, measSet.chatKeys)
	facetsDBElapsed := time.Since(startFacets)
	require.NoError(t, err)
	sortedAgents := decisionAgentFacetViews(measSet, rawAgents)
	facetsTotalElapsed := time.Since(startFacets)
	t.Logf("200x5 ListDecisionFacets: rawAgents=%d rawConvs=%d db=%v db+sort+cap500=%v (capped=%d)",
		len(rawAgents), len(rawConvKeys), facetsDBElapsed, facetsTotalElapsed, len(sortedAgents))

	assert.True(t, resp.TruncatedProjects)
	assert.Len(t, resp.Projects, maxProfileDecisionProjects)
	assert.Len(t, resp.Agents, maxProjectDecisionFacetOptions, "1,000 distinct agents across 200 scopes must be capped at 500")
	assert.Equal(t, maxProfileDecisionProjects*topicsPerProject, resp.Counts.WaitingOnYou)
	for _, pr := range resp.Projects {
		assert.NotEqual(t, oldestProjectID, pr.ID, "oldest project beyond 200 cap should not be in top 200")
	}

	// Explicitly selecting ?project=<oldestProjectID> resolves that project's
	// decisions ("pick a project to see others", design R3) while keeping truncatedProjects=true.
	picked := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		userMeDecisionsPath+"?project="+oldestProjectID, nil))
	assert.True(t, picked.TruncatedProjects)
	require.Len(t, picked.Prompts, topicsPerProject)
	assert.Equal(t, "cap-k-000-4", picked.Prompts[0].Decisions[0].Key)

	// Design D9: every profile request runs under ChatSearchTimeout (even without q)
	// and maps timeout errors (including non-context errors after deadline, such as
	// an interrupted SQLite scan) to 503 SEARCH_TIMEOUT.
	f.srv.config.ChatSearchTimeout = time.Nanosecond
	for _, tab := range []string{ProjectDecisionsTabPending, ProjectDecisionsTabMade} {
		rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab="+tab, nil)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s: %s", tab, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "SEARCH_TIMEOUT", tab)
	}

	intSrv, intStore, intAlice, intBob, intProj, _, _ := setupDemoPolicyTestWithFault(t, func(inner store.Store, _ *storeFaultSwitch) *interruptingDecisionStore {
		return &interruptingDecisionStore{Store: inner}
	})
	intFix := newChoicesFixtureOn(t, intSrv, intStore, intAlice, intBob, intProj)
	setChoicesExperiment(t, intFix.srv, true)
	intFix.topicPrompt(t, `[{"id":"ship-now","question":"Deploy?","choices":["yes","no"]}]`, false)
	intFix.srv.config.ChatSearchTimeout = 50 * time.Millisecond
	for _, tab := range []string{ProjectDecisionsTabPending, ProjectDecisionsTabMade} {
		rec := doRequestAsUser(t, intFix.srv, intFix.bob, http.MethodGet, userMeDecisionsPath+"?tab="+tab, nil)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "interrupt %s: %s", tab, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "SEARCH_TIMEOUT", tab)
	}
}

func TestWaitingOnYouTotal(t *testing.T) {
	f := newChoicesFixture(t)
	ctx := context.Background()

	// Empty / unknown user -> (0, 0, nil).
	tot, projs, err := f.srv.waitingOnYouTotal(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, 0, tot)
	assert.Equal(t, 0, projs)

	tot, projs, err = f.srv.waitingOnYouTotal(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Equal(t, 0, tot)
	assert.Equal(t, 0, projs)

	// Project 1 (f.project): 2 waitingOnYou decisions in a multi-question prompt.
	f.topicPrompt(t, `[
		{"id":"w1","question":"First?","choices":["yes","no"]},
		{"id":"w2","question":"Second?","choices":["yes","no"]}
	]`, true)

	// Project 2 (projDM): 1 waitingOnYou decision in Bob's DM.
	projDM := &store.Project{ID: api.NewUUID(), Name: "DM Proj", Slug: "dm-proj", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projDM))
	bindProjectMember(t, f.store, projDM.ID, f.bob.ID)
	dmBot := &store.Agent{
		ID: api.NewUUID(), Slug: "dm-bot-2", Name: "DM Bot 2",
		ProjectID: projDM.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, dmBot))
	dmKey, err := messages.DMConversationKey("agent", dmBot.ID, "user", f.bob.ID)
	require.NoError(t, err)
	decsDM, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"w3","question":"Third?","choices":["ok","skip"]}]`))
	require.NoError(t, err)
	_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projDM.ID, Sender: "agent:" + dmBot.Slug, SenderID: dmBot.ID,
			Recipient: "user:" + f.bob.ID, ThreadID: dmKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "DM", AskerAgentID: dmBot.ID, AskerAgentName: dmBot.Name, Decisions: decsDM,
	})
	require.NoError(t, err)

	// Project 3 (projOthersOnly): Bob IS a member, but its ONLY open decision is
	// waitingOnOthers (Alice's interpreted answer awaiting Alice's confirmation) ->
	// must NOT increment Bob's total or Bob's distinct project count!
	projOthersOnly := &store.Project{ID: api.NewUUID(), Name: "Others Only", Slug: "others-only", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projOthersOnly))
	bindProjectMember(t, f.store, projOthersOnly.ID, f.bob.ID)
	bindProjectMember(t, f.store, projOthersOnly.ID, f.alice.ID)
	tp3ID := createTestDecisionTopic(t, f.wcs, projOthersOnly.ID, "general", f.alice.ID)
	bot3 := &store.Agent{
		ID: api.NewUUID(), Slug: "bot-3", Name: "Bot 3",
		ProjectID: projOthersOnly.ID, OwnerID: f.alice.ID, CreatedBy: f.alice.ID, Ancestry: []string{f.alice.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, bot3))
	decs3, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"other-only","question":"Alice confirm?","choices":["y","n"]}]`))
	require.NoError(t, err)
	res3, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projOthersOnly.ID, Sender: "agent:" + bot3.Slug, SenderID: bot3.ID,
			Recipient: "chat", ThreadID: tp3ID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "P3", AskerAgentID: bot3.ID, AskerAgentName: bot3.Name, Decisions: decs3,
	})
	require.NoError(t, err)
	recordTestInterpretedForUser(t, f.store, projOthersOnly.ID, tp3ID, bot3, f.alice, res3.Decisions[0].RowID, "1")

	// Project 4 (non-member): must be ignored.
	projOut := &store.Project{ID: api.NewUUID(), Name: "Outsider", Slug: "outsider", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, projOut))
	tp4ID := createTestDecisionTopic(t, f.wcs, projOut.ID, "general", f.alice.ID)
	decs4, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"out-1","question":"Out?","choices":["y","n"]}]`))
	require.NoError(t, err)
	outBotID := api.NewUUID()
	_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projOut.ID, Sender: "agent:out-bot", SenderID: outBotID,
			Recipient: "chat", ThreadID: tp4ID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "P4", AskerAgentID: outBotID, AskerAgentName: "out-bot", Decisions: decs4,
	})
	require.NoError(t, err)

	tot, projs, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, tot, "2 in f.project + 1 in projDM")
	assert.Equal(t, 2, projs, "distinct projects with >=1 waitingOnYou decision")

	// Suspended user -> (0, 0, nil) via checkUserAccount.
	bobRow, err := f.store.GetUser(ctx, f.bob.ID)
	require.NoError(t, err)
	bobRow.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, bobRow))
	tot, projs, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, tot)
	assert.Equal(t, 0, projs)

	bobRow.Status = store.UserStatusActive
	require.NoError(t, f.store.UpdateUser(ctx, bobRow))

	// Expired/canceled bounded ctx surfaces an error.
	expiredCtx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	_, _, err = f.srv.waitingOnYouTotal(expiredCtx, f.bob.ID)
	require.Error(t, err)

	// Unbounded ctx (no deadline) automatically applies s.chatSearchTimeout().
	f.srv.config.ChatSearchTimeout = time.Nanosecond
	_, _, err = f.srv.waitingOnYouTotal(context.Background(), f.bob.ID)
	require.Error(t, err)
}
