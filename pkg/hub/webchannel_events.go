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

package hub

// TopicEvent is published on project.<id>.chat.topic when a topic is
// created, updated, or deleted. It carries the action and the full
// topic snapshot so subscribers can update their local state without
// a subsequent REST fetch.
type TopicEvent struct {
	Action string       `json:"action"` // "created", "updated", "deleted"
	Topic  WebChatTopic `json:"topic"`
}

// ChatReadStateEvent is published on user.<peerID>.chat.read-state when a DM
// participant advances their read watermark. The peer's client uses it to
// render the "seen" indicator on the messages it sent.
type ChatReadStateEvent struct {
	ConversationKey string `json:"conversationKey"`
	// UserID is the reader — the participant whose watermark moved.
	UserID    string `json:"userId"`
	MessageID string `json:"messageId"`
	ReadAt    string `json:"readAt"`
}

// ChatMessageEditedEvent is published when a message is edited.
// Published on project.<projectID>.chat.message.edited and
// user.<id>.chat.dm for DM edits.
type ChatMessageEditedEvent struct {
	ConversationKey string `json:"conversationKey"`
	MessageID       string `json:"messageId"`
	Content         string `json:"content"`
	EditedAt        string `json:"editedAt"`
}

// ChatMessageDeletedEvent is published when a message is soft-deleted.
// Published on project.<projectID>.chat.message.deleted and
// user.<id>.chat.dm for DM deletes.
type ChatMessageDeletedEvent struct {
	ConversationKey string `json:"conversationKey"`
	MessageID       string `json:"messageId"`
	DeletedAt       string `json:"deletedAt"`
}

// DMPromotedEvent is published on user.<id>.chat.dm.promoted when a DM
// is promoted to a space thread. The DM participant's client should close
// the DM view and optionally navigate to the new thread.
type DMPromotedEvent struct {
	// OldConversationKey is the DM key that was promoted.
	OldConversationKey string `json:"oldConversationKey"`
	// NewTopic is the created thread.
	NewTopic WebChatTopic `json:"newTopic"`
}
