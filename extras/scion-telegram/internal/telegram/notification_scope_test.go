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

package telegram

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- /notifications ---

func TestCommandHandler_Notifications_FreshCacheForUnreadableProjectIsLeftOut(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	principal := linkTestUser(t, store, 42, "alice@example.com")
	saveTestGroupLink(t, store, -101, "proj-1", "alpha", "")
	saveTestGroupLink(t, store, -102, "proj-2", "secret", "")
	ctx := context.Background()
	for _, pa := range []*ProjectAgents{
		{ProjectID: "proj-1", Agents: []AgentInfo{{Slug: "coder"}}, RefreshedAt: time.Now()},
		{ProjectID: "proj-2", Agents: []AgentInfo{{Slug: "hidden-agent"}}, RefreshedAt: time.Now()},
	} {
		require.NoError(t, store.SaveProjectAgents(ctx, pa))
	}
	hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1", Slug: "alpha"}}}
	hub.listAgentsErr = forbiddenListAgents()

	h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	require.NotNil(t, sent[0].ReplyMarkup)
	var labels []string
	for _, row := range sent[0].ReplyMarkup.InlineKeyboard {
		for _, btn := range row {
			labels = append(labels, btn.Text)
		}
	}
	joined := sent[0].Text + " " + joinStrings(labels)
	assert.Contains(t, joined, "coder")
	assert.NotContains(t, joined, "hidden-agent")
	assert.NotContains(t, joined, "secret")
}

func TestCommandHandler_Notifications_NoReadableProjects(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	principal := linkTestUser(t, store, 42, "alice@example.com")
	saveTestGroupLink(t, store, -102, "proj-2", "secret", "")
	saveStaleAgentCache(t, store, "proj-2", "hidden-agent")
	hub.userProjects = map[string][]ProjectOption{principal: {}}

	h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})

	assert.Empty(t, hub.agentCalls())
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "No linked projects found")
}

func TestCommandHandler_Notifications_ProjectListFailureIsReported(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	linkTestUser(t, store, 42, "alice@example.com")
	saveTestGroupLink(t, store, -101, "proj-1", "alpha", "")
	hub.listUserProjectsErr = errors.New("list user projects returned status 500")

	h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})

	assert.Empty(t, hub.agentCalls())
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, setupProjectsFailedText, sent[0].Text)
}

func joinStrings(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s + " "
	}
	return out
}

// --- notification DMs ---

func newDMScopeBroker(t *testing.T) (*TelegramBrokerV2, *fakeTGServerV2, *fakeHubClient, string) {
	t.Helper()
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)
	principal := linkTestUser(t, b.store, 456, "alice@example.com")
	return b, tgSrv, hub, principal
}

// stateChangeFor returns a state-change message with a unique timestamp so
// repeated publishes are not deduplicated.
func stateChangeFor(recipient string) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Sender:    "agent:coder",
		Recipient: recipient,
		Msg:       "Agent coder completed successfully",
		Type:      messages.TypeStateChange,
		Status:    "completed",
	}
}

func TestV2_StateChangeDM_SentOnlyForReadableProjects(t *testing.T) {
	b, tgSrv, hub, principal := newDMScopeBroker(t)
	hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1", Slug: "alpha"}}}
	ctx := context.Background()

	require.NoError(t, b.Publish(ctx, "scion.project.proj-2.agent.coder.messages", stateChangeFor("user:alice@example.com")))
	assert.Empty(t, tgSrv.getSentMessages(), "no DM for a project the recipient cannot read")

	require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", stateChangeFor("user:alice@example.com")))
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(456), sent[0].ChatID)

	// The recipient's project list is listed as that user, once per cache window.
	assert.Equal(t, []string{principal}, hub.listUserProjectsCalls)
}

func TestV2_StateChangeDM_NotSentWhenProjectListFails(t *testing.T) {
	b, tgSrv, hub, _ := newDMScopeBroker(t)
	hub.listUserProjectsErr = errors.New("list user projects returned status 500")

	require.NoError(t, b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", stateChangeFor("user:alice@example.com")))
	assert.Empty(t, tgSrv.getSentMessages())
}

func TestV2_StateChangeDM_ProjectListRefreshedAfterCacheWindow(t *testing.T) {
	b, tgSrv, hub, principal := newDMScopeBroker(t)
	hub.userProjects = map[string][]ProjectOption{principal: {}}
	ctx := context.Background()

	require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", stateChangeFor("user:alice@example.com")))
	assert.Empty(t, tgSrv.getSentMessages())

	// The user gains access; an expired cache entry picks it up.
	hub.mu.Lock()
	hub.userProjects[principal] = []ProjectOption{{ID: "proj-1"}}
	hub.mu.Unlock()
	b.userProjectsMu.Lock()
	e := b.userProjects[principal]
	e.fetchedAt = time.Now().Add(-2 * userProjectsCacheTTL)
	b.userProjects[principal] = e
	b.userProjectsMu.Unlock()

	require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", stateChangeFor("user:alice@example.com")))
	assert.Len(t, tgSrv.getSentMessages(), 1)
}

func TestV2_ResolveRecipientChats_OnlyForReadableProjects(t *testing.T) {
	b, _, hub, principal := newDMScopeBroker(t)
	hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1"}}}
	ctx := context.Background()
	for _, pid := range []string{"proj-1", "proj-2"} {
		require.NoError(t, b.store.SaveConversationContext(ctx, &ConversationContext{
			TelegramUserID: "456", ProjectID: pid, AgentSlug: "coder", LastChatID: 999, LastMessageAt: time.Now(),
		}))
	}

	assert.Equal(t, []int64{999}, b.resolveRecipientChats(ctx, "user:alice@example.com", "", "proj-1", "coder"))
	assert.Nil(t, b.resolveRecipientChats(ctx, "user:alice@example.com", "", "proj-2", "coder"))
}

func inputNeededFor(recipient string) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Sender:    "agent:coder",
		Recipient: recipient,
		Msg:       "Should I deploy?",
		Type:      messages.TypeInputNeeded,
	}
}

func TestV2_InputNeededDM_SentOnlyForReadableProjects(t *testing.T) {
	b, tgSrv, hub, principal := newDMScopeBroker(t)
	hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1", Slug: "alpha"}}}
	ctx := context.Background()

	require.NoError(t, b.Publish(ctx, "scion.project.proj-2.agent.coder.messages", inputNeededFor("user:alice@example.com")))
	assert.Empty(t, tgSrv.getSentMessages(), "no ask-user DM for a project the recipient cannot read")

	require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", inputNeededFor("user:alice@example.com")))
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(456), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "Should I deploy?")
}

func TestV2_InputNeededDM_NotSentWhenProjectListFails(t *testing.T) {
	b, tgSrv, hub, _ := newDMScopeBroker(t)
	hub.listUserProjectsErr = errors.New("list user projects returned status 500")

	require.NoError(t, b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", inputNeededFor("user:alice@example.com")))
	assert.Empty(t, tgSrv.getSentMessages())
}
