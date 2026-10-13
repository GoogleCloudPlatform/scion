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

package runtimebroker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestStartAgentEndpoint(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should have an agent in the response
	if resp.Agent == nil {
		t.Fatal("expected agent info in start response")
	}

	// Created should be false for a start (not a create)
	if resp.Created {
		t.Error("expected Created to be false for start operation")
	}
}

// TestStartAgentEndpoint_SkillResolutionError checks that a required skill
// that cannot be resolved while starting gets the typed skill response
// (status from the cause, {skill, cause} details, Retry-After) instead of a
// generic 500.
func TestStartAgentEndpoint_SkillResolutionError(t *testing.T) {
	tests := []struct {
		name           string
		code           string
		retryAfter     string
		wantStatus     int
		wantRetryAfter string
	}{
		{"not found", agent.SkillErrCodeNotFound, "", http.StatusNotFound, ""},
		{"rate limited", agent.SkillErrCodeRateLimited, "42", http.StatusTooManyRequests, "42"},
		{"forbidden", "forbidden", "", http.StatusForbidden, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			mgr.startErr = fmt.Errorf("provision: %w", &agent.SkillResolutionError{
				URI:        "gh://example-org/example-skills/my-skill@main",
				Code:       tt.code,
				Message:    "could not resolve",
				RetryAfter: tt.retryAfter,
			})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d: %s", tt.wantStatus, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("expected Retry-After %q, got %q", tt.wantRetryAfter, got)
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
			}
			if resp.Error.Code != ErrCodeSkillResolution {
				t.Errorf("expected code %q, got %q", ErrCodeSkillResolution, resp.Error.Code)
			}
			if resp.Error.Details["skill"] != "gh://example-org/example-skills/my-skill@main" {
				t.Errorf("expected details.skill to name the ref, got: %v", resp.Error.Details)
			}
			if resp.Error.Details["cause"] != tt.code {
				t.Errorf("expected details.cause %q, got: %v", tt.code, resp.Error.Details)
			}
			// The failure came from inside Manager.Start, so the hub must
			// also see the start marker it uses to settle the run ID.
			if resp.Error.Details[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected details.%s true, got: %v", api.BrokerErrorDetailStartAttempted, resp.Error.Details)
			}
		})
	}
}

// TestRestartAgentEndpoint_SkillResolutionError checks that a restart whose
// start fails on a required skill gets the typed skill response with the
// start run details, the same as start. A not_found cause must not be
// reported as a missing agent.
func TestRestartAgentEndpoint_SkillResolutionError(t *testing.T) {
	tests := []struct {
		name       string
		code       string
		message    string
		wantStatus int
	}{
		{"not found cause", agent.SkillErrCodeNotFound, "skill not found", http.StatusNotFound},
		{"forbidden", agent.SkillErrCodeForbidden, "could not resolve", http.StatusForbidden},
		{"timeout", agent.SkillErrCodeTimeout, "could not resolve", http.StatusGatewayTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			mgr.startErr = fmt.Errorf("provision: %w", &agent.SkillResolutionError{
				URI:     "gh://example-org/example-skills/my-skill@main",
				Code:    tt.code,
				Message: tt.message,
			})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d: %s", tt.wantStatus, w.Code, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
			}
			if resp.Error.Code != ErrCodeSkillResolution {
				t.Errorf("expected code %q, got %q (%s)", ErrCodeSkillResolution, resp.Error.Code, resp.Error.Message)
			}
			if resp.Error.Details["skill"] != "gh://example-org/example-skills/my-skill@main" || resp.Error.Details["cause"] != tt.code {
				t.Errorf("expected details {skill, cause}, got: %v", resp.Error.Details)
			}
			if resp.Error.Details[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected details.%s true, got: %v", api.BrokerErrorDetailStartAttempted, resp.Error.Details)
			}
		})
	}
}

// TestStartAgentEndpoint_OtherErrorStays500 checks that a start failure that
// is not a skill resolution failure keeps the generic 500.
func TestStartAgentEndpoint_OtherErrorStays500(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = errors.New("container runtime unavailable")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
}

// TestStartAgentBrokerIDEnv verifies that startAgent sets SCION_BROKER_ID from broker config.
func TestStartAgentBrokerIDEnv(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if got := mgr.lastOpts.Env["SCION_BROKER_ID"]; got != "test-broker-id" {
		t.Errorf("expected SCION_BROKER_ID='test-broker-id', got %q", got)
	}

	if got := mgr.lastOpts.Env["SCION_BROKER_NAME"]; got != "test-host" {
		t.Errorf("expected SCION_BROKER_NAME='test-host', got %q", got)
	}
}

// TestStartAgentLegacyGroveFieldsNotHonoured verifies that a start request
// carrying only the retired grovePath/groveSlug fields resolves no project
// path: the legacy fallback that used to promote them into
// ProjectPath/ProjectSlug is gone.
func TestStartAgentLegacyGroveFieldsNotHonoured(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"grovePath": "/projects/my-local-project/.scion", "groveSlug": "my-hub-project"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/legacy-fields-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if mgr.lastOpts.ProjectPath != "" {
		t.Errorf("expected legacy grovePath to be ignored, got ProjectPath %q", mgr.lastOpts.ProjectPath)
	}
}

func TestStartAgentInlineConfigModelUpdatesExistingAgentConfig(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "configured-agent"
	agentDir := config.GetAgentDir(projectDir, agentName, false)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"gemini","max_turns":3}`), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}

	body := fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"model": "gemini-2.5-pro",
			"max_turns": 7
		}
	}`, projectDir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read updated scion-agent.json: %v", err)
	}
	var updated api.ScionConfig
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatalf("failed to parse updated scion-agent.json: %v", err)
	}

	if updated.Harness != "gemini" {
		t.Errorf("expected existing harness to be preserved, got %q", updated.Harness)
	}
	if updated.Model != "gemini-2.5-pro" {
		t.Errorf("expected model %q, got %q", "gemini-2.5-pro", updated.Model)
	}
	if updated.MaxTurns != 7 {
		t.Errorf("expected max_turns 7, got %d", updated.MaxTurns)
	}
}

// TestStartAgent_RejectsTraversalName is the regression anchor for
// startAgent's containment check: id must be a single, clean path element
// that resolves under the project's agents root before any of
// applyInlineConfigUpdate, GetSavedProfile, GetSavedPhase, or mgr.Start runs,
// checked independently of whatever the request routing already filtered.
func TestStartAgent_RejectsTraversalName(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	projectDir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(filepath.Join(projectDir, "agents"), 0755); err != nil {
		t.Fatalf("failed to create projectDir/agents: %v", err)
	}
	// Sentinel representing project-level state that must remain untouched
	// when the request's agent id fails containment.
	sentinelPath := filepath.Join(projectDir, "scion-agent.json")
	const sentinelContent = `{"harness":"sentinel-do-not-touch"}`
	if err := os.WriteFile(sentinelPath, []byte(sentinelContent), 0644); err != nil {
		t.Fatalf("failed to write sentinel: %v", err)
	}

	body := fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"model": "gemini-2.5-pro"
		}
	}`, projectDir)
	// Decodes to id="..", action="start".
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/..%2Fstart", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if mgr.startCalled {
		t.Fatal("expected Start not to be called")
	}

	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatalf("sentinel must survive untouched, but read failed: %v", err)
	}
	if string(data) != sentinelContent {
		t.Fatalf("sentinel must survive unmodified, got: %s", data)
	}
}

func TestStartAgentInlineConfigPassedForProvisionOnStart(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	projectDir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}

	body := fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"model": "gemini-2.5-pro"
		}
	}`, projectDir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/provision-on-start-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	if mgr.lastOpts.InlineConfig == nil {
		t.Fatal("expected InlineConfig to be passed to Start")
	}
	if mgr.lastOpts.InlineConfig.Model != "gemini-2.5-pro" {
		t.Errorf("expected inline model %q, got %q", "gemini-2.5-pro", mgr.lastOpts.InlineConfig.Model)
	}
}

func TestStartAgentTelemetryOverrideFromResolvedEnv(t *testing.T) {
	// When resolvedEnv contains SCION_TELEMETRY_ENABLED=true, startAgent
	// should translate it to opts.TelemetryOverride so that Start() enables
	// harness telemetry env injection and cloud config merging.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"resolvedEnv": {"SCION_TELEMETRY_ENABLED": "true"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/telemetry-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	if mgr.lastOpts.TelemetryOverride == nil {
		t.Fatal("expected TelemetryOverride to be set")
	}
	if !*mgr.lastOpts.TelemetryOverride {
		t.Error("expected TelemetryOverride to be true")
	}
}

func TestStartAgentTelemetryOverrideDisabled(t *testing.T) {
	// When resolvedEnv contains SCION_TELEMETRY_ENABLED=false, startAgent
	// should set TelemetryOverride to false.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"resolvedEnv": {"SCION_TELEMETRY_ENABLED": "false"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/telemetry-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	if mgr.lastOpts.TelemetryOverride == nil {
		t.Fatal("expected TelemetryOverride to be set")
	}
	if *mgr.lastOpts.TelemetryOverride {
		t.Error("expected TelemetryOverride to be false")
	}
}
