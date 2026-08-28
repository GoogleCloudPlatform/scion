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
	"encoding/json"
	"log/slog"
	"strings"
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

	// Subscribe to every subject a notification could plausibly reach, so a
	// test that expects silence sees a chat payload escaping onto the wrong
	// subject rather than passing because it was only watching the right one.
	notifCh, unsub := pub.Subscribe("user.>", "notification.>", "project.>")

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
	senderID := api.NewUUID()
	conversationKey := api.NewUUID() // topic UUID
	projectID := api.NewUUID()

	env.notifier.NotifyMention(ctx, userID, ChatMessageContext{
		SenderID:         senderID,
		SenderName:       "Alice",
		ConversationKey:  conversationKey,
		ConversationName: "design-review",
		Preview:          "Hey @Bob check this out",
		ProjectID:        projectID,
	})

	// Verify notification was published via SSE.
	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "expected a notification event")
	assert.Equal(t, "user."+userID+".notification", evt.Subject,
		"mention notifications must be scoped to the mentioned user")

	// The browser notification is built entirely from the event payload: the
	// client needs the recipient (to ignore events meant for another tab's
	// user), the conversation (for the tag and the click target) and the
	// sender (for the title), none of which are recoverable from the
	// rendered message string.
	var payload ChatNotificationEvent
	require.NoError(t, json.Unmarshal(evt.Data, &payload))
	assert.Equal(t, userID, payload.SubscriberID)
	assert.Equal(t, senderID, payload.SenderID)
	assert.Equal(t, "Alice", payload.SenderName)
	assert.Equal(t, conversationKey, payload.ConversationKey)
	assert.Equal(t, "design-review", payload.ConversationName)
	assert.Equal(t, projectID, payload.ProjectID)
	assert.Equal(t, ChatNotificationMention, payload.Status)

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

	env.notifier.NotifyMention(ctx, userID, ChatMessageContext{
		SenderName:       "Alice",
		ConversationKey:  conversationKey,
		ConversationName: "general",
		Preview:          "Hey @Bob",
		ProjectID:        projectID,
	})

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
	senderID := api.NewUUID()
	conversationKey := "dm:user:" + api.NewUUID() + ":user:" + recipientID
	projectID := ""

	env.notifier.NotifyDMReceived(ctx, recipientID, ChatMessageContext{
		SenderID:        senderID,
		SenderName:      "Charlie",
		ConversationKey: conversationKey,
		Preview:         "Hello there!",
		ProjectID:       projectID,
	})

	// Verify notification was published.
	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "expected a notification event")
	assert.Equal(t, "user."+recipientID+".notification", evt.Subject,
		"DM notifications must be scoped to the recipient")

	var payload ChatNotificationEvent
	require.NoError(t, json.Unmarshal(evt.Data, &payload))
	assert.Equal(t, recipientID, payload.SubscriberID)
	assert.Equal(t, senderID, payload.SenderID)
	assert.Equal(t, "Charlie", payload.SenderName)
	assert.Equal(t, conversationKey, payload.ConversationKey)
	assert.Equal(t, ChatNotificationDMReceived, payload.Status)
	// A DM has no thread name; the notification title is the sender. Carrying
	// a stale conversationName here would put the wrong heading on the popup.
	assert.Empty(t, payload.ConversationName,
		"DM events must not carry a conversation name")

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

// B5/F2: After the auth-derivation override, SenderName arrives as a raw UUID
// (the agent ID) because the Sender field is "agent:"+UUID. NotifyDMReceived
// must resolve SenderID → agent display name for the notification text. Name
// is preferred over Slug, matching handlers_agent_messaging.go:353-357.
// On lookup failure it falls back to the original label (fail-open, not
// fail-closed — dropping a notification is worse than showing a UUID).
//
// F3: the resolution only fires when SenderName == SenderID (the UUID form).
// When a caller already passes a proper label (agent.Name, user display name),
// it must NOT be clobbered.
func TestChatNotifier_DMReceived_ResolvesAgentSlugFromSenderID(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()

	// Create project and agent. Name and Slug differ so we can assert
	// Name is preferred over Slug.
	project := &store.Project{
		ID: api.NewUUID(), Name: "f2-notif", Slug: "f2-notif",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateProject(ctx, project))

	agent := &store.Agent{
		ID: api.NewUUID(), Name: "Helpful Bot", Slug: "helpful-bot",
		ProjectID: project.ID, Phase: "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, agent))

	recipientID := api.NewUUID()
	conversationKey := "dm:agent:" + agent.ID + ":user:" + recipientID

	// SenderName is the UUID form (what TrimPrefix produces after B5 override).
	env.notifier.NotifyDMReceived(ctx, recipientID, ChatMessageContext{
		SenderID:        agent.ID,
		SenderName:      agent.ID, // raw UUID, not slug
		ConversationKey: conversationKey,
		Preview:         "Hello from the bot!",
		ProjectID:       project.ID,
	})

	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "expected a notification event")

	var payload ChatNotificationEvent
	require.NoError(t, json.Unmarshal(evt.Data, &payload))

	// The notification must display agent.Name (preferred), not UUID or Slug.
	assert.Equal(t, "Helpful Bot", payload.SenderName,
		"notification SenderName must be agent.Name, not the raw UUID or slug")

	// Also check the persisted notification text.
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, recipientID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Contains(t, notifs[0].Message, "Helpful Bot",
		"persisted notification text must use agent.Name")
	assert.NotContains(t, notifs[0].Message, agent.ID,
		"persisted notification text must not contain the raw UUID")
}

// B5/F3/F6: When a caller already passes a proper label (e.g.
// handlers_agent_messaging.go:353-357 passing agent.Name), the F2 slug
// resolution must NOT overwrite it. The guard SenderName == SenderID
// ensures the lookup only fires when SenderName is the raw UUID.
// This test uses a SenderName distinct from both agent.Name and
// agent.Slug so it fails if the guard is removed (resolution would
// overwrite with Name) or if the preference order changes.
func TestChatNotifier_DMReceived_DoesNotClobberCallerLabel(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()

	project := &store.Project{
		ID: api.NewUUID(), Name: "f6-noclobber", Slug: "f6-noclobber",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateProject(ctx, project))

	agent := &store.Agent{
		ID: api.NewUUID(), Name: "Agent Real Name", Slug: "agent-slug",
		ProjectID: project.ID, Phase: "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, agent))

	recipientID := api.NewUUID()

	// Caller supplies a label distinct from both Name and Slug.
	// This is what handlers_agent_messaging.go:353-357 does (passes
	// agent.Name), but we use a third string so the test is sensitive
	// to any overwrite, not just Slug-over-Name.
	env.notifier.NotifyDMReceived(ctx, recipientID, ChatMessageContext{
		SenderID:        agent.ID,
		SenderName:      "Caller Chosen Label",
		ConversationKey: "dm:agent:" + agent.ID + ":user:" + recipientID,
		Preview:         "hello",
		ProjectID:       project.ID,
	})

	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "expected a notification event")

	var payload ChatNotificationEvent
	require.NoError(t, json.Unmarshal(evt.Data, &payload))

	assert.Equal(t, "Caller Chosen Label", payload.SenderName,
		"caller-supplied label must not be overwritten by slug resolution")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, recipientID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Contains(t, notifs[0].Message, "Caller Chosen Label",
		"persisted notification must use the caller-supplied label")
}

// B5/F2 fallback: when the agent lookup fails (e.g. deleted agent), the
// notification must still be created with the original label, not dropped.
func TestChatNotifier_DMReceived_FallsBackOnLookupFailure(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()
	recipientID := api.NewUUID()
	nonexistentAgentID := api.NewUUID()
	conversationKey := "dm:agent:" + nonexistentAgentID + ":user:" + recipientID

	// SenderName is the UUID (agent not in store — lookup will fail).
	env.notifier.NotifyDMReceived(ctx, recipientID, ChatMessageContext{
		SenderID:        nonexistentAgentID,
		SenderName:      nonexistentAgentID,
		ConversationKey: conversationKey,
		Preview:         "Ghost message",
		ProjectID:       "",
	})

	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "notification must still be created on lookup failure (fail-open)")

	var payload ChatNotificationEvent
	require.NoError(t, json.Unmarshal(evt.Data, &payload))

	// Falls back to the UUID label — not ideal but the notification is not dropped.
	assert.Equal(t, nonexistentAgentID, payload.SenderName,
		"on lookup failure, SenderName must fall back to the original label")
}

// TestChatNotifier_EventPreviewMatchesMessageTruncation pins the two
// renderings of the same text to one truncation rule. The tray row reads the
// formatted Message; the browser popup reads Preview. If they truncate
// differently the same message stops at two different words depending on
// where you read it.
func TestChatNotifier_EventPreviewMatchesMessageTruncation(t *testing.T) {
	env := setupChatNotifTest(t)
	defer env.unsub()

	ctx := context.Background()
	recipientID := api.NewUUID()

	// Comfortably longer than the cap, and multi-byte, so a byte-based
	// truncation would produce a replacement character rather than "é".
	long := strings.Repeat("é", 150)

	env.notifier.NotifyDMReceived(ctx, recipientID, ChatMessageContext{
		SenderID:        api.NewUUID(),
		SenderName:      "Charlie",
		ConversationKey: "dm:user:" + api.NewUUID() + ":user:" + recipientID,
		Preview:         long,
	})

	evt := drainNotification(env.notifCh, 2*time.Second)
	require.NotNil(t, evt, "expected a notification event")

	var payload ChatNotificationEvent
	require.NoError(t, json.Unmarshal(evt.Data, &payload))

	// Recomputed, not hard-coded: whatever the cap is, both must honour it.
	expected := truncateChatPreview(long)
	assert.Equal(t, expected, payload.Preview)
	assert.Contains(t, payload.Message, expected,
		"the formatted message and the event preview must truncate identically")
	assert.Less(t, len([]rune(payload.Preview)), len([]rune(long)),
		"preview was not truncated at all — the test proves nothing")
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

	env.notifier.NotifyDMReceived(ctx, recipientID, ChatMessageContext{
		SenderName:      "Charlie",
		ConversationKey: conversationKey,
		Preview:         "Hello!",
	})

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

	notifier.NotifyDMReceived(ctx, recipientID, ChatMessageContext{
		SenderName:      "Charlie",
		ConversationKey: conversationKey,
		Preview:         "Hello!",
	})

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
	cn.NotifyMention(context.Background(), "user", ChatMessageContext{SenderName: "sender", ConversationKey: "key", ConversationName: "thread", Preview: "msg", ProjectID: "proj"})
	cn.NotifyDMReceived(context.Background(), "user", ChatMessageContext{SenderName: "sender", ConversationKey: "key", Preview: "msg", ProjectID: "proj"})
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

// ---------------------------------------------------------------------------
// Tests: presence wiring on the Server (#1032)
// ---------------------------------------------------------------------------

// The notifier used to be constructed with a nil PresenceChecker, which falls
// back to NoOpPresenceChecker and reports every user as absent — so the
// "don't interrupt someone who is already looking at chat" suppression never
// fired. The mocked-checker tests above could not catch that: only the real
// server wiring can. Note the construction order this depends on —
// SetWebChatStore runs before InitPresenceManager on the startup path, so the
// checker has to resolve the presence manager lazily.
func TestServer_ChatNotifier_UsesServerPresence(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())

	// Startup order: the webchat store (and with it the notifier) is wired
	// first, the presence manager second.
	srv.SetWebChatStore(wcs)
	srv.InitPresenceManager()
	t.Cleanup(srv.StopPresenceManager)

	cn := srv.getChatNotifier()
	require.NotNil(t, cn, "expected a chat notifier after SetWebChatStore")

	viewer := api.NewUUID()
	absent := api.NewUUID()

	// The viewer is actively using chat; the other user is not.
	srv.presenceManager.Heartbeat(ctx, viewer, "Active Viewer", nil)
	require.True(t, srv.presenceManager.IsUserActive(viewer))
	require.False(t, srv.presenceManager.IsUserActive(absent))

	cn.NotifyDMReceived(ctx, viewer, ChatMessageContext{SenderName: "Sender", ConversationKey: "dm:user:" + api.NewUUID() + ":user:" + viewer, Preview: "hello"})
	cn.NotifyDMReceived(ctx, absent, ChatMessageContext{SenderName: "Sender", ConversationKey: "dm:user:" + api.NewUUID() + ":user:" + absent, Preview: "hello"})

	viewerNotifs, err := st.GetNotifications(ctx, store.SubscriberTypeUser, viewer, false)
	require.NoError(t, err)
	assert.Empty(t, viewerNotifs, "an actively present user must not be notified")

	absentNotifs, err := st.GetNotifications(ctx, store.SubscriberTypeUser, absent, false)
	require.NoError(t, err)
	assert.Len(t, absentNotifs, 1, "an absent user must still be notified")
}

// A presence manager that has never seen a heartbeat for the user, and a nil
// manager (no InitPresenceManager, e.g. in tests or a trimmed deployment),
// must both report absent so notifications keep flowing.
func TestPresenceManager_IsUserActive(t *testing.T) {
	var nilPM *PresenceManager
	assert.False(t, nilPM.IsUserActive("user-1"))

	pm := NewPresenceManager(NewChannelEventPublisher(), nil)
	t.Cleanup(pm.Stop)

	assert.False(t, pm.IsUserActive("user-1"))
	pm.Heartbeat(context.Background(), "user-1", "User One", nil)
	assert.True(t, pm.IsUserActive("user-1"))
	assert.False(t, pm.IsUserActive("user-2"))
}
