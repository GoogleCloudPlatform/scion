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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Each upload handler that parses a multipart form gets two tests: a
// successful upload and a malformed query string (x=%zz), which makes
// ParseMultipartForm fail after it has spooled the part. Both send a part over
// the in-memory limit, so it spools, and check no temporary file remains. The
// bulk invite route is the exception: its body cap equals that limit.

// malformedUploadQuery is a query string that url.ParseQuery refuses.
const malformedUploadQuery = "?x=%zz"

// lowUploadFormMemory replaces maxUploadTotalSize as the in-memory limit on
// the template, harness-config and project workspace upload handlers, whose
// 100 MiB body cap otherwise keeps every part in memory.
const lowUploadFormMemory = 1024

// spoolingUploadContent is larger than lowUploadFormMemory.
var spoolingUploadContent = bytes.Repeat([]byte("spooled upload content\n"), 512)

// adminSpoolingCSV returns a CSV larger than the admin import handlers'
// 2 MiB in-memory limit. It holds the given addresses, padded with lines
// that are not addresses and are skipped.
func adminSpoolingCSV(emails ...string) []byte {
	var b bytes.Buffer
	b.WriteString("email,note\n")
	for _, e := range emails {
		b.WriteString(e + ",spool\n")
	}
	pad := "padding-" + strings.Repeat("x", 1000) + "\n"
	for b.Len() <= 2<<20+64<<10 {
		b.WriteString(pad)
	}
	return b.Bytes()
}

func adminCSVUploadRequest(t *testing.T, target string, csv []byte) *http.Request {
	t.Helper()
	return buildMultipartTestRequest(t, http.MethodPost, target, testDevToken,
		multipartTestPart{field: "file", filename: "emails.csv", content: csv})
}

func serveUpload(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func requireUploadStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("expected status %d, got %d: %s", want, rec.Code, rec.Body.String())
	}
}

// --- Admin allow-list import (2 MiB) ---

func TestAdminAllowListImportUpload_SpoolRemoved_Success(t *testing.T) {
	srv, s := testServer(t)
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, adminCSVUploadRequest(t, "/api/v1/admin/allow-list/import",
		adminSpoolingCSV("spool-allow@example.com")))
	requireUploadStatus(t, rec, http.StatusOK)

	user, err := s.GetUserByEmail(context.Background(), "spool-allow@example.com")
	if err != nil {
		t.Fatalf("expected the imported user to exist: %v", err)
	}
	if user.Status != store.UserStatusInvited {
		t.Errorf("expected status invited, got %q", user.Status)
	}
	assertNoMultipartSpool(t, dir)
}

func TestAdminAllowListImportUpload_SpoolRemoved_MalformedQuery(t *testing.T) {
	srv, _ := testServer(t)
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, adminCSVUploadRequest(t, "/api/v1/admin/allow-list/import"+malformedUploadQuery,
		adminSpoolingCSV("spool-allow-bad@example.com")))
	requireUploadStatus(t, rec, http.StatusBadRequest)
	assertNoMultipartSpool(t, dir)
}

// --- Admin bulk user invite (2 MiB) ---

// This route's body cap equals the in-memory limit (both
// inviteBulkMaxMultipartBytes), so no part can spool here: this CSV is kept
// under the cap, and TestAdminUserInviteUpload_SpoolRemoved_MalformedQuery is
// rejected by the cap (400) before any part spools; both still check that no
// temporary file remains. Spooled-part removal is covered by
// TestAdminAllowListImportUpload_SpoolRemoved_Success (same 2 MiB call, other
// route), TestParseMultipartUpload_FormReadableUntilCleanup and
// TestParseMultipartUpload_ParseErrorStillCleansUp (the helper), and
// TestMultipartFormSourcePin (decodeInviteBatchMultipart defers the cleanup).
func TestAdminUserInviteUpload_SpoolRemoved_Success(t *testing.T) {
	srv, s := testServer(t)
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, adminCSVUploadRequest(t, "/api/v1/admin/users/invite/bulk",
		[]byte("email,note\nspool-invite@example.com,spool\n")))
	requireUploadStatus(t, rec, http.StatusOK)

	var resp UserInviteBulkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Invited != 1 {
		t.Errorf("expected 1 invited, got %d", resp.Invited)
	}
	if _, err := s.GetUserByEmail(context.Background(), "spool-invite@example.com"); err != nil {
		t.Errorf("expected the invited user to exist: %v", err)
	}
	assertNoMultipartSpool(t, dir)
}

func TestAdminUserInviteUpload_SpoolRemoved_MalformedQuery(t *testing.T) {
	srv, _ := testServer(t)
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, adminCSVUploadRequest(t, "/api/v1/admin/users/invite/bulk"+malformedUploadQuery,
		adminSpoolingCSV("spool-invite-bad@example.com")))
	requireUploadStatus(t, rec, http.StatusBadRequest)
	assertNoMultipartSpool(t, dir)
}

// --- Skill version multipart publish (10 MiB) ---

func skillPublishUploadRequest(t *testing.T, srv *Server, user *store.User, target string, files ...multipartTestPart) *http.Request {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	parts := append([]multipartTestPart{{field: "version", content: []byte("1.0.0")}}, files...)
	return buildMultipartTestRequest(t, http.MethodPost, target, token, parts...)
}

func TestSkillPublishUpload_SpoolRemoved_Success(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)
	dir := redirectMultipartTemp(t)

	// Two 6 MiB files: together they pass the 10 MiB in-memory limit, so the
	// second is spooled; each stays within the 10 MiB per-file limit.
	skillMD := []byte("---\nname: spool\n---\n# Spool")
	sizes := map[string]int64{"SKILL.md": int64(len(skillMD)), "a.txt": 6 << 20, "b.txt": 6 << 20}
	rec := serveUpload(srv, skillPublishUploadRequest(t, srv, alice, "/api/v1/skills/"+skill.ID+"/versions",
		multipartTestPart{field: "file", filename: "SKILL.md", content: skillMD},
		multipartTestPart{field: "file", filename: "a.txt", content: bytes.Repeat([]byte("a"), 6<<20)},
		multipartTestPart{field: "file", filename: "b.txt", content: bytes.Repeat([]byte("b"), 6<<20)},
	))
	requireUploadStatus(t, rec, http.StatusCreated)

	var resp PublishVersionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Version.Files) != len(sizes) {
		t.Fatalf("expected %d files, got %d", len(sizes), len(resp.Version.Files))
	}
	for _, f := range resp.Version.Files {
		if f.Size != sizes[f.Path] {
			t.Errorf("file %s: expected size %d, got %d", f.Path, sizes[f.Path], f.Size)
		}
	}
	assertNoMultipartSpool(t, dir)
}

func TestSkillPublishUpload_SpoolRemoved_MalformedQuery(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, skillPublishUploadRequest(t, srv, alice, "/api/v1/skills/"+skill.ID+"/versions"+malformedUploadQuery,
		multipartTestPart{field: "file", filename: "SKILL.md", content: bytes.Repeat([]byte("s"), 11<<20)},
	))
	requireUploadStatus(t, rec, http.StatusBadRequest)
	assertNoMultipartSpool(t, dir)
}

// --- Template file upload (100 MiB, lowered) ---

func TestTemplateFileUpload_SpoolRemoved_Success(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	srv.uploadFormMemoryForTest = lowUploadFormMemory
	tmpl := createTestTemplate(t, s, stor, map[string]string{"CLAUDE.md": "# Agent"})
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, buildMultipartTestRequest(t, http.MethodPost,
		"/api/v1/templates/"+tmpl.ID+"/files", testDevToken,
		multipartTestPart{field: "notes.txt", filename: "notes.txt", content: spoolingUploadContent}))
	requireUploadStatus(t, rec, http.StatusOK)

	updated, err := s.GetTemplate(context.Background(), tmpl.ID)
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if got := stor.content[blobObjectPath(updated, string(spoolingUploadContent))]; !bytes.Equal(got, spoolingUploadContent) {
		t.Errorf("stored content mismatch: got %d bytes, want %d", len(got), len(spoolingUploadContent))
	}
	assertNoMultipartSpool(t, dir)
}

func TestTemplateFileUpload_SpoolRemoved_MalformedQuery(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	srv.uploadFormMemoryForTest = lowUploadFormMemory
	tmpl := createTestTemplate(t, s, stor, map[string]string{"CLAUDE.md": "# Agent"})
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, buildMultipartTestRequest(t, http.MethodPost,
		"/api/v1/templates/"+tmpl.ID+"/files"+malformedUploadQuery, testDevToken,
		multipartTestPart{field: "notes.txt", filename: "notes.txt", content: spoolingUploadContent}))
	requireUploadStatus(t, rec, http.StatusBadRequest)
	assertNoMultipartSpool(t, dir)
}

// --- Harness-config file upload (100 MiB, lowered) ---

// spoolingHarnessConfigYAML is a config.yaml larger than lowUploadFormMemory
// that sets a model alias the test can check.
var spoolingHarnessConfigYAML = []byte("harness: codex\nmodel_aliases:\n  large: spooled-large-model\n" +
	strings.Repeat("# padding line for a part larger than the in-memory limit\n", 64))

func TestHarnessConfigFileUpload_SpoolRemoved_Success(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	srv.uploadFormMemoryForTest = lowUploadFormMemory
	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, buildMultipartTestRequest(t, http.MethodPost,
		"/api/v1/harness-configs/"+hc.ID+"/files", testDevToken,
		multipartTestPart{field: "config.yaml", filename: "config.yaml", content: spoolingHarnessConfigYAML}))
	requireUploadStatus(t, rec, http.StatusOK)

	updated, err := s.GetHarnessConfig(context.Background(), hc.ID)
	if err != nil {
		t.Fatalf("get harness config: %v", err)
	}
	if updated.Config == nil || updated.Config.ModelAliases["large"] != "spooled-large-model" {
		t.Errorf("expected Config.ModelAliases[large] = %q, got %+v", "spooled-large-model", updated.Config)
	}
	assertNoMultipartSpool(t, dir)
}

func TestHarnessConfigFileUpload_SpoolRemoved_MalformedQuery(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	srv.uploadFormMemoryForTest = lowUploadFormMemory
	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, buildMultipartTestRequest(t, http.MethodPost,
		"/api/v1/harness-configs/"+hc.ID+"/files"+malformedUploadQuery, testDevToken,
		multipartTestPart{field: "config.yaml", filename: "config.yaml", content: spoolingHarnessConfigYAML}))
	requireUploadStatus(t, rec, http.StatusBadRequest)
	assertNoMultipartSpool(t, dir)
}

// --- Project workspace file upload (100 MiB, lowered) ---

func TestProjectWorkspaceUpload_SpoolRemoved_Success(t *testing.T) {
	srv, _ := testServer(t)
	srv.uploadFormMemoryForTest = lowUploadFormMemory
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Upload Spool")
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, buildMultipartTestRequest(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), testDevToken,
		multipartTestPart{field: "notes.txt", filename: "notes.txt", content: spoolingUploadContent}))
	requireUploadStatus(t, rec, http.StatusOK)

	got, err := os.ReadFile(filepath.Join(workspacePath, "notes.txt"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if !bytes.Equal(got, spoolingUploadContent) {
		t.Errorf("uploaded content mismatch: got %d bytes, want %d", len(got), len(spoolingUploadContent))
	}
	assertNoMultipartSpool(t, dir)
}

func TestProjectWorkspaceUpload_SpoolRemoved_MalformedQuery(t *testing.T) {
	srv, _ := testServer(t)
	srv.uploadFormMemoryForTest = lowUploadFormMemory
	project, _ := createTestHubManagedProject(t, srv, "WS Upload Spool Bad")
	dir := redirectMultipartTemp(t)

	rec := serveUpload(srv, buildMultipartTestRequest(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files%s", project.ID, malformedUploadQuery), testDevToken,
		multipartTestPart{field: "notes.txt", filename: "notes.txt", content: spoolingUploadContent}))
	requireUploadStatus(t, rec, http.StatusBadRequest)
	assertNoMultipartSpool(t, dir)
}
