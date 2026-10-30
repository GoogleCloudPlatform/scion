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

package api

import "time"

// WorkspaceDownloadModeSignedURLs is the create request's
// workspaceDownloadMode value that tells a Runtime Broker to bootstrap a
// hub-managed workspace through the Hub's signed-URL workspace download
// (POST /api/v1/runtime-brokers/{id}/agents/{agentId}/workspace-download)
// instead of reading the bucket with its own credentials. An empty mode is
// the bucket download.
const WorkspaceDownloadModeSignedURLs = "signed_urls"

// WorkspaceDownloadPageSize is the most object names the Hub lists for one
// workspace download page. Excluded names are dropped after listing, so a
// page can carry fewer files, or none, and still have a next cursor.
const WorkspaceDownloadPageSize = 500

// WorkspaceDownloadRequest is the body of a workspace download page request.
// Cursor is empty for the first page, else the NextCursor of the previous
// page; it is opaque to the broker.
type WorkspaceDownloadRequest struct {
	Cursor string `json:"cursor,omitempty"`
}

// WorkspaceDownloadFile is one file of a workspace download page: its path
// relative to the workspace root, a GET-only signed URL for it, and the
// size the Hub listed for it. URL is a bearer capability: it is never
// logged, persisted or put in error text.
type WorkspaceDownloadFile struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// WorkspaceDownloadResponse is one page of a workspace download. NextCursor
// is empty on the last page. Expires is when this page's URLs expire.
type WorkspaceDownloadResponse struct {
	Files      []WorkspaceDownloadFile `json:"files"`
	NextCursor string                  `json:"nextCursor"`
	Expires    time.Time               `json:"expires"`
}
