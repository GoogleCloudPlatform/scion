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
	"log/slog"
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
	h, tgSrv, hub, _ := newTestCommandHandler(t)
	hub.projects = []ProjectOption{}

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "No projects found")
}

func TestCommandHandler_Setup_InGroup_WithProjects(t *testing.T) {
	h, tgSrv, hub, _ := newTestCommandHandler(t)
	hub.projects = []ProjectOption{
		{ID: "proj-1", Slug: "my-project"},
		{ID: "proj-2", Slug: "other-project"},
	}

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Select a project")
	require.NotNil(t, sent[0].ReplyMarkup)
}

func TestCommandHandler_Setup_InGroup_CachedProjects(t *testing.T) {
	h, tgSrv, hub, _ := newTestCommandHandler(t)
	hub.projects = []ProjectOption{} // hub returns nothing

	h.SetProjects([]ProjectOption{
		{ID: "proj-1", Slug: "cached-project"},
		{ID: "proj-2", Slug: "other-cached"},
	})

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Select a project")
	require.NotNil(t, sent[0].ReplyMarkup)
}

func TestCommandHandler_Setup_InGroup_CachedProjectsFallbackToHub(t *testing.T) {
	h, tgSrv, hub, _ := newTestCommandHandler(t)
	hub.projects = []ProjectOption{
		{ID: "proj-1", Slug: "hub-project"},
	}
	// no cached projects set — should fall back to hub

	h.HandleCommand(&TGMessage{
		Text: "/setup",
		Chat: TGChat{ID: -100, Type: "group"},
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Select a project")
	require.NotNil(t, sent[0].ReplyMarkup)
}

func TestCommandHandler_Setup_AlreadyLinked(t *testing.T) {
	h, tgSrv, _, store := newTestCommandHandler(t)

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

	h.HandleCommand(&TGMessage{
		Text: "/agents",
		Chat: TGChat{ID: -100, Type: "group"},
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

	h.HandleCommand(&TGMessage{
		Text: "/agents",
		Chat: TGChat{ID: -100, Type: "group"},
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
	assert.Contains(t, sent[0].Text, "No groups are currently linked.")
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
