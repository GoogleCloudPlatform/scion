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
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// ============================================================================
// Workspace Request/Response Types
// ============================================================================

// WorkspaceUploadRequest is the request body for uploading workspace to GCS.
type WorkspaceUploadRequest struct {
	// Slug is the identifier of the agent whose workspace to upload.
	Slug string `json:"slug"`
	// StoragePath is the path within the bucket where files should be uploaded.
	// Format: "workspaces/{projectId}/{slug}"
	StoragePath string `json:"storagePath"`
	// Bucket is the GCS bucket name for storage.
	Bucket string `json:"bucket,omitempty"`
	// ExcludePatterns are glob patterns to exclude from the upload.
	ExcludePatterns []string `json:"excludePatterns,omitempty"`
}

// WorkspaceUploadResponse is the response after uploading workspace to GCS.
type WorkspaceUploadResponse struct {
	// Manifest contains the list of files uploaded with their hashes.
	Manifest *transfer.Manifest `json:"manifest"`
	// UploadedFiles is the number of files uploaded.
	UploadedFiles int `json:"uploadedFiles"`
	// UploadedBytes is the total size of uploaded files.
	UploadedBytes int64 `json:"uploadedBytes"`
}

// WorkspaceApplyRequest is the request body for applying workspace from GCS.
type WorkspaceApplyRequest struct {
	// Slug is the identifier of the agent whose workspace to update.
	Slug string `json:"slug"`
	// StoragePath is the path within the bucket where files are stored.
	// Format: "workspaces/{projectId}/{slug}"
	StoragePath string `json:"storagePath"`
	// Bucket is the GCS bucket name for storage.
	Bucket string `json:"bucket,omitempty"`
	// Manifest contains the list of files to apply with their modes.
	Manifest *transfer.Manifest `json:"manifest,omitempty"`
}

// WorkspaceApplyResponse is the response after applying workspace from GCS.
type WorkspaceApplyResponse struct {
	// Applied indicates whether the workspace was successfully applied.
	Applied bool `json:"applied"`
	// FilesApplied is the number of files applied.
	FilesApplied int `json:"filesApplied"`
	// BytesTransferred is the total size of transferred files.
	BytesTransferred int64 `json:"bytesTransferred"`
}

// ============================================================================
// Workspace Handlers
// ============================================================================

// Workspace transfer ops (writeRuntimeOpError): a failed step answers with
// the fixed "Failed to <op>" text only. The cause's own text stays in the
// broker log, since it can name broker filesystem paths or GCS bucket,
// object or credential detail (ptone/scion#3496). The GCS download op is
// opDownloadWorkspace, shared with the create-time bootstrap.
const (
	opResolveWorkspacePath   = "resolve workspace path"
	opAccessWorkspacePath    = "access workspace path"
	opBuildWorkspaceManifest = "build workspace manifest"
	opUploadWorkspace        = "upload workspace to GCS"
	opUploadManifest         = "upload manifest"
)

// handleWorkspaceUpload handles POST /api/v1/workspace/upload
// It uploads the agent's workspace directory to GCS.
func (s *Server) handleWorkspaceUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	var req WorkspaceUploadRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Slug == "" {
		ValidationError(w, "slug is required", nil)
		return
	}
	if req.StoragePath == "" {
		ValidationError(w, "storagePath is required", nil)
		return
	}

	// Get bucket from request or config
	bucket := req.Bucket
	if bucket == "" {
		bucket = s.config.StorageBucket
	}
	if bucket == "" {
		ValidationError(w, "bucket is required (not configured on broker)", nil)
		return
	}

	if s.config.Debug {
		slog.Debug("Workspace upload requested",
			"slug", req.Slug,
			"bucket", bucket,
			"storagePath", req.StoragePath,
		)
	}

	// Get the container's workspace path
	workspacePath, err := s.getAgentWorkspacePath(ctx, req.Slug)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		s.writeRuntimeOpError(w, ctx, opResolveWorkspacePath, err, "agent_slug", req.Slug)
		return
	}

	// Build manifest from container workspace
	manifest, err := s.buildWorkspaceManifest(workspacePath, req.ExcludePatterns)
	if err != nil {
		s.writeRuntimeOpError(w, ctx, opBuildWorkspaceManifest, err, "agent_slug", req.Slug)
		return
	}

	// Sync workspace to GCS using rclone
	filesPath := req.StoragePath + "/files"
	if err := s.workspaceUploader()(ctx, workspacePath, bucket, filesPath); err != nil {
		s.writeRuntimeOpError(w, ctx, opUploadWorkspace, err, "agent_slug", req.Slug)
		return
	}

	// Upload the manifest
	if err := s.manifestUploader()(ctx, bucket, req.StoragePath, manifest); err != nil {
		s.writeRuntimeOpError(w, ctx, opUploadManifest, err, "agent_slug", req.Slug)
		return
	}

	// Calculate response stats
	var totalBytes int64
	for _, f := range manifest.Files {
		totalBytes += f.Size
	}

	resp := WorkspaceUploadResponse{
		Manifest:      manifest,
		UploadedFiles: len(manifest.Files),
		UploadedBytes: totalBytes,
	}

	if s.config.Debug {
		slog.Debug("Workspace upload complete",
			"files", resp.UploadedFiles,
			"bytes", resp.UploadedBytes,
		)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleWorkspaceApply handles POST /api/v1/workspace/apply
// It downloads files from GCS and applies them to the agent's workspace.
func (s *Server) handleWorkspaceApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	var req WorkspaceApplyRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Slug == "" {
		ValidationError(w, "slug is required", nil)
		return
	}
	if req.StoragePath == "" {
		ValidationError(w, "storagePath is required", nil)
		return
	}

	// Get bucket from request or config
	bucket := req.Bucket
	if bucket == "" {
		bucket = s.config.StorageBucket
	}
	if bucket == "" {
		ValidationError(w, "bucket is required (not configured on broker)", nil)
		return
	}

	if s.config.Debug {
		slog.Debug("Workspace apply requested",
			"slug", req.Slug,
			"bucket", bucket,
			"storagePath", req.StoragePath,
		)
	}

	// Get the container's workspace path
	workspacePath, err := s.getAgentWorkspacePath(ctx, req.Slug)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		s.writeRuntimeOpError(w, ctx, opResolveWorkspacePath, err, "agent_slug", req.Slug)
		return
	}

	// Sync workspace from GCS to local using rclone
	filesPath := req.StoragePath + "/files"
	if err := s.workspaceDownloader()(ctx, bucket, filesPath, workspacePath); err != nil {
		s.writeRuntimeOpError(w, ctx, opDownloadWorkspace, err, "agent_slug", req.Slug)
		return
	}

	// Apply file permissions if manifest is provided
	var filesApplied int
	var bytesTransferred int64
	if req.Manifest != nil && len(req.Manifest.Files) > 0 {
		if err := s.applyFilePermissions(workspacePath, req.Manifest.Files); err != nil {
			// Log but don't fail - permissions are optional
			slog.Warn("Failed to apply file permissions", "error", err)
		}
		filesApplied = len(req.Manifest.Files)
		for _, f := range req.Manifest.Files {
			bytesTransferred += f.Size
		}
	} else {
		// Count files in workspace if no manifest
		filesApplied, bytesTransferred = s.countWorkspaceFiles(workspacePath)
	}

	resp := WorkspaceApplyResponse{
		Applied:          true,
		FilesApplied:     filesApplied,
		BytesTransferred: bytesTransferred,
	}

	if s.config.Debug {
		slog.Debug("Workspace apply complete",
			"files", resp.FilesApplied,
			"bytes", resp.BytesTransferred,
		)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ============================================================================
// Helper Functions
// ============================================================================

// getAgentWorkspacePath resolves the workspace path for an agent.
// It first tries to find the container and inspect its volume mounts,
// then falls back to the known worktree location pattern.
func (s *Server) getAgentWorkspacePath(ctx context.Context, agentID string) (string, error) {
	// First, try to find the agent in the manager
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		return "", fmt.Errorf("failed to list agents: %w", err)
	}

	var containerID string
	var projectPath string
	var agentName string

	for _, agent := range agents {
		if agent.Name == agentID || agent.ContainerID == agentID || agent.Slug == agentID || strings.EqualFold(agent.Name, agentID) {
			containerID = agent.ContainerID
			projectPath = agent.ProjectPath
			agentName = agent.Name
			break
		}
	}

	if containerID == "" {
		return "", fmt.Errorf("agent not found: %s", agentID)
	}

	// Try to get workspace from runtime (Docker volume mounts)
	if s.runtime != nil {
		workspacePath, err := s.runtime.GetWorkspacePath(ctx, containerID)
		if err == nil && workspacePath != "" {
			// Verify the path exists
			if _, statErr := os.Stat(workspacePath); statErr == nil {
				// No per-project root is available for this branch: the
				// value comes from the runtime layer, which already
				// validates a workspace source at the point it becomes a
				// mount (pkg/agent Start(), buildCommonRunArgs). This is a
				// second gate before it's used as an upload source or an
				// apply destination, using the resolved, symlink-free path
				// it returns; only the fixed deny-set applies here.
				resolved, verr := runtime.ValidateWorkspaceSource(workspacePath, "")
				if verr != nil {
					return "", verr
				}
				return resolved, nil
			}
		}
	}

	// Fall back to worktree location pattern
	// Worktrees are typically at: {parent}/.scion_worktrees/{project}/{agent}
	if projectPath != "" && agentName != "" {
		// Get project's parent directory
		projectParent := filepath.Dir(projectPath)
		projectName := filepath.Base(projectParent)

		worktreePath := filepath.Join(projectParent, ".scion_worktrees", projectName, agentName)
		if _, statErr := os.Stat(worktreePath); statErr == nil {
			// worktreePath is constructed directly under projectParent, so
			// that's the natural root for the containment check.
			resolved, verr := runtime.ValidateWorkspaceSource(worktreePath, projectParent)
			if verr != nil {
				return "", verr
			}
			return resolved, nil
		}
	}

	// Try the configured worktree base if available
	if s.config.WorktreeBase != "" && agentName != "" {
		worktreePath := filepath.Join(s.config.WorktreeBase, agentName)
		if _, statErr := os.Stat(worktreePath); statErr == nil {
			// worktreePath is constructed directly under WorktreeBase, so
			// that's the natural root for the containment check.
			resolved, verr := runtime.ValidateWorkspaceSource(worktreePath, s.config.WorktreeBase)
			if verr != nil {
				return "", verr
			}
			return resolved, nil
		}
	}

	return "", fmt.Errorf("could not resolve workspace path for agent %s", agentID)
}

// buildWorkspaceManifest builds a manifest from the workspace directory.
func (s *Server) buildWorkspaceManifest(workspacePath string, excludePatterns []string) (*transfer.Manifest, error) {
	builder := transfer.NewManifestBuilder(workspacePath)

	// Add custom exclude patterns
	if len(excludePatterns) > 0 {
		builder.WithExcludePatterns(excludePatterns)
	}

	return builder.Build()
}

// uploadManifest uploads the workspace manifest to GCS.
func uploadManifest(ctx context.Context, bucket, storagePath string, manifest *transfer.Manifest) error {
	// Create storage client
	cfg := storage.Config{
		Provider: storage.ProviderGCS,
		Bucket:   bucket,
	}

	store, err := storage.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to create storage client: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Serialize manifest to JSON
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	// Upload manifest
	manifestPath := storagePath + "/manifest.json"
	_, err = store.Upload(ctx, manifestPath, strings.NewReader(string(manifestJSON)), storage.UploadOptions{
		ContentType: "application/json",
	})
	if err != nil {
		return fmt.Errorf("failed to upload manifest: %w", err)
	}

	return nil
}

// applyFilePermissions applies file mode permissions from the manifest.
func (s *Server) applyFilePermissions(workspacePath string, files []transfer.FileInfo) error {
	var firstErr error

	for _, file := range files {
		if file.Mode == "" {
			continue
		}

		fullPath := filepath.Join(workspacePath, file.Path)

		// Parse octal mode string (e.g., "0644")
		var mode fs.FileMode
		if _, err := fmt.Sscanf(file.Mode, "%o", &mode); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("invalid mode %s for %s: %w", file.Mode, file.Path, err)
			}
			continue
		}

		if err := os.Chmod(fullPath, mode); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to chmod %s: %w", file.Path, err)
			}
			continue
		}
	}

	return firstErr
}

// countWorkspaceFiles counts files and total size in the workspace directory.
func (s *Server) countWorkspaceFiles(workspacePath string) (int, int64) {
	var count int
	var totalSize int64

	_ = filepath.WalkDir(workspacePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}

		count++
		if info, err := d.Info(); err == nil {
			totalSize += info.Size()
		}

		return nil
	})

	return count, totalSize
}

// ============================================================================
// Project-Level Workspace Upload (Phase 3: Linked Project Relay)
// ============================================================================

// ProjectWorkspaceUploadRequest is the request body for uploading a project's
// workspace (not an individual agent's workspace) to GCS.
// This is used by the hub to populate its cached copy of a linked project.
type ProjectWorkspaceUploadRequest struct {
	// ProjectID is the project identifier.
	ProjectID string `json:"projectId"`
	// StoragePath is the path within the bucket where files should be uploaded.
	StoragePath string `json:"storagePath"`
	// WorkspacePath is the local filesystem path to the project workspace on this broker.
	// Provided by the hub from the ProjectProvider.LocalPath.
	WorkspacePath string `json:"workspacePath"`
	// Bucket is the GCS bucket name for storage.
	Bucket string `json:"bucket,omitempty"`
	// ExcludePatterns are glob patterns to exclude from the upload.
	ExcludePatterns []string `json:"excludePatterns,omitempty"`
}

// ProjectWorkspaceUploadResponse is the response after uploading a project workspace.
type ProjectWorkspaceUploadResponse struct {
	// Manifest contains the list of files uploaded with their hashes.
	Manifest *transfer.Manifest `json:"manifest"`
	// UploadedFiles is the number of files uploaded.
	UploadedFiles int `json:"uploadedFiles"`
	// UploadedBytes is the total size of uploaded files.
	UploadedBytes int64 `json:"uploadedBytes"`
}

// handleProjectWorkspaceUpload handles POST /api/v1/workspace/project-upload.
// It uploads the project's workspace directory to GCS so the hub can cache it.
func (s *Server) handleProjectWorkspaceUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	var req ProjectWorkspaceUploadRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.ProjectID == "" {
		ValidationError(w, "projectId is required", nil)
		return
	}
	if req.StoragePath == "" {
		ValidationError(w, "storagePath is required", nil)
		return
	}
	if req.WorkspacePath == "" {
		ValidationError(w, "workspacePath is required", nil)
		return
	}

	// Get bucket from request or config
	bucket := req.Bucket
	if bucket == "" {
		bucket = s.config.StorageBucket
	}
	if bucket == "" {
		ValidationError(w, "bucket is required (not configured on broker)", nil)
		return
	}

	// Reject a workspace path that is not an allowed workspace path before
	// touching the filesystem at all. req.WorkspacePath is caller
	// (request-body) supplied, not derived from an agent lookup, so this is
	// the only gate it goes through. No per-project root is available at
	// this call site — req.ProjectID identifies the project but not a
	// filesystem location — so only the fixed deny-set (and its named
	// ~/.scion allow list) applies. This must run before any os.Stat: a
	// relative path must be refused outright rather than resolved against
	// the broker's own working directory, and a rejected path must produce
	// the same response whether or not it exists (an os.Stat run first
	// would answer that through the choice between a 404 and a 500). A
	// request-body validation failure is the caller's bad input, not a
	// broker runtime failure, so it is reported the same way as the
	// required-field checks above (400), not as a 500.
	resolvedWorkspacePath, err := runtime.ValidateWorkspaceSource(req.WorkspacePath)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	req.WorkspacePath = resolvedWorkspacePath

	// Verify the resolved workspace path exists.
	if _, err := s.projectWorkspaceStatter()(req.WorkspacePath); err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "Project workspace path")
			return
		}
		s.writeRuntimeOpError(w, ctx, opAccessWorkspacePath, err, "project_id", req.ProjectID)
		return
	}

	if s.config.Debug {
		slog.Debug("Project workspace upload requested",
			"projectId", req.ProjectID,
			"bucket", bucket,
			"storagePath", req.StoragePath,
			"workspacePath", req.WorkspacePath,
		)
	}

	// Build manifest from project workspace
	manifest, err := s.buildWorkspaceManifest(req.WorkspacePath, req.ExcludePatterns)
	if err != nil {
		s.writeRuntimeOpError(w, ctx, opBuildWorkspaceManifest, err, "project_id", req.ProjectID)
		return
	}

	// Sync workspace to GCS using rclone
	filesPath := req.StoragePath + "/files"
	if err := s.workspaceUploader()(ctx, req.WorkspacePath, bucket, filesPath); err != nil {
		s.writeRuntimeOpError(w, ctx, opUploadWorkspace, err, "project_id", req.ProjectID)
		return
	}

	// Upload the manifest
	if err := s.manifestUploader()(ctx, bucket, req.StoragePath, manifest); err != nil {
		s.writeRuntimeOpError(w, ctx, opUploadManifest, err, "project_id", req.ProjectID)
		return
	}

	// Calculate response stats
	var totalBytes int64
	for _, f := range manifest.Files {
		totalBytes += f.Size
	}

	resp := ProjectWorkspaceUploadResponse{
		Manifest:      manifest,
		UploadedFiles: len(manifest.Files),
		UploadedBytes: totalBytes,
	}

	if s.config.Debug {
		slog.Debug("Project workspace upload complete",
			"projectId", req.ProjectID,
			"files", resp.UploadedFiles,
			"bytes", resp.UploadedBytes,
		)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ============================================================================
// Test seams
// ============================================================================
//
// Each step below follows SetWorkspaceDownloader/workspaceDownloader: a
// setter replaces the step on this Server (nil restores the default), and
// the accessor returns the replacement or the real implementation. They
// exist so tests can fail one step without real GCS or filesystem faults.

// setWorkspaceUploader replaces the GCS upload of a workspace directory.
func (s *Server) setWorkspaceUploader(fn func(ctx context.Context, localPath, bucket, prefix string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaceUpload = fn
}

// workspaceUploader returns the GCS upload of a workspace directory.
func (s *Server) workspaceUploader() func(ctx context.Context, localPath, bucket, prefix string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.workspaceUpload != nil {
		return s.workspaceUpload
	}
	return gcp.SyncToGCS
}

// setManifestUploader replaces the upload of a workspace manifest.
func (s *Server) setManifestUploader(fn func(ctx context.Context, bucket, storagePath string, manifest *transfer.Manifest) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifestUpload = fn
}

// manifestUploader returns the upload of a workspace manifest.
func (s *Server) manifestUploader() func(ctx context.Context, bucket, storagePath string, manifest *transfer.Manifest) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.manifestUpload != nil {
		return s.manifestUpload
	}
	return uploadManifest
}

// setProjectWorkspaceStatter replaces the project upload's existence
// check, so a test can produce a stat error other than not-exist (path
// validation refuses every one it can cause on a real filesystem first).
func (s *Server) setProjectWorkspaceStatter(fn func(path string) (os.FileInfo, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectWorkspaceStat = fn
}

// projectWorkspaceStatter returns the project upload's existence check.
func (s *Server) projectWorkspaceStatter() func(path string) (os.FileInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.projectWorkspaceStat != nil {
		return s.projectWorkspaceStat
	}
	return os.Stat
}

// setProjectAbs replaces deleteProject's filepath.Abs, which cannot fail
// for the absolute paths it is given.
func (s *Server) setProjectAbs(fn func(path string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectPathAbs = fn
}

// projectAbs returns deleteProject's filepath.Abs.
func (s *Server) projectAbs() func(path string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.projectPathAbs != nil {
		return s.projectPathAbs
	}
	return filepath.Abs
}
