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

package apiclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIError_MessageDeniedReason checks that a message_denied error
// shows the server's reason and modes, and that other errors are
// unchanged (ptone/scion#4411).
func TestAPIError_MessageDeniedReason(t *testing.T) {
	tests := []struct {
		name string
		err  *APIError
		want string
	}{
		{
			name: "message_denied with full details",
			err: &APIError{StatusCode: 403, Code: ErrCodeMessageDenied, Message: "Message delivery denied",
				Details: map[string]interface{}{"reason": "mode_branch_no_edge", "senderMode": "branch", "recipientMode": "branch"}},
			want: "message_denied: Message delivery denied [reason: mode_branch_no_edge, sender mode: branch, recipient mode: branch] (status: 403)",
		},
		{
			name: "message_denied with request ID and partial details",
			err: &APIError{StatusCode: 403, Code: ErrCodeMessageDenied, Message: "Message delivery denied", RequestID: "req-1",
				Details: map[string]interface{}{"reason": "mode_none", "senderMode": ""}},
			want: "message_denied: Message delivery denied [reason: mode_none] (status: 403, request: req-1)",
		},
		{
			name: "message_denied without details",
			err:  &APIError{StatusCode: 403, Code: ErrCodeMessageDenied, Message: "Message delivery denied"},
			want: "message_denied: Message delivery denied (status: 403)",
		},
		{
			name: "message_denied with non-string details",
			err: &APIError{StatusCode: 403, Code: ErrCodeMessageDenied, Message: "Message delivery denied",
				Details: map[string]interface{}{"reason": 7}},
			want: "message_denied: Message delivery denied (status: 403)",
		},
		{
			name: "other code keeps its details out of the message",
			err: &APIError{StatusCode: 403, Code: "forbidden", Message: "Access denied",
				Details: map[string]interface{}{"reason": "nope"}},
			want: "forbidden: Access denied (status: 403)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q\nwant      %q", got, tt.want)
			}
		})
	}
}

// TestParseErrorResponse_MessageDeniedReason checks the full path from the
// Hub's 403 body to the error string the CLI prints.
func TestParseErrorResponse_MessageDeniedReason(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(http.StatusForbidden)
	_, _ = rec.WriteString(`{"error":{"code":"message_denied","message":"Message delivery denied",` +
		`"details":{"reason":"mode_none_sender","senderMode":"none","recipientMode":"project"}}}`)

	apiErr := ParseErrorResponse(rec.Result())
	want := "message_denied: Message delivery denied [reason: mode_none_sender, sender mode: none, recipient mode: project] (status: 403)"
	if got := apiErr.Error(); got != want {
		t.Errorf("Error() = %q\nwant      %q", got, want)
	}
}

func TestParseErrorResponse_NilResponse(t *testing.T) {
	got := ParseErrorResponse(nil)
	if got == nil {
		t.Fatal("ParseErrorResponse(nil) = nil, want non-nil *APIError")
	}
	if got.Code != ErrCodeInternalError {
		t.Errorf("Code = %q, want %q", got.Code, ErrCodeInternalError)
	}
	if got.Message != "nil response" {
		t.Errorf("Message = %q, want %q", got.Message, "nil response")
	}
	var err error = got
	if err.Error() == "" {
		t.Error("err.Error() is empty")
	}
}

func TestParseErrorResponse_NilBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
	}
	got := ParseErrorResponse(resp)
	if got == nil {
		t.Fatal("ParseErrorResponse returned nil for 500 response")
	}
	if got.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}
	if got.Code != ErrCodeInternalError {
		t.Errorf("Code = %q, want %q", got.Code, ErrCodeInternalError)
	}
}

func TestParseErrorResponse_NilHeader(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"not_found","message":"item missing"}}`)),
		Header:     nil,
	}
	got := ParseErrorResponse(resp)
	if got == nil {
		t.Fatal("ParseErrorResponse returned nil")
	}
	if got.Code != ErrCodeNotFound || got.Message != "item missing" {
		t.Errorf("got Code=%q Message=%q, want not_found / item missing", got.Code, got.Message)
	}
}

type chunkReader struct {
	totalRead int
	limit     int
}

func (r *chunkReader) Read(p []byte) (n int, err error) {
	if r.totalRead >= r.limit {
		return 0, io.EOF
	}
	toRead := len(p)
	if r.totalRead+toRead > r.limit {
		toRead = r.limit - r.totalRead
	}
	for i := 0; i < toRead; i++ {
		p[i] = 'a'
	}
	r.totalRead += toRead
	return toRead, nil
}

func (r *chunkReader) Close() error { return nil }

func TestParseErrorResponse_BoundedBodyRead(t *testing.T) {
	reader := &chunkReader{limit: 256 * 1024}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       reader,
		Header:     http.Header{},
	}
	_ = ParseErrorResponse(resp)
	if reader.totalRead > maxErrorResponseBodyBytes {
		t.Errorf("read %d bytes, want <= %d", reader.totalRead, maxErrorResponseBodyBytes)
	}
}
