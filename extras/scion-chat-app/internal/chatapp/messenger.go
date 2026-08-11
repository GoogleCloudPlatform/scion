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
	"io"
)

// Messenger abstracts chat platform operations.
type Messenger interface {
	SendMessage(ctx context.Context, req SendMessageRequest) (string, error)
	SendCard(ctx context.Context, spaceID string, card Card) (string, error)
	UpdateMessage(ctx context.Context, messageID string, req SendMessageRequest) error
	OpenDialog(ctx context.Context, triggerID string, dialog Dialog) error
	UpdateDialog(ctx context.Context, triggerID string, dialog Dialog) error
	GetUser(ctx context.Context, userID string) (*ChatUser, error)
	SetAgentIdentity(ctx context.Context, agent AgentIdentity) error
	UploadMedia(ctx context.Context, spaceID, filename string, content io.Reader) (string, error)
}

// SendMessageRequest contains parameters for sending a message to a chat space.
type SendMessageRequest struct {
	SpaceID     string
	ThreadID    string
	ThreadKey   string // Platform-specific thread key for creating new threads.
	Text        string
	Card        *Card
	AgentID     string
	Attachments []Attachment
}

// Attachment describes a file to send or received from a chat platform.
type Attachment struct {
	Filename string // display name of the file
	Path     string // local file path (for outbound)
	URL      string // download URL (for inbound)
	Size     int64
}

// AgentIdentity represents the visual identity of an agent in chat.
type AgentIdentity struct {
	Slug    string
	Name    string
	IconURL string
	GroveID string
}

// ChatUser represents a user on a chat platform.
type ChatUser struct {
	PlatformID  string
	DisplayName string
	Email       string
}

// AttachmentDownloader can download files from chat platform attachment URIs.
type AttachmentDownloader interface {
	DownloadAttachment(ctx context.Context, downloadURI string) (io.ReadCloser, error)
}
