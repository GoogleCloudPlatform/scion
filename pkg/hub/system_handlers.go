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
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// --- 2.1: System Check (Doctor) ---

type DiagnosticResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "pass", "warn", "fail"
	Message string `json:"message"`
}

type systemCheckResponse struct {
	Results []DiagnosticResult `json:"results"`
	Ready   bool               `json:"ready"`
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
			activeProfile := vs.ActiveProfile
			if activeProfile == "" {
				activeProfile = "default"
			}
			if vs.Profiles != nil {
				if profile, ok := vs.Profiles[activeProfile]; ok {
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

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "cannot determine config directory", nil)
		return
	}

	vs, err := config.LoadSingleFileVersioned(globalDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to load settings", nil)
		return
	}

	activeProfile := vs.ActiveProfile
	if activeProfile == "" {
		activeProfile = "default"
	}

	if vs.Profiles == nil {
		vs.Profiles = make(map[string]config.V1ProfileConfig)
	}
	profile := vs.Profiles[activeProfile]
	profile.Runtime = req.Runtime
	vs.Profiles[activeProfile] = profile

	if err := config.SaveVersionedSettings(globalDir, vs); err != nil {
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
	Initialized         bool   `json:"initialized"`
	IdentitySet         bool   `json:"identitySet"`
	RuntimeOK           bool   `json:"runtimeOK"`
	HarnessesSeeded     bool   `json:"harnessesSeeded"`
	ImagesPresent       bool   `json:"imagesPresent"`
	HasWorkspace        bool   `json:"hasWorkspace"`
	Complete            bool   `json:"complete"`
	EmbeddedBrokerID    string `json:"embeddedBrokerID,omitempty"`
	ImageRegistry       string `json:"imageRegistry,omitempty"`
	BuildAvailable      bool   `json:"buildAvailable"`
	CloudBuildAvailable bool   `json:"cloudBuildAvailable"`
	GCPProjectID        string `json:"gcpProjectId,omitempty"`
	GitVersion          string `json:"gitVersion,omitempty"`
	GitVersionOK        bool   `json:"gitVersionOK"`
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

	status.EmbeddedBrokerID = s.GetEmbeddedBrokerID()

	// ImageRegistry: resolve configured image registry
	if vs, loadErr := config.LoadSingleFileVersioned(globalDir); loadErr == nil && vs != nil {
		status.ImageRegistry = vs.ResolveImageRegistry("")
	}

	// BuildAvailable: true only if the build script can be resolved
	status.BuildAvailable = resolveBuildScript() != ""

	// CloudBuildAvailable: true when a GCP project is configured
	status.CloudBuildAvailable = s.config.GCPProjectID != ""
	if s.config.GCPProjectID != "" {
		status.GCPProjectID = s.config.GCPProjectID
	}

	// GitVersion: report installed git version and whether it meets the worktree requirement
	if gitVersion, _, err := util.GetGitVersion(); err == nil {
		status.GitVersion = gitVersion
		status.GitVersionOK = util.CheckGitVersion() == nil
	} else {
		status.GitVersion = "not found"
		status.GitVersionOK = false
	}

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

// --- 4.1: Image Pull ---

type imagePullRequest struct {
	Harnesses []string `json:"harnesses"`
}

type imagePullResponse struct {
	JobID string `json:"jobId"`
}

func (s *Server) handleSystemImagesPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	if !s.imagePullActive.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, ErrCodeConflict, "a pull is already in progress", nil)
		return
	}

	var req imagePullRequest
	if err := readJSON(r, &req); err != nil {
		s.imagePullActive.Store(false)
		BadRequest(w, "invalid request body")
		return
	}

	if len(req.Harnesses) == 0 {
		s.imagePullActive.Store(false)
		ValidationError(w, "at least one harness must be specified", nil)
		return
	}

	allowed := map[string]bool{"claude": true, "gemini": true, "codex": true, "opencode": true}
	for _, h := range req.Harnesses {
		if !allowed[h] {
			s.imagePullActive.Store(false)
			ValidationError(w, fmt.Sprintf("unknown harness %q", h), nil)
			return
		}
	}

	var registry string
	globalDir, err := config.GetGlobalDir()
	if err == nil {
		if vs, loadErr := config.LoadSingleFileVersioned(globalDir); loadErr == nil && vs != nil {
			registry = vs.ResolveImageRegistry("")
		}
	}
	if registry == "" {
		s.imagePullActive.Store(false)
		writeError(w, http.StatusUnprocessableEntity, ErrCodeUnprocessable, "image_registry is not configured — run 'scion config set --global image_registry <registry>' or reinstall via Homebrew", nil)
		return
	}

	jobID := api.NewUUID()

	rt := runtime.GetRuntime("", "")

	go func() {
		defer s.imagePullActive.Store(false)
		if err := runtime.PullImages(s.ctx, rt, req.Harnesses, registry, func(pr runtime.PullResult) {
			s.events.PublishRaw("system.images."+jobID, pr)
		}); err != nil {
			s.events.PublishRaw("system.images."+jobID, map[string]string{
				"status": "error",
				"error":  err.Error(),
			})
		}
	}()

	writeJSON(w, http.StatusOK, imagePullResponse{JobID: jobID})
}

// --- 4.2: Image Build ---

type imageBuildRequest struct {
	Harnesses []string `json:"harnesses"`
}

type imageBuildLogEvent struct {
	Type string `json:"type"` // "log"
	Line string `json:"line"`
}

func resolveBuildScript() string {
	var path string
	if root := os.Getenv("SCION_ROOT"); root != "" {
		path = filepath.Join(root, "image-build", "scripts", "build-images.sh")
	} else if exe, err := os.Executable(); err == nil {
		path = filepath.Join(filepath.Dir(exe), "..", "image-build", "scripts", "build-images.sh")
	}
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func (s *Server) handleSystemImagesBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	if !s.imageBuildActive.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, ErrCodeConflict, "a build is already in progress", nil)
		return
	}
	buildStarted := false
	defer func() {
		if !buildStarted {
			s.imageBuildActive.Store(false)
		}
	}()

	_, detectErr := config.DetectLocalRuntime()
	if detectErr != nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "no container runtime available", nil)
		return
	}

	var req imageBuildRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if len(req.Harnesses) == 0 {
		ValidationError(w, "at least one harness must be specified", nil)
		return
	}

	allowed := map[string]bool{"claude": true, "gemini": true, "codex": true, "opencode": true}
	for _, h := range req.Harnesses {
		if !allowed[h] {
			ValidationError(w, fmt.Sprintf("unknown harness %q", h), nil)
			return
		}
	}

	buildScript := resolveBuildScript()
	if buildScript == "" {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeUnprocessable, "local builds require a source checkout; use image pull instead", map[string]interface{}{"buildUnavailable": true})
		return
	}

	buildStarted = true
	jobID := api.NewUUID()
	subject := "system.images." + jobID
	requestedHarnesses := req.Harnesses

	go func() {
		defer s.imageBuildActive.Store(false)
		cmd := exec.CommandContext(s.ctx, buildScript, "--target", "harnesses")
		cmd.Dir = filepath.Dir(buildScript)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error("build: stdout pipe failed", "error", err)
			s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: "error: " + err.Error()})
			return
		}
		cmd.Stderr = cmd.Stdout

		if err := cmd.Start(); err != nil {
			slog.Error("build: start failed", "error", err)
			s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: "error: " + err.Error()})
			return
		}

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: scanner.Text()})
		}
		if err := scanner.Err(); err != nil {
			s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: "error reading build log: " + err.Error()})
		}

		if err := cmd.Wait(); err != nil {
			s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: "build failed: " + err.Error()})
		} else {
			for _, h := range requestedHarnesses {
				s.events.PublishRaw(subject, map[string]string{
					"image":  "scion-" + h + ":latest",
					"status": "done",
				})
			}
			s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: "build complete"})
		}
	}()

	writeJSON(w, http.StatusOK, imagePullResponse{JobID: jobID})
}

// --- 5.2: Filesystem Endpoints ---

type fsListEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	IsGit bool   `json:"isGit,omitempty"`
}

type fsListResponse struct {
	Path    string        `json:"path"`
	Entries []fsListEntry `json:"entries"`
}

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "cannot determine home directory", nil)
			return
		}
		dirPath = home
	}

	resolved, err := filepath.EvalSymlinks(dirPath)
	if err != nil {
		resolved = filepath.Clean(dirPath)
	} else {
		resolved = filepath.Clean(resolved)
	}
	if !filepath.IsAbs(resolved) {
		if resolved, err = filepath.Abs(resolved); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "cannot resolve absolute path", nil)
			return
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "cannot determine home directory", nil)
		return
	}
	sep := string(filepath.Separator)
	if !pathEqual(resolved, home) && !pathHasPrefix(resolved, home+sep) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "path must be within the home directory", nil)
		return
	}

	rawEntries, err := os.ReadDir(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "directory not found", nil)
			return
		}
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "cannot read directory", nil)
		return
	}

	var entries []fsListEntry
	for _, e := range rawEntries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		entry := fsListEntry{
			Name:  name,
			IsDir: e.IsDir(),
		}
		if e.IsDir() {
			gitPath := filepath.Join(resolved, name, ".git")
			if _, err := os.Stat(gitPath); err == nil {
				entry.IsGit = true
			}
		}
		entries = append(entries, entry)
	}

	writeJSON(w, http.StatusOK, fsListResponse{
		Path:    resolved,
		Entries: entries,
	})
}

type fsMkdirRequest struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
}

type fsMkdirResponse struct {
	Path string `json:"path"`
}

func (s *Server) handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	var req fsMkdirRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if req.Parent == "" || req.Name == "" {
		ValidationError(w, "parent and name are required", nil)
		return
	}

	if strings.ContainsAny(req.Name, "/\\") || req.Name == "." || req.Name == ".." {
		ValidationError(w, "name must not contain path separators or be . or ..", nil)
		return
	}

	resolved, err := filepath.EvalSymlinks(req.Parent)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "parent directory does not exist", nil)
		return
	}
	resolved = filepath.Clean(resolved)
	if !filepath.IsAbs(resolved) {
		if resolved, err = filepath.Abs(resolved); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "cannot resolve absolute path", nil)
			return
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "cannot determine home directory", nil)
		return
	}
	sep := string(filepath.Separator)
	if !pathEqual(resolved, home) && !pathHasPrefix(resolved, home+sep) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "parent must be within the home directory", nil)
		return
	}

	managedRoot, _ := managedProjectRoot()
	if managedRoot != "" {
		cleanManaged := filepath.Clean(managedRoot)
		if pathHasPrefix(resolved, cleanManaged+string(filepath.Separator)) || pathEqual(resolved, cleanManaged) {
			ValidationError(w, "cannot create directories inside the Scion managed directory", nil)
			return
		}
	}

	newPath := filepath.Join(resolved, req.Name)
	if err := os.Mkdir(newPath, 0755); err != nil {
		if os.IsExist(err) {
			writeError(w, http.StatusConflict, ErrCodeConflict, "directory already exists", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to create directory", nil)
		return
	}

	writeJSON(w, http.StatusOK, fsMkdirResponse{Path: newPath})
}

type fsValidatePathRequest struct {
	Path string `json:"path"`
}

type fsValidatePathResponse struct {
	PathClass
	Error string `json:"error,omitempty"`
}

// handleFSValidatePath classifies a candidate path for linked-grove creation.
// Intentionally no home-directory fence: linked groves may reference directories
// anywhere on disk (e.g. external drives, /opt projects). Safety is enforced by
// ClassifyPath's managed-path overlap check and the assertLoopback guard.
func (s *Server) handleFSValidatePath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	var req fsValidatePathRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if req.Path == "" {
		ValidationError(w, "path is required", nil)
		return
	}

	managedRoot, _ := managedProjectRoot()

	pc, err := ClassifyPath(r.Context(), s.store, req.Path, managedRoot)
	if err != nil {
		slog.Warn("fs/validate-path: classify error", "path", req.Path, "error", err)
	}

	resp := fsValidatePathResponse{PathClass: pc}

	if pc.IsManaged {
		resp.Error = "This path is inside the Scion managed directory and cannot be linked"
	}

	writeJSON(w, http.StatusOK, resp)
}

// --- Apple DNS ---

type appleDNSResponse struct {
	Configured bool   `json:"configured"`
	Hostname   string `json:"hostname"`
	IP         string `json:"ip"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) handleAppleDNS(w http.ResponseWriter, r *http.Request) {
	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleAppleDNSStatus(w, r)
	case http.MethodPost:
		s.handleAppleDNSSetup(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleAppleDNSStatus(w http.ResponseWriter, r *http.Request) {
	exists, err := runtime.AppleDNSRuleExists(r.Context(), runtime.AppleDNSHostname)
	resp := appleDNSResponse{
		Configured: err == nil && exists,
		Hostname:   runtime.AppleDNSHostname,
		IP:         runtime.AppleDNSIP,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAppleDNSSetup(w http.ResponseWriter, r *http.Request) {
	_, err := runtime.EnsureAppleDNS(r.Context(), runtime.AppleDNSHostname, runtime.AppleDNSIP)
	resp := appleDNSResponse{
		Configured: err == nil,
		Hostname:   runtime.AppleDNSHostname,
		IP:         runtime.AppleDNSIP,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// trimOutput removes a trailing newline from command output.
func trimOutput(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}
