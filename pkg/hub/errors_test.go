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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errorsHelperCaptureLogs redirects the default slog logger into a buffer for
// the duration of the test and returns it. Mirrors authzHelperCaptureLogs in
// authorize_test.go.
func errorsHelperCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// errorsHelperAPIRecords returns every JSON log record in buf whose msg
// starts with "API ", across all levels. Filtering by prefix, rather than
// taking the last line in the buffer, avoids picking up an unrelated log
// line that a background goroutine left over from an earlier test could
// still write into the process-global logger while this test runs.
func errorsHelperAPIRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		if strings.HasPrefix(msg, "API ") {
			recs = append(recs, rec)
		}
	}
	return recs
}

// errorsHelperRecordAtLevel returns the first record from
// errorsHelperAPIRecords at the given level, or nil if there is none.
func errorsHelperRecordAtLevel(t *testing.T, buf *bytes.Buffer, level string) map[string]any {
	t.Helper()
	for _, rec := range errorsHelperAPIRecords(t, buf) {
		if rec["level"] == level {
			return rec
		}
	}
	return nil
}

func TestWriteErrorFromErr_PermissionError(t *testing.T) {
	// Simulate a PermissionDenied error from GCP Secret Manager
	grpcErr := status.Errorf(codes.PermissionDenied, "caller does not have permission")
	permErr := &secret.PermissionError{
		Operation: "create secret",
		Err:       grpcErr,
	}
	// Wrap it like gcpbackend.go would
	wrappedErr := fmt.Errorf("failed to create GCP SM secret: %w", permErr)

	rr := httptest.NewRecorder()
	writeErrorFromErr(rr, wrappedErr, "test-req-1")

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected status %d, got %d", http.StatusForbidden, rr.Code)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error.Code != ErrCodeForbidden {
		t.Errorf("expected error code %q, got %q", ErrCodeForbidden, resp.Error.Code)
	}
	if resp.Error.Message == "" {
		t.Error("expected non-empty error message")
	}
	// The message should contain actionable guidance
	if got := resp.Error.Message; got == "Internal server error" {
		t.Errorf("error message should not be generic 500, got: %q", got)
	}
}

func TestMethodNotAllowed_WithAllowedMethods(t *testing.T) {
	rr := httptest.NewRecorder()
	MethodNotAllowed(rr, http.MethodGet, http.MethodPost)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}

	allow := rr.Header().Get("Allow")
	if allow != "GET, POST" {
		t.Errorf("expected Allow header %q, got %q", "GET, POST", allow)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Error.Code != "method_not_allowed" {
		t.Errorf("expected error code %q, got %q", "method_not_allowed", resp.Error.Code)
	}
}

func TestMethodNotAllowed_WithoutAllowedMethods(t *testing.T) {
	rr := httptest.NewRecorder()
	MethodNotAllowed(rr)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}

	allow := rr.Header().Get("Allow")
	if allow != "" {
		t.Errorf("expected no Allow header, got %q", allow)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Error.Code != "method_not_allowed" {
		t.Errorf("expected error code %q, got %q", "method_not_allowed", resp.Error.Code)
	}
}

func TestWriteErrorFromErr_GenericError_Still500(t *testing.T) {
	// A generic error that is NOT a PermissionError should still be 500
	err := fmt.Errorf("something went wrong")

	rr := httptest.NewRecorder()
	writeErrorFromErr(rr, err, "")

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, rr.Code)
	}
}

// TestWriteError_LogLevel covers ptone/scion#2352's rule for writeError over
// the full set of statuses it applies to: 400/409/422 promoted to INFO so
// operators see them without turning up global log verbosity, 401/403/404/429
// left at DEBUG since promoting them would flood logs on routine client
// errors, and 500 kept at ERROR.
func TestWriteError_LogLevel(t *testing.T) {
	tests := []struct {
		status int
		level  string
	}{
		{http.StatusBadRequest, "INFO"},
		{http.StatusConflict, "INFO"},
		{http.StatusUnprocessableEntity, "INFO"},
		{http.StatusUnauthorized, "DEBUG"},
		{http.StatusForbidden, "DEBUG"},
		{http.StatusNotFound, "DEBUG"},
		{http.StatusTooManyRequests, "DEBUG"},
		{http.StatusInternalServerError, "ERROR"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.status), func(t *testing.T) {
			buf := errorsHelperCaptureLogs(t)

			rr := httptest.NewRecorder()
			writeError(rr, tt.status, "test_code", "test message", nil)

			rec := errorsHelperRecordAtLevel(t, buf, tt.level)
			if rec == nil {
				t.Fatalf("expected a %s record for status %d, got none", tt.level, tt.status)
			}
			if n := len(errorsHelperAPIRecords(t, buf)); n != 1 {
				t.Errorf("expected exactly one API log record for status %d, got %d", tt.status, n)
			}
		})
	}
}

// TestWriteErrorFromErr_LogLevel covers the same rule for writeErrorFromErr,
// mapped to the store sentinels that actually drive its status mapping.
// It also asserts that on the elevated path the INFO line carries only
// the public message, while the raw underlying error is still logged
// at DEBUG.
func TestWriteErrorFromErr_LogLevel(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   int
		level    string
		elevated bool
	}{
		{"invalid_input_400", fmt.Errorf("detail: %w", store.ErrInvalidInput), http.StatusBadRequest, "INFO", true},
		{"version_conflict_409", fmt.Errorf("detail: %w", store.ErrVersionConflict), http.StatusConflict, "INFO", true},
		{"forbidden_403", fmt.Errorf("detail: %w", &secret.PermissionError{Operation: "create secret", Err: fmt.Errorf("denied")}), http.StatusForbidden, "DEBUG", false},
		{"not_found_404", fmt.Errorf("detail: %w", store.ErrNotFound), http.StatusNotFound, "DEBUG", false},
		{"generic_500", fmt.Errorf("boom"), http.StatusInternalServerError, "ERROR", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := errorsHelperCaptureLogs(t)

			rr := httptest.NewRecorder()
			writeErrorFromErr(rr, tt.err, "test-req")

			if rr.Code != tt.status {
				t.Fatalf("status = %d, want %d", rr.Code, tt.status)
			}

			rec := errorsHelperRecordAtLevel(t, buf, tt.level)
			if rec == nil {
				t.Fatalf("expected a %s record, got none", tt.level)
			}

			want := 1
			if tt.elevated {
				want = 2
			}
			if n := len(errorsHelperAPIRecords(t, buf)); n != want {
				t.Errorf("expected exactly %d API log record(s) for %s, got %d", want, tt.name, n)
			}

			if !tt.elevated {
				if _, present := rec["error"]; !present {
					t.Errorf("expected the raw error field on the %s line, got none", tt.level)
				}
				return
			}

			// Elevated (400/409): the INFO line must not carry the raw
			// error, but a DEBUG line with the raw error must still exist.
			if _, present := rec["error"]; present {
				t.Errorf("expected no raw error field on the elevated INFO line, got %v", rec["error"])
			}
			debugRec := errorsHelperRecordAtLevel(t, buf, "DEBUG")
			if debugRec == nil {
				t.Fatal("expected a DEBUG record carrying the underlying error, got none")
			}
			got, _ := debugRec["error"].(string)
			if !strings.Contains(got, tt.err.Error()) {
				t.Errorf("expected DEBUG error field to contain %q, got %q", tt.err.Error(), got)
			}
		})
	}
}
