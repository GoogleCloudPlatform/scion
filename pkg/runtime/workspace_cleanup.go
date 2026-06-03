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

package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// CleanupNFSProject removes the NFS project subtree for a given project ID.
// This mirrors the K8s-side cleanupSharedDirPVCs (k8s_runtime.go:753-770)
// for the Docker/VM NFS model.
//
// The target path is <MountRoot>/<shareID>/projects/<projectID>/, which
// contains the workspace and shared-dirs subdirectories.
//
// Isolation guard: refuses to delete the share root or any path that is not
// a proper subdirectory of the share mount. This uses the same
// ValidateNotExportRoot guard as Realize (design §9.4).
//
// Idempotent: returns nil if the directory does not exist.
func CleanupNFSProject(cfg *config.V1NFSConfig, projectID string) error {
	if cfg == nil {
		return fmt.Errorf("CleanupNFSProject: NFS config is nil")
	}
	if projectID == "" {
		return fmt.Errorf("CleanupNFSProject: projectID is required")
	}
	if len(cfg.Shares) == 0 {
		return fmt.Errorf("CleanupNFSProject: no NFS shares configured")
	}

	subPathRoot := cfg.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}

	share := cfg.Shares[0]
	hostBase := filepath.Join(cfg.MountRoot, share.ID)

	// Compute the project subtree path: <MountRoot>/<shareID>/<SubPathRoot>/<projectID>/
	projectPath := filepath.Join(hostBase, subPathRoot, projectID)

	// Isolation guard: the target must be a proper subdirectory of the host
	// base (share mount), NEVER the share root itself. This prevents
	// accidental deletion of the entire NFS share.
	if err := ValidateNotExportRoot(projectPath, hostBase); err != nil {
		return fmt.Errorf("CleanupNFSProject: %w", err)
	}

	// Additional safety: the resolved path must be under the projects subtree.
	// This catches path traversal attempts (e.g. projectID = "../../something").
	cleanPath := filepath.Clean(projectPath)
	cleanBase := filepath.Clean(filepath.Join(hostBase, subPathRoot))
	if len(cleanPath) <= len(cleanBase) || cleanPath[:len(cleanBase)] != cleanBase {
		return fmt.Errorf("CleanupNFSProject: path traversal detected: %q is not under %q",
			projectPath, filepath.Join(hostBase, subPathRoot))
	}

	// Idempotent: if the directory doesn't exist, nothing to do.
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		slog.Debug("CleanupNFSProject: project subtree does not exist (already cleaned)",
			"project_id", projectID, "path", projectPath)
		return nil
	}

	slog.Info("CleanupNFSProject: removing project subtree",
		"project_id", projectID, "path", projectPath)

	if err := os.RemoveAll(projectPath); err != nil {
		return fmt.Errorf("CleanupNFSProject: rm -rf %s: %w", projectPath, err)
	}

	slog.Info("CleanupNFSProject: project subtree removed",
		"project_id", projectID, "path", projectPath)
	return nil
}
