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
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/storage"
	"github.com/ptone/scion-agent/pkg/transfer"
	"github.com/ptone/scion-agent/pkg/wsprotocol"
)

// Workspace sync request/response types following the design in sync-design.md Section 7.

// SyncFromRequest is the request body for initiating a workspace sync from an agent.
type SyncFromRequest struct {
	// ExcludePatterns are glob patterns to exclude from the sync (e.g., ".git/**").
	ExcludePatterns []string `json:"excludePatterns,omitempty"`
}

// SyncFromResponse is the response for a workspace sync-from operation.
type SyncFromResponse struct {
	// Manifest contains the file manifest from the agent workspace.
	Manifest *transfer.Manifest `json:"manifest"`
	// DownloadURLs contains signed URLs for downloading each file.
	DownloadURLs []transfer.DownloadURLInfo `json:"downloadUrls"`
	// Expires is when the signed URLs expire.
	Expires time.Time `json:"expires"`
}

// SyncToRequest is the request body for initiating a workspace sync to an agent.
type SyncToRequest struct {
	// Files lists the files to be uploaded with their metadata.
	Files []transfer.FileInfo `json:"files"`
}

// SyncToResponse is the response for a workspace sync-to initiation.
type SyncToResponse struct {
	// UploadURLs contains signed URLs for uploading files.
	UploadURLs []transfer.UploadURLInfo `json:"uploadUrls"`
	// ExistingFiles lists file paths that already exist with matching hashes (skip upload).
	ExistingFiles []string `json:"existingFiles"`
	// Expires is when the signed URLs expire.
	Expires time.Time `json:"expires"`
}

// SyncToFinalizeRequest is the request body for finalizing a workspace sync-to operation.
type SyncToFinalizeRequest struct {
	// Manifest contains the complete file manifest for the workspace.
	Manifest *transfer.Manifest `json:"manifest"`
}

// SyncToFinalizeResponse is the response for finalizing a workspace sync-to operation.
type SyncToFinalizeResponse struct {
	// Applied indicates whether the workspace was successfully applied.
	Applied bool `json:"applied"`
	// ContentHash is the computed hash of the workspace content.
	ContentHash string `json:"contentHash,omitempty"`
	// FilesApplied is the number of files applied to the workspace.
	FilesApplied int `json:"filesApplied"`
	// BytesTransferred is the total bytes transferred.
	BytesTransferred int64 `json:"bytesTransferred"`
}

// WorkspaceStatusResponse is the response for getting workspace sync status.
type WorkspaceStatusResponse struct {
	// Slug is the agent's URL-safe identifier.
	Slug string `json:"slug"`
	// GroveID is the grove ID.
	GroveID string `json:"groveId"`
	// StorageURI is the GCS URI for the workspace storage.
	StorageURI string `json:"storageUri"`
	// LastSync contains information about the last sync operation.
	LastSync *WorkspaceSyncInfo `json:"lastSync,omitempty"`
}

// WorkspaceSyncInfo contains information about a sync operation.
type WorkspaceSyncInfo struct {
	// Direction is the sync direction ("from" or "to").
	Direction string `json:"direction"`
	// Timestamp is when the sync occurred.
	Timestamp time.Time `json:"timestamp"`
	// ContentHash is the content hash of the synced workspace.
	ContentHash string `json:"contentHash,omitempty"`
	// FileCount is the number of files synced.
	FileCount int `json:"fileCount"`
	// TotalSize is the total size of synced files.
	TotalSize int64 `json:"totalSize"`
}

// handleWorkspaceRoutes dispatches workspace-related actions.
// action should be one of: "", "sync-from", "sync-to", "sync-to/finalize"
func (s *Server) handleWorkspaceRoutes(w http.ResponseWriter, r *http.Request, agentID, action string) {
	switch action {
	case "":
		// GET /api/v1/agents/{id}/workspace - Get workspace status
		if r.Method == http.MethodGet {
			s.handleWorkspaceStatus(w, r, agentID)
		} else {
			MethodNotAllowed(w)
		}
	case "sync-from":
		// POST /api/v1/agents/{id}/workspace/sync-from - Initiate sync from agent
		if r.Method == http.MethodPost {
			s.handleWorkspaceSyncFrom(w, r, agentID)
		} else {
			MethodNotAllowed(w)
		}
	case "sync-to":
		// POST /api/v1/agents/{id}/workspace/sync-to - Initiate sync to agent
		if r.Method == http.MethodPost {
			s.handleWorkspaceSyncTo(w, r, agentID)
		} else {
			MethodNotAllowed(w)
		}
	case "sync-to/finalize":
		// POST /api/v1/agents/{id}/workspace/sync-to/finalize - Finalize sync to agent
		if r.Method == http.MethodPost {
			s.handleWorkspaceSyncToFinalize(w, r, agentID)
		} else {
			MethodNotAllowed(w)
		}
	default:
		NotFound(w, "Workspace action")
	}
}

// handleWorkspaceStatus returns the current workspace sync status.
// GET /api/v1/agents/{id}/workspace
func (s *Server) handleWorkspaceStatus(w http.ResponseWriter, r *http.Request, agentID string) {
	ctx := r.Context()

	// Validate agent exists
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Get storage for URI generation
	stor := s.GetStorage()
	storageURI := ""
	if stor != nil {
		storageURI = storage.WorkspaceStorageURI(stor.Bucket(), agent.GroveID, agentID)
	}

	// TODO: Fetch last sync info from storage metadata
	// For now, return basic status
	writeJSON(w, http.StatusOK, WorkspaceStatusResponse{
		Slug:       agentID, // agentID parameter is the URL slug
		GroveID:    agent.GroveID,
		StorageURI: storageURI,
		LastSync:   nil, // Will be populated in Phase 4
	})
}

// handleWorkspaceSyncFrom initiates a workspace sync from an agent.
// POST /api/v1/agents/{id}/workspace/sync-from
//
// This endpoint:
// 1. Validates the agent exists and is running
// 2. Tunnels a request to the Runtime Broker to upload workspace to GCS
// 3. Returns signed download URLs for the CLI to fetch files
func (s *Server) handleWorkspaceSyncFrom(w http.ResponseWriter, r *http.Request, agentID string) {
	ctx := r.Context()

	// Parse optional request body
	var req SyncFromRequest
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
	}

	// Validate agent exists
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Check agent is running
	if agent.Status != "running" {
		Conflict(w, "Agent is not running")
		return
	}

	// Check storage is configured
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Get workspace storage path
	storagePath := storage.WorkspaceStoragePath(agent.GroveID, agentID)

	// Tunnel request to Runtime Broker to upload workspace to GCS
	cc := s.GetControlChannelManager()
	if cc == nil {
		RuntimeError(w, "Control channel not available")
		return
	}

	// Build request for Runtime Broker
	uploadReq := RuntimeBrokerWorkspaceUploadRequest{
		Slug:            agentID, // agentID parameter is the URL slug
		StoragePath:     storagePath,
		ExcludePatterns: req.ExcludePatterns,
	}

	// Send tunneled request to Runtime Broker
	var uploadResp RuntimeBrokerWorkspaceUploadResponse
	if err := tunnelWorkspaceRequest(ctx, cc, agent.RuntimeBrokerID, "POST", "/api/v1/workspace/upload", uploadReq, &uploadResp); err != nil {
		// Check if it's a timeout or connection issue
		if strings.Contains(err.Error(), "timeout") {
			GatewayTimeout(w, "Runtime Broker unreachable")
			return
		}
		RuntimeError(w, "Failed to sync workspace: "+err.Error())
		return
	}

	// Generate signed download URLs for each file
	expires := time.Now().Add(SignedURLExpiry)
	downloadURLs := make([]transfer.DownloadURLInfo, 0, len(uploadResp.Manifest.Files))

	for _, file := range uploadResp.Manifest.Files {
		objectPath := storagePath + "/files/" + file.Path
		signedURL, err := stor.GenerateSignedURL(ctx, objectPath, storage.SignedURLOptions{
			Method:  "GET",
			Expires: SignedURLExpiry,
		})
		if err != nil {
			RuntimeError(w, "Failed to generate download URL: "+err.Error())
			return
		}

		downloadURLs = append(downloadURLs, transfer.DownloadURLInfo{
			Path: file.Path,
			URL:  signedURL.URL,
			Size: file.Size,
			Hash: file.Hash,
		})
	}

	writeJSON(w, http.StatusOK, SyncFromResponse{
		Manifest:     uploadResp.Manifest,
		DownloadURLs: downloadURLs,
		Expires:      expires,
	})
}

// handleWorkspaceSyncTo initiates a workspace sync to an agent.
// POST /api/v1/agents/{id}/workspace/sync-to
//
// This endpoint:
// 1. Validates the agent exists
// 2. Checks which files already exist in storage (for incremental sync)
// 3. Returns signed upload URLs for new/changed files
func (s *Server) handleWorkspaceSyncTo(w http.ResponseWriter, r *http.Request, agentID string) {
	ctx := r.Context()

	// Parse request body
	var req SyncToRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate files list is not empty
	if len(req.Files) == 0 {
		ValidationError(w, "files list is required", nil)
		return
	}

	// Validate agent exists
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Check storage is configured
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Get workspace storage path
	storagePath := storage.WorkspaceStoragePath(agent.GroveID, agentID)

	// Check for existing files with matching hashes (incremental sync)
	expires := time.Now().Add(SignedURLExpiry)
	uploadURLs := make([]transfer.UploadURLInfo, 0, len(req.Files))
	existingFiles := make([]string, 0)

	for _, file := range req.Files {
		objectPath := storagePath + "/files/" + file.Path

		// Check if file already exists with matching hash
		// This enables incremental sync - skip files that haven't changed
		obj, err := stor.GetObject(ctx, objectPath)
		if err == nil && obj != nil {
			// File exists, check if hash matches via ETag or metadata
			// GCS ETag is MD5, so we check metadata for SHA256 hash
			if storedHash, ok := obj.Metadata["sha256"]; ok && storedHash == file.Hash {
				existingFiles = append(existingFiles, file.Path)
				continue
			}
		}

		// File doesn't exist or hash doesn't match - generate upload URL
		signedURL, err := stor.GenerateSignedURL(ctx, objectPath, storage.SignedURLOptions{
			Method:      "PUT",
			Expires:     SignedURLExpiry,
			ContentType: "application/octet-stream",
		})
		if err != nil {
			RuntimeError(w, "Failed to generate upload URL: "+err.Error())
			return
		}

		uploadURLs = append(uploadURLs, transfer.UploadURLInfo{
			Path:    file.Path,
			URL:     signedURL.URL,
			Method:  "PUT",
			Headers: signedURL.Headers,
			Expires: expires,
		})
	}

	writeJSON(w, http.StatusOK, SyncToResponse{
		UploadURLs:    uploadURLs,
		ExistingFiles: existingFiles,
		Expires:       expires,
	})
}

// handleWorkspaceSyncToFinalize finalizes a workspace sync-to operation.
// POST /api/v1/agents/{id}/workspace/sync-to/finalize
//
// This endpoint:
// 1. Validates the manifest and uploaded files
// 2. Tunnels request to Runtime Broker to apply workspace from GCS
// 3. Updates workspace metadata
func (s *Server) handleWorkspaceSyncToFinalize(w http.ResponseWriter, r *http.Request, agentID string) {
	ctx := r.Context()

	// Parse request body
	var req SyncToFinalizeRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate manifest
	if req.Manifest == nil {
		ValidationError(w, "manifest is required", nil)
		return
	}

	// Validate agent exists
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Check agent is running for apply
	if agent.Status != "running" {
		Conflict(w, "Agent is not running")
		return
	}

	// Check storage is configured
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Get workspace storage path
	storagePath := storage.WorkspaceStoragePath(agent.GroveID, agentID)

	// Verify all files exist in storage
	for _, file := range req.Manifest.Files {
		objectPath := storagePath + "/files/" + file.Path
		exists, err := stor.Exists(ctx, objectPath)
		if err != nil {
			RuntimeError(w, "Failed to verify file: "+err.Error())
			return
		}
		if !exists {
			ValidationError(w, "File not found in storage: "+file.Path, nil)
			return
		}
	}

	// Compute content hash from file hashes
	contentHash := transfer.ComputeContentHash(req.Manifest.Files)

	// Tunnel request to Runtime Broker to apply workspace
	cc := s.GetControlChannelManager()
	if cc == nil {
		RuntimeError(w, "Control channel not available")
		return
	}

	applyReq := RuntimeBrokerWorkspaceApplyRequest{
		Slug:        agentID, // agentID parameter is the URL slug
		StoragePath: storagePath,
		Manifest:    req.Manifest,
	}

	var applyResp RuntimeBrokerWorkspaceApplyResponse
	if err := tunnelWorkspaceRequest(ctx, cc, agent.RuntimeBrokerID, "POST", "/api/v1/workspace/apply", applyReq, &applyResp); err != nil {
		if strings.Contains(err.Error(), "timeout") {
			GatewayTimeout(w, "Runtime Broker unreachable")
			return
		}
		RuntimeError(w, "Failed to apply workspace: "+err.Error())
		return
	}

	// Calculate total bytes transferred
	var totalBytes int64
	for _, file := range req.Manifest.Files {
		totalBytes += file.Size
	}

	writeJSON(w, http.StatusOK, SyncToFinalizeResponse{
		Applied:          true,
		ContentHash:      contentHash,
		FilesApplied:     len(req.Manifest.Files),
		BytesTransferred: totalBytes,
	})
}

// Runtime Broker request/response types for control channel tunneling

// RuntimeBrokerWorkspaceUploadRequest is sent to Runtime Broker to upload workspace to GCS.
type RuntimeBrokerWorkspaceUploadRequest struct {
	Slug            string   `json:"slug"`
	StoragePath     string   `json:"storagePath"`
	ExcludePatterns []string `json:"excludePatterns,omitempty"`
}

// RuntimeBrokerWorkspaceUploadResponse is the response from Runtime Broker after workspace upload.
type RuntimeBrokerWorkspaceUploadResponse struct {
	Manifest      *transfer.Manifest `json:"manifest"`
	UploadedFiles int                `json:"uploadedFiles"`
	UploadedBytes int64              `json:"uploadedBytes"`
}

// RuntimeBrokerWorkspaceApplyRequest is sent to Runtime Broker to apply workspace from GCS.
type RuntimeBrokerWorkspaceApplyRequest struct {
	Slug        string             `json:"slug"`
	StoragePath string             `json:"storagePath"`
	Manifest    *transfer.Manifest `json:"manifest"`
}

// RuntimeBrokerWorkspaceApplyResponse is the response from Runtime Broker after workspace apply.
type RuntimeBrokerWorkspaceApplyResponse struct {
	Applied      bool  `json:"applied"`
	FilesApplied int   `json:"filesApplied"`
	BytesApplied int64 `json:"bytesApplied"`
}

// tunnelWorkspaceRequest tunnels a workspace request to a Runtime Broker via the control channel.
func tunnelWorkspaceRequest(ctx context.Context, cc *ControlChannelManager, brokerID, method, path string, reqBody interface{}, respBody interface{}) error {
	// Check broker is connected
	if !cc.IsConnected(brokerID) {
		return errBrokerNotConnected(brokerID)
	}

	// Marshal request body
	var body []byte
	var err error
	if reqBody != nil {
		body, err = json.Marshal(reqBody)
		if err != nil {
			return err
		}
	}

	// Create request envelope
	headers := map[string]string{
		"Content-Type": "application/json",
	}
	reqEnv := wsprotocol.NewRequestEnvelope(uuid.New().String(), method, path, "", headers, body)

	// Send request through control channel
	respEnv, err := cc.TunnelRequest(ctx, brokerID, reqEnv)
	if err != nil {
		return err
	}

	// Check for error status codes
	if respEnv.StatusCode >= 400 {
		return errRuntimeBrokerError(respEnv.StatusCode, string(respEnv.Body))
	}

	// Unmarshal response body
	if respBody != nil && len(respEnv.Body) > 0 {
		if err := json.Unmarshal(respEnv.Body, respBody); err != nil {
			return err
		}
	}

	return nil
}

// errBrokerNotConnected returns an error indicating the broker is not connected.
func errBrokerNotConnected(brokerID string) error {
	return &brokerError{brokerID: brokerID, msg: "broker not connected via control channel"}
}

// errRuntimeBrokerError returns an error from the runtime broker.
func errRuntimeBrokerError(statusCode int, body string) error {
	return &brokerError{statusCode: statusCode, msg: body}
}

// brokerError represents an error from communication with a runtime broker.
type brokerError struct {
	brokerID     string
	statusCode int
	msg        string
}

func (e *brokerError) Error() string {
	if e.brokerID != "" {
		return "broker " + e.brokerID + ": " + e.msg
	}
	return e.msg
}
