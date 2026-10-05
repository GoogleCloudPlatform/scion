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
	"fmt"
	"log/slog"
	"sync"
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

	chats, denied := b.resolveRecipientChats(ctx, "user:alice@example.com", "", "proj-1", "coder")
	assert.Equal(t, []int64{999}, chats)
	assert.False(t, denied)
	chats, denied = b.resolveRecipientChats(ctx, "user:alice@example.com", "", "proj-2", "coder")
	assert.Nil(t, chats)
	assert.True(t, denied)
	chats, denied = b.resolveRecipientChats(ctx, "user:nobody@example.com", "", "proj-2", "coder")
	assert.Nil(t, chats)
	assert.False(t, denied, "an unknown recipient is not a denial")
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

// --- notification toggle presses ---

func notifyCallback(projectID, agentSlug string, fromID int64) *CallbackQuery {
	return &CallbackQuery{
		ID:      "cb-n",
		From:    &TGUser{ID: fromID},
		Message: &TGMessage{MessageID: 9, Chat: TGChat{ID: fromID, Type: "private"}},
		Data:    "notify:" + projectID + ":" + agentSlug,
	}
}

func markupButtonTexts(kb *InlineKeyboardMarkup) string {
	out := ""
	if kb == nil {
		return out
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			out += btn.Text + "|" + btn.CallbackData + " "
		}
	}
	return out
}

// setupToggleScope links two projects with fresh agent caches; the user
// can read only proj-1.
func setupToggleScope(t *testing.T) (*CallbackHandler, *fakeTGServerV2, *fakeHubClient, Store) {
	t.Helper()
	h, tgSrv, hub, store := newTestCallbackHandler(t)
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
	return h, tgSrv, hub, store
}

func TestCallbackHandler_NotifyToggle_FreshCacheForUnreadableProjectIsLeftOut(t *testing.T) {
	h, tgSrv, _, store := setupToggleScope(t)

	_, err := h.HandleCallback(context.Background(), notifyCallback("proj-1", "coder", 42))
	require.NoError(t, err)

	edits := tgSrv.getEditedMarkups()
	require.Len(t, edits, 1)
	buttons := markupButtonTexts(edits[0].ReplyMarkup)
	assert.Contains(t, buttons, "coder")
	assert.NotContains(t, buttons, "hidden-agent")
	assert.NotContains(t, buttons, "proj-2")

	pref, err := store.GetNotificationPref(context.Background(), "42", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, pref)
	assert.False(t, pref.Enabled, "first press turns notifications off")
}

func TestCallbackHandler_NotifyToggle_UnreadableProjectIsRejected(t *testing.T) {
	h, tgSrv, _, store := setupToggleScope(t)

	_, err := h.HandleCallback(context.Background(), notifyCallback("proj-2", "hidden-agent", 42))
	require.NoError(t, err)

	assert.Empty(t, tgSrv.getEditedMarkups())
	pref, err := store.GetNotificationPref(context.Background(), "42", "proj-2", "hidden-agent")
	require.NoError(t, err)
	assert.Nil(t, pref)
	answered := tgSrv.getAnsweredCallbacks()
	require.Len(t, answered, 1)
	assert.Contains(t, answered[0].Text, "can no longer read this project")
}

func TestCallbackHandler_NotifyToggle_ListFailureKeepsMarkup(t *testing.T) {
	for name, listErr := range map[string]error{
		"stale link":   staleLinkError("on-behalf-of principal not found"),
		"list failure": errors.New("list user projects returned status 500"),
	} {
		t.Run(name, func(t *testing.T) {
			h, tgSrv, hub, store := setupToggleScope(t)
			hub.listUserProjectsErr = listErr

			_, err := h.HandleCallback(context.Background(), notifyCallback("proj-1", "coder", 42))
			require.NoError(t, err)

			assert.Empty(t, tgSrv.getEditedMarkups(), "existing keyboard is kept")
			pref, err := store.GetNotificationPref(context.Background(), "42", "proj-1", "coder")
			require.NoError(t, err)
			assert.Nil(t, pref, "preference unchanged")
			answered := tgSrv.getAnsweredCallbacks()
			require.Len(t, answered, 1)
			assert.Equal(t, hubErrorText(listErr, "alice@example.com", "", setupProjectsFailedText), answered[0].Text)
		})
	}
}

func TestCallbackHandler_NotifyToggle_UnlinkedUserGetsRegisterHint(t *testing.T) {
	h, tgSrv, hub, _ := newTestCallbackHandler(t)

	_, err := h.HandleCallback(context.Background(), notifyCallback("proj-1", "coder", 77))
	require.NoError(t, err)

	assert.Empty(t, hub.listUserProjectsCalls)
	assert.Empty(t, tgSrv.getEditedMarkups())
	answered := tgSrv.getAnsweredCallbacks()
	require.Len(t, answered, 1)
	assert.Equal(t, registerHint, answered[0].Text)
}

// --- recipient project cache ---

// canRead returns only the readable result of recipientCanReadProject.
func canRead(b *TelegramBrokerV2, ctx context.Context, m *TelegramUserMapping, projectID string) bool {
	ok, _ := b.recipientCanReadProject(ctx, m, projectID)
	return ok
}

func TestV2_RecipientCanReadProject_FailureIsCachedBriefly(t *testing.T) {
	b, _, hub, _ := newDMScopeBroker(t)
	hub.listUserProjectsErr = errors.New("list user projects returned status 500")
	mapping, err := b.store.GetUserMapping(context.Background(), "456")
	require.NoError(t, err)

	assert.False(t, canRead(b, context.Background(), mapping, "proj-1"))
	assert.False(t, canRead(b, context.Background(), mapping, "proj-1"))
	assert.Len(t, hub.userProjectCalls(), 1, "a failed list is remembered")

	// After the failure window the list is fetched again.
	hub.mu.Lock()
	hub.listUserProjectsErr = nil
	hub.projects = []ProjectOption{{ID: "proj-1"}}
	hub.mu.Unlock()
	b.userProjectsMu.Lock()
	e := b.userProjects["user:alice@example.com"]
	e.fetchedAt = time.Now().Add(-2 * userProjectsFailureTTL)
	b.userProjects["user:alice@example.com"] = e
	b.userProjectsMu.Unlock()

	assert.True(t, canRead(b, context.Background(), mapping, "proj-1"))
	assert.Len(t, hub.userProjectCalls(), 2)
}

func TestV2_RecipientCanReadProject_ConcurrentMissesMakeOneHubCall(t *testing.T) {
	b, _, hub, _ := newDMScopeBroker(t)
	hub.projects = []ProjectOption{{ID: "proj-1"}}
	gate := make(chan struct{})
	entered := make(chan struct{}, 16)
	hub.listUserProjectsGate = gate
	hub.listUserProjectsEntered = entered
	mapping, err := b.store.GetUserMapping(context.Background(), "456")
	require.NoError(t, err)

	const n = 8
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() { results <- canRead(b, context.Background(), mapping, "proj-1") }()
	}
	// Callers that join while the hub call is held share it; callers that
	// arrive after it finished read its cached result. Either way there is
	// one hub call.
	<-entered
	close(gate)
	for i := 0; i < n; i++ {
		assert.True(t, <-results)
	}
	assert.Len(t, hub.userProjectCalls(), 1)
}

func TestV2_RecipientCanReadProject_CallerStopsWaitingSharedCallFillsCache(t *testing.T) {
	b, _, hub, _ := newDMScopeBroker(t)
	hub.projects = []ProjectOption{{ID: "proj-1"}}
	gate := make(chan struct{})
	entered := make(chan struct{}, 4)
	hub.listUserProjectsGate = gate
	hub.listUserProjectsEntered = entered
	mapping, err := b.store.GetUserMapping(context.Background(), "456")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() { result <- canRead(b, ctx, mapping, "proj-1") }()
	<-entered
	cancel()
	assert.False(t, <-result, "a caller that stops waiting fails closed")

	// The shared call is still running; once it finishes it fills the cache.
	close(gate)
	require.Eventually(t, func() bool {
		_, ok := b.cachedUserProjects("user:alice@example.com")
		return ok
	}, 2*time.Second, 5*time.Millisecond)
	assert.True(t, canRead(b, context.Background(), mapping, "proj-1"))
	assert.Len(t, hub.userProjectCalls(), 1)
}

func TestV2_FetchUserProjects_TimeoutIsCached(t *testing.T) {
	for name, ctxErr := range map[string]error{"canceled": context.Canceled, "deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			b, _, hub, _ := newDMScopeBroker(t)
			hub.listUserProjectsErr = fmt.Errorf("list user projects request failed: %w", ctxErr)
			mapping, err := b.store.GetUserMapping(context.Background(), "456")
			require.NoError(t, err)

			assert.False(t, canRead(b, context.Background(), mapping, "proj-1"))
			e, cached := b.cachedUserProjects("user:alice@example.com")
			require.True(t, cached, "a timed-out shared call is remembered")
			assert.True(t, e.failed)

			// Within the failure window no new hub call is made.
			assert.False(t, canRead(b, context.Background(), mapping, "proj-1"))
			assert.Len(t, hub.userProjectCalls(), 1)
		})
	}
}

func TestV2_RecipientCanReadProject_EvictsExpiredEntriesOnWrite(t *testing.T) {
	b, _, hub, _ := newDMScopeBroker(t)
	hub.projects = []ProjectOption{{ID: "proj-1"}}
	b.userProjects = map[string]userProjectsEntry{
		"user:old@example.com":   {ids: map[string]bool{"proj-9": true}, fetchedAt: time.Now().Add(-2 * userProjectsCacheTTL)},
		"user:fresh@example.com": {ids: map[string]bool{"proj-9": true}, fetchedAt: time.Now()},
	}
	mapping, err := b.store.GetUserMapping(context.Background(), "456")
	require.NoError(t, err)

	assert.True(t, canRead(b, context.Background(), mapping, "proj-1"))

	b.userProjectsMu.Lock()
	defer b.userProjectsMu.Unlock()
	assert.NotContains(t, b.userProjects, "user:old@example.com")
	assert.Contains(t, b.userProjects, "user:fresh@example.com")
	assert.Contains(t, b.userProjects, "user:alice@example.com")
}

func TestCallbackHandler_NotifyToggle_UnlistedAgentIsRejected(t *testing.T) {
	h, tgSrv, _, store := setupToggleScope(t)

	_, err := h.HandleCallback(context.Background(), notifyCallback("proj-1", "ghost-agent", 42))
	require.NoError(t, err)

	assert.Empty(t, tgSrv.getEditedMarkups())
	pref, err := store.GetNotificationPref(context.Background(), "42", "proj-1", "ghost-agent")
	require.NoError(t, err)
	assert.Nil(t, pref, "no preference saved for an agent that is not listed")
	answered := tgSrv.getAnsweredCallbacks()
	require.Len(t, answered, 1)
	assert.Contains(t, answered[0].Text, "no longer available")
}

func TestV2_StateChangeDM_DisabledPrefSkipsProjectCheck(t *testing.T) {
	b, tgSrv, hub, principal := newDMScopeBroker(t)
	hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1"}}}
	require.NoError(t, b.store.SaveNotificationPref(context.Background(), &NotificationPref{
		TelegramUserID: "456", ProjectID: "proj-1", AgentSlug: "coder", Enabled: false,
	}))

	require.NoError(t, b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", stateChangeFor("user:alice@example.com")))

	assert.Empty(t, tgSrv.getSentMessages())
	assert.Empty(t, hub.userProjectCalls(), "the local preference is checked first")
}

func TestCommandHandler_Notifications_RequiresUsableLink(t *testing.T) {
	t.Run("unlinked", func(t *testing.T) {
		h, tgSrv, hub, _ := newTestCommandHandler(t)
		h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})
		sent := tgSrv.getSentMessages()
		require.Len(t, sent, 1)
		assert.Equal(t, registerHint, sent[0].Text)
		assert.Empty(t, hub.userProjectCalls())
	})
	t.Run("link without email", func(t *testing.T) {
		h, tgSrv, hub, store := newTestCommandHandler(t)
		require.NoError(t, store.SaveUserMapping(context.Background(), &TelegramUserMapping{
			TelegramUserID: "42", ScionUserID: "u-42", LinkedAt: time.Now().UTC(),
		}))
		h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})
		sent := tgSrv.getSentMessages()
		require.Len(t, sent, 1)
		assert.Equal(t, staleLinkText, sent[0].Text)
		assert.Empty(t, hub.userProjectCalls())
	})
}

// --- Publish routing for user recipients ---

func agentReplyFor(recipient string) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Sender:    "agent:coder",
		Recipient: recipient,
		Msg:       "Here is the deploy plan",
		Type:      messages.TypeInstruction,
	}
}

func TestV2_Publish_DeniedRecipientIsNotBroadcast(t *testing.T) {
	for name, setup := range map[string]func(*fakeHubClient, string){
		"cannot read project": func(h *fakeHubClient, p string) { h.userProjects = map[string][]ProjectOption{p: {}} },
		"check fails closed": func(h *fakeHubClient, _ string) {
			h.listUserProjectsErr = errors.New("list user projects returned status 500")
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, tgSrv, hub, principal := newDMScopeBroker(t)
			setup(hub, principal)
			saveTestGroupLink(t, b.store, -200, "proj-1", "alpha", "")
			require.NoError(t, b.store.SaveConversationContext(context.Background(), &ConversationContext{
				TelegramUserID: "456", ProjectID: "proj-1", AgentSlug: "coder", LastChatID: -200, LastMessageAt: time.Now(),
			}))

			require.NoError(t, b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", agentReplyFor("user:alice@example.com")))

			assert.Empty(t, tgSrv.getSentMessages(), "the message is dropped, not broadcast to the project's groups")
		})
	}
}

func TestV2_Publish_RecipientWithoutContextStillBroadcasts(t *testing.T) {
	for name, recipient := range map[string]string{
		"unknown user recipient": "user:nobody@example.com",
		"no recipient":           "",
		"linked recipient who can read, without context": "user:alice@example.com",
	} {
		t.Run(name, func(t *testing.T) {
			b, tgSrv, hub, principal := newDMScopeBroker(t)
			hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1"}}}
			saveTestGroupLink(t, b.store, -200, "proj-1", "alpha", "")

			require.NoError(t, b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", agentReplyFor(recipient)))

			sent := tgSrv.getSentMessages()
			require.Len(t, sent, 1)
			assert.Equal(t, int64(-200), sent[0].ChatID)
		})
	}
}

// levelRecorder is a slog handler that records messages with their level.
type levelRecorder struct {
	mu      sync.Mutex
	records map[string]slog.Level
}

func (r *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *levelRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records == nil {
		r.records = make(map[string]slog.Level)
	}
	r.records[rec.Message] = rec.Level
	return nil
}
func (r *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *levelRecorder) WithGroup(string) slog.Handler      { return r }
func (r *levelRecorder) level(msg string) (slog.Level, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.records[msg]
	return l, ok
}

func TestV2_RecipientCanReadProject_Reasons(t *testing.T) {
	t.Run("cannot read", func(t *testing.T) {
		b, _, hub, principal := newDMScopeBroker(t)
		hub.userProjects = map[string][]ProjectOption{principal: {}}
		m, _ := b.store.GetUserMapping(context.Background(), "456")
		ok, reason := b.recipientCanReadProject(context.Background(), m, "proj-1")
		assert.False(t, ok)
		assert.Equal(t, readDenialCannotRead, reason)
	})
	t.Run("check failed", func(t *testing.T) {
		b, _, hub, _ := newDMScopeBroker(t)
		hub.listUserProjectsErr = errors.New("list user projects returned status 500")
		m, _ := b.store.GetUserMapping(context.Background(), "456")
		ok, reason := b.recipientCanReadProject(context.Background(), m, "proj-1")
		assert.False(t, ok)
		assert.Equal(t, readDenialCheckFailed, reason)
	})
	t.Run("caller gave up", func(t *testing.T) {
		b, _, hub, _ := newDMScopeBroker(t)
		gate := make(chan struct{})
		entered := make(chan struct{}, 2)
		hub.listUserProjectsGate = gate
		hub.listUserProjectsEntered = entered
		defer close(gate)
		m, _ := b.store.GetUserMapping(context.Background(), "456")
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-entered; cancel() }()
		ok, reason := b.recipientCanReadProject(ctx, m, "proj-1")
		assert.False(t, ok)
		assert.Equal(t, readDenialCallerGaveUp, reason)
	})
	t.Run("readable", func(t *testing.T) {
		b, _, hub, principal := newDMScopeBroker(t)
		hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1"}}}
		m, _ := b.store.GetUserMapping(context.Background(), "456")
		ok, reason := b.recipientCanReadProject(context.Background(), m, "proj-1")
		assert.True(t, ok)
		assert.Empty(t, reason)
	})
}

func TestV2_NotificationDrops_LogLevelByReason(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*fakeHubClient, string)
		want  slog.Level
	}{
		"plain denial at debug":    {func(h *fakeHubClient, p string) { h.userProjects = map[string][]ProjectOption{p: {}} }, slog.LevelDebug},
		"fail-closed drop at warn": {func(h *fakeHubClient, _ string) { h.listUserProjectsErr = errors.New("status 500") }, slog.LevelWarn},
	} {
		t.Run(name, func(t *testing.T) {
			b, _, hub, principal := newDMScopeBroker(t)
			tc.setup(hub, principal)
			rec := &levelRecorder{}
			b.log = slog.New(rec)
			ctx := context.Background()
			require.NoError(t, b.store.SaveConversationContext(ctx, &ConversationContext{
				TelegramUserID: "456", ProjectID: "proj-1", AgentSlug: "coder", LastChatID: -200, LastMessageAt: time.Now(),
			}))

			require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", stateChangeFor("user:alice@example.com")))
			require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", inputNeededFor("user:alice@example.com")))
			require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", agentReplyFor("user:alice@example.com")))

			for _, msg := range []string{"Dropping state-change DM", "Dropping input-needed DM", "Not routing to recipient's chat"} {
				lvl, ok := rec.level(msg)
				require.True(t, ok, msg)
				assert.Equal(t, tc.want, lvl, msg)
			}
		})
	}
}
