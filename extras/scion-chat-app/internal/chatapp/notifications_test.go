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

package chatapp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// TestHandleBrokerMessage_UserMessageRouting verifies that user-targeted
// messages with the full scion broker topic prefix are correctly routed
// to handleUserMessage.
func TestHandleBrokerMessage_UserMessageRouting(t *testing.T) {
	log := slog.Default()
	relay := NewNotificationRelay(nil, nil, log)

	// Message with empty RecipientID triggers early return in handleUserMessage
	// without touching the store, so we can test topic routing safely.
	msg := &messages.StructuredMessage{
		Sender: "agent:test-agent",
		Msg:    "hello from agent",
	}

	// Full scion-prefixed topic should route to handleUserMessage.
	err := relay.HandleBrokerMessage(context.Background(),
		"scion.grove.grove-123.user.user-456.messages", msg)
	if err != nil {
		t.Errorf("expected nil error for user message topic, got: %v", err)
	}
}

// TestHandleBrokerMessage_IgnoredTopics verifies that unrecognized or
// malformed topics are silently ignored.
func TestHandleBrokerMessage_IgnoredTopics(t *testing.T) {
	log := slog.Default()
	relay := NewNotificationRelay(nil, nil, log)
	msg := &messages.StructuredMessage{Msg: "test"}

	topics := []string{
		"x",
		"scion.global.broadcast",
		"user.user-456.message", // old unprefixed format
	}

	for _, topic := range topics {
		t.Run(topic, func(t *testing.T) {
			err := relay.HandleBrokerMessage(context.Background(), topic, msg)
			if err != nil {
				t.Errorf("expected nil error for ignored topic %q, got: %v", topic, err)
			}
		})
	}
}

// fakeMessenger records SendMessage and SendCard calls for test assertions.
type fakeMessenger struct {
	messages []SendMessageRequest
}

func (f *fakeMessenger) SendMessage(_ context.Context, req SendMessageRequest) (string, error) {
	f.messages = append(f.messages, req)
	return "msg-1", nil
}

func (f *fakeMessenger) SendCard(_ context.Context, spaceID string, card Card) (string, error) {
	f.messages = append(f.messages, SendMessageRequest{SpaceID: spaceID, Card: &card})
	return "msg-1", nil
}
func (f *fakeMessenger) UpdateMessage(context.Context, string, SendMessageRequest) error { return nil }
func (f *fakeMessenger) OpenDialog(context.Context, string, Dialog) error                { return nil }
func (f *fakeMessenger) UpdateDialog(context.Context, string, Dialog) error              { return nil }
func (f *fakeMessenger) GetUser(context.Context, string) (*ChatUser, error)              { return nil, nil }
func (f *fakeMessenger) SetAgentIdentity(context.Context, AgentIdentity) error           { return nil }

// newTestStore creates an ephemeral SQLite store in a temp directory.
func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("creating test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestHandleUserMessage_NoSubscriptionRequired verifies that a direct message
// from an agent to a user is delivered even when the user has zero subscriptions.
func TestHandleUserMessage_NoSubscriptionRequired(t *testing.T) {
	store := newTestStore(t)

	// Seed a user mapping and a space link but NO subscriptions.
	if err := store.SetUserMapping(&state.UserMapping{
		PlatformUserID: "users/12345",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "test@example.com",
		RegisteredBy:   "auto",
	}); err != nil {
		t.Fatalf("setting user mapping: %v", err)
	}

	if err := store.SetSpaceLink(&state.SpaceLink{
		SpaceID:   "spaces/AAQAx",
		Platform:  "googlechat",
		ProjectID:   "grove-abc",
		ProjectSlug: "my-grove",
		LinkedBy:  "test",
	}); err != nil {
		t.Fatalf("setting space link: %v", err)
	}

	fm := &fakeMessenger{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	relay := NewNotificationRelay(store, fm, log)

	msg := &messages.StructuredMessage{
		Sender:      "agent:simon",
		RecipientID: "hub-user-1",
		Msg:         "Here is the answer to your question.",
		Type:        messages.TypeInstruction,
	}

	err := relay.HandleBrokerMessage(context.Background(),
		"scion.grove.grove-abc.user.hub-user-1.messages", msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.messages) == 0 {
		t.Fatal("expected at least one message to be sent, got none — direct messages must not require subscriptions")
	}

	got := fm.messages[0]
	if got.SpaceID != "spaces/AAQAx" {
		t.Errorf("message sent to wrong space: got %q, want %q", got.SpaceID, "spaces/AAQAx")
	}
	if got.Card == nil {
		t.Fatal("expected a card in the message")
	}
	wantTitle := "\U0001F916 simon"
	if got.Card.Header.Title != wantTitle {
		t.Errorf("card title = %q, want %q", got.Card.Header.Title, wantTitle)
	}

	// @mentions should be in the text body, not inside the card
	if got.Text != "<users/12345>" {
		t.Errorf("text body = %q, want @mention %q", got.Text, "<users/12345>")
	}

	// Card should have no action buttons
	if len(got.Card.Actions) != 0 {
		t.Errorf("expected no card actions, got %d", len(got.Card.Actions))
	}
}

// TestHandleUserMessage_RoutesNonInstructionToNotification verifies that
// state-change and input-needed messages on the user topic are rendered as
// notification cards (via handleAgentNotification) rather than as direct
// agent response cards. Only explicit instruction messages should use the
// direct-message card format.
func TestHandleUserMessage_RoutesNonInstructionToNotification(t *testing.T) {
	store := newTestStore(t)

	if err := store.SetUserMapping(&state.UserMapping{
		PlatformUserID: "users/12345",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "test@example.com",
		RegisteredBy:   "auto",
	}); err != nil {
		t.Fatalf("setting user mapping: %v", err)
	}

	if err := store.SetSpaceLink(&state.SpaceLink{
		SpaceID:   "spaces/AAQAx",
		Platform:  "googlechat",
		ProjectID:   "grove-abc",
		ProjectSlug: "my-grove",
		LinkedBy:  "test",
	}); err != nil {
		t.Fatalf("setting space link: %v", err)
	}

	fm := &fakeMessenger{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	relay := NewNotificationRelay(store, fm, log)

	for _, tc := range []struct {
		name    string
		msgType string
	}{
		{"state-change", messages.TypeStateChange},
		{"input-needed", messages.TypeInputNeeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm.messages = nil
			msg := &messages.StructuredMessage{
				Sender:      "agent:simon",
				RecipientID: "hub-user-1",
				Msg:         "agent COMPLETED its task",
				Type:        tc.msgType,
			}

			err := relay.HandleBrokerMessage(context.Background(),
				"scion.grove.grove-abc.user.hub-user-1.messages", msg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(fm.messages) == 0 {
				t.Fatal("expected a notification card to be sent")
			}

			got := fm.messages[0]
			if got.SpaceID != "spaces/AAQAx" {
				t.Errorf("message sent to wrong space: got %q, want %q", got.SpaceID, "spaces/AAQAx")
			}
			if got.Card == nil {
				t.Fatal("expected a card in the message")
			}
			// Notification cards include a subtitle with the activity status,
			// unlike direct-message cards which only have the agent name.
			if got.Card.Header.Subtitle == "" {
				t.Error("expected notification card to have a subtitle with activity status")
			}
		})
	}
}

func TestExtractActivity_UsesStatusField(t *testing.T) {
	tests := []struct {
		name    string
		msg     *messages.StructuredMessage
		wantAct string
	}{
		{
			name:    "status field takes precedence over content",
			msg:     &messages.StructuredMessage{Msg: "agent COMPLETED something", Status: "ERROR"},
			wantAct: "ERROR",
		},
		{
			name:    "status field normalized to uppercase",
			msg:     &messages.StructuredMessage{Msg: "some message", Status: "stalled"},
			wantAct: "STALLED",
		},
		{
			name:    "falls back to content matching when status empty",
			msg:     &messages.StructuredMessage{Msg: "agent has COMPLETED"},
			wantAct: "COMPLETED",
		},
		{
			name:    "falls back to type when no content match",
			msg:     &messages.StructuredMessage{Msg: "some generic message", Type: messages.TypeInputNeeded},
			wantAct: "WAITING_FOR_INPUT",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractActivity(tc.msg)
			if got != tc.wantAct {
				t.Errorf("extractActivity() = %q, want %q", got, tc.wantAct)
			}
		})
	}
}

func TestHandleUserMessage_AssistantReplyTruncated(t *testing.T) {
	store := newTestStore(t)

	if err := store.SetUserMapping(&state.UserMapping{
		PlatformUserID: "users/12345",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "test@example.com",
		RegisteredBy:   "auto",
	}); err != nil {
		t.Fatalf("setting user mapping: %v", err)
	}

	if err := store.SetSpaceLink(&state.SpaceLink{
		SpaceID:   "spaces/AAQAx",
		Platform:  "googlechat",
		ProjectID:   "grove-abc",
		ProjectSlug: "my-grove",
		LinkedBy:  "test",
	}); err != nil {
		t.Fatalf("setting space link: %v", err)
	}

	fm := &fakeMessenger{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	relay := NewNotificationRelay(store, fm, log)

	longText := strings.Repeat("x", 2000)
	msg := &messages.StructuredMessage{
		Sender:      "agent:claude-agent",
		RecipientID: "hub-user-1",
		Msg:         longText,
		Type:        messages.TypeAssistantReply,
	}

	err := relay.HandleBrokerMessage(context.Background(),
		"scion.grove.grove-abc.user.hub-user-1.messages", msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.messages) == 0 {
		t.Fatal("expected a message to be sent")
	}

	got := fm.messages[0]
	if got.Card == nil {
		t.Fatal("expected a card in the message")
	}

	wantTitle := "\U0001F916 claude-agent"
	if got.Card.Header.Title != wantTitle {
		t.Errorf("card title = %q, want %q", got.Card.Header.Title, wantTitle)
	}
	if got.Card.Header.Subtitle != "" {
		t.Errorf("assistant-reply should use direct message card (no subtitle), got subtitle = %q", got.Card.Header.Subtitle)
	}

	if len(got.Card.Sections) == 0 {
		t.Fatal("expected at least one card section")
	}
	cardContent := got.Card.Sections[0].Widgets[0].Content
	if len(cardContent) > 600 {
		t.Errorf("card content should be truncated, got %d chars", len(cardContent))
	}
	if !strings.Contains(cardContent, "chars truncated") {
		t.Error("truncated card should contain truncation notice")
	}
}

func TestHandleUserMessage_ShortAssistantReplyNotTruncated(t *testing.T) {
	store := newTestStore(t)

	if err := store.SetUserMapping(&state.UserMapping{
		PlatformUserID: "users/12345",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "test@example.com",
		RegisteredBy:   "auto",
	}); err != nil {
		t.Fatalf("setting user mapping: %v", err)
	}

	if err := store.SetSpaceLink(&state.SpaceLink{
		SpaceID:   "spaces/AAQAx",
		Platform:  "googlechat",
		ProjectID:   "grove-abc",
		ProjectSlug: "my-grove",
		LinkedBy:  "test",
	}); err != nil {
		t.Fatalf("setting space link: %v", err)
	}

	fm := &fakeMessenger{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	relay := NewNotificationRelay(store, fm, log)

	shortText := "Task completed successfully."
	msg := &messages.StructuredMessage{
		Sender:      "agent:claude-agent",
		RecipientID: "hub-user-1",
		Msg:         shortText,
		Type:        messages.TypeAssistantReply,
	}

	err := relay.HandleBrokerMessage(context.Background(),
		"scion.grove.grove-abc.user.hub-user-1.messages", msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.messages) == 0 {
		t.Fatal("expected a message to be sent")
	}

	got := fm.messages[0]
	if got.Card == nil {
		t.Fatal("expected a card")
	}
	cardContent := got.Card.Sections[0].Widgets[0].Content
	if cardContent != shortText {
		t.Errorf("short message should not be truncated, got %q, want %q", cardContent, shortText)
	}
}
