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

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// multipartTestPart is one part of a multipart test body. An empty filename
// makes it a plain form field.
type multipartTestPart struct {
	field, filename string
	content         []byte
}

// buildMultipartTestRequest builds a multipart request for target with the
// given parts in order. token, if set, is sent as a bearer token.
func buildMultipartTestRequest(t *testing.T, method, target, token string, parts ...multipartTestPart) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var (
			w   io.Writer
			err error
		)
		if p.filename == "" {
			w, err = mw.CreateFormField(p.field)
		} else {
			w, err = mw.CreateFormFile(p.field, p.filename)
		}
		if err != nil {
			t.Fatalf("create part %q: %v", p.field, err)
		}
		if _, err := w.Write(p.content); err != nil {
			t.Fatalf("write part %q: %v", p.field, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(method, target, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// redirectMultipartTemp points TMPDIR at a fresh empty directory, so that
// multipart parsing spools its temporary files there, and returns it. Call it
// after the test server is built, right before the request.
func redirectMultipartTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

// multipartSpoolFiles lists the multipart temporary files left in dir.
func multipartSpoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "multipart-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// assertNoMultipartSpool fails the test when a multipart temporary file is
// left in dir.
func assertNoMultipartSpool(t *testing.T, dir string) {
	t.Helper()
	if left := multipartSpoolFiles(t, dir); len(left) != 0 {
		t.Errorf("multipart temporary files left after the request: %v", left)
	}
}

// TestParseMultipartUpload_ParseErrorStillCleansUp checks that a parse that
// spools a part and then fails on the query string returns a cleanup that
// removes the spooled file.
func TestParseMultipartUpload_ParseErrorStillCleansUp(t *testing.T) {
	dir := redirectMultipartTemp(t)
	req := buildMultipartTestRequest(t, http.MethodPost, "/upload?x=%zz", "",
		multipartTestPart{field: "f", filename: "f.bin", content: bytes.Repeat([]byte("a"), 4096)})

	cleanup, err := parseMultipartUpload(req, 1024)
	if cleanup == nil {
		t.Fatal("cleanup is nil")
	}
	if err == nil {
		t.Fatal("expected a parse error from the malformed query")
	}
	if req.MultipartForm == nil {
		t.Fatal("expected the form to be parsed despite the query error")
	}
	if got := len(multipartSpoolFiles(t, dir)); got != 1 {
		t.Fatalf("expected 1 spooled file before cleanup, got %d", got)
	}
	cleanup()
	assertNoMultipartSpool(t, dir)
}

// TestParseMultipartUpload_FormReadableUntilCleanup checks that the spooled
// part stays readable after the helper returns and is removed by cleanup.
func TestParseMultipartUpload_FormReadableUntilCleanup(t *testing.T) {
	dir := redirectMultipartTemp(t)
	content := bytes.Repeat([]byte("b"), 4096)
	req := buildMultipartTestRequest(t, http.MethodPost, "/upload", "",
		multipartTestPart{field: "f", filename: "f.bin", content: content})

	cleanup, err := parseMultipartUpload(req, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := readMultipartFile(req.MultipartForm.File["f"][0])
	if err != nil {
		t.Fatalf("read spooled part after the helper returned: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("spooled part content mismatch: got %d bytes", len(got))
	}
	if n := len(multipartSpoolFiles(t, dir)); n != 1 {
		t.Fatalf("expected 1 spooled file before cleanup, got %d", n)
	}
	cleanup()
	assertNoMultipartSpool(t, dir)
}
