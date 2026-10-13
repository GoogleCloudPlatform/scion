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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errMappingCase is one error writeErrorFromErr maps to a fixed response.
// The expected status, code and message are literals, not the package
// constants, so the table pins the wire response independently of where
// those constants are defined.
type errMappingCase struct {
	name   string
	err    error
	status int
	code   string
	// message returns the expected message for an error that matched this
	// case. Most cases have a fixed message; the PermissionError and
	// ErrNoSecretBackend cases derive it from the error.
	message func(err error) string
}

func fixedMessage(msg string) func(error) string {
	return func(error) string { return msg }
}

// errMappingPermErr is the PermissionError used by the table.
var errMappingPermErr = &secret.PermissionError{
	Operation: "create secret",
	Err:       errors.New("caller does not have permission"),
}

// errMappingCases lists, in precedence order (first match wins), every
// error writeErrorFromErr maps to a non-500 response.
func errMappingCases() []errMappingCase {
	return []errMappingCase{
		{
			name: "secret.PermissionError", err: errMappingPermErr,
			status: 403, code: "forbidden",
			// The message is the PermissionError's own text, never the text
			// of an error that wraps it.
			message: func(error) string { return errMappingPermErr.Error() },
		},
		{name: "store.ErrNotFound", err: store.ErrNotFound, status: 404, code: "not_found", message: fixedMessage("Resource not found")},
		{name: "store.ErrAlreadyExists", err: store.ErrAlreadyExists, status: 409, code: "conflict", message: fixedMessage("Resource already exists")},
		{name: "store.ErrDeleteInProgress", err: store.ErrDeleteInProgress, status: 409, code: "delete_in_progress", message: fixedMessage("a delete is in progress for this agent; wait for it to finish, or force the delete")},
		{name: "store.ErrVersionConflict", err: store.ErrVersionConflict, status: 409, code: "version_conflict", message: fixedMessage("Version conflict - resource was modified")},
		{name: "store.ErrProjectMembersGroupPrincipal", err: store.ErrProjectMembersGroupPrincipal, status: 400, code: "invalid_request", message: fixedMessage("Project members groups cannot be role-binding principals or child groups")},
		{name: "store.ErrInvalidInput", err: store.ErrInvalidInput, status: 400, code: "validation_error", message: fixedMessage("Invalid input")},
		{name: "store.ErrScopeMismatch", err: store.ErrScopeMismatch, status: 400, code: "scope_mismatch", message: fixedMessage("Binding scope type does not match role definition scope type")},
		{name: "store.ErrDirectUserOnly", err: store.ErrDirectUserOnly, status: 400, code: "validation_error", message: fixedMessage("This role requires a direct user principal")},
		{name: "store.ErrBuiltInMembershipConflict", err: store.ErrBuiltInMembershipConflict, status: 409, code: "conflict", message: fixedMessage("Principal already has a built-in membership role in this project")},
		{name: "store.ErrIdentityKeyConflict", err: store.ErrIdentityKeyConflict, status: 409, code: "conflict", message: fixedMessage("display name collides with another agent in this project")},
		{
			name: "secret.ErrNoSecretBackend", err: secret.ErrNoSecretBackend,
			status: 501, code: "unavailable",
			// The message is the full text of the error passed in,
			// including any wrapping.
			message: func(err error) string { return err.Error() },
		},
		{name: "errInvalidCursor", err: errInvalidCursor, status: 400, code: "invalid_cursor", message: fixedMessage("invalid cursor: restart pagination from the first page")},
	}
}

// assertErrMapping calls writeErrorFromErr and asserts the exact status,
// headers and body.
func assertErrMapping(t *testing.T, err error, requestID string, status int, code, message string) {
	t.Helper()
	rr := httptest.NewRecorder()
	writeErrorFromErr(rr, err, requestID)

	if rr.Code != status {
		t.Errorf("status = %d, want %d", rr.Code, status)
	}
	wantHeader := http.Header{"Content-Type": []string{"application/json"}}
	if !reflect.DeepEqual(rr.Header(), wantHeader) {
		t.Errorf("headers = %v, want %v", rr.Header(), wantHeader)
	}

	// An independent mirror of the wire shape, so the expected body does
	// not depend on ErrorResponse.
	type wireErr struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId,omitempty"`
	}
	want, mErr := json.Marshal(struct {
		Error wireErr `json:"error"`
	}{wireErr{Code: code, Message: message, RequestID: requestID}})
	if mErr != nil {
		t.Fatalf("marshal expected body: %v", mErr)
	}
	if got := rr.Body.String(); got != string(want)+"\n" {
		t.Errorf("body = %q, want %q", got, string(want)+"\n")
	}
}

// TestWriteErrorFromErr_Mapping pins the response writeErrorFromErr writes
// for every error it maps: status, code, message, headers and body, for the
// error itself and wrapped, with and without a request ID. It also pins the
// precedence between cases (first match in errMappingCases wins) by joining
// every pair of mapped errors, and the 500 fallback.
func TestWriteErrorFromErr_Mapping(t *testing.T) {
	cases := errMappingCases()
	for _, tc := range cases {
		variants := []struct {
			name string
			err  error
		}{
			{"bare", tc.err},
			{"wrapped", fmt.Errorf("outer context: %w", tc.err)},
			{"double-wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", tc.err))},
			{"joined-with-unmapped", errors.Join(errors.New("unmapped"), tc.err)},
		}
		for _, v := range variants {
			for _, reqID := range []string{"req-1", ""} {
				t.Run(fmt.Sprintf("%s/%s/reqID=%q", tc.name, v.name, reqID), func(t *testing.T) {
					assertErrMapping(t, v.err, reqID, tc.status, tc.code, tc.message(v.err))
				})
			}
		}
	}

	// Precedence: for every pair (hi before lo in errMappingCases), an error
	// matching both maps as hi, whichever order the join lists them in.
	for i, hi := range cases {
		for _, lo := range cases[i+1:] {
			for _, joined := range []error{errors.Join(lo.err, hi.err), errors.Join(hi.err, lo.err)} {
				t.Run(fmt.Sprintf("precedence/%s>%s/%q", hi.name, lo.name, joined.Error()), func(t *testing.T) {
					assertErrMapping(t, joined, "req-p", hi.status, hi.code, hi.message(joined))
				})
			}
		}
	}

	// store.ErrProjectMembersGroupPrincipal wraps store.ErrInvalidInput; it
	// must keep its own mapping, not ErrInvalidInput's.
	if !errors.Is(store.ErrProjectMembersGroupPrincipal, store.ErrInvalidInput) {
		t.Fatal("store.ErrProjectMembersGroupPrincipal no longer wraps store.ErrInvalidInput; revisit the precedence note")
	}

	// Fallback: anything unmapped, including nil and the hub error
	// types that carry their own status, is a 500 with a fixed message.
	fallbacks := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain", errors.New("something went wrong")},
		{"RuntimeTargetRefusal", &RuntimeTargetRefusal{Code: "x", Status: 409, Message: "m"}},
		{"brokerStatusError", &brokerStatusError{StatusCode: 503, Body: `{"error":{"code":"runtime_unavailable"}}`, RetryAfter: "5"}},
	}
	for _, f := range fallbacks {
		t.Run("fallback/"+f.name, func(t *testing.T) {
			assertErrMapping(t, f.err, "req-f", 500, "internal_error", "Internal server error")
		})
	}
}
