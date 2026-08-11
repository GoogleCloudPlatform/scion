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

package googlechat

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/chatapp"
)

func TestParseSubscriptionResource(t *testing.T) {
	tests := []struct {
		name      string
		resource  string
		wantProj  string
		wantSub   string
		wantError bool
	}{
		{
			name:     "valid resource",
			resource: "projects/my-proj/subscriptions/chat-events",
			wantProj: "my-proj",
			wantSub:  "chat-events",
		},
		{
			name:      "missing projects prefix",
			resource:  "my-proj/subscriptions/chat-events",
			wantError: true,
		},
		{
			name:      "missing subscriptions segment",
			resource:  "projects/my-proj/topics/chat-events",
			wantError: true,
		},
		{
			name:      "too few segments",
			resource:  "projects/my-proj",
			wantError: true,
		},
		{
			name:      "too many segments",
			resource:  "projects/my-proj/subscriptions/chat-events/extra",
			wantError: true,
		},
		{
			name:      "empty string",
			resource:  "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proj, sub, err := parseSubscriptionResource(tt.resource)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error for resource %q, got nil", tt.resource)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for resource %q: %v", tt.resource, err)
				return
			}
			if proj != tt.wantProj {
				t.Errorf("projectID = %q, want %q", proj, tt.wantProj)
			}
			if sub != tt.wantSub {
				t.Errorf("subscriptionID = %q, want %q", sub, tt.wantSub)
			}
		})
	}
}

func TestDialogToCard(t *testing.T) {
	dialog := &chatapp.Dialog{
		Title: "Test Dialog",
		Fields: []chatapp.DialogField{
			{
				ID:          "name",
				Label:       "Name",
				Type:        "text",
				Placeholder: "Enter name",
			},
			{
				ID:    "role",
				Label: "Role",
				Type:  "select",
				Options: []chatapp.SelectOption{
					{Label: "Admin", Value: "admin"},
					{Label: "User", Value: "user"},
				},
			},
			{
				ID:    "notes",
				Label: "Notes",
				Type:  "textarea",
			},
		},
		Submit: chatapp.CardAction{Label: "OK", ActionID: "submit"},
		Cancel: chatapp.CardAction{Label: "Cancel", ActionID: "cancel"},
	}

	card := dialogToCard(dialog)

	if card.Header.Title != "Test Dialog" {
		t.Errorf("card header title = %q, want %q", card.Header.Title, "Test Dialog")
	}

	if len(card.Sections) != 1 {
		t.Fatalf("card sections count = %d, want 1", len(card.Sections))
	}

	widgets := card.Sections[0].Widgets
	if len(widgets) != 3 {
		t.Fatalf("widget count = %d, want 3", len(widgets))
	}

	// First widget: text input
	if widgets[0].Type != chatapp.WidgetInput {
		t.Errorf("widget[0].Type = %q, want %q", widgets[0].Type, chatapp.WidgetInput)
	}
	if widgets[0].Label != "Name" {
		t.Errorf("widget[0].Label = %q, want %q", widgets[0].Label, "Name")
	}

	// Second widget: select → checkbox
	if widgets[1].Type != chatapp.WidgetCheckbox {
		t.Errorf("widget[1].Type = %q, want %q", widgets[1].Type, chatapp.WidgetCheckbox)
	}
	if len(widgets[1].Options) != 2 {
		t.Errorf("widget[1].Options count = %d, want 2", len(widgets[1].Options))
	}

	// Third widget: textarea → input
	if widgets[2].Type != chatapp.WidgetInput {
		t.Errorf("widget[2].Type = %q, want %q", widgets[2].Type, chatapp.WidgetInput)
	}
}

func TestDialogToCard_EmptyDialog(t *testing.T) {
	dialog := &chatapp.Dialog{
		Title: "Empty",
	}

	card := dialogToCard(dialog)
	if card.Header.Title != "Empty" {
		t.Errorf("card header title = %q, want %q", card.Header.Title, "Empty")
	}
	if len(card.Sections) != 0 {
		t.Errorf("expected no sections for empty dialog, got %d", len(card.Sections))
	}
}

func TestNormalizeEvent_MessageName(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, nil)

	tests := []struct {
		name            string
		raw             rawEvent
		wantMessageName string
	}{
		{
			name: "message name from messagePayload",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1"},
					MessagePayload: &rawMessagePayload{
						Message: &rawMessage{
							Name: "spaces/s1/messages/m1",
							Text: "hello",
						},
						Space: &rawSpace{Name: "spaces/s1"},
					},
				},
			},
			wantMessageName: "spaces/s1/messages/m1",
		},
		{
			name: "message name from appCommandPayload",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1"},
					AppCommandPayload: &rawAppCommandPayload{
						Space: &rawSpace{Name: "spaces/s1"},
						AppCommandMetadata: &rawAppCommandMetadata{
							AppCommandId: jsonNumber("1"),
						},
						Message: &rawMessage{
							Name:         "spaces/s1/messages/m2",
							Text:         "/scion help",
							ArgumentText: "help",
						},
					},
				},
			},
			wantMessageName: "spaces/s1/messages/m2",
		},
		{
			name: "message name from buttonClickedPayload",
			raw: rawEvent{
				CommonEventObject: &rawCommonEventObject{
					Parameters: map[string]string{
						"action": "test_action",
					},
				},
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1"},
					ButtonClickedPayload: &rawButtonClickedPayload{
						Space: &rawSpace{Name: "spaces/s1"},
						Message: &rawMessage{
							Name: "spaces/s1/messages/m3",
						},
					},
				},
			},
			wantMessageName: "spaces/s1/messages/m3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := adapter.normalizeEvent(&tt.raw)
			if event == nil {
				t.Fatal("normalizeEvent returned nil")
			}
			if event.MessageName != tt.wantMessageName {
				t.Errorf("MessageName = %q, want %q", event.MessageName, tt.wantMessageName)
			}
		})
	}
}
