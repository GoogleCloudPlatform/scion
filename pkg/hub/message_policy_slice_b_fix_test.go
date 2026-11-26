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
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestR1_GroupCopyNormalization exercises the R1 participant normalization
// invariant (P0 ⊆ L ⊆ P0 ∪ R) across E2 and E3b group sends, with and without
// enforce policies reading conversation.agent_participant_ids and
// conversation.unmentioned_agent_slugs, plus strict drift refusal cases.
func TestR1_GroupCopyNormalization(t *testing.T) {
	ctx := context.Background()

	runE2GroupMultiMention := func(t *testing.T, withPolicies bool, mentionCount int) {
		t.Helper()
		f := newMpgFixture(t, true)
		var slugs []string
		for i := 1; i <= mentionCount; i++ {
			ag := sliceBMakeAgent(t, f, "r1-e2-m"+string(rune('0'+i)))
			slugs = append(slugs, "@"+ag.Slug)
		}

		if withPolicies {
			mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-r1-e2", sliceBS1Handler(nil, false))
			require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
				ProjectID:     f.project.ID,
				Name:          "r1-e2-participants",
				Enabled:       true,
				Mode:          store.MessagePolicyModeEnforce,
				Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
				ViolationRule: `size(conversation.agent_participant_ids) == 999`,
				OnFailed:      store.MessagePolicyOutcomeReject,
			}))
			require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
				ProjectID:     f.project.ID,
				Name:          "r1-e2-unmentioned",
				Enabled:       true,
				Mode:          store.MessagePolicyModeEnforce,
				Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
				ViolationRule: `judgment_flags.asks_choice`,
				SystemOneRule: `{"dialect":"system_one","inputs":["message.body","conversation.unmentioned_agent_slugs"],"spec":{"version":1,"binding":{"instance":"ts-r1-e2"},"questions":[{"id":"asks_choice","type":"binary","instructions":"is bad?"}]}}`,
				SystemOneRefs: []string{"ts-r1-e2"},
				OnUncertain:   store.MessagePolicyOutcomeReject,
				OnFailed:      store.MessagePolicyOutcomeReject,
			}))
		}

		sliceBResetDispatcher(f.dispatcher)
		rr := f.sendOutbound(t, OutboundMessageRequest{
			ConversationRef: "conv:" + f.groupConv.ID,
			Msg:             "group update " + strings.Join(slugs, " "),
			Type:            messages.TypeInstruction,
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var resp struct {
			MentionResults []messages.MentionResult `json:"mention_results"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		require.Len(t, resp.MentionResults, mentionCount)
		for _, mr := range resp.MentionResults {
			assert.Equal(t, "delivered", mr.Status, "mention %s must be delivered (err=%q)", mr.Slug, mr.Error)
		}
		assert.Len(t, f.dispatcher.getCalls(), mentionCount)
	}

	t.Run("E2_group_2_mentions_no_policy", func(t *testing.T) { runE2GroupMultiMention(t, false, 2) })
	t.Run("E2_group_3_mentions_no_policy", func(t *testing.T) { runE2GroupMultiMention(t, false, 3) })
	t.Run("E2_group_2_mentions_with_policies", func(t *testing.T) { runE2GroupMultiMention(t, true, 2) })
	t.Run("E2_group_3_mentions_with_policies", func(t *testing.T) { runE2GroupMultiMention(t, true, 3) })

	runE3bGroupPrimaryPlusMentions := func(t *testing.T, withPolicies bool) {
		t.Helper()
		f := newMpgFixture(t, true)
		// f.peerAgent is NOT a participant of f.groupConv; m1 and m2 are NOT participants either.
		m1 := sliceBMakeAgent(t, f, "r1-e3b-m1")
		m2 := sliceBMakeAgent(t, f, "r1-e3b-m2")

		if withPolicies {
			mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-r1-e3b", sliceBS1Handler(nil, false))
			require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
				ProjectID:     f.project.ID,
				Name:          "r1-e3b-participants",
				Enabled:       true,
				Mode:          store.MessagePolicyModeEnforce,
				Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
				ViolationRule: `size(conversation.agent_participant_ids) == 999`,
				OnFailed:      store.MessagePolicyOutcomeReject,
			}))
			require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
				ProjectID:     f.project.ID,
				Name:          "r1-e3b-unmentioned",
				Enabled:       true,
				Mode:          store.MessagePolicyModeEnforce,
				Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
				ViolationRule: `judgment_flags.asks_choice`,
				SystemOneRule: `{"dialect":"system_one","inputs":["message.body","conversation.unmentioned_agent_slugs"],"spec":{"version":1,"binding":{"instance":"ts-r1-e3b"},"questions":[{"id":"asks_choice","type":"binary","instructions":"is bad?"}]}}`,
				SystemOneRefs: []string{"ts-r1-e3b"},
				OnUncertain:   store.MessagePolicyOutcomeReject,
				OnFailed:      store.MessagePolicyOutcomeReject,
			}))
		}

		sliceBResetDispatcher(f.dispatcher)
		rr := sliceBSendE3b(t, f, f.sender, f.peerAgent, map[string]any{
			"structured_message": map[string]any{
				"version":         1,
				"msg":             "thread msg @" + m1.Slug + " @" + m2.Slug,
				"type":            messages.TypeInstruction,
				"conversation_id": f.groupConv.ID,
			},
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var resp MessageDeliveryResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		require.Len(t, resp.MentionResults, 2)
		for _, mr := range resp.MentionResults {
			assert.Equal(t, "delivered", mr.Status, "E3b group mention %s must be delivered (err=%q)", mr.Slug, mr.Error)
		}
		assert.Len(t, f.dispatcher.getCalls(), 3, "primary + 2 mentions must all be dispatched")
	}

	t.Run("E3b_group_nonparticipant_primary_plus_2_mentions_no_policy", func(t *testing.T) {
		runE3bGroupPrimaryPlusMentions(t, false)
	})
	t.Run("E3b_group_nonparticipant_primary_plus_2_mentions_with_policies", func(t *testing.T) {
		runE3bGroupPrimaryPlusMentions(t, true)
	})

	// Strict drift refusal cases:
	t.Run("preexisting_target_removed_refused", func(t *testing.T) {
		f := newMpgFixture(t, true)
		// f.defaultAgent is already in P0 of f.groupConv.
		parentConv := &messaging.ConversationResult{
			ConversationID: f.groupConv.ID,
			ExternalRef:    f.groupConv.ExternalRef,
			Kind:           "group",
			Surface:        "native",
		}
		ts, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
			Agent:              f.defaultAgent,
			Position:           "body",
			ParentConv:         parentConv,
			ParentConvVerified: true,
		}, "hi @"+f.defaultAgent.Slug, 0, false, nil, false, false)
		require.NoError(t, ts.factsErr)

		adm := mintCopyAdmission(copyAdmissionMintInput{
			sendID:             "r1-rm-target",
			senderAgentID:      f.sender.ID,
			senderIdentityKind: "agent",
			evalProjectID:      f.project.ID,
			copyBody:           "hi @" + f.defaultAgent.Slug,
			snapshots:          []copyTargetSnapshot{ts},
		})
		defer adm.Close()

		// Remove pre-existing target from the group before its copy verifies.
		require.NoError(t, f.s.RemoveParticipant(ctx, f.groupConv.ID, "agent", f.defaultAgent.ID))

		sliceBResetDispatcher(f.dispatcher)
		out := f.srv.deliverAgentMentionCopy(ctx, ctx, agentMentionFanoutInput{
			Sender:             f.sender,
			SenderIdent:        sliceBSendAgentIdent(f.sender),
			Msg:                "hi @" + f.defaultAgent.Slug,
			Type:               messages.TypeInstruction,
			ParentConv:         parentConv,
			ParentConvVerified: true,
			CopyAdmission:      adm,
		}, f.defaultAgent, "body")
		assert.Equal(t, "error", out.Status)
		assert.Contains(t, out.Error, "drifted")
		assert.Empty(t, f.dispatcher.getCalls())
	})

	t.Run("third_party_add_of_unacknowledged_target_refused", func(t *testing.T) {
		f := newMpgFixture(t, true)
		m1 := sliceBMakeAgent(t, f, "r1-unack-m1")
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
			sendID:             "r1-unack",
			senderAgentID:      f.sender.ID,
			senderIdentityKind: "agent",
			evalProjectID:      f.project.ID,
			copyBody:           "hi @" + m1.Slug,
			snapshots:          []copyTargetSnapshot{ts},
		})
		defer adm.Close()

		// Third party adds m1 BEFORE this send has delivered/acknowledged m1 (m1 not in R).
		require.NoError(t, f.s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: f.groupConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    m1.ID,
			Role:           "member",
		}))

		sliceBResetDispatcher(f.dispatcher)
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
		assert.Empty(t, f.dispatcher.getCalls())
	})

	t.Run("unrelated_third_party_add_and_remove_refused", func(t *testing.T) {
		f := newMpgFixture(t, true)
		m1 := sliceBMakeAgent(t, f, "r1-rel-m1")
		outsider := sliceBMakeAgent(t, f, "r1-outsider")
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
			sendID:             "r1-unrelated",
			senderAgentID:      f.sender.ID,
			senderIdentityKind: "agent",
			evalProjectID:      f.project.ID,
			copyBody:           "hi @" + m1.Slug,
			snapshots:          []copyTargetSnapshot{ts},
		})
		defer adm.Close()

		// Unrelated add -> refused.
		require.NoError(t, f.s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: f.groupConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    outsider.ID,
			Role:           "member",
		}))
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

		// Undo add, now remove an unrelated baseline participant (defaultAgent) -> refused.
		require.NoError(t, f.s.RemoveParticipant(ctx, f.groupConv.ID, "agent", outsider.ID))
		require.NoError(t, f.s.RemoveParticipant(ctx, f.groupConv.ID, "agent", f.defaultAgent.ID))
		out = f.srv.deliverAgentMentionCopy(ctx, ctx, agentMentionFanoutInput{
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
	})

	t.Run("accepted_same_id_race_after_acknowledgment_delivered", func(t *testing.T) {
		// Documented R1 limitation: once m1's ID is in R (acknowledged), a
		// subsequent copy (m2) tolerates m1's presence (fact-equivalent).
		f := newMpgFixture(t, true)
		m1 := sliceBMakeAgent(t, f, "r1-race-m1")
		m2 := sliceBMakeAgent(t, f, "r1-race-m2")
		parentConv := &messaging.ConversationResult{
			ConversationID: f.groupConv.ID,
			ExternalRef:    f.groupConv.ExternalRef,
			Kind:           "group",
			Surface:        "native",
		}
		ts1, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
			Agent: m1, Position: "body", ParentConv: parentConv, ParentConvVerified: true,
		}, "hi @"+m1.Slug+" @"+m2.Slug, 0, false, nil, false, false)
		ts2, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{
			Agent: m2, Position: "body", ParentConv: parentConv, ParentConvVerified: true,
		}, "hi @"+m1.Slug+" @"+m2.Slug, 0, false, nil, false, false)

		adm := mintCopyAdmission(copyAdmissionMintInput{
			sendID:             "r1-race",
			senderAgentID:      f.sender.ID,
			senderIdentityKind: "agent",
			evalProjectID:      f.project.ID,
			copyBody:           "hi @" + m1.Slug + " @" + m2.Slug,
			snapshots:          []copyTargetSnapshot{ts1, ts2},
		})
		defer adm.Close()

		// Simulate m1 added and acknowledged in R.
		require.NoError(t, f.s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: f.groupConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    m1.ID,
			Role:           "member",
		}))
		adm.RecordAcknowledgedRegistration(m1.ID)

		sliceBResetDispatcher(f.dispatcher)
		out := f.srv.deliverAgentMentionCopy(ctx, ctx, agentMentionFanoutInput{
			Sender:             f.sender,
			SenderIdent:        sliceBSendAgentIdent(f.sender),
			Msg:                "hi @" + m1.Slug + " @" + m2.Slug,
			Type:               messages.TypeInstruction,
			ParentConv:         parentConv,
			ParentConvVerified: true,
			CopyAdmission:      adm,
		}, m2, "body")
		assert.Equal(t, "delivered", out.Status, "acknowledged ID in R must be tolerated for later copy (err=%q)", out.Error)
		assert.Len(t, f.dispatcher.getCalls(), 1)
	})
}

// TestR2_ShadowOnlyUnmentionedFactsIsolation verifies that:
//  1. A shadow policy reading conversation.unmentioned_agent_slugs receives the
//     real non-empty list even when enforce policies do not read unmentioned
//     slugs (both origin and copy units).
//  2. A shadow-only lookup failure yields facts_unavailable ONLY on shadow rows,
//     leaving enforce outcomes and copy delivery completely unaffected.
func TestR2_ShadowOnlyUnmentionedFactsIsolation(t *testing.T) {
	ctx := context.Background()

	t.Run("shadow_receives_real_unmentioned_slugs_origin_and_copy", func(t *testing.T) {
		f := newMpgFixture(t, true)
		m1 := sliceBMakeAgent(t, f, "r2-m1")

		var (
			mu           sync.Mutex
			seenPayloads []string
		)
		mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-r2", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			seenPayloads = append(seenPayloads, string(raw))
			mu.Unlock()
			sliceBS1Handler(nil, false)(w, r)
		})

		// Enforce policy does NOT read unmentioned_agent_slugs.
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "r2-enf-no-unmentioned",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToConversation, store.MessagePolicySurfaceMentionDelivery}},
			ViolationRule: `false`,
			OnFailed:      store.MessagePolicyOutcomeReject,
		}))
		// Shadow policy DOES read conversation.unmentioned_agent_slugs on both origin and copy.
		s1Unm := `{"dialect":"system_one","inputs":["message.body","conversation.unmentioned_agent_slugs"],"spec":{"version":1,"binding":{"instance":"ts-r2"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Check unmentioned."}]}}`
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "r2-shd-unmentioned",
			Enabled:       true,
			Mode:          store.MessagePolicyModeShadow,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToConversation, store.MessagePolicySurfaceMentionDelivery}},
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: s1Unm,
			SystemOneRefs: []string{"ts-r2"},
			OnUncertain:   store.MessagePolicyOutcomeAdmit,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}))

		rr := f.sendOutbound(t, OutboundMessageRequest{
			ConversationRef: "conv:" + f.groupConv.ID,
			Msg:             "hello @" + m1.Slug,
			Type:            messages.TypeInstruction,
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		// 2 enforce rows + 2 shadow rows = 4 decisions.
		decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 4)
		require.Len(t, decs, 4)

		mu.Lock()
		payloads := append([]string(nil), seenPayloads...)
		mu.Unlock()
		require.Len(t, payloads, 2, "shadow must evaluate both origin and copy System One rules")
		for _, p := range payloads {
			assert.Contains(t, p, f.defaultAgent.Slug, "shadow System One input must carry real non-empty unmentioned_agent_slugs")
		}
	})

	t.Run("shadow_lookup_failure_leaves_enforce_and_copy_delivery_unaffected", func(t *testing.T) {
		f := newMpgFixture(t, true)
		m1 := sliceBMakeAgent(t, f, "r2-fault-m1")

		mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-r2-fault", sliceBS1Handler(nil, false))

		// Enforce policy does NOT read unmentioned_agent_slugs.
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "r2-enf-clean",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToConversation, store.MessagePolicySurfaceMentionDelivery}},
			ViolationRule: `false`,
			OnFailed:      store.MessagePolicyOutcomeReject,
		}))
		// Shadow policy reads unmentioned_agent_slugs.
		s1Unm := `{"dialect":"system_one","inputs":["message.body","conversation.unmentioned_agent_slugs"],"spec":{"version":1,"binding":{"instance":"ts-r2-fault"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Check unmentioned."}]}}`
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "r2-shd-fault",
			Enabled:       true,
			Mode:          store.MessagePolicyModeShadow,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: s1Unm,
			SystemOneRefs: []string{"ts-r2-fault"},
			OnUncertain:   store.MessagePolicyOutcomeAdmit,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}))

		// Inject ListAgents fault ONLY inside the shadow worker after foreground
		// copy delivery completes, so we deterministically test a shadow-only
		// lookup failure without faulting the foreground request's own lookups.
		copyDone := make(chan struct{})
		f.srv.messagePolicyGate.testShadowBeforeEval = func() {
			<-copyDone
			f.faultWrapper.failListAgents.Store(true)
		}
		defer func() {
			f.srv.messagePolicyGate.testShadowBeforeEval = nil
			f.faultWrapper.failListAgents.Store(false)
		}()

		sliceBResetDispatcher(f.dispatcher)
		rr := f.sendOutbound(t, OutboundMessageRequest{
			ConversationRef: "conv:" + f.groupConv.ID,
			Msg:             "hello @" + m1.Slug,
			Type:            messages.TypeInstruction,
		})
		close(copyDone)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Len(t, f.dispatcher.getCalls(), 1, "copy delivery must succeed unaffected by shadow lookup failure")

		// 2 enforce rows + 1 shadow row = 3 decisions.
		decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 3)
		var shdFailed bool
		for _, d := range decs {
			if d.Mode == store.MessagePolicyModeShadow {
				assert.Equal(t, messagepolicy.ClassFailed, d.Class)
				assert.Equal(t, messagepolicy.ReasonFactsUnavailable, d.CELErrorReason)
				shdFailed = true
			} else {
				assert.Equal(t, messagepolicy.ClassPass, d.Class)
			}
		}
		assert.True(t, shdFailed, "shadow row must record facts_unavailable")
	})
}

// TestO1_ExpiryAndRegistryRelease exercises real expiry/close BETWEEN preflight
// and ExecuteAgentDM verify (asserting errCopyAdmissionClosed with zero
// delivery and persistence) and verifies registry release on handler return.
func TestO1_ExpiryAndRegistryRelease(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "o1-m1")

	// Mint a real admission through the production origin gate.
	out, rej := f.srv.evaluateOriginMessagePolicies(ctx, originPolicyEvalInput{
		sendID:                 "o1-send-1",
		sender:                 f.sender,
		senderIdent:            sliceBSendAgentIdent(f.sender),
		requireActorCrossCheck: true,
		originSnap: h2FactSnapshot{
			surface:       store.MessagePolicySurfaceAgentDM,
			projectID:     f.project.ID,
			senderID:      f.sender.ID,
			senderSlug:    f.sender.Slug,
			recipientKind: "agent",
			recipientID:   f.peerAgent.ID,
			recipientSlug: f.peerAgent.Slug,
			msgType:       messages.TypeInstruction,
			introBody:     "hi @" + m1.Slug,
			msgBody:       "hi @" + m1.Slug,
			convKind:      "direct",
		},
		copyTargets: []resolvedCopyTarget{{Agent: m1, Position: "body"}},
		copyBody:    "hi @" + m1.Slug,
	})
	require.Nil(t, rej)
	require.NotNil(t, out.copyAdmission)
	out.copyAdmission.AttachMessageID("o1-origin-msg")
	defer out.releaseCopyAdmission()

	// 1. Real preflight passes while open.
	preflightErr := f.srv.verifyCopyAdmissionPreflight(ctx, agentMentionFanoutInput{
		Sender:          f.sender,
		SenderIdent:     sliceBSendAgentIdent(f.sender),
		Msg:             "hi @" + m1.Slug,
		Type:            messages.TypeInstruction,
		ParentMessageID: "o1-origin-msg",
		CopyAdmission:   out.copyAdmission,
	}, m1, "body")
	require.NoError(t, preflightErr)

	// 2a. Expire context between preflight and delivery verify -> errCopyAdmissionClosed.
	expiredCtx, cancel := context.WithCancel(ctx)
	cancel()
	verifyErr := f.srv.verifyCopyAdmissionAtDelivery(expiredCtx, &AgentDMInput{
		SenderAgent:         f.sender,
		SenderIdentity:      sliceBSendAgentIdent(f.sender),
		TargetAgent:         m1,
		Msg:                 "hi @" + m1.Slug,
		Type:                messages.TypeMention,
		ProjectID:           f.project.ID,
		SkipPhaseGate:       true,
		copyIntent:          true,
		copyAdmission:       out.copyAdmission,
		copyParentMessageID: "o1-origin-msg",
	})
	assert.ErrorIs(t, verifyErr, errCopyAdmissionClosed)

	// 2b. Close admission between preflight and ExecuteAgentDM verify -> errCopyAdmissionClosed,
	// zero delivery, and zero persistence.
	sliceBResetDispatcher(f.dispatcher)
	out.copyAdmission.Close()
	assert.False(t, out.copyAdmission.isOpen(), "Close must release admission from registry")
	_, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:         f.sender,
		SenderIdentity:      sliceBSendAgentIdent(f.sender),
		TargetAgent:         m1,
		Msg:                 "hi @" + m1.Slug,
		Type:                messages.TypeMention,
		ProjectID:           f.project.ID,
		SkipPhaseGate:       true,
		copyIntent:          true,
		copyAdmission:       out.copyAdmission,
		copyParentMessageID: "o1-origin-msg",
	})
	require.NotNil(t, dmErr)
	assert.Contains(t, dmErr.Message, errCopyAdmissionClosed.Error())
	assert.Empty(t, f.dispatcher.getCalls())

	msgs, err := f.s.ListMessages(ctx, store.MessageFilter{RecipientID: m1.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "expired/closed copy admission must persist zero messages")
}

// TestO3_AttachmentCountPinning verifies that the planned attachment count is
// pinned in the copy fingerprint and compared identically at preflight and
// delivery (both with zero policies and with policies enabled).
func TestO3_AttachmentCountPinning(t *testing.T) {
	ctx := context.Background()

	runCase := func(t *testing.T, withPolicy bool) {
		t.Helper()
		f := newMpgFixture(t, true)
		m1 := sliceBMakeAgent(t, f, "o3-m1")

		if withPolicy {
			require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
				ProjectID:     f.project.ID,
				Name:          "o3-attach-check",
				Enabled:       true,
				Mode:          store.MessagePolicyModeEnforce,
				Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceMentionDelivery}},
				ViolationRule: `message.attachment_count > 5`,
				OnFailed:      store.MessagePolicyOutcomeReject,
			}))
		}

		// Plan with 2 attempted attachments.
		out, rej := f.srv.evaluateOriginMessagePolicies(ctx, originPolicyEvalInput{
			sendID:                 "o3-send",
			sender:                 f.sender,
			senderIdent:            sliceBSendAgentIdent(f.sender),
			requireActorCrossCheck: true,
			originSnap: h2FactSnapshot{
				surface:       store.MessagePolicySurfaceAgentDM,
				projectID:     f.project.ID,
				senderID:      f.sender.ID,
				senderSlug:    f.sender.Slug,
				recipientKind: "agent",
				recipientID:   f.peerAgent.ID,
				recipientSlug: f.peerAgent.Slug,
				msgType:       messages.TypeInstruction,
				introBody:     "see files @" + m1.Slug,
				msgBody:       "see files @" + m1.Slug,
				convKind:      "direct",
			},
			copyTargets:         []resolvedCopyTarget{{Agent: m1, Position: "body"}},
			copyBody:            "see files @" + m1.Slug,
			copyAttachmentCount: 2,
		})
		require.Nil(t, rej)
		require.NotNil(t, out.copyAdmission)
		out.copyAdmission.AttachMessageID("o3-origin-msg")
		defer out.releaseCopyAdmission()

		// 1. Only 1 of 2 attachments bound -> preflight refuses BEFORE DM creation, addressee, or dispatch.
		sliceBResetDispatcher(f.dispatcher)
		copyDMKey, err := messages.DMConversationKey("agent", f.sender.ID, "agent", m1.ID)
		require.NoError(t, err)

		res := f.srv.deliverResolvedAgentMentions(ctx, agentMentionFanoutInput{
			Sender:            f.sender,
			SenderIdent:       sliceBSendAgentIdent(f.sender),
			Msg:               "see files @" + m1.Slug,
			Type:              messages.TypeInstruction,
			ParentMessageID:   "o3-origin-msg",
			ParentAttachments: []AttachmentRef{{ID: "art-1", Name: "a.txt"}}, // 1 != 2
			CopyAdmission:     out.copyAdmission,
		}, &resolvedMentionPlan{
			Results: []messages.MentionResult{{Slug: m1.Slug, Status: "delivered"}},
			Targets: map[string]*store.Agent{m1.Slug: m1},
		})
		require.Len(t, res, 1)
		assert.Equal(t, "error", res[0].Status, "attachment count mismatch must refuse copy")
		assert.Contains(t, res[0].Error, "drifted")
		assert.Empty(t, f.dispatcher.getCalls(), "refused copy must not dispatch")
		_, convErr := f.s.GetConversationByExternalRef(ctx, "native", copyDMKey)
		assert.ErrorIs(t, convErr, store.ErrNotFound, "refused copy must not create DM conversation")

		// 2. Delivery-time ExecuteAgentDM verify ALSO enforces copyBoundAttachmentCount identically.
		out2, rej2 := f.srv.evaluateOriginMessagePolicies(ctx, originPolicyEvalInput{
			sendID:                 "o3-send-2",
			sender:                 f.sender,
			senderIdent:            sliceBSendAgentIdent(f.sender),
			requireActorCrossCheck: true,
			originSnap: h2FactSnapshot{
				surface:       store.MessagePolicySurfaceAgentDM,
				projectID:     f.project.ID,
				senderID:      f.sender.ID,
				senderSlug:    f.sender.Slug,
				recipientKind: "agent",
				recipientID:   f.peerAgent.ID,
				recipientSlug: f.peerAgent.Slug,
				msgType:       messages.TypeInstruction,
				introBody:     "see files @" + m1.Slug,
				msgBody:       "see files @" + m1.Slug,
				convKind:      "direct",
			},
			copyTargets:         []resolvedCopyTarget{{Agent: m1, Position: "body"}},
			copyBody:            "see files @" + m1.Slug,
			copyAttachmentCount: 2,
		})
		require.Nil(t, rej2)
		out2.copyAdmission.AttachMessageID("o3-origin-msg-2")
		defer out2.releaseCopyAdmission()

		// All 2 attachments bound -> copy delivers cleanly.
		resOK := f.srv.deliverResolvedAgentMentions(ctx, agentMentionFanoutInput{
			Sender:          f.sender,
			SenderIdent:     sliceBSendAgentIdent(f.sender),
			Msg:             "see files @" + m1.Slug,
			Type:            messages.TypeInstruction,
			ParentMessageID: "o3-origin-msg-2",
			ParentAttachments: []AttachmentRef{
				{ID: "art-1", Name: "a.txt"},
				{ID: "art-2", Name: "b.txt"},
			},
			CopyAdmission: out2.copyAdmission,
		}, &resolvedMentionPlan{
			Results: []messages.MentionResult{{Slug: m1.Slug, Status: "delivered"}},
			Targets: map[string]*store.Agent{m1.Slug: m1},
		})
		require.Len(t, resOK, 1)
		assert.Equal(t, "delivered", resOK[0].Status, "matching attachment count must deliver (err=%q)", resOK[0].Error)
		assert.Len(t, f.dispatcher.getCalls(), 1)
	}

	t.Run("zero_policies", func(t *testing.T) { runCase(t, false) })
	t.Run("with_policy", func(t *testing.T) { runCase(t, true) })
}

// TestN2_ParentMessageIDLinkage verifies that an admission attached to one
// origin message ID refuses copies carrying a mismatched or empty parent ID.
func TestN2_ParentMessageIDLinkage(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)
	m1 := sliceBMakeAgent(t, f, "n2-m1")

	ts, _, _ := f.srv.buildCopyTargetSnapshot(ctx, f.sender, resolvedCopyTarget{Agent: m1, Position: "body"}, "hi @"+m1.Slug, 0, false, nil, false, false)
	adm := mintCopyAdmission(copyAdmissionMintInput{
		sendID:             "n2-send",
		senderAgentID:      f.sender.ID,
		senderIdentityKind: "agent",
		evalProjectID:      f.project.ID,
		copyBody:           "hi @" + m1.Slug,
		snapshots:          []copyTargetSnapshot{ts},
	})
	defer adm.Close()
	adm.AttachMessageID("origin-msg-A")

	// Mismatched ParentMessageID -> refused.
	err := f.srv.verifyCopyAdmissionPreflight(ctx, agentMentionFanoutInput{
		Sender:          f.sender,
		SenderIdent:     sliceBSendAgentIdent(f.sender),
		Msg:             "hi @" + m1.Slug,
		ParentMessageID: "origin-msg-B",
		CopyAdmission:   adm,
	}, m1, "body")
	assert.ErrorIs(t, err, errCopyAdmissionMismatch)

	// Empty ParentMessageID when admission is attached -> refused.
	err = f.srv.verifyCopyAdmissionAtDelivery(ctx, &AgentDMInput{
		SenderAgent:         f.sender,
		SenderIdentity:      sliceBSendAgentIdent(f.sender),
		TargetAgent:         m1,
		Msg:                 "hi @" + m1.Slug,
		copyAdmission:       adm,
		copyParentMessageID: "",
	})
	assert.ErrorIs(t, err, errCopyAdmissionMismatch)

	// Matching ParentMessageID -> accepted.
	err = f.srv.verifyCopyAdmissionPreflight(ctx, agentMentionFanoutInput{
		Sender:          f.sender,
		SenderIdent:     sliceBSendAgentIdent(f.sender),
		Msg:             "hi @" + m1.Slug,
		ParentMessageID: "origin-msg-A",
		CopyAdmission:   adm,
	}, m1, "body")
	assert.NoError(t, err)
}

// Keep unused time import check happy.
var _ = time.Second
