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

// Package hub – tests for the base64/raw-value fallback and structured error
// responses in all four secret-write handlers:
//
//   - setSecret             (PUT /api/v1/secrets/{key})
//   - handleAgentSecrets    (PUT /api/v1/agents/{id}/secrets/{key})
//   - handleProjectSecretByKey (PUT /api/v1/projects/{id}/secrets/{key})
//   - handleBrokerSecretByKey  (PUT /api/v1/runtime-brokers/{id}/secrets/{key})
//
// The tests cover three scenarios per handler:
//  1. Valid base64-encoded value → 200/201 (backward compat, CLI behaviour)
//  2. Raw/non-base64 value       → 200/201 (new fallback, fixes web-UI regression)
//  3. Remaining 400 validations  → body is parseable JSON with {"error":…}
//     (path traversal and file size limit)

package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// checkJSONError asserts that the response body is a valid JSON ErrorResponse.
func checkJSONError(t *testing.T, body string) {
	t.Helper()
	var errResp ErrorResponse
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Errorf("expected JSON error body, got non-JSON: %s", body)
		return
	}
	if errResp.Error.Code == "" {
		t.Errorf("expected non-empty error.code in JSON response, got: %s", body)
	}
	if errResp.Error.Message == "" {
		t.Errorf("expected non-empty error.message in JSON response, got: %s", body)
	}
}

// ============================================================================
// setSecret (PUT /api/v1/secrets/{key})
// ============================================================================

func TestSetSecret_Base64Value_Works(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))

	body := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("my-plain-value")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/BASE64_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("base64 value: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSetSecret_RawValue_FallsBack(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))

	// Send raw text that is NOT valid base64 (contains punctuation that base64 rejects).
	body := SetSecretRequest{
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/RAW_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw value fallback: expected 200 (not 400), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSetSecret_PathTraversal_ReturnsJSONError(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))

	body := SetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("data")),
		Type:   "file",
		Target: "/tmp/../etc/passwd",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/PATH_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("path traversal: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

func TestSetSecret_SizeLimit_ReturnsJSONError(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))

	// Send a raw value that exceeds 64 KiB as a file type.
	// The fallback decodes to []byte(req.Value), so len(decoded) == len(rawValue).
	bigValue := strings.Repeat("x", 64*1024+1)
	body := SetSecretRequest{
		Value:  bigValue,
		Type:   "file",
		Target: "/tmp/big-file.txt",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/BIG_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("size limit: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

// ============================================================================
// handleAgentSecrets (PUT /api/v1/agents/{id}/secrets/{key})
// ============================================================================

func TestAgentSecrets_Base64Value_Works_Compat(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("agent-secret")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/COMPAT_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("base64 compat: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_RawValue_FallsBack(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/RAW_AGENT_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("raw fallback: expected 201 (not 400), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_PathTraversal_ReturnsJSONError(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("data")),
		Type:   "file",
		Target: "/tmp/../etc/shadow",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/TRAV_KEY", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("path traversal: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

func TestAgentSecrets_SizeLimit_ReturnsJSONError(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	bigValue := strings.Repeat("a", 64*1024+1)
	body := AgentSetSecretRequest{
		Value:  bigValue,
		Type:   "file",
		Target: "/tmp/bigfile.dat",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/BIG_AGENT_KEY", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("size limit: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

// ============================================================================
// handleProjectSecretByKey (PUT /api/v1/projects/{id}/secrets/{key})
// ============================================================================

func setupProjectSecretTest(t *testing.T) (*Server, string) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))
	ctx := context.Background()

	projectID := tid("proj-secret-b64")
	project := &store.Project{
		ID:      projectID,
		Name:    "Base64 Fallback Project",
		Slug:    "base64-fallback-project",
		OwnerID: DevUserID, // makes the test-user (admin) the owner
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	return srv, projectID
}

func TestProjectSecretByKey_Base64Value_Works(t *testing.T) {
	srv, projectID := setupProjectSecretTest(t)

	body := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("project-value")),
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+projectID+"/secrets/PROJ_B64_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("base64 value: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectSecretByKey_RawValue_FallsBack(t *testing.T) {
	srv, projectID := setupProjectSecretTest(t)

	body := SetSecretRequest{
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+projectID+"/secrets/PROJ_RAW_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw fallback: expected 200 (not 400), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectSecretByKey_PathTraversal_ReturnsJSONError(t *testing.T) {
	srv, projectID := setupProjectSecretTest(t)

	body := SetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("data")),
		Type:   "file",
		Target: "/home/../etc/passwd",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+projectID+"/secrets/PROJ_TRAV_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("path traversal: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

func TestProjectSecretByKey_SizeLimit_ReturnsJSONError(t *testing.T) {
	srv, projectID := setupProjectSecretTest(t)

	bigValue := strings.Repeat("z", 64*1024+1)
	body := SetSecretRequest{
		Value:  bigValue,
		Type:   "file",
		Target: "/tmp/bigfile.txt",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+projectID+"/secrets/PROJ_BIG_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("size limit: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

// ============================================================================
// handleBrokerSecretByKey (PUT /api/v1/runtime-brokers/{id}/secrets/{key})
// ============================================================================

func setupBrokerSecretTest(t *testing.T) (*Server, string) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))
	ctx := context.Background()

	brokerID := tid("broker-secret-b64")
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "Base64 Fallback Broker",
		Slug:    "base64-fallback-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	return srv, brokerID
}

func TestBrokerSecretByKey_Base64Value_Works(t *testing.T) {
	srv, brokerID := setupBrokerSecretTest(t)

	body := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("broker-value")),
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_B64_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("base64 value: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBrokerSecretByKey_RawValue_FallsBack(t *testing.T) {
	srv, brokerID := setupBrokerSecretTest(t)

	body := SetSecretRequest{
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_RAW_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw fallback: expected 200 (not 400), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBrokerSecretByKey_PathTraversal_ReturnsJSONError(t *testing.T) {
	srv, brokerID := setupBrokerSecretTest(t)

	body := SetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("data")),
		Type:   "file",
		Target: "/opt/../etc/cron.d/badfile",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_TRAV_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("path traversal: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

func TestBrokerSecretByKey_SizeLimit_ReturnsJSONError(t *testing.T) {
	srv, brokerID := setupBrokerSecretTest(t)

	bigValue := strings.Repeat("b", 64*1024+1)
	body := SetSecretRequest{
		Value:  bigValue,
		Type:   "file",
		Target: "/tmp/brokerfile.dat",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_BIG_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("size limit: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

// ============================================================================
// Cross-cutting: verify raw-value roundtrip stores the correct bytes
// ============================================================================

// TestSetSecret_RawValueRoundtrip verifies that when a raw (non-base64) value
// is accepted via the fallback, the stored bytes equal the original raw string —
// not some corrupted decoding of it.
func TestSetSecret_RawValueRoundtrip(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	rawValue := "pa$$w0rd!with&special<chars>"
	body := SetSecretRequest{
		Value: rawValue,
		Scope: "user",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/ROUNDTRIP_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Retrieve the stored secret via the backend and verify the stored value matches.
	stored, err := localBackend.Get(ctx, "ROUNDTRIP_KEY", store.ScopeUser, DevUserID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("roundtrip: stored value %q != original %q", stored.Value, rawValue)
	}
}

// TestAgentSecrets_RawValueRoundtrip performs the same roundtrip check for the
// agent secret endpoint.
func TestAgentSecrets_RawValueRoundtrip(t *testing.T) {
	srv, s, agentID, projectID, agentToken := setupAgentSecretTest(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	rawValue := "agent_raw!value@host"
	body := AgentSetSecretRequest{
		Value: rawValue,
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/AGENT_ROUND_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := localBackend.Get(ctx, "AGENT_ROUND_KEY", store.ScopeProject, projectID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("roundtrip: stored value %q != original %q", stored.Value, rawValue)
	}
}

// Ensure the agent fixture is usable from this file (it uses a local import
// via setupAgentSecretTest which references state.PhaseRunning).
var _ = state.PhaseRunning
