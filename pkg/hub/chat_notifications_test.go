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
	// This test verifies that when an agent is @mentioned, the
	// fireHumanMentionNotifications helper does NOT create a user
	// notification for the agent. Agent mentions create type:mention
	// messages for the agent dispatch pipeline — not human notifications.
	srv, s := testServer(t)
	ctx := context.Background()

	// Set up WebChatStore + ChatNotifier on the server.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Create a project.
	proj := &store.Project{
		ID: api.NewUUID(), Name: "mention-test", Slug: "mention-test",
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	// Create an agent in the project.
	agent := &store.Agent{
		ID: api.NewUUID(), Slug: "scout", Name: "Scout",
		Template: "claude", ProjectID: proj.ID,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Create a human user and add to project members group.
	humanUser := &store.User{
		ID: api.NewUUID(), Email: "alice@example.com",
		DisplayName: "Alice", Role: "member", Status: "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, humanUser))

	groupID := api.NewUUID()
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID:   groupID,
		Name: "mention-test members",
		Slug: "project:mention-test:members",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   humanUser.ID,
		Role:       "member",
	}))

	// Create a topic for the conversation.
	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "general",
		CreatedBy: humanUser.ID, CreatedAt: time.Now(),
	}))

	senderID := api.NewUUID() // some other user

	// Call fireHumanMentionNotifications with the agent slug as a mention.
	// The helper should resolve "scout" against project members, NOT find
	// it as a human, and skip it.
	srv.fireHumanMentionNotifications(ctx,
		[]string{"scout"},       // mentionNames — agent slug, not a human
		proj.ID,                 // projectID
		topicID,                 // conversationKey
		senderID,                // senderUserID
		"Bob",                   // senderName
		"Hey @scout check this", // messageContent
	)

	// Verify: no notification was created for the agent UUID.
	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, agent.ID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs, "agent mentions must not create user notifications")

	// Also verify no notification for the human — the human was not mentioned.
	humanNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, humanUser.ID, false)
	require.NoError(t, err)
	assert.Empty(t, humanNotifs, "human should not be notified when only the agent was mentioned")

	// Now mention the human by display name — she SHOULD get a notification.
	srv.fireHumanMentionNotifications(ctx,
		[]string{"Alice"}, // mentionNames — human display name
		proj.ID,
		topicID,
		senderID,
		"Bob",
		"Hey @Alice check this",
	)
	humanNotifs, err = s.GetNotifications(ctx, store.SubscriberTypeUser, humanUser.ID, false)
	require.NoError(t, err)
	require.Len(t, humanNotifs, 1, "human should be notified when mentioned by display name")
	assert.Contains(t, humanNotifs[0].Message, "@Bob mentioned you")

	// Mention both agent and human — only human gets a notification.
	srv.fireHumanMentionNotifications(ctx,
		[]string{"scout", "Alice"}, // both agent and human
		proj.ID,
		topicID,
		senderID,
		"Charlie",
		"Hey @scout @Alice",
	)
	// Alice should now have 2 total notifications.
	humanNotifs, err = s.GetNotifications(ctx, store.SubscriberTypeUser, humanUser.ID, false)
	require.NoError(t, err)
	assert.Len(t, humanNotifs, 2, "human should get another notification")

	// Agent should still have zero.
	agentNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, agent.ID, false)
	require.NoError(t, err)
	assert.Empty(t, agentNotifs, "agent must never get user notifications from mentions")
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
