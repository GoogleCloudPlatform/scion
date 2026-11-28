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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestGroupConversations(t *testing.T, ctx context.Context, s store.Store, projectID, userID string) (legacyGroup, nativeGroup *store.Conversation) {
	t.Helper()

	var err error
	legacyGroup, err = s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + projectID + ":group-dm-thread-legacy",
		ProjectID:   &projectID,
		DriftState:  "active",
	})
	require.NoError(t, err)

	nativeGroup = &store.Conversation{
		ID:          tid("group-dm-thread-native"),
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "",
		ProjectID:   &projectID,
		DriftState:  "active",
	}
	require.NoError(t, s.CreateConversation(ctx, nativeGroup))

	for _, conv := range []*store.Conversation{legacyGroup, nativeGroup} {
		require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: conv.ID,
			PrincipalKind:  "user",
			PrincipalID:    userID,
			Role:           "member",
		}))
	}
	return legacyGroup, nativeGroup
}

func assertAgentNotParticipant(t *testing.T, ctx context.Context, s store.Store, convID, agentID string) {
	t.Helper()
	parts, err := s.ListParticipants(ctx, convID)
	require.NoError(t, err)
	for _, p := range parts {
		if p.PrincipalKind == "agent" && p.PrincipalID == agentID {
			t.Fatalf("rejected send must not register agent %s as participant of conversation %s", agentID, convID)
		}
	}
}

func TestGroupConversation_DMThreadID_Rejected_AllOutboundKinds(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.Decisions:       true,
		experiments.AgentSecrets:    true,
		experiments.CommandRequests: true,
	})

	legacyGroup, nativeGroup := createTestGroupConversations(t, ctx, s, project.ID, user.ID)
	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)

	groups := []struct {
		name string
		conv *store.Conversation
	}{
		{name: "legacy_group", conv: legacyGroup},
		{name: "native_group", conv: nativeGroup},
	}

	kinds := []struct {
		name  string
		build func(convID, threadID string) OutboundMessageRequest
	}{
		{
			name: "plain_message",
			build: func(convID, threadID string) OutboundMessageRequest {
				return OutboundMessageRequest{
					Msg:             "group message with dm thread_id",
					ConversationRef: "conv:" + convID,
					ThreadID:        threadID,
					Channel:         "web",
				}
			},
		},
		{
			name: "plain_message_whitespace_dm_prefix",
			build: func(convID, threadID string) OutboundMessageRequest {
				return OutboundMessageRequest{
					Msg:             "group message with whitespace-prefixed dm thread_id",
					ConversationRef: "conv:" + convID,
					ThreadID:        " " + threadID,
					Channel:         "web",
				}
			},
		},
		{
			name: "decision_prompt",
			build: func(convID, threadID string) OutboundMessageRequest {
				return OutboundMessageRequest{
					Msg:             "pick one",
					ConversationRef: "conv:" + convID,
					ThreadID:        threadID,
					Channel:         "web",
					Decisions: []messages.DecisionInput{
						{
							ID:       "grp-dec-1",
							Question: "Proceed?",
							Choices: []messages.ChoiceInput{
								{Label: "Yes"},
								{Label: "No"},
							},
						},
					},
				}
			},
		},
		{
			name: "secret_request",
			build: func(convID, threadID string) OutboundMessageRequest {
				return OutboundMessageRequest{
					Msg:             "need key",
					ConversationRef: "conv:" + convID,
					ThreadID:        threadID,
					Channel:         "web",
					SecretRequests: []messages.SecretRequestInput{
						{
							Key:         "GROUP_TEST_TOKEN",
							Description: "API token",
						},
					},
				}
			},
		},
		{
			name: "command_request",
			build: func(convID, threadID string) OutboundMessageRequest {
				return OutboundMessageRequest{
					Msg:             "run check",
					ConversationRef: "conv:" + convID,
					ThreadID:        threadID,
					Channel:         "web",
					CommandRequest: &messages.CommandRequestInput{
						Key:      "grp-cmd-1",
						Script:   "echo ok",
						Approver: "@" + user.Email,
					},
				}
			},
		},
	}

	for _, g := range groups {
		for _, k := range kinds {
			t.Run(g.name+"/"+k.name, func(t *testing.T) {
				req := k.build(g.conv.ID, dmKey)
				rr := postOutboundRequest(t, srv, project.ID, agent.ID, req)
				require.Equal(t, http.StatusBadRequest, rr.Code,
					"dm: thread_id on %s (%s) must be rejected 400: %s", g.name, k.name, rr.Body.String())
				assert.Contains(t, rr.Body.String(), "non-DM conversation",
					"error must explain dm: thread_id refusal: %s", rr.Body.String())

				msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
				require.NoError(t, err)
				assert.Empty(t, msgs.Items, "rejected send must not persist a message")

				assertAgentNotParticipant(t, ctx, s, g.conv.ID, agent.ID)
			})
		}
	}
}

func TestGroupConversation_DMThreadID_RawConversationID_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.Decisions:    true,
		experiments.AgentSecrets: true,
	})

	legacyGroup, nativeGroup := createTestGroupConversations(t, ctx, s, project.ID, user.ID)
	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)

	cases := []struct {
		name string
		req  OutboundMessageRequest
		conv *store.Conversation
	}{
		{
			name: "legacy_plain",
			conv: legacyGroup,
			req: OutboundMessageRequest{
				Recipient:      "user:" + user.Email,
				RecipientID:    user.ID,
				Msg:            "raw conv id plain",
				ConversationID: legacyGroup.ID,
				ThreadID:       dmKey,
				Channel:        "web",
			},
		},
		{
			name: "native_decision",
			conv: nativeGroup,
			req: OutboundMessageRequest{
				Recipient:      "user:" + user.Email,
				RecipientID:    user.ID,
				Msg:            "raw conv id decision",
				ConversationID: nativeGroup.ID,
				ThreadID:       dmKey,
				Channel:        "web",
				Decisions: []messages.DecisionInput{
					{
						ID:       "raw-dec-1",
						Question: "Continue?",
						Choices: []messages.ChoiceInput{
							{Label: "Yes"},
							{Label: "No"},
						},
					},
				},
			},
		},
		{
			name: "native_secret_request",
			conv: nativeGroup,
			req: OutboundMessageRequest{
				Recipient:      "user:" + user.Email,
				RecipientID:    user.ID,
				Msg:            "raw conv id secret",
				ConversationID: nativeGroup.ID,
				ThreadID:       dmKey,
				Channel:        "web",
				SecretRequests: []messages.SecretRequestInput{
					{
						Key:         "RAW_GROUP_TOKEN",
						Description: "API token",
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := postOutboundRequest(t, srv, project.ID, agent.ID, tc.req)
			require.Equal(t, http.StatusBadRequest, rr.Code,
				"raw conversation_id (%s): dm: thread_id on group conversation must be rejected 400: %s",
				tc.name, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "non-DM conversation")

			msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, msgs.Items, "rejected send must not persist a message")
			assertAgentNotParticipant(t, ctx, s, tc.conv.ID, agent.ID)
		})
	}
}

func TestGroupConversation_ValidThreadID_StillAccepted(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)

	legacyGroup, nativeGroup := createTestGroupConversations(t, ctx, s, project.ID, user.ID)
	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	t.Run("legacy_group_empty_thread_id", func(t *testing.T) {
		rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Msg:             "legacy group message",
			ConversationRef: "conv:" + legacyGroup.ID,
			Channel:         "web",
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	})

	t.Run("legacy_group_non_dm_thread_id", func(t *testing.T) {
		rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Msg:             "legacy group message with topic thread_id",
			ConversationRef: "conv:" + legacyGroup.ID,
			ThreadID:        "group-dm-thread-legacy",
			Channel:         "web",
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	})

	t.Run("native_group_empty_thread_id", func(t *testing.T) {
		rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Msg:             "native group message",
			ConversationRef: "conv:" + nativeGroup.ID,
			Channel:         "web",
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	})

	t.Run("direct_conv_matching_dm_thread_id", func(t *testing.T) {
		rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Msg:             "direct message with matching dm thread_id",
			ConversationRef: "conv:" + dmConv.ID,
			ThreadID:        dmKey,
			Channel:         "web",
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	})
}

func TestDirectConversation_WhitespacePaddedDMThreadID_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)

	otherUser := &store.User{
		ID:          tid("direct-ws-dm-other-user"),
		Email:       "direct-ws-dm-other@example.com",
		DisplayName: "Other Direct WS User",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	otherDMKey := mustDMKey(t, "agent", agent.ID, "user", otherUser.ID)

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	cases := []struct {
		name string
		req  OutboundMessageRequest
	}{
		{
			name: "conv_ref_leading_space_mismatched_dm_key",
			req: OutboundMessageRequest{
				Msg:             "direct conv ref leading space mismatched",
				ConversationRef: "conv:" + dmConv.ID,
				ThreadID:        " " + otherDMKey,
				Channel:         "web",
			},
		},
		{
			name: "conv_ref_surrounding_whitespace_mismatched_dm_key",
			req: OutboundMessageRequest{
				Msg:             "direct conv ref surrounding whitespace mismatched",
				ConversationRef: "conv:" + dmConv.ID,
				ThreadID:        " \t" + otherDMKey + "\n",
				Channel:         "web",
			},
		},
		{
			name: "conv_ref_leading_space_matching_dm_key",
			req: OutboundMessageRequest{
				Msg:             "direct conv ref leading space matching",
				ConversationRef: "conv:" + dmConv.ID,
				ThreadID:        " " + dmKey,
				Channel:         "web",
			},
		},
		{
			name: "raw_conv_id_leading_space_mismatched_dm_key",
			req: OutboundMessageRequest{
				Recipient:      "user:" + user.Email,
				RecipientID:    user.ID,
				Msg:            "direct raw conv id leading space mismatched",
				ConversationID: dmConv.ID,
				ThreadID:       " " + otherDMKey,
				Channel:        "web",
			},
		},
		{
			name: "raw_conv_id_leading_space_matching_dm_key",
			req: OutboundMessageRequest{
				Recipient:      "user:" + user.Email,
				RecipientID:    user.ID,
				Msg:            "direct raw conv id leading space matching",
				ConversationID: dmConv.ID,
				ThreadID:       " " + dmKey,
				Channel:        "web",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := postOutboundRequest(t, srv, project.ID, agent.ID, tc.req)
			require.Equal(t, http.StatusBadRequest, rr.Code,
				"%s: whitespace-padded dm: thread_id on direct conversation must be rejected 400: %s",
				tc.name, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "does not match the direct conversation's key")

			msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, msgs.Items, "rejected send must not persist a message")
		})
	}
}

func TestDerivedGroupThread_WhitespacePaddedDMThreadID_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Register an external channel ("discord") in addition to "web" so
	// outboundThreadGateApplies("discord") is false and the request reaches
	// the derived kind=="group" guard without being intercepted by the
	// web thread-existence gate (422).
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
		{Name: "discord", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(project.ID)

	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)

	for _, ch := range []string{"web", "discord"} {
		t.Run("channel_"+ch, func(t *testing.T) {
			rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
				Recipient:   "user:" + user.Email,
				RecipientID: user.ID,
				Msg:         "derived group thread with whitespace-padded dm thread_id",
				ThreadID:    " " + dmKey,
				Channel:     ch,
			})
			require.Equal(t, http.StatusBadRequest, rr.Code,
				"derived group thread (%s) with whitespace-padded dm: thread_id must be rejected 400: %s",
				ch, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "non-DM conversation")

			msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, msgs.Items, "rejected send must not persist a message")
		})
	}
}

func TestAgentMessage_GroupAndDirect_DMThreadID_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	agent.Phase = string(state.PhaseRunning)
	require.NoError(t, s.UpdateAgent(ctx, agent))

	ensureHubMembership(ctx, s, user.ID)
	project.OwnerID = user.ID
	project.CreatedBy = user.ID
	srv.seedProjectCreatorMembership(ctx, project)

	otherUser := &store.User{
		ID:          tid("agent-msg-other-user"),
		Email:       "agent-msg-other@example.com",
		DisplayName: "Other Agent Message User",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	legacyGroup, nativeGroup := createTestGroupConversations(t, ctx, s, project.ID, user.ID)
	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	otherDMKey := mustDMKey(t, "agent", agent.ID, "user", otherUser.ID)

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	postAgentMsg := func(t *testing.T, sm *messages.StructuredMessage) *httptest.ResponseRecorder {
		t.Helper()
		body, marshalErr := json.Marshal(MessageRequest{StructuredMessage: sm})
		require.NoError(t, marshalErr)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ident := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, store.UserRoleAdmin, "web")
		req = req.WithContext(contextWithIdentity(req.Context(), ident))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, req, agent.ID)
		return rr
	}

	cases := []struct {
		name        string
		msg         *messages.StructuredMessage
		wantContain string
		checkGroup  string
	}{
		{
			name: "legacy_group_conv_id_dm_thread_id",
			msg: &messages.StructuredMessage{
				Msg:            "group msg with dm thread_id",
				Type:           messages.TypeInstruction,
				Channel:        "web",
				ConversationID: legacyGroup.ID,
				ThreadID:       dmKey,
			},
			wantContain: "non-DM conversation",
			checkGroup:  legacyGroup.ID,
		},
		{
			name: "native_group_conv_id_whitespace_dm_thread_id",
			msg: &messages.StructuredMessage{
				Msg:            "native group msg with whitespace dm thread_id",
				Type:           messages.TypeInstruction,
				Channel:        "web",
				ConversationID: nativeGroup.ID,
				ThreadID:       " " + dmKey,
			},
			wantContain: "non-DM conversation",
			checkGroup:  nativeGroup.ID,
		},
		{
			name: "direct_conv_id_whitespace_mismatched_dm_thread_id",
			msg: &messages.StructuredMessage{
				Msg:            "direct msg with whitespace mismatched dm thread_id",
				Type:           messages.TypeInstruction,
				Channel:        "web",
				ConversationID: dmConv.ID,
				ThreadID:       " " + otherDMKey,
			},
			wantContain: "does not match the direct conversation's key",
		},
		{
			name: "derived_group_whitespace_dm_thread_id",
			msg: &messages.StructuredMessage{
				Msg:      "derived group msg with whitespace dm thread_id",
				Type:     messages.TypeInstruction,
				Channel:  "web",
				ThreadID: " " + dmKey,
			},
			wantContain: "non-DM conversation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := postAgentMsg(t, tc.msg)
			require.Equal(t, http.StatusBadRequest, rr.Code,
				"%s: expected 400 BadRequest, got %d: %s", tc.name, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tc.wantContain)

			msgs, listErr := s.ListMessages(ctx, store.MessageFilter{RecipientID: agent.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, listErr)
			assert.Empty(t, msgs.Items, "rejected send must not persist a message")

			if tc.checkGroup != "" {
				assertAgentNotParticipant(t, ctx, s, tc.checkGroup, agent.ID)
			}
		})
	}
}
