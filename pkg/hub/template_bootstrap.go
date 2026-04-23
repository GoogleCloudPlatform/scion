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
	"github.com/GoogleCloudPlatform/scion/pkg/config/templateimport"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// BootstrapTemplatesFromDir imports or updates local templates from a directory
// into the Hub's database and storage. On first run it imports all templates;
// on subsequent runs it detects changed templates (by content hash) and
// re-uploads only those that differ from the database version.
func (s *Server) BootstrapTemplatesFromDir(ctx context.Context, templatesDir string) error {
	// Check if the directory exists
	info, err := os.Stat(templatesDir)
	if err != nil || !info.IsDir() {
		s.templateLog.Debug("template bootstrap: directory not found, skipping", "dir", templatesDir)
		return nil
	}

	// Check that storage is configured
	stor := s.GetStorage()
	if stor == nil {
		s.templateLog.Warn("template bootstrap: no storage backend configured, skipping")
		return nil
	}

	// Scan the directory for template subdirectories
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		return err
	}

	imported, updated := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		templatePath := filepath.Join(templatesDir, name)
		slug := api.Slugify(name)

		// Check if this template already exists in the database
		existing, err := s.store.GetTemplateBySlug(ctx, slug, string(store.TemplateScopeGlobal), "")
		if err != nil && err != store.ErrNotFound {
			s.templateLog.Warn("template bootstrap: failed to look up template, skipping",
				"template", name, "error", err)
			continue
		}

		if existing == nil {
			// New template — import it
			if err := s.bootstrapSingleTemplate(ctx, name, templatePath, store.TemplateScopeGlobal, ""); err != nil {
				s.templateLog.Warn("template bootstrap: failed to import template, skipping",
					"template", name, "error", err)
				continue
			}
			imported++
		} else {
			// Existing template — check if local files have changed
			changed, err := s.syncExistingTemplate(ctx, existing, templatePath, false)
			if err != nil {
				s.templateLog.Warn("template bootstrap: failed to sync template, skipping",
					"template", name, "error", err)
				continue
			}
			if changed {
				updated++
			}
		}
	}

	if imported > 0 || updated > 0 {
		s.templateLog.Info("template bootstrap: sync complete",
			"imported", imported, "updated", updated)
	}

	return nil
}

// syncExistingTemplate re-uploads a local template directory into the Hub's
// storage and updates the database record. When force is true (e.g. an
// explicit re-import from a remote URL), it always re-uploads all files and
// reconciles the storage backend by deleting any objects under the template's
// storage prefix that are not in the new manifest. When force is false (e.g.
// the periodic bootstrap-from-disk path on hub start), it short-circuits if
// the aggregate content hash already matches what is stored. The bool return
// reports whether the resulting ContentHash differed from what was previously
// stored.
func (s *Server) syncExistingTemplate(ctx context.Context, existing *store.Template, templatePath string, force bool) (bool, error) {
	stor := s.GetStorage()

	// Collect current files from disk
	files, err := transfer.CollectFiles(templatePath, nil)
	if err != nil {
		return false, err
	}

	if !force {
		var preview []store.TemplateFile
		for _, fi := range files {
			preview = append(preview, store.TemplateFile{
				Path: fi.Path,
				Size: fi.Size,
				Hash: fi.Hash,
				Mode: fi.Mode,
			})
		}
		hashMatch := computeContentHash(preview) == existing.ContentHash

		// Even when content hasn't changed, backfill DefaultHarnessConfig
		// for templates imported before that field existed.
		if hashMatch && existing.DefaultHarnessConfig == "" {
			cfgInfo := detectHarnessFromConfig(templatePath, existing.Name)
			if cfgInfo.DefaultHarnessConfig != "" {
				existing.DefaultHarnessConfig = cfgInfo.DefaultHarnessConfig
				existing.Harness = cfgInfo.Harness
				_ = s.store.UpdateTemplate(ctx, existing)
				s.importTemplateHarnessConfigs(ctx, templatePath, existing.Scope, existing.ScopeID)
				s.templateLog.Info("template bootstrap: backfilled defaultHarnessConfig",
					"template", existing.Name, "defaultHarnessConfig", cfgInfo.DefaultHarnessConfig)
			}
		}

		if hashMatch {
			return false, nil
		}
	}

	storagePath := existing.StoragePath
	if storagePath == "" {
		storagePath = storage.TemplateStoragePath(existing.Scope, existing.ScopeID, existing.Slug)
	}

	var uploadedFiles []store.TemplateFile
	newPaths := make(map[string]struct{}, len(files))
	for _, fi := range files {
		objectPath := storagePath + "/" + fi.Path

		f, err := os.Open(fi.FullPath)
		if err != nil {
			s.templateLog.Warn("template bootstrap: failed to open file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		_, err = stor.Upload(ctx, objectPath, f, storage.UploadOptions{})
		f.Close()
		if err != nil {
			s.templateLog.Warn("template bootstrap: failed to upload file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		uploadedFiles = append(uploadedFiles, store.TemplateFile{
			Path: fi.Path,
			Size: fi.Size,
			Hash: fi.Hash,
			Mode: fi.Mode,
		})
		newPaths[objectPath] = struct{}{}
	}

	// Reconcile storage: delete objects under the template's prefix that are
	// no longer in the new manifest, so removed files don't linger.
	if listResult, err := stor.List(ctx, storage.ListOptions{Prefix: storagePath + "/"}); err != nil {
		s.templateLog.Warn("template bootstrap: failed to list storage for reconcile",
			"template", existing.Name, "prefix", storagePath, "error", err)
	} else {
		for _, obj := range listResult.Objects {
			if _, keep := newPaths[obj.Name]; keep {
				continue
			}
			if err := stor.Delete(ctx, obj.Name); err != nil {
				s.templateLog.Warn("template bootstrap: failed to delete stale object",
					"template", existing.Name, "object", obj.Name, "error", err)
			}
		}
	}

	newHash := computeContentHash(uploadedFiles)
	changed := newHash != existing.ContentHash

	if changed {
		s.templateLog.Info("template bootstrap: template re-synced",
			"template", existing.Name, "oldHash", existing.ContentHash, "newHash", newHash)
	}

	// Update the database record with new files and hash
	existing.Files = uploadedFiles
	existing.ContentHash = newHash
	cfgInfo := detectHarnessFromConfig(templatePath, existing.Name)
	existing.Harness = cfgInfo.Harness
	existing.DefaultHarnessConfig = cfgInfo.DefaultHarnessConfig

	if err := s.store.UpdateTemplate(ctx, existing); err != nil {
		return false, err
	}

	// Re-import any harness-configs bundled inside the template
	s.importTemplateHarnessConfigs(ctx, templatePath, existing.Scope, existing.ScopeID)

	return changed, nil
}

// bootstrapSingleTemplate imports one local template directory into the
// Hub's database and storage backend under the given scope and groveID.
// For global templates pass store.TemplateScopeGlobal and "".
func (s *Server) bootstrapSingleTemplate(ctx context.Context, name, templatePath, scope, groveID string) error {
	stor := s.GetStorage()

	// Collect files from the template directory
	files, err := transfer.CollectFiles(templatePath, nil)
	if err != nil {
		return err
	}

	// Detect harness type and default harness config from the template config
	cfgInfo := detectHarnessFromConfig(templatePath, name)

	slug := api.Slugify(name)

	// Create a pending template record
	storagePath := storage.TemplateStoragePath(scope, groveID, slug)
	tmpl := &store.Template{
		ID:                   api.NewUUID(),
		Name:                 name,
		Slug:                 slug,
		Harness:              cfgInfo.Harness,
		DefaultHarnessConfig: cfgInfo.DefaultHarnessConfig,
		Scope:                scope,
		ScopeID:              groveID,
		GroveID:              groveID, // deprecated alias kept for compatibility
		Status:               store.TemplateStatusPending,
		StoragePath:          storagePath,
		StorageBucket:        stor.Bucket(),
		StorageURI:           storage.TemplateStorageURI(stor.Bucket(), scope, groveID, slug),
		Visibility:           store.VisibilityPrivate,
	}

	if err := s.store.CreateTemplate(ctx, tmpl); err != nil {
		return err
	}

	// Upload each file to storage
	var templateFiles []store.TemplateFile
	for _, fi := range files {
		objectPath := storagePath + "/" + fi.Path

		f, err := os.Open(fi.FullPath)
		if err != nil {
			s.templateLog.Warn("template bootstrap: failed to open file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		_, err = stor.Upload(ctx, objectPath, f, storage.UploadOptions{})
		f.Close()
		if err != nil {
			s.templateLog.Warn("template bootstrap: failed to upload file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		templateFiles = append(templateFiles, store.TemplateFile{
			Path: fi.Path,
			Size: fi.Size,
			Hash: fi.Hash,
			Mode: fi.Mode,
		})
	}

	// Compute content hash and activate the template
	contentHash := computeContentHash(templateFiles)
	tmpl.Files = templateFiles
	tmpl.ContentHash = contentHash
	tmpl.Status = store.TemplateStatusActive

	if err := s.store.UpdateTemplate(ctx, tmpl); err != nil {
		return err
	}

	s.templateLog.Info("template bootstrap: imported template",
		"name", name, "files", len(templateFiles), "harness", cfgInfo.Harness,
		"defaultHarnessConfig", cfgInfo.DefaultHarnessConfig)

	// Import any harness-configs bundled inside the template
	s.importTemplateHarnessConfigs(ctx, templatePath, scope, groveID)

	return nil
}

// templateConfigInfo holds the harness type and default harness config name
// extracted from a template's scion-agent.yaml.
type templateConfigInfo struct {
	Harness              string // inferred harness type (claude, gemini, etc.)
	DefaultHarnessConfig string // actual harness-config name from config (e.g. "claude-web", "adk")
}

// detectHarnessFromConfig reads a template's config and returns the harness type
// and the default harness config name. The harness type is inferred from the
// config name or explicit harness field. The default harness config name preserves
// the original value from scion-agent.yaml so it can be used for hub resolution.
func detectHarnessFromConfig(templatePath, templateName string) templateConfigInfo {
	t := &config.Template{Name: templateName, Path: templatePath}
	cfg, err := t.LoadConfig()
	if err == nil && cfg != nil {
		if cfg.HarnessConfig != "" {
			return templateConfigInfo{
				Harness:              inferHarnessFromName(cfg.HarnessConfig),
				DefaultHarnessConfig: cfg.HarnessConfig,
			}
		}
		if cfg.DefaultHarnessConfig != "" {
			return templateConfigInfo{
				Harness:              inferHarnessFromName(cfg.DefaultHarnessConfig),
				DefaultHarnessConfig: cfg.DefaultHarnessConfig,
			}
		}
		if cfg.Harness != "" {
			return templateConfigInfo{Harness: cfg.Harness}
		}
	}

	return templateConfigInfo{Harness: inferHarnessFromName(templateName)}
}

// inferHarnessFromName guesses the harness type from a name string.
func inferHarnessFromName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "claude"):
		return "claude"
	case strings.Contains(lower, "gemini"):
		return "gemini"
	case strings.Contains(lower, "opencode"):
		return "opencode"
	case strings.Contains(lower, "codex"):
		return "codex"
	default:
		return ""
	}
}

// importTemplateHarnessConfigs imports harness-configs bundled inside a
// template's harness-configs/ subdirectory into the Hub's harness-config store.
// Configs are scoped to match the template's scope (global or grove).
func (s *Server) importTemplateHarnessConfigs(ctx context.Context, templatePath, scope, scopeID string) {
	hcDir := filepath.Join(templatePath, "harness-configs")
	info, err := os.Stat(hcDir)
	if err != nil || !info.IsDir() {
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		return
	}

	entries, err := os.ReadDir(hcDir)
	if err != nil {
		return
	}

	hcScope := store.HarnessConfigScopeGlobal
	if scope == string(store.TemplateScopeGrove) {
		hcScope = store.HarnessConfigScopeGrove
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		dirPath := filepath.Join(hcDir, name)
		slug := api.Slugify(name)

		hcDirCfg, err := config.LoadHarnessConfigDir(dirPath)
		if err != nil {
			s.templateLog.Debug("template harness-config import: failed to load config, skipping",
				"config", name, "error", err)
			continue
		}

		existing, err := s.store.GetHarnessConfigBySlug(ctx, slug, hcScope, scopeID)
		if err != nil && err != store.ErrNotFound {
			continue
		}

		if existing == nil {
			if err := s.bootstrapSingleHarnessConfigScoped(ctx, name, dirPath, hcDirCfg, stor, hcScope, scopeID); err != nil {
				s.templateLog.Warn("template harness-config import: failed to import, skipping",
					"config", name, "error", err)
				continue
			}
			s.templateLog.Info("template harness-config import: imported config",
				"config", name, "harness", hcDirCfg.Config.Harness, "scope", hcScope)
		} else {
			if _, err := s.syncExistingHarnessConfig(ctx, existing, dirPath, hcDirCfg, stor); err != nil {
				s.templateLog.Warn("template harness-config import: failed to sync, skipping",
					"config", name, "error", err)
			}
		}
	}
}

// importTemplatesFromRemote fetches a remote source URL, discovers scion
// templates within it, and registers each one into the Hub store scoped
// to the given grove. Returns the names of all templates imported or updated.
func (s *Server) importTemplatesFromRemote(ctx context.Context, groveID, sourceURL string) ([]string, error) {
	if !config.IsRemoteURI(sourceURL) {
		return nil, fmt.Errorf("source must be a remote URI (http://, https://, or rclone)")
	}

	stor := s.GetStorage()
	if stor == nil {
		return nil, fmt.Errorf("template storage is not configured")
	}

	// If the grove has a GitHub App installation, mint a token for authenticated access
	var authToken string
	grove, err := s.store.GetGrove(ctx, groveID)
	if err == nil && grove != nil && grove.GitHubInstallationID != nil {
		if token, _, mintErr := s.MintGitHubAppTokenForGrove(ctx, grove); mintErr == nil && token != "" {
			authToken = token
		}
	}

	// Fetch to a temporary directory
	cachePath, err := config.FetchRemoteTemplate(ctx, sourceURL, authToken)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch remote templates: %w", err)
	}
	defer os.RemoveAll(cachePath)

	// Collect template directories to import
	type templateDir struct{ name, path string }
	var dirs []templateDir

	if templateimport.IsScionTemplate(cachePath) {
		// URL pointed directly at a single template directory
		dirs = append(dirs, templateDir{filepath.Base(cachePath), cachePath})
	} else {
		entries, err := os.ReadDir(cachePath)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(cachePath, entry.Name())
			if templateimport.IsScionTemplate(dir) {
				dirs = append(dirs, templateDir{entry.Name(), dir})
			}
		}
	}

	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion templates found at %s", sourceURL)
	}

	var imported []string
	for _, td := range dirs {
		slug := api.Slugify(td.name)
		existing, err := s.store.GetTemplateBySlug(ctx, slug, store.TemplateScopeGrove, groveID)
		if err != nil && err != store.ErrNotFound {
			s.templateLog.Warn("template import: failed to look up template, skipping",
				"name", td.name, "error", err)
			continue
		}
		if existing == nil {
			if err := s.bootstrapSingleTemplate(ctx, td.name, td.path, store.TemplateScopeGrove, groveID); err != nil {
				s.templateLog.Warn("template import: failed to import template, skipping",
					"name", td.name, "error", err)
				continue
			}
		} else {
			if _, err := s.syncExistingTemplate(ctx, existing, td.path, true); err != nil {
				s.templateLog.Warn("template import: failed to sync template, skipping",
					"name", td.name, "error", err)
				continue
			}
		}
		imported = append(imported, td.name)
	}
	return imported, nil
}

// importTemplatesFromWorkspace imports templates from a path within the
// grove's workspace filesystem. The workspacePath is relative to the grove's
// workspace root (e.g. "/.scion/templates" or "/my/custom/path").
func (s *Server) importTemplatesFromWorkspace(ctx context.Context, grove *store.Grove, workspacePath string) ([]string, error) {
	stor := s.GetStorage()
	if stor == nil {
		return nil, fmt.Errorf("template storage is not configured")
	}

	// Resolve the grove's workspace root on disk
	groveRoot, err := s.resolveGroveWebDAVPath(ctx, grove)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve grove workspace: %w", err)
	}

	// Clean and join the workspace path to the grove root.
	// Strip leading slash so it joins correctly.
	rel := strings.TrimPrefix(filepath.Clean(workspacePath), "/")
	templatesDir := filepath.Join(groveRoot, rel)

	// Validate the resolved path is within the grove root
	absRoot, _ := filepath.Abs(groveRoot)
	absDir, _ := filepath.Abs(templatesDir)
	if !strings.HasPrefix(absDir, absRoot) {
		return nil, fmt.Errorf("workspace path must be within the grove workspace")
	}

	info, err := os.Stat(templatesDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace path not found or not a directory: %s", workspacePath)
	}

	// Collect template directories to import (same logic as importTemplatesFromRemote)
	type templateDir struct{ name, path string }
	var dirs []templateDir

	if templateimport.IsScionTemplate(templatesDir) {
		dirs = append(dirs, templateDir{filepath.Base(templatesDir), templatesDir})
	} else {
		entries, readErr := os.ReadDir(templatesDir)
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(templatesDir, entry.Name())
			if templateimport.IsScionTemplate(dir) {
				dirs = append(dirs, templateDir{entry.Name(), dir})
			}
		}
	}

	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion templates found at workspace path %s", workspacePath)
	}

	var imported []string
	for _, td := range dirs {
		slug := api.Slugify(td.name)
		existing, lookupErr := s.store.GetTemplateBySlug(ctx, slug, store.TemplateScopeGrove, grove.ID)
		if lookupErr != nil && lookupErr != store.ErrNotFound {
			s.templateLog.Warn("workspace template import: failed to look up template, skipping",
				"name", td.name, "error", lookupErr)
			continue
		}
		if existing == nil {
			if bootstrapErr := s.bootstrapSingleTemplate(ctx, td.name, td.path, store.TemplateScopeGrove, grove.ID); bootstrapErr != nil {
				s.templateLog.Warn("workspace template import: failed to import template, skipping",
					"name", td.name, "error", bootstrapErr)
				continue
			}
		} else {
			if _, syncErr := s.syncExistingTemplate(ctx, existing, td.path, true); syncErr != nil {
				s.templateLog.Warn("workspace template import: failed to sync template, skipping",
					"name", td.name, "error", syncErr)
				continue
			}
		}
		imported = append(imported, td.name)
	}
	return imported, nil
}
