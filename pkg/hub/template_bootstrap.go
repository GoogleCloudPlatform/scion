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
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
//
// This now delegates to the shared ResourceStore (§7.3); the template-specific
// behavior (harness detection, DefaultHarnessConfig backfill, bundled
// harness-config import) lives in templatePersistence.
func (s *Server) syncExistingTemplate(ctx context.Context, existing *store.Template, templatePath string, force bool) (bool, error) {
	return s.templateStore().Bootstrap(ctx, existing.Name, templatePath, existing.Scope, existing.ScopeID, "", force)
}

// bootstrapSingleTemplate imports one local template directory into the
// Hub's database and storage backend under the given scope and projectID.
// For global templates pass store.TemplateScopeGlobal and "".
func (s *Server) bootstrapSingleTemplate(ctx context.Context, name, templatePath, scope, projectID string) error {
	_, err := s.templateStore().Bootstrap(ctx, name, templatePath, scope, projectID, "", false)
	return err
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
// Configs are scoped to match the template's scope (global or project).
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
	if scope == string(store.TemplateScopeProject) {
		hcScope = store.HarnessConfigScopeProject
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
			if _, err := s.syncExistingHarnessConfig(ctx, existing, dirPath, hcDirCfg, stor, false); err != nil {
				s.templateLog.Warn("template harness-config import: failed to sync, skipping",
					"config", name, "error", err)
			}
		}
	}
}

// importTemplatesFromRemote fetches a remote source URL, discovers scion
// templates within it, and registers each one into the Hub store scoped
// to the given project. Returns the names of all templates imported or updated.
func (s *Server) importTemplatesFromRemote(ctx context.Context, projectID, sourceURL string) ([]string, error) {
	return s.importFromRemote(ctx, projectID, sourceURL, store.TemplateScopeProject, s.templateImportKind(), nil, nil)
}

// importTemplatesFromWorkspace imports templates from a path within the
// project's workspace filesystem. The workspacePath is relative to the project's
// workspace root (e.g. "/.scion/templates" or "/my/custom/path").
func (s *Server) importTemplatesFromWorkspace(ctx context.Context, project *store.Project, workspacePath string) ([]string, error) {
	return s.importFromWorkspace(ctx, project, workspacePath, store.TemplateScopeProject, s.templateImportKind(), nil, nil)
}
