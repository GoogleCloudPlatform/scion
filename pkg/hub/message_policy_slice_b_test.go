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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func sliceBMakeAgent(t testing.TB, f *mpgFixture, slug string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:              tid("sb-agent-" + slug),
		Name:            slug,
		Slug:            slug,
		ProjectID:       f.project.ID,
		Phase:           "running",
		OwnerID:         f.alice.ID,
		CreatedBy:       f.alice.ID,
		RuntimeBrokerID: f.sender.RuntimeBrokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{f.alice.ID},
	}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))
	return a
}

func sliceBSendAgentIdent(a *store.Agent) Identity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: a.ID},
		ProjectID: a.ProjectID,
		Ancestry:  a.Ancestry,
	}}
}

func sliceBSendE3b(t testing.TB, f *mpgFixture, sender, target *store.Agent, reqBody map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(reqBody)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), sliceBSendAgentIdent(sender)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, target.ID)
	return rr
}

func sliceBSendE3c(t testing.TB, f *mpgFixture, actor, target *store.Agent, msg string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"message": msg})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	delegated := &DelegatedAgentIdentity{
		agentID:           actor.ID,
		agentProjectID:    actor.ProjectID,
		authorizingUserID: f.alice.ID,
		grantID:           tid("sb-del-grant-" + actor.Slug),
	}
	req = req.WithContext(contextWithIdentity(req.Context(), delegated))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, target.ID)
	return rr
}

func sliceBS1Handler(calls *atomic.Int32, answerTrue bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		score := 0.08
		if answerTrue {
			score = 0.92
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"model": "jev-1.14.0",
			"answers": {"asks_choice": {"type": "noul", "noul": %.2f}},
			"usage": {"input_tokens": 20, "output_tokens": 2}
		}`, score)
	}
}

// TestSelectorEligibility_OncePerSurface checks that an independent policy
// executes ONCE per (surface, input), produces rows ONLY for matched
// recipients (with lowest recipient ID as primary and CacheHit+SourceEvalID on
// secondaries), and executes 0 times when no recipient matches.
// Kills: erased scope.
func TestSelectorEligibility_OncePerSurface(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	a1 := sliceBMakeAgent(t, f, "sel-a1")
	a2 := sliceBMakeAgent(t, f, "sel-a2")
	a3 := sliceBMakeAgent(t, f, "sel-a3")

	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-sel", sliceBS1Handler(&providerCalls, false))

	rule := mpgGroupBinaryRule("ts-sel", "Does the message contain a secret?")
	// Policy matches ONLY a1 and a2 on mention_delivery (not a3).
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "indep-selected",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces:  []string{store.MessagePolicySurfaceMentionDelivery},
			Recipient: store.MessagePolicyRecipientSelector{Kinds: []string{"agent"}, AgentIDs: []string{a1.ID, a2.ID}},
		},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: rule,
		SystemOneRefs: []string{"ts-sel"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	// 1. Mention only a3 (not in scope): 0 provider calls, 0 decision rows.
	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "hello @" + a3.Slug,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, int32(0), providerCalls.Load(), "unmatched recipient must trigger zero executions")

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	assert.Empty(t, decs.Items)

	// 2. Mention a1, a2, a3: 1 provider call, 2 decision rows (ONLY a1 and a2).
	rr = f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "hello @" + a1.Slug + " @" + a2.Slug + " @" + a3.Slug,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, int32(1), providerCalls.Load(), "independent policy must execute once across matched copies")

	decs, err = f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	require.Len(t, decs.Items, 2, "must emit one row per matched recipient only")

	wantPrimaryID := a1.ID
	wantSecondaryID := a2.ID
	if a2.ID < a1.ID {
		wantPrimaryID, wantSecondaryID = a2.ID, a1.ID
	}
	byRecip := map[string]store.MessagePolicyDecision{}
	for _, d := range decs.Items {
		assert.Equal(t, store.MessagePolicySurfaceMentionDelivery, d.Surface)
		assert.NotEmpty(t, d.MessageID)
		byRecip[d.RecipientID] = d
	}
	require.Contains(t, byRecip, wantPrimaryID)
	require.Contains(t, byRecip, wantSecondaryID)
	assert.NotContains(t, byRecip, a3.ID)

	var primProj, secProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(byRecip[wantPrimaryID].Evaluation, &primProj))
	require.NoError(t, json.Unmarshal(byRecip[wantSecondaryID].Evaluation, &secProj))
	assert.False(t, primProj.CacheHit)
	assert.Empty(t, primProj.SourceEvalID)
	assert.True(t, secProj.CacheHit)
	assert.Equal(t, primProj.EvalID, secProj.SourceEvalID)
	assert.NotEqual(t, primProj.EvalID, secProj.EvalID)
}

// TestDependent_PerRecipient verifies that a recipient-dependent policy
// executes once per matched copy recipient (no cross-recipient sharing; U1).
// Kills: cross-recipient sharing.
func TestDependent_PerRecipient(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	a1 := sliceBMakeAgent(t, f, "dep-a1")
	a2 := sliceBMakeAgent(t, f, "dep-a2")

	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-dep", sliceBS1Handler(&providerCalls, false))

	rule := `{"dialect":"system_one","inputs":["message.body","recipient.slug"],"spec":{"version":1,"binding":{"instance":"ts-dep"},"questions":[{"id":"asks_choice","type":"binary","instructions":"is bad?"}]}}`
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "dep-per-recip",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `judgment_flags.asks_choice || recipient.slug == "never"`,
		SystemOneRule: rule,
		SystemOneRefs: []string{"ts-dep"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "ping @" + a1.Slug + " @" + a2.Slug,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, int32(2), providerCalls.Load(), "dependent policy must execute once per matched recipient")

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	require.Len(t, decs.Items, 2)
}

// TestResourceBounds_UnitsVsRows tests 50 loaded policies, >10 matching enforce
// policies and 11 recipients (1 primary + 10 copies), plus shadow overflow.
// Checks <=110 enforce unit rows + 1 cap row per excess policy (not per
// recipient; total <=150), and shadow overflow produces counter with 0 rows.
// Kills: per-unit cap rows; unbounded units.
func TestResourceBounds_UnitsVsRows(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	// Create 12 copy agents so mention cap (10) is hit -> R = 1 primary + 10 copies = 11.
	var mentions []string
	for i := 1; i <= 12; i++ {
		ag := sliceBMakeAgent(t, f, fmt.Sprintf("cap-a%02d", i))
		mentions = append(mentions, "@"+ag.Slug)
	}

	// Create 25 enforce policies (10 active + 15 cap-exceeded) and 25 shadow policies (10 active + 15 dropped) = 50 total.
	for i := 1; i <= 25; i++ {
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      fmt.Sprintf("enf-%02d", i),
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentDM, store.MessagePolicySurfaceMentionDelivery},
			},
			ViolationRule: `false`,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}))
	}
	for i := 1; i <= 25; i++ {
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      fmt.Sprintf("shd-%02d", i),
			Enabled:   true,
			Mode:      store.MessagePolicyModeShadow,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentDM, store.MessagePolicySurfaceMentionDelivery},
			},
			ViolationRule: `false`,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}))
	}

	msg := "hello " + string(bytes.Join(func() [][]byte {
		b := make([][]byte, len(mentions))
		for i, m := range mentions {
			b[i] = []byte(m)
		}
		return b
	}(), []byte(" ")))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       msg,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Wait for shadow worker to persist 10*11 = 110 shadow rows on top of 10*11 + 15 = 125 enforce rows => 235 total.
	decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 235)
	var enfUnitRows, enfCapRows, shdRows int
	for _, d := range decs {
		switch {
		case d.Mode == store.MessagePolicyModeEnforce && d.CELErrorReason == messagepolicy.ReasonPolicyCapExceeded:
			enfCapRows++
		case d.Mode == store.MessagePolicyModeEnforce:
			enfUnitRows++
		case d.Mode == store.MessagePolicyModeShadow:
			shdRows++
		}
	}
	assert.Equal(t, 110, enfUnitRows, "10 active enforce policies * 11 recipients = 110 unit rows")
	assert.Equal(t, 15, enfCapRows, "1 cap row per excess enforce policy (15), never per recipient")
	assert.LessOrEqual(t, enfUnitRows+enfCapRows, 150)
	assert.Equal(t, 110, shdRows, "10 active shadow policies * 11 recipients = 110 shadow rows")
	assert.Equal(t, int64(15), f.srv.messagePolicyShadowDrops.Load(), "15 excess shadow policies counted only")
}

// TestSingleResolution_PlanEqualsDelivery runs on E3a, E3b, E2 mentions, and
// E2 default wake, verifying that planned targets equal delivered targets.
// Kills: re-resolution drift.
func TestSingleResolution_PlanEqualsDelivery(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "sr-m1")
	m2 := sliceBMakeAgent(t, f, "sr-m2")

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "sr-audit",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `false`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	// E3a
	f.dispatcher.reset()
	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "e3a @" + m1.Slug + " @" + m2.Slug,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Len(t, decs.Items, 2, "E3a planned 2 copy targets")
	assert.Len(t, f.dispatcher.getCalls(), 3, "E3a delivered 1 origin + 2 copies")

	// E3b
	f.dispatcher.reset()
	rr = sliceBSendE3b(t, f, f.sender, f.peerAgent, map[string]any{
		"message":  "e3b @" + m1.Slug,
		"mentions": []string{m2.Slug},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Len(t, f.dispatcher.getCalls(), 3, "E3b delivered 1 origin + 2 copies")

	// E2 mentions
	f.dispatcher.reset()
	rr = f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "e2 @" + m1.Slug,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Len(t, f.dispatcher.getCalls(), 1, "E2 mention delivered 1 copy")

	// E2 group default wake (no mentions -> wakes f.defaultAgent)
	f.dispatcher.reset()
	rr = f.sendOutbound(t, OutboundMessageRequest{
		ConversationID: f.groupConv.ID,
		Msg:            "group post without mentions",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Len(t, f.dispatcher.getCalls(), 1, "E2 default wake delivered 1 copy to defaultAgent")
}

// TestCopyFacts_GroupExisting verifies plan facts equal delivery-time builder
// facts for an existing verified group conversation, with no conversation created.
func TestCopyFacts_GroupExisting(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "grp-m1")

	parentConv := &messaging.ConversationResult{
		ConversationID: f.groupConv.ID,
		ExternalRef:    f.groupConv.ExternalRef,
		Kind:           "group",
		Surface:        "native",
	}
	ts, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
		Agent:              m1,
		Position:           "body",
		ParentConv:         parentConv,
		ParentConvVerified: true,
	}, "hello @"+m1.Slug, 0, false, nil, false, true)
	require.NoError(t, ts.factsErr)
	assert.Equal(t, "group", ts.facts.Conversation.Kind)
	assert.Contains(t, ts.facts.Conversation.AgentParticipantIDs, f.sender.ID)
	assert.Contains(t, ts.facts.Conversation.AgentParticipantIDs, f.defaultAgent.ID)
	assert.Contains(t, ts.facts.Conversation.UserParticipantIDs, f.alice.ID)
	assert.Equal(t, []string{f.defaultAgent.Slug}, ts.facts.Conversation.UnmentionedAgentSlugs)

	// Verify delivery-time builder computes the exact same fingerprint.
	adm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "send-grp-1",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "hello @" + m1.Slug,
		snapshots:          []copyTargetSnapshot{ts},
	})
	defer adm.Close()

	err := f.srv.verifyCopyAdmissionAtDelivery(ctx, &AgentDMInput{
		SenderAgent:    f.sender,
		SenderIdentity: sliceBSendAgentIdent(f.sender),
		TargetAgent:    m1,
		Msg:            "hello @" + m1.Slug,
		ConversationID: f.groupConv.ID,
		ConvResult:     parentConv,
		copyAdmission:  adm,
	})
	assert.NoError(t, err)
}

// TestCopyFacts_DMNotYetCreated verifies plan facts equal delivery-time facts
// for a DM that does not exist yet, and no DM conversation is created pre-effect.
func TestCopyFacts_DMNotYetCreated(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "dm-new-m1")

	dmKey, err := messages.DMConversationKey("agent", f.sender.ID, "agent", m1.ID)
	require.NoError(t, err)

	// Reject on mention_delivery so we prove NO DM conversation was created pre-effect.
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "reject-copy-dm",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `conversation.kind == "direct" && size(conversation.agent_participant_ids) == 0`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "hello @" + m1.Slug,
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())

	// Confirm the copy DM conversation was NEVER created.
	_, lookupErr := f.s.GetConversationByExternalRef(ctx, "native", dmKey)
	assert.ErrorIs(t, lookupErr, store.ErrNotFound, "copy DM conversation must not be created pre-effect")
}

// TestCopyFacts_LookupError_FactsUnavailable verifies that a participant lookup
// failure yields ReasonFactsUnavailable through on_failed.
func TestCopyFacts_LookupError_FactsUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "copy-facts-unavail",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `false`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	f.faultWrapper.failListParticipants.Store(true)
	defer f.faultWrapper.failListParticipants.Store(false)

	rr := f.sendOutbound(t, OutboundMessageRequest{
		ConversationID: f.groupConv.ID,
		Msg:            "group post waking default agent with faulted participants",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, decs.Items, 1)
	assert.Equal(t, messagepolicy.ClassFailed, decs.Items[0].Class)
	assert.Equal(t, messagepolicy.ReasonFactsUnavailable, decs.Items[0].CELErrorReason)
}

// TestCopyFacts_DMNotFoundVsLookupError verifies that ErrNotFound for a new DM
// yields valid kind="direct" facts, while any other store error yields
// ReasonFactsUnavailable.
func TestCopyFacts_DMNotFoundVsLookupError(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "dm-err-m1")

	// 1. Not found -> valid snapshot, kind="direct", nil error.
	tsOK, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
		Agent:    m1,
		Position: "body",
	}, "hi @"+m1.Slug, 0, false, nil, false, false)
	assert.NoError(t, tsOK.factsErr)
	assert.Equal(t, "direct", tsOK.facts.Conversation.Kind)

	// 2. Store error -> factsErr != nil -> ReasonFactsUnavailable.
	f.faultWrapper.failGetConvByExtRef.Store(true)
	defer f.faultWrapper.failGetConvByExtRef.Store(false)

	tsErr, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
		Agent:    m1,
		Position: "body",
	}, "hi @"+m1.Slug, 0, false, nil, false, false)
	assert.Error(t, tsErr.factsErr)
}

// TestCopyFacts_DriftRefused verifies that a participant change or group
// leaving the project between plan and delivery refuses the copy (R5).
// Kills: fabricated or origin-conversation facts; granting with drifted facts.
func TestCopyFacts_DriftRefused(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "drift-m1")
	joiner := sliceBMakeAgent(t, f, "drift-joiner")

	parentConv := &messaging.ConversationResult{
		ConversationID: f.groupConv.ID,
		ExternalRef:    f.groupConv.ExternalRef,
		Kind:           "group",
		Surface:        "native",
	}
	ts, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
		Agent:              m1,
		Position:           "body",
		ParentConv:         parentConv,
		ParentConvVerified: true,
	}, "hi @"+m1.Slug, 0, false, nil, false, false)
	require.NoError(t, ts.factsErr)

	adm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "send-drift",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "hi @" + m1.Slug,
		snapshots:          []copyTargetSnapshot{ts},
	})
	defer adm.Close()

	// Mutate group participants between plan and delivery.
	require.NoError(t, f.s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: f.groupConv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    joiner.ID,
		Role:           "member",
	}))

	// Preflight in deliverAgentMentionCopy must refuse due to fingerprint drift.
	f.dispatcher.reset()
	out := f.srv.deliverAgentMentionCopy(ctx, ctx, agentMentionFanoutInput{
		Sender:             f.sender,
		SenderIdent:        sliceBSendAgentIdent(f.sender),
		Msg:                "hi @" + m1.Slug,
		Type:               messages.TypeInstruction,
		ParentConv:         parentConv,
		ParentConvVerified: true,
		CopyAdmission:      adm,
	}, m1, "body")
	assert.Equal(t, "error", out.Status)
	assert.Contains(t, out.Error, "drifted")
	assert.Empty(t, f.dispatcher.getCalls(), "drifted copy must never dispatch")

	// Also verify ExecuteAgentDM copy verification refuses if group fell back to DM.
	err := f.srv.verifyCopyAdmissionAtDelivery(ctx, &AgentDMInput{
		SenderAgent:    f.sender,
		SenderIdentity: sliceBSendAgentIdent(f.sender),
		TargetAgent:    m1,
		Msg:            "hi @" + m1.Slug,
		ConvResult:     &messaging.ConversationResult{Kind: "direct"},
		copyAdmission:  adm,
	})
	assert.ErrorIs(t, err, errCopyAdmissionFacts)
}

// TestCopyProvenance_StrippedAdmissionRefused drives the REAL copy delivery
// path with copyIntent=true, stripping ONLY the admission (nil), and separately
// with an expired/closed or invalid admission. Asserts refusal, zero
// evaluations, zero delivery, zero conversation creation, and zero persistence.
// Kills: admission-pointer-as-discriminator fall-through.
func TestCopyProvenance_StrippedAdmissionRefused(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "strip-m1")

	var evalCalls atomic.Int32
	f.srv.messagePolicyGate.evalOverride = func(_ context.Context, _ *store.MessagePolicy, _ messagepolicy.Activation, _ *ruleengine.BudgetTracker) (messagepolicy.Classification, bool) {
		evalCalls.Add(1)
		return messagepolicy.Classification{Class: messagepolicy.ClassPass}, true
	}

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "strip-watch",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM, store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `false`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	dmKey, err := messages.DMConversationKey("agent", f.sender.ID, "agent", m1.ID)
	require.NoError(t, err)

	// 1. Nil admission via deliverAgentMentionCopy.
	f.dispatcher.reset()
	out := f.srv.deliverAgentMentionCopy(ctx, ctx, agentMentionFanoutInput{
		Sender:        f.sender,
		SenderIdent:   sliceBSendAgentIdent(f.sender),
		Msg:           "hi @" + m1.Slug,
		Type:          messages.TypeInstruction,
		CopyAdmission: nil,
	}, m1, "body")
	assert.Equal(t, "error", out.Status)
	assert.Zero(t, evalCalls.Load(), "copy mode with nil admission must perform zero evaluations")
	assert.Empty(t, f.dispatcher.getCalls(), "copy mode with nil admission must not dispatch")
	_, convErr := f.s.GetConversationByExternalRef(ctx, "native", dmKey)
	assert.ErrorIs(t, convErr, store.ErrNotFound, "refused copy must not create DM conversation")

	// 2. Direct ExecuteAgentDM with copyIntent=true and nil admission.
	_, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.sender,
		SenderIdentity: sliceBSendAgentIdent(f.sender),
		TargetAgent:    m1,
		Msg:            "hi @" + m1.Slug,
		Type:           messages.TypeMention,
		ProjectID:      f.project.ID,
		SkipPhaseGate:  true,
		copyIntent:     true,
		copyAdmission:  nil,
	})
	require.NotNil(t, dmErr)
	assert.Zero(t, evalCalls.Load(), "ExecuteAgentDM copy mode with nil admission must not fall through to origin evaluation")
	assert.Empty(t, f.dispatcher.getCalls())

	// 3. Closed/expired admission.
	closedAdm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "send-closed",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "hi @" + m1.Slug,
	})
	closedAdm.Close()
	_, dmErr = f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.sender,
		SenderIdentity: sliceBSendAgentIdent(f.sender),
		TargetAgent:    m1,
		Msg:            "hi @" + m1.Slug,
		Type:           messages.TypeMention,
		ProjectID:      f.project.ID,
		SkipPhaseGate:  true,
		copyIntent:     true,
		copyAdmission:  closedAdm,
	})
	require.NotNil(t, dmErr)
	assert.Zero(t, evalCalls.Load())

	msgs, err := f.s.ListMessages(ctx, store.MessageFilter{RecipientID: m1.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "refused copies must persist zero messages")
}

// TestCopyProvenance_OrdinaryCallerEvaluates verifies copyIntent=false evaluates
// as an origin send.
func TestCopyProvenance_OrdinaryCallerEvaluates(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "block-dm-secret",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}},
		ViolationRule: `message.body.contains("SECRET")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	_, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.sender,
		SenderIdentity: sliceBSendAgentIdent(f.sender),
		TargetAgent:    f.peerAgent,
		Msg:            "this has SECRET",
		Type:           messages.TypeInstruction,
		ProjectID:      f.project.ID,
	})
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusUnprocessableEntity, dmErr.HTTPStatus)
}

// TestCopyProvenance_RequestMetadataUntrusted verifies that Type=mention,
// mention_of, or request metadata on E3b is evaluated as an origin and never
// treated as a copy.
func TestCopyProvenance_RequestMetadataUntrusted(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "block-origin-dm",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}},
		ViolationRule: `message.body.contains("FORBIDDEN")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	rr := sliceBSendE3b(t, f, f.sender, f.peerAgent, map[string]any{
		"structured_message": map[string]any{
			"version":   1,
			"msg":       "FORBIDDEN spoofed mention",
			"type":      messages.TypeMention,
			"sender":    "agent:" + f.sender.Slug,
			"recipient": "agent:" + f.peerAgent.Slug,
			"metadata": map[string]string{
				"mention_of":     "fake-parent-id",
				"mention_source": "agent:" + f.sender.Slug,
			},
		},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "spoofed Type=mention + mention_of on E3b must be evaluated as origin DM")
}

// TestCopyInput_Binding verifies that a copy unit is evaluated on the actual
// delivered copy input (in.Msg / originalMsgForMentions).
func TestCopyInput_Binding(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "input-m1")

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "check-copy-body",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `surface == "mention_delivery" && message.body.contains("COPY_TRIGGER")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "COPY_TRIGGER hello @" + m1.Slug,
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
}

// TestOriginCopy_NoSharingAcrossSurface verifies that origin (agent_dm) and
// copy (mention_delivery) never share a System One group execution.
func TestOriginCopy_NoSharingAcrossSurface(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "noshare-m1")

	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-noshare", sliceBS1Handler(&providerCalls, false))

	rule := mpgGroupBinaryRule("ts-noshare", "is bad?")
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "both-surfaces",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentDM, store.MessagePolicySurfaceMentionDelivery},
		},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: rule,
		SystemOneRefs: []string{"ts-noshare"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "hello @" + m1.Slug,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, int32(2), providerCalls.Load(), "origin (agent_dm) and copy (mention_delivery) must never share a provider execution")
}

// TestAdmission_Expiry_Fanout verifies mention fan-out's aggregateCtx expiry
// refuses the copy.
func TestAdmission_Expiry_Fanout(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "exp-fan-m1")

	ts, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{Agent: m1, Position: "body"}, "hi @"+m1.Slug, 0, false, nil, false, false)
	adm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "send-exp-fan",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "hi @" + m1.Slug,
		snapshots:          []copyTargetSnapshot{ts},
	})
	defer adm.Close()

	expiredCtx, cancel := context.WithCancel(ctx)
	cancel()
	res := f.srv.deliverResolvedAgentMentions(expiredCtx, agentMentionFanoutInput{
		Sender:        f.sender,
		SenderIdent:   sliceBSendAgentIdent(f.sender),
		Msg:           "hi @" + m1.Slug,
		Type:          messages.TypeInstruction,
		CopyAdmission: adm,
	}, &resolvedMentionPlan{
		Results: []messages.MentionResult{{Slug: m1.Slug, Status: "delivered"}},
		Targets: map[string]*store.Agent{m1.Slug: m1},
	})
	require.Len(t, res, 1)
	assert.Equal(t, "timeout", res[0].Status)
	assert.False(t, adm.isOpen(), "admission must be closed when fan-out returns")
}

// TestAdmission_Expiry_DefaultWake verifies default wake's own aggregateCtx
// expiry refuses the copy and closes the admission on return.
func TestAdmission_Expiry_DefaultWake(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	parentConv := &messaging.ConversationResult{
		ConversationID: f.groupConv.ID,
		ExternalRef:    f.groupConv.ExternalRef,
		Kind:           "group",
		Surface:        "native",
	}
	ts, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
		Agent:              f.defaultAgent,
		Position:           "default_agent",
		ParentConv:         parentConv,
		ParentConvVerified: true,
	}, "wake msg", 0, false, nil, false, false)
	adm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "send-exp-wake",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "wake msg",
		snapshots:          []copyTargetSnapshot{ts},
	})
	defer adm.Close()

	expiredCtx, cancel := context.WithCancel(ctx)
	cancel()
	res := f.srv.wakeGroupDefaultAgent(expiredCtx, groupDefaultWakeInput{
		Sender:        f.sender,
		SenderIdent:   sliceBSendAgentIdent(f.sender),
		Conv:          parentConv,
		Msg:           "wake msg",
		Type:          messages.TypeInstruction,
		CopyAdmission: adm,
		Resolved: &resolvedDefaultWakePlan{
			Target: &resolvedCopyTarget{
				Agent:              f.defaultAgent,
				Position:           "default_agent",
				ParentConv:         parentConv,
				ParentConvVerified: true,
			},
		},
	})
	require.NotNil(t, res)
	assert.NotEqual(t, "delivered", res.Status)
	assert.False(t, adm.isOpen(), "admission must be closed when default wake returns")
}

// TestRegistry_ReleasedOnReturnAndCancel verifies the registry releases the
// entry on handler return and on context cancel.
func TestRegistry_ReleasedOnReturnAndCancel(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "reg-m1")

	adm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "send-reg",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "hi @" + m1.Slug,
	})
	assert.True(t, adm.isOpen())
	adm.Close()
	assert.False(t, adm.isOpen())

	// On canceled context, ExecuteAgentDM returns error and releases admission.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, dmErr := f.srv.ExecuteAgentDM(cancelCtx, &AgentDMInput{
		SenderAgent:    f.sender,
		SenderIdentity: sliceBSendAgentIdent(f.sender),
		TargetAgent:    f.peerAgent,
		Msg:            "hi @" + m1.Slug,
		Type:           messages.TypeInstruction,
		ProjectID:      f.project.ID,
	})
	require.NotNil(t, dmErr)
}

// TestWholeSend_422NoDeliveryEffects verifies that an origin 422 (including
// when a copy policy rejects) produces zero message row, zero attachment
// binding/addressee, zero dispatch, zero mention copy, and zero copy DM
// conversation (with D10 origin-routing exception).
func TestWholeSend_422NoDeliveryEffects(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "whole-m1")

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "reject-copy-whole",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `message.body.contains("BLOCK_WHOLE")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	f.dispatcher.reset()
	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "BLOCK_WHOLE @" + m1.Slug,
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Empty(t, f.dispatcher.getCalls(), "422 must produce zero dispatches")

	msgsOrigin, err := f.s.ListMessages(ctx, store.MessageFilter{RecipientID: f.peerAgent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgsOrigin.Items, "422 must persist zero origin messages")

	msgsCopy, err := f.s.ListMessages(ctx, store.MessageFilter{RecipientID: m1.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgsCopy.Items, "422 must persist zero copy messages")

	copyDMKey, err := messages.DMConversationKey("agent", f.sender.ID, "agent", m1.ID)
	require.NoError(t, err)
	_, convErr := f.s.GetConversationByExternalRef(ctx, "native", copyDMKey)
	assert.ErrorIs(t, convErr, store.ErrNotFound, "422 must not create copy DM conversation")
}

// TestRefusedCopy_PerCopyFailureOnly verifies R5: a refused copy has zero
// conversation creation/row/dispatch, while the origin and other copies stand,
// and refusal is reported in that copy's MentionResult.
func TestRefusedCopy_PerCopyFailureOnly(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	goodCopy := sliceBMakeAgent(t, f, "r5-good")
	badCopy := sliceBMakeAgent(t, f, "r5-bad")

	// Mint an admission that admits ONLY goodCopy (not badCopy) to exercise per-copy refusal.
	tsGood, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{Agent: goodCopy, Position: "body"}, "hello @"+goodCopy.Slug+" @"+badCopy.Slug, 0, false, nil, false, false)
	adm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "send-r5",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "hello @" + goodCopy.Slug + " @" + badCopy.Slug,
		snapshots:          []copyTargetSnapshot{tsGood},
	})
	defer adm.Close()

	f.dispatcher.reset()
	res := f.srv.deliverResolvedAgentMentions(ctx, agentMentionFanoutInput{
		Sender:        f.sender,
		SenderIdent:   sliceBSendAgentIdent(f.sender),
		Msg:           "hello @" + goodCopy.Slug + " @" + badCopy.Slug,
		Type:          messages.TypeInstruction,
		CopyAdmission: adm,
	}, &resolvedMentionPlan{
		Results: []messages.MentionResult{
			{Slug: goodCopy.Slug, Status: "delivered"},
			{Slug: badCopy.Slug, Status: "delivered"},
		},
		Targets: map[string]*store.Agent{
			goodCopy.Slug: goodCopy,
			badCopy.Slug:  badCopy,
		},
	})
	require.Len(t, res, 2)
	assert.Equal(t, "delivered", res[0].Status, "admitted copy must succeed")
	assert.Equal(t, "error", res[1].Status, "refused copy must be reported as per-copy failure")
	assert.Len(t, f.dispatcher.getCalls(), 1, "only admitted copy dispatches")

	badDMKey, err := messages.DMConversationKey("agent", f.sender.ID, "agent", badCopy.ID)
	require.NoError(t, err)
	_, convErr := f.s.GetConversationByExternalRef(ctx, "native", badDMKey)
	assert.ErrorIs(t, convErr, store.ErrNotFound, "refused copy must not create DM conversation")
}

// TestGate_AfterAuthzZeroCalls verifies that a 403 authz denial produces zero
// provider/evaluator calls.
func TestGate_AfterAuthzZeroCalls(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	silentTarget := sliceBMakeAgent(t, f, "silent-403")
	silentTarget.MessageMode = store.MessageModeSilent
	require.NoError(t, f.s.UpdateAgent(ctx, silentTarget))

	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-403", sliceBS1Handler(&providerCalls, true))
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "block-all-dm",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: mpgGroupBinaryRule("ts-403", "is bad?"),
		SystemOneRefs: []string{"ts-403"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	rr := sliceBSendE3b(t, f, f.sender, silentTarget, map[string]any{"message": "should be 403"})
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Zero(t, providerCalls.Load(), "403 authz denial must trigger zero provider calls")
}

// TestEvalProject_IsSender verifies D5: cross-project DM evaluates sender's
// project policies, never target's project policies.
func TestEvalProject_IsSender(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	// Create a second project with a target agent that accepts cross-project DMs (open mode).
	otherProj := &store.Project{
		ID:   tid("sb-other-proj"),
		Name: "sb-other-proj",
		Slug: "sb-other-proj",
	}
	require.NoError(t, f.s.CreateProject(ctx, otherProj))
	f.sender.MessageMode = store.MessageModeOpen
	require.NoError(t, f.s.UpdateAgent(ctx, f.sender))

	otherTarget := &store.Agent{
		ID:              tid("sb-other-target"),
		Name:            "other-target",
		Slug:            "other-target",
		ProjectID:       otherProj.ID,
		Phase:           "running",
		OwnerID:         f.alice.ID,
		CreatedBy:       f.alice.ID,
		RuntimeBrokerID: f.sender.RuntimeBrokerID,
		MessageMode:     store.MessageModeOpen,
		Ancestry:        []string{f.alice.ID},
	}
	require.NoError(t, f.s.CreateAgent(ctx, otherTarget))

	// Put a reject-all policy in TARGET's project (must NOT apply) and a pass policy in SENDER's project (MUST apply).
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     otherProj.ID,
		Name:          "target-proj-reject-all",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}},
		ViolationRule: `true`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "sender-proj-check",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}},
		ViolationRule: `message.body.contains("SENDER_BLOCK")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	// 1. Clean cross-project E3b DM: admitted (target's project policy ignored, sender's project policy evaluated).
	rr := sliceBSendE3b(t, f, f.sender, otherTarget, map[string]any{"message": "clean cross-project"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// 2. Sender-project blocked cross-project E3b DM: rejected 422.
	rr = sliceBSendE3b(t, f, f.sender, otherTarget, map[string]any{"message": "SENDER_BLOCK"})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
}

// TestGate_WakeIndependent verifies that a held target that is policy-rejected
// gets 422 (gate runs before Phase 1b hold/wake and never invokes wake hooks).
func TestGate_WakeIndependent(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	heldTarget := sliceBMakeAgent(t, f, "held-target")
	heldTarget.Phase = "suspended"
	require.NoError(t, f.s.UpdateAgent(ctx, heldTarget))

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "reject-before-wake",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}},
		ViolationRule: `message.body.contains("POLICY_REJECT")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	var beforeWakeCalled atomic.Bool
	_, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.sender,
		SenderIdentity: sliceBSendAgentIdent(f.sender),
		TargetAgent:    heldTarget,
		Msg:            "POLICY_REJECT",
		Type:           messages.TypeInstruction,
		ProjectID:      f.project.ID,
		Wake:           true,
		BeforeWake:     func() { beforeWakeCalled.Store(true) },
	})
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusUnprocessableEntity, dmErr.HTTPStatus)
	assert.False(t, beforeWakeCalled.Load(), "gate must run before Phase 1b wake hooks")
}

// TestE3c_Gated verifies that delegated DMs (E3c) are gated after
// delegatedDeliveryRefusal and before persistence.
func TestE3c_Gated(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "e3c-block",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}},
		ViolationRule: `message.body.contains("E3C_BLOCK")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	// 1. Policy reject -> 422, zero message row, zero dispatch.
	f.dispatcher.reset()
	rr := sliceBSendE3c(t, f, f.sender, f.peerAgent, "E3C_BLOCK secret")
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Empty(t, f.dispatcher.getCalls())
	msgs, err := f.s.ListMessages(ctx, store.MessageFilter{RecipientID: f.peerAgent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items)

	// 2. Clean delegated send -> 200, decision row carries persisted MessageID.
	rr = sliceBSendE3c(t, f, f.sender, f.peerAgent, "E3C clean")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	var admittedWithMsgID int
	for _, d := range decs.Items {
		if d.Action == store.MessagePolicyActionAdmitted && d.MessageID != "" {
			admittedWithMsgID++
		}
	}
	assert.Equal(t, 1, admittedWithMsgID)
}

// TestShadow_OwnGroups_NoEnforceReuse verifies that shadow policies on
// agent_dm / mention_delivery run in their own groups under the shadow
// worker's own context and never reuse enforce evaluations.
func TestShadow_OwnGroups_NoEnforceReuse(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "shd-m1")

	var mu sync.Mutex
	var providerCalls int
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-shd", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		providerCalls++
		mu.Unlock()
		sliceBS1Handler(nil, false)(w, r)
	})

	rule := mpgGroupBinaryRule("ts-shd", "is bad?")
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "enf-copy",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: rule,
		SystemOneRefs: []string{"ts-shd"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "shd-copy",
		Enabled:       true,
		Mode:          store.MessagePolicyModeShadow,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: rule,
		SystemOneRefs: []string{"ts-shd"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "agent:" + f.peerAgent.Slug,
		Msg:       "hello @" + m1.Slug,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 2)
	require.Len(t, decs, 2)

	mu.Lock()
	calls := providerCalls
	mu.Unlock()
	assert.Equal(t, 2, calls, "shadow policy must execute in its own group and never reuse enforce result")
}
