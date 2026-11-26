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
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setDecisionsExperiments(t *testing.T, srv *Server, decisionsOn bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.ChatScheduledSend: true,
		experiments.Decisions:         decisionsOn,
	})
	require.Equal(t, decisionsOn, srv.experimentEnabled(experiments.Decisions))
}

func TestProjectDecisions_FilterQueryDialect(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsExperiments(t, f.srv, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	// Create a second topic #ops.
	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()
	opsTopic := WebChatTopic{
		ID:        api.NewUUID(),
		ProjectID: f.project.ID,
		Name:      "ops",
		CreatedBy: f.bob.ID,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, wcs.CreateTopic(ctx, opsTopic))

	// Create additional agents:
	// - f.agent (live, name "alpha" / whatever fixture created)
	// - agentOps (live, name "db-ops")
	// - agentCollide1 & agentCollide2 (two agents sharing name "shared-bot")
	agentOps := &store.Agent{
		ID: api.NewUUID(), Slug: "db-ops", Name: "db-ops",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	agentCollide1 := &store.Agent{
		ID: api.NewUUID(), Slug: "shared-bot-1", Name: "shared-bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	agentCollide2 := &store.Agent{
		ID: api.NewUUID(), Slug: "shared-bot-2", Name: "shared-bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	for _, ag := range []*store.Agent{agentOps, agentCollide1, agentCollide2} {
		require.NoError(t, f.store.CreateAgent(ctx, ag))
	}

	// Create two users who share the same display name "Sam Taylor", plus an
	// outsider user who only answers in Bob's private DM (unreadable to Alice).
	sam1 := &store.User{
		ID: tid("sam-1"), Email: "sam1@example.com", DisplayName: "Sam Taylor",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	sam2 := &store.User{
		ID: tid("sam-2"), Email: "sam2@example.com", DisplayName: "Sam Taylor",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	for _, u := range []*store.User{sam1, sam2} {
		require.NoError(t, f.store.CreateUser(ctx, u))
		ensureHubMembership(ctx, f.store, u.ID)
		addProjectMemberWithRole(t, f.store, f.project, u.ID, store.GroupMemberRoleAdmin)
	}

	seedPrompt := func(ag *store.Agent, chatKey, key, question string) (string, string) {
		msgID := api.NewUUID()
		res, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID:        msgID,
				ProjectID: f.project.ID,
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
	// - P1: f.agent in #sched-topic ("Rollout canary?")
	// - P2: agentOps in #ops ("Scale postgres?")
	// - P3: agentCollide1 in #sched-topic ("Collide pending 1?")
	// - P4: agentCollide2 in #ops ("Collide pending 2?")
	// - P5: f.agent in Bob's private DM ("Secret Bob DM?")
	seedPrompt(f.agent, f.topicID, "P1", "Rollout canary?")
	seedPrompt(agentOps, opsTopic.ID, "P2", "Scale postgres?")
	seedPrompt(agentCollide1, f.topicID, "P3", "Collide pending 1?")
	seedPrompt(agentCollide2, opsTopic.ID, "P4", "Collide pending 2?")
	bobDMKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	_, p5Row := seedPrompt(f.agent, bobDMKey, "P5", "Secret Bob DM?")

	// Made answers:
	// - M1: f.agent, #sched-topic, answered by Bob (direct)
	// - M2: agentOps, #ops, answered by Alice (direct)
	// - M3: agentCollide1, #sched-topic, answered by sam1 (direct)
	// - M4: agentCollide2, #ops, answered by sam2 (dismissed)
	// - M5: f.agent, Bob's private DM, answered by Bob (direct) — Alice cannot see!
	_, m1Row := seedPrompt(f.agent, f.topicID, "M1", "Ship frontend v1?")
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, m1Row, map[string]interface{}{"choice_id": "yes"}).Code)

	_, m2Row := seedPrompt(agentOps, opsTopic.ID, "M2", "Resize cluster?")
	require.Equal(t, http.StatusOK, f.answer(t, f.alice, m2Row, map[string]interface{}{"choice_id": "yes"}).Code)

	_, m3Row := seedPrompt(agentCollide1, f.topicID, "M3", "Enable flag alpha?")
	require.Equal(t, http.StatusOK, f.answer(t, sam1, m3Row, map[string]interface{}{"choice_id": "yes"}).Code)

	m4Msg, m4Row := seedPrompt(agentCollide2, opsTopic.ID, "M4", "Purge stale cache?")
	m4Dec, err := f.store.GetDecision(ctx, m4Row)
	require.NoError(t, err)
	_, err = f.store.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: m4Msg,
		RowIDs:    []string{m4Row},
		User:      store.DecisionAnswerer{UserID: sam2.ID, Name: sam2.DisplayName, Email: sam2.Email},
		Reason:    "obsolete",
		Surface:   store.DecisionSurfaceNative,
		Now:       func() time.Time { return m4Dec.CreatedAt.Add(store.DecisionDismissMinAge + time.Minute) },
		ResponseMessage: &store.Message{
			ID:        api.NewUUID(),
			ProjectID: f.project.ID,
			Sender:    "user:" + sam2.Email,
			Recipient: "agent:" + agentCollide2.Slug,
			Msg:       "Dismissed",
		},
	})
	require.NoError(t, err)

	// Answer P5 in Bob's DM? Keep P5 pending AND add M5 answered in Bob's DM.
	_ = p5Row
	_, m5Row := seedPrompt(f.agent, bobDMKey, "M5", "Private DM answer?")
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, m5Row, map[string]interface{}{"choice_id": "yes"}).Code)

	getPending := func(u *store.User, params url.Values) ProjectPendingDecisionsResponse {
		params.Set("tab", "pending")
		rr := doRequestAsUser(t, f.srv, u, http.MethodGet, basePath+"?"+params.Encode(), nil)
		return decodePendingDecisions(t, rr)
	}
	getMade := func(u *store.User, params url.Values) ProjectMadeDecisionsResponse {
		params.Set("tab", "made")
		rr := doRequestAsUser(t, f.srv, u, http.MethodGet, basePath+"?"+params.Encode(), nil)
		return decodeMadeDecisions(t, rr)
	}
	promptKeys := func(resp ProjectPendingDecisionsResponse) []string {
		var out []string
		for _, p := range resp.Prompts {
			for _, d := range p.Decisions {
				out = append(out, d.Key)
			}
		}
		return out
	}
	entryKeys := func(resp ProjectMadeDecisionsResponse) []string {
		out := make([]string, len(resp.Entries))
		for i, e := range resp.Entries {
			out[i] = e.Decision.Key
		}
		return out
	}

	// 1. Text mode parity (no recognized keys -> plain text search; <2 runes rejected).
	pText := getPending(f.alice, url.Values{"q": {"canary"}})
	assert.ElementsMatch(t, []string{"P1"}, promptKeys(pText))
	rrShort := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, basePath+"?tab=pending&q=a", nil)
	assert.Equal(t, http.StatusBadRequest, rrShort.Code)

	// 2. Structured: agent OR & negation on Pending and Made.
	pOr := getPending(f.alice, url.Values{"q": {"agent:" + f.agent.Name + ",db-ops"}})
	assert.ElementsMatch(t, []string{"P1", "P2"}, promptKeys(pOr))
	assert.NotNil(t, pOr.FilteredCounts)

	pNeg := getPending(f.alice, url.Values{"q": {"-agent:shared-bot"}})
	assert.ElementsMatch(t, []string{"P1", "P2"}, promptKeys(pNeg))

	mOr := getMade(f.alice, url.Values{"q": {"agent:db-ops," + f.agent.ID}})
	assert.ElementsMatch(t, []string{"M1", "M2"}, entryKeys(mOr))

	mNeg := getMade(f.alice, url.Values{"q": {"-agent:shared-bot -agent:db-ops"}})
	assert.ElementsMatch(t, []string{"M1"}, entryKeys(mNeg))

	// 3. Name collision: agent:shared-bot matches BOTH agentCollide1 and agentCollide2.
	pCollide := getPending(f.alice, url.Values{"q": {"agent:shared-bot"}})
	assert.ElementsMatch(t, []string{"P3", "P4"}, promptKeys(pCollide))
	mCollide := getMade(f.alice, url.Values{"q": {"agent:shared-bot"}})
	assert.ElementsMatch(t, []string{"M3", "M4"}, entryKeys(mCollide))

	// 4. Conversation by label, quoted DM label, and negation.
	pConv := getPending(f.alice, url.Values{"q": {"conversation:#ops"}})
	assert.ElementsMatch(t, []string{"P2", "P4"}, promptKeys(pConv))
	pConvNeg := getPending(f.alice, url.Values{"q": {"-conversation:#ops"}})
	assert.ElementsMatch(t, []string{"P1", "P3"}, promptKeys(pConvNeg))
	// Bob can filter his own DM by quoted label; Alice gets empty 200 (never an error or leak).
	bobDMLabel := "DM with " + f.agent.Name
	pBobDM := getPending(f.bob, url.Values{"q": {fmt.Sprintf(`conversation:%q`, bobDMLabel)}})
	assert.ElementsMatch(t, []string{"P5"}, promptKeys(pBobDM))
	pAliceDM := getPending(f.alice, url.Values{"q": {fmt.Sprintf(`conversation:%q`, bobDMLabel)}})
	assert.Empty(t, pAliceDM.Prompts)
	mAliceDM := getMade(f.alice, url.Values{"q": {"conversation:" + bobDMKey}})
	assert.Empty(t, mAliceDM.Entries)

	// 5. by: me, email, user ID, display name, colliding display name -> both, outside scope -> empty 200.
	mByMe := getMade(f.alice, url.Values{"q": {"by:me"}})
	assert.ElementsMatch(t, []string{"M2"}, entryKeys(mByMe))
	mByEmail := getMade(f.alice, url.Values{"q": {"by:sam1@example.com"}})
	assert.ElementsMatch(t, []string{"M3"}, entryKeys(mByEmail))
	mByID := getMade(f.alice, url.Values{"q": {"by:" + sam2.ID}})
	assert.ElementsMatch(t, []string{"M4"}, entryKeys(mByID))
	mByCollideName := getMade(f.alice, url.Values{"q": {`by:"Sam Taylor"`}})
	assert.ElementsMatch(t, []string{"M3", "M4"}, entryKeys(mByCollideName))
	mByOutsideScope := getMade(f.alice, url.Values{"q": {"by:nonexistent@example.com"}})
	assert.Empty(t, mByOutsideScope.Entries)
	mByNeg := getMade(f.alice, url.Values{"q": {`-by:"Sam Taylor" -by:me`}})
	assert.ElementsMatch(t, []string{"M1"}, entryKeys(mByNeg))

	// 6. provenance: OR and negation.
	mProv := getMade(f.alice, url.Values{"q": {"provenance:dismissed"}})
	assert.ElementsMatch(t, []string{"M4"}, entryKeys(mProv))
	mProvNeg := getMade(f.alice, url.Values{"q": {"-provenance:dismissed"}})
	assert.ElementsMatch(t, []string{"M1", "M2", "M3"}, entryKeys(mProvNeg))

	// 7. Made-only keys (by:, provenance:) are ignored on Pending tab (and FilteredCounts stays nil).
	pMadeOnly := getPending(f.alice, url.Values{"q": {"by:me provenance:dismissed"}})
	assert.ElementsMatch(t, []string{"P1", "P2", "P3", "P4"}, promptKeys(pMadeOnly))
	assert.Nil(t, pMadeOnly.FilteredCounts)

	// 8. Unresolvable positive value -> empty 200, never an error.
	pUnknown := getPending(f.alice, url.Values{"q": {"agent:no-such-agent"}})
	assert.Empty(t, pUnknown.Prompts)
	mUnknown := getMade(f.alice, url.Values{"q": {"agent:no-such-agent"}})
	assert.Empty(t, mUnknown.Entries)

	// 9. Separate query params AND with q.
	mCombined := getMade(f.alice, url.Values{
		"agent": {agentCollide1.ID},
		"q":     {"agent:shared-bot"},
	})
	assert.ElementsMatch(t, []string{"M3"}, entryKeys(mCombined))

	// 10. Invalid filter query -> 400 invalid_filter_query with offset and length.
	rrBad := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, basePath+"?tab=made&q="+url.QueryEscape("provenance:bogus"), nil)
	assert.Equal(t, http.StatusBadRequest, rrBad.Code)
	var errBody struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rrBad.Body.Bytes(), &errBody))
	assert.Equal(t, ErrCodeInvalidFilterQuery, errBody.Error.Code)
	assert.Equal(t, float64(0), errBody.Error.Details["offset"])
	assert.Equal(t, float64(len("provenance:bogus")), errBody.Error.Details["length"])

	// 11. Malformed structured query returns 400 invalid_filter_query.
	rrMalformed := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, basePath+"?tab=made&q="+url.QueryEscape("agent:"), nil)
	assert.Equal(t, http.StatusBadRequest, rrMalformed.Code)

	// 12. (O5a) Negated free text on Pending (excFolds) and Made (ExcludeQueries with conversation-label keys).
	pNegText := getPending(f.alice, url.Values{"q": {"agent:" + f.agent.Name + ",db-ops -canary"}})
	assert.ElementsMatch(t, []string{"P2"}, promptKeys(pNegText))
	mNegText := getMade(f.alice, url.Values{"q": {"agent:db-ops," + f.agent.Name + " -ops"}})
	assert.ElementsMatch(t, []string{"M1"}, entryKeys(mNegText))

	// 13. (O5b) Negated conversation: first verify -conversation:#ops alone still returns #sched-topic rows
	// on both tabs (non-vacuous), then remove every readable key Alice has -> empty 200 on both tabs.
	pNegOpsOnly := getPending(f.alice, url.Values{"q": {"-conversation:#ops"}})
	assert.ElementsMatch(t, []string{"P1", "P3"}, promptKeys(pNegOpsOnly))
	mNegOpsOnly := getMade(f.alice, url.Values{"q": {"-conversation:#ops"}})
	assert.ElementsMatch(t, []string{"M1", "M3"}, entryKeys(mNegOpsOnly))

	excludeAllAliceConvs := fmt.Sprintf(`-conversation:#sched-topic,#ops,%q,"DM with db-ops","DM with shared-bot"`, "DM with "+f.agent.Name)
	pNegAllConv := getPending(f.alice, url.Values{"q": {excludeAllAliceConvs}})
	assert.Empty(t, pNegAllConv.Prompts)
	mNegAllConv := getMade(f.alice, url.Values{"q": {excludeAllAliceConvs}})
	assert.Empty(t, mNegAllConv.Entries)

	// 14. (O5c) user= ANDed with by:, and provenance= ANDed with provenance:.
	mUserAndBy := getMade(f.alice, url.Values{
		"user": {sam1.ID},
		"q":    {`by:"Sam Taylor"`},
	})
	assert.ElementsMatch(t, []string{"M3"}, entryKeys(mUserAndBy))
	mUserAndByDisjoint := getMade(f.alice, url.Values{
		"user": {f.bob.ID},
		"q":    {"by:me"},
	})
	assert.Empty(t, mUserAndByDisjoint.Entries)
	mProvAndProv := getMade(f.alice, url.Values{
		"provenance": {"dismissed"},
		"q":          {"provenance:direct,dismissed"},
	})
	assert.ElementsMatch(t, []string{"M4"}, entryKeys(mProvAndProv))
	mProvAndProvDisjoint := getMade(f.alice, url.Values{
		"provenance": {"direct"},
		"q":          {"provenance:dismissed"},
	})
	assert.Empty(t, mProvAndProvDisjoint.Entries)

	// 15. (O5d) Unknown key `foo:bar` stays text mode with dialect on (no 400).
	seedPrompt(f.agent, f.topicID, "P-FOO", "Check foo:bar literal?")
	pUnknownKeyText := getPending(f.alice, url.Values{"q": {"foo:bar"}})
	assert.ElementsMatch(t, []string{"P-FOO"}, promptKeys(pUnknownKeyText))

	// 16. (O6) limit=0 with structured q still loads facets for resolution and returns accurate FilteredCounts.
	pLimitZero := getPending(f.alice, url.Values{"limit": {"0"}, "q": {"agent:db-ops"}})
	assert.Empty(t, pLimitZero.Prompts)
	assert.Empty(t, pLimitZero.Agents)
	require.NotNil(t, pLimitZero.FilteredCounts)
	assert.Equal(t, 1, pLimitZero.FilteredCounts.WaitingOnYou+pLimitZero.FilteredCounts.WaitingOnOthers)
}

func TestProjectDecisions_AnswererSortThenCap600(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsExperiments(t, f.srv, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	// Seed 600 distinct answerers whose user IDs sort opposite to their display names.
	const total = 600
	for i := 0; i < total; i++ {
		uid := fmt.Sprintf("u-%04d", total-1-i)
		name := fmt.Sprintf("Ans %04d", i)
		email := fmt.Sprintf("ans%04d@example.com", i)
		res, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID:        api.NewUUID(),
				ProjectID: f.project.ID,
				Sender:    "agent:" + f.agent.Slug,
				SenderID:  f.agent.ID,
				Recipient: "group:topic",
				ThreadID:  f.topicID,
				CreatedAt: time.Now().UTC(),
			},
			AskerAgentID:   f.agent.ID,
			AskerAgentName: f.agent.Name,
			Decisions: []messages.Decision{
				{Key: fmt.Sprintf("CAP-%04d", i), Question: "Ship?", Choices: []messages.Choice{{ID: "y", Label: "Yes"}}},
			},
		})
		require.NoError(t, err)
		_, _, err = f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID:    res.Decisions[0].RowID,
			ChoiceID:         "y",
			AnsweredByUserID: uid,
			AnsweredByName:   name,
			AnsweredByEmail:  email,
			Provenance:       store.DecisionProvenanceDirect,
			Outcome:          store.DecisionOutcomeAnswer,
			Surface:          store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID:        uuid.New().String(),
				ProjectID: f.project.ID,
				Sender:    "user:" + email,
				Recipient: "agent:" + f.agent.Slug,
				Msg:       "y",
			},
		})
		require.NoError(t, err)
	}

	// Answerers facet is sorted by name first, then capped at 500.
	rrOff := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, basePath+"?tab=made&limit=5", nil)
	madeOff := decodeMadeDecisions(t, rrOff)
	require.Len(t, madeOff.Answerers, maxProjectDecisionFacetOptions)
	assert.Equal(t, "Ans 0000", madeOff.Answerers[0].Name)
	assert.Equal(t, "ans0000@example.com", madeOff.Answerers[0].Hint)
	assert.Equal(t, "Ans 0499", madeOff.Answerers[499].Name)

	// Typing an answerer PAST the 500 cap still filters!
	rrPast := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, basePath+"?tab=made&q="+url.QueryEscape(`by:"Ans 0599"`), nil)
	madePast := decodeMadeDecisions(t, rrPast)
	require.Len(t, madePast.Entries, 1)
	assert.Equal(t, "CAP-0599", madePast.Entries[0].Decision.Key)
}
