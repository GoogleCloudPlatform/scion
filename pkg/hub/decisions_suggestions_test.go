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

	// Page 2 with valid cursor returns remaining 2 items and no nextCursor.
	p2RR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent&prefix=dep&limit=2&cursor="+url.QueryEscape(p1.NextCursor), nil)
	require.Equal(t, http.StatusOK, p2RR.Code)
	var p2 DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(p2RR.Body.Bytes(), &p2))
	require.Len(t, p2.Values, 2)
	assert.Equal(t, 4, p2.Total)
	assert.Empty(t, p2.NextCursor)
	assert.Equal(t, []string{"release-deployer", "data-engine-prod"}, []string{p2.Values[0].Value, p2.Values[1].Value})

	// by: collision rules — two answerers share "Sam Taylor", one unique "Priya Raman", plus caller Bob as "me".
	sam1 := &store.User{ID: tid("sugg-sam1"), Email: "sam1@example.com", DisplayName: "Sam Taylor", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	sam2 := &store.User{ID: tid("sugg-sam2"), Email: "sam2@example.com", DisplayName: "Sam Taylor", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	priya := &store.User{ID: tid("sugg-priya"), Email: "priya@example.com", DisplayName: "Priya Raman", Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC()}
	for _, u := range []*store.User{sam1, sam2, priya} {
		require.NoError(t, f.store.CreateUser(ctx, u))
		ensureHubMembership(ctx, f.store, u.ID)
		addProjectMemberWithRole(t, f.store, f.project, u.ID, store.GroupMemberRoleAdmin)
	}
	for i, u := range []*store.User{f.bob, sam1, sam2, priya} {
		_, rowID := seedSuggestionPrompt(t, f, f.agent.ID, f.agent.Slug, f.agent.Name, f.topicID, fmt.Sprintf("k-by-%d", i), fmt.Sprintf("Ans Q %d", i))
		require.Equal(t, http.StatusOK, f.answer(t, u, rowID, map[string]interface{}{"choice_id": "yes"}).Code)
	}

	byRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=by", nil)
	require.Equal(t, http.StatusOK, byRR.Code, byRR.Body.String())
	var byResp DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(byRR.Body.Bytes(), &byResp))
	byMap := make(map[string]string, len(byResp.Values))
	for _, v := range byResp.Values {
		byMap[v.Value] = v.Help
	}
	assert.Contains(t, byMap, "me")
	assert.Contains(t, byMap["me"], "You")
	assert.Equal(t, "priya@example.com", byMap["Priya Raman"], "unique answerer inserts display name with email hint")
	assert.Equal(t, "Sam Taylor", byMap["sam1@example.com"], "colliding answerer inserts email with display name hint")
	assert.Equal(t, "Sam Taylor", byMap["sam2@example.com"], "colliding answerer inserts email with display name hint")
	assert.NotContains(t, byMap, "Sam Taylor", "colliding display name must not be inserted directly")
}

func TestProjectDecisionSuggestions_UncappedPast500AndFacetsTruncated(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsAndDialectExperiments(t, f.srv, true, true)
	listPath := "/api/v1/projects/" + f.project.ID + "/decisions"
	suggPath := listPath + "/suggestions"

	// Small project: facetsTruncated is omitted.
	seedSuggestionPrompt(t, f, f.agent.ID, f.agent.Slug, f.agent.Name, f.topicID, "k-small", "Small Q")
	smallRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, listPath+"?tab=pending", nil)
	require.Equal(t, http.StatusOK, smallRR.Code)
	var smallResp ProjectPendingDecisionsResponse
	require.NoError(t, json.Unmarshal(smallRR.Body.Bytes(), &smallResp))
	assert.Nil(t, smallResp.FacetsTruncated)

	// Seed 505 distinct asking agents via decisions (agent-000 .. agent-504,
	// with zzz-past-cap-agent sorting past the 500 cap).
	const totalAgents = 505
	for i := 0; i < totalAgents; i++ {
		name := fmt.Sprintf("agent-%03d", i)
		if i == totalAgents-1 {
			name = "zzz-past-cap-agent"
		}
		agID := api.NewUUID()
		seedSuggestionPrompt(t, f, agID, name, name, f.topicID, fmt.Sprintf("k-cap-%03d", i), "Q?")
	}

	// List response caps agents at 500 and sets facetsTruncated.agents = true.
	bigRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, listPath+"?tab=pending&limit=5", nil)
	require.Equal(t, http.StatusOK, bigRR.Code, bigRR.Body.String())
	var bigResp ProjectPendingDecisionsResponse
	require.NoError(t, json.Unmarshal(bigRR.Body.Bytes(), &bigResp))
	assert.Len(t, bigResp.Agents, maxProjectDecisionFacetOptions)
	require.NotNil(t, bigResp.FacetsTruncated)
	assert.True(t, bigResp.FacetsTruncated.Agents)
	assert.False(t, bigResp.FacetsTruncated.Answerers)
	assert.False(t, bigResp.FacetsTruncated.Conversations)
	for _, a := range bigResp.Agents {
		assert.NotEqual(t, "zzz-past-cap-agent", a.Name)
	}

	// Suggestions endpoint searches uncapped and finds zzz-past-cap-agent!
	suggRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, suggPath+"?key=agent&prefix=zzz-past-cap", nil)
	require.Equal(t, http.StatusOK, suggRR.Code, suggRR.Body.String())
	var suggResp DecisionSuggestionsResponse
	require.NoError(t, json.Unmarshal(suggRR.Body.Bytes(), &suggResp))
	assert.Equal(t, 1, suggResp.Total)
	require.Len(t, suggResp.Values, 1)
	assert.Equal(t, "zzz-past-cap-agent", suggResp.Values[0].Value)
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
}
