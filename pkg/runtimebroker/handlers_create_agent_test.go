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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestCreateAgent(t *testing.T) {
	srv := newTestServer(t)

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Created {
		t.Error("expected Created to be true")
	}

	if resp.Agent == nil {
		t.Error("expected agent to be present")
	}
}

func TestCreateAgentMissingName(t *testing.T) {
	srv := newTestServer(t)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

// TestCreateAgentRejectsMultiSegmentName is the regression anchor for the
// isSingleCleanPathElement check added to createAgent. req.Name is joined
// onto a directory as a single path segment all the way down to
// GetAgentDir, so a name that isn't exactly one clean segment must be
// rejected here rather than reaching that join. GetAgentDir/GetAgent (in
// pkg/agent) enforce the same constraint independently before their own
// stale-directory removal branch; this covers the same shape earlier, at
// the request boundary, for every name this check can see regardless of
// what a caller intended.
//
// "a\\b" covers backslash specifically: isSingleCleanPathElement must
// reject it on every platform, not only when os.PathSeparator is '\' (i.e.
// on Windows). '\' is a path separator on Windows and a general
// path-injection vector everywhere else, so it must be rejected regardless
// of GOOS.
func TestCreateAgentRejectsMultiSegmentName(t *testing.T) {
	srv := newTestServer(t)

	for _, name := range []string{"../sibling", "a/../..", "..", ".", "a/b", "/etc", `a\b`, "a\x00b"} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(CreateAgentRequest{Name: name})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("name %q: expected status %d, got %d: %s", name, http.StatusBadRequest, w.Code, w.Body.String())
			}
		})
	}
}

// TestCreateAgentRejectsInvalidProjectSlugBeforeGCSBootstrap is the
// regression test for the GCS-bootstrap workspace directory being built
// from an unvalidated ProjectSlug: a slug of ".." joined onto
// ~/.scion/projects resolves to ~/.scion itself, which must never be
// created or synced into. Asserts the request is rejected and that nothing
// is created under the global directory at all.
func TestCreateAgentRejectsInvalidProjectSlugBeforeGCSBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)

	for _, slug := range []string{"..", ".", "a/../..", "/etc", `a\b`} {
		t.Run(slug, func(t *testing.T) {
			body, err := json.Marshal(CreateAgentRequest{
				Name:                 "agent1",
				ProjectSlug:          slug,
				WorkspaceStoragePath: "workspaces/project-123/project-workspace",
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("slug %q: expected status %d, got %d: %s", slug, http.StatusBadRequest, w.Code, w.Body.String())
			}

			globalDir, err := config.GetGlobalDir()
			if err != nil {
				t.Fatalf("GetGlobalDir: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(globalDir, "projects")); !os.IsNotExist(statErr) {
				t.Errorf("slug %q: expected no projects directory to be created, stat returned: %v", slug, statErr)
			}
		})
	}
}

// TestCreateAgentRejectsWorkspaceDirOutsideRootViaSymlink is a second,
// independent regression test for the same GCS-bootstrap validation gate as
// TestCreateAgentRejectsInvalidProjectSlugBeforeGCSBootstrap, but one that
// the single-path-element check on ProjectSlug cannot catch: an ordinary,
// single-component slug whose directory entry under ~/.scion/projects is
// itself a symlink resolving outside workspaceRoot. Every example slug in
// the sibling test above ("..", ".", "a/../..", "/etc", `a\b`) is already
// refused by isSingleCleanPathElement before the handler ever reaches
// runtime.ValidateWorkspaceSource, so neither test alone proves that second,
// independent gate actually runs. This one reaches it with a slug that
// passes the first check cleanly.
func TestCreateAgentRejectsWorkspaceDirOutsideRootViaSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("GetGlobalDir: %v", err)
	}
	projectsDir := filepath.Join(globalDir, "projects")
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		t.Fatalf("MkdirAll(projectsDir): %v", err)
	}

	// outsideTarget is outside ~/.scion entirely, so a workspace dir that
	// resolves into it must be refused regardless of the slug's own
	// spelling.
	outsideTarget := t.TempDir()
	const slug = "outside-slug"
	if err := os.Symlink(outsideTarget, filepath.Join(projectsDir, slug)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	body, err := json.Marshal(CreateAgentRequest{
		Name:                 "agent1",
		ProjectSlug:          slug,
		WorkspaceStoragePath: "workspaces/project-123/project-workspace",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}

	// The outside target must not have been written into: a 500 from a later
	// stage (storage bucket not configured, GCS sync failure, and so on)
	// would also leave it untouched, so status code alone does not prove
	// the validation gate is what stopped this request before MkdirAll or
	// SyncFromGCS ran.
	entries, err := os.ReadDir(outsideTarget)
	if err != nil {
		t.Fatalf("ReadDir(outsideTarget): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected outside target to remain empty, found: %v", entries)
	}
}

// TestCreateAgentFullStart_HarnessConfigNotFound proves the fix for
// ptone/scion#1316 fault 3: when Start fails because a configured
// harness-config name does not resolve anywhere the broker looked, the
// broker must report a 404 naming the resource instead of the blanket 502
// every other provisioning failure gets.
func TestCreateAgentFullStart_HarnessConfigNotFound(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("failed to find harness-config %q: %w", "antigravity",
		&config.HarnessConfigNotFoundError{Name: "antigravity", Searched: []string{"/srv/broker/harness-configs/antigravity"}})

	body := `{"name": "new-agent", "config": {"template": "claude", "harness": "antigravity"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "antigravity") {
		t.Errorf("expected response to name the unresolved resource, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_OtherErrorStaysRuntimeError proves that a
// provisioning failure unrelated to naming (e.g. a runtime/infra failure)
// still gets the generic 502-mapped RuntimeError, not a 404 — the
// classification in dispatchCreateErrorResponse and its broker-side
// counterpart must be narrow.
func TestCreateAgentFullStart_OtherErrorStaysRuntimeError(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("docker daemon unreachable")

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
}

// TestCreateAgentProvisionOnly_TemplateNotFound is the ProvisionOnly-path
// counterpart of TestCreateAgentFullStart_HarnessConfigNotFound, covering the
// other named resource (template) and the other dispatch branch (Provision).
func TestCreateAgentProvisionOnly_TemplateNotFound(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.provisionErr = fmt.Errorf("failed to load template: %w",
		config.NewTemplateNotFoundError("missing-template", "template missing-template not found"))

	body := `{"name": "new-agent", "provisionOnly": true, "config": {"template": "missing-template"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing-template") {
		t.Errorf("expected response to name the unresolved resource, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_SkillResolutionRateLimited proves that a required
// skill reference failing to resolve because of GitHub rate limiting
// surfaces as a 429, naming the ref and the cause, instead of the generic
// 500 the "other error" branch maps to (#2546).
func TestCreateAgentFullStart_SkillResolutionRateLimited(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/my-skill@main",
		Code:    agent.SkillErrCodeRateLimited,
		Message: "GitHub API request to /repos/example-org/example-skills/commits/main rate limited",
	}

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status %d, got %d: %s", http.StatusTooManyRequests, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/my-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate_limited") {
		t.Errorf("expected response to name the cause, got: %s", w.Body.String())
	}
}

// TestCreateAgentProvisionOnly_SkillResolutionNotFound is the ProvisionOnly
// counterpart, covering the not-found cause mapped to 404.
func TestCreateAgentProvisionOnly_SkillResolutionNotFound(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.provisionErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/missing-skill@main",
		Code:    agent.SkillErrCodeNotFound,
		Message: `skill "missing-skill" not found in repo example-org/example-skills at ref main`,
	}

	body := `{"name": "new-agent", "provisionOnly": true, "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/missing-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not_found") {
		t.Errorf("expected response to name the cause, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_SkillResolutionUpstreamUnavailableBecomes502
// proves that GitHub itself failing (5xx after retries exhausted) surfaces as
// 502, the one 5xx code in the mapping that still names the ref and the
// cause, instead of either the pre-fix 500 or the briefly-considered 400
// default (#2546 R3, O1).
func TestCreateAgentFullStart_SkillResolutionUpstreamUnavailableBecomes502(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/my-skill@main",
		Code:    agent.SkillErrCodeUpstreamUnavailable,
		Message: "GitHub API error (503) while resolving commit, retries exhausted",
	}

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadGateway, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/my-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream_unavailable") {
		t.Errorf("expected response to name the cause, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_SkillResolutionDefaultCodeStaysInternalError
// proves that an uncategorized SkillResolutionError.Code (e.g. a Hub-side
// PreResolvedSkills code this broker version does not recognize) stays on
// the existing 500 path, matching the "any other error" branch, rather than
// being guessed at as a 4xx (#2546 R3).
func TestCreateAgentFullStart_SkillResolutionDefaultCodeStaysInternalError(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/my-skill@main",
		Code:    "storage_error",
		Message: "skill my-skill version abc123 has storage files missing",
	}

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/my-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
	}
}

// TestCreateAgentLaunchIDFromRunID pins SCION_LAUNCH_ID as broker-owned:
// the create request's run ID reaches Manager.Start (which sets the
// variable from it), and a resolved-env value is dropped.
func TestCreateAgentLaunchIDFromRunID(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{
		"name": "test-agent",
		"id": "agent-uuid-123",
		"runId": "run-uuid-789",
		"resolvedEnv": {"SCION_LAUNCH_ID": "forged"},
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if mgr.lastRunID != "run-uuid-789" {
		t.Errorf("RunID = %q, want %q", mgr.lastRunID, "run-uuid-789")
	}
	if got, ok := mgr.lastEnv["SCION_LAUNCH_ID"]; ok {
		t.Errorf("SCION_LAUNCH_ID = %q passed through, want it left to Manager.Start", got)
	}
}

// TestCreateAgentWithHubCredentials tests that Hub authentication env vars are passed to agent.
// This verifies the fix from progress-report.md: RuntimeBroker sets SCION_HUB_URL, SCION_AUTH_TOKEN, SCION_AGENT_ID.
func TestCreateAgentWithHubCredentials(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{
		"name": "test-agent",
		"id": "agent-uuid-123",
		"projectId": "project-uuid-456",
		"hubEndpoint": "https://hub.example.com",
		"agentToken": "secret-token-xyz",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Hub credentials were passed to the manager
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	// Check SCION_HUB_ENDPOINT (primary)
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com', got %q", got)
	}

	// Check SCION_HUB_URL (legacy compat)
	if got := mgr.lastEnv["SCION_HUB_URL"]; got != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_URL='https://hub.example.com' (legacy compat), got %q", got)
	}

	// Check SCION_AUTH_TOKEN
	if got := mgr.lastEnv["SCION_AUTH_TOKEN"]; got != "secret-token-xyz" {
		t.Errorf("expected SCION_AUTH_TOKEN='secret-token-xyz', got %q", got)
	}

	// Check SCION_AGENT_ID
	if got := mgr.lastEnv["SCION_AGENT_ID"]; got != "agent-uuid-123" {
		t.Errorf("expected SCION_AGENT_ID='agent-uuid-123', got %q", got)
	}

	// Check SCION_PROJECT_ID
	if got := mgr.lastEnv["SCION_PROJECT_ID"]; got != "project-uuid-456" {
		t.Errorf("expected SCION_PROJECT_ID='project-uuid-456', got %q", got)
	}
	if _, ok := mgr.lastEnv["SCION_GROVE_ID"]; ok {
		t.Errorf("expected SCION_GROVE_ID to be absent, got %q", mgr.lastEnv["SCION_GROVE_ID"])
	}
}

// TestCreateAgentDebugServerDoesNotSetAgentDebug covers ptone/scion#4098:
// the test server runs with Debug enabled, and the created agent's
// environment must not gain SCION_DEBUG (or a SCION_LOG_LEVEL) from it.
func TestCreateAgentDebugServerDoesNotSetAgentDebug(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()
	if !srv.config.Debug {
		t.Fatal("precondition: test server must run with Debug enabled")
	}

	body := `{"name": "debug-agent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}
	for _, key := range []string{"SCION_DEBUG", "SCION_LOG_LEVEL"} {
		if v, ok := mgr.lastEnv[key]; ok {
			t.Errorf("expected %s to be absent when the server runs with debug, got %q", key, v)
		}
	}
}

// TestCreateAgentExplicitLogLevelPassesThrough covers ptone/scion#4098: an
// explicit SCION_LOG_LEVEL in the request env reaches the agent unchanged.
func TestCreateAgentExplicitLogLevelPassesThrough(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "log-level-agent", "config": {"env": ["SCION_LOG_LEVEL=info,hub=debug"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if got := mgr.lastEnv["SCION_LOG_LEVEL"]; got != "info,hub=debug" {
		t.Errorf("expected SCION_LOG_LEVEL='info,hub=debug' unchanged, got %q", got)
	}
	if v, ok := mgr.lastEnv["SCION_DEBUG"]; ok {
		t.Errorf("expected SCION_DEBUG to be absent, got %q", v)
	}
}

// TestCreateAgentWithBrokerID tests that SCION_BROKER_ID env var is set from server config.
func TestCreateAgentWithBrokerID(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "broker-id-agent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	if got := mgr.lastEnv["SCION_BROKER_ID"]; got != "test-broker-id" {
		t.Errorf("expected SCION_BROKER_ID='test-broker-id', got %q", got)
	}

	if got := mgr.lastEnv["SCION_BROKER_NAME"]; got != "test-host" {
		t.Errorf("expected SCION_BROKER_NAME='test-host', got %q", got)
	}
}

// TestCreateAgentWithResolvedEnv tests that resolvedEnv from Hub is merged with config.Env.
func TestCreateAgentWithResolvedEnv(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	// resolvedEnv contains Hub-provided secrets and variables
	// config.Env contains explicit overrides (takes precedence)
	body := `{
		"name": "env-merge-agent",
		"resolvedEnv": {
			"SECRET_KEY": "hub-secret",
			"SHARED_VAR": "from-hub"
		},
		"config": {
			"env": ["EXPLICIT_VAR=explicit-value", "SHARED_VAR=from-config"]
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	// Check that resolvedEnv was applied
	if got := mgr.lastEnv["SECRET_KEY"]; got != "hub-secret" {
		t.Errorf("expected SECRET_KEY='hub-secret' from resolvedEnv, got %q", got)
	}

	// Check that config.Env was applied
	if got := mgr.lastEnv["EXPLICIT_VAR"]; got != "explicit-value" {
		t.Errorf("expected EXPLICIT_VAR='explicit-value' from config.Env, got %q", got)
	}

	// Check that config.Env takes precedence over resolvedEnv
	if got := mgr.lastEnv["SHARED_VAR"]; got != "from-config" {
		t.Errorf("expected SHARED_VAR='from-config' (config.Env should override resolvedEnv), got %q", got)
	}
}

// TestCreateAgentWithoutHubCredentials tests agent creation without Hub integration.
func TestCreateAgentWithoutHubCredentials(t *testing.T) {
	// Clear dev token env var to prevent broker from forwarding it to agents
	t.Setenv("SCION_AUTH_TOKEN", "")

	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "local-agent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Env should still be set (at minimum SCION_DEBUG since debug mode is on)
	if mgr.lastEnv == nil {
		t.Fatal("expected environment to be initialized")
	}

	// Hub credentials should NOT be present
	if _, exists := mgr.lastEnv["SCION_HUB_ENDPOINT"]; exists {
		t.Error("expected SCION_HUB_ENDPOINT to not be set when no hubEndpoint provided")
	}

	if _, exists := mgr.lastEnv["SCION_HUB_URL"]; exists {
		t.Error("expected SCION_HUB_URL to not be set when no hubEndpoint provided")
	}

	if _, exists := mgr.lastEnv["SCION_AUTH_TOKEN"]; exists {
		t.Error("expected SCION_AUTH_TOKEN to not be set when no agentToken provided")
	}

	if _, exists := mgr.lastEnv["SCION_AGENT_ID"]; exists {
		t.Error("expected SCION_AGENT_ID to not be set when no id provided")
	}
}

func TestCreateAgentWithWorkspace(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "workspace-agent",
		"config": {"template": "claude", "workspace": "./zz-ecommerce-site"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Start was called and workspace was passed through
	if !mgr.startCalled {
		t.Error("expected Start to be called")
	}
	if mgr.lastOpts.Workspace != "./zz-ecommerce-site" {
		t.Errorf("expected workspace './zz-ecommerce-site', got '%s'", mgr.lastOpts.Workspace)
	}
}

func TestCreateAgentProvisionOnlyWithWorkspace(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "ws-provision-agent",
		"id": "agent-uuid-ws",
		"slug": "ws-provision-agent",
		"provisionOnly": true,
		"config": {"template": "claude", "workspace": "./my-subfolder", "task": "do work"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Provision was called with the workspace
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
	if mgr.lastOpts.Workspace != "./my-subfolder" {
		t.Errorf("expected workspace './my-subfolder', got '%s'", mgr.lastOpts.Workspace)
	}
}

func TestCreateAgentWithCreatorName(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{
		"name": "creator-agent",
		"creatorName": "alice@example.com",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	if got := mgr.lastEnv["SCION_CREATOR"]; got != "alice@example.com" {
		t.Errorf("expected SCION_CREATOR='alice@example.com', got %q", got)
	}
}

func TestCreateAgentWithoutCreatorName(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "no-creator-agent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	if _, exists := mgr.lastEnv["SCION_CREATOR"]; exists {
		t.Error("expected SCION_CREATOR to not be set when no creatorName provided")
	}
}

// TestCreateAgentStartFailure_CleansUpFiles verifies that when mgr.Start() fails
// (e.g. auth resolution error), the broker cleans up provisioned agent files so
// they don't become orphans that trigger spurious hub sync-registration.
func TestCreateAgentStartFailure_CleansUpFiles(t *testing.T) {
	// Create a temp directory to act as the project path with agent files
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	agentDir := filepath.Join(projectPath, "agents", "fail-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	// Write a scion-agent.yaml so the agent is discoverable
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.yaml"), []byte("harness: gemini\n"), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.yaml: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	mgr := &provisionCapturingManager{}
	mgr.startErr = fmt.Errorf("auth resolution failed: gemini: auth type \"api-key\" selected but no API key found")
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	body := fmt.Sprintf(`{
		"name": "fail-agent",
		"projectPath": %q,
		"config": {"task": "do something"}
	}`, projectPath)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should return runtime error
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}

	// Verify agent directory was cleaned up
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("expected agent directory to be cleaned up after start failure, but it still exists: %s", agentDir)
	}
}
