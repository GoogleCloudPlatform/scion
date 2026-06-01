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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// BootstrapHarnessConfigsFromDir imports or updates local harness configs from
// a directory into the Hub's database and storage. On first run it imports all
// configs; on subsequent runs it detects changed configs (by content hash) and
// re-uploads only those that differ from the database version.
func (s *Server) BootstrapHarnessConfigsFromDir(ctx context.Context, harnessConfigsDir string) error {
	info, err := os.Stat(harnessConfigsDir)
	if err != nil || !info.IsDir() {
		s.templateLog.Debug("harness config bootstrap: directory not found, skipping", "dir", harnessConfigsDir)
		return nil
	}

	stor := s.GetStorage()
	if stor == nil {
		s.templateLog.Warn("harness config bootstrap: no storage backend configured, skipping")
		return nil
	}

	entries, err := os.ReadDir(harnessConfigsDir)
	if err != nil {
		return err
	}

	imported, updated := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		dirPath := filepath.Join(harnessConfigsDir, name)
		slug := api.Slugify(name)

		// Load config.yaml to get harness type
		hcDir, err := config.LoadHarnessConfigDir(dirPath)
		if err != nil {
			s.templateLog.Warn("harness config bootstrap: failed to load config, skipping",
				"config", name, "error", err)
			continue
		}

		existing, err := s.store.GetHarnessConfigBySlug(ctx, slug, store.HarnessConfigScopeGlobal, "")
		if err != nil && err != store.ErrNotFound {
			s.templateLog.Warn("harness config bootstrap: failed to look up config, skipping",
				"config", name, "error", err)
			continue
		}

		if existing == nil {
			if err := s.bootstrapSingleHarnessConfig(ctx, name, dirPath, hcDir, stor); err != nil {
				s.templateLog.Warn("harness config bootstrap: failed to import config, skipping",
					"config", name, "error", err)
				continue
			}
			imported++
		} else {
			changed, err := s.syncExistingHarnessConfig(ctx, existing, dirPath, hcDir, stor, false)
			if err != nil {
				s.templateLog.Warn("harness config bootstrap: failed to sync config, skipping",
					"config", name, "error", err)
				continue
			}
			if changed {
				updated++
			}
		}
	}

	if imported > 0 || updated > 0 {
		s.templateLog.Info("harness config bootstrap: sync complete",
			"imported", imported, "updated", updated)
	}

	return nil
}

// bootstrapSingleHarnessConfig imports one local harness config directory into
// the Hub's database and storage backend.
func (s *Server) bootstrapSingleHarnessConfig(ctx context.Context, name, dirPath string, hcDir *config.HarnessConfigDir, stor storage.Storage) error {
	return s.bootstrapSingleHarnessConfigScoped(ctx, name, dirPath, hcDir, stor, store.HarnessConfigScopeGlobal, "")
}

// bootstrapSingleHarnessConfigScoped delegates to the shared ResourceStore
// (§7.3). stor is unused — the store resolves the backend itself — but is kept
// in the signature to match the bundled-import call sites.
func (s *Server) bootstrapSingleHarnessConfigScoped(ctx context.Context, name, dirPath string, hcDir *config.HarnessConfigDir, _ storage.Storage, scope, scopeID string) error {
	_, err := s.harnessConfigStore(hcDir.Config.Harness).Bootstrap(ctx, name, dirPath, scope, scopeID, false)
	return err
}

// isHarnessConfigDir reports whether dir looks like a harness-config directory,
// i.e. it contains a config.yaml file. Analogous to
// templateimport.IsScionTemplate (which checks for scion-agent.yaml).
func isHarnessConfigDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "config.yaml"))
	return err == nil && !info.IsDir()
}

// importHarnessConfigsFromRemote fetches a remote source URL, discovers
// harness-configs within it, and registers each one into the Hub store scoped to
// the given project. Returns the names of all configs imported or updated.
func (s *Server) importHarnessConfigsFromRemote(ctx context.Context, projectID, sourceURL string) ([]string, error) {
	if !config.IsRemoteURI(sourceURL) {
		return nil, fmt.Errorf("source must be a remote URI (http://, https://, or rclone)")
	}

	stor := s.GetStorage()
	if stor == nil {
		return nil, fmt.Errorf("harness-config storage is not configured")
	}

	// If the project has a GitHub App installation, mint a token for authenticated access.
	var authToken string
	project, err := s.store.GetProject(ctx, projectID)
	if err == nil && project != nil && project.GitHubInstallationID != nil {
		if token, _, mintErr := s.MintGitHubAppTokenForProject(ctx, project); mintErr == nil && token != "" {
			authToken = token
		}
	}

	// Fetch to a temporary directory.
	cachePath, err := config.FetchRemoteTemplate(ctx, sourceURL, authToken)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch remote harness-configs: %w", err)
	}
	defer func() { _ = os.RemoveAll(cachePath) }()

	dirs, err := discoverHarnessConfigDirs(cachePath)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion harness-configs found at %s", sourceURL)
	}

	return s.importHarnessConfigDirs(ctx, dirs, projectID), nil
}

// importHarnessConfigsFromWorkspace imports harness-configs from a path within
// the project's workspace filesystem. The workspacePath is relative to the
// project's workspace root (e.g. "/.scion/harness-configs").
func (s *Server) importHarnessConfigsFromWorkspace(ctx context.Context, project *store.Project, workspacePath string) ([]string, error) {
	stor := s.GetStorage()
	if stor == nil {
		return nil, fmt.Errorf("harness-config storage is not configured")
	}

	// Resolve the project's workspace root on disk.
	projectRoot, err := s.resolveProjectWebDAVPath(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project workspace: %w", err)
	}

	// Clean and join the workspace path to the project root.
	rel := strings.TrimPrefix(filepath.Clean(workspacePath), "/")
	configsDir := filepath.Join(projectRoot, rel)

	// Validate the resolved path is within the project root.
	absRoot, _ := filepath.Abs(projectRoot)
	absDir, _ := filepath.Abs(configsDir)
	relPath, err := filepath.Rel(absRoot, absDir)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return nil, fmt.Errorf("workspace path must be within the project workspace")
	}

	info, err := os.Stat(configsDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace path not found or not a directory: %s", workspacePath)
	}

	dirs, err := discoverHarnessConfigDirs(configsDir)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion harness-configs found at workspace path %s", workspacePath)
	}

	return s.importHarnessConfigDirs(ctx, dirs, project.ID), nil
}

// harnessConfigDir pairs a config's directory name with its on-disk path.
type harnessConfigDir struct{ name, path string }

// discoverHarnessConfigDirs returns the harness-config directories at root. If
// root itself is a harness-config it is returned directly; otherwise its
// immediate subdirectories are scanned.
func discoverHarnessConfigDirs(root string) ([]harnessConfigDir, error) {
	if isHarnessConfigDir(root) {
		return []harnessConfigDir{{filepath.Base(root), root}}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []harnessConfigDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if isHarnessConfigDir(dir) {
			dirs = append(dirs, harnessConfigDir{entry.Name(), dir})
		}
	}
	return dirs, nil
}

// importHarnessConfigDirs imports or force-syncs each discovered harness-config
// directory into the project scope. Configs that fail to load or import are
// logged and skipped. Returns the names successfully imported or updated.
func (s *Server) importHarnessConfigDirs(ctx context.Context, dirs []harnessConfigDir, projectID string) []string {
	stor := s.GetStorage()
	var imported []string
	for _, hc := range dirs {
		hcDir, err := config.LoadHarnessConfigDir(hc.path)
		if err != nil {
			s.templateLog.Warn("harness-config import: failed to load config, skipping",
				"config", hc.name, "error", err)
			continue
		}

		slug := api.Slugify(hc.name)
		existing, err := s.store.GetHarnessConfigBySlug(ctx, slug, store.HarnessConfigScopeProject, projectID)
		if err != nil && err != store.ErrNotFound {
			s.templateLog.Warn("harness-config import: failed to look up config, skipping",
				"config", hc.name, "error", err)
			continue
		}

		if existing == nil {
			if err := s.bootstrapSingleHarnessConfigScoped(ctx, hc.name, hc.path, hcDir, stor, store.HarnessConfigScopeProject, projectID); err != nil {
				s.templateLog.Warn("harness-config import: failed to import config, skipping",
					"config", hc.name, "error", err)
				continue
			}
		} else {
			if _, err := s.syncExistingHarnessConfig(ctx, existing, hc.path, hcDir, stor, true); err != nil {
				s.templateLog.Warn("harness-config import: failed to sync config, skipping",
					"config", hc.name, "error", err)
				continue
			}
		}
		imported = append(imported, hc.name)
	}
	return imported
}

// syncExistingHarnessConfig re-syncs a local harness config directory through
// the shared ResourceStore. Returns true if the stored content changed. When
// force is true the config is re-uploaded and storage reconciled even if the
// content hash is unchanged (used by direct imports).
func (s *Server) syncExistingHarnessConfig(ctx context.Context, existing *store.HarnessConfig, dirPath string, hcDir *config.HarnessConfigDir, _ storage.Storage, force bool) (bool, error) {
	return s.harnessConfigStore(hcDir.Config.Harness).Bootstrap(ctx, existing.Name, dirPath, existing.Scope, existing.ScopeID, force)
}
