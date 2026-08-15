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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// An agent attaches files by path — the web chat renders them only once they
// exist as W7 attachment records. These tests pin that translation: the path
// maps back to the hub host, the file is copied into the attachment store, and
// the message it arrived with links to it.

func TestAgentAttachmentHostPath(t *testing.T) {
	const shared = "/srv/project-configs/demo/shared-dirs/scratchpad"

	tests := []struct {
		name      string
		agentPath string
		want      string
		wantOK    bool
	}{
		{
			name:      "staged under the scratchpad mount",
			agentPath: "/scion-volumes/scratchpad/.attachments/sender/msg1/shot.png",
			want:      shared + "/.attachments/sender/msg1/shot.png",
			wantOK:    true,
		},
		{
			name:      "in-workspace mount point",
			agentPath: "/workspace/.scion-volumes/scratchpad/.attachments/sender/msg1/shot.png",
			want:      shared + "/.attachments/sender/msg1/shot.png",
			wantOK:    true,
		},
		{
			name:      "traversal out of the shared dir",
			agentPath: "/scion-volumes/scratchpad/../../etc/passwd",
			wantOK:    false,
		},
		{
			name:      "a hub-host path the agent never saw",
			agentPath: "/root/.scion/settings.yaml",
			wantOK:    false,
		},
		{
			name:      "another shared dir",
			agentPath: "/scion-volumes/secrets/token",
			wantOK:    false,
		},
		{
			name:      "the mount point itself",
			agentPath: "/scion-volumes/scratchpad",
			wantOK:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := agentAttachmentHostPath(tc.agentPath, shared)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (path %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("path = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttachmentMimeForName(t *testing.T) {
	tests := map[string]string{
		"shot.PNG":   "image/png",
		"notes.md":   "text/markdown",
		"main.go":    "text/plain",
		"widget.ts":  "text/plain", // not video/mp2t, whatever /etc/mime.types says
		"data.blob":  "application/octet-stream",
		"report.pdf": "application/pdf",
	}
	for name, want := range tests {
		if got := attachmentMimeForName(name); got != want {
			t.Errorf("attachmentMimeForName(%q) = %q, want %q", name, got, want)
		}
	}
}

// agentAttachmentServer wires a server with attachment storage plus a project
// whose scratchpad shared dir exists on this host, and returns that host dir.
func agentAttachmentServer(t *testing.T) (*Server, store.Store, *store.Project, string) {
	t.Helper()

	srv, s := testServer(t)

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalDiskAttachmentStore: %v", err)
	}
	srv.SetAttachmentStore(as)

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "attach-project",
		Slug:       "attach-project",
		Visibility: store.VisibilityPrivate,
		SharedDirs: []api.SharedDir{{Name: attachmentSharedDirName}},
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// sharedDirHostPath falls back to the conventional project-configs layout
	// under the home directory when no co-located broker reports a path.
	home := t.TempDir()
	t.Setenv("HOME", home)
	sharedDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	return srv, s, project, sharedDir
}

// stageAgentFile writes a file where the CLI would have staged it and returns
// the container-visible path the agent would send.
func stageAgentFile(t *testing.T, sharedDir, name, content string) string {
	t.Helper()

	dir := filepath.Join(sharedDir, ".attachments", "sender", "msg1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return "/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/" + name
}

func TestIngestAgentAttachments(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)
	ctx := context.Background()

	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")
	refs := srv.ingestAgentAttachments(ctx, project.ID, "agent-1", []string{
		staged,
		"/etc/passwd", // outside the shared dir: skipped, not fatal
		filepath.Join(sharedDir, ".attachments", "sender", "msg1", "notes.md"), // host path, not a mount path
	})

	if len(refs) != 1 {
		t.Fatalf("expected 1 recorded attachment, got %d (%+v)", len(refs), refs)
	}
	if refs[0].Name != "notes.md" || refs[0].MimeType != "text/markdown" {
		t.Errorf("unexpected ref: %+v", refs[0])
	}
	if refs[0].Size != int64(len("# hello\n")) {
		t.Errorf("size = %d, want %d", refs[0].Size, len("# hello\n"))
	}

	// The metadata row is queryable, and the bytes came back with it.
	meta, err := srv.webChatStore.GetAttachment(ctx, refs[0].ID)
	if err != nil || meta == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if meta.UploadedBy != "agent-1" {
		t.Errorf("uploadedBy = %q, want the sending agent", meta.UploadedBy)
	}
	reader, _, err := srv.attachmentStore.Get(ctx, project.ID, refs[0].ID)
	if err != nil {
		t.Fatalf("attachment store Get: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		t.Fatalf("read stored attachment: %v", err)
	}
	if buf.String() != "# hello\n" {
		t.Errorf("stored content = %q, want the staged file's bytes", buf.String())
	}
}

func TestIngestAgentAttachments_RejectsSymlink(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("hub-only"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dir := filepath.Join(sharedDir, ".attachments", "sender", "msg1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	refs := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1",
		[]string{"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/leak.txt"})
	if len(refs) != 0 {
		t.Fatalf("a symlink into the hub's filesystem must not be published: %+v", refs)
	}
}

// The end-to-end path an agent takes: POST an outbound message with attachment
// paths, and the persisted message carries them for the chat history endpoint.
func TestOutboundMessage_AttachmentsLinkedToMessage(t *testing.T) {
	srv, s, project, sharedDir := agentAttachmentServer(t)
	ctx := context.Background()

	recipient := &store.User{
		ID:          api.NewUUID(),
		Email:       "human@example.com",
		DisplayName: "Human",
	}
	if err := s.CreateUser(ctx, recipient); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "sender",
		Slug:       "sender",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	staged := stageAgentFile(t, sharedDir, "shot.png", "\x89PNG fake bytes")

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:   "user:human@example.com",
		Msg:         "here is the screenshot",
		Attachments: []string{staged},
		ThreadID:    "dm:" + recipient.ID + "+" + agent.ID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	attachments, err := srv.webChatStore.GetAttachmentsByMessage(ctx, resp.MessageID)
	if err != nil {
		t.Fatalf("GetAttachmentsByMessage: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected the message to carry 1 attachment, got %d", len(attachments))
	}
	if attachments[0].Filename != "shot.png" || attachments[0].MimeType != "image/png" {
		t.Errorf("unexpected attachment: %+v", attachments[0])
	}
}

func TestParseAttachmentRefs(t *testing.T) {
	refs := []AttachmentRef{{ID: "a1", Name: "shot.png", MimeType: "image/png", Size: 12}}
	encoded, ok := attachmentRefsMetadata(refs)
	if !ok {
		t.Fatal("expected refs to encode")
	}

	got := parseAttachmentRefs(map[string]string{attachmentsMetadataKey: encoded})
	if len(got) != 1 || got[0].ID != "a1" || got[0].Name != "shot.png" {
		t.Fatalf("round trip lost the refs: %+v", got)
	}

	if _, ok := attachmentRefsMetadata(nil); ok {
		t.Error("no refs should encode to nothing")
	}
	if got := parseAttachmentRefs(map[string]string{attachmentsMetadataKey: "not json"}); got != nil {
		t.Errorf("malformed metadata should yield no refs, got %+v", got)
	}
	if got := parseAttachmentRefs(nil); got != nil {
		t.Errorf("absent metadata should yield no refs, got %+v", got)
	}
}
