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

// errorsHelperLastRecord returns the last JSON log record in buf, or nil if
// there are none.
func errorsHelperLastRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		last = rec
	}
	return last
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

// TestWriteError_LogLevel_ElevatedStatus covers ptone/scion#2352: 422 (the
// no-runtime-broker-available case on agent create) must log at INFO, not
// DEBUG, so operators see it without turning up global log verbosity.
func TestWriteError_LogLevel_ElevatedStatus(t *testing.T) {
	buf := errorsHelperCaptureLogs(t)

	rr := httptest.NewRecorder()
	writeError(rr, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker, "no runtime brokers available", nil)

	rec := errorsHelperLastRecord(t, buf)
	if rec == nil {
		t.Fatal("expected a log record, got none")
	}
	if rec["level"] != "INFO" {
		t.Errorf("expected level INFO for status 422, got %v", rec["level"])
	}
	if got := rec["message"]; got != "no runtime brokers available" {
		t.Errorf("expected logged message to match the public message, got %v", got)
	}
}

// TestWriteError_LogLevel_ExcludedStatus covers the other side of the rule:
// a 404 (and, by the same reasoning, 401/403/429) must stay at DEBUG since
// promoting it would flood logs on routine client errors.
func TestWriteError_LogLevel_ExcludedStatus(t *testing.T) {
	buf := errorsHelperCaptureLogs(t)

	rr := httptest.NewRecorder()
	writeError(rr, http.StatusNotFound, ErrCodeNotFound, "agent not found", nil)

	rec := errorsHelperLastRecord(t, buf)
	if rec == nil {
		t.Fatal("expected a log record, got none")
	}
	if rec["level"] != "DEBUG" {
		t.Errorf("expected level DEBUG for status 404, got %v", rec["level"])
	}
}

// TestWriteErrorFromErr_LogLevel_ElevatedStatus covers the second motivating
// case from ptone/scion#2352: a 400/409-class Go error (here, a version
// conflict) must log at INFO, with only the public message attached — never
// the raw underlying error, which may carry more detail than the response.
func TestWriteErrorFromErr_LogLevel_ElevatedStatus(t *testing.T) {
	buf := errorsHelperCaptureLogs(t)

	rr := httptest.NewRecorder()
	writeErrorFromErr(rr, store.ErrVersionConflict, "test-req-2")

	rec := errorsHelperLastRecord(t, buf)
	if rec == nil {
		t.Fatal("expected a log record, got none")
	}
	if rec["level"] != "INFO" {
		t.Errorf("expected level INFO for a version-conflict (409) error, got %v", rec["level"])
	}
	if _, present := rec["error"]; present {
		t.Errorf("expected no raw error field on the elevated log line, got %v", rec["error"])
	}
}

// TestWriteErrorFromErr_LogLevel_ExcludedStatus asserts a not-found Go error
// (404) still logs at DEBUG.
func TestWriteErrorFromErr_LogLevel_ExcludedStatus(t *testing.T) {
	buf := errorsHelperCaptureLogs(t)

	rr := httptest.NewRecorder()
	writeErrorFromErr(rr, store.ErrNotFound, "test-req-3")

	rec := errorsHelperLastRecord(t, buf)
	if rec == nil {
		t.Fatal("expected a log record, got none")
	}
	if rec["level"] != "DEBUG" {
		t.Errorf("expected level DEBUG for a not-found (404) error, got %v", rec["level"])
	}
}
