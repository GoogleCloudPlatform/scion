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

// The hub applies the shared agent-prefixed mention rule
// (rewriteIncomingAgentMentions) to every body it accepts, so a web, chat
// or agent sender that writes "@agent:<slug>" gets the same stored body and
// the same routed mention as a CLI sender, which rewrites before sending.
//
// Each send path has one test with three subtests:
//   - agent_prefixed: "@agent:<slug>" is stored as "@<slug>" and the
//     mention reaches <slug>.
//   - already_plain: a body the CLI already rewrote is stored unchanged
//     (no second rewrite).
//   - code_span: "@agent:<slug>" inside a code span is stored unchanged
//     and is not routed.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestRewriteIncomingAgentMentions(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"agent_prefixed", "hey @agent:foo, look", "hey @foo, look"},
		{"already_plain", "hey @foo, look", "hey @foo, look"},
		{"code_span", "run `@agent:foo` now", "run `@agent:foo` now"},
		{"quoted_line", "> @agent:foo said\nok", "> @agent:foo said\nok"},
		{"fenced_block", "```\n@agent:foo\n```", "```\n@agent:foo\n```"},
		{"no_mention", "plain text", "plain text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteIncomingAgentMentions(tc.in)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, rewriteIncomingAgentMentions(got), "rewrite must be idempotent")
		})
	}
}

// mentionRewriteCases are the three bodies each send path is checked with.
// slug is the agent the body names; wantRouted reports whether the mention
// must reach it.
func mentionRewriteCases(prefix, slug string) []struct {
	name, body, wantStored string
	wantRouted             bool
} {
	return []struct {
		name, body, wantStored string
		wantRouted             bool
	}{
		{"agent_prefixed", prefix + "@agent:" + slug + " please look", prefix + "@" + slug + " please look", true},
		{"already_plain", prefix + "@" + slug + " please look", prefix + "@" + slug + " please look", true},
		{"code_span", prefix + "`@agent:" + slug + "` please look", prefix + "`@agent:" + slug + "` please look", false},
	}
}

// handleAgentMessage: an agent's structured message to another agent.
func TestAgentMentionRewrite_AgentMessage(t *testing.T) {
	for _, tc := range mentionRewriteCases("hey ", mentionBystanderSlug) {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

			rr := sendViaStructured(t, srv, sender, target, tc.body)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
			var resp MessageDeliveryResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.MessageID)

			stored, err := s.GetMessage(context.Background(), resp.MessageID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStored, stored.Msg, "stored body")

			primary := dispatchesTo(dispatcher, target.ID)
			require.Len(t, primary, 1)
			require.NotNil(t, primary[0].StructuredMessage)
			assert.Equal(t, tc.wantStored, primary[0].StructuredMessage.Msg, "dispatched body")

			got := dispatchesTo(dispatcher, bystander.ID)
			if tc.wantRouted {
				require.Len(t, got, 1, "the mention must reach %s once", mentionBystanderSlug)
				require.NotNil(t, got[0].StructuredMessage)
				assert.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
			} else {
				assert.Empty(t, got, "a mention in a code span must not be routed")
			}
		})
	}
}

// handleAgentOutboundMessage: an agent's outbound message into a DM.
func TestAgentMentionRewrite_OutboundMessage(t *testing.T) {
	for _, tc := range mentionRewriteCases("looping in ", mentionBystanderSlug) {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, sender, target, bystander, dmConvID, dispatcher := mentionFanoutSetup(t)

			rr := sendViaOutbound(t, srv, sender, dmConvID, tc.body)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
			var resp MessageDeliveryResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.MessageID)

			stored, err := s.GetMessage(context.Background(), resp.MessageID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStored, stored.Msg, "stored body")

			primary := dispatchesTo(dispatcher, target.ID)
			require.Len(t, primary, 1)
			require.NotNil(t, primary[0].StructuredMessage)
			assert.Equal(t, tc.wantStored, primary[0].StructuredMessage.Msg, "dispatched body")

			got := dispatchesTo(dispatcher, bystander.ID)
			if tc.wantRouted {
				require.Len(t, got, 1, "the mention must reach %s once", mentionBystanderSlug)
				require.NotNil(t, got[0].StructuredMessage)
				assert.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
			} else {
				assert.Empty(t, got, "a mention in a code span must not be routed")
			}
		})
	}
}

// sendChatMessage (POST /chat/conversations/{key}/messages): a web chat
// send to a topic whose default agent is not the mentioned one. A leading
// mention that resolves overrides the default, so only the mentioned agent
// is dispatched; an unrouted body goes to the default agent.
func TestAgentMentionRewrite_ChatSend(t *testing.T) {
	for _, tc := range mentionRewriteCases("", "rw-mentioned") {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, wcs, proj, db := setupSendTest(t)
			enableEnvelopeSwitch(t, srv, s)
			ctx := t.Context()

			dispatcher := &brokerMockDispatcher{}
			srv.SetDispatcher(dispatcher)

			for _, slug := range []string{"rw-default", "rw-mentioned"} {
				require.NoError(t, s.CreateAgent(ctx, &store.Agent{
					ID:        tid(slug),
					ProjectID: proj.ID,
					Name:      slug,
					Slug:      slug,
					Phase:     "idle",
					OwnerID:   DevUserID,
					CreatedBy: DevUserID,
				}))
			}
			topicID := tid("rw-topic")
			require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
				ID:           topicID,
				ProjectID:    proj.ID,
				Name:         "mention-rewrite",
				CreatedBy:    "dev",
				CreatedAt:    time.Now().UTC(),
				DefaultAgent: "rw-default",
			}))
			setTopicConversationID(t, db, s, topicID, proj.ID)

			rec := doRequest(t, srv, http.MethodPost,
				"/api/v1/chat/conversations/"+topicID+"/messages",
				map[string]string{"content": tc.body})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var resp chatMessageResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantStored, resp.Content, "rendered body")

			stored, err := s.GetMessage(ctx, resp.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStored, stored.Msg, "stored body")

			dispatched := dispatcher.getMessages()
			require.Len(t, dispatched, 1)
			if tc.wantRouted {
				assert.Equal(t, "rw-mentioned", dispatched[0].agentSlug, "the leading mention must route to the plain slug")
			} else {
				assert.Equal(t, "rw-default", dispatched[0].agentSlug, "a mention in a code span must not be routed")
			}
		})
	}
}

// handleScheduledCreate: a scheduled chat message is stored with the
// plain form when it is scheduled, and is sent with it when it fires.
func TestAgentMentionRewrite_ChatScheduledSend(t *testing.T) {
	for _, tc := range mentionRewriteCases("", "sched-agent") {
		t.Run(tc.name, func(t *testing.T) {
			f := newScheduledSendFixture(t)
			ctx := context.Background()
			fireAt := time.Now().Add(2 * time.Minute)

			sm := f.schedule(t, f.bob, tc.body, fireAt)
			assert.Equal(t, tc.wantStored, f.row(t, f.bob, sm.ID).Content, "scheduled body")

			require.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)))
			msgs := f.topicMessages(t)
			require.Len(t, msgs, 1)
			assert.Equal(t, tc.wantStored, msgs[0].Msg, "stored body")
		})
	}
}

// handleBrokerInboundRouted: a chat integration message routed by its
// mentions. alpha is the default agent; the mention adds beta.
func TestAgentMentionRewrite_BrokerInboundRouted(t *testing.T) {
	for _, tc := range mentionRewriteCases("hello ", "beta") {
		t.Run(tc.name, func(t *testing.T) {
			env := setupRoutedTestEnv(t)

			rec := env.doRoutedRequest(t, routedInboundRequest{
				ProjectID:    env.project.ID,
				DefaultAgent: "alpha",
				Message: &messages.StructuredMessage{
					Version: messages.Version,
					Channel: "slack",
					Sender:  "user:" + env.user.Email,
					Msg:     tc.body,
					Type:    messages.TypeInstruction,
				},
			})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var resp routedInboundResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			assert.Equal(t, "alpha", resp.PrimaryAgent)
			assert.Empty(t, resp.UnresolvedMentions)

			msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
				AgentID: env.agent1.ID,
			}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.NotEmpty(t, msgs.Items)
			assert.Equal(t, tc.wantStored, msgs.Items[0].Msg, "stored body")

			calls := env.dispatcher.getCalls()
			require.NotEmpty(t, calls)
			require.NotNil(t, calls[0].StructuredMessage)
			assert.Equal(t, tc.wantStored, calls[0].StructuredMessage.Msg, "dispatched body")

			if tc.wantRouted {
				require.Len(t, resp.Results, 2)
				assert.Equal(t, "beta", resp.Results[1].AgentSlug)
				assert.Equal(t, "mention", resp.Results[1].Type)
				assert.Equal(t, "delivered", resp.Results[1].Status)
			} else {
				require.Len(t, resp.Results, 1, "a mention in a code span must not be routed")
			}
		})
	}
}

// handleBrokerInbound: a chat integration message addressed to one named
// agent. It extracts no mentions, so the check is the stored and dispatched
// body only.
func TestAgentMentionRewrite_BrokerInbound(t *testing.T) {
	for _, tc := range mentionRewriteCases("ping ", "reviewer") {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			dispatcher := &recordingDispatcher{}
			srv.SetDispatcher(dispatcher)
			enableWriteDenySwitch(t, srv)

			owner := &store.User{
				ID: tid("owner-rw"), Email: "rw-owner@example.com", DisplayName: "RW Owner",
				Role: store.UserRoleMember, Status: "active", Created: time.Now(),
			}
			require.NoError(t, s.CreateUser(ctx, owner))
			ensureHubMembership(ctx, s, owner.ID)
			user := &store.User{
				ID: tid("user-rw"), Email: "rw-user@example.com", DisplayName: "RW User",
				Role: store.UserRoleMember, Status: "active", Created: time.Now(),
			}
			require.NoError(t, s.CreateUser(ctx, user))
			ensureHubMembership(ctx, s, user.ID)

			project := &store.Project{
				ID: tid("proj-rw"), Slug: "rw-proj", Name: "RW Project",
				OwnerID: owner.ID, CreatedBy: owner.ID, Created: time.Now(), Updated: time.Now(),
			}
			require.NoError(t, s.CreateProject(ctx, project))
			srv.seedProjectCreatorMembership(ctx, project)
			msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)
			msgAuthzGrantAgentMessage(t, s, user.ID, project.ID)

			agent := &store.Agent{
				ID: tid("agent-rw"), Slug: "rw-agent", Name: "RW Agent", ProjectID: project.ID,
				Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
				StateVersion: 1, Created: time.Now(), Updated: time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			body, err := json.Marshal(inboundMessageRequest{
				Topic: "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
				Message: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Channel:   "discord",
					Sender:    "user:" + user.Email,
					Recipient: "agent:" + agent.Slug,
					Msg:       tc.body,
					Type:      messages.TypeInstruction,
				},
			})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			calls := dispatcher.getCalls()
			require.Len(t, calls, 1)
			assert.Equal(t, tc.wantStored, calls[0].Message, "dispatched body")

			msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, msgs.Items, 1)
			assert.Equal(t, tc.wantStored, msgs.Items[0].Msg, "stored body")
		})
	}
}
