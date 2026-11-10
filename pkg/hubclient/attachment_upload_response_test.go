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

package hubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

func TestUploadAttachments_ResponseContract(t *testing.T) {
	const stored = `{"attachments":[{"id":"att-1","name":"a.txt","size":5}]}`
	const refused = `{"failures":[{"name":"a.txt","error":"refused"}]}`
	wantStored := &AttachmentUploadResult{Attachments: []UploadedAttachment{{ID: "att-1", Name: "a.txt", Size: 5}}}
	wantRefused := &AttachmentUploadResult{Failures: []AttachmentUploadFailure{{Name: "a.txt", Error: "refused"}}}
	tests := []struct {
		name          string
		status        int
		body          string
		want          *AttachmentUploadResult // nil means the response must return an error
		wantNoContent bool
		wantAPIError  bool
	}{
		{name: "unexpected OK", status: 200, body: stored},
		{name: "unexpected accepted", status: 202, body: stored},
		{name: "no content", status: 204},
		{name: "reset content", status: 205},
		{name: "unknown success", status: 299, body: stored},
		{name: "empty OK", status: 200},
		{name: "malformed OK", status: 200, body: `{"attachments":`},
		{name: "unfollowed redirect", status: 307},
		{name: "created", status: 201, body: stored, want: wantStored},
		{name: "empty created", status: 201, wantNoContent: true},
		{name: "malformed created", status: 201, body: `{"attachments":`},
		{
			name: "partial failure", status: 201,
			body: `{"attachments":[{"id":"att-1","name":"a.txt","size":5}],"failures":[{"name":"b.txt","error":"refused"}]}`,
			want: &AttachmentUploadResult{Attachments: wantStored.Attachments, Failures: []AttachmentUploadFailure{{Name: "b.txt", Error: "refused"}}},
		},
		{name: "bad request with file failures", status: 400, body: refused, want: wantRefused},
		{name: "server error with file failures", status: 500, body: refused, want: wantRefused},
		{name: "OK with file failures", status: 200, body: refused, want: wantRefused},
		{name: "forbidden", status: 403, body: `{"error":{"code":"forbidden","message":"refused"}}`, wantAPIError: true},
		{name: "unavailable", status: 503, body: `{"error":{"code":"unavailable","message":"try later"}}`, wantAPIError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.txt")
			if err := os.WriteFile(path, []byte("alpha"), 0600); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != chatAttachmentsPath {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("read upload: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				defer func() { _ = r.MultipartForm.RemoveAll() }()
				files := r.MultipartForm.File["files"]
				if len(files) != 1 || files[0].Filename != "a.txt" {
					t.Errorf("unexpected uploaded files: %v", files)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				file, err := files[0].Open()
				if err != nil {
					t.Errorf("open uploaded file: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				data, err := io.ReadAll(file)
				_ = file.Close()
				if err != nil || string(data) != "alpha" {
					t.Errorf("uploaded content = %q, error = %v", data, err)
				}
				w.Header().Set("Content-Type", "application/json")
				if tt.status == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "/unexpected-redirect")
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			client, err := New(srv.URL, WithRetry(3, 0))
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.(AttachmentUploader).UploadAttachments(context.Background(), []string{path})
			if requests.Load() != 1 {
				t.Errorf("upload requests = %d, want exactly one", requests.Load())
			}
			if tt.want != nil {
				if err != nil || !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("UploadAttachments() = %+v, %v; want %+v, nil", got, err, tt.want)
				}
				return
			}
			if err == nil || got != nil {
				t.Fatalf("UploadAttachments() = %+v, %v; want nil, error", got, err)
			}
			if tt.wantNoContent && !errors.Is(err, apiclient.ErrNoContent) {
				t.Errorf("error = %v, want ErrNoContent", err)
			}
			if tt.status != http.StatusCreated && tt.status < 400 && !strings.Contains(err.Error(), strconv.Itoa(tt.status)) {
				t.Errorf("error %q does not identify status %d", err, tt.status)
			}
			if tt.wantAPIError {
				var apiErr *apiclient.APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.status {
					t.Errorf("error = %v, want APIError with status %d", err, tt.status)
				}
			}
		})
	}
}

func TestUploadAttachments_TruncatedResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTemporaryRedirect, http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.txt")
			if err := os.WriteFile(path, []byte("alpha"), 0600); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				const body = `{"failures":[{"name":"a.txt","error":"refused"}]}`
				w.Header().Set("Content-Type", "application/json")
				// Even a complete JSON value is not a complete HTTP response
				// when the connection closes before its advertised length.
				w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			client, err := New(srv.URL, WithRetry(3, 0))
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.(AttachmentUploader).UploadAttachments(context.Background(), []string{path})
			if got != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("UploadAttachments() = %+v, %v; want nil, error wrapping unexpected EOF", got, err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("error %q does not identify status %d", err, status)
			}
			if requests.Load() != 1 {
				t.Errorf("upload requests = %d, want exactly one", requests.Load())
			}
		})
	}
}
