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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedSuggestionPrompt(t *testing.T, f *decisionsFixture, agID, agSlug, agName, chatKey, key, question string) (string, string) {
	t.Helper()
	msgID := api.NewUUID()
	res, err := f.store.CreateDecisionPrompt(context.Background(), &store.DecisionPromptInput{
		Message: &store.Message{
			ID:        msgID,
			ProjectID: f.project.ID,
			Sender:    "agent:" + agSlug,
			SenderID:  agID,
			Recipient: "group:topic",
			ThreadID:  chatKey,
			CreatedAt: time.Now().UTC(),
		},
		AskerAgentID:   agID,
		AskerAgentName: agName,
		Decisions: []messages.Decision{
			{Key: key, Question: question, Choices: []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}},
		},
	})
	require.NoError(t, err)
	return msgID, res.Decisions[0].RowID
}

func doRequestAsAgentMux(srv *Server, ag *store.Agent, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(agentCtx(req.Context(), ag))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func TestProjectDecisionSuggestions_AuthzScopeAndExperiments(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()
	suggPath := "/api/v1/projects/" + f.project.ID + "/decisions/suggestions"

	// Unsupported method returns 405 even while experiments are off.
	setDecisionsAndDialectExperiments(t, f.srv, false, false)
	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, suggPath+"?key=agent", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// 404 while either web.decisions or filters.query_dialect is off.
	for _, tc := range []struct {
		name        string
		decisionsOn bool
		dialectOn   bool
	}{
		{"both off", false, false},
		{"decisions on dialect off", true, false},
		{"decisions off dialect on", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDecisionsAndDialectExperiments(t, f.srv, tc.decisionsOn, tc.dialectOn)
			rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent", nil)
			assert.Equal(t, http.StatusNotFound, rr.Code)
		})
	}

	setDecisionsAndDialectExperiments(t, f.srv, true, true)

	// Seed a topic decision visible to both Alice and Bob, plus a Bob-only DM
	// decision with a secret agent and a secret answerer.
	secretAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "secret-dm-bot", Name: "secret-dm-bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, secretAgent))

	bobDMKey, err := messages.DMConversationKey("agent", secretAgent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	secretAnswerer := &store.User{
		ID: tid("secret-dm-user"), Email: "secret-dm@example.com", DisplayName: "Secret DM User",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateUser(ctx, secretAnswerer))
	ensureHubMembership(ctx, f.store, secretAnswerer.ID)
	addProjectMemberWithRole(t, f.store, f.project, secretAnswerer.ID, store.GroupMemberRoleAdmin)
	secretUserDMKey, err := messages.DMConversationKey("agent", secretAgent.ID, "user", secretAnswerer.ID)
	require.NoError(t, err)

	_, topicRow := seedSuggestionPrompt(t, f, f.agent.ID, f.agent.Slug, f.agent.Name, f.topicID, "k-topic", "Topic Q")
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, topicRow, map[string]interface{}{"choice_id": "yes"}).Code)

	seedSuggestionPrompt(t, f, secretAgent.ID, secretAgent.Slug, secretAgent.Name, bobDMKey, "k-dm", "Bob private DM Q")
	_, secretDMRow := seedSuggestionPrompt(t, f, secretAgent.ID, secretAgent.Slug, secretAgent.Name, secretUserDMKey, "k-secret-dm", "Secret user private DM Q")
	require.Equal(t, http.StatusOK, f.answer(t, secretAnswerer, secretDMRow, map[string]interface{}{"choice_id": "yes"}).Code)

	// Agent token gets 403.
	agentRR := doRequestAsAgentMux(f.srv, f.agent, http.MethodGet, suggPath+"?key=agent")
	assert.Equal(t, http.StatusForbidden, agentRR.Code)

	// Visitor gets 403.
	visitor := NewVisitorIdentity(tid("vis-sugg"), "https://issuer.test", "sub-v", "vis-sugg@example.com", "Visitor")
	visRR := requestAsIdentity(t, f.srv, visitor, http.MethodGet, suggPath+"?key=agent", nil)
	assert.Equal(t, http.StatusForbidden, visRR.Code)

	// Non-member user without project:read gets 403.
	outsider := &store.User{
		ID: tid("outsider-sugg"), Email: "outsider-sugg@example.com", DisplayName: "Outsider",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateUser(ctx, outsider))
	outRR := doRequestAsUser(t, f.srv, outsider, http.MethodGet, suggPath+"?key=agent", nil)
	assert.Equal(t, http.StatusForbidden, outRR.Code)

	// Alice (member) sees topic agent/conversation/answerer, NEVER Bob's DM label,
	// secret DM agent, or secret DM answerer.
	for _, tc := range []struct {
		key       string
		prefix    string
		mustHave  string
		mustAvoid string
	}{
		{"agent", "", f.agent.Name, "secret-dm-bot"},
		{"conversation", "", "#sched-topic", "DM with secret-dm-bot"},
		{"by", "", f.bob.DisplayName, "Secret DM User"},
	} {
		rr := doRequestAsUser(t, f.srv, f.alice, http.MethodGet,
			suggPath+"?key="+tc.key+"&prefix="+url.QueryEscape(tc.prefix), nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var resp DecisionSuggestionsResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		var vals []string
		for _, v := range resp.Values {
			vals = append(vals, v.Value)
		}
		assert.Contains(t, vals, tc.mustHave, "key=%s", tc.key)
		assert.NotContains(t, vals, tc.mustAvoid, "key=%s must not leak Bob's DM", tc.key)
	}

	// Bob sees his own DM conversation and secret-dm-bot.
	bobConvRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=conversation&prefix=secret", nil)
	require.Equal(t, http.StatusOK, bobConvRR.Code)
	var bobConvResp DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(bobConvRR.Body.Bytes(), &bobConvResp))
	require.Len(t, bobConvResp.Values, 1)
	assert.Equal(t, "DM with secret-dm-bot", bobConvResp.Values[0].Value)

	// R2: UATs (project-bounded and hub-bounded, with and without inbox:read,
	// wrong-project, missing project:read) and federated user refusal.
	t.Run("UATs and federated user", func(t *testing.T) {
		cf := newChatFixture(t)
		setDecisionsAndDialectExperiments(t, cf.srv, true, true)
		cctx := context.Background()
		pPath := "/api/v1/projects/" + cf.projA + "/decisions/suggestions"

		cf.agentA.OwnerID = cf.member
		cf.agentA.Ancestry = []string{cf.member}
		cf.agentA.MessageMode = store.MessageModeProject
		require.NoError(t, cf.s.UpdateAgent(cctx, cf.agentA))

		// Create a DM-only agent so we can prove UAT without inbox:read sees
		// zero DM-derived agent, answerer, or conversation values across all 3 keys.
		dmOnlyAgent := &store.Agent{
			ID: api.NewUUID(), Slug: "uat-dm-only-bot", Name: "uat-dm-only-bot",
			ProjectID: cf.projA, OwnerID: cf.member, CreatedBy: cf.member, Ancestry: []string{cf.member},
			Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
		}
		require.NoError(t, cf.s.CreateAgent(cctx, dmOnlyAgent))

		memberUser, err := cf.s.GetUser(cctx, cf.member)
		require.NoError(t, err)
		dmOnlyAnswerer := &store.User{
			ID: tid("uat-dm-ans"), Email: "uat-dm-ans@example.com", DisplayName: "UAT DM Only Answerer",
			Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
		}
		require.NoError(t, cf.s.CreateUser(cctx, dmOnlyAnswerer))

		// Seed topic decision answered by admin, plus member's private DM with
		// dmOnlyAgent answered by dmOnlyAnswerer.
		seedChatDec := func(chatKey, recip string, ag *store.Agent, key string) string {
			decs, nerr := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"`+key+`?","choices":[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]}]`))
			require.NoError(t, nerr)
			res, cerr := cf.s.CreateDecisionPrompt(cctx, &store.DecisionPromptInput{
				Message: &store.Message{
					ID: api.NewUUID(), ProjectID: cf.projA, Sender: "agent:" + ag.Slug, SenderID: ag.ID,
					Recipient: recip, ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
				},
				Intro: key, AskerAgentID: ag.ID, AskerAgentName: ag.Name, Decisions: decs,
			})
			require.NoError(t, cerr)
			return res.Decisions[0].RowID
		}
		adminUser, err := cf.s.GetUser(cctx, cf.admin)
		require.NoError(t, err)
		topicRowID := seedChatDec(cf.topicA, "chat", cf.agentA, "uat-topic")
		_, _, err = cf.s.RecordAnswer(cctx, &store.RecordAnswerInput{
			DecisionRowID: topicRowID, ChoiceID: "yes",
			AnsweredByUserID: adminUser.ID, AnsweredByName: adminUser.DisplayName, AnsweredByEmail: adminUser.Email,
			Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeAnswer, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{ID: api.NewUUID(), ProjectID: cf.projA, Sender: "user:" + adminUser.Email, Recipient: "agent:" + cf.agentA.Slug, ThreadID: cf.topicA, Msg: "yes"},
		})
		require.NoError(t, err)

		dmKey := cf.dm(t, cf.member, "agent", dmOnlyAgent.ID)
		dmRowID := seedChatDec(dmKey, "user:"+cf.member, dmOnlyAgent, "uat-dm")
		_, _, err = cf.s.RecordAnswer(cctx, &store.RecordAnswerInput{
			DecisionRowID: dmRowID, ChoiceID: "yes",
			AnsweredByUserID: dmOnlyAnswerer.ID, AnsweredByName: dmOnlyAnswerer.DisplayName, AnsweredByEmail: dmOnlyAnswerer.Email,
			Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeAnswer, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{ID: api.NewUUID(), ProjectID: cf.projA, Sender: "user:" + dmOnlyAnswerer.Email, Recipient: "agent:" + dmOnlyAgent.Slug, ThreadID: dmKey, Msg: "yes"},
		})
		require.NoError(t, err)

		// Federated user -> 403.
		fed := NewFederatedUserIdentity("https://issuer.test", "sub-fed", memberUser.Email, "Fed Member", "member", nil)
		assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, cf.srv, fed, http.MethodGet, pPath+"?key=agent", nil).Code)

		// Wrong-project UAT -> 403.
		tokWrongProj := cf.mint(t, cf.admin, projectBoundary(cf.projB), "project:read", "inbox:read", "agent:read")
		assert.Equal(t, http.StatusForbidden, cf.call(t, tokWrongProj, http.MethodGet, pPath+"?key=agent", nil).Code)

		// UAT missing project:read -> 403.
		tokNoProjRead := cf.mint(t, cf.member, projectBoundary(cf.projA), "inbox:read", "agent:read")
		assert.Equal(t, http.StatusForbidden, cf.call(t, tokNoProjRead, http.MethodGet, pPath+"?key=agent", nil).Code)

		decodeVals := func(rr *httptest.ResponseRecorder) []string {
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var resp DecisionSuggestionsResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			out := make([]string, 0, len(resp.Values))
			for _, v := range resp.Values {
				out = append(out, v.Value)
			}
			return out
		}

		// Project UAT with project:read only (no inbox:read): topic suggestions only,
		// NEVER DM-only agent, DM-only answerer, or DM conversation label!
		tokNoInbox := cf.mint(t, cf.member, projectBoundary(cf.projA), "project:read")
		assert.NotContains(t, decodeVals(cf.call(t, tokNoInbox, http.MethodGet, pPath+"?key=agent", nil)), "uat-dm-only-bot")
		assert.Contains(t, decodeVals(cf.call(t, tokNoInbox, http.MethodGet, pPath+"?key=agent", nil)), cf.agentA.Name)
		assert.NotContains(t, decodeVals(cf.call(t, tokNoInbox, http.MethodGet, pPath+"?key=conversation", nil)), "DM with uat-dm-only-bot")
		assert.NotContains(t, decodeVals(cf.call(t, tokNoInbox, http.MethodGet, pPath+"?key=by", nil)), "UAT DM Only Answerer")

		// Project UAT with project:read + inbox:read + agent:read: own DM included for all 3 keys!
		tokWithInbox := cf.mint(t, cf.member, projectBoundary(cf.projA), "project:read", "inbox:read", "agent:read")
		assert.Contains(t, decodeVals(cf.call(t, tokWithInbox, http.MethodGet, pPath+"?key=agent", nil)), "uat-dm-only-bot")
		assert.Contains(t, decodeVals(cf.call(t, tokWithInbox, http.MethodGet, pPath+"?key=conversation", nil)), "DM with uat-dm-only-bot")
		assert.Contains(t, decodeVals(cf.call(t, tokWithInbox, http.MethodGet, pPath+"?key=by", nil)), "UAT DM Only Answerer")
	})
}

func TestProjectDecisionSuggestions_ValidationCursorRankingAndCollisions(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsAndDialectExperiments(t, f.srv, true, true)
	ctx := context.Background()
	suggPath := "/api/v1/projects/" + f.project.ID + "/decisions/suggestions"

	// Validation errors.
	longPrefix := make([]byte, 201)
	for i := range longPrefix {
		longPrefix[i] = 'a'
	}
	for _, tc := range []struct {
		name     string
		query    string
		wantCode string
	}{
		{"missing key", "", errCodeInvalidKey},
		{"unknown key", "key=bogus", errCodeInvalidKey},
		{"enum key provenance", "key=provenance", errCodeInvalidKey},
		{"project key not in project schema", "key=project", errCodeInvalidKey},
		{"prefix too long", "key=agent&prefix=" + string(longPrefix), ""},
		{"limit zero", "key=agent&limit=0", ""},
		{"limit over max", "key=agent&limit=101", ""},
		{"limit non-int", "key=agent&limit=abc", ""},
		{"malformed cursor", "key=agent&cursor=not-valid-base64!!!", errCodeInvalidCursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?"+tc.query, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			if tc.wantCode != "" {
				assert.Contains(t, rr.Body.String(), tc.wantCode)
			}
		})
	}

	// Seed agents for ranking tiers.
	agents := []*store.Agent{
		{ID: api.NewUUID(), Slug: "dep", Name: "dep", ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID}, Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject},
		{ID: api.NewUUID(), Slug: "deploy-frontend", Name: "deploy-frontend", ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID}, Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject},
		{ID: api.NewUUID(), Slug: "release-deployer", Name: "release-deployer", ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID}, Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject},
		{ID: api.NewUUID(), Slug: "data-engine-prod", Name: "data-engine-prod", ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID}, Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject},
	}
	for i, ag := range agents {
		require.NoError(t, f.store.CreateAgent(ctx, ag))
		seedSuggestionPrompt(t, f, ag.ID, ag.Slug, ag.Name, f.topicID, fmt.Sprintf("k-rank-%d", i), fmt.Sprintf("Q-%d", i))
	}

	// Ranking order for prefix="dep": exact ("dep") -> prefix ("deploy-frontend") -> substring ("release-deployer") -> subsequence ("data-engine-prod").
	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent&prefix=dep", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var rankResp DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &rankResp))
	assert.Equal(t, "agent", rankResp.Key)
	assert.Equal(t, 4, rankResp.Total)
	assert.Empty(t, rankResp.NextCursor)
	require.Len(t, rankResp.Values, 4)
	assert.Equal(t, "dep", rankResp.Values[0].Value)
	assert.Equal(t, "deploy-frontend", rankResp.Values[1].Value)
	assert.Equal(t, "release-deployer", rankResp.Values[2].Value)
	assert.Equal(t, "data-engine-prod", rankResp.Values[3].Value)

	// Cursor pagination with limit=2 across the 4 ranked items.
	p1RR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent&prefix=dep&limit=2", nil)
	require.Equal(t, http.StatusOK, p1RR.Code)
	var p1 DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(p1RR.Body.Bytes(), &p1))
	require.Len(t, p1.Values, 2)
	assert.Equal(t, 4, p1.Total)
	require.NotEmpty(t, p1.NextCursor)
	assert.Equal(t, []string{"dep", "deploy-frontend"}, []string{p1.Values[0].Value, p1.Values[1].Value})

	// Cursor mismatch on key or prefix returns 400 invalid_cursor.
	badKeyRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=by&prefix=dep&cursor="+url.QueryEscape(p1.NextCursor), nil)
	assert.Equal(t, http.StatusBadRequest, badKeyRR.Code)
	assert.Contains(t, badKeyRR.Body.String(), errCodeInvalidCursor)

	badPrefixRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent&prefix=depl&cursor="+url.QueryEscape(p1.NextCursor), nil)
	assert.Equal(t, http.StatusBadRequest, badPrefixRR.Code)
	assert.Contains(t, badPrefixRR.Body.String(), errCodeInvalidCursor)

	// O4: project A's cursor is rejected (400 invalid_cursor) on project B and on /users/me.
	otherProj := &store.Project{ID: api.NewUUID(), Name: "Other Project", Slug: "other-proj", Created: time.Now().UTC(), Updated: time.Now().UTC()}
	require.NoError(t, f.store.CreateProject(ctx, otherProj))
	require.NoError(t, f.store.CreateGroup(ctx, &store.Group{
		ID:        api.NewUUID(),
		Name:      otherProj.Name + " Members",
		Slug:      "project:" + otherProj.Slug + ":members",
		GroupType: store.GroupTypeExplicit,
		ProjectID: otherProj.ID,
		Created:   time.Now().UTC(),
		Updated:   time.Now().UTC(),
	}))
	addProjectMemberWithRole(t, f.store, otherProj, f.bob.ID, store.GroupMemberRoleAdmin)
	otherProjRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		"/api/v1/projects/"+otherProj.ID+"/decisions/suggestions?key=agent&prefix=dep&cursor="+url.QueryEscape(p1.NextCursor), nil)
	assert.Equal(t, http.StatusBadRequest, otherProjRR.Code)
	assert.Contains(t, otherProjRR.Body.String(), errCodeInvalidCursor)

	profileCurRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		"/api/v1/users/me/decisions/suggestions?key=agent&prefix=dep&cursor="+url.QueryEscape(p1.NextCursor), nil)
	assert.Equal(t, http.StatusBadRequest, profileCurRR.Code)
	assert.Contains(t, profileCurRR.Body.String(), errCodeInvalidCursor)

	// Page 2 with valid cursor returns remaining 2 items and no nextCursor.
	p2RR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent&prefix=dep&limit=2&cursor="+url.QueryEscape(p1.NextCursor), nil)
	require.Equal(t, http.StatusOK, p2RR.Code)
	var p2 DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(p2RR.Body.Bytes(), &p2))
	require.Len(t, p2.Values, 2)
	assert.Equal(t, 4, p2.Total)
	assert.Empty(t, p2.NextCursor)
	assert.Equal(t, []string{"release-deployer", "data-engine-prod"}, []string{p2.Values[0].Value, p2.Values[1].Value})

	// by: collision rules — two answerers share "Sam Taylor", one unique "Aaron Park"
	// (alphabetically before "me") + "Priya Raman", plus caller Bob pinned first as "me".
	aaron := &store.User{ID: tid("sugg-aaron"), Email: "aaron@example.com", DisplayName: "Aaron Park", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	sam1 := &store.User{ID: tid("sugg-sam1"), Email: "sam1@example.com", DisplayName: "Sam Taylor", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	sam2 := &store.User{ID: tid("sugg-sam2"), Email: "sam2@example.com", DisplayName: "Sam Taylor", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	priya := &store.User{ID: tid("sugg-priya"), Email: "priya@example.com", DisplayName: "Priya Raman", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	namedMe := &store.User{ID: tid("sugg-me-name"), Email: "me-user@example.com", DisplayName: "Me", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	for _, u := range []*store.User{aaron, sam1, sam2, priya, namedMe} {
		require.NoError(t, f.store.CreateUser(ctx, u))
		ensureHubMembership(ctx, f.store, u.ID)
		addProjectMemberWithRole(t, f.store, f.project, u.ID, store.GroupMemberRoleAdmin)
	}
	for i, u := range []*store.User{f.bob, aaron, sam1, sam2, priya, namedMe} {
		_, rowID := seedSuggestionPrompt(t, f, f.agent.ID, f.agent.Slug, f.agent.Name, f.topicID, fmt.Sprintf("k-by-%d", i), fmt.Sprintf("Ans Q %d", i))
		require.Equal(t, http.StatusOK, f.answer(t, u, rowID, map[string]interface{}{"choice_id": "yes"}).Code)
	}

	byRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=by", nil)
	require.Equal(t, http.StatusOK, byRR.Code, byRR.Body.String())
	var byResp DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(byRR.Body.Bytes(), &byResp))
	require.NotEmpty(t, byResp.Values)
	assert.Equal(t, "me", byResp.Values[0].Value, "me must stay pinned first for by: even ahead of alphabetically earlier names")
	byMap := make(map[string]string, len(byResp.Values))
	for _, v := range byResp.Values {
		byMap[v.Value] = v.Help
	}
	assert.Contains(t, byMap, "me")
	assert.Contains(t, byMap["me"], "You")
	assert.Equal(t, "aaron@example.com", byMap["Aaron Park"])
	assert.Equal(t, "priya@example.com", byMap["Priya Raman"], "unique answerer inserts display name with email hint")
	assert.Equal(t, "Sam Taylor", byMap["sam1@example.com"], "colliding answerer inserts email with display name hint")
	assert.Equal(t, "Sam Taylor", byMap["sam2@example.com"], "colliding answerer inserts email with display name hint")
	assert.NotContains(t, byMap, "Sam Taylor", "colliding display name must not be inserted directly")
	assert.Equal(t, "Me", byMap["me-user@example.com"], "answerer named Me collides with me keyword and inserts email with Me hint")
	assert.NotContains(t, byMap, "Me", "display name Me must not be inserted directly")

	// O3: cancelled/timed-out context aborts decisionSuggestionsCore cleanly.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, cerr := f.srv.decisionSuggestionsCore(cancelCtx, nil, newDecisionScopeSet([]*projectReadableScope{{project: f.project, chatKeys: []string{f.topicID}}}), f.project.ID, decisionSuggestionsQuery{key: "agent", limit: 12}, false)
	assert.ErrorIs(t, cerr, context.Canceled)

	// limit=0 with structured q loads raw facets for resolution, returns filteredCounts,
	// and emits no facet views or facetsTruncated.
	lim0RR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		"/api/v1/projects/"+f.project.ID+"/decisions?tab=pending&limit=0&q="+url.QueryEscape("agent:dep"), nil)
	require.Equal(t, http.StatusOK, lim0RR.Code, lim0RR.Body.String())
	var lim0Resp ProjectPendingDecisionsResponse
	require.NoError(t, json.Unmarshal(lim0RR.Body.Bytes(), &lim0Resp))
	require.NotNil(t, lim0Resp.FilteredCounts)
	assert.Equal(t, 1, lim0Resp.FilteredCounts.WaitingOnYou)
	assert.Empty(t, lim0Resp.Agents)
	assert.Empty(t, lim0Resp.Answerers)
	assert.Empty(t, lim0Resp.Conversations)
	assert.Nil(t, lim0Resp.FacetsTruncated)
}

func TestProjectDecisionSuggestions_UncappedPast500AndFacetsTruncated(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsAndDialectExperiments(t, f.srv, true, true)
	ctx := context.Background()
	listPath := "/api/v1/projects/" + f.project.ID + "/decisions"
	suggPath := listPath + "/suggestions"

	// Small project: facetsTruncated is omitted.
	seedSuggestionPrompt(t, f, f.agent.ID, f.agent.Slug, f.agent.Name, f.topicID, "k-small", "Small Q")
	smallRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, listPath+"?tab=pending", nil)
	require.Equal(t, http.StatusOK, smallRR.Code)
	var smallResp ProjectPendingDecisionsResponse
	require.NoError(t, json.Unmarshal(smallRR.Body.Bytes(), &smallResp))
	assert.Nil(t, smallResp.FacetsTruncated)

	// O3: Seed >500 distinct asking agents, >500 distinct answerers, and >500
	// distinct readable topics so all three facetsTruncated flags must be true
	// and /suggestions must find the past-cap entry for every key.
	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()

	const totalItems = 505
	for i := 0; i < totalItems; i++ {
		agName := fmt.Sprintf("agent-%03d", i)
		ansName := fmt.Sprintf("Answerer %03d", i)
		ansEmail := fmt.Sprintf("ans%03d@example.com", i)
		topicName := fmt.Sprintf("topic-%03d", i)
		if i == totalItems-1 {
			agName = "zzz-past-cap-agent"
			ansName = "Zzz Past Cap Answerer"
			ansEmail = "zzz-past-cap@example.com"
			topicName = "zzz-topic-504"
		}

		topicID := api.NewUUID()
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID:        topicID,
			ProjectID: f.project.ID,
			Name:      topicName,
			CreatedBy: f.bob.ID,
			CreatedAt: time.Now().UTC(),
		}))

		agID := api.NewUUID()
		_, rowID := seedSuggestionPrompt(t, f, agID, agName, agName, topicID, fmt.Sprintf("k-cap-%03d", i), "Q?")
		_, _, err := f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID:    rowID,
			ChoiceID:         "yes",
			AnsweredByUserID: fmt.Sprintf("u-cap-%04d", i),
			AnsweredByName:   ansName,
			AnsweredByEmail:  ansEmail,
			Provenance:       store.DecisionProvenanceDirect,
			Outcome:          store.DecisionOutcomeAnswer,
			Surface:          store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID:        api.NewUUID(),
				ProjectID: f.project.ID,
				Sender:    "user:" + ansEmail,
				Recipient: "agent:" + agName,
				ThreadID:  topicID,
				Msg:       "Yes",
			},
		})
		require.NoError(t, err)
	}

	// List response caps all 3 facets at 500 and sets all 3 facetsTruncated flags.
	bigRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, listPath+"?tab=made&limit=5", nil)
	require.Equal(t, http.StatusOK, bigRR.Code, bigRR.Body.String())
	var bigResp ProjectMadeDecisionsResponse
	require.NoError(t, json.Unmarshal(bigRR.Body.Bytes(), &bigResp))
	assert.Len(t, bigResp.Agents, maxProjectDecisionFacetOptions)
	assert.Len(t, bigResp.Answerers, maxProjectDecisionFacetOptions)
	assert.Len(t, bigResp.Conversations, maxProjectDecisionFacetOptions)
	require.NotNil(t, bigResp.FacetsTruncated)
	assert.True(t, bigResp.FacetsTruncated.Agents, "agents must be flagged truncated")
	assert.True(t, bigResp.FacetsTruncated.Answerers, "answerers must be flagged truncated")
	assert.True(t, bigResp.FacetsTruncated.Conversations, "conversations must be flagged truncated")
	for _, a := range bigResp.Agents {
		assert.NotEqual(t, "zzz-past-cap-agent", a.Name)
	}
	for _, u := range bigResp.Answerers {
		assert.NotEqual(t, "Zzz Past Cap Answerer", u.Name)
	}
	for _, c := range bigResp.Conversations {
		assert.NotEqual(t, "#zzz-topic-504", c.Label)
	}

	// Suggestions endpoint searches uncapped and finds all 3 past-cap values!
	for _, tc := range []struct {
		key    string
		prefix string
		want   string
	}{
		{"agent", "zzz-past-cap", "zzz-past-cap-agent"},
		{"by", "Zzz Past Cap", "Zzz Past Cap Answerer"},
		{"conversation", "zzz-topic-504", "#zzz-topic-504"},
	} {
		suggRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
			suggPath+"?key="+tc.key+"&prefix="+url.QueryEscape(tc.prefix), nil)
		require.Equal(t, http.StatusOK, suggRR.Code, suggRR.Body.String())
		var suggResp DecisionSuggestionsResponse
		require.NoError(t, json.Unmarshal(suggRR.Body.Bytes(), &suggResp))
		assert.Equal(t, 1, suggResp.Total, "key=%s", tc.key)
		require.Len(t, suggResp.Values, 1, "key=%s", tc.key)
		assert.Equal(t, tc.want, suggResp.Values[0].Value, "key=%s", tc.key)
	}
}

func TestUserMeDecisionSuggestions_AuthzScopeAndValidation(t *testing.T) {
	f := newDecisionsFixture(t)
	suggPath := "/api/v1/users/me/decisions/suggestions"

	// 405 on POST before experiment gates.
	setDecisionsAndDialectExperiments(t, f.srv, false, false)
	assert.Equal(t, http.StatusMethodNotAllowed,
		doRequestAsUser(t, f.srv, f.bob, http.MethodPost, suggPath+"?key=agent", nil).Code)

	// 404 when either experiment is off.
	setDecisionsAndDialectExperiments(t, f.srv, true, false)
	assert.Equal(t, http.StatusNotFound,
		doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent", nil).Code)
	setDecisionsAndDialectExperiments(t, f.srv, false, true)
	assert.Equal(t, http.StatusNotFound,
		doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent", nil).Code)

	setDecisionsAndDialectExperiments(t, f.srv, true, true)

	// Agent and visitor get 403.
	assert.Equal(t, http.StatusForbidden,
		doRequestAsAgentMux(f.srv, f.agent, http.MethodGet, suggPath+"?key=agent").Code)
	visitor := NewVisitorIdentity(tid("vis-me-sugg"), "https://issuer.test", "sub-v", "vis-me-sugg@example.com", "Visitor")
	assert.Equal(t, http.StatusForbidden,
		requestAsIdentity(t, f.srv, visitor, http.MethodGet, suggPath+"?key=agent", nil).Code)

	// Validation: invalid_key returns 400.
	badRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=project", nil)
	assert.Equal(t, http.StatusBadRequest, badRR.Code)
	assert.Contains(t, badRR.Body.String(), errCodeInvalidKey)

	// Seed a decision in f.project (Bob & Alice are members) and a Bob-only DM.
	seedSuggestionPrompt(t, f, f.agent.ID, f.agent.Slug, f.agent.Name, f.topicID, "k-prof-topic", "Profile topic Q")
	bobDMKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	seedSuggestionPrompt(t, f, f.agent.ID, f.agent.Slug, f.agent.Name, bobDMKey, "k-prof-dm", "Bob profile DM Q")

	// Alice sees the topic conversation (with project name hint), never Bob's DM.
	aliceRR := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, suggPath+"?key=conversation", nil)
	require.Equal(t, http.StatusOK, aliceRR.Code, aliceRR.Body.String())
	var aliceResp DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(aliceRR.Body.Bytes(), &aliceResp))
	require.Len(t, aliceResp.Values, 1)
	assert.Equal(t, "#sched-topic", aliceResp.Values[0].Value)
	assert.Equal(t, f.project.Name, aliceResp.Values[0].Help)

	// Bob sees both the topic and his own DM.
	bobRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=conversation", nil)
	require.Equal(t, http.StatusOK, bobRR.Code)
	var bobResp DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(bobRR.Body.Bytes(), &bobResp))
	assert.Equal(t, 2, bobResp.Total)

	// R2: Profile route UATs (hub UAT without project:read -> 200 values: [],
	// project-bounded UAT sees only its boundary project, UAT without inbox:read
	// omits own DM across all 3 keys, UAT with inbox:read includes own DM) and
	// federated user refusal.
	t.Run("UATs and federated user", func(t *testing.T) {
		cf := newChatFixture(t)
		setDecisionsAndDialectExperiments(t, cf.srv, true, true)
		cctx := context.Background()
		bindProjectMember(t, cf.s, cf.projB, cf.member)

		for _, ag := range []*store.Agent{cf.agentA, cf.agentB} {
			ag.OwnerID = cf.member
			ag.Ancestry = []string{cf.member}
			ag.MessageMode = store.MessageModeProject
			require.NoError(t, cf.s.UpdateAgent(cctx, ag))
		}
		dmOnlyAgent := &store.Agent{
			ID: api.NewUUID(), Slug: "prof-dm-only-bot", Name: "prof-dm-only-bot",
			ProjectID: cf.projA, OwnerID: cf.member, CreatedBy: cf.member, Ancestry: []string{cf.member},
			Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
		}
		require.NoError(t, cf.s.CreateAgent(cctx, dmOnlyAgent))

		memberUser, err := cf.s.GetUser(cctx, cf.member)
		require.NoError(t, err)
		dmOnlyAnswerer := &store.User{
			ID: tid("prof-dm-ans"), Email: "prof-dm-ans@example.com", DisplayName: "Prof DM Only Answerer",
			Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
		}
		require.NoError(t, cf.s.CreateUser(cctx, dmOnlyAnswerer))

		seedProfDec := func(projID, chatKey, recip string, ag *store.Agent, key string) string {
			decs, nerr := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"`+key+`?","choices":[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]}]`))
			require.NoError(t, nerr)
			res, cerr := cf.s.CreateDecisionPrompt(cctx, &store.DecisionPromptInput{
				Message: &store.Message{
					ID: api.NewUUID(), ProjectID: projID, Sender: "agent:" + ag.Slug, SenderID: ag.ID,
					Recipient: recip, ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
				},
				Intro: key, AskerAgentID: ag.ID, AskerAgentName: ag.Name, Decisions: decs,
			})
			require.NoError(t, cerr)
			return res.Decisions[0].RowID
		}
		seedProfDec(cf.projA, cf.topicA, "chat", cf.agentA, "prof-a-topic")
		seedProfDec(cf.projB, cf.topicB, "chat", cf.agentB, "prof-b-topic")

		dmKeyA := cf.dm(t, cf.member, "agent", dmOnlyAgent.ID)
		dmRowA := seedProfDec(cf.projA, dmKeyA, "user:"+cf.member, dmOnlyAgent, "prof-a-dm")
		_, _, err = cf.s.RecordAnswer(cctx, &store.RecordAnswerInput{
			DecisionRowID: dmRowA, ChoiceID: "yes",
			AnsweredByUserID: dmOnlyAnswerer.ID, AnsweredByName: dmOnlyAnswerer.DisplayName, AnsweredByEmail: dmOnlyAnswerer.Email,
			Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeAnswer, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{ID: api.NewUUID(), ProjectID: cf.projA, Sender: "user:" + dmOnlyAnswerer.Email, Recipient: "agent:" + dmOnlyAgent.Slug, ThreadID: dmKeyA, Msg: "yes"},
		})
		require.NoError(t, err)

		// Federated user -> 403.
		fed := NewFederatedUserIdentity("https://issuer.test", "sub-fed-prof", memberUser.Email, "Fed Member", "member", nil)
		assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, cf.srv, fed, http.MethodGet, suggPath+"?key=agent", nil).Code)

		decodeVals := func(rr *httptest.ResponseRecorder) []string {
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var resp DecisionSuggestionsResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			out := make([]string, 0, len(resp.Values))
			for _, v := range resp.Values {
				out = append(out, v.Value)
			}
			return out
		}

		// Hub UAT without project:read -> 200 with empty values for all 3 keys.
		tokHubNoProjRead := cf.mint(t, cf.member, hubBoundary(), "inbox:read")
		for _, k := range []string{"agent", "conversation", "by"} {
			assert.Empty(t, decodeVals(cf.call(t, tokHubNoProjRead, http.MethodGet, suggPath+"?key="+k, nil)), "key=%s", k)
		}

		// Project-bounded UAT (projA) without inbox:read -> sees projA topic only,
		// NOT projB and NOT own DM across all 3 keys.
		tokProjANoInbox := cf.mint(t, cf.member, projectBoundary(cf.projA), "project:read")
		agVals := decodeVals(cf.call(t, tokProjANoInbox, http.MethodGet, suggPath+"?key=agent", nil))
		assert.Contains(t, agVals, cf.agentA.Name)
		assert.NotContains(t, agVals, cf.agentB.Name)
		assert.NotContains(t, agVals, "prof-dm-only-bot")
		assert.NotContains(t, decodeVals(cf.call(t, tokProjANoInbox, http.MethodGet, suggPath+"?key=conversation", nil)), "DM with prof-dm-only-bot")
		assert.NotContains(t, decodeVals(cf.call(t, tokProjANoInbox, http.MethodGet, suggPath+"?key=by", nil)), "Prof DM Only Answerer")

		// Project-bounded UAT (projA) with project:read + inbox:read + agent:read ->
		// sees projA topic + own projA DM, still NOT projB!
		tokProjAWithInbox := cf.mint(t, cf.member, projectBoundary(cf.projA), "project:read", "inbox:read", "agent:read")
		agValsDM := decodeVals(cf.call(t, tokProjAWithInbox, http.MethodGet, suggPath+"?key=agent", nil))
		assert.Contains(t, agValsDM, cf.agentA.Name)
		assert.Contains(t, agValsDM, "prof-dm-only-bot")
		assert.NotContains(t, agValsDM, cf.agentB.Name)
		assert.Contains(t, decodeVals(cf.call(t, tokProjAWithInbox, http.MethodGet, suggPath+"?key=conversation", nil)), "DM with prof-dm-only-bot")
		assert.Contains(t, decodeVals(cf.call(t, tokProjAWithInbox, http.MethodGet, suggPath+"?key=by", nil)), "Prof DM Only Answerer")
	})
}
