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

package transfer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClient_HTTPStatus(t *testing.T) {
	for _, operation := range []string{"upload", "download"} {
		for _, tt := range []struct {
			status  int
			wantErr bool
		}{
			{http.StatusOK, false},
			{http.StatusCreated, false},
			{http.StatusNoContent, false},
			{http.StatusMultipleChoices, true},
			{http.StatusNotModified, true},
			{http.StatusForbidden, true},
			{http.StatusInternalServerError, true},
		} {
			t.Run(operation+"/"+strconv.Itoa(tt.status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
					if tt.status != http.StatusNoContent && tt.status != http.StatusNotModified {
						_, _ = io.WriteString(w, "response body")
					}
				}))
				defer server.Close()
				client := NewClient(server.Client())

				var data []byte
				var err error
				if operation == "upload" {
					err = client.UploadFileWithMethod(t.Context(), server.URL, http.MethodPut, nil, strings.NewReader("file content"))
				} else {
					data, err = client.DownloadFile(t.Context(), server.URL)
				}
				if tt.wantErr {
					if err == nil || !strings.Contains(err.Error(), "status "+strconv.Itoa(tt.status)) {
						t.Fatalf("expected an error for HTTP %d, got %v", tt.status, err)
					}
					if data != nil {
						t.Errorf("returned response body as file content: %q", data)
					}
					return
				}
				if err != nil {
					t.Fatalf("transfer failed: %v", err)
				}
				if operation == "download" {
					want := "response body"
					if tt.status == http.StatusNoContent {
						want = ""
					}
					if string(data) != want {
						t.Errorf("downloaded %q, want %q", data, want)
					}
				}
			})
		}
	}
}

func TestClient_HTTPRedirectPolicy(t *testing.T) {
	for _, operation := range []string{"upload", "download"} {
		for _, policy := range []string{"follow", "decline"} {
			t.Run(operation+"/"+policy, func(t *testing.T) {
				var targetRequests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/start" {
						w.Header().Set("Location", "/target")
						w.WriteHeader(http.StatusTemporaryRedirect)
						_, _ = io.WriteString(w, "redirect body")
						return
					}
					targetRequests.Add(1)
					if operation == "upload" {
						body, err := io.ReadAll(r.Body)
						if err != nil || r.Method != http.MethodPut || string(body) != "file content" {
							t.Errorf("redirect lost upload: method=%s body=%q error=%v", r.Method, body, err)
						}
					}
					_, _ = io.WriteString(w, "file content")
				}))
				defer server.Close()
				httpClient := server.Client()
				if policy == "decline" {
					httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
						return http.ErrUseLastResponse
					}
				}
				client := NewClient(httpClient)

				var data []byte
				var err error
				if operation == "upload" {
					err = client.UploadFileWithMethod(t.Context(), server.URL+"/start", http.MethodPut, nil, strings.NewReader("file content"))
				} else {
					data, err = client.DownloadFile(t.Context(), server.URL+"/start")
				}
				if policy == "decline" {
					if err == nil || !strings.Contains(err.Error(), "status 307") {
						t.Errorf("expected an error for the unfollowed redirect, got %v", err)
					}
					if targetRequests.Load() != 0 {
						t.Error("followed a redirect against the client's policy")
					}
					return
				}
				if err != nil {
					t.Fatalf("transfer through redirect failed: %v", err)
				}
				if targetRequests.Load() != 1 {
					t.Error("transfer did not reach the redirect target exactly once")
				}
				if operation == "download" && string(data) != "file content" {
					t.Errorf("downloaded %q, want redirect target's content", data)
				}
			})
		}
	}
}

func TestClient_DownloadHTTPFailurePreservesFile(t *testing.T) {
	for _, operation := range []string{"single", "batch"} {
		for _, status := range []int{http.StatusMultipleChoices, http.StatusNotModified} {
			t.Run(operation+"/"+strconv.Itoa(status), func(t *testing.T) {
				dir := t.TempDir()
				dest := filepath.Join(dir, "existing.txt")
				if err := os.WriteFile(dest, []byte("original content"), 0644); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					if status != http.StatusNotModified {
						_, _ = io.WriteString(w, "not file content")
					}
				}))
				defer server.Close()
				client := NewClient(server.Client())

				var err error
				if operation == "single" {
					err = client.DownloadToFile(t.Context(), server.URL, dest)
				} else {
					err = client.DownloadFiles(t.Context(), []DownloadURLInfo{{Path: "existing.txt", URL: server.URL}}, dir, func(FileInfo, int64) error {
						t.Error("reported progress for a failed download")
						return nil
					})
				}
				if err == nil {
					t.Error("reported a failed download as successful")
				}
				data, err := os.ReadFile(dest)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "original content" {
					t.Errorf("failed download overwrote existing file with %q", data)
				}
			})
		}
	}
}

type transferTestBody struct {
	*strings.Reader
	closed bool
}

func (b *transferTestBody) Close() error {
	b.closed = true
	return nil
}

type transferTestRoundTripper func(*http.Request) (*http.Response, error)

func (rt transferTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt(req)
}

func transferResponseClient(status int, content string) (*Client, *transferTestBody) {
	body := &transferTestBody{Reader: strings.NewReader(content)}
	client := NewClient(&http.Client{Transport: transferTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return &http.Response{
			StatusCode:    status,
			ContentLength: int64(len(content)),
			Body:          body,
			Request:       req,
		}, nil
	})})
	return client, body
}

func TestClient_HTTPErrorBodyLimit(t *testing.T) {
	for _, operation := range []string{"upload", "download"} {
		for _, status := range []int{http.StatusMultipleChoices, http.StatusInternalServerError} {
			for _, size := range []string{"small", "large"} {
				t.Run(operation+"/"+strconv.Itoa(status)+"/"+size, func(t *testing.T) {
					content := "failure details"
					if size == "large" {
						content = strings.Repeat("x", 128<<10)
					}
					client, body := transferResponseClient(status, content)
					var err error
					if operation == "upload" {
						err = client.UploadFileWithMethod(t.Context(), "https://transfer.invalid/file", http.MethodPut, nil, strings.NewReader("file content"))
					} else {
						_, err = client.DownloadFile(t.Context(), "https://transfer.invalid/file")
					}
					if err == nil {
						t.Fatal("expected a transfer error")
					}
					// Bound actual body consumption, not just the returned string.
					wantExcerpt := content
					if len(wantExcerpt) > 1024 {
						wantExcerpt = wantExcerpt[:1024]
					}
					if read := len(content) - body.Len(); read != len(wantExcerpt) {
						t.Errorf("read %d error-body bytes, want %d", read, len(wantExcerpt))
					}
					want := operation + " failed with status " + strconv.Itoa(status) + ": " + wantExcerpt
					if err.Error() != want {
						t.Errorf("error diagnostic differs: got %d bytes, want %d", len(err.Error()), len(want))
					}
					if !body.closed {
						t.Error("response body was not closed")
					}
				})
			}
		}
	}
}

func TestClient_DownloadFile_LargeSuccessfulBody(t *testing.T) {
	content := strings.Repeat("x", 128<<10)
	client, body := transferResponseClient(http.StatusOK, content)
	data, err := client.DownloadFile(t.Context(), "https://transfer.invalid/file")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Errorf("successful download returned %d bytes, want %d", len(data), len(content))
	}
	if !body.closed {
		t.Error("response body was not closed")
	}
}
