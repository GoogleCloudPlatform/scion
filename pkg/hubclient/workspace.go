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

package hubclient

import (
	"context"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/transfer"
)

// WorkspaceService handles workspace synchronization operations.
type WorkspaceService interface {
	// SyncFrom initiates download of workspace from an agent.
	// Triggers the Runtime Broker to upload workspace to GCS and returns signed download URLs.
	SyncFrom(ctx context.Context, agentID string, opts *SyncFromOptions) (*SyncFromResponse, error)

	// SyncTo initiates upload of workspace to an agent.
	// Returns signed upload URLs for files that need to be uploaded.
	SyncTo(ctx context.Context, agentID string, files []transfer.FileInfo) (*SyncToResponse, error)

	// FinalizeSyncTo completes the sync-to operation after files are uploaded.
	// Triggers the Runtime Broker to apply the workspace from GCS.
	FinalizeSyncTo(ctx context.Context, agentID string, manifest *transfer.Manifest) (*SyncToFinalizeResponse, error)

	// GetStatus returns the current workspace sync status for an agent.
	GetStatus(ctx context.Context, agentID string) (*WorkspaceStatusResponse, error)
}

// workspaceService is the implementation of WorkspaceService.
type workspaceService struct {
	c              *client
	transferClient *transfer.Client
}

// SyncFromOptions configures sync-from operations.
type SyncFromOptions struct {
	// ExcludePatterns are glob patterns to exclude from sync (e.g., ".git/**").
	ExcludePatterns []string `json:"excludePatterns,omitempty"`
}

// SyncFromResponse is the response from initiating a sync-from operation.
type SyncFromResponse struct {
	// Manifest contains the file manifest from the agent workspace.
	Manifest *transfer.Manifest `json:"manifest"`
	// DownloadURLs contains signed URLs for downloading each file.
	DownloadURLs []transfer.DownloadURLInfo `json:"downloadUrls"`
	// Expires is when the signed URLs expire.
	Expires time.Time `json:"expires"`
}

// SyncToResponse is the response from initiating a sync-to operation.
type SyncToResponse struct {
	// UploadURLs contains signed URLs for uploading files.
	UploadURLs []transfer.UploadURLInfo `json:"uploadUrls"`
	// ExistingFiles lists file paths that already exist with matching hashes (skip upload).
	ExistingFiles []string `json:"existingFiles"`
	// Expires is when the signed URLs expire.
	Expires time.Time `json:"expires"`
}

// SyncToFinalizeResponse is the response from finalizing a sync-to operation.
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

// WorkspaceStatusResponse is the response from getting workspace status.
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

// SyncFrom initiates download of workspace from an agent.
func (s *workspaceService) SyncFrom(ctx context.Context, agentID string, opts *SyncFromOptions) (*SyncFromResponse, error) {
	// Build request body
	var req interface{}
	if opts != nil {
		req = struct {
			ExcludePatterns []string `json:"excludePatterns,omitempty"`
		}{
			ExcludePatterns: opts.ExcludePatterns,
		}
	}

	resp, err := s.c.transport.Post(ctx, "/api/v1/agents/"+agentID+"/workspace/sync-from", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SyncFromResponse](resp)
}

// SyncTo initiates upload of workspace to an agent.
func (s *workspaceService) SyncTo(ctx context.Context, agentID string, files []transfer.FileInfo) (*SyncToResponse, error) {
	req := struct {
		Files []transfer.FileInfo `json:"files"`
	}{
		Files: files,
	}

	resp, err := s.c.transport.Post(ctx, "/api/v1/agents/"+agentID+"/workspace/sync-to", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SyncToResponse](resp)
}

// FinalizeSyncTo completes the sync-to operation after files are uploaded.
func (s *workspaceService) FinalizeSyncTo(ctx context.Context, agentID string, manifest *transfer.Manifest) (*SyncToFinalizeResponse, error) {
	req := struct {
		Manifest *transfer.Manifest `json:"manifest"`
	}{
		Manifest: manifest,
	}

	resp, err := s.c.transport.Post(ctx, "/api/v1/agents/"+agentID+"/workspace/sync-to/finalize", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SyncToFinalizeResponse](resp)
}

// GetStatus returns the current workspace sync status for an agent.
func (s *workspaceService) GetStatus(ctx context.Context, agentID string) (*WorkspaceStatusResponse, error) {
	resp, err := s.c.transport.Get(ctx, "/api/v1/agents/"+agentID+"/workspace", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[WorkspaceStatusResponse](resp)
}

// getTransferClient returns the transfer client, creating one if necessary.
func (s *workspaceService) getTransferClient() *transfer.Client {
	if s.transferClient == nil {
		s.transferClient = transfer.NewClient(s.c.transport.HTTPClient)
	}
	return s.transferClient
}

// UploadFiles uploads files to their respective signed URLs.
// This is a convenience method that uses the transfer client.
func (s *workspaceService) UploadFiles(ctx context.Context, files []transfer.FileInfo, urls []transfer.UploadURLInfo, progress transfer.ProgressCallback) error {
	client := s.getTransferClient()
	return client.UploadFiles(ctx, files, urls, progress)
}

// DownloadFiles downloads files from signed URLs to a destination directory.
// This is a convenience method that uses the transfer client.
func (s *workspaceService) DownloadFiles(ctx context.Context, urls []transfer.DownloadURLInfo, destDir string, progress transfer.ProgressCallback) error {
	client := s.getTransferClient()
	return client.DownloadFiles(ctx, urls, destDir, progress)
}
