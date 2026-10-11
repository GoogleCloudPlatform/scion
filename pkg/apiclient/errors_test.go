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
	"net/http"
	"net/http/httptest"
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
				Details: map[string]interface{}{"reason": "branch_not_parent_child", "senderMode": "branch", "recipientMode": "branch"}},
			want: "message_denied: Message delivery denied [reason: branch_not_parent_child, sender mode: branch, recipient mode: branch] (status: 403)",
		},
		{
			name: "message_denied with request ID and partial details",
			err: &APIError{StatusCode: 403, Code: ErrCodeMessageDenied, Message: "Message delivery denied", RequestID: "req-1",
				Details: map[string]interface{}{"reason": "target_mode_none", "senderMode": ""}},
			want: "message_denied: Message delivery denied [reason: target_mode_none] (status: 403, request: req-1)",
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
		`"details":{"reason":"sender_mode_none","senderMode":"none","recipientMode":"project"}}}`)

	apiErr := ParseErrorResponse(rec.Result())
	want := "message_denied: Message delivery denied [reason: sender_mode_none, sender mode: none, recipient mode: project] (status: 403)"
	if got := apiErr.Error(); got != want {
		t.Errorf("Error() = %q\nwant      %q", got, want)
	}
}
