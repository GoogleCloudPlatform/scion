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
	// Format: "workspaces/{groveId}/{slug}"
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
	// Format: "workspaces/{groveId}/{slug}"
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

// handleWorkspaceUpload handles POST /api/v1/workspace/upload
// It uploads the agent's workspace directory to GCS.
func (s *Server) handleWorkspaceUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
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
		RuntimeError(w, "Failed to resolve workspace path: "+err.Error())
		return
	}

	// Build manifest from container workspace
	manifest, err := s.buildWorkspaceManifest(workspacePath, req.ExcludePatterns)
	if err != nil {
		RuntimeError(w, "Failed to build workspace manifest: "+err.Error())
		return
	}

	// Sync workspace to GCS using rclone
	filesPath := req.StoragePath + "/files"
	if err := gcp.SyncToGCS(ctx, workspacePath, bucket, filesPath); err != nil {
		RuntimeError(w, "Failed to upload workspace to GCS: "+err.Error())
		return
	}

	// Upload the manifest
	if err := s.uploadManifest(ctx, bucket, req.StoragePath, manifest); err != nil {
		RuntimeError(w, "Failed to upload manifest: "+err.Error())
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
		MethodNotAllowed(w)
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
		RuntimeError(w, "Failed to resolve workspace path: "+err.Error())
		return
	}

	// Sync workspace from GCS to local using rclone
	filesPath := req.StoragePath + "/files"
	if err := gcp.SyncFromGCS(ctx, bucket, filesPath, workspacePath); err != nil {
		RuntimeError(w, "Failed to download workspace from GCS: "+err.Error())
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
	var grovePath string
	var agentName string

	for _, agent := range agents {
		if agent.Name == agentID || agent.ContainerID == agentID || agent.Slug == agentID || strings.EqualFold(agent.Name, agentID) {
			containerID = agent.ContainerID
			grovePath = agent.ProjectPath
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
				return workspacePath, nil
			}
		}
	}

	// Fall back to worktree location pattern
	// Worktrees are typically at: {parent}/.scion_worktrees/{grove}/{agent}
	if grovePath != "" && agentName != "" {
		// Get grove's parent directory
		groveParent := filepath.Dir(grovePath)
		groveName := filepath.Base(groveParent)

		worktreePath := filepath.Join(groveParent, ".scion_worktrees", groveName, agentName)
		if _, statErr := os.Stat(worktreePath); statErr == nil {
			return worktreePath, nil
		}
	}

	// Try the configured worktree base if available
	if s.config.WorktreeBase != "" && agentName != "" {
		worktreePath := filepath.Join(s.config.WorktreeBase, agentName)
		if _, statErr := os.Stat(worktreePath); statErr == nil {
			return worktreePath, nil
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
func (s *Server) uploadManifest(ctx context.Context, bucket, storagePath string, manifest *transfer.Manifest) error {
	// Create storage client
	cfg := storage.Config{
		Provider: storage.ProviderGCS,
		Bucket:   bucket,
	}

	store, err := storage.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to create storage client: %w", err)
	}
	defer store.Close()

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

	filepath.WalkDir(workspacePath, func(path string, d fs.DirEntry, err error) error {
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

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (r *ProjectWorkspaceUploadRequest) UnmarshalJSON(data []byte) error {
	type Alias ProjectWorkspaceUploadRequest
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.GroveID != "" {
		r.ProjectID = aux.GroveID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (r ProjectWorkspaceUploadRequest) MarshalJSON() ([]byte, error) {
	type Alias ProjectWorkspaceUploadRequest
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(r),
		GroveID: r.ProjectID,
	})
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

// handleProjectWorkspaceUpload handles POST /api/v1/workspace/project-upload (and legacy grove-upload)
// It uploads the project's workspace directory to GCS so the hub can cache it.
func (s *Server) handleProjectWorkspaceUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
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

	// Verify workspace path exists
	if _, err := os.Stat(req.WorkspacePath); err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "Project workspace path")
			return
		}
		RuntimeError(w, "Failed to access workspace path: "+err.Error())
		return
	}

	if s.config.Debug {
		slog.Debug("Project workspace upload requested",
			"groveId", req.ProjectID,
			"bucket", bucket,
			"storagePath", req.StoragePath,
			"workspacePath", req.WorkspacePath,
		)
	}

	// Build manifest from grove workspace
	manifest, err := s.buildWorkspaceManifest(req.WorkspacePath, req.ExcludePatterns)
	if err != nil {
		RuntimeError(w, "Failed to build workspace manifest: "+err.Error())
		return
	}

	// Sync workspace to GCS using rclone
	filesPath := req.StoragePath + "/files"
	if err := gcp.SyncToGCS(ctx, req.WorkspacePath, bucket, filesPath); err != nil {
		RuntimeError(w, "Failed to upload workspace to GCS: "+err.Error())
		return
	}

	// Upload the manifest
	if err := s.uploadManifest(ctx, bucket, req.StoragePath, manifest); err != nil {
		RuntimeError(w, "Failed to upload manifest: "+err.Error())
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
			"groveId", req.ProjectID,
			"files", resp.UploadedFiles,
			"bytes", resp.UploadedBytes,
		)
	}

	writeJSON(w, http.StatusOK, resp)
}
