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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/templateimport"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// resource_import.go is the Phase-1 landing of the resource-import refactor: a
// single kind-generic, source-generic import driver that sits *above* the
// shared ResourceStore.Bootstrap and *replaces* the four near-identical
// import*From{Remote,Workspace} functions that templates and harness-configs
// used to carry separately.
//
// The per-kind quirks stay in small closures bundled in resourceImportKind
// (which marker file identifies a resource directory, and which ResourceStore
// persists it); everything else — remote fetch + auth, discovery, leaf-vs-parent
// naming, the create-or-sync loop — is shared here.

// resourceDir pairs a derived resource name with its on-disk directory.
type resourceDir struct{ name, path string }

// resourceImportKind bundles the per-kind knobs the shared import driver needs.
// Construct one via Server.templateImportKind / Server.harnessConfigImportKind.
type resourceImportKind struct {
	// noun names the kind in log lines and "no scion <noun> found" errors
	// (e.g. "templates", "harness-configs").
	noun string
	// isResourceDir reports whether a directory is a resource of this kind, by
	// checking for the kind's marker file (scion-agent.yaml / config.yaml).
	isResourceDir func(dir string) bool
	// newStore builds the ResourceStore that persists a directory of this kind.
	// For harness-configs this loads config.yaml to resolve the harness type, so
	// it can fail; failures cause that directory to be skipped.
	newStore func(dir string) (*ResourceStore, error)
}

// templateImportKind returns the import knobs for templates.
func (s *Server) templateImportKind() resourceImportKind {
	return resourceImportKind{
		noun:          "templates",
		isResourceDir: templateimport.IsScionTemplate,
		newStore:      func(string) (*ResourceStore, error) { return s.templateStore(), nil },
	}
}

// harnessConfigImportKind returns the import knobs for harness-configs.
func (s *Server) harnessConfigImportKind() resourceImportKind {
	return resourceImportKind{
		noun:          "harness-configs",
		isResourceDir: isHarnessConfigDir,
		newStore: func(dir string) (*ResourceStore, error) {
			hcDir, err := config.LoadHarnessConfigDir(dir)
			if err != nil {
				return nil, err
			}
			return s.harnessConfigStore(hcDir.Config.Harness), nil
		},
	}
}

// importFromRemote fetches a remote source URL, discovers resources of the given
// kind within it, and create-or-syncs each into the store under the project
// scope. Returns the names of all resources imported or updated.
func (s *Server) importFromRemote(ctx context.Context, projectID, sourceURL, scope string, kind resourceImportKind) ([]string, error) {
	if !config.IsRemoteURI(sourceURL) {
		return nil, fmt.Errorf("source must be a remote URI (http://, https://, or rclone)")
	}
	if s.GetStorage() == nil {
		return nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	cachePath, err := s.fetchRemoteForImport(ctx, projectID, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch remote %s: %w", kind.noun, err)
	}
	defer func() { _ = os.RemoveAll(cachePath) }()

	dirs, err := discoverResourceDirs(cachePath, sourceURL, kind.isResourceDir)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion %s found at %s", kind.noun, sourceURL)
	}

	return s.importResourceDirs(ctx, dirs, scope, projectID, kind), nil
}

// importFromWorkspace imports resources of the given kind from a path within the
// project's workspace filesystem. workspacePath is relative to the project's
// workspace root (e.g. "/.scion/templates").
func (s *Server) importFromWorkspace(ctx context.Context, project *store.Project, workspacePath, scope string, kind resourceImportKind) ([]string, error) {
	if s.GetStorage() == nil {
		return nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	// Resolve the project's workspace root on disk.
	projectRoot, err := s.resolveProjectWebDAVPath(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project workspace: %w", err)
	}

	// Clean and join the workspace path to the project root. Strip the leading
	// slash so it joins relative to the root.
	rel := strings.TrimPrefix(filepath.Clean(workspacePath), "/")
	resourcesDir := filepath.Join(projectRoot, rel)

	// Validate the resolved path is within the project root.
	absRoot, _ := filepath.Abs(projectRoot)
	absDir, _ := filepath.Abs(resourcesDir)
	relPath, err := filepath.Rel(absRoot, absDir)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return nil, fmt.Errorf("workspace path must be within the project workspace")
	}

	info, err := os.Stat(resourcesDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace path not found or not a directory: %s", workspacePath)
	}

	// Workspace dirs are real directories, so pass "" as sourceURL: the leaf's
	// own base name is correct (no content-hash cache directory in play).
	dirs, err := discoverResourceDirs(resourcesDir, "", kind.isResourceDir)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion %s found at workspace path %s", kind.noun, workspacePath)
	}

	return s.importResourceDirs(ctx, dirs, scope, project.ID, kind), nil
}

// fetchRemoteForImport fetches a remote source URL to a local cache directory,
// authenticating against GitHub when possible. It first tries a GitHub App
// installation token for the project, then falls back to the project's
// GITHUB_TOKEN secret. The fallback applies to both resource kinds — closing the
// gap where harness-config remote import previously skipped the secret fallback.
// The caller owns the returned cache path and must remove it.
//
// projectID may be "" for global-scope (hub-level) imports, which have no project
// to authenticate against; in that case the fetch is unauthenticated.
func (s *Server) fetchRemoteForImport(ctx context.Context, projectID, sourceURL string) (string, error) {
	var authToken string

	if projectID != "" {
		// Prefer a GitHub App installation token if the project has one.
		project, err := s.store.GetProject(ctx, projectID)
		if err == nil && project != nil && project.GitHubInstallationID != nil {
			if token, _, mintErr := s.MintGitHubAppTokenForProject(ctx, project); mintErr == nil && token != "" {
				authToken = token
			}
		}
	}

	// Fall back to the project GITHUB_TOKEN secret if no App token was minted.
	if authToken == "" && projectID != "" {
		if sb := s.GetSecretBackend(); sb != nil {
			sec, secErr := sb.Get(ctx, "GITHUB_TOKEN", secret.ScopeProject, projectID)
			if secErr == nil && sec != nil && sec.Value != "" {
				authToken = sec.Value
				s.templateLog.Info("using project GITHUB_TOKEN for resource import", "projectID", projectID)
			} else if secErr != nil && !errors.Is(secErr, store.ErrNotFound) {
				s.templateLog.Warn("Failed to retrieve GITHUB_TOKEN from secret backend", "projectID", projectID, "error", secErr)
			}
		}
	}

	return config.FetchRemoteTemplate(ctx, sourceURL, authToken)
}

// discoverResourceDirs returns the resource directories at root, classifying by
// the kind's marker file (via isResourceDir). It owns both the leaf-vs-parent
// decision and per-branch naming:
//
//   - Leaf: root itself is a resource. Its name comes from the source URL's leaf
//     segment when sourceURL is set (the remote cache dir is a content hash, so
//     filepath.Base(root) would be the hash); for workspace imports pass
//     sourceURL == "" so the real directory's base name is used.
//   - Parent: root's immediate children are scanned; each child that is a
//     resource is named by its own directory name. Children without the marker
//     file are skipped.
func discoverResourceDirs(root, sourceURL string, isResourceDir func(string) bool) ([]resourceDir, error) {
	if isResourceDir(root) {
		name := filepath.Base(root)
		if sourceURL != "" {
			if derived := config.DeriveResourceName(sourceURL); derived != "" {
				name = derived
			}
		}
		return []resourceDir{{name, root}}, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []resourceDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if isResourceDir(dir) {
			dirs = append(dirs, resourceDir{entry.Name(), dir})
		}
	}
	return dirs, nil
}

// importResourceDirs create-or-syncs each discovered directory into the store
// under the given scope. Directories that fail to build a store (e.g. an
// unreadable harness config.yaml) or fail to import are logged and skipped.
// Returns the names successfully imported or updated.
//
// Each directory is force-synced (force=true): a re-import always re-uploads and
// reconciles storage, matching the prior direct-import behavior. For a
// not-yet-existing resource the force flag is irrelevant — Bootstrap creates it.
func (s *Server) importResourceDirs(ctx context.Context, dirs []resourceDir, scope, scopeID string, kind resourceImportKind) []string {
	var imported []string
	for _, rd := range dirs {
		rstore, err := kind.newStore(rd.path)
		if err != nil {
			s.templateLog.Warn(kind.noun+" import: failed to load resource, skipping",
				"name", rd.name, "error", err)
			continue
		}
		if _, err := rstore.Bootstrap(ctx, rd.name, rd.path, scope, scopeID, true); err != nil {
			s.templateLog.Warn(kind.noun+" import: failed to import resource, skipping",
				"name", rd.name, "error", err)
			continue
		}
		imported = append(imported, rd.name)
	}
	return imported
}
