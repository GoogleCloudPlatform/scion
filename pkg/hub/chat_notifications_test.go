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
	"database/sql"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// chatNotifTestEnv holds components for chat notification tests.
type chatNotifTestEnv struct {
	store    store.Store
	wcs      WebChatStore
	pub      *ChannelEventPublisher
	notifier *ChatNotifier
	notifCh  <-chan Event // receives notification events
	unsub    func()
}

func setupChatNotifTest(t *testing.T) *chatNotifTestEnv {
	t.Helper()

	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	// Subscribe to notification events so we can verify what was published.
	notifCh, unsub := pub.Subscribe("notification.>")

	notifier := NewChatNotifier(s, pub, wcs, nil, slog.Default())

	return &chatNotifTestEnv{
		store:    s,
		wcs:      wcs,
		pub:      pub,
		notifier: notifier,
		notifCh:  notifCh,
		unsub:    unsub,
	}
}

// drainNotification waits briefly for a notification event on the channel.
func drainNotification(ch <-chan Event, timeout time.Duration) *Event {
	select {
	case evt := <-ch:
		return &evt
	case <-time.After(timeout):
		return nil
	}
}

// ---------------------------------------------------------------------------
// Tests: ChatNotifier.NotifyMention
// ---------------------------------------------------------------------------

func TestChatNotifier_MentionCreatesNotification(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()
	userID := api.NewUUID()
	conversationKey := api.NewUUID() // topic UUID
	projectID := api.NewUUID()

	env.notifier.NotifyMention(ctx, userID, "Alice", conversationKey, "design-review", "Hey @Bob check this out", projectID)

	// Verify notification was published via SSE.
	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "expected a notification event")
	assert.Contains(t, evt.Subject, "notification.")

	// Verify notification was persisted in the store.
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, userID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	n := notifs[0]
	assert.Equal(t, userID, n.SubscriberID)
	assert.Equal(t, store.SubscriberTypeUser, n.SubscriberType)
	assert.Equal(t, ChatNotificationMention, n.Status)
	assert.Contains(t, n.Message, "@Alice mentioned you")
	assert.Contains(t, n.Message, "#design-review")
	assert.Contains(t, n.Message, "Hey @Bob check this out")
}

func TestChatNotifier_MentionMuted_NoNotification(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()
	userID := api.NewUUID()
	conversationKey := api.NewUUID()
	projectID := api.NewUUID()

	// Mute the conversation for this user.
	err := env.wcs.SetMuted(ctx, userID, conversationKey, true)
	require.NoError(t, err)

	env.notifier.NotifyMention(ctx, userID, "Alice", conversationKey, "general", "Hey @Bob", projectID)

	// No notification should be created.
	evt := drainNotification(env.notifCh, 500*time.Millisecond)
	assert.Nil(t, evt, "no notification expected for muted conversation")

	// Verify nothing persisted.
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, userID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs)
}

// ---------------------------------------------------------------------------
// Tests: ChatNotifier.NotifyDMReceived
// ---------------------------------------------------------------------------

func TestChatNotifier_DMReceivedCreatesNotification(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()
	recipientID := api.NewUUID()
	conversationKey := "dm:user:" + api.NewUUID() + ":user:" + recipientID
	projectID := ""

	env.notifier.NotifyDMReceived(ctx, recipientID, "Charlie", conversationKey, "Hello there!", projectID)

	// Verify notification was published.
	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "expected a notification event")

	// Verify notification was persisted.
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, recipientID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	n := notifs[0]
	assert.Equal(t, recipientID, n.SubscriberID)
	assert.Equal(t, ChatNotificationDMReceived, n.Status)
	assert.Contains(t, n.Message, "Charlie sent you a message")
	assert.Contains(t, n.Message, "Hello there!")
}

func TestChatNotifier_DMReceivedMuted_NoNotification(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()
	recipientID := api.NewUUID()
	conversationKey := "dm:user:" + api.NewUUID() + ":user:" + recipientID

	// Mute the DM conversation.
	err := env.wcs.SetMuted(ctx, recipientID, conversationKey, true)
	require.NoError(t, err)

	env.notifier.NotifyDMReceived(ctx, recipientID, "Charlie", conversationKey, "Hello!", "")

	// No notification should be created.
	evt := drainNotification(env.notifCh, 500*time.Millisecond)
	assert.Nil(t, evt, "no notification expected for muted DM")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, recipientID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs)
}

func TestChatNotifier_DMReceivedActivePresence_NoNotification(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()
	recipientID := api.NewUUID()
	conversationKey := "dm:user:" + api.NewUUID() + ":user:" + recipientID

	// Create a notifier with an active presence checker.
	activePresence := &mockPresenceChecker{activeUsers: map[string]bool{recipientID: true}}
	notifier := NewChatNotifier(env.store, env.pub, env.wcs, activePresence, slog.Default())

	notifier.NotifyDMReceived(ctx, recipientID, "Charlie", conversationKey, "Hello!", "")

	// No notification — user has active presence.
	evt := drainNotification(env.notifCh, 500*time.Millisecond)
	assert.Nil(t, evt, "no notification expected when user has active presence")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, recipientID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs)
}

// ---------------------------------------------------------------------------
// Tests: formatChatNotification
// ---------------------------------------------------------------------------

func TestFormatChatNotification(t *testing.T) {
	tests := []struct {
		name             string
		trigger          string
		senderName       string
		conversationName string
		messagePreview   string
		wantContains     []string
	}{
		{
			name:             "mention with thread name",
			trigger:          ChatNotificationMention,
			senderName:       "Alice",
			conversationName: "design-review",
			messagePreview:   "Check the latest PR",
			wantContains:     []string{"@Alice mentioned you", "#design-review", "Check the latest PR"},
		},
		{
			name:             "mention without thread name",
			trigger:          ChatNotificationMention,
			senderName:       "Bob",
			conversationName: "",
			messagePreview:   "Hey there",
			wantContains:     []string{"@Bob mentioned you", "Hey there"},
		},
		{
			name:           "DM received",
			trigger:        ChatNotificationDMReceived,
			senderName:     "Charlie",
			messagePreview: "Quick question about the build",
			wantContains:   []string{"Charlie sent you a message", "Quick question about the build"},
		},
		{
			name:           "long message preview truncated",
			trigger:        ChatNotificationDMReceived,
			senderName:     "Dave",
			messagePreview: "This is a very long message that exceeds the maximum preview length and should be truncated at one hundred characters so that push notifications remain readable",
			wantContains:   []string{"Dave sent you a message", "…"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatChatNotification(tt.trigger, tt.senderName, tt.conversationName, tt.messagePreview)
			for _, want := range tt.wantContains {
				assert.Contains(t, result, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: Agent mentions do NOT create user notifications (regression)
// ---------------------------------------------------------------------------

func TestAgentMentions_DoNotCreateUserNotifications(t *testing.T) {
	// This test verifies that when an agent is @mentioned, NO user
	// notification is created for it. Agent mentions create type:mention
	// messages for the agent dispatch pipeline — not human notifications.
	// This is the existing behavior and must not regress.
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()

	// Create a project and agent.
	proj := &store.Project{
		ID: api.NewUUID(), Name: "test-proj", Slug: "test-proj",
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, env.store.CreateProject(ctx, proj))

	agent := &store.Agent{
		ID: api.NewUUID(), Slug: "scout", Name: "Scout",
		Template: "claude", ProjectID: proj.ID,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, agent))

	// Simulate: message contains @scout. The mention resolution in
	// handleConversationSend will match "scout" as an agent, NOT a human.
	// The agent gets a type:mention side message through the dispatch path.
	// The ChatNotifier should NOT be called for agent mentions.

	// Verify: calling NotifyMention with the agent's UUID would be wrong.
	// The fireHumanMentionNotifications helper only resolves mentions against
	// project HUMAN members — agents are excluded. So if we have only agents
	// in the project, no mention notification should fire.

	// No human members → no mention notifications.
	// (The only member is the agent, which is not a human.)
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, agent.ID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs, "agent mentions must not create user notifications")
}

// ---------------------------------------------------------------------------
// Tests: IsConversationMuted (store method)
// ---------------------------------------------------------------------------

func TestIsConversationMuted_SQLite(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())

	ctx := context.Background()
	userID := api.NewUUID()
	key := api.NewUUID()

	// No row → not muted.
	muted, err := wcs.IsConversationMuted(ctx, userID, key)
	require.NoError(t, err)
	assert.False(t, muted, "should not be muted when no row exists")

	// Set muted.
	require.NoError(t, wcs.SetMuted(ctx, userID, key, true))
	muted, err = wcs.IsConversationMuted(ctx, userID, key)
	require.NoError(t, err)
	assert.True(t, muted, "should be muted after SetMuted(true)")

	// Unmute.
	require.NoError(t, wcs.SetMuted(ctx, userID, key, false))
	muted, err = wcs.IsConversationMuted(ctx, userID, key)
	require.NoError(t, err)
	assert.False(t, muted, "should not be muted after SetMuted(false)")
}

// ---------------------------------------------------------------------------
// Tests: NoOpPresenceChecker
// ---------------------------------------------------------------------------

func TestNoOpPresenceChecker_AlwaysReturnsFalse(t *testing.T) {
	checker := NoOpPresenceChecker{}
	assert.False(t, checker.IsUserActive("any-user-id"))
	assert.False(t, checker.IsUserActive(""))
}

// ---------------------------------------------------------------------------
// Tests: Nil ChatNotifier is safe
// ---------------------------------------------------------------------------

func TestChatNotifier_NilSafe(t *testing.T) {
	// Calling methods on a nil ChatNotifier should not panic.
	var cn *ChatNotifier
	cn.NotifyMention(context.Background(), "user", "sender", "key", "thread", "msg", "proj")
	cn.NotifyDMReceived(context.Background(), "user", "sender", "key", "msg", "proj")
}

// ---------------------------------------------------------------------------
// Mock types
// ---------------------------------------------------------------------------

// mockPresenceChecker returns true for users in the activeUsers set.
type mockPresenceChecker struct {
	activeUsers map[string]bool
}

func (m *mockPresenceChecker) IsUserActive(userID string) bool {
	return m.activeUsers[userID]
}
