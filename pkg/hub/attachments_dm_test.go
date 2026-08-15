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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A DM belongs to no space, so the composer has no project to send with an
// upload. These tests pin the project-less path end to end: the upload is
// accepted, the file is stored, and the recipient can fetch it back.

// attachmentTestServer wires a chat-capable server with attachment storage.
func attachmentTestServer(t *testing.T) (*Server, store.Store) {
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

	return srv, s
}

// uploadAttachment posts one file to the upload endpoint. The composer always
// sends project_id, empty string and all, so the helper does the same; pass
// nil to leave the field out altogether.
func uploadAttachment(t *testing.T, srv *Server, projectID *string, name, mime, content string) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if projectID != nil {
		if err := mw.WriteField("project_id", *projectID); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
	}
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="files"; filename="` + name + `"`}
	h["Content-Type"] = []string{mime}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAttachmentUpload_WithoutProjectID(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	empty := ""
	rec := uploadAttachment(t, srv, &empty, "notes.txt", "text/plain", "from a DM")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a project-less upload, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Attachments []attachmentUploadResult `json:"attachments"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(resp.Attachments))
	}

	// The stored metadata carries no project — the file is reachable only
	// through the messages that reference it.
	meta, err := srv.webChatStore.GetAttachment(context.Background(), resp.Attachments[0].ID)
	if err != nil || meta == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if meta.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty", meta.ProjectID)
	}
	if meta.UploadedBy != DevUserID {
		t.Errorf("UploadedBy = %q, want %q", meta.UploadedBy, DevUserID)
	}
}

func TestAttachmentDownload_WithoutProjectID(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	rec := uploadAttachment(t, srv, nil, "notes.txt", "text/plain", "from a DM")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Attachments []attachmentUploadResult `json:"attachments"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got := doRequest(t, srv, http.MethodGet, resp.Attachments[0].URL, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("download: expected 200, got %d: %s", got.Code, got.Body.String())
	}
	if got.Body.String() != "from a DM" {
		t.Errorf("body = %q, want %q", got.Body.String(), "from a DM")
	}
}

// A named project still has to exist: dropping the requirement must not turn
// into accepting any project ID a client cares to send.
func TestAttachmentUpload_UnknownProjectStillRejected(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	unknown := tid("no-such-project")
	rec := uploadAttachment(t, srv, &unknown, "notes.txt", "text/plain", "hi")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown project, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAttachmentUpload_WithProjectID(t *testing.T) {
	srv, s := attachmentTestServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("att-proj"), Name: "att-proj", Slug: "att-proj", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	rec := uploadAttachment(t, srv, &proj.ID, "notes.txt", "text/plain", "in a space")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Attachments []attachmentUploadResult `json:"attachments"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	meta, err := srv.webChatStore.GetAttachment(ctx, resp.Attachments[0].ID)
	if err != nil || meta == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if meta.ProjectID != proj.ID {
		t.Errorf("ProjectID = %q, want %q", meta.ProjectID, proj.ID)
	}
}

// ---------------------------------------------------------------------------
// Typing fan-out
// ---------------------------------------------------------------------------

// Typing in a DM must not reach the space's subject: the typist subscribes to
// their own spaces and would see their own indicator, and the rest of the
// space has no business hearing about a private conversation.
func TestConversationTyping_DMSkipsProjectSubject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("typing-proj"), Name: "typing-proj", Slug: "typing-proj", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agent := &store.Agent{
		ID:        tid("typing-agent"),
		ProjectID: proj.ID,
		Name:      "Helper",
		Slug:      "helper",
		Phase:     "running",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	events := &mockEventPublisher{}
	srv.events = events

	key := "dm:agent:" + agent.ID + ":user:" + DevUserID
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+key+"/typing", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, e := range events.getEvents() {
		if strings.HasPrefix(e.subject, "project.") {
			t.Errorf("DM typing published to %q; only the participants' user subjects should be used", e.subject)
		}
	}
}

// The peer of a human DM is reached on their user subject; the typist is not.
func TestConversationTyping_DMFansOutToPeer(t *testing.T) {
	srv, _ := testServer(t)

	events := &mockEventPublisher{}
	srv.events = events

	peerID := tid("typing-peer")
	key := "dm:user:" + DevUserID + ":user:" + peerID
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+key+"/typing", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	subjects := make([]string, 0, 2)
	for _, e := range events.getEvents() {
		subjects = append(subjects, e.subject)
	}
	want := "user." + peerID + ".chat.typing"
	found := false
	for _, subj := range subjects {
		if subj == want {
			found = true
		}
		if subj == "user."+DevUserID+".chat.typing" {
			t.Errorf("typing echoed back to the typist on %q", subj)
		}
	}
	if !found {
		t.Errorf("subjects = %v, want one of them to be %q", subjects, want)
	}
}
