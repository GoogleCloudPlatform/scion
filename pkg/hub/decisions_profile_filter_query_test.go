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
	"net/http"
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

func TestProfileDecisions_FilterQueryDialect(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsAndDialectExperiments(t, f.srv, true, true)
	ctx := context.Background()

	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()

	// Project Alpha is f.project (both Alice and Bob are members).
	projAlpha := f.project

	adminRole, err := f.store.GetRoleDefinitionByName(ctx, "project-admin", store.RoleScopeProject)
	require.NoError(t, err)
	grantProjectAdmin := func(projID, userID string) {
		t.Helper()
		_, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: adminRole.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	// Project Beta: both Alice and Bob are members.
	projBeta := &store.Project{
		ID:      api.NewUUID(),
		Slug:    "proj-beta",
		Name:    "Project Beta",
		Created: time.Now().UTC(),
		Updated: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateProject(ctx, projBeta))
	grantProjectAdmin(projBeta.ID, f.alice.ID)
	grantProjectAdmin(projBeta.ID, f.bob.ID)
	betaTopicID := createTestDecisionTopic(t, wcs, projBeta.ID, "beta-ops", f.bob.ID)

	// Project Secret: Bob is a member, Alice is NOT a member (even if she knows its slug/ID).
	projSecret := &store.Project{
		ID:      api.NewUUID(),
		Slug:    "proj-secret",
		Name:    "Project Secret",
		Created: time.Now().UTC(),
		Updated: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateProject(ctx, projSecret))
	grantProjectAdmin(projSecret.ID, f.bob.ID)
	secretTopicID := createTestDecisionTopic(t, wcs, projSecret.ID, "secret-ops", f.bob.ID)

	agentBeta := &store.Agent{
		ID: api.NewUUID(), Slug: "beta-bot", Name: "beta-bot",
		ProjectID: projBeta.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	agentSecret := &store.Agent{
		ID: api.NewUUID(), Slug: "secret-bot", Name: "secret-bot",
		ProjectID: projSecret.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	for _, ag := range []*store.Agent{agentBeta, agentSecret} {
		require.NoError(t, f.store.CreateAgent(ctx, ag))
	}

	seedPrompt := func(projID string, ag *store.Agent, chatKey, key, question string) (string, string) {
		msgID := api.NewUUID()
		res, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID:        msgID,
				ProjectID: projID,
				Sender:    "agent:" + ag.Slug,
				SenderID:  ag.ID,
				Recipient: "group:topic",
				ThreadID:  chatKey,
				CreatedAt: time.Now().UTC(),
			},
			AskerAgentID:   ag.ID,
			AskerAgentName: ag.Name,
			Decisions: []messages.Decision{
				{Key: key, Question: question, Choices: []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}},
			},
		})
		require.NoError(t, err)
		return msgID, res.Decisions[0].RowID
	}

	// Pending prompts:
	// - PA1 in projAlpha (#general, f.agent)
	// - PB1 in projBeta (#beta-ops, agentBeta)
	// - PS1 in projSecret (#secret-ops, agentSecret) — Alice is not a member
	// - PDM in projAlpha (Bob's private DM, f.agent) — unreadable to Alice
	seedPrompt(projAlpha.ID, f.agent, f.topicID, "PA1", "Deploy alpha service?")
	seedPrompt(projBeta.ID, agentBeta, betaTopicID, "PB1", "Deploy beta service?")
	seedPrompt(projSecret.ID, agentSecret, secretTopicID, "PS1", "Deploy secret service?")
	bobDMKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	seedPrompt(projAlpha.ID, f.agent, bobDMKey, "PDM", "Secret Bob DM pending?")

	// Made answers:
	// - MA1 in projAlpha, answered by Alice (direct)
	// - MA2 in projAlpha, answered by Bob (dismissed)
	// - MB1 in projBeta, answered by Bob (direct)
	// - MS1 in projSecret, answered by Bob (direct) — Alice is not a member
	// - MDM in projAlpha, Bob's private DM, answered by Bob (direct) — unreadable to Alice
	_, ma1Row := seedPrompt(projAlpha.ID, f.agent, f.topicID, "MA1", "Alpha rollout v1?")
	require.Equal(t, http.StatusOK, f.answer(t, f.alice, ma1Row, map[string]interface{}{"choice_id": "yes"}).Code)

	_, ma2Row := seedPrompt(projAlpha.ID, f.agent, f.topicID, "MA2", "Alpha cleanup task?")
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, ma2Row, map[string]interface{}{"choice_id": "yes"}).Code)

	_, mb1Row := seedPrompt(projBeta.ID, agentBeta, betaTopicID, "MB1", "Beta rollout v1?")
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, mb1Row, map[string]interface{}{"choice_id": "yes"}).Code)

	_, ms1Row := seedPrompt(projSecret.ID, agentSecret, secretTopicID, "MS1", "Secret rollout v1?")
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, ms1Row, map[string]interface{}{"choice_id": "yes"}).Code)

	_, mdmRow := seedPrompt(projAlpha.ID, f.agent, bobDMKey, "MDM", "Secret Bob DM made?")
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, mdmRow, map[string]interface{}{"choice_id": "yes"}).Code)

	getPending := func(u *store.User, params url.Values) ProfilePendingDecisionsResponse {
		t.Helper()
		p := userMeDecisionsPath
		if len(params) > 0 {
			p += "?" + params.Encode()
		}
		rr := doRequestAsUser(t, f.srv, u, http.MethodGet, p, nil)
		return decodeProfilePendingDecisions(t, rr)
	}
	getMade := func(u *store.User, params url.Values) ProfileMadeDecisionsResponse {
		t.Helper()
		params.Set("tab", "made")
		if params.Get("sort") == "" {
			params.Set("sort", "recent")
		}
		rr := doRequestAsUser(t, f.srv, u, http.MethodGet, userMeDecisionsPath+"?"+params.Encode(), nil)
		return decodeProfileMadeDecisions(t, rr)
	}
	getMadeByProject := func(u *store.User, params url.Values) ProfileMadeByProjectDecisionsResponse {
		t.Helper()
		params.Set("tab", "made")
		params.Set("sort", "project")
		rr := doRequestAsUser(t, f.srv, u, http.MethodGet, userMeDecisionsPath+"?"+params.Encode(), nil)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		var out ProfileMadeByProjectDecisionsResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
		return out
	}

	promptKeys := func(resp ProfilePendingDecisionsResponse) []string {
		var keys []string
		for _, p := range resp.Prompts {
			for _, d := range p.Decisions {
				keys = append(keys, d.Key)
			}
		}
		return keys
	}
	madeKeys := func(resp ProfileMadeDecisionsResponse) []string {
		keys := make([]string, 0, len(resp.Entries))
		for _, e := range resp.Entries {
			keys = append(keys, e.Decision.Key)
		}
		return keys
	}

	t.Run("project filter by slug and ID on Pending and Made", func(t *testing.T) {
		// Pending by slug (case-insensitive)
		pSlug := getPending(f.alice, url.Values{"q": {"project:PROJ-BETA"}})
		assert.Equal(t, []string{"PB1"}, promptKeys(pSlug))
		require.NotNil(t, pSlug.FilteredCounts)

		// Pending by ID
		pID := getPending(f.alice, url.Values{"q": {"project:" + projAlpha.ID}})
		assert.Equal(t, []string{"PA1"}, promptKeys(pID))

		// Made (sort=recent) by slug
		mSlug := getMade(f.alice, url.Values{"q": {"project:proj-beta"}})
		assert.Equal(t, []string{"MB1"}, madeKeys(mSlug))

		// Made (sort=project) by slug
		mGroups := getMadeByProject(f.alice, url.Values{"q": {"project:proj-beta"}})
		require.Len(t, mGroups.Groups, 1)
		assert.Equal(t, projBeta.ID, mGroups.Groups[0].Project.ID)
	})

	t.Run("project OR and negation on Pending and Made", func(t *testing.T) {
		// OR matches both projects
		pOr := getPending(f.alice, url.Values{"q": {"project:" + projAlpha.Slug + ",proj-beta"}})
		assert.ElementsMatch(t, []string{"PA1", "PB1"}, promptKeys(pOr))

		// Negation excludes alpha -> only beta remains
		pNeg := getPending(f.alice, url.Values{"q": {"-project:" + projAlpha.Slug}})
		assert.Equal(t, []string{"PB1"}, promptKeys(pNeg))

		mNeg := getMade(f.alice, url.Values{"q": {"-project:" + projAlpha.Slug}})
		assert.Equal(t, []string{"MB1"}, madeKeys(mNeg))

		// Negating all projects in scope returns empty 200 on both tabs
		pAllNeg := getPending(f.alice, url.Values{"q": {"-project:" + projAlpha.Slug + ",proj-beta"}})
		assert.Empty(t, promptKeys(pAllNeg))

		mAllNeg := getMade(f.alice, url.Values{"q": {"-project:" + projAlpha.Slug + ",proj-beta"}})
		assert.Empty(t, madeKeys(mAllNeg))

		mAllNegGroups := getMadeByProject(f.alice, url.Values{"q": {"-project:" + projAlpha.Slug + ",proj-beta"}})
		assert.Empty(t, mAllNegGroups.Groups)
	})

	t.Run("non-member project and unknown slug return empty 200 (no oracle)", func(t *testing.T) {
		for _, qVal := range []string{
			"project:proj-secret",
			"project:" + projSecret.ID,
			"project:does-not-exist",
		} {
			pResp := getPending(f.alice, url.Values{"q": {qVal}})
			assert.Empty(t, promptKeys(pResp), "pending q=%s", qVal)

			mResp := getMade(f.alice, url.Values{"q": {qVal}})
			assert.Empty(t, madeKeys(mResp), "made q=%s", qVal)
		}

		// Bob IS a member of proj-secret and sees its decisions.
		bobSecret := getPending(f.bob, url.Values{"q": {"project:proj-secret"}})
		assert.Equal(t, []string{"PS1"}, promptKeys(bobSecret))
	})

	t.Run("unreadable DM returns empty 200 for Alice", func(t *testing.T) {
		pDM := getPending(f.alice, url.Values{"q": {"conversation:" + bobDMKey}})
		assert.Empty(t, promptKeys(pDM))

		mDM := getMade(f.alice, url.Values{"q": {"conversation:" + bobDMKey}})
		assert.Empty(t, madeKeys(mDM))
	})

	t.Run("separate project param ANDs with q", func(t *testing.T) {
		// Same project -> matches
		same := getPending(f.alice, url.Values{"project": {projBeta.ID}, "q": {"project:proj-beta"}})
		assert.Equal(t, []string{"PB1"}, promptKeys(same))

		// Conflicting project -> empty 200
		conflict := getPending(f.alice, url.Values{"project": {projBeta.ID}, "q": {"project:" + projAlpha.Slug}})
		assert.Empty(t, promptKeys(conflict))

		// Negating the same project -> empty 200
		negSame := getMade(f.alice, url.Values{"project": {projBeta.ID}, "q": {"-project:proj-beta"}})
		assert.Empty(t, madeKeys(negSame))
	})

	t.Run("by:me and agent: combined with project:, Made-only ignored on Pending", func(t *testing.T) {
		// Made: project + by:me
		mMe := getMade(f.alice, url.Values{"q": {"project:" + projAlpha.Slug + " by:me"}})
		assert.Equal(t, []string{"MA1"}, madeKeys(mMe))

		// Made: project + -by:me
		mNotMe := getMade(f.alice, url.Values{"q": {"project:" + projAlpha.Slug + " -by:me"}})
		assert.Equal(t, []string{"MA2"}, madeKeys(mNotMe))

		// Pending: Made-only keys ignored (all pending returned, FilteredCounts nil)
		pIgnored := getPending(f.alice, url.Values{"q": {"by:me provenance:direct"}})
		assert.ElementsMatch(t, []string{"PA1", "PB1"}, promptKeys(pIgnored))
		assert.Nil(t, pIgnored.FilteredCounts)
	})

	t.Run("text mode parity and invalid query 400", func(t *testing.T) {
		// Pure text mode
		pText := getPending(f.alice, url.Values{"q": {"beta service"}})
		assert.Equal(t, []string{"PB1"}, promptKeys(pText))

		// 1-rune pure text rejected with 400
		rrShort := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath+"?q=a", nil)
		assert.Equal(t, http.StatusBadRequest, rrShort.Code)

		// Invalid syntax in structured mode -> 400 invalid_filter_query with offset/length
		rrBad := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath+"?"+url.Values{"q": {"project:a OR project:b"}}.Encode(), nil)
		require.Equal(t, http.StatusBadRequest, rrBad.Code)
		assert.Contains(t, rrBad.Body.String(), "invalid_filter_query")
		assert.Contains(t, rrBad.Body.String(), `"offset"`)
	})

	t.Run("cursor binds to structured filter query", func(t *testing.T) {
		// Paginate with limit=1 under project:alpha
		page1 := getMade(f.alice, url.Values{"q": {"project:" + projAlpha.Slug}, "limit": {"1"}})
		require.Len(t, page1.Entries, 1)
		require.NotEmpty(t, page1.NextCursor)

		// Replaying the cursor with the same query succeeds
		page2 := getMade(f.alice, url.Values{"q": {"project:" + projAlpha.Slug}, "limit": {"1"}, "cursor": {page1.NextCursor}})
		assert.Len(t, page2.Entries, 1)

		// Replaying the cursor with a different structured query is rejected with 400
		rrCross := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath+"?"+url.Values{
			"tab":    {"made"},
			"sort":   {"recent"},
			"q":      {"project:proj-beta"},
			"limit":  {"1"},
			"cursor": {page1.NextCursor},
		}.Encode(), nil)
		assert.Equal(t, http.StatusBadRequest, rrCross.Code)
	})

	t.Run("experiment off keeps q as plain text", func(t *testing.T) {
		setDecisionsAndDialectExperiments(t, f.srv, true, false)
		defer setDecisionsAndDialectExperiments(t, f.srv, true, true)

		// Reserved OR syntax is NOT a 400 when dialect is off (treated as plain text).
		rr := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath+"?"+url.Values{"q": {"project:a OR project:b"}}.Encode(), nil)
		require.Equal(t, http.StatusOK, rr.Code)
		resp := decodeProfilePendingDecisions(t, rr)
		assert.Empty(t, promptKeys(resp))

		// project:<slug> is NOT interpreted as a project filter when dialect is off.
		rr2 := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, userMeDecisionsPath+"?"+url.Values{"q": {"project:proj-beta"}}.Encode(), nil)
		require.Equal(t, http.StatusOK, rr2.Code)
		resp2 := decodeProfilePendingDecisions(t, rr2)
		assert.Empty(t, promptKeys(resp2))
	})
}
