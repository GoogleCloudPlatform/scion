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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Policy unit tests for fanOutAgentMentions. These call fanOutAgentMentions
// directly rather than through an HTTP adapter — the adapter wiring itself
// is covered by the tests in agent_mention_fanout_paths_test.go and by the
// handleAgentMessage-level tests further down this file.
// ---------------------------------------------------------------------------

// mentionFanoutBaseInput builds a minimal, valid agentMentionFanoutInput for
// sender -> body, addressed as a DM whose primary is target (nil for a
// group-conversation/user-style send with no single agent primary).
func mentionFanoutBaseInput(sender, target *store.Agent, body string) agentMentionFanoutInput {
	return agentMentionFanoutInput{
		Sender:          sender,
		SenderIdent:     GetAgentIdentityFromContext(agentCtx(context.Background(), sender)),
		Primary:         target,
		Msg:             body,
		Type:            messages.TypeInstruction,
		ParentMessageID: "parent-message-id",
	}
}

// Mentioning the explicit primary recipient produces no mention result and
// no extra dispatch to it.
func TestMentionFanout_PrimaryExcluded(t *testing.T) {
	srv, _, _, sender, target, _, _, dispatcher := mentionFanoutSetup(t)

	in := mentionFanoutBaseInput(sender, target, "hey @"+target.Slug+" ping")
	results := srv.fanOutAgentMentions(context.Background(), in)

	require.Empty(t, results, "mentioning the primary recipient must produce no mention result")
	require.Empty(t, dispatchesTo(dispatcher, target.ID), "the primary must not receive a duplicate mention dispatch")
}

// A self-mention produces no delivery and no result entry.
func TestMentionFanout_SelfMentionDropped(t *testing.T) {
	srv, _, _, sender, target, _, _, dispatcher := mentionFanoutSetup(t)

	in := mentionFanoutBaseInput(sender, target, "hey @"+sender.Slug+" noted")
	results := srv.fanOutAgentMentions(context.Background(), in)

	require.Empty(t, results, "a self-mention must produce no mention result")
	require.Empty(t, dispatchesTo(dispatcher, sender.ID), "the sender must never receive its own mention")
}

// A self-mention alongside the maximum number of other recipients must not
// itself consume one of the capped slots: all of the other recipients still
// get delivered.
func TestMentionFanout_SelfMentionDoesNotConsumeRecipientCap(t *testing.T) {
	srv, s, project, sender, target, _, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	const total = messages.MaxMentionRecipients
	others := make([]*store.Agent, 0, total)
	for i := 0; i < total; i++ {
		a := &store.Agent{
			ID:              tid(fmt.Sprintf("amf-cap-agent-%d", i)),
			Name:            fmt.Sprintf("amf-cap-agent-%d", i),
			Slug:            fmt.Sprintf("amf-cap-agent-%d", i),
			ProjectID:       project.ID,
			Phase:           "running",
			RuntimeBrokerID: target.RuntimeBrokerID,
			MessageMode:     store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		others = append(others, a)
	}

	body := "cc @" + sender.Slug
	for _, a := range others {
		body += " @" + a.Slug
	}

	in := mentionFanoutBaseInput(sender, target, body)
	results := srv.fanOutAgentMentions(ctx, in)

	require.Len(t, results, total, "the self-mention must not occupy one of the capped slots")
	for _, r := range results {
		require.Equal(t, "delivered", r.Status)
	}
	for _, a := range others {
		require.NotEmpty(t, dispatchesTo(dispatcher, a.ID))
	}
}

// A nil Sender must produce no results rather than panicking at its first
// field dereference (in.Sender.Slug, in dropSelfMentionName). A nil
// SenderIdent does not risk a panic — authorizeAgentMessage already treats a
// nil identity as unauthorized and denies before any store work — but the
// guard short-circuits it too, so the caller sees an empty result instead of
// a per-mention "unauthorized" one. Every production call site always
// supplies both; this is a defensive backstop against a future caller
// mistake. Both subtests mention a real, resolvable agent (not an unknown
// name) so that a guard which only checked in.Sender would still be caught
// by the nil_sender_ident case: an unguarded nil SenderIdent would reach
// authorizeAgentMessage and come back "unauthorized" for that agent, not an
// empty result.
func TestMentionFanout_NilSenderOrSenderIdentReturnsNoResults(t *testing.T) {
	srv, _, _, sender, _, bystander, _, dispatcher := mentionFanoutSetup(t)

	t.Run("nil_sender", func(t *testing.T) {
		in := agentMentionFanoutInput{
			Sender:      nil,
			SenderIdent: GetAgentIdentityFromContext(agentCtx(context.Background(), sender)),
			Type:        messages.TypeInstruction,
			Msg:         "hey @" + bystander.Slug,
		}
		results := srv.fanOutAgentMentions(context.Background(), in)
		require.Empty(t, results)
	})

	t.Run("nil_sender_ident", func(t *testing.T) {
		in := agentMentionFanoutInput{
			Sender:      sender,
			SenderIdent: nil,
			Type:        messages.TypeInstruction,
			Msg:         "hey @" + bystander.Slug,
		}
		results := srv.fanOutAgentMentions(context.Background(), in)
		require.Empty(t, results, "a nil SenderIdent must produce an empty result, not a per-mention unauthorized one")
	})

	require.Empty(t, dispatchesTo(dispatcher, bystander.ID), "neither case should have dispatched to the mentioned agent")
}

// Only a deliberate, human-authored-shaped send triggers fan-out; every
// automatic or already-fanned-out message type produces zero dispatches.
func TestMentionFanout_TypeGate(t *testing.T) {
	cases := []struct {
		name       string
		msgType    string
		wantFanOut bool
	}{
		{"empty_treated_as_instruction", "", true},
		{"instruction", messages.TypeInstruction, true},
		{"input_needed", messages.TypeInputNeeded, true},
		{"reply", messages.TypeReply, true},
		{"chat", messages.TypeChat, true},
		{"assistant_reply_excluded", messages.TypeAssistantReply, false},
		{"state_change_excluded", messages.TypeStateChange, false},
		{"system_excluded", messages.TypeSystem, false},
		{"group_set_excluded", messages.TypeGroupSet, false},
		{"mention_excluded", messages.TypeMention, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
			in := mentionFanoutBaseInput(sender, target, "ping @"+bystander.Slug)
			in.Type = tc.msgType
			results := srv.fanOutAgentMentions(context.Background(), in)

			if tc.wantFanOut {
				require.Len(t, results, 1)
				require.Equal(t, "delivered", results[0].Status)
				require.NotEmpty(t, dispatchesTo(dispatcher, bystander.ID))
			} else {
				require.Empty(t, results, "type %q must not trigger fan-out", tc.msgType)
				require.Empty(t, dispatchesTo(dispatcher, bystander.ID), "type %q must not dispatch", tc.msgType)
			}
		})
	}
}

// An @-mention inside a fenced code block, an inline backtick span, or a
// '>' quoted line is not extracted for agent senders.
func TestMentionFanout_CodeSpansIgnored(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"fenced_block", "see the log:\n```\n@amf-bystander failed to start\n```\ndone"},
		{"unclosed_fence", "see the log:\n```\n@amf-bystander failed to start"},
		{"tilde_fence", "see the log:\n~~~\n@amf-bystander failed to start\n~~~\ndone"},
		{"inline_backtick", "run `@amf-bystander --help` to see options"},
		{"blockquote_line", "> @amf-bystander said this already\nnothing new here"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
			in := mentionFanoutBaseInput(sender, target, tc.body)
			results := srv.fanOutAgentMentions(context.Background(), in)

			require.Empty(t, results, "body %q must not extract a mention", tc.body)
			require.Empty(t, dispatchesTo(dispatcher, bystander.ID))
		})
	}
}

// ExtractMentions (used by human chat) is unaffected by the ExtractProseMentions
// change — a fenced @-mention is still extracted for a human-authored body.
func TestMentionFanout_HumanExtractMentionsUnchanged(t *testing.T) {
	got := messages.ExtractMentions("```\n@amf-bystander\n```")
	require.Equal(t, []string{"amf-bystander"}, got, "ExtractMentions must remain fence-agnostic for human senders")
}

// A mention to an agent the sender may not DM is unauthorized, creates no
// row/conversation, and the primary is unaffected.
func TestMentionFanout_Unauthorized(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	bystander.MessageMode = store.MessageModeNone
	require.NoError(t, s.UpdateAgent(context.Background(), bystander))

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
	require.NoError(t, err)
	_, convErrBefore := s.GetConversationByExternalRef(context.Background(), "native", dmKey)

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results := srv.fanOutAgentMentions(context.Background(), in)

	require.Len(t, results, 1)
	require.Equal(t, "unauthorized", results[0].Status)
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID))

	_, convErrAfter := s.GetConversationByExternalRef(context.Background(), "native", dmKey)
	require.Equal(t, convErrBefore, convErrAfter, "an unauthorized mention must not create the sender<->recipient DM conversation")

	// Primary is unaffected: fanOutAgentMentions never touches the primary
	// send, which already succeeded before this call in the real adapters.
	require.Empty(t, dispatchesTo(dispatcher, target.ID))

	// No type=mention row was persisted either.
	rows, err := s.ListMessages(context.Background(), store.MessageFilter{
		SenderID: sender.ID, RecipientID: bystander.ID, Type: messages.TypeMention,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Empty(t, rows.Items, "an unauthorized mention must not persist a row")
}

// A mention that never delivers (denied by authorization here) must not
// consume a slot in the per-pair loop cap — the cap counts deliveries, not
// attempts. Uses a cap of exactly 1 so a leak would be immediately visible:
// if a denial consumed the slot, the first genuinely authorized mention
// right after it would come back suppressed instead of delivered.
func TestMentionFanout_UnauthorizedDoesNotConsumePairCap(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	fakeNow := time.Now()
	srv.mentionPairLimiter = newMentionPairLimiterWithParams(1, time.Minute, func() time.Time { return fakeNow })

	bystander.MessageMode = store.MessageModeNone
	require.NoError(t, s.UpdateAgent(context.Background(), bystander))

	for i := 0; i < 3; i++ {
		in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
		results := srv.fanOutAgentMentions(context.Background(), in)
		require.Len(t, results, 1)
		require.Equal(t, "unauthorized", results[0].Status, "attempt %d", i)
	}

	// The cap is still fully available: the first authorized attempt after
	// three denials succeeds...
	bystander.MessageMode = store.MessageModeProject
	require.NoError(t, s.UpdateAgent(context.Background(), bystander))

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results := srv.fanOutAgentMentions(context.Background(), in)
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status)
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)

	// ...and, because that delivery really did occupy the cap's one slot,
	// the very next attempt (with nothing standing in its way but the cap)
	// is suppressed.
	in = mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results = srv.fanOutAgentMentions(context.Background(), in)
	require.Len(t, results, 1)
	require.Equal(t, "suppressed", results[0].Status)
}

// A mention rejected by the sender's own aggregate send budget must also not
// consume a pair-cap slot: the two limiters are independent, and a sender
// hitting its own rate limit is not a loop between this pair.
func TestMentionFanout_RateLimitedDoesNotConsumePairCap(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	fakeNow := time.Now()
	srv.mentionPairLimiter = newMentionPairLimiterWithParams(1, time.Minute, func() time.Time { return fakeNow })
	srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman:       chatSendHumanRatePerMinute,
		chatSenderAgent:       1,
		chatSenderAgentMirror: chatSendAgentMirrorRatePerMinute,
	}, func() time.Time { return fakeNow })
	// The bucket always starts full (floored at 1 token), so drain it first:
	// this call is charged to the sender's budget but is not itself a
	// mention, so it does not touch the pair cap either.
	require.True(t, srv.chatSendLimiter.Allow(sender.ID, chatSenderAgent).Allowed)

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results := srv.fanOutAgentMentions(context.Background(), in)
	require.Len(t, results, 1)
	require.Equal(t, "rate_limited", results[0].Status)
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID))

	// Restore the budget; the pair cap (still 1) must be untouched by the
	// rate-limited attempt above.
	srv.chatSendLimiter = newChatSendLimiter()
	in = mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results = srv.fanOutAgentMentions(context.Background(), in)
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status)
}

// A mention whose conversation resolution fails must also not consume a
// pair-cap slot: the reservation is released on this exit the same as it is
// on an authorization denial or a dispatch error.
func TestMentionFanout_ConversationResolutionFailureDoesNotConsumePairCap(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	// write-deny ON so a resolution failure denies the mention (status
	// "error") instead of continuing best-effort without a conversation.
	enableConversationEnvelopeForMentionTests(t, srv)

	senderBystanderDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
	require.NoError(t, err)
	srv.store = &upsertConversationFailStore{Store: s, failExternalRef: senderBystanderDMKey}

	srv.mentionPairLimiter = newMentionPairLimiterWithParams(1, time.Minute, time.Now)

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results := srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 1)
	require.Equal(t, "error", results[0].Status)
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID))

	// Restore a working store: the earlier failure must not have consumed
	// the pair's only slot, so this retry still succeeds.
	srv.store = s
	in = mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results = srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status,
		"the earlier conversation-resolution failure must not have consumed the pair cap's only slot")
}

// When the aggregate deadline expires while authorizeAgentMessage's own
// store lookup is in flight, the mention must be reported as "timeout" (the
// same status the loop-top deadline check uses), not "unauthorized" — the
// deadline is why the check came back negative, not a real denial by the
// recipient. The pair-cap reservation must also still be released.
func TestMentionFanout_AuthzDeadlineDuringCheckReportsTimeout(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	origAggregate := mentionFanoutAggregateTimeout
	mentionFanoutAggregateTimeout = 80 * time.Millisecond
	t.Cleanup(func() { mentionFanoutAggregateTimeout = origAggregate })

	srv.store = &blockingGetAgentStore{Store: s}
	srv.mentionPairLimiter = newMentionPairLimiterWithParams(1, time.Minute, time.Now)

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results := srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 1)
	require.Equal(t, "timeout", results[0].Status,
		"an aggregate deadline that expires during the authz check must be reported the same way the loop-top deadline check reports it")
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID))

	rows, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: bystander.ID, Type: messages.TypeMention}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Empty(t, rows.Items)

	// The reservation must have been released: restore a normal store and a
	// generous timeout, and confirm the pair's only slot is still available.
	srv.store = s
	mentionFanoutAggregateTimeout = origAggregate
	in = mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results = srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status,
		"the timed-out attempt must not have consumed the pair cap's only slot")
}

// blockingGetAgentStore blocks GetAgent until the call's own context is
// done, but only when that context carries a deadline — this targets
// exactly the aggregate-bounded calls fanOutAgentMentions makes (e.g.
// authorizeAgentMessage's internal sender-agent lookup) without blocking the
// unbounded contexts test setup and other bookkeeping use.
type blockingGetAgentStore struct {
	store.Store
}

func (s *blockingGetAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if _, ok := ctx.Deadline(); ok {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Store.GetAgent(ctx, id)
}

// upsertConversationFailStore fails UpsertConversationByExternalRef for one
// specific external_ref, delegating every other store call (including other
// external_refs) to the embedded real store.
type upsertConversationFailStore struct {
	store.Store
	failExternalRef string
}

func (s *upsertConversationFailStore) UpsertConversationByExternalRef(ctx context.Context, conv *store.Conversation) (*store.Conversation, error) {
	if conv.ExternalRef == s.failExternalRef {
		return nil, errors.New("injected UpsertConversationByExternalRef failure")
	}
	return s.Store.UpsertConversationByExternalRef(ctx, conv)
}

// An unknown name is not_found; a same-slug agent that exists only in
// another project gets no delivery either, because mention resolution is
// scoped to the sender's own project.
func TestMentionFanout_NotFoundAndCrossProject(t *testing.T) {
	srv, s, project, sender, target, _, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	t.Run("unknown_name", func(t *testing.T) {
		in := mentionFanoutBaseInput(sender, target, "cc @does-not-exist")
		results := srv.fanOutAgentMentions(ctx, in)
		require.Len(t, results, 1)
		require.Equal(t, "not_found", results[0].Status)
	})

	t.Run("qualified_cross_project_ref", func(t *testing.T) {
		in := mentionFanoutBaseInput(sender, target, "cc @otherproj/someone")
		results := srv.fanOutAgentMentions(ctx, in)
		require.Len(t, results, 1)
		require.Equal(t, "not_found", results[0].Status)
	})

	t.Run("same_slug_other_project", func(t *testing.T) {
		otherOwner := &store.User{
			ID:      tid("amf-other-owner"),
			Email:   "amf-other-owner@test.example",
			Role:    store.UserRoleMember,
			Status:  "active",
			Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, otherOwner))
		ensureHubMembership(ctx, s, otherOwner.ID)

		otherProject := &store.Project{
			ID:        tid("amf-other-project"),
			Name:      "amf-other-project",
			Slug:      "amf-other-project",
			OwnerID:   otherOwner.ID,
			CreatedBy: otherOwner.ID,
		}
		require.NoError(t, s.CreateProject(ctx, otherProject))

		twin := &store.Agent{
			ID:              tid("amf-bystander-twin"),
			Name:            mentionBystanderSlug,
			Slug:            mentionBystanderSlug,
			ProjectID:       otherProject.ID,
			Phase:           "running",
			RuntimeBrokerID: target.RuntimeBrokerID,
			MessageMode:     store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, twin))
		require.NotEqual(t, project.ID, otherProject.ID)

		in := mentionFanoutBaseInput(sender, target, "cc @"+mentionBystanderSlug)
		results := srv.fanOutAgentMentions(ctx, in)
		// mentionFanoutSetup's own same-project bystander DOES exist, so this must
		// resolve+deliver to it, not to the cross-project twin.
		require.Len(t, results, 1)
		require.Equal(t, "delivered", results[0].Status)
		require.Empty(t, dispatchesTo(dispatcher, twin.ID), "a same-slug agent in another project must never receive the mention")
	})
}

// A primary recipient from another project must not suppress a same-project
// agent mention that happens to share its slug: primary exclusion is scoped
// to the sender's own project the same way mention resolution is.
func TestMentionFanout_CrossProjectPrimaryDoesNotShadowSameSlugAgent(t *testing.T) {
	srv, s, project, sender, _, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	otherOwner := &store.User{
		ID: tid("amf-xproj-owner"), Email: "amf-xproj-owner@test.example",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, otherOwner))
	ensureHubMembership(ctx, s, otherOwner.ID)

	otherProject := &store.Project{
		ID: tid("amf-xproj-other-project"), Name: "amf-xproj-other-project",
		Slug: "amf-xproj-other-project", OwnerID: otherOwner.ID, CreatedBy: otherOwner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	// A foreign-project agent that happens to share the bystander's slug,
	// used here only as the (foreign-project) primary.
	foreignPrimary := &store.Agent{
		ID:              tid("amf-xproj-foreign-primary"),
		Name:            mentionBystanderSlug,
		Slug:            mentionBystanderSlug,
		ProjectID:       otherProject.ID,
		Phase:           "running",
		RuntimeBrokerID: sender.RuntimeBrokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, foreignPrimary))
	require.Equal(t, foreignPrimary.Slug, bystander.Slug, "sanity: the two must share a slug across projects")
	require.NotEqual(t, project.ID, otherProject.ID)

	in := mentionFanoutBaseInput(sender, foreignPrimary, "cc @"+bystander.Slug)
	results := srv.fanOutAgentMentions(ctx, in)

	require.Len(t, results, 1, "the same-project bystander must still get a result despite sharing a slug with the foreign-project primary")
	require.Equal(t, "delivered", results[0].Status)
	require.NotEmpty(t, dispatchesTo(dispatcher, bystander.ID))
}

// A human project member's @firstname-lastname mention (or a raw email)
// must not be resolved as an agent mention — it addresses a different kind
// of recipient this function does not handle, not an error.
func TestMentionFanout_HumanMentionNotResolvedAsAgent(t *testing.T) {
	srv, s, project, sender, target, bystander, _, _ := mentionFanoutSetup(t)
	ctx := context.Background()

	human := &store.User{
		ID: tid("amf-human"), Email: "jane-doe@test.example", DisplayName: "Jane Doe",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, human))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      human.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	t.Run("display_name_slug", func(t *testing.T) {
		in := mentionFanoutBaseInput(sender, target, "cc @jane-doe for visibility")
		results := srv.fanOutAgentMentions(ctx, in)
		require.Empty(t, results, "a human's display-name-slug mention must produce no agent mention result")
	})

	t.Run("raw_email", func(t *testing.T) {
		in := mentionFanoutBaseInput(sender, target, "cc @jane-doe@test.example for visibility")
		results := srv.fanOutAgentMentions(ctx, in)
		require.Empty(t, results, "a human's raw email mention must produce no agent mention result")
	})

	t.Run("mixed_with_a_real_agent_mention", func(t *testing.T) {
		in := mentionFanoutBaseInput(sender, target, "cc @jane-doe and @"+bystander.Slug)
		results := srv.fanOutAgentMentions(ctx, in)
		require.Len(t, results, 1, "only the agent mention should produce a result")
		require.Equal(t, bystander.Slug, results[0].Slug)
		require.Equal(t, "delivered", results[0].Status)
	})
}

// A raw-email mention that matches no agent must be dropped silently even
// in a project with no human members at all — recognizing it as an email
// address does not depend on finding a matching member.
func TestMentionFanout_RawEmailMentionDroppedEvenWithNoHumanMembers(t *testing.T) {
	srv, _, _, sender, target, _, _, _ := mentionFanoutSetup(t)
	ctx := context.Background()

	in := mentionFanoutBaseInput(sender, target, "cc @nobody@test.example for visibility")
	results := srv.fanOutAgentMentions(ctx, in)
	require.Empty(t, results, "a raw-email mention must produce no result even when the project has no human members")
}

// When a human member's display-name slug collides with a real agent's
// slug, the agent must still be reached: agent names are resolved first,
// and the human-member check only ever removes a name that failed to match
// any agent.
func TestMentionFanout_HumanNameCollisionWithAgentSlugStillDelivers(t *testing.T) {
	srv, s, project, sender, target, _, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	builder := &store.Agent{
		ID: tid("amf-builder-agent"), Name: "builder", Slug: "builder",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, builder))

	human := &store.User{
		ID: tid("amf-builder-human"), Email: "builder-human@test.example", DisplayName: "Builder",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, human))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      human.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Also mention a name that resolves to no agent and no human, so the
	// result set has a genuine "not_found" entry alongside the colliding
	// "delivered" one — this is what makes dropHumanMentionResults actually
	// walk its per-result filter instead of short-circuiting before it (the
	// filter only runs at all when at least one result came back
	// "not_found"), so a defect that applies the human-name match without
	// checking status would drop the "builder" result too and get caught
	// here instead of a single-mention test that would never reach the
	// filter loop in the first place.
	in := mentionFanoutBaseInput(sender, target, "cc @builder and @totally-unknown-name")
	results := srv.fanOutAgentMentions(ctx, in)

	require.Len(t, results, 2, "the agent result and the genuine not_found result must both survive")
	byslug := map[string]messages.MentionResult{}
	for _, r := range results {
		byslug[r.Slug] = r
	}
	require.Equal(t, "delivered", byslug["builder"].Status, "the agent must still be reached despite the human display-name collision")
	require.Equal(t, "not_found", byslug["totally-unknown-name"].Status)
	require.NotEmpty(t, dispatchesTo(dispatcher, builder.ID))
}

// Mention resolution uses the paginated agent listing, so it still resolves
// correctly with more than one page (200) of project agents.
func TestMentionFanout_PaginatedProjectAgents(t *testing.T) {
	srv, s, project, sender, target, _, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	const total = 205
	var last *store.Agent
	for i := 0; i < total; i++ {
		a := &store.Agent{
			ID:              tid(fmt.Sprintf("amf-page-agent-%d", i)),
			Name:            fmt.Sprintf("amf-page-agent-%d", i),
			Slug:            fmt.Sprintf("amf-page-agent-%d", i),
			ProjectID:       project.ID,
			Phase:           "running",
			RuntimeBrokerID: target.RuntimeBrokerID,
			MessageMode:     store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		last = a
	}

	in := mentionFanoutBaseInput(sender, target, "cc @"+last.Slug)
	results := srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status)
	require.NotEmpty(t, dispatchesTo(dispatcher, last.ID))
}

// A group conversation from a different project than the sender must never
// be reused for a mention delivery: fan-out falls back to a fresh
// sender-to-mentioned-agent conversation instead, and the mentioned agent is
// never registered as a participant of the foreign-project group.
func TestMentionFanout_GroupParentFromOtherProjectFallsBackToDM(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	otherOwner := &store.User{
		ID: tid("amf-other-proj-owner"), Email: "amf-other-proj-owner@test.example",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, otherOwner))
	ensureHubMembership(ctx, s, otherOwner.ID)

	otherProject := &store.Project{
		ID: tid("amf-other-proj"), Name: "amf-other-proj",
		Slug: "amf-other-proj", OwnerID: otherOwner.ID, CreatedBy: otherOwner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	foreignGroup := &store.Conversation{
		ID:          api.NewUUID(),
		Kind:        "group",
		Surface:     "native",
		ProjectID:   &otherProject.ID,
		DisplayName: "foreign-project-group",
		DriftState:  "active",
	}
	require.NoError(t, s.CreateConversation(ctx, foreignGroup))

	// A spy on GetConversation proves groupConversationBelongsToProject (the
	// project-scope check) actually ran for this conversation, not merely
	// that the end-to-end outcome happened to look right — ParentConv alone
	// is not enough to reach the check at all; ParentConvVerified is also
	// required (see resolveMentionConversation), and forgetting to set it
	// here would let this test pass for the wrong reason (the verified-flag
	// short-circuit, not the project check).
	getConvSpy := &getConversationSpy{Store: s}
	srv.store = getConvSpy

	in := mentionFanoutBaseInput(sender, target, "cc @"+bystander.Slug)
	in.ParentConv = &messaging.ConversationResult{
		ConversationID: foreignGroup.ID,
		ExternalRef:    "",
		Kind:           "group",
	}
	in.ParentConvVerified = true
	results := srv.fanOutAgentMentions(ctx, in)

	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status)
	require.Contains(t, getConvSpy.calledFor, foreignGroup.ID,
		"the project-scope check must actually have looked up the foreign group conversation")

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1)
	require.NotEqual(t, foreignGroup.ID, got[0].StructuredMessage.ConversationID,
		"the mention must not land in the foreign-project group conversation")

	participants, err := s.ListParticipants(ctx, foreignGroup.ID)
	require.NoError(t, err)
	for _, p := range participants {
		require.NotEqual(t, bystander.ID, p.PrincipalID, "the mentioned agent must never be registered on a foreign-project group conversation")
	}
}

// When the project-scope check's own conversation lookup fails (not just
// when it succeeds and finds a different project), groupConversationBelongsToProject
// must fail closed the same way: the mention falls back to a fresh
// sender<->mentioned-agent DM rather than reusing an unverifiable group.
func TestMentionFanout_GroupProjectCheckLookupErrorFallsBackToDM(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	group := &store.Conversation{
		ID:          api.NewUUID(),
		Kind:        "group",
		Surface:     "native",
		ProjectID:   &project.ID,
		DisplayName: "same-project-group",
		DriftState:  "active",
	}
	require.NoError(t, s.CreateConversation(ctx, group))

	// The lookup itself fails (not merely finds a mismatched project), so
	// groupConversationBelongsToProject must fail closed here too.
	getConvSpy := &getConversationSpy{Store: s, failFor: group.ID, errFor: store.ErrNotFound}
	srv.store = getConvSpy

	in := mentionFanoutBaseInput(sender, target, "cc @"+bystander.Slug)
	in.ParentConv = &messaging.ConversationResult{
		ConversationID: group.ID,
		ExternalRef:    "",
		Kind:           "group",
	}
	in.ParentConvVerified = true
	results := srv.fanOutAgentMentions(ctx, in)

	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status)
	require.Contains(t, getConvSpy.calledFor, group.ID,
		"the project-scope check must actually have attempted the lookup")

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1)
	senderBystanderDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
	require.NoError(t, err)
	bystanderConv, err := s.GetConversationByExternalRef(ctx, "native", senderBystanderDMKey)
	require.NoError(t, err, "the sender<->bystander DM conversation must have been created")
	require.Equal(t, bystanderConv.ID, got[0].StructuredMessage.ConversationID,
		"a lookup error must fall back to the fresh sender<->mentioned-agent DM, not reuse the unverifiable group")

	participants, err := s.ListParticipants(ctx, group.ID)
	require.NoError(t, err)
	for _, p := range participants {
		require.NotEqual(t, bystander.ID, p.PrincipalID, "the mentioned agent must never be registered on a group conversation whose project could not be confirmed")
	}
}

// getConversationSpy records every conversation ID GetConversation was
// called with, delegating to the embedded real store, so a test can prove a
// particular lookup actually happened rather than only checking the
// end-to-end outcome. When failFor matches the requested ID, it returns
// errFor instead of delegating, so a test can exercise the lookup-error
// fail-closed branch of a caller like groupConversationBelongsToProject.
type getConversationSpy struct {
	store.Store
	calledFor []string
	failFor   string
	errFor    error
}

func (s *getConversationSpy) GetConversation(ctx context.Context, id string) (*store.Conversation, error) {
	s.calledFor = append(s.calledFor, id)
	if s.failFor != "" && id == s.failFor {
		return nil, s.errFor
	}
	return s.Store.GetConversation(ctx, id)
}

// The pair cap suppresses the delivery once the window's cap is reached, the
// primary is unaffected, and a fresh window allows it again.
func TestMentionFanout_PairCap(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	fakeNow := time.Now()
	srv.mentionPairLimiter = newMentionPairLimiterWithParams(2, time.Minute, func() time.Time { return fakeNow })

	for i := 0; i < 2; i++ {
		in := mentionFanoutBaseInput(sender, target, fmt.Sprintf("ping %d @%s", i, bystander.Slug))
		results := srv.fanOutAgentMentions(context.Background(), in)
		require.Len(t, results, 1)
		require.Equal(t, "delivered", results[0].Status, "delivery %d should be within the cap", i)
	}

	// Third delivery within the same window is suppressed.
	in := mentionFanoutBaseInput(sender, target, "ping 3 @"+bystander.Slug)
	results := srv.fanOutAgentMentions(context.Background(), in)
	require.Len(t, results, 1)
	require.Equal(t, "suppressed", results[0].Status)
	beforeCount := len(dispatchesTo(dispatcher, bystander.ID))

	// A fresh window allows it again.
	fakeNow = fakeNow.Add(2 * time.Minute)
	in = mentionFanoutBaseInput(sender, target, "ping 4 @"+bystander.Slug)
	results = srv.fanOutAgentMentions(context.Background(), in)
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status)
	require.Greater(t, len(dispatchesTo(dispatcher, bystander.ID)), beforeCount)
}

// Every mention delivery consumes one aggregate send-budget token; once
// exhausted mid-fan-out, the remaining mentions are rate_limited without
// ever reaching a per-recipient dispatch attempt.
func TestMentionFanout_BudgetExhaustion(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	fakeNow := time.Now()
	// Allow exactly 1 agent-class send per minute so the second mention in
	// one fan-out call exhausts the aggregate budget.
	srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman:       chatSendHumanRatePerMinute,
		chatSenderAgent:       1,
		chatSenderAgentMirror: chatSendAgentMirrorRatePerMinute,
	}, func() time.Time { return fakeNow })

	// Three targets are needed: with only two, the second is rejected
	// directly by ExecuteAgentDM's own rate limiter and the fan-out's own
	// "budget already known exhausted" short-circuit is never exercised. A
	// third target proves that branch skips ExecuteAgentDM entirely once
	// the budget is known to be exhausted, rather than calling it again.
	project := sender.ProjectID
	second := &store.Agent{
		ID: tid("amf-bystander-2"), Name: "amf-bystander-2", Slug: "amf-bystander-2",
		ProjectID: project, Phase: "running", RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	third := &store.Agent{
		ID: tid("amf-bystander-3"), Name: "amf-bystander-3", Slug: "amf-bystander-3",
		ProjectID: project, Phase: "running", RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	ctx := context.Background()
	s := srv.store
	require.NoError(t, s.CreateAgent(ctx, second))
	require.NoError(t, s.CreateAgent(ctx, third))

	in := mentionFanoutBaseInput(sender, target, fmt.Sprintf("cc @%s @%s @%s", bystander.Slug, second.Slug, third.Slug))
	results := srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 3)

	require.Equal(t, "delivered", results[0].Status, "the 1st mention should fit the budget")
	require.Equal(t, "rate_limited", results[1].Status, "the 2nd mention is rejected by ExecuteAgentDM's own rate limiter")
	require.Equal(t, "rate_limited", results[2].Status, "the 3rd mention must be skipped once the budget is known exhausted")
	require.Contains(t, results[2].Error, "exhausted by an earlier mention",
		"the 3rd result's wording proves the short-circuit fired — ExecuteAgentDM (and its own rate-limit message) was never reached")

	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)
	require.Empty(t, dispatchesTo(dispatcher, second.ID))
	require.Empty(t, dispatchesTo(dispatcher, third.ID), "the 3rd mention must not have been dispatched at all")

	rows, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: third.ID, Type: messages.TypeMention}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Empty(t, rows.Items, "the 3rd mention must not have persisted a row either — it never reached ExecuteAgentDM")

	require.Empty(t, dispatchesTo(dispatcher, target.ID), "the primary is not this function's concern and must be untouched")
}

// Fan-out gives every mention a bounded budget: a recipient whose dispatch
// never completes (a broker repeatedly deferring the message) must not hold
// up the whole call. Uses shrunk package-level timeouts so the test itself
// runs in well under a second.
func TestMentionFanout_DeferredDispatchStaysWithinBudget(t *testing.T) {
	srv, _, _, sender, target, bystander, _, _ := mentionFanoutSetup(t)

	origAggregate, origDispatch := mentionFanoutAggregateTimeout, mentionDispatchTimeout
	mentionFanoutAggregateTimeout = 300 * time.Millisecond
	mentionDispatchTimeout = 150 * time.Millisecond
	t.Cleanup(func() {
		mentionFanoutAggregateTimeout = origAggregate
		mentionDispatchTimeout = origDispatch
	})

	deferringDispatcher := &alwaysDeferDispatcher{}
	srv.SetDispatcher(deferringDispatcher)

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)

	start := time.Now()
	results := srv.fanOutAgentMentions(context.Background(), in)
	elapsed := time.Since(start)

	require.Len(t, results, 1)
	require.NotEqual(t, "delivered", results[0].Status, "a permanently-deferred dispatch must not be reported as delivered")
	require.Less(t, elapsed, 2*time.Second,
		"one unreachable recipient must not be able to hold up the whole fan-out call anywhere near as long as the default aggregate budget")
}

// alwaysDeferDispatcher is an AgentDispatcher whose every DispatchAgentMessage
// call reports the message as deferred (the "broker not locally reachable
// right now" case), so dispatchWithBrokerRetry's retry loop never terminates
// on its own and only the caller's context deadline ends the attempt. Every
// other AgentDispatcher method is inherited from the embedded
// recordingDispatcher, unused by this test.
type alwaysDeferDispatcher struct {
	recordingDispatcher
}

func (d *alwaysDeferDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	return ErrMessageDeferred
}

// The per-dispatch timeout — not just the aggregate one — is what lets a
// later recipient in the same fan-out call still be attempted after an
// earlier one hangs: without it, the first recipient's deferred dispatch
// would retry until the aggregate budget itself runs out, leaving nothing
// for anyone after it. Two recipients: the first always defers, the second
// dispatches normally.
func TestMentionFanout_PerDispatchTimeoutLetsLaterRecipientsStillBeAttempted(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	second := &store.Agent{
		ID: tid("mention-fanout-second-recipient"), Name: "second-recipient", Slug: "second-recipient",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, second))

	origAggregate, origDispatch := mentionFanoutAggregateTimeout, mentionDispatchTimeout
	mentionFanoutAggregateTimeout = 2 * time.Second
	mentionDispatchTimeout = 150 * time.Millisecond
	t.Cleanup(func() {
		mentionFanoutAggregateTimeout = origAggregate
		mentionDispatchTimeout = origDispatch
	})

	selective := &selectiveDeferDispatcher{recordingDispatcher: dispatcher, deferFor: bystander.ID}
	srv.SetDispatcher(selective)

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug+" and @"+second.Slug)

	start := time.Now()
	results := srv.fanOutAgentMentions(ctx, in)
	elapsed := time.Since(start)

	require.Len(t, results, 2)
	byslug := map[string]messages.MentionResult{}
	for _, r := range results {
		byslug[r.Slug] = r
	}
	require.NotEqual(t, "delivered", byslug[bystander.Slug].Status, "the permanently-deferred recipient must not be reported as delivered")
	require.Equal(t, "delivered", byslug[second.Slug].Status, "a later recipient must still be attempted after an earlier one exhausts its own dispatch timeout")
	require.Less(t, elapsed, mentionFanoutAggregateTimeout, "the per-dispatch timeout, not the aggregate one, must be what ends the first recipient's attempt")
}

// selectiveDeferDispatcher defers DispatchAgentMessage only for one agent ID
// and otherwise delegates to the embedded recordingDispatcher, so a test can
// exercise "one recipient hangs, another does not" within a single fan-out
// call.
type selectiveDeferDispatcher struct {
	*recordingDispatcher
	deferFor string
}

func (d *selectiveDeferDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if agent != nil && agent.ID == d.deferFor {
		return ErrMessageDeferred
	}
	return d.recordingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

// SkipPhaseGate has effect ONLY for Type mention. A non-mention send with
// SkipPhaseGate set must still be rejected for a non-running target, and a
// mention send WITHOUT SkipPhaseGate set must be rejected too — the flag,
// not the type alone, is what allows delivery to a non-running recipient.
func TestAgentDMSkipPhaseGate_MentionOnly(t *testing.T) {
	srv, s, _, sender, target, _, _, _ := mentionFanoutSetup(t)
	ctx := context.Background()

	require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{Phase: "stopped"}))
	fresh, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)

	senderIdent := GetAgentIdentityFromContext(agentCtx(ctx, sender))

	// Non-mention + SkipPhaseGate: still rejected.
	_, dmErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: senderIdent,
		TargetAgent:    fresh,
		Msg:            "hello",
		Type:           messages.TypeInstruction,
		ProjectID:      sender.ProjectID,
		SkipPhaseGate:  true,
	})
	require.NotNil(t, dmErr, "a non-mention send must still be rejected for a non-running target even with SkipPhaseGate set")
	require.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)

	// Mention WITHOUT SkipPhaseGate: still rejected.
	_, dmErr = srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: senderIdent,
		TargetAgent:    fresh,
		Msg:            "hello",
		Type:           messages.TypeMention,
		ProjectID:      sender.ProjectID,
		SkipPhaseGate:  false,
	})
	require.NotNil(t, dmErr, "Type mention alone, without SkipPhaseGate, must still be rejected for a non-running target")
	require.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)

	// Mention + SkipPhaseGate: proceeds despite the non-running phase.
	result, dmErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: senderIdent,
		TargetAgent:    fresh,
		Msg:            "hello",
		Type:           messages.TypeMention,
		ProjectID:      sender.ProjectID,
		SkipPhaseGate:  true,
	})
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	require.Equal(t, AgentDMAccepted, result.Outcome)
}

// A stopped recipient still gets a persisted row and a real dispatch
// attempt from the actual fan-out entry point (not just ExecuteAgentDM in
// isolation): the result is reported delivered, with the recipient's
// current phase attached.
func TestMentionFanout_StoppedRecipient(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	require.NoError(t, s.UpdateAgentStatus(ctx, bystander.ID, store.AgentStatusUpdate{Phase: "stopped"}))

	in := mentionFanoutBaseInput(sender, target, "please help @"+bystander.Slug)
	results := srv.fanOutAgentMentions(ctx, in)

	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status, "a stopped recipient must still be reported delivered")
	require.Equal(t, "stopped", results[0].AgentPhase)

	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1, "dispatch must still be attempted (buffered) for a stopped recipient")

	rows, err := s.ListMessages(ctx, store.MessageFilter{
		RecipientID: bystander.ID, Type: messages.TypeMention,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1, "the mention row must be persisted even though the recipient is stopped")
}

// ---------------------------------------------------------------------------
// Explicit-mentions union tests: the hub unions caller-supplied mention
// names (the CLI's --cc, or MessageRequest.Mentions) with body-extracted
// mentions. These specifically isolate the explicit-only source, since a
// test whose body also happens to contain the same name cannot tell whether
// the explicit field did anything at all.
// ---------------------------------------------------------------------------

// A mention supplied only through the explicit field (an empty-of-mentions
// body) is still resolved and delivered.
func TestMentionFanout_ExplicitOnly(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	in := mentionFanoutBaseInput(sender, target, "no mentions in this body at all")
	in.Explicit = []string{bystander.Slug}
	results := srv.fanOutAgentMentions(context.Background(), in)

	require.Len(t, results, 1)
	require.Equal(t, bystander.Slug, results[0].Slug)
	require.Equal(t, "delivered", results[0].Status)
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)
}

// The explicit field and the body are unioned case-insensitively: the same
// name spelled differently in each source still produces exactly one
// result and one delivery, not two.
func TestMentionFanout_ExplicitAndBodyUnionCaseInsensitive(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	in := mentionFanoutBaseInput(sender, target, "please loop in @"+bystander.Slug)
	in.Explicit = []string{"AMF-BYSTANDER"}
	results := srv.fanOutAgentMentions(context.Background(), in)

	require.Len(t, results, 1, "the same recipient named in both sources must produce exactly one result")
	require.Equal(t, "delivered", results[0].Status)
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)
}

// The explicit field can also introduce a DIFFERENT recipient than the body
// mentions: both are delivered.
func TestMentionFanout_ExplicitAddsADifferentRecipientThanBody(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	second := &store.Agent{
		ID: tid("amf-explicit-2"), Name: "amf-explicit-2", Slug: "amf-explicit-2",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, second))

	in := mentionFanoutBaseInput(sender, target, "please loop in @"+bystander.Slug)
	in.Explicit = []string{second.Slug}
	results := srv.fanOutAgentMentions(ctx, in)

	require.Len(t, results, 2)
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)
	require.Len(t, dispatchesTo(dispatcher, second.ID), 1)
}

// ---------------------------------------------------------------------------
// HTTP-level tests: these go through handleAgentMessage itself (not a direct
// fanOutAgentMentions call), so they also exercise the primary send being
// genuinely unaffected by a mention outcome, and a group-conversation parent
// arriving through the request's own conversation resolution rather than a
// hand-built agentMentionFanoutInput.
// ---------------------------------------------------------------------------

// An explicit MessageRequest.Mentions entry, on a mention-free body, is
// still delivered when sent as a real HTTP request — not just when the
// input struct is built by hand.
func TestHandleAgentMessage_ExplicitMentionsFieldNoBodyOverlap(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	sm := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Recipient: "agent:" + target.Slug,
		Msg:       "no mentions written here",
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: []string{bystander.Slug}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), sender))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1, "an explicit mention on a mention-free body must still be delivered")
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
}

// A mention that turns out unauthorized does not fail — or even touch — the
// primary send: the HTTP response is still a normal dispatched 200, the
// primary is dispatched exactly once, and the mention's own outcome is
// reported alongside it.
func TestHandleAgentMessage_UnauthorizedMentionLeavesPrimaryUnaffected(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	bystander.MessageMode = store.MessageModeNone
	require.NoError(t, s.UpdateAgent(context.Background(), bystander))

	rr := sendViaStructured(t, srv, sender, target, "hey @"+bystander.Slug+" take a look")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	require.Len(t, dispatchesTo(dispatcher, target.ID), 1, "the primary must be dispatched regardless of the mention's outcome")
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID))

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "dispatched", resp.Status)
	require.Len(t, resp.MentionResults, 1)
	require.Equal(t, "unauthorized", resp.MentionResults[0].Status)
}

// A mention suppressed by the per-pair loop cap also leaves the primary send
// untouched.
func TestHandleAgentMessage_SuppressedMentionLeavesPrimaryUnaffected(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	fakeNow := time.Now()
	srv.mentionPairLimiter = newMentionPairLimiterWithParams(0, time.Minute, func() time.Time { return fakeNow })

	rr := sendViaStructured(t, srv, sender, target, "hey @"+bystander.Slug+" take a look")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	require.Len(t, dispatchesTo(dispatcher, target.ID), 1)
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID))

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "dispatched", resp.Status)
	require.Len(t, resp.MentionResults, 1)
	require.Equal(t, "suppressed", resp.MentionResults[0].Status)
}

// A mention in a message sent with an explicit, same-project group
// conversation_id lands in that group conversation (not a fresh DM), and the
// mentioned agent is registered as a participant of it.
func TestHandleAgentMessage_GroupConversationMentionRegistersParticipant(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()
	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	grantAgentProjectAccess(t, s, sender.ID, project.ID)
	grantAgentProjectAccess(t, s, target.ID, project.ID)

	createBytes, _ := json.Marshal(createConversationRequest{
		DisplayName: "amf-handle-agent-message-group",
		ProjectID:   project.ID,
		Kind:        "group",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(createBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(agentContextWithScopes(sender.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	createRR := httptest.NewRecorder()
	srv.handleCreateConversation(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code, "body: %s", createRR.Body.String())
	var created conversationResponse
	require.NoError(t, json.Unmarshal(createRR.Body.Bytes(), &created))

	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Type:           messages.TypeInstruction,
		Recipient:      "agent:" + target.Slug,
		Msg:            "please pick this up @" + bystander.Slug,
		ConversationID: created.ID,
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), sender))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1)
	require.Equal(t, created.ID, got[0].StructuredMessage.ConversationID, "the mention must land in the group conversation, not a fresh DM")

	participants, err := s.ListParticipants(ctx, created.ID)
	require.NoError(t, err)
	var found bool
	for _, p := range participants {
		if p.PrincipalKind == "agent" && p.PrincipalID == bystander.ID {
			found = true
		}
	}
	require.True(t, found, "the mentioned agent must be registered as a group conversation participant")
}

// Group-participant registration after a delivered mention must survive the
// aggregate fan-out context having already expired by the time it runs — a
// slow-but-successful dispatch can eat most or all of the aggregate budget,
// and the registration write still needs to land.
func TestMentionFanout_ParticipantRegistrationSurvivesExpiredAggregateContext(t *testing.T) {
	srv, s, project, sender, _, bystander, _, dispatcher := mentionFanoutSetup(t)

	group := &store.Conversation{
		ID: api.NewUUID(), Kind: "group", Surface: "native",
		ProjectID: &project.ID, DisplayName: "g", DriftState: "active",
		ExternalRef: "thread:" + project.ID + ":some-topic",
	}
	require.NoError(t, s.CreateConversation(context.Background(), group))

	origAggregate, origDispatch := mentionFanoutAggregateTimeout, mentionDispatchTimeout
	mentionFanoutAggregateTimeout = 30 * time.Millisecond
	mentionDispatchTimeout = 2 * time.Second
	t.Cleanup(func() {
		mentionFanoutAggregateTimeout = origAggregate
		mentionDispatchTimeout = origDispatch
	})

	// The caller's own context (not just the internally-derived aggregate
	// one) is also already expired by the time registration runs: this is
	// what actually distinguishes finalizationContext(ctx) — which detaches
	// from ctx's own cancellation via context.WithoutCancel before applying
	// its own timeout — from a plain context.WithCancel(ctx), which would
	// inherit ctx's already-expired deadline and fail immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	slow := &slowThenRecordDispatcher{recordingDispatcher: dispatcher, delay: 80 * time.Millisecond}
	srv.SetDispatcher(slow)

	in := mentionFanoutBaseInput(sender, nil, "please help @"+bystander.Slug)
	in.ParentConv = &messaging.ConversationResult{ConversationID: group.ID, ExternalRef: group.ExternalRef, Kind: "group"}
	in.ParentConvVerified = true

	results := srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status)

	// A fresh context for the verification query — ctx itself is expired by
	// now, same as it would be for a real request whose client already gave
	// up on the response.
	participants, err := s.ListParticipants(context.Background(), group.ID)
	require.NoError(t, err)
	var found bool
	for _, p := range participants {
		if p.PrincipalKind == "agent" && p.PrincipalID == bystander.ID {
			found = true
		}
	}
	require.True(t, found, "participant registration must survive the caller's own context already having expired by the time it runs")
}

// slowThenRecordDispatcher sleeps for delay before delegating to the
// embedded *recordingDispatcher, so a test can make a delivery slow enough
// to outlast a short aggregate deadline while still succeeding.
type slowThenRecordDispatcher struct {
	*recordingDispatcher
	delay time.Duration
}

func (d *slowThenRecordDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	time.Sleep(d.delay)
	return d.recordingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}
