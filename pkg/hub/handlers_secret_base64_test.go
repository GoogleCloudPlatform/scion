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

// Package hub – tests for the Encoding field and base64/raw-value handling in
// all four secret-write handlers:
//
//   - setSecret             (PUT /api/v1/secrets/{key})
//   - handleAgentSecrets    (PUT /api/v1/agents/{id}/secrets/{key})
//   - handleProjectSecretByKey (PUT /api/v1/projects/{id}/secrets/{key})
//   - handleBrokerSecretByKey  (PUT /api/v1/runtime-brokers/{id}/secrets/{key})
//
// Contract under test:
//   - Default (encoding omitted or "base64"): value MUST be valid base64; any
//     invalid base64 is rejected with 400 JSON — no silent fallback.
//   - Encoding "raw": value is stored as literal text with no decoding.
//   - Remaining validations (path traversal, size limit) still reject with
//     structured JSON 400s regardless of encoding.

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

// TestSetSecret_ValidBase64_Works confirms backward-compatible base64 path works.
func TestSetSecret_ValidBase64_Works(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	body := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("my-plain-value")),
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/BASE64_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid base64: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSetSecret_InvalidBase64_DefaultEncoding_Returns400 confirms that without
// explicit encoding="raw", values that fail base64 decode are rejected.
func TestSetSecret_InvalidBase64_DefaultEncoding_Returns400(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	body := SetSecretRequest{
		// Raw text with characters that make it invalid base64.
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/RAW_KEY_NO_ENC", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid base64 without encoding field: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

// TestSetSecret_ValidBase64LookingValue_DefaultEncoding_Decoded ensures that
// values which happen to be valid base64 (e.g. "admin123") are decoded rather
// than stored raw under the default path.  This is the correct behavior — if
// the caller wants literal storage they must set Encoding:"raw".
func TestSetSecret_ValidBase64LookingValue_DefaultEncoding_Decoded(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	// "YWRtaW4xMjM=" is base64 for "admin123".
	base64OfAdmin123 := base64.StdEncoding.EncodeToString([]byte("admin123"))
	body := SetSecretRequest{Value: base64OfAdmin123}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/ADMIN123_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := localBackend.Get(ctx, "ADMIN123_KEY", store.ScopeUser, DevUserID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	// The stored value should be the decoded "admin123", not the base64 string.
	if stored.Value != "admin123" {
		t.Errorf("expected stored value %q, got %q", "admin123", stored.Value)
	}
}

// TestSetSecret_RawEncoding_StoresLiteralValue confirms encoding="raw" stores
// the value byte-for-byte, including text that happens to be valid base64.
func TestSetSecret_RawEncoding_StoresLiteralValue(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	// "testtest" is valid base64 (decodes to binary), but with encoding=raw
	// it must be stored literally.
	rawValue := "testtest"
	body := SetSecretRequest{
		Value:    rawValue,
		Encoding: "raw",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/RAW_ENC_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("encoding=raw: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := localBackend.Get(ctx, "RAW_ENC_KEY", store.ScopeUser, DevUserID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("encoding=raw roundtrip: stored %q != original %q", stored.Value, rawValue)
	}
}

// TestSetSecret_RawEncoding_SpecialCharsLiteral confirms encoding="raw" stores
// arbitrary special-character strings without any encoding/decoding.
func TestSetSecret_RawEncoding_SpecialCharsLiteral(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	rawValue := "pa$$w0rd!with&special<chars>"
	body := SetSecretRequest{
		Value:    rawValue,
		Encoding: "raw",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/SPECIAL_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("encoding=raw special chars: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := localBackend.Get(ctx, "SPECIAL_KEY", store.ScopeUser, DevUserID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("encoding=raw roundtrip: stored %q != original %q", stored.Value, rawValue)
	}
}

// TestSetSecret_PathTraversal_ReturnsJSONError confirms the path-traversal
// validation still produces structured JSON regardless of encoding.
func TestSetSecret_PathTraversal_ReturnsJSONError(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

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

// TestSetSecret_SizeLimit_ReturnsJSONError confirms the size-limit validation
// still produces structured JSON; uses encoding=raw to reach that check with
// an oversized value.
func TestSetSecret_SizeLimit_ReturnsJSONError(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	body := SetSecretRequest{
		Value:    strings.Repeat("x", 64*1024+1),
		Encoding: "raw",
		Type:     "file",
		Target:   "/tmp/big-file.txt",
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

func TestAgentSecrets_ValidBase64_Works(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("agent-secret")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/AGENT_B64_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid base64: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_InvalidBase64_DefaultEncoding_Returns400(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/AGENT_RAW_NO_ENC", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid base64 without encoding: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

func TestAgentSecrets_RawEncoding_StoresLiteralValue(t *testing.T) {
	srv, s, agentID, projectID, agentToken := setupAgentSecretTest(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	// "password" is valid base64, but encoding=raw must store it literally.
	rawValue := "password"
	body := AgentSetSecretRequest{
		Value:    rawValue,
		Encoding: "raw",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/AGENT_RAW_ENC_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("encoding=raw: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := localBackend.Get(ctx, "AGENT_RAW_ENC_KEY", store.ScopeProject, projectID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("encoding=raw roundtrip: stored %q != original %q", stored.Value, rawValue)
	}
}

func TestAgentSecrets_RawEncoding_SpecialCharsLiteral(t *testing.T) {
	srv, s, agentID, projectID, agentToken := setupAgentSecretTest(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	rawValue := "agent_raw!value@host"
	body := AgentSetSecretRequest{
		Value:    rawValue,
		Encoding: "raw",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/AGENT_SPECIAL_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("encoding=raw: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := localBackend.Get(ctx, "AGENT_SPECIAL_KEY", store.ScopeProject, projectID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("encoding=raw roundtrip: stored %q != original %q", stored.Value, rawValue)
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

	body := AgentSetSecretRequest{
		Value:    strings.Repeat("a", 64*1024+1),
		Encoding: "raw",
		Type:     "file",
		Target:   "/tmp/bigfile.dat",
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
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("proj-secret-b64")
	project := &store.Project{
		ID:      projectID,
		Name:    "Encoding Test Project",
		Slug:    "encoding-test-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	return srv, projectID
}

func TestProjectSecretByKey_ValidBase64_Works(t *testing.T) {
	srv, projectID := setupProjectSecretTest(t)

	body := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("project-value")),
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+projectID+"/secrets/PROJ_B64_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid base64: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectSecretByKey_InvalidBase64_DefaultEncoding_Returns400(t *testing.T) {
	srv, projectID := setupProjectSecretTest(t)

	body := SetSecretRequest{
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+projectID+"/secrets/PROJ_INVALID_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid base64 without encoding: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

func TestProjectSecretByKey_RawEncoding_StoresLiteralValue(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	projectID := tid("proj-roundtrip")
	project := &store.Project{
		ID:      projectID,
		Name:    "Roundtrip Project",
		Slug:    "roundtrip-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// "admin123" is coincidentally valid base64 — encoding=raw must store it literally.
	rawValue := "admin123"
	body := SetSecretRequest{Value: rawValue, Encoding: "raw"}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+projectID+"/secrets/PROJ_ROUND_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("encoding=raw: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := localBackend.Get(ctx, "PROJ_ROUND_KEY", store.ScopeProject, projectID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("encoding=raw roundtrip: stored %q != original %q", stored.Value, rawValue)
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

	body := SetSecretRequest{
		Value:    strings.Repeat("z", 64*1024+1),
		Encoding: "raw",
		Type:     "file",
		Target:   "/tmp/bigfile.txt",
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
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	brokerID := tid("broker-secret-b64")
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "Encoding Test Broker",
		Slug:    "encoding-test-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	return srv, brokerID
}

func TestBrokerSecretByKey_ValidBase64_Works(t *testing.T) {
	srv, brokerID := setupBrokerSecretTest(t)

	body := SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("broker-value")),
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_B64_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid base64: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBrokerSecretByKey_InvalidBase64_DefaultEncoding_Returns400(t *testing.T) {
	srv, brokerID := setupBrokerSecretTest(t)

	body := SetSecretRequest{
		Value: "raw!value$that=is*not-base64",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_INVALID_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid base64 without encoding: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

func TestBrokerSecretByKey_RawEncoding_StoresLiteralValue(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	brokerID := tid("broker-roundtrip")
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "Roundtrip Broker",
		Slug:    "roundtrip-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	// "testtest" is valid base64 — encoding=raw must store it literally.
	rawValue := "testtest"
	body := SetSecretRequest{Value: rawValue, Encoding: "raw"}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_ROUND_KEY", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("encoding=raw: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := localBackend.Get(ctx, "BROKER_ROUND_KEY", store.ScopeRuntimeBroker, brokerID)
	if err != nil {
		t.Fatalf("failed to retrieve stored secret: %v", err)
	}
	if stored.Value != rawValue {
		t.Errorf("encoding=raw roundtrip: stored %q != original %q", stored.Value, rawValue)
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

	body := SetSecretRequest{
		Value:    strings.Repeat("b", 64*1024+1),
		Encoding: "raw",
		Type:     "file",
		Target:   "/tmp/brokerfile.dat",
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/runtime-brokers/"+brokerID+"/secrets/BROKER_BIG_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("size limit: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
}

// ============================================================================
// Unrecognized encoding value (all handlers share the same guard)
// ============================================================================

// TestSetSecret_UnrecognizedEncoding_Returns400 confirms that an encoding value
// other than "" / "base64" / "raw" is rejected with a structured JSON 400, so
// typos like "bas64" or "Base64" are caught rather than silently treated as
// strict base64.
func TestSetSecret_UnrecognizedEncoding_Returns400(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	body := SetSecretRequest{
		Value:    base64.StdEncoding.EncodeToString([]byte("value")),
		Encoding: "bas64", // typo — not a valid encoding value
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/UNK_ENC_KEY", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unrecognized encoding: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	checkJSONError(t, rec.Body.String())
	// Confirm the error body mentions the invalid value so callers can self-diagnose.
	if !strings.Contains(rec.Body.String(), "bas64") {
		t.Errorf("expected error body to contain the invalid encoding value, got: %s", rec.Body.String())
	}
}

// Ensure the agent fixture is usable from this file (it uses a local import
// via setupAgentSecretTest which references state.PhaseRunning).
var _ = state.PhaseRunning
