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

// ChatEvent represents a normalized inbound event from a chat platform.
type ChatEvent struct {
	Type            ChatEventType
	Platform        string
	SpaceID         string
	ThreadID        string
	MessageName     string // message resource name (e.g. "spaces/x/messages/y") for UpdateMessage
	UserID          string
	UserEmail       string // user's email from the chat platform (Google-asserted identity)
	UserDisplayName string
	Text            string
	Command         string
	Args            string
	ActionID        string
	ActionData      string
	DialogData      map[string]string
	Attachments     []EventAttachment // files uploaded by the user
	InteractionAdd  bool              // true when added to space via @mention (multi-turn)
	IsDialogEvent   bool              // true when the event is a dialog request/submit
}

// EventAttachment represents a file attached to an inbound chat event.
type EventAttachment struct {
	Name        string // display name of the file
	ContentType string // MIME type
	DownloadURI string // URL to download the file content (requires authenticated client)
}

// EventResponse holds an optional synchronous response to return in the HTTP body.
// Handlers return nil for async-only responses (200 OK with empty body).
type EventResponse struct {
	// Message, if set, creates a message synchronously in the response.
	Message *SendMessageRequest
	// UpdateMessage, if set, updates the triggering message synchronously.
	UpdateMessage *SendMessageRequest
	// Dialog, if set, opens a dialog via pushCard navigation.
	Dialog *Dialog
	// CloseDialog, if true, closes the current dialog.
	CloseDialog bool
	// Notification is optional text shown as a snackbar after dialog close.
	Notification string
}

// ChatEventType identifies the kind of inbound chat event.
type ChatEventType string

const (
	EventMessage      ChatEventType = "message"
	EventCommand      ChatEventType = "command"
	EventAction       ChatEventType = "action"
	EventDialogSubmit ChatEventType = "dialog_submit"
	EventSpaceJoin    ChatEventType = "space_join"
	EventSpaceRemove  ChatEventType = "space_remove"
)
