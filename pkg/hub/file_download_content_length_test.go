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
	"net/http"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// rawDownloadContentLengthCase describes how the stored object diverges from
// the database record, which always records the original file size.
type rawDownloadContentLengthCase struct {
	name string
	// stored is the content actually in storage at download time.
	stored string
	// obj returns the object metadata Download reports for the stored
	// content (nil means Download returns no metadata).
	obj func(objectPath string, stored []byte) *storage.Object
	// wantContentLength is the expected header; "" means unset.
	wantContentLength string
}

const rawDownloadOriginal = "original content\n" // 17 bytes, the record's size

func rawDownloadContentLengthCases() []rawDownloadContentLengthCase {
	exact := func(p string, b []byte) *storage.Object {
		return &storage.Object{Name: p, Size: int64(len(b))}
	}
	return []rawDownloadContentLengthCase{
		{
			name:              "object larger than record",
			stored:            "replaced with considerably longer content\n",
			obj:               exact,
			wantContentLength: strconv.Itoa(len("replaced with considerably longer content\n")),
		},
		{
			name:              "object smaller than record",
			stored:            "short\n",
			obj:               exact,
			wantContentLength: strconv.Itoa(len("short\n")),
		},
		{
			name:   "object size unknown (negative)",
			stored: "short\n",
			obj: func(p string, _ []byte) *storage.Object {
				return &storage.Object{Name: p, Size: -1}
			},
			wantContentLength: "",
		},
		{
			name:   "object size unknown (zero)",
			stored: "short\n",
			obj: func(p string, _ []byte) *storage.Object {
				return &storage.Object{Name: p}
			},
			wantContentLength: "",
		},
		{
			name:              "object metadata missing",
			stored:            "short\n",
			obj:               func(string, []byte) *storage.Object { return nil },
			wantContentLength: "",
		},
	}
}

// checkRawDownloadContentLength asserts the declared length (when set) equals
// the bytes sent, and that the record's stale size never leaks into it.
func checkRawDownloadContentLength(t *testing.T, srv *Server, url string, tc rawDownloadContentLengthCase) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, url, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw read: status %d: %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	if got := rec.Body.String(); got != tc.stored {
		t.Errorf("body = %q, want stored content %q", got, tc.stored)
	}
	got := rec.Header().Get("Content-Length")
	if got != tc.wantContentLength {
		t.Errorf("Content-Length = %q, want %q", got, tc.wantContentLength)
	}
	if got != "" && got != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length %q does not match the %d body bytes sent", got, rec.Body.Len())
	}
	if got == strconv.Itoa(len(rawDownloadOriginal)) {
		t.Errorf("Content-Length %q came from the stale record size", got)
	}
}

func TestTemplateFileRead_RawContentLengthFromStorageObject(t *testing.T) {
	for _, tc := range rawDownloadContentLengthCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			tmpl := createTestTemplate(t, s, stor, map[string]string{"f.txt": rawDownloadOriginal})

			objectPath := tmpl.StoragePath + "/f.txt"
			stor.content[objectPath] = []byte(tc.stored)
			stor.objects[objectPath] = tc.obj(objectPath, stor.content[objectPath])

			checkRawDownloadContentLength(t, srv, "/api/v1/templates/"+tmpl.ID+"/files/f.txt?raw=1", tc)
		})
	}
}

func TestHarnessConfigFileRead_RawContentLengthFromStorageObject(t *testing.T) {
	for _, tc := range rawDownloadContentLengthCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, stor := testHarnessConfigFileServer(t)
			hc := createTestHarnessConfigWithFiles(t, s, stor, map[string]string{"f.txt": rawDownloadOriginal})

			objectPath := hc.StoragePath + "/f.txt"
			stor.content[objectPath] = []byte(tc.stored)
			stor.objects[objectPath] = tc.obj(objectPath, stor.content[objectPath])

			checkRawDownloadContentLength(t, srv, "/api/v1/harness-configs/"+hc.ID+"/files/f.txt?raw=1", tc)
		})
	}
}
