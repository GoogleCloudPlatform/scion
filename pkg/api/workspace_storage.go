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

import "strings"

// Workspace storage backends reported in BrokerWorkspaceStorage.Backend.
const (
	WorkspaceStorageBackendLocal = "local"
	WorkspaceStorageBackendNFS   = "nfs"
)

// BrokerWorkspaceStorage describes where a runtime broker places agent
// workspaces. Brokers report it at registration and on every heartbeat; the
// hub uses it to decide whether two brokers share a workspace export (the
// precondition for moving an agent between brokers without copying its
// workspace). A nil descriptor means the broker did not report one (an older
// broker) and is treated as "unknown", never as "local".
type BrokerWorkspaceStorage struct {
	// Backend is the configured workspace storage backend: "local" or "nfs".
	Backend string `json:"backend"`
	// NFS describes the share agent workspaces are placed on. It is set only
	// when Backend is "nfs" and the broker's NFS configuration is valid.
	NFS *BrokerNFSWorkspaceStorage `json:"nfs,omitempty"`
}

// BrokerNFSWorkspaceStorage identifies the NFS export a broker places agent
// workspaces on. Agent workspaces always live on the first configured share
// (V1NFSConfig.Shares[0]), so that share alone is described.
type BrokerNFSWorkspaceStorage struct {
	// Server is the NFS server host of Shares[0].
	Server string `json:"server"`
	// Export is the exported path of Shares[0].
	Export string `json:"export"`
	// SubPathRoot is the directory under the export that holds per-project
	// workspace trees (<export>/<subPathRoot>/<projectID>/...).
	SubPathRoot string `json:"subPathRoot"`
	// Healthy reports whether the broker's last health check of the share
	// succeeded. It is false until the broker has checked the mount.
	Healthy bool `json:"healthy"`
}

// SameWorkspaceExport reports whether a and b place agent workspaces on the
// same NFS export with the same sub-path root, so a workspace written by one
// broker is visible at the same relative path on the other. Share IDs and
// local mount roots are broker-local naming and are not compared. Health is
// not part of identity.
func SameWorkspaceExport(a, b *BrokerWorkspaceStorage) bool {
	if a == nil || b == nil || a.NFS == nil || b.NFS == nil {
		return false
	}
	if a.Backend != WorkspaceStorageBackendNFS || b.Backend != WorkspaceStorageBackendNFS {
		return false
	}
	return a.NFS.Server == b.NFS.Server &&
		normalizeExportPath(a.NFS.Export) == normalizeExportPath(b.NFS.Export) &&
		a.NFS.SubPathRoot == b.NFS.SubPathRoot
}

func normalizeExportPath(p string) string {
	if len(p) > 1 {
		return strings.TrimRight(p, "/")
	}
	return p
}
