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
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setInboxAndDecisionsExperiments(t *testing.T, srv *Server, inboxOn, decisionsOn bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard: inboxOn,
		experiments.Decisions:      decisionsOn,
	})
	require.Equal(t, inboxOn, srv.experimentEnabled(experiments.InboxDashboard))
	require.Equal(t, decisionsOn, srv.experimentEnabled(experiments.Decisions))
}

func decodeDecisionPreviews(t *testing.T, rawPreviews []any) []InboxDecisionPreview {
	t.Helper()
	out := make([]InboxDecisionPreview, 0, len(rawPreviews))
	for _, raw := range rawPreviews {
		b, err := json.Marshal(raw)
		require.NoError(t, err)
		var p InboxDecisionPreview
		require.NoError(t, json.Unmarshal(b, &p))
		out = append(out, p)
	}
	return out
}

func TestUserMeInboxSummary_DecisionsFlagOffAndParity(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()

	// Make f.member a member of both f.projA and f.projB.
	bindProjectMember(t, f.s, f.projB, f.member)
	for _, ag := range []*store.Agent{f.agentA, f.agentB} {
		ag.OwnerID = f.member
		ag.Ancestry = []string{f.member}
		ag.MessageMode = store.MessageModeProject
		require.NoError(t, f.s.UpdateAgent(ctx, ag))
	}

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	otherUser, err := f.s.GetUser(ctx, f.admin)
	require.NoError(t, err)

	seedPrompt := func(projID, chatKey, recipient string, agent *store.Agent, rawJSON string, at time.Time) (string, *store.DecisionPromptResult) {
		decs, nerr := messages.NormalizeDecisions("decisions", decisionInputs(t, rawJSON))
		require.NoError(t, nerr)
		msgID := api.NewUUID()
		res, cerr := f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: projID, Sender: "agent:" + agent.Slug, SenderID: agent.ID,
				Recipient: recipient, ThreadID: chatKey, Channel: "web", CreatedAt: at,
			},
			Intro: "Please decide", AskerAgentID: agent.ID, AskerAgentName: agent.Slug, Decisions: decs,
		})
		require.NoError(t, cerr)
		return msgID, res
	}

	now := time.Now().UTC().Truncate(time.Second)
	dmKeyA := f.dm(t, f.member, "agent", f.agentA.ID)

	// 1. Oldest: single decision in DM with allow_other.
	p1MsgID, p1 := seedPrompt(f.projA, dmKeyA, "user:"+f.member, f.agentA,
		`[{"id":"D1","question":"Pick deploy target?","choices":[{"id":"prod","label":"Production"},{"id":"stg","label":"Staging"}],"allow_other":true}]`,
		now.Add(-40*time.Minute))

	// 2. Second oldest: single decision in thread with a >200-rune question.
	longQ := strings.Repeat("α", 220)
	p2MsgID, _ := seedPrompt(f.projA, f.topicA, "chat", f.agentA,
		`[{"id":"D2","question":"`+longQ+`","choices":["yes","no"]}]`,
		now.Add(-30*time.Minute))

	// 3. Third oldest: multi-decision set prompt in projB (inline must be absent).
	p3MsgID, _ := seedPrompt(f.projB, f.topicB, "chat", f.agentB,
		`[{"id":"D3A","question":"First set question?","choices":["a","b"]},{"id":"D3B","question":"Second set question?","choices":["c","d"]}]`,
		now.Add(-20*time.Minute))

	// 4. Fourth (newest): single decision in projB (falls past default previews=3 cap).
	_, _ = seedPrompt(f.projB, f.topicB, "chat", f.agentB,
		`[{"id":"D4","question":"Newest prompt?","choices":["ok","skip"]}]`,
		now.Add(-10*time.Minute))

	// 5. Waiting-on-others prompt (interpreted for another user): must NOT be counted in waitingOnYou.
	_, pOther := seedPrompt(f.projA, f.topicA, "chat", f.agentA,
		`[{"id":"D5","question":"Waiting on admin confirm?","choices":["yes","no"]}]`,
		now.Add(-50*time.Minute))
	recordTestInterpretedForUser(t, f.s, f.projA, f.topicA, f.agentA, otherUser, pOther.Decisions[0].RowID, "1")

	// AC4: web.inbox_dashboard ON, web.decisions OFF -> state:"disabled", count:0.
	setInboxAndDecisionsExperiments(t, f.srv, true, false)
	offResp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil))
	offDec := offResp.Categories[InboxCategoryDecisions]
	assert.Equal(t, InboxStateDisabled, offDec.State)
	assert.Equal(t, 0, offDec.Count)
	assert.False(t, offDec.Capped)
	assert.False(t, offDec.CountsTowardTotal)
	assert.Equal(t, "/profile/decisions?tab=pending", offDec.Href)
	assert.Empty(t, offDec.Previews)
	assert.Equal(t, 0, offResp.Total)

	// Enable web.decisions too.
	setInboxAndDecisionsExperiments(t, f.srv, true, true)

	// AC3: decisions count must equal /api/v1/users/me/decisions counts.waitingOnYou.
	profileDec := decodeProfilePendingDecisions(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeDecisionsPath, nil))
	require.Equal(t, 5, profileDec.Counts.WaitingOnYou, "1+1+2+1 waiting-on-you decisions")

	summaryResp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil))
	decCat := summaryResp.Categories[InboxCategoryDecisions]
	assert.Equal(t, InboxStateOK, decCat.State)
	assert.Equal(t, profileDec.Counts.WaitingOnYou, decCat.Count)
	assert.Equal(t, profileDec.Counts.WaitingOnYou, summaryResp.Total)
	assert.False(t, decCat.Capped)
	assert.Equal(t, "/profile/decisions?tab=pending", decCat.Href)

	// AC5: capped at default 3, ordered oldest first, exact preview shapes.
	previews := decodeDecisionPreviews(t, decCat.Previews)
	require.Len(t, previews, 3)

	// Preview 0: p1 (oldest, DM, single, canAnswer=true, allowsOther=true).
	assert.Equal(t, p1MsgID, previews[0].ID)
	assert.Equal(t, p1MsgID, previews[0].PromptMessageID)
	assert.Equal(t, f.projA, previews[0].ProjectID)
	assert.NotEmpty(t, previews[0].ProjectName)
	assert.Equal(t, starDMPermalink(dmKeyA, p1MsgID), previews[0].Href)
	assert.Equal(t, "Pick deploy target?", previews[0].Question)
	assert.Equal(t, 1, previews[0].DecisionCount)
	assert.Equal(t, DecisionAnswerModeSingle, previews[0].AnswerMode)
	assert.True(t, previews[0].CanAnswer)
	assert.Empty(t, previews[0].DisabledReason)
	require.NotNil(t, previews[0].Inline)
	assert.Equal(t, p1.Decisions[0].RowID, previews[0].Inline.DecisionRowID)
	assert.True(t, previews[0].Inline.AllowsOther)
	assert.Equal(t, []InboxDecisionChoice{
		{ID: "prod", Label: "Production"},
		{ID: "stg", Label: "Staging"},
	}, previews[0].Inline.Choices)

	// Preview 1: p2 (thread, >200-rune question truncated to 200 runes with ellipsis).
	assert.Equal(t, p2MsgID, previews[1].ID)
	assert.Equal(t, starPermalink(f.projA, f.topicA, p2MsgID), previews[1].Href)
	assert.Equal(t, 200, utf8.RuneCountInString(previews[1].Question))
	assert.True(t, strings.HasSuffix(previews[1].Question, "…"))
	require.NotNil(t, previews[1].Inline)
	assert.False(t, previews[1].Inline.AllowsOther)

	// Preview 2: p3 (multi-decision set, inline must be nil).
	assert.Equal(t, p3MsgID, previews[2].ID)
	assert.Equal(t, f.projB, previews[2].ProjectID)
	assert.Equal(t, "First set question?", previews[2].Question)
	assert.Equal(t, 2, previews[2].DecisionCount)
	assert.Equal(t, DecisionAnswerModeSet, previews[2].AnswerMode)
	assert.True(t, previews[2].CanAnswer)
	assert.Nil(t, previews[2].Inline, "set prompts must omit inline block")

	// previews=0 returns count only with empty previews array.
	zeroResp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil))
	assert.Equal(t, 5, zeroResp.Categories[InboxCategoryDecisions].Count)
	assert.Empty(t, zeroResp.Categories[InboxCategoryDecisions].Previews)
}

// TestUserMeInboxSummary_DecisionsTokenScopingAndSingleflight pins §4.2 token
// scoping for the scope-dependent decisions category and confirms that
// concurrent requests from distinct UATs for the same user never coalesce or
// share results across token scopes/boundaries.
func TestUserMeInboxSummary_DecisionsTokenScopingAndSingleflight(t *testing.T) {
	f := newChatFixture(t)
	setInboxAndDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()

	bindProjectMember(t, f.s, f.projB, f.member)
	for _, ag := range []*store.Agent{f.agentA, f.agentB} {
		ag.OwnerID = f.member
		ag.Ancestry = []string{f.member}
		ag.MessageMode = store.MessageModeProject
		require.NoError(t, f.s.UpdateAgent(ctx, ag))
	}

	seedPrompt := func(projID, chatKey, recipient, key string, agent *store.Agent, at time.Time) {
		decs, nerr := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"`+key+`?","choices":["yes","no"]}]`))
		require.NoError(t, nerr)
		_, cerr := f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
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
	seedPrompt(f.projA, f.topicA, "chat", "A1", f.agentA, now.Add(-3*time.Minute))
	seedPrompt(f.projA, dmKeyA, "user:"+f.member, "A2", f.agentA, now.Add(-2*time.Minute))
	seedPrompt(f.projB, f.topicB, "chat", "B1", f.agentB, now.Add(-1*time.Minute))

	// Three distinct UATs for the same user with different scopes/boundaries:
	tokNoProjRead := f.mint(t, f.member, hubBoundary(), "inbox:read")
	tokProjA := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "project:read", "agent:read")
	tokHubAll := f.mint(t, f.member, hubBoundary(), "inbox:read", "project:read", "agent:read")

	// Sequential verification of token scoping and inline omission.
	respNoProj := decodeInboxSummary(t, f.call(t, tokNoProjRead, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateOK, respNoProj.Categories[InboxCategoryDecisions].State)
	assert.Equal(t, 0, respNoProj.Categories[InboxCategoryDecisions].Count)
	assert.Empty(t, respNoProj.Categories[InboxCategoryDecisions].Previews)

	respProjA := decodeInboxSummary(t, f.call(t, tokProjA, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 2, respProjA.Categories[InboxCategoryDecisions].Count)
	projAPreviews := decodeDecisionPreviews(t, respProjA.Categories[InboxCategoryDecisions].Previews)
	require.Len(t, projAPreviews, 2)
	for _, p := range projAPreviews {
		assert.Equal(t, f.projA, p.ProjectID)
		assert.False(t, p.CanAnswer, "UAT callers cannot answer decisions inline")
		assert.NotEmpty(t, p.DisabledReason)
		assert.Nil(t, p.Inline, "UAT previews must omit inline block")
	}

	respHubAll := decodeInboxSummary(t, f.call(t, tokHubAll, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 3, respHubAll.Categories[InboxCategoryDecisions].Count)

	// Concurrent verification: all three tokens fire simultaneously and must
	// never share singleflight results.
	const rounds = 4
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		var cNoProj, cProjA, cHubAll int
		wg.Add(3)
		go func() {
			defer wg.Done()
			cNoProj = decodeInboxSummary(t, f.call(t, tokNoProjRead, http.MethodGet, userMeInboxSummaryPath, nil)).Categories[InboxCategoryDecisions].Count
		}()
		go func() {
			defer wg.Done()
			cProjA = decodeInboxSummary(t, f.call(t, tokProjA, http.MethodGet, userMeInboxSummaryPath, nil)).Categories[InboxCategoryDecisions].Count
		}()
		go func() {
			defer wg.Done()
			cHubAll = decodeInboxSummary(t, f.call(t, tokHubAll, http.MethodGet, userMeInboxSummaryPath, nil)).Categories[InboxCategoryDecisions].Count
		}()
		wg.Wait()
		assert.Equal(t, 0, cNoProj, "round %d: token without project:read must see 0", r)
		assert.Equal(t, 2, cProjA, "round %d: projA token must see 2", r)
		assert.Equal(t, 3, cHubAll, "round %d: hub token must see 3", r)
	}
}

type inboxDecisionsCountingStore struct {
	store.Store
	listDirectBindings atomic.Int32
	getEffectiveGroups atomic.Int32
	projectsWithDecs   atomic.Int32
	listOpenPrompts    atomic.Int32
}

func (s *inboxDecisionsCountingStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	s.listDirectBindings.Add(1)
	return s.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

func (s *inboxDecisionsCountingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	s.getEffectiveGroups.Add(1)
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s *inboxDecisionsCountingStore) ProjectsWithDecisions(ctx context.Context, projectIDs []string) ([]string, error) {
	s.projectsWithDecs.Add(1)
	return s.Store.ProjectsWithDecisions(ctx, projectIDs)
}

func (s *inboxDecisionsCountingStore) ListOpenPrompts(ctx context.Context, projectIDs, chatKeys []string) ([]*store.ProjectDecisionPrompt, error) {
	s.listOpenPrompts.Add(1)
	return s.Store.ListOpenPrompts(ctx, projectIDs, chatKeys)
}

func TestUserMeInboxSummary_DecisionsStoreCallBound(t *testing.T) {
	f := newDecisionsFixture(t)
	counter, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *inboxDecisionsCountingStore {
		return &inboxDecisionsCountingStore{Store: inner}
	})
	setInboxAndDecisionsExperiments(t, f.srv, true, true)

	f.topicPrompt(t, oneDecision, false)

	counter.listDirectBindings.Store(0)
	counter.getEffectiveGroups.Store(0)
	counter.projectsWithDecs.Store(0)
	counter.listOpenPrompts.Store(0)

	resp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateOK, resp.Categories[InboxCategoryDecisions].State)
	assert.Equal(t, 1, resp.Categories[InboxCategoryDecisions].Count)

	// AC6: bounded constant calls plus memoised per-project authz.
	assert.Equal(t, int32(1), counter.projectsWithDecs.Load(), "ProjectsWithDecisions should run once")
	assert.Equal(t, int32(1), counter.listOpenPrompts.Load(), "ListOpenPrompts should run once")
	assert.LessOrEqual(t, counter.listDirectBindings.Load(), int32(2), "role bindings should be memoised")
	assert.LessOrEqual(t, counter.getEffectiveGroups.Load(), int32(2), "effective groups should be memoised")

	// Directly verify chatMemberProjectIDs memoisation reuses the cached result
	// across repeated calls on the same request context.
	counter.listDirectBindings.Store(0)
	counter.getEffectiveGroups.Store(0)
	memoCtx := withChatMemberProjectIDsMemo(context.Background())
	m1, err := f.srv.chatMemberProjectIDs(memoCtx, f.bob.ID)
	require.NoError(t, err)
	m2, err := f.srv.chatMemberProjectIDs(memoCtx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, m1, m2)
	assert.Equal(t, int32(1), counter.listDirectBindings.Load(), "second chatMemberProjectIDs call must hit memo")
	assert.Equal(t, int32(1), counter.getEffectiveGroups.Load(), "second chatMemberProjectIDs call must hit memo")
}

func TestDecisionAnswer_SurfaceInbox(t *testing.T) {
	f := newDecisionsFixture(t)

	// Single-decision answer with surface:"inbox" is accepted and recorded.
	_, promptRes := f.topicPrompt(t, oneDecision, false)
	rowID := promptRes.Decisions[0].RowID

	rec := f.answer(t, f.bob, rowID, map[string]interface{}{
		"choice_id": "1",
		"surface":   store.DecisionSurfaceInbox,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var ansResp DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ansResp))
	require.NotNil(t, ansResp.Decision.Decision)
	assert.Equal(t, store.DecisionSurfaceInbox, ansResp.Decision.Decision.Surface)

	// Confirm the stored answer carries surface:"inbox" on /users/me/decisions?tab=made.
	madeResp := decodeProfileMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made", nil))
	require.Len(t, madeResp.Entries, 1)
	assert.Equal(t, store.DecisionSurfaceInbox, madeResp.Entries[0].Surface)

	// Unsupported surface still returns 400 validation error.
	_, promptRes2 := f.topicPrompt(t, `[{"id":"D999","question":"Another?","choices":["a","b"]}]`, false)
	badRec := f.answer(t, f.bob, promptRes2.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "a",
		"surface":   "bogus",
	})
	assert.Equal(t, http.StatusBadRequest, badRec.Code)
	assert.Contains(t, badRec.Body.String(), store.DecisionSurfaceInbox)
}

type inboxDecisionsFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	mu    sync.Mutex
	err   error
}

func (s *inboxDecisionsFaultStore) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *inboxDecisionsFaultStore) ListOpenPrompts(ctx context.Context, projectIDs, chatKeys []string) ([]*store.ProjectDecisionPrompt, error) {
	if s.fault.Active() {
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return s.Store.ListOpenPrompts(ctx, projectIDs, chatKeys)
}

func TestUserMeInboxSummary_DecisionsForcedErrorIsolation(t *testing.T) {
	srv, rawStore, alice, bob, project, faultStore, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, sw *storeFaultSwitch) *inboxDecisionsFaultStore {
		return &inboxDecisionsFaultStore{Store: inner, fault: sw}
	})
	f := newDecisionsFixtureOn(t, srv, rawStore, alice, bob, project)
	setInboxAndDecisionsExperiments(t, f.srv, true, true)

	// Seed one open decision and one live pending secret for f.bob so we can
	// verify approvals remains intact and decisions is excluded from total when
	// ListOpenPrompts fails.
	f.topicPrompt(t, oneDecision, false)
	now := time.Now().UTC()
	_, err := f.store.CreateOrReplacePendingSecret(context.Background(), &store.PendingSecret{
		ID:          api.NewUUID(),
		Key:         "ISOLATION_SECRET",
		SecretType:  store.SecretTypeEnvironment,
		Scope:       store.ScopeUser,
		ScopeID:     f.bob.ID,
		ValueRef:    "ref-do-not-leak",
		ValueLen:    8,
		AgentID:     f.agent.ID,
		AgentSlug:   f.agent.Slug,
		ProjectID:   f.project.ID,
		ProjectName: f.project.Name,
		Created:     now,
		Updated:     now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}, now)
	require.NoError(t, err)

	// 1. Internal store error on ListOpenPrompts -> HTTP 200, decisions error("internal"),
	// approvals ok(1), total == 1.
	faultStore.setErr(errors.New("forced ListOpenPrompts failure"))
	fault.Arm()
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	assert.Equal(t, 1, resp.Total, "errored decisions category must be excluded from total")
	assert.Equal(t, InboxStateOK, resp.Categories[InboxCategoryApprovals].State)
	assert.Equal(t, 1, resp.Categories[InboxCategoryApprovals].Count)
	decCat := resp.Categories[InboxCategoryDecisions]
	assert.Equal(t, InboxStateError, decCat.State)
	assert.Equal(t, 0, decCat.Count)
	assert.Equal(t, InboxErrorInternal, decCat.ErrorCode)
	assert.Empty(t, decCat.Previews)
	assert.Equal(t, "/profile/decisions?tab=pending", decCat.Href)

	// 2. Timeout error on ListOpenPrompts -> HTTP 200, decisions error("timeout"),
	// approvals still ok(1), total == 1.
	faultStore.setErr(context.DeadlineExceeded)
	recTO := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeInboxSummaryPath, nil)
	respTO := decodeInboxSummary(t, recTO)
	assert.Equal(t, 1, respTO.Total)
	assert.Equal(t, InboxStateOK, respTO.Categories[InboxCategoryApprovals].State)
	assert.Equal(t, InboxStateError, respTO.Categories[InboxCategoryDecisions].State)
	assert.Equal(t, 0, respTO.Categories[InboxCategoryDecisions].Count)
	assert.Equal(t, InboxErrorTimeout, respTO.Categories[InboxCategoryDecisions].ErrorCode)
}

type cancelAfterListOpenPromptsStore struct {
	store.Store
	fault  *storeFaultSwitch
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (s *cancelAfterListOpenPromptsStore) setCancel(fn context.CancelFunc) {
	s.mu.Lock()
	s.cancel = fn
	s.mu.Unlock()
}

func (s *cancelAfterListOpenPromptsStore) ListOpenPrompts(ctx context.Context, projectIDs, chatKeys []string) ([]*store.ProjectDecisionPrompt, error) {
	prompts, err := s.Store.ListOpenPrompts(ctx, projectIDs, chatKeys)
	if err == nil && s.fault.Active() {
		s.mu.Lock()
		fn := s.cancel
		s.mu.Unlock()
		if fn != nil {
			fn()
		}
	}
	return prompts, err
}

func TestWaitingOnYouSummary_PropagatesContextCancellation(t *testing.T) {
	srv, rawStore, alice, bob, project, cancelStore, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, sw *storeFaultSwitch) *cancelAfterListOpenPromptsStore {
		return &cancelAfterListOpenPromptsStore{Store: inner, fault: sw}
	})
	f := newDecisionsFixtureOn(t, srv, rawStore, alice, bob, project)
	f.topicPrompt(t, oneDecision, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelStore.setCancel(cancel)
	fault.Arm()

	viewer := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))
	_, err := f.srv.waitingOnYouSummary(withAuthzInputMemo(ctx), viewer, nil, 3)
	require.ErrorIs(t, err, context.Canceled, "context cancellation during classifyProjectOpenPrompts must propagate instead of returning partial counts")
}

// TestUserMeInboxSummary_DecisionsCappedMatchesTruncatedProjects pins
// decisions.capped == truncatedProjects, including the early-return path where
// filterProjectsWithUnsettledDecisions leaves zero open projects.
func TestUserMeInboxSummary_DecisionsCappedMatchesTruncatedProjects(t *testing.T) {
	f := newChatFixture(t)
	setInboxAndDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()

	// Give f.member read membership on 3 projects: f.projA, f.projB, and projC.
	bindProjectMember(t, f.s, f.projB, f.member)
	projC := &store.Project{
		ID:      api.NewUUID(),
		Slug:    "inbox-cap-proj-c",
		Name:    "Inbox Cap Project C",
		OwnerID: f.owner,
		Created: time.Now().UTC(),
		Updated: time.Now().UTC(),
	}
	require.NoError(t, f.s.CreateProject(ctx, projC))
	bindProjectMember(t, f.s, projC.ID, f.member)
	topicC := api.NewUUID()
	require.NoError(t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID:             topicC,
		ProjectID:      projC.ID,
		Name:           "general",
		IsGeneral:      true,
		CreatedBy:      f.member,
		CreatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	}))
	agentC := &store.Agent{
		ID:          api.NewUUID(),
		Slug:        "inbox-agent-c",
		Name:        "Inbox Agent C",
		ProjectID:   projC.ID,
		OwnerID:     f.member,
		Ancestry:    []string{f.member},
		MessageMode: store.MessageModeProject,
		Phase:       "running",
	}
	require.NoError(t, f.s.CreateAgent(ctx, agentC))

	for _, ag := range []*store.Agent{f.agentA, f.agentB} {
		ag.OwnerID = f.member
		ag.Ancestry = []string{f.member}
		ag.MessageMode = store.MessageModeProject
		require.NoError(t, f.s.UpdateAgent(ctx, ag))
	}

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)

	// Seed one settled (withdrawn) decision in each of the 3 projects so
	// ProjectsWithDecisions returns all 3 projects while
	// filterProjectsWithUnsettledDecisions leaves zero open projects.
	seedSettled := func(projID, chatKey string, ag *store.Agent, key string) {
		decs, nErr := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"Settled?","choices":["yes","no"]}]`))
		require.NoError(t, nErr)
		res, cErr := f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: projID, Sender: "agent:" + ag.Slug, SenderID: ag.ID,
				Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			Intro: "Settled", AskerAgentID: ag.ID, AskerAgentName: ag.Slug, Decisions: decs,
		})
		require.NoError(t, cErr)
		_, wErr := f.s.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
			AskerAgentID: ag.ID,
			Keys:         []string{res.Decisions[0].Key},
			Reason:       "no longer needed",
		})
		require.NoError(t, wErr)
	}
	seedSettled(f.projA, f.topicA, f.agentA, "S1")
	seedSettled(f.projB, f.topicB, f.agentB, "S2")
	seedSettled(projC.ID, topicC, agentC, "S3")

	// 1. Cap at 2 with 3 readable member projects and ZERO unsettled decisions.
	//    filterProjectsWithUnsettledDecisions returns empty; capped must STILL be true.
	f.srv.maxProfileDecisionProjectsForTest = 2
	zeroResp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil))
	zeroDec := zeroResp.Categories[InboxCategoryDecisions]
	assert.Equal(t, InboxStateOK, zeroDec.State)
	assert.Equal(t, 0, zeroDec.Count)
	assert.True(t, zeroDec.Capped, "capped must mirror truncatedProjects even when zero projects have unsettled decisions")
	assert.True(t, zeroDec.CountsTowardTotal)
	assert.True(t, zeroResp.TotalCapped)

	// 2. Cap at 3 (not exceeded) -> capped must be false.
	f.srv.maxProfileDecisionProjectsForTest = 3
	uncappedResp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil))
	uncappedDec := uncappedResp.Categories[InboxCategoryDecisions]
	assert.Equal(t, InboxStateOK, uncappedDec.State)
	assert.Equal(t, 0, uncappedDec.Count)
	assert.False(t, uncappedDec.Capped)
	assert.False(t, uncappedResp.TotalCapped)

	// 3. Seed an open decision in every project and cap back at 2 -> capped must be true
	//    on the non-empty open-projects path too.
	for idx, target := range []struct {
		projID  string
		chatKey string
		ag      *store.Agent
		key     string
	}{
		{f.projA, f.topicA, f.agentA, "O1"},
		{f.projB, f.topicB, f.agentB, "O2"},
		{projC.ID, topicC, agentC, "O3"},
	} {
		decs, nErr := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+target.key+`","question":"Cap check?","choices":["yes","no"]}]`))
		require.NoError(t, nErr)
		_, cErr := f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: target.projID, Sender: "agent:" + target.ag.Slug, SenderID: target.ag.ID,
				Recipient: "chat", ThreadID: target.chatKey, Channel: "web", CreatedAt: time.Now().UTC().Add(time.Duration(idx) * time.Second),
			},
			Intro: "Cap check", AskerAgentID: target.ag.ID, AskerAgentName: target.ag.Slug, Decisions: decs,
		})
		require.NoError(t, cErr)
	}

	f.srv.maxProfileDecisionProjectsForTest = 2
	openResp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil))
	openDec := openResp.Categories[InboxCategoryDecisions]
	assert.Equal(t, InboxStateOK, openDec.State)
	assert.Equal(t, 2, openDec.Count)
	assert.True(t, openDec.Capped, "capped must be true when readable projects exceed cap with open decisions")
	assert.True(t, openResp.TotalCapped)
}

func TestInboxDecisionPromptHref_UsesSharedChatPathHelpers(t *testing.T) {
	dmKey := "dm:agent:a/1:user:u:2"
	projID := "proj/with space"
	threadKey := "topic:with/slash"
	msgID := "msg:1/2"

	set := &decisionScopeSet{
		projectReadableScope: &projectReadableScope{
			conversationAvailable: map[string]bool{
				dmKey:     true,
				threadKey: true,
			},
		},
	}

	assert.Equal(t, starDMPermalink(dmKey, msgID), inboxDecisionPromptHref(set, classifiedOpenPrompt{
		raw:         &store.ProjectDecisionPrompt{ChatKey: dmKey, MessageID: msgID},
		promptAvail: true,
	}))
	assert.Equal(t, chatDMWebPath(dmKey), inboxDecisionPromptHref(set, classifiedOpenPrompt{
		raw:         &store.ProjectDecisionPrompt{ChatKey: dmKey, MessageID: msgID},
		promptAvail: false,
	}))
	assert.Equal(t, "/chat/dm/dm%3Aagent%3Aa%2F1%3Auser%3Au%3A2", chatDMWebPath(dmKey))

	assert.Equal(t, starPermalink(projID, threadKey, msgID), inboxDecisionPromptHref(set, classifiedOpenPrompt{
		raw:         &store.ProjectDecisionPrompt{ProjectID: projID, ChatKey: threadKey, MessageID: msgID},
		promptAvail: true,
	}))
	assert.Equal(t, chatThreadWebPath(projID, threadKey), inboxDecisionPromptHref(set, classifiedOpenPrompt{
		raw:         &store.ProjectDecisionPrompt{ProjectID: projID, ChatKey: threadKey, MessageID: msgID},
		promptAvail: false,
	}))
	assert.Equal(t, "/chat/space/proj%2Fwith%20space/thread/topic:with%2Fslash", chatThreadWebPath(projID, threadKey))

	// Fallback when conversation is unavailable or projectID is missing on a thread.
	assert.Equal(t, "/profile/decisions?tab=pending", inboxDecisionPromptHref(set, classifiedOpenPrompt{
		raw:         &store.ProjectDecisionPrompt{ChatKey: "missing-key", MessageID: msgID},
		promptAvail: true,
	}))
	assert.Equal(t, "/profile/decisions?tab=pending", inboxDecisionPromptHref(set, classifiedOpenPrompt{
		raw:         &store.ProjectDecisionPrompt{ChatKey: threadKey, MessageID: msgID},
		promptAvail: true,
	}))
}
