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

// Package hub – tests for PATCH /api/v1/secrets/{key} (metadata-only updates).
//
// Contract under test:
//   - PATCH returns 200 with updated metadata
//   - PATCH does not modify the stored secret value
//   - PATCH on nonexistent secret returns 404
//   - PATCH with invalid type returns 422

package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestPatchSecret_ReturnsUpdatedMetadata verifies PATCH returns 200 with
// the updated metadata fields.
func TestPatchSecret_ReturnsUpdatedMetadata(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)

	// Create a secret first via PUT
	createBody := SetSecretRequest{
		Value:         base64.StdEncoding.EncodeToString([]byte("my-secret-value")),
		Description:   "Original description",
		InjectionMode: "as_needed",
		Type:          "environment",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/PATCH_TEST_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH to update description and injection mode
	newDesc := "Updated description"
	patchBody := PatchSecretRequest{
		Description:   &newDesc,
		InjectionMode: "always",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/PATCH_TEST_KEY", patchBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.Secret
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result.Description != "Updated description" {
		t.Errorf("expected description %q, got %q", "Updated description", result.Description)
	}
	if result.InjectionMode != "always" {
		t.Errorf("expected injectionMode %q, got %q", "always", result.InjectionMode)
	}
	if result.Version < 2 {
		t.Errorf("expected version >= 2 after PATCH, got %d", result.Version)
	}
}

// TestPatchSecret_ValueUnchanged verifies that PATCH does not modify the stored
// secret value — a subsequent GET/read should return the original.
func TestPatchSecret_ValueUnchanged(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	// Create a secret
	originalValue := "super-secret-value"
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte(originalValue)),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/VALUE_CHECK_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH to update description
	desc := "New description"
	patchBody := PatchSecretRequest{
		Description: &desc,
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/VALUE_CHECK_KEY", patchBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Read the secret value to verify it's unchanged
	sv, err := localBackend.Get(ctx, "VALUE_CHECK_KEY", store.ScopeUser, DevUserID)
	if err != nil {
		t.Fatalf("Get after PATCH failed: %v", err)
	}
	if sv.Value != originalValue {
		t.Errorf("expected value %q unchanged after PATCH, got %q", originalValue, sv.Value)
	}
}

// TestPatchSecret_NotFound verifies that PATCH returns 404 for a nonexistent secret.
func TestPatchSecret_NotFound(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	desc := "test"
	patchBody := PatchSecretRequest{
		Description: &desc,
	}
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/NONEXISTENT_KEY", patchBody)
	if rec.Code != http.StatusNotFound {
		t.Errorf("PATCH on nonexistent secret: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSecret_InvalidType verifies that PATCH returns 400 for an invalid type.
func TestPatchSecret_InvalidType(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	// Create a secret first
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/TYPE_CHECK_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with invalid type
	patchBody := PatchSecretRequest{
		Type: "invalid_type",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/TYPE_CHECK_KEY", patchBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with invalid type: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
