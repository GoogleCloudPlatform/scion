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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCommandHandler(t *testing.T) (*CommandHandler, *fakeTGServerV2, *fakeHubClient, Store) {
	t.Helper()
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	store := newTestStore(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	h := NewCommandHandler(store, api, hub, "test_bot", slog.Default())
	return h, tgSrv, hub, store
}

// linkTestUser stores a link mapping for a Telegram user and returns the
// principal the plugin acts as for that user.
func linkTestUser(t *testing.T, store Store, telegramUserID int64, email string) string {
	t.Helper()
	require.NoError(t, store.SaveUserMapping(context.Background(), &TelegramUserMapping{
		TelegramUserID: strconv.FormatInt(telegramUserID, 10),
		ScionUserID:    "scion-" + strconv.FormatInt(telegramUserID, 10),
		ScionEmail:     email,
		LinkedAt:       time.Now().UTC(),
	}))
	return "user:" + email
}

func TestCommandHandler_HandleCommand_UnrecognizedReturnsFalse(t *testing.T) {
	h, _, _, _ := newTestCommandHandler(t)

	got := h.HandleCommand(&TGMessage{
		Text: "/unknown_command",
		Chat: TGChat{ID: -100, Type: "group"},
	})
	assert.False(t, got)
}

func TestCommandHandler_HandleCommand_NilMessage(t *testing.T) {
	h, _, _, _ := newTestCommandHandler(t)
	assert.False(t, h.HandleCommand(nil))
}

func TestCommandHandler_HandleCommand_NotACommand(t *testing.T) {
	h, _, _, _ := newTestCommandHandler(t)
	assert.False(t, h.HandleCommand(&TGMessage{
		Text: "hello world",
		Chat: TGChat{ID: -100, Type: "group"},
	}))
}

func TestCommandHandler_HandleCommand_WithBotSuffix(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	got := h.HandleCommand(&TGMessage{
		Text: "/help@test_bot",
		Chat: TGChat{ID: -100, Type: "group"},
	})
	assert.True(t, got)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "/setup")
}

// --- /setup ---

func TestCommandHandler_Setup_InDM(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: 456, Type: "private"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "group chat")
}

func TestCommandHandler_Setup_InGroup_NoProjects(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	hub.projects = []ProjectOption{}
	linkTestUser(t, store, 42, "alice@example.com")

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: -100, Type: "group"},
		From: &TGUser{ID: 42},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, noUserProjectsText, sent[0].Text)
}

func TestCommandHandler_Setup_InGroup_WithProjects(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	hub.projects = []ProjectOption{
		{ID: "proj-1", Slug: "my-project"},
		{ID: "proj-2", Slug: "other-project"},
	}
	linkTestUser(t, store, 42, "alice@example.com")

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: -100, Type: "group"},
		From: &TGUser{ID: 42},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Select a project")
	require.NotNil(t, sent[0].ReplyMarkup)
}

func TestCommandHandler_Setup_OffersOnlyTheUsersProjects(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	principal := linkTestUser(t, store, 42, "alice@example.com")
	hub.projects = []ProjectOption{{ID: "proj-1", Slug: "alpha"}, {ID: "proj-2", Slug: "beta"}}
	hub.userProjects = map[string][]ProjectOption{principal: {{ID: "proj-1", Slug: "alpha"}}}
	h.SetProjects([]ProjectOption{{ID: "proj-3", Slug: "cached-gamma"}})

	h.HandleCommand(&TGMessage{Text: "/setup", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	require.NotNil(t, sent[0].ReplyMarkup)
	var data []string
	for _, row := range sent[0].ReplyMarkup.InlineKeyboard {
		for _, btn := range row {
			data = append(data, btn.CallbackData)
		}
	}
	assert.Contains(t, data, "setup:proj:proj-1")
	assert.NotContains(t, data, "setup:proj:proj-2")
	assert.NotContains(t, data, "setup:proj:proj-3")
	assert.Zero(t, hub.listFreshCalls, "setup does not use the broker project list")
}

func TestCommandHandler_Setup_DoesNotFallBackToBrokerOrCachedProjects(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	principal := linkTestUser(t, store, 42, "alice@example.com")
	hub.projects = []ProjectOption{{ID: "proj-2", Slug: "beta"}}
	hub.userProjects = map[string][]ProjectOption{principal: {}}
	h.SetProjects([]ProjectOption{{ID: "proj-3", Slug: "cached-gamma"}})

	h.HandleCommand(&TGMessage{Text: "/setup", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, noUserProjectsText, sent[0].Text)
	assert.Nil(t, sent[0].ReplyMarkup)
	assert.Zero(t, hub.listFreshCalls)
}

func TestCommandHandler_Setup_ProjectListFailureIsReported(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	linkTestUser(t, store, 42, "alice@example.com")
	hub.projects = []ProjectOption{{ID: "proj-2", Slug: "beta"}}
	hub.listUserProjectsErr = errors.New("list user projects returned status 500")

	h.HandleCommand(&TGMessage{Text: "/setup", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, setupProjectsFailedText, sent[0].Text)
	assert.Zero(t, hub.listFreshCalls)
}

func TestCommandHandler_Setup_StaleLinkShowsReregisterText(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	linkTestUser(t, store, 42, "alice@example.com")
	hub.listUserProjectsErr = staleLinkError("on-behalf-of principal not found")

	h.HandleCommand(&TGMessage{Text: "/setup", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, staleLinkText, sent[0].Text)
}

func TestCommandHandler_Setup_RequiresLinkedUser(t *testing.T) {
	h, tgSrv, hub, _ := newTestCommandHandler(t)
	hub.projects = []ProjectOption{{ID: "proj-1", Slug: "alpha"}}
	h.SetProjects([]ProjectOption{{ID: "proj-1", Slug: "alpha"}})

	h.HandleCommand(&TGMessage{Text: "/setup", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, registerHint, sent[0].Text)
	assert.Nil(t, sent[0].ReplyMarkup)
	assert.Empty(t, hub.listUserProjectsCalls)
	assert.Zero(t, hub.listFreshCalls)
}

func TestCommandHandler_Setup_AlreadyLinked(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	linkTestUser(t, store, 42, "alice@example.com")

	ctx := context.Background()
	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:      -100,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		LinkedAt:    time.Now().UTC(),
		Active:      true,
	}))

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: -100, Type: "group"},
		From: &TGUser{ID: 42},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "already linked")
	assert.Contains(t, sent[0].Text, "my-project")
	require.NotNil(t, sent[0].ReplyMarkup)
}

// --- /agents ---

func TestCommandHandler_Agents_NotLinked(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/agents",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "not linked")
}

func TestCommandHandler_Agents_WithAgents(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)

	ctx := context.Background()
	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -100,
		ProjectID:    "proj-1",
		ProjectSlug:  "my-project",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder", Activity: "executing"}, {Slug: "reviewer", Activity: "idle"}}

	linkTestUser(t, store, 42, "alice@example.com")

	h.HandleCommand(&TGMessage{
		Text: "/agents",
		Chat: TGChat{ID: -100, Type: "group"},
		From: &TGUser{ID: 42},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "coder")
	assert.Contains(t, sent[0].Text, "reviewer")
	assert.Contains(t, sent[0].Text, "(default)")
	assert.Contains(t, sent[0].Text, "executing")
	assert.Contains(t, sent[0].Text, "idle")
}

func TestCommandHandler_Agents_NoAgents(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)

	ctx := context.Background()
	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))
	hub.agents["proj-1"] = []AgentInfo{}

	linkTestUser(t, store, 42, "alice@example.com")

	h.HandleCommand(&TGMessage{
		Text: "/agents",
		Chat: TGChat{ID: -100, Type: "group"},
		From: &TGUser{ID: 42},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "No agents found")
}

// --- /help ---

func TestCommandHandler_Help_Group(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/help",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "/setup")
	assert.Contains(t, sent[0].Text, "/agents")
	assert.Contains(t, sent[0].Text, "/unlink")
}

func TestCommandHandler_Help_DM(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/help",
		Chat: TGChat{ID: 456, Type: "private"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "/register")
	assert.Contains(t, sent[0].Text, "/setup")
}

// --- /unlink ---

func TestCommandHandler_Unlink_NotLinked(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/unlink",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "not linked")
}

func TestCommandHandler_Unlink_ByLinker(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)

	ctx := context.Background()
	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:      -100,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		LinkedBy:    "456",
		LinkedAt:    time.Now().UTC(),
		Active:      true,
	}))

	h.HandleCommand(&TGMessage{
		Text: "/unlink",
		From: &TGUser{ID: 456, Username: "alice"},
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "unlinked")

	got, err := store.GetGroupLink(ctx, -100)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestCommandHandler_Unlink_WrongUser(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)

	ctx := context.Background()
	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:   -100,
		LinkedBy: "789",
		LinkedAt: time.Now().UTC(),
		Active:   true,
	}))

	h.HandleCommand(&TGMessage{
		Text: "/unlink",
		From: &TGUser{ID: 456, Username: "alice"},
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Only the user who linked")
}

// --- /status ---

func TestCommandHandler_Status_InGroup(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/status",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "direct message")
}

func TestCommandHandler_Status_InDM_Unregistered(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/status",
		From: &TGUser{ID: 456, Username: "alice"},
		Chat: TGChat{ID: 456, Type: "private"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Not registered")
	assert.Contains(t, sent[0].Text, "No groups")
}

func TestCommandHandler_Status_InDM_NoLinks(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)

	ctx := context.Background()
	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	h.HandleCommand(&TGMessage{
		Text: "/status",
		From: &TGUser{ID: 456, Username: "alice"},
		Chat: TGChat{ID: 456, Type: "private"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "No groups")
}

func TestCommandHandler_Status_InDM_WithLinks(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)

	ctx := context.Background()
	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -100,
		ChatTitle:    "Dev Group",
		ProjectSlug:  "my-project",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	tgSrv.setChatMember(-100, 456, "member")

	h.HandleCommand(&TGMessage{
		Text: "/status",
		From: &TGUser{ID: 456, Username: "alice"},
		Chat: TGChat{ID: 456, Type: "private"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Dev Group")
	assert.Contains(t, sent[0].Text, "my-project")
	assert.Contains(t, sent[0].Text, "coder")
}

// --- /settings ---

func TestCommandHandler_Settings_NotLinked(t *testing.T) {
	h, tgSrv, _, _ := newTestCommandHandler(t)

	h.HandleCommand(&TGMessage{
		Text: "/settings",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "not linked")
}

func TestCommandHandler_Settings_Linked(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)

	ctx := context.Background()
	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))

	h.HandleCommand(&TGMessage{
		Text: "/settings",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "settings")
	require.NotNil(t, sent[0].ReplyMarkup)
}

// --- linked user on hub reads ---

func saveTestGroupLink(t *testing.T, store Store, chatID int64, projectID, slug, defaultAgent string) {
	t.Helper()
	require.NoError(t, store.SaveGroupLink(context.Background(), &GroupLink{
		ChatID:       chatID,
		ProjectID:    projectID,
		ProjectSlug:  slug,
		DefaultAgent: defaultAgent,
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
}

func TestCommandHandler_GroupCommands_SendLinkedUser(t *testing.T) {
	for _, text := range []string{"/agents", "/default", "/terminal coder"} {
		t.Run(text, func(t *testing.T) {
			h, _, hub, store := newTestCommandHandler(t)
			saveTestGroupLink(t, store, -100, "proj-1", "my-project", "coder")
			hub.agents["proj-1"] = []AgentInfo{{ID: "a1", Slug: "coder", Phase: "running"}}
			principal := linkTestUser(t, store, 42, "alice@example.com")

			h.HandleCommand(&TGMessage{Text: text, Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

			calls := hub.agentCalls()
			require.Len(t, calls, 1)
			assert.Equal(t, fakeListAgentsCall{ProjectID: "proj-1", OnBehalfOf: principal}, calls[0])
		})
	}
}

func TestCommandHandler_GroupCommands_UnlinkedSenderGetsRegisterHint(t *testing.T) {
	for _, text := range []string{"/agents", "/default", "/terminal coder"} {
		t.Run(text, func(t *testing.T) {
			h, tgSrv, hub, store := newTestCommandHandler(t)
			saveTestGroupLink(t, store, -100, "proj-1", "my-project", "coder")
			hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}

			h.HandleCommand(&TGMessage{Text: text, Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

			assert.Empty(t, hub.agentCalls(), "no hub read without a linked user")
			sent := tgSrv.getSentMessages()
			require.Len(t, sent, 1)
			assert.Contains(t, sent[0].Text, "/register")
		})
	}
}

func TestCommandHandler_Notifications_SendsLinkedUser(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	saveTestGroupLink(t, store, -100, "proj-1", "my-project", "")
	hub.projects = []ProjectOption{{ID: "proj-1", Slug: "my-project"}}
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	principal := linkTestUser(t, store, 42, "alice@example.com")

	h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})

	calls := hub.agentCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, principal, calls[0].OnBehalfOf)
	assert.Equal(t, []string{principal}, hub.listUserProjectsCalls)
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "toggle notifications")
}

func TestCommandHandler_Setup_ListsProjectsAsLinkedUser(t *testing.T) {
	h, _, hub, store := newTestCommandHandler(t)
	hub.projects = []ProjectOption{{ID: "p1", Slug: "alpha"}}
	principal := linkTestUser(t, store, 42, "alice@example.com")

	h.HandleCommand(&TGMessage{Text: "/setup", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	assert.Equal(t, []string{principal}, hub.listUserProjectsCalls)
}

// --- /status scoping ---

func saveStatusTestLinks(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	for _, l := range []*GroupLink{
		{ChatID: -101, ChatTitle: "Member Group", ProjectID: "p1", ProjectSlug: "alpha", LinkedBy: "999"},
		{ChatID: -102, ChatTitle: "Linked By Me", ProjectID: "p2", ProjectSlug: "beta", LinkedBy: "456"},
		{ChatID: -103, ChatTitle: "Other Team", ProjectID: "p3", ProjectSlug: "secret-project", LinkedBy: "999"},
		{ChatID: -104, ChatTitle: "Left Group", ProjectID: "p4", ProjectSlug: "gone", LinkedBy: "999"},
		{ChatID: -105, ChatTitle: "Kicked Group", ProjectID: "p5", ProjectSlug: "banned", LinkedBy: "999"},
		{ChatID: -106, ChatTitle: "Admin Group", ProjectID: "p6", ProjectSlug: "admin-proj", LinkedBy: "999"},
	} {
		l.LinkedAt = time.Now().UTC()
		l.Active = true
		require.NoError(t, store.SaveGroupLink(ctx, l))
	}
}

func TestCommandHandler_Status_ListsOnlyGroupsTheUserLinkedOrBelongsTo(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	linkTestUser(t, store, 456, "alice@example.com")
	saveStatusTestLinks(t, store)
	tgSrv.setChatMember(-101, 456, "member")
	tgSrv.setChatMember(-104, 456, "left")
	tgSrv.setChatMember(-105, 456, "kicked")
	tgSrv.setChatMember(-106, 456, "administrator")
	// -103: membership unknown (getChatMember fails) → left out.

	h.HandleCommand(&TGMessage{Text: "/status", From: &TGUser{ID: 456}, Chat: TGChat{ID: 456, Type: "private"}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	text := sent[0].Text
	assert.Contains(t, text, "Member Group")
	assert.Contains(t, text, "Linked By Me")
	assert.Contains(t, text, "Admin Group")
	for _, hidden := range []string{"Other Team", "secret-project", "-103", "Left Group", "Kicked Group"} {
		assert.NotContains(t, text, hidden)
	}
}

func TestCommandHandler_Status_UnregisteredUserSeesOnlyTheirGroups(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	saveStatusTestLinks(t, store)
	tgSrv.setChatMember(-101, 456, "member")

	h.HandleCommand(&TGMessage{Text: "/status", From: &TGUser{ID: 456}, Chat: TGChat{ID: 456, Type: "private"}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Not registered")
	assert.Contains(t, sent[0].Text, "Member Group")
	assert.NotContains(t, sent[0].Text, "Other Team")
}

func TestCommandHandler_Status_NoVisibleGroups(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	linkTestUser(t, store, 777, "bob@example.com")
	saveStatusTestLinks(t, store)

	h.HandleCommand(&TGMessage{Text: "/status", From: &TGUser{ID: 777}, Chat: TGChat{ID: 777, Type: "private"}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Registered as bob@example.com")
	assert.Contains(t, sent[0].Text, "No groups you linked or belong to")
	assert.NotContains(t, sent[0].Text, "Group")
}

func TestTGChatMember_IsCurrentMember(t *testing.T) {
	for status, want := range map[string]bool{
		"creator": true, "administrator": true, "member": true, "left": false, "kicked": false,
	} {
		assert.Equal(t, want, (&TGChatMember{Status: status}).IsCurrentMember(), status)
	}
	assert.True(t, (&TGChatMember{Status: "restricted", IsMember: true}).IsCurrentMember())
	assert.False(t, (&TGChatMember{Status: "restricted"}).IsCurrentMember())
}

func TestCommandHandler_Status_FailedCheckIsNotedNotHidden(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	linkTestUser(t, store, 456, "alice@example.com")
	saveStatusTestLinks(t, store)
	tgSrv.setChatMember(-101, 456, "member")
	tgSrv.mu.Lock()
	tgSrv.failChatMember = map[int64]bool{-103: true}
	tgSrv.mu.Unlock()

	h.HandleCommand(&TGMessage{Text: "/status", From: &TGUser{ID: 456}, Chat: TGChat{ID: 456, Type: "private"}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Member Group")
	assert.NotContains(t, sent[0].Text, "Other Team")
	assert.Contains(t, sent[0].Text, statusUncheckedNote)
}

func TestCommandHandler_Status_AllChecksFailedDoesNotClaimNone(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	linkTestUser(t, store, 777, "bob@example.com")
	saveStatusTestLinks(t, store)
	tgSrv.mu.Lock()
	tgSrv.failChatMember = map[int64]bool{-101: true, -102: true, -103: true, -104: true, -105: true, -106: true}
	tgSrv.mu.Unlock()

	h.HandleCommand(&TGMessage{Text: "/status", From: &TGUser{ID: 777}, Chat: TGChat{ID: 777, Type: "private"}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.NotContains(t, sent[0].Text, "No groups you linked or belong to")
	assert.Contains(t, sent[0].Text, "could be confirmed")
	assert.Contains(t, sent[0].Text, statusUncheckedNote)
}

func TestCommandHandler_GroupsVisibleTo_CancelledContextIsNoted(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	saveStatusTestLinks(t, store)
	tgSrv.setChatMember(-101, 456, "member")
	links, err := store.GetAllGroupLinks(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	visible, unchecked := h.groupsVisibleTo(ctx, 456, links)

	assert.True(t, unchecked)
	require.Len(t, visible, 1, "only the group the user linked is known without a check")
	assert.Equal(t, int64(-102), visible[0].ChatID)
	calls, _ := tgSrv.chatMemberStats()
	assert.Zero(t, calls)
}

func TestCommandHandler_GroupsVisibleTo_CachesMembershipAndBoundsConcurrency(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)
	ctx := context.Background()
	const groups = 20
	for i := 1; i <= groups; i++ {
		chatID := int64(-1000 - i)
		require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
			ChatID: chatID, ChatTitle: fmt.Sprintf("G%02d", i), ProjectID: fmt.Sprintf("p%d", i),
			LinkedBy: "999", LinkedAt: time.Now().UTC(), Active: true,
		}))
		if i%2 == 0 {
			tgSrv.setChatMember(chatID, 456, "member")
		} else {
			tgSrv.setChatMember(chatID, 456, "left")
		}
	}
	tgSrv.mu.Lock()
	tgSrv.chatMemberDelay = 20 * time.Millisecond
	tgSrv.mu.Unlock()
	links, err := store.GetAllGroupLinks(ctx)
	require.NoError(t, err)

	visible, unchecked := h.groupsVisibleTo(ctx, 456, links)
	assert.False(t, unchecked)
	assert.Len(t, visible, groups/2)
	calls, maxInFlight := tgSrv.chatMemberStats()
	assert.Equal(t, groups, calls)
	assert.LessOrEqual(t, maxInFlight, memberCheckWorkers)
	assert.Greater(t, maxInFlight, 1, "checks run concurrently")

	// Order of the input links is kept.
	var want []int64
	for _, l := range links {
		if (-l.ChatID-1000)%2 == 0 {
			want = append(want, l.ChatID)
		}
	}
	var got []int64
	for _, l := range visible {
		got = append(got, l.ChatID)
	}
	assert.Equal(t, want, got)

	// A second call within the cache window makes no new checks.
	visible2, _ := h.groupsVisibleTo(ctx, 456, links)
	assert.Len(t, visible2, groups/2)
	calls2, _ := tgSrv.chatMemberStats()
	assert.Equal(t, groups, calls2)
}
