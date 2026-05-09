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
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/google/uuid"
)

// ProjectCacheRefreshResponse is the response for a grove cache refresh operation.
type ProjectCacheRefreshResponse struct {
	// ProjectID is the grove that was refreshed.
	ProjectID string `json:"groveId"`
	// BrokerID is the broker that provided the workspace.
	BrokerID string `json:"brokerId"`
	// FileCount is the number of files in the cached workspace.
	FileCount int `json:"fileCount"`
	// TotalBytes is the total size of the cached workspace.
	TotalBytes int64 `json:"totalBytes"`
	// CachedAt is when the cache was last refreshed.
	CachedAt time.Time `json:"cachedAt"`
}

// ProjectCacheStatusResponse is the response for the grove cache status endpoint.
type ProjectCacheStatusResponse struct {
	// ProjectID is the grove identifier.
	ProjectID string `json:"groveId"`
	// Cached indicates whether a cached copy exists on the hub.
	Cached bool `json:"cached"`
	// BrokerID is the broker that last provided the workspace.
	BrokerID string `json:"brokerId,omitempty"`
	// FileCount is the number of files in the cache.
	FileCount int `json:"fileCount"`
	// TotalBytes is the total size of cached files.
	TotalBytes int64 `json:"totalBytes"`
	// LastRefresh is when the cache was last refreshed.
	LastRefresh *time.Time `json:"lastRefresh,omitempty"`
}

// RuntimeBrokerProjectUploadRequest is sent to a Runtime Broker to upload a grove's
// workspace (not an individual agent workspace) to GCS.
type RuntimeBrokerProjectUploadRequest struct {
	// ProjectID is the grove identifier.
	ProjectID string `json:"groveId"`
	// StoragePath is the path within the bucket where files should be uploaded.
	StoragePath string `json:"storagePath"`
	// WorkspacePath is the local filesystem path to the grove workspace on the broker.
	// Provided by the hub from the GroveProvider.LocalPath record.
	WorkspacePath string `json:"workspacePath"`
	// ExcludePatterns are glob patterns to exclude from the upload.
	ExcludePatterns []string `json:"excludePatterns,omitempty"`
}

// RuntimeBrokerProjectUploadResponse is the response from the Runtime Broker after
// uploading a grove workspace.
type RuntimeBrokerProjectUploadResponse struct {
	// Manifest contains the list of files uploaded with their hashes.
	Manifest *transfer.Manifest `json:"manifest"`
	// UploadedFiles is the number of files uploaded.
	UploadedFiles int `json:"uploadedFiles"`
	// UploadedBytes is the total size of uploaded files.
	UploadedBytes int64 `json:"uploadedBytes"`
}

// handleProjectCacheRefresh triggers a cache refresh for a linked grove.
// POST /api/v1/groves/{groveId}/workspace/cache/refresh
//
// This endpoint:
// 1. Validates the grove is a linked grove (workspace lives on a broker)
// 2. Identifies a connected provider broker for the grove
// 3. Tunnels a request to the broker to upload the workspace to GCS
// 4. Downloads the workspace from GCS to the hub's local cache
// 5. Updates sync state
func (s *Server) handleProjectCacheRefresh(w http.ResponseWriter, r *http.Request, groveID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Hub-native groves don't need cache refresh — they are the source of truth
	if grove.GitRemote == "" && !s.isLinkedProject(ctx, grove) {
		Conflict(w, "Cache refresh is only applicable to linked groves with remote workspaces")
		return
	}

	// Find a connected provider broker
	brokerID, err := s.findConnectedProvider(ctx, grove)
	if err != nil {
		Conflict(w, err.Error())
		return
	}

	// Check storage is configured
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured; cache refresh requires GCS")
		return
	}

	// Perform the cache refresh
	resp, err := s.refreshProjectCacheFromBroker(ctx, grove, brokerID, stor)
	if err != nil {
		RuntimeError(w, "Cache refresh failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleProjectCacheStatus returns the cache status for a linked grove.
// GET /api/v1/groves/{groveId}/workspace/cache/status
func (s *Server) handleProjectCacheStatus(w http.ResponseWriter, r *http.Request, groveID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Check if a cache exists on disk
	cachePath, err := hubNativeProjectPath(grove.Slug)
	if err != nil {
		InternalError(w)
		return
	}

	cached := false
	if _, statErr := os.Stat(cachePath); statErr == nil {
		cached = true
	}

	// Get sync state for the cache
	resp := ProjectCacheStatusResponse{
		ProjectID: groveID,
		Cached:  cached,
	}

	// Look up the latest sync state (from any broker)
	states, err := s.store.ListProjectSyncStates(ctx, groveID)
	if err == nil {
		for _, st := range states {
			if st.BrokerID != "" {
				resp.BrokerID = st.BrokerID
				resp.FileCount = st.FileCount
				resp.TotalBytes = st.TotalBytes
				resp.LastRefresh = st.LastSyncTime
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleProjectCacheNotify handles a notification from a broker that it has pushed
// workspace updates to GCS and the hub cache should be refreshed.
// POST /api/v1/groves/{groveId}/workspace/cache/notify
func (s *Server) handleProjectCacheNotify(w http.ResponseWriter, r *http.Request, groveID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
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

	// Download the latest workspace from GCS to local cache
	cachePath, err := hubNativeProjectPath(grove.Slug)
	if err != nil {
		InternalError(w)
		return
	}

	if err := os.MkdirAll(cachePath, 0755); err != nil {
		s.workspaceLog.Error("failed to create cache directory", "grove_id", groveID, "error", err)
		InternalError(w)
		return
	}

	storagePath := storage.ProjectWorkspaceStoragePath(groveID)
	if err := gcp.SyncFromGCS(ctx, stor.Bucket(), storagePath+"/files", cachePath); err != nil {
		RuntimeError(w, "Failed to download workspace from GCS: "+err.Error())
		return
	}

	// Update sync state
	now := time.Now()
	var fileCount int
	var totalBytes int64
	_ = walkFilteredDir(cachePath, func(relPath string, info os.FileInfo) {
		fileCount++
		totalBytes += info.Size()
	})

	// Extract broker ID from auth context
	brokerID := ""
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil {
		brokerID = brokerIdent.BrokerID()
	}

	state := &store.ProjectSyncState{
		ProjectID:      groveID,
		BrokerID:     brokerID,
		LastSyncTime: &now,
		FileCount:    fileCount,
		TotalBytes:   totalBytes,
	}
	if err := s.store.UpsertProjectSyncState(ctx, state); err != nil {
		s.workspaceLog.Warn("failed to update grove sync state after cache notify", "grove_id", groveID, "error", err)
	}

	s.workspaceLog.Info("grove cache refreshed via notify",
		"grove_id", groveID, "files", fileCount, "bytes", totalBytes)

	writeJSON(w, http.StatusOK, ProjectCacheRefreshResponse{
		ProjectID:    groveID,
		BrokerID:   brokerID,
		FileCount:  fileCount,
		TotalBytes: totalBytes,
		CachedAt:   now,
	})
}

// refreshProjectCacheFromBroker triggers a broker to upload the grove workspace
// to GCS, then downloads it to the hub's local cache.
func (s *Server) refreshProjectCacheFromBroker(ctx context.Context, grove *store.Project, brokerID string, stor storage.Storage) (*ProjectCacheRefreshResponse, error) {
	cc := s.GetControlChannelManager()
	if cc == nil {
		return nil, fmt.Errorf("control channel not available")
	}

	// Get the provider's local path so the broker knows which directory to upload
	provider, err := s.store.GetProjectProvider(ctx, grove.ID, brokerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider info: %w", err)
	}
	if provider.LocalPath == "" {
		return nil, fmt.Errorf("broker %s has no local path recorded for grove %s", brokerID, grove.Name)
	}

	storagePath := storage.ProjectWorkspaceStoragePath(grove.ID)

	// Tunnel request to broker to upload grove workspace to GCS.
	// The workspace path tells the broker which directory to upload.
	uploadReq := RuntimeBrokerProjectUploadRequest{
		ProjectID:       grove.ID,
		StoragePath:   storagePath,
		WorkspacePath: provider.LocalPath,
	}

	var uploadResp RuntimeBrokerProjectUploadResponse
	if err := tunnelProjectWorkspaceRequest(ctx, cc, brokerID, "POST", "/api/v1/workspace/grove-upload", uploadReq, &uploadResp); err != nil {
		return nil, fmt.Errorf("broker upload failed: %w", err)
	}

	// Download from GCS to local cache
	cachePath, err := hubNativeProjectPath(grove.Slug)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve cache path: %w", err)
	}

	if err := os.MkdirAll(cachePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	if err := gcp.SyncFromGCS(ctx, stor.Bucket(), storagePath+"/files", cachePath); err != nil {
		return nil, fmt.Errorf("GCS download failed: %w", err)
	}

	// Update sync state
	now := time.Now()
	state := &store.ProjectSyncState{
		ProjectID:      grove.ID,
		BrokerID:     brokerID,
		LastSyncTime: &now,
		FileCount:    uploadResp.UploadedFiles,
		TotalBytes:   uploadResp.UploadedBytes,
	}
	if err := s.store.UpsertProjectSyncState(ctx, state); err != nil {
		s.workspaceLog.Warn("failed to update sync state after cache refresh",
			"grove_id", grove.ID, "error", err)
	}

	s.workspaceLog.Info("grove cache refreshed from broker",
		"grove_id", grove.ID, "broker_id", brokerID,
		"files", uploadResp.UploadedFiles, "bytes", uploadResp.UploadedBytes)

	return &ProjectCacheRefreshResponse{
		ProjectID:    grove.ID,
		BrokerID:   brokerID,
		FileCount:  uploadResp.UploadedFiles,
		TotalBytes: uploadResp.UploadedBytes,
		CachedAt:   now,
	}, nil
}

// isLinkedProject returns true if the grove has at least one provider broker
// with a local_path, indicating the workspace lives on a remote broker.
func (s *Server) isLinkedProject(ctx context.Context, grove *store.Project) bool {
	providers, err := s.store.GetProjectProviders(ctx, grove.ID)
	if err != nil {
		return false
	}
	for _, p := range providers {
		if p.LocalPath != "" && !s.isEmbeddedBroker(p.BrokerID) {
			return true
		}
	}
	return false
}

// findConnectedProvider finds a connected provider broker for a grove.
// It prefers the default runtime broker, then falls back to any connected provider.
func (s *Server) findConnectedProvider(ctx context.Context, grove *store.Project) (string, error) {
	cc := s.GetControlChannelManager()
	if cc == nil {
		return "", fmt.Errorf("control channel not available")
	}

	providers, err := s.store.GetProjectProviders(ctx, grove.ID)
	if err != nil {
		return "", fmt.Errorf("failed to get grove providers: %w", err)
	}

	if len(providers) == 0 {
		return "", fmt.Errorf("grove has no provider brokers")
	}

	// Prefer the default runtime broker if connected
	if grove.DefaultRuntimeBrokerID != "" && cc.IsConnected(grove.DefaultRuntimeBrokerID) {
		return grove.DefaultRuntimeBrokerID, nil
	}

	// Fall back to any connected provider with a local path
	for _, p := range providers {
		if p.LocalPath != "" && cc.IsConnected(p.BrokerID) {
			return p.BrokerID, nil
		}
	}

	// Fall back to any connected provider
	for _, p := range providers {
		if cc.IsConnected(p.BrokerID) {
			return p.BrokerID, nil
		}
	}

	return "", fmt.Errorf("no provider broker is currently connected for grove %s", grove.Name)
}

// hasProjectCache returns true if the hub has a cached copy of the grove workspace.
func hasProjectCache(slug string) bool {
	cachePath, err := hubNativeProjectPath(slug)
	if err != nil {
		return false
	}
	info, err := os.Stat(cachePath)
	return err == nil && info.IsDir()
}

// tunnelProjectWorkspaceRequest tunnels a grove workspace request to a Runtime Broker
// via the control channel. This is similar to tunnelWorkspaceRequest but for
// grove-level (not agent-level) operations.
func tunnelProjectWorkspaceRequest(ctx context.Context, cc *ControlChannelManager, brokerID, method, path string, reqBody interface{}, respBody interface{}) error {
	if !cc.IsConnected(brokerID) {
		return errBrokerNotConnected(brokerID)
	}

	var body []byte
	var err error
	if reqBody != nil {
		body, err = json.Marshal(reqBody)
		if err != nil {
			return err
		}
	}

	headers := map[string]string{
		"Content-Type": "application/json",
	}
	reqEnv := wsprotocol.NewRequestEnvelope(uuid.New().String(), method, path, "", headers, body)

	respEnv, err := cc.TunnelRequest(ctx, brokerID, reqEnv)
	if err != nil {
		return err
	}

	if respEnv.StatusCode >= 400 {
		return errRuntimeBrokerError(respEnv.StatusCode, string(respEnv.Body))
	}

	if respBody != nil && len(respEnv.Body) > 0 {
		if err := json.Unmarshal(respEnv.Body, respBody); err != nil {
			return err
		}
	}

	return nil
}
