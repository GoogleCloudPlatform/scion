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
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/chatapp"
)

func jsonNumber(s string) json.Number {
	return json.Number(s)
}

func TestNormalizeEvent_UserEmail(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, slog.Default())

	tests := []struct {
		name      string
		raw       rawEvent
		wantEmail string
		wantID    string
	}{
		{
			name: "email populated from user object",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{
						Name:  "users/12345",
						Email: "alice@example.com",
					},
					AppCommandPayload: &rawAppCommandPayload{
						Space: &rawSpace{Name: "spaces/abc"},
						AppCommandMetadata: &rawAppCommandMetadata{
							AppCommandId: "1",
						},
					},
				},
			},
			wantEmail: "alice@example.com",
			wantID:    "users/12345",
		},
		{
			name: "empty email when user has no email",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{
						Name: "users/12345",
					},
					AppCommandPayload: &rawAppCommandPayload{
						Space: &rawSpace{Name: "spaces/abc"},
						AppCommandMetadata: &rawAppCommandMetadata{
							AppCommandId: "1",
						},
					},
				},
			},
			wantEmail: "",
			wantID:    "users/12345",
		},
		{
			name: "email populated for message events",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{
						Name:  "users/67890",
						Email: "bob@example.com",
					},
					MessagePayload: &rawMessagePayload{
						Message: &rawMessage{Text: "hello"},
						Space:   &rawSpace{Name: "spaces/xyz"},
					},
				},
			},
			wantEmail: "bob@example.com",
			wantID:    "users/67890",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := adapter.normalizeEvent(&tt.raw)
			if event == nil {
				t.Fatal("normalizeEvent returned nil")
			}
			if event.UserEmail != tt.wantEmail {
				t.Errorf("UserEmail = %q, want %q", event.UserEmail, tt.wantEmail)
			}
			if event.UserID != tt.wantID {
				t.Errorf("UserID = %q, want %q", event.UserID, tt.wantID)
			}
			if event.Platform != PlatformName {
				t.Errorf("Platform = %q, want %q", event.Platform, PlatformName)
			}
		})
	}
}

func TestNormalizeEvent_CommandIDMapping(t *testing.T) {
	adapter := NewAdapter(Config{
		CommandIDMap: map[string]string{
			"1": "scion",
			"2": "scionAdmin",
		},
	}, nil, nil, slog.Default())

	tests := []struct {
		name        string
		commandID   string
		wantCommand string
	}{
		{
			name:        "command ID 1 maps to scion",
			commandID:   "1",
			wantCommand: "scion",
		},
		{
			name:        "command ID 2 maps to scionAdmin",
			commandID:   "2",
			wantCommand: "scionAdmin",
		},
		{
			name:        "unknown command ID falls back to scion",
			commandID:   "99",
			wantCommand: "scion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := adapter.normalizeEvent(&rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					AppCommandPayload: &rawAppCommandPayload{
						Space:              &rawSpace{Name: "spaces/s"},
						AppCommandMetadata: &rawAppCommandMetadata{AppCommandId: jsonNumber(tt.commandID)},
					},
				},
			})
			if event == nil {
				t.Fatal("normalizeEvent returned nil")
			}
			if event.Command != tt.wantCommand {
				t.Errorf("Command = %q, want %q", event.Command, tt.wantCommand)
			}
		})
	}
}

func TestNormalizeEvent_CommandNameFromText(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, slog.Default())

	tests := []struct {
		name        string
		raw         rawEvent
		wantCommand string
	}{
		{
			name: "appCommandPayload resolves command name from message text",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					AppCommandPayload: &rawAppCommandPayload{
						Space:              &rawSpace{Name: "spaces/s"},
						AppCommandMetadata: &rawAppCommandMetadata{AppCommandId: jsonNumber("99")},
						Message: &rawMessage{
							Text:         "/scionAdmin list",
							ArgumentText: "list",
						},
					},
				},
			},
			wantCommand: "scionAdmin",
		},
		{
			name: "messagePayload resolves command name from message text",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					MessagePayload: &rawMessagePayload{
						Space: &rawSpace{Name: "spaces/s"},
						Message: &rawMessage{
							Text:         "/scionAdmin help",
							ArgumentText: "help",
							SlashCommand: &rawSlashCommand{CommandId: jsonNumber("42")},
						},
					},
				},
			},
			wantCommand: "scionAdmin",
		},
		{
			name: "appCommandPayload with no message falls back to scion",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					AppCommandPayload: &rawAppCommandPayload{
						Space:              &rawSpace{Name: "spaces/s"},
						AppCommandMetadata: &rawAppCommandMetadata{AppCommandId: jsonNumber("99")},
					},
				},
			},
			wantCommand: "scion",
		},
		{
			name: "appCommandPayload slashCommand fallback resolves from text",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					AppCommandPayload: &rawAppCommandPayload{
						Space: &rawSpace{Name: "spaces/s"},
						Message: &rawMessage{
							Text:         "/scionAdmin info",
							ArgumentText: "info",
							SlashCommand: &rawSlashCommand{CommandId: jsonNumber("77")},
						},
					},
				},
			},
			wantCommand: "scionAdmin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := adapter.normalizeEvent(&tt.raw)
			if event == nil {
				t.Fatal("normalizeEvent returned nil")
			}
			if event.Command != tt.wantCommand {
				t.Errorf("Command = %q, want %q", event.Command, tt.wantCommand)
			}
		})
	}
}

func TestNormalizeEvent_SlashCommandInMessage(t *testing.T) {
	adapter := NewAdapter(Config{
		CommandIDMap: map[string]string{
			"1": "scion",
			"2": "scionAdmin",
		},
	}, nil, nil, slog.Default())

	tests := []struct {
		name        string
		raw         rawEvent
		wantType    chatapp.ChatEventType
		wantCommand string
		wantArgs    string
	}{
		{
			name: "messagePayload with slashCommand routes as command",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					MessagePayload: &rawMessagePayload{
						Space: &rawSpace{Name: "spaces/s"},
						Message: &rawMessage{
							ArgumentText: "help",
							SlashCommand: &rawSlashCommand{CommandId: jsonNumber("2")},
						},
					},
				},
			},
			wantType:    chatapp.EventCommand,
			wantCommand: "scionAdmin",
			wantArgs:    "help",
		},
		{
			name: "messagePayload without slashCommand remains a message",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					MessagePayload: &rawMessagePayload{
						Space:   &rawSpace{Name: "spaces/s"},
						Message: &rawMessage{Text: "hello"},
					},
				},
			},
			wantType:    chatapp.EventMessage,
			wantCommand: "",
			wantArgs:    "",
		},
		{
			name: "appCommandPayload falls back to message slashCommand",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					AppCommandPayload: &rawAppCommandPayload{
						Space: &rawSpace{Name: "spaces/s"},
						Message: &rawMessage{
							ArgumentText: "info",
							SlashCommand: &rawSlashCommand{CommandId: jsonNumber("2")},
						},
					},
				},
			},
			wantType:    chatapp.EventCommand,
			wantCommand: "scionAdmin",
			wantArgs:    "info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := adapter.normalizeEvent(&tt.raw)
			if event == nil {
				t.Fatal("normalizeEvent returned nil")
			}
			if event.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", event.Type, tt.wantType)
			}
			if event.Command != tt.wantCommand {
				t.Errorf("Command = %q, want %q", event.Command, tt.wantCommand)
			}
			if tt.wantArgs != "" && event.Args != tt.wantArgs {
				t.Errorf("Args = %q, want %q", event.Args, tt.wantArgs)
			}
		})
	}
}

func TestNormalizeEvent_NilChat(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, slog.Default())
	event := adapter.normalizeEvent(&rawEvent{})
	if event != nil {
		t.Errorf("expected nil event for empty rawEvent, got %+v", event)
	}
}

// --- extractAttachments tests ---

func TestExtractAttachments(t *testing.T) {
	tests := []struct {
		name string
		msg  *rawMessage
		want int // number of attachments expected
	}{
		{
			name: "nil message returns nil",
			msg:  nil,
			want: 0,
		},
		{
			name: "message with no attachments",
			msg:  &rawMessage{Text: "hello"},
			want: 0,
		},
		{
			name: "single attachment",
			msg: &rawMessage{
				Attachment: []rawAttachment{
					{
						Name:        "attachments/abc",
						ContentName: "report.pdf",
						ContentType: "application/pdf",
						DownloadURI: "https://example.com/download/abc",
					},
				},
			},
			want: 1,
		},
		{
			name: "attachment without download URI is skipped",
			msg: &rawMessage{
				Attachment: []rawAttachment{
					{
						Name:        "attachments/abc",
						ContentName: "report.pdf",
						ContentType: "application/pdf",
						DownloadURI: "", // empty
					},
				},
			},
			want: 0,
		},
		{
			name: "multiple attachments mixed",
			msg: &rawMessage{
				Attachment: []rawAttachment{
					{Name: "a/1", ContentName: "file1.txt", DownloadURI: "https://example.com/1"},
					{Name: "a/2", ContentName: "", DownloadURI: "https://example.com/2"}, // empty content name, uses Name
					{Name: "a/3", ContentName: "file3.txt", DownloadURI: ""},             // skipped: no URI
					{Name: "a/4", ContentName: "file4.txt", DownloadURI: "https://example.com/4"},
				},
			},
			want: 3, // a/1, a/2, a/4
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAttachments(tt.msg)
			if len(got) != tt.want {
				t.Errorf("extractAttachments returned %d attachments, want %d", len(got), tt.want)
			}
		})
	}
}

func TestExtractAttachments_FallbackName(t *testing.T) {
	msg := &rawMessage{
		Attachment: []rawAttachment{
			{
				Name:        "attachments/some-id/report.pdf",
				ContentName: "", // empty content name
				ContentType: "application/pdf",
				DownloadURI: "https://example.com/download",
			},
		},
	}
	got := extractAttachments(msg)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(got))
	}
	// Should fall back to filepath.Base(Name) = "report.pdf"
	if got[0].Name != "report.pdf" {
		t.Errorf("Name = %q, want %q", got[0].Name, "report.pdf")
	}
}

func TestExtractAttachments_PreferContentName(t *testing.T) {
	msg := &rawMessage{
		Attachment: []rawAttachment{
			{
				Name:        "attachments/id",
				ContentName: "user-named.docx",
				ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
				DownloadURI: "https://example.com/download",
			},
		},
	}
	got := extractAttachments(msg)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(got))
	}
	if got[0].Name != "user-named.docx" {
		t.Errorf("Name = %q, want %q", got[0].Name, "user-named.docx")
	}
}

// --- extractAttachments via normalizeEvent integration ---

func TestNormalizeEvent_MessageWithAttachments(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, slog.Default())

	event := adapter.normalizeEvent(&rawEvent{
		Chat: &rawChatPayload{
			User: &rawUser{Name: "users/1", Email: "u@e.com"},
			MessagePayload: &rawMessagePayload{
				Space: &rawSpace{Name: "spaces/s"},
				Message: &rawMessage{
					Text: "here are the files",
					Attachment: []rawAttachment{
						{
							ContentName: "data.csv",
							ContentType: "text/csv",
							DownloadURI: "https://example.com/dl/1",
						},
						{
							ContentName: "image.png",
							ContentType: "image/png",
							DownloadURI: "https://example.com/dl/2",
						},
					},
				},
			},
		},
	})
	if event == nil {
		t.Fatal("normalizeEvent returned nil")
	}
	if event.Type != chatapp.EventMessage {
		t.Errorf("Type = %q, want %q", event.Type, chatapp.EventMessage)
	}
	if len(event.Attachments) != 2 {
		t.Errorf("got %d attachments, want 2", len(event.Attachments))
	}
	if event.Attachments[0].Name != "data.csv" {
		t.Errorf("Attachments[0].Name = %q, want %q", event.Attachments[0].Name, "data.csv")
	}
}

// --- UploadMedia tests ---

func TestUploadMedia_MultipartRelatedFormat(t *testing.T) {
	// Set up a test server that captures the request to verify multipart format.
	var capturedContentType string
	var capturedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedContentType = r.Header.Get("Content-Type")
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"attachmentDataRef":{"resourceName":"spaces/abc/attachments/123"}}`))
	}))
	defer server.Close()

	adapter := NewAdapter(Config{}, nil, server.Client(), slog.Default())
	// Override the upload URL base for testing.
	content := strings.NewReader("file content here")

	// We need to override the URL. Since UploadMedia constructs the URL internally,
	// we'll test via the captured request. We need to make the adapter point to our
	// test server. Let's test directly by calling with a space that routes to our server.
	// Patch the upload URL by using the test server URL.
	result, err := adapter.uploadMediaToURL(context.Background(), server.URL+"/upload", "test.txt", content)
	if err != nil {
		t.Fatalf("UploadMedia failed: %v", err)
	}
	if result != "spaces/abc/attachments/123" {
		t.Errorf("resource name = %q, want %q", result, "spaces/abc/attachments/123")
	}

	// Verify Content-Type is multipart/related, not multipart/form-data.
	if !strings.HasPrefix(capturedContentType, "multipart/related") {
		t.Errorf("Content-Type = %q, want multipart/related prefix", capturedContentType)
	}

	// Parse the multipart body and verify both parts use CreatePart style.
	_, params, err := mime.ParseMediaType(capturedContentType)
	if err != nil {
		t.Fatalf("parsing Content-Type: %v", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatal("no boundary in Content-Type")
	}

	reader := multipart.NewReader(strings.NewReader(string(capturedBody)), boundary)

	// Part 1: JSON metadata.
	part1, err := reader.NextPart()
	if err != nil {
		t.Fatalf("reading part 1: %v", err)
	}
	if ct := part1.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("part 1 Content-Type = %q, want %q", ct, "application/json")
	}
	// Part 1 should NOT have Content-Disposition: form-data.
	if cd := part1.Header.Get("Content-Disposition"); strings.Contains(cd, "form-data") {
		t.Errorf("part 1 should not have form-data disposition, got: %q", cd)
	}
	meta, _ := io.ReadAll(part1)
	if !strings.Contains(string(meta), "test.txt") {
		t.Errorf("metadata should contain filename, got: %s", string(meta))
	}

	// Part 2: file content.
	part2, err := reader.NextPart()
	if err != nil {
		t.Fatalf("reading part 2: %v", err)
	}
	if ct := part2.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("part 2 Content-Type = %q, want %q", ct, "application/octet-stream")
	}
	// Part 2 should NOT have Content-Disposition: form-data.
	if cd := part2.Header.Get("Content-Disposition"); strings.Contains(cd, "form-data") {
		t.Errorf("part 2 should not have form-data disposition, got: %q", cd)
	}
	fileContent, _ := io.ReadAll(part2)
	if string(fileContent) != "file content here" {
		t.Errorf("file content = %q, want %q", string(fileContent), "file content here")
	}

	// Should be no more parts.
	_, err = reader.NextPart()
	if err != io.EOF {
		t.Errorf("expected EOF after 2 parts, got: %v", err)
	}
}

func TestUploadMedia_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"permission denied"}`))
	}))
	defer server.Close()

	adapter := NewAdapter(Config{}, nil, server.Client(), slog.Default())
	content := strings.NewReader("data")

	_, err := adapter.uploadMediaToURL(context.Background(), server.URL+"/upload", "test.txt", content)
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status code 403, got: %v", err)
	}
}

func TestNormalizeEvent_EventTypes(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, slog.Default())

	tests := []struct {
		name     string
		raw      rawEvent
		wantType chatapp.ChatEventType
	}{
		{
			name: "app command",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User: &rawUser{Name: "users/1", Email: "u@e.com"},
					AppCommandPayload: &rawAppCommandPayload{
						Space:              &rawSpace{Name: "spaces/s"},
						AppCommandMetadata: &rawAppCommandMetadata{AppCommandId: "1"},
					},
				},
			},
			wantType: chatapp.EventCommand,
		},
		{
			name: "added to space",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User:                &rawUser{Name: "users/1", Email: "u@e.com"},
					AddedToSpacePayload: &rawAddedToSpacePayload{Space: &rawSpace{Name: "spaces/s"}},
				},
			},
			wantType: chatapp.EventSpaceJoin,
		},
		{
			name: "removed from space",
			raw: rawEvent{
				Chat: &rawChatPayload{
					User:                    &rawUser{Name: "users/1"},
					RemovedFromSpacePayload: &rawRemovedFromSpacePayload{Space: &rawSpace{Name: "spaces/s"}},
				},
			},
			wantType: chatapp.EventSpaceRemove,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := adapter.normalizeEvent(&tt.raw)
			if event == nil {
				t.Fatal("normalizeEvent returned nil")
			}
			if event.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", event.Type, tt.wantType)
			}
		})
	}
}
