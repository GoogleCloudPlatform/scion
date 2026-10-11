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
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/google/uuid"
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
	// Warnings lists non-fatal notices, for example that the agent was
	// already launching and the workspace was not applied, or that files were
	// ignored because the project gives each agent an empty workspace
	// directory.
	Warnings []string `json:"warnings,omitempty"`
}

// WorkspaceStatusResponse is the response for getting workspace sync status.
type WorkspaceStatusResponse struct {
	// Slug is the agent's URL-safe identifier.
	Slug string `json:"slug"`
	// ProjectID is the project ID.
	ProjectID string `json:"projectId"`
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
			MethodNotAllowed(w, http.MethodGet)
		}
	case "sync-from":
		// POST /api/v1/agents/{id}/workspace/sync-from - Initiate sync from agent
		if r.Method == http.MethodPost {
			s.handleWorkspaceSyncFrom(w, r, agentID)
		} else {
			MethodNotAllowed(w, http.MethodPost)
		}
	case "sync-to":
		// POST /api/v1/agents/{id}/workspace/sync-to - Initiate sync to agent
		if r.Method == http.MethodPost {
			s.handleWorkspaceSyncTo(w, r, agentID)
		} else {
			MethodNotAllowed(w, http.MethodPost)
		}
	case "sync-to/finalize":
		// POST /api/v1/agents/{id}/workspace/sync-to/finalize - Finalize sync to agent
		if r.Method == http.MethodPost {
			s.handleWorkspaceSyncToFinalize(w, r, agentID)
		} else {
			MethodNotAllowed(w, http.MethodPost)
		}
	default:
		NotFound(w, "Workspace action")
	}
}

// getAuthorizedWorkspaceAgent loads the agent and enforces the required user permission.
func (s *Server) getAuthorizedWorkspaceAgent(w http.ResponseWriter, r *http.Request, agentID string, requiredAction Action) *store.Agent {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil
	}

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user authentication", nil)
		return nil
	}

	decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), requiredAction)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
		return nil
	}

	return agent
}

// handleWorkspaceStatus returns the current workspace sync status.
// GET /api/v1/agents/{id}/workspace
func (s *Server) handleWorkspaceStatus(w http.ResponseWriter, r *http.Request, agentID string) {
	agent := s.getAuthorizedWorkspaceAgent(w, r, agentID, ActionRead)
	if agent == nil {
		return
	}

	// Get storage for URI generation
	stor := s.GetStorage()
	storageURI := ""
	if stor != nil {
		storageURI = storage.WorkspaceStorageURI(s.HubID(), stor.Bucket(), agent.ProjectID, agentID)
	}

	// TODO: Fetch last sync info from storage metadata
	// For now, return basic status
	writeJSON(w, http.StatusOK, WorkspaceStatusResponse{
		Slug:       agentID, // agentID parameter is the URL slug
		ProjectID:  agent.ProjectID,
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

	agent := s.getAuthorizedWorkspaceAgent(w, r, agentID, ActionUpdate)
	if agent == nil {
		return
	}

	// Check agent is running
	if agent.Phase != string(state.PhaseRunning) {
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
	storagePath := storage.WorkspaceStoragePath(s.HubID(), agent.ProjectID, agentID)

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

	// The response waits on the upload tunneled to the broker and then, for
	// a hub-managed project on a remote broker, on the download of the
	// project workspace into the hub. Both together are bounded by
	// syncDispatchTimeout, the hub-to-broker request limit that already
	// capped the upload, and this request's write deadline is extended to
	// cover them (ptone/scion#4212). As at stop, a download cut by the
	// bound is logged only (syncHubManagedWorkspaceBack).
	workCtx, cancelWork := context.WithTimeout(ctx, syncDispatchTimeout)
	defer cancelWork()
	extendWriteDeadlineForSyncDispatch(ctx, w, s.config.WriteTimeout)

	// Send tunneled request to Runtime Broker
	var uploadResp RuntimeBrokerWorkspaceUploadResponse
	if err := tunnelWorkspaceRequest(workCtx, cc, agent.RuntimeBrokerID, "POST", "/api/v1/workspace/upload", uploadReq, &uploadResp); err != nil {
		// Check if it's a timeout or connection issue
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout") {
			GatewayTimeout(w, "Runtime Broker unreachable")
			return
		}
		RuntimeError(w, "Failed to sync workspace: "+err.Error())
		return
	}
	// A broker reply with a 2xx status but no manifest is a broker fault:
	// answer 502, as for other broker failures, rather than dereference
	// the missing manifest (ptone/scion#4245).
	if uploadResp.Manifest == nil {
		RuntimeError(w, "Failed to sync workspace: runtime broker returned no workspace manifest")
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

	// For hub-managed projects on remote brokers, also sync workspace back
	// to the Hub filesystem so the local copy stays up-to-date.
	s.syncHubManagedWorkspaceBack(workCtx, agent, storagePath)

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

	agent := s.getAuthorizedWorkspaceAgent(w, r, agentID, ActionUpdate)
	if agent == nil {
		return
	}

	// Check storage is configured
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Get workspace storage path
	storagePath := storage.WorkspaceStoragePath(s.HubID(), agent.ProjectID, agentID)

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

	agent := s.getAuthorizedWorkspaceAgent(w, r, agentID, ActionUpdate)
	if agent == nil {
		return
	}

	// A launch is already in flight (for example a duplicate finalize): do
	// not dispatch again.
	if agent.IsInFlight() {
		writeJSON(w, http.StatusOK, SyncToFinalizeResponse{
			Applied:  false,
			Warnings: []string{launchInFlightInputsWarning},
		})
		return
	}

	// Check agent is in a valid state for finalize
	if agent.Phase != string(state.PhaseRunning) && agent.Phase != string(state.PhaseProvisioning) {
		Conflict(w, "Agent must be running or provisioning")
		return
	}

	// Check storage is configured
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Get workspace storage path
	storagePath := storage.WorkspaceStoragePath(s.HubID(), agent.ProjectID, agentID)

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

	// Calculate total bytes transferred
	var totalBytes int64
	for _, file := range req.Manifest.Files {
		totalBytes += file.Size
	}

	// Bootstrap mode: agent is provisioning, dispatch to broker now
	if agent.Phase == string(state.PhaseProvisioning) {
		project, err := s.store.GetProject(ctx, agent.ProjectID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Empty-per-agent agents start in an empty private directory
		// (design #2703): the uploaded files are ignored, as on create, so
		// the broker never pre-seeds the directory from GCS.
		emptyPerAgent := project.IsEmptyPerAgent()
		if agent.AppliedConfig == nil {
			agent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		if emptyPerAgent {
			if len(req.Manifest.Files) > 0 {
				s.workspaceLog.Warn("Ignoring workspace files for empty-per-agent project",
					"agent_id", agent.ID, "project_id", project.ID, "files", len(req.Manifest.Files))
			}
			agent.AppliedConfig.WorkspaceStoragePath = ""
			agent.AppliedConfig.WorkspaceStorageBucket = ""
		} else {
			// Store workspace storage path on agent record for broker download
			agent.AppliedConfig.WorkspaceStoragePath = storagePath
			agent.AppliedConfig.WorkspaceStorageBucket = workspaceDownloadBucket(stor)
		}
		if err := s.store.UpdateAgent(ctx, agent); err != nil {
			RuntimeError(w, "Failed to update agent config: "+err.Error())
			return
		}

		// Dispatch to broker (creates and starts the agent)
		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			RuntimeError(w, "No dispatcher available")
			return
		}
		// From here the launch no longer follows the client
		// (ptone/scion#1961). The create-and-start runs under a start claim,
		// which records run intent running; its dispatch is bounded by
		// syncDispatch, derived from the claim's context.
		ctx = detachLaunchFromClient(ctx)
		// The response waits on that dispatch for up to
		// syncDispatchTimeout: extend this request's write deadline to
		// cover it (ptone/scion#3890, as ptone/scion#3850 did for create).
		extendWriteDeadlineForSyncDispatch(ctx, w, s.config.WriteTimeout)
		// The collector carries the outcome of the compensating delete of
		// a run that landed after a delete won (compensateLandedRun).
		ctx, dispatchWarns := withDispatchWarnings(ctx)
		created, err := s.createUnderClaim(ctx, agent, func(ctx context.Context) (out *CreateDispatchResult, err error) {
			err = syncDispatch(ctx, func(dctx context.Context) error {
				out, err = dispatcher.DispatchAgentCreate(dctx, agent)
				return err
			})
			return out, err
		})
		if errors.Is(err, ErrLaunchInvalidPhase) {
			writeLaunchInvalidPhase(w, err, agent.ID)
			return
		}
		if writeAgentTokenRecordError(w, err) {
			return
		}
		if s.writeStartClaimError(ctx, w, err, agent.ID) {
			return
		}
		if err != nil {
			if ref := deleteClaimedDuringDispatch(err, agent.ID); ref != nil {
				ref.write(w)
				return
			}
			if writeAgentTokenIssueError(w, err) {
				return
			}
			if writeEmptyPerAgentCapabilityError(w, err) {
				return
			}
			if relayDispatchRefusal(w, err) {
				return
			}
			if relayWorkspaceStorageUnconfigured(w, err) {
				return
			}
			if relayIdentityMappingError(w, err) {
				return
			}
			if relayBrokerRefusal(w, err) {
				return
			}
			RuntimeError(w, "Failed to dispatch agent: "+err.Error())
			return
		}

		if created.AcceptedLaunch() != nil {
			// Accepted for asynchronous launch: the row is already
			// provisioning; merge only the non-status fields.
			if _, err := s.persistAcceptedLaunch(ctx, agent); err != nil {
				s.workspaceLog.Warn("Failed to update agent after accepted launch", "error", err)
			}
		} else if s.deleteWonAfterLanding(ctx, agent.ID) {
			// A delete that won after the broker run landed answers 409,
			// as the synchronous create does (ptone/scion#3099,
			// ptone/scion#3518), with the outcome of the compensating
			// delete the dispatch ran. An accepted launch is settled by
			// its launch report instead.
			s.workspaceLog.Info("Agent was deleted while its workspace finalize launched it; answering 409",
				"agent_id", agent.ID)
			writeDeletedDuringCreate(w, agent.ID, dispatchWarns.Warnings())
			return
		} else if err := s.store.UpdateAgent(ctx, agent); err != nil {
			// Update agent status from broker response
			s.workspaceLog.Warn("Failed to update agent status after dispatch", "error", err)
		}

		// The dispatch's warnings (for example a failed delete-won check
		// in compensateLandedRun) are returned, as env submit does.
		if emptyPerAgent {
			resp := SyncToFinalizeResponse{ContentHash: contentHash}
			if len(req.Manifest.Files) > 0 {
				resp.Warnings = []string{api.WarningEmptyPerAgentWorkspaceFilesIgnored}
			}
			resp.Warnings = append(resp.Warnings, dispatchWarns.Warnings()...)
			writeJSON(w, http.StatusOK, resp)
			return
		}

		writeJSON(w, http.StatusOK, SyncToFinalizeResponse{
			Applied:          true,
			ContentHash:      contentHash,
			FilesApplied:     len(req.Manifest.Files),
			BytesTransferred: totalBytes,
			Warnings:         dispatchWarns.Warnings(),
		})
		return
	}

	// Normal mode: agent is running, tunnel apply to running container via control channel
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

	// The response waits on the broker's apply, bounded by the hub-to-broker
	// request limit (syncDispatchTimeout): extend this request's write
	// deadline to cover it (ptone/scion#4178).
	extendWriteDeadlineForSyncDispatch(ctx, w, s.config.WriteTimeout)
	var applyResp RuntimeBrokerWorkspaceApplyResponse
	if err := tunnelWorkspaceRequest(ctx, cc, agent.RuntimeBrokerID, "POST", "/api/v1/workspace/apply", applyReq, &applyResp); err != nil {
		if strings.Contains(err.Error(), "timeout") {
			GatewayTimeout(w, "Runtime Broker unreachable")
			return
		}
		RuntimeError(w, "Failed to apply workspace: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, SyncToFinalizeResponse{
		Applied:          true,
		ContentHash:      contentHash,
		FilesApplied:     len(req.Manifest.Files),
		BytesTransferred: totalBytes,
	})
}

// generateWorkspaceUploadURLs generates signed upload URLs for workspace files.
// It checks for existing files with matching hashes and only generates URLs for new/changed files.
// Returns upload URLs, list of existing (unchanged) files, and any error.
func generateWorkspaceUploadURLs(ctx context.Context, stor storage.Storage, storagePath string, files []transfer.FileInfo) ([]transfer.UploadURLInfo, []string, error) {
	expires := time.Now().Add(SignedURLExpiry)
	uploadURLs := make([]transfer.UploadURLInfo, 0, len(files))
	existingFiles := make([]string, 0)

	for _, file := range files {
		objectPath := storagePath + "/files/" + file.Path

		// Check if file already exists with matching hash (incremental sync)
		obj, err := stor.GetObject(ctx, objectPath)
		if err == nil && obj != nil {
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
			return nil, nil, err
		}

		uploadURLs = append(uploadURLs, transfer.UploadURLInfo{
			Path:    file.Path,
			URL:     signedURL.URL,
			Method:  "PUT",
			Headers: signedURL.Headers,
			Expires: expires,
		})
	}

	return uploadURLs, existingFiles, nil
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
	brokerID   string
	statusCode int
	msg        string
}

func (e *brokerError) Error() string {
	if e.brokerID != "" {
		return "broker " + e.brokerID + ": " + e.msg
	}
	return e.msg
}

// hubWorkspaceNeedsSyncBack is the gating shared by the two hub workspace
// sync-backs, the stop-time one (syncWorkspaceOnStop) and the sync-from one
// (syncHubManagedWorkspaceBack), so the two cannot drift apart. It loads the
// agent's project and reports whether the hub's copy of the project
// workspace must be synced back from agent's broker: the project syncs a hub
// workspace (hub-native or shared-workspace, not empty-per-agent; see
// syncsHubProjectWorkspace) and the agent's broker, when it has one, is
// neither the embedded broker nor a colocated one (a provider with a local
// path), which already share the hub's filesystem. An agent without a
// project needs no sync-back. err is the project lookup's error, returned
// so each caller keeps its own logging; ok is then false.
func (s *Server) hubWorkspaceNeedsSyncBack(ctx context.Context, agent *store.Agent) (project *store.Project, ok bool, err error) {
	if agent.ProjectID == "" {
		return nil, false, nil
	}
	project, err = s.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		return nil, false, err
	}
	// Empty-per-agent projects have no shared project workspace to keep in
	// sync, and syncing an agent's private directory would overwrite the
	// project's hub workspace (design #2703).
	if !syncsHubProjectWorkspace(project) {
		return project, false, nil
	}
	if agent.RuntimeBrokerID != "" {
		if s.isEmbeddedBroker(agent.RuntimeBrokerID) {
			return project, false, nil // Embedded broker, no sync needed
		}
		provider, perr := s.store.GetProjectProvider(ctx, project.ID, agent.RuntimeBrokerID)
		if perr == nil && provider.LocalPath != "" {
			return project, false, nil // Colocated broker, no sync needed
		}
	}
	return project, true, nil
}

// syncHubManagedWorkspaceBack downloads workspace files from GCS to the Hub's local
// filesystem for hub-managed projects on remote brokers. This keeps the Hub's copy
// (~/.scion/projects/<slug>/) in sync after workspace changes on a remote broker.
// storagePath is the storage path the caller's broker upload wrote; the
// files are read from storagePath + "/files".
// This is a best-effort operation: errors are logged but do not fail the caller.
func (s *Server) syncHubManagedWorkspaceBack(ctx context.Context, agent *store.Agent, storagePath string) {
	project, ok, err := s.hubWorkspaceNeedsSyncBack(ctx, agent)
	if err != nil {
		s.workspaceLog.Warn("syncHubManagedWorkspaceBack: failed to get project", "agent_id", agent.ID, "project_id", agent.ProjectID, "error", err)
		return
	}
	if !ok {
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		return
	}

	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		s.workspaceLog.Warn("syncHubManagedWorkspaceBack: failed to get project path", "error", err)
		return
	}

	// Download from storagePath, the path the caller's upload just wrote
	// (sync-from uploads to the agent's WorkspaceStoragePath). The
	// project-level ProjectWorkspaceStoragePath is written only by the
	// hub's own upload at create, by the stop-time sync-back and by
	// project-cache refreshes, so reading it here found nothing or stale
	// content (ptone/scion#4244).
	if err := s.syncHubWorkspaceFromGCS(ctx, stor.Bucket(), storagePath+"/files", workspacePath); err != nil {
		s.workspaceLog.Warn("syncHubManagedWorkspaceBack: GCS download failed",
			"project_id", project.ID, "storagePath", storagePath, "error", err)
	} else {
		s.workspaceLog.Info("syncHubManagedWorkspaceBack: workspace synced to Hub filesystem",
			"project_id", project.ID, "path", workspacePath)
	}
}

// workspaceDownloadBucket returns the bucket a broker should download a
// workspace upload in stor from, or "" when stor is not GCS: a broker
// cannot read the hub's local storage, so it is then left to the broker's
// own bucket setting and its explicit refusal when it has none
// (ptone/scion#3422).
func workspaceDownloadBucket(stor storage.Storage) string {
	if stor == nil || stor.Provider() != storage.ProviderGCS {
		return ""
	}
	return stor.Bucket()
}
