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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

func TestWriteErrorFromErr_GenericError_Still500(t *testing.T) {
	// A generic error that is NOT a PermissionError should still be 500
	err := fmt.Errorf("something went wrong")

	rr := httptest.NewRecorder()
	writeErrorFromErr(rr, err, "")

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, rr.Code)
	}
}
