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
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- 2.1: System Check (Doctor) ---

type DiagnosticResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "pass", "warn", "fail"
	Message string `json:"message"`
}

type systemCheckResponse struct {
	Results []DiagnosticResult `json:"results"`
	Ready   bool              `json:"ready"`
}

func GatherDiagnostics(ctx context.Context, cfg *config.VersionedSettings) []DiagnosticResult {
	var results []DiagnosticResult

	// Check git
	if _, err := exec.LookPath("git"); err != nil {
		results = append(results, DiagnosticResult{Name: "git", Status: "fail", Message: "git not found in PATH"})
	} else if out, err := exec.CommandContext(ctx, "git", "--version").Output(); err != nil {
		results = append(results, DiagnosticResult{Name: "git", Status: "warn", Message: "git found but version check failed"})
	} else {
		results = append(results, DiagnosticResult{Name: "git", Status: "pass", Message: trimOutput(string(out))})
	}

	// Check runtime detection
	detected, err := config.DetectLocalRuntime()
	if err != nil {
		results = append(results, DiagnosticResult{Name: "runtime", Status: "fail", Message: err.Error()})
	} else {
		results = append(results, DiagnosticResult{Name: "runtime", Status: "pass", Message: fmt.Sprintf("detected runtime: %s", detected)})
	}

	// Check global dir exists
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		results = append(results, DiagnosticResult{Name: "config", Status: "fail", Message: "cannot determine global config directory"})
	} else if _, err := os.Stat(filepath.Join(globalDir, "settings.yaml")); os.IsNotExist(err) {
		results = append(results, DiagnosticResult{Name: "config", Status: "warn", Message: "settings.yaml not found — run init"})
	} else {
		results = append(results, DiagnosticResult{Name: "config", Status: "pass", Message: "settings.yaml found"})
	}

	return results
}

func (s *Server) handleSystemCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	results := GatherDiagnostics(r.Context(), nil)

	ready := true
	for _, res := range results {
		if res.Status == "fail" {
			ready = false
			break
		}
	}

	writeJSON(w, http.StatusOK, systemCheckResponse{
		Results: results,
		Ready:   ready,
	})
}

// --- 2.2: Runtime GET/PUT ---

type systemRuntimeResponse struct {
	Detected   string `json:"detected"`
	Configured string `json:"configured"`
	Available  bool   `json:"available"`
}

type putRuntimeRequest struct {
	Runtime string `json:"runtime"`
}

var validRuntimes = map[string]bool{
	"docker":    true,
	"podman":    true,
	"container": true,
}

func (s *Server) handleSystemRuntime(w http.ResponseWriter, r *http.Request) {
	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetRuntime(w, r)
	case http.MethodPut:
		s.handlePutRuntime(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleGetRuntime(w http.ResponseWriter, r *http.Request) {
	detected, detectErr := config.DetectLocalRuntime()
	available := detectErr == nil

	var configured string
	globalDir, err := config.GetGlobalDir()
	if err == nil {
		if vs, loadErr := config.LoadSingleFileVersioned(globalDir); loadErr == nil && vs != nil {
			configured = vs.ActiveProfile
			if vs.Profiles != nil {
				if profile, ok := vs.Profiles[vs.ActiveProfile]; ok {
					configured = profile.Runtime
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, systemRuntimeResponse{
		Detected:   detected,
		Configured: configured,
		Available:  available,
	})
}

func (s *Server) handlePutRuntime(w http.ResponseWriter, r *http.Request) {
	var req putRuntimeRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if !validRuntimes[req.Runtime] {
		ValidationError(w, fmt.Sprintf("invalid runtime %q: must be docker, podman, or container", req.Runtime), nil)
		return
	}

	if err := config.UpdateSetting("", "active_profile", req.Runtime, true); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save runtime setting", nil)
		return
	}

	writeJSON(w, http.StatusOK, systemRuntimeResponse{
		Detected:   req.Runtime,
		Configured: req.Runtime,
		Available:  true,
	})
}

// --- 2.3: Onboarding Status ---

type OnboardingStatus struct {
	Initialized     bool `json:"initialized"`
	IdentitySet     bool `json:"identitySet"`
	RuntimeOK       bool `json:"runtimeOK"`
	HarnessesSeeded bool `json:"harnessesSeeded"`
	ImagesPresent   bool `json:"imagesPresent"`
	HasWorkspace    bool `json:"hasWorkspace"`
	Complete        bool `json:"complete"`
}

func (s *Server) computeOnboardingStatus(ctx context.Context) OnboardingStatus {
	var status OnboardingStatus

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return status
	}

	// Initialized: settings.yaml exists
	settingsPath := config.GetSettingsPath(globalDir)
	status.Initialized = settingsPath != ""

	// IdentitySet: dev auth has a non-default username
	if status.Initialized {
		if vs, loadErr := config.LoadSingleFileVersioned(globalDir); loadErr == nil && vs != nil {
			if vs.Server != nil && vs.Server.Auth != nil {
				auth := vs.Server.Auth
				status.IdentitySet = auth.DisplayName != "" || auth.Email != "" || auth.Username != ""
			}
		}
	}

	// RuntimeOK: a runtime is detected and reachable
	_, detectErr := config.DetectLocalRuntime()
	status.RuntimeOK = detectErr == nil

	// HarnessesSeeded: at least one harness-config exists
	harnessConfigsDir := filepath.Join(globalDir, "harness-configs")
	if entries, err := os.ReadDir(harnessConfigsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				status.HarnessesSeeded = true
				break
			}
		}
	}

	// ImagesPresent: best-effort check — skip for now (optional per spec)
	status.ImagesPresent = false

	// HasWorkspace: at least one project in the store
	if s.store != nil {
		result, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1})
		if err == nil && result != nil && len(result.Items) > 0 {
			status.HasWorkspace = true
		}
	}

	// Complete: all required steps done (ImagesPresent is optional)
	status.Complete = status.Initialized && status.IdentitySet && status.RuntimeOK && status.HarnessesSeeded

	return status
}

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	status := s.computeOnboardingStatus(r.Context())
	writeJSON(w, http.StatusOK, status)
}

// --- 2.4: System Init ---

type systemInitRequest struct {
	Harnesses []string `json:"harnesses"`
}

type systemInitResponse struct {
	OK          bool `json:"ok"`
	Initialized bool `json:"initialized"`
}

func (s *Server) handleSystemInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	var req systemInitRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	allowed := map[string]bool{
		"claude":   true,
		"gemini":   true,
		"codex":    true,
		"opencode": true,
	}

	var selected []string
	for _, name := range req.Harnesses {
		if !allowed[name] {
			ValidationError(w, fmt.Sprintf("unknown harness %q", name), nil)
			return
		}
		selected = append(selected, name)
	}

	if len(selected) == 0 {
		ValidationError(w, "at least one harness must be specified", nil)
		return
	}

	// Build harness instances for selected names
	var harnessInstances []api.Harness
	for _, name := range selected {
		harnessInstances = append(harnessInstances, harness.New(name))
	}

	if err := config.InitMachine(harnessInstances); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			fmt.Sprintf("initialization failed: %s", err.Error()), nil)
		return
	}

	writeJSON(w, http.StatusOK, systemInitResponse{
		OK:          true,
		Initialized: true,
	})
}

// trimOutput removes a trailing newline from command output.
func trimOutput(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}
