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

// Package hub – tests for PATCH on project-scoped and broker-scoped secrets.
//
// Contract under test:
//   - Project-scope PATCH returns 200 with updated metadata
//   - Project-scope PATCH with allowProgeny=true returns 400
//   - Broker-scope PATCH returns 200 with updated metadata
//   - Broker-scope PATCH with allowProgeny=true returns 400

package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestPatchSecret_ProjectScope_ReturnsUpdatedMetadata verifies that PATCH on a
// project-scoped secret returns 200 with updated metadata fields.
func TestPatchSecret_ProjectScope_ReturnsUpdatedMetadata(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("project_patch_meta"),
		Name:    "Patch Meta Project",
		Slug:    "patch-meta-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a secret at project scope via PUT
	createBody := SetSecretRequest{
		Value:         base64.StdEncoding.EncodeToString([]byte("project-secret-value")),
		Description:   "Original project description",
		InjectionMode: "as_needed",
		Type:          "environment",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/PROJ_PATCH_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH to update description and injection mode
	newDesc := "Updated project description"
	patchBody := PatchSecretRequest{
		Description:   &newDesc,
		InjectionMode: "always",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID+"/secrets/PROJ_PATCH_KEY", patchBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.Secret
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result.Description != "Updated project description" {
		t.Errorf("expected description %q, got %q", "Updated project description", result.Description)
	}
	if result.InjectionMode != "always" {
		t.Errorf("expected injectionMode %q, got %q", "always", result.InjectionMode)
	}
	if result.Version < 2 {
		t.Errorf("expected version >= 2 after PATCH, got %d", result.Version)
	}
}

// TestPatchSecret_ProjectScope_AllowProgenyRejected verifies that PATCH with
// allowProgeny=true at project scope returns 400.
func TestPatchSecret_ProjectScope_AllowProgenyRejected(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("project_patch_progeny"),
		Name:    "Patch Progeny Project",
		Slug:    "patch-progeny-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a secret at project scope
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("project-secret")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/PROJ_PROGENY_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with allowProgeny=true — should be rejected
	allowProgeny := true
	patchBody := PatchSecretRequest{
		AllowProgeny: &allowProgeny,
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID+"/secrets/PROJ_PROGENY_KEY", patchBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with allowProgeny at project scope: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSecret_BrokerScope_ReturnsUpdatedMetadata verifies that PATCH on a
// broker-scoped secret returns 200 with updated metadata fields.
func TestPatchSecret_BrokerScope_ReturnsUpdatedMetadata(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("broker_patch_meta"),
		Name:    "Patch Meta Broker",
		Slug:    "patch-meta-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	// Create a secret at broker scope via PUT
	createBody := SetSecretRequest{
		Value:         base64.StdEncoding.EncodeToString([]byte("broker-secret-value")),
		Description:   "Original broker description",
		InjectionMode: "as_needed",
		Type:          "environment",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_PATCH_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH to update description and injection mode
	newDesc := "Updated broker description"
	patchBody := PatchSecretRequest{
		Description:   &newDesc,
		InjectionMode: "always",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_PATCH_KEY", patchBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.Secret
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result.Description != "Updated broker description" {
		t.Errorf("expected description %q, got %q", "Updated broker description", result.Description)
	}
	if result.InjectionMode != "always" {
		t.Errorf("expected injectionMode %q, got %q", "always", result.InjectionMode)
	}
	if result.Version < 2 {
		t.Errorf("expected version >= 2 after PATCH, got %d", result.Version)
	}
}

// TestPatchSecret_BrokerScope_AllowProgenyRejected verifies that PATCH with
// allowProgeny=true at broker scope returns 400.
func TestPatchSecret_BrokerScope_AllowProgenyRejected(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("broker_patch_progeny"),
		Name:    "Patch Progeny Broker",
		Slug:    "patch-progeny-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	// Create a secret at broker scope
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("broker-secret")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_PROGENY_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with allowProgeny=true — should be rejected
	allowProgeny := true
	patchBody := PatchSecretRequest{
		AllowProgeny: &allowProgeny,
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_PROGENY_KEY", patchBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with allowProgeny at broker scope: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// =============================================================================
// R1: File-type target path validation bypass
// =============================================================================

// TestPatchSecret_UserScope_PathTraversalOnFileSecret verifies that PATCH with
// a path traversal target on an existing file-type secret (without type in
// request) is rejected with 400.
func TestPatchSecret_UserScope_PathTraversalOnFileSecret(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)

	// Create a file-type secret at user scope
	createBody := SetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("file-content")),
		Type:   "file",
		Target: "/tmp/safe-file.txt",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/FILE_TRAV_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with path traversal target (no type in request) — should be rejected
	patchBody := PatchSecretRequest{
		Target: "../../etc/shadow",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/FILE_TRAV_KEY", patchBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with path traversal on file secret: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSecret_UserScope_RelativeTargetOnFileSecret verifies that PATCH with
// a relative (non-absolute) target on an existing file-type secret is rejected.
func TestPatchSecret_UserScope_RelativeTargetOnFileSecret(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)

	// Create a file-type secret at user scope
	createBody := SetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("file-content")),
		Type:   "file",
		Target: "/tmp/safe-file.txt",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/FILE_REL_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with relative target (no type in request) — should be rejected
	patchBody := PatchSecretRequest{
		Target: "relative/path",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/FILE_REL_KEY", patchBody)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("PATCH with relative target on file secret: expected 4xx, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSecret_ProjectScope_PathTraversalOnFileSecret verifies that PATCH
// with a path traversal target on an existing file-type project secret is rejected.
func TestPatchSecret_ProjectScope_PathTraversalOnFileSecret(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("project_file_trav"),
		Name:    "File Traversal Project",
		Slug:    "file-trav-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a file-type secret at project scope
	createBody := SetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("file-content")),
		Type:   "file",
		Target: "/tmp/project-file.txt",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/PROJ_FILE_TRAV", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with path traversal target (no type) — should be rejected
	patchBody := PatchSecretRequest{
		Target: "../../etc/shadow",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID+"/secrets/PROJ_FILE_TRAV", patchBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with path traversal on project file secret: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSecret_BrokerScope_PathTraversalOnFileSecret verifies that PATCH
// with a path traversal target on an existing file-type broker secret is rejected.
func TestPatchSecret_BrokerScope_PathTraversalOnFileSecret(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("broker_file_trav"),
		Name:    "File Traversal Broker",
		Slug:    "file-trav-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	// Create a file-type secret at broker scope
	createBody := SetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("file-content")),
		Type:   "file",
		Target: "/tmp/broker-file.txt",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_FILE_TRAV", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with path traversal target (no type) — should be rejected
	patchBody := PatchSecretRequest{
		Target: "../../etc/shadow",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_FILE_TRAV", patchBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with path traversal on broker file secret: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// =============================================================================
// R3-1: Type change to file bypasses stored-target validation
// =============================================================================

// TestPatchSecret_UserScope_TypeChangeToFileValidatesStoredTarget verifies that
// changing type to "file" without providing a target validates the stored target
// (which defaults to the key name, a relative path) and rejects it.
func TestPatchSecret_UserScope_TypeChangeToFileValidatesStoredTarget(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)

	// Create an environment-type secret (target defaults to key name "MY_KEY")
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("secret-value")),
		Type:  "environment",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/MY_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with type=file but no target — stored target "MY_KEY" is relative,
	// so validation should reject it.
	patchBody := PatchSecretRequest{
		Type: "file",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/MY_KEY", patchBody)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("PATCH type change to file with relative stored target: expected 4xx, got %d: %s", rec.Code, rec.Body.String())
	}
}

// =============================================================================
// R2: Missing injectionMode validation
// =============================================================================

// TestPatchSecret_UserScope_InvalidInjectionMode verifies that PATCH with an
// invalid injectionMode is rejected.
func TestPatchSecret_UserScope_InvalidInjectionMode(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)

	// Create a secret at user scope
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("secret-value")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/INJ_INVALID_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with invalid injectionMode — should be rejected
	patchBody := PatchSecretRequest{
		InjectionMode: "invalid",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/INJ_INVALID_KEY", patchBody)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("PATCH with invalid injectionMode: expected 4xx, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSecret_UserScope_ValidInjectionMode verifies that PATCH with a
// valid injectionMode succeeds.
func TestPatchSecret_UserScope_ValidInjectionMode(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)

	// Create a secret at user scope
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("secret-value")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/INJ_VALID_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with valid injectionMode "always" — should succeed
	patchBody := PatchSecretRequest{
		InjectionMode: "always",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/INJ_VALID_KEY", patchBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH with valid injectionMode: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.Secret
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result.InjectionMode != "always" {
		t.Errorf("expected injectionMode %q, got %q", "always", result.InjectionMode)
	}
}

// TestPatchSecret_ProjectScope_InvalidInjectionMode verifies that PATCH with an
// invalid injectionMode at project scope is rejected.
func TestPatchSecret_ProjectScope_InvalidInjectionMode(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("project_inj_invalid"),
		Name:    "Injection Invalid Project",
		Slug:    "inj-invalid-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a secret at project scope
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("secret-value")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/PROJ_INJ_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with invalid injectionMode — should be rejected
	patchBody := PatchSecretRequest{
		InjectionMode: "invalid",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID+"/secrets/PROJ_INJ_KEY", patchBody)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("PATCH with invalid injectionMode at project scope: expected 4xx, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSecret_BrokerScope_InvalidInjectionMode verifies that PATCH with an
// invalid injectionMode at broker scope is rejected.
func TestPatchSecret_BrokerScope_InvalidInjectionMode(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("broker_inj_invalid"),
		Name:    "Injection Invalid Broker",
		Slug:    "inj-invalid-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	// Create a secret at broker scope
	createBody := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("secret-value")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_INJ_KEY", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (create) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH with invalid injectionMode — should be rejected
	patchBody := PatchSecretRequest{
		InjectionMode: "invalid",
	}
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/BROKER_INJ_KEY", patchBody)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("PATCH with invalid injectionMode at broker scope: expected 4xx, got %d: %s", rec.Code, rec.Body.String())
	}
}
