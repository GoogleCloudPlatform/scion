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
	"log/slog"
	"os"
	"path/filepath"

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
		s.resourceLog.Debug("harness config bootstrap: directory not found, skipping", "dir", harnessConfigsDir)
		return nil
	}

	if s.GetStorage() == nil {
		s.resourceLog.Warn("harness config bootstrap: no storage backend configured, skipping")
		return nil
	}

	entries, err := os.ReadDir(harnessConfigsDir)
	if err != nil {
		return err
	}

	// Seeded-built-ins ledger (ptone/scion#3544): a deleted built-in stays
	// deleted even though UpdateDefaultTemplates re-materializes it on disk.
	// If the ledger cannot be loaded, fail closed for built-ins only: no
	// missing built-in is created, but user dirs are still imported and
	// existing rows still synced. The load error is returned at the end.
	ledger, loadErr := s.loadBuiltinSeedLedger(ctx)
	if loadErr != nil {
		loadErr = fmt.Errorf("harness config bootstrap: %w", loadErr)
		s.resourceLog.Error("harness config bootstrap: cannot load built-in seed ledger; not creating missing built-ins this run",
			"error", loadErr)
		ledger = newFailClosedBuiltinSeedLedger()
	}
	const kind = storage.ResourceKindHarnessConfig

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
			s.resourceLog.Warn("harness config bootstrap: failed to load config, skipping",
				"config", name, "error", err)
			continue
		}

		existing, err := s.store.GetHarnessConfigBySlug(ctx, slug, store.HarnessConfigScopeGlobal, "")
		if err != nil && err != store.ErrNotFound {
			s.resourceLog.Warn("harness config bootstrap: failed to look up config, skipping",
				"config", name, "error", err)
			continue
		}

		builtin := isBuiltinName(kind, slug)
		if existing == nil {
			if builtin && ledger.Seen(kind, slug) {
				s.resourceLog.Info("harness config bootstrap: built-in previously deleted; not re-seeding",
					"config", name)
				continue
			}
			if err := s.bootstrapSingleHarnessConfig(ctx, name, dirPath, hcDir, store.HarnessConfigScopeGlobal, ""); err != nil {
				s.resourceLog.Warn("harness config bootstrap: failed to import config, skipping",
					"config", name, "error", err)
				continue
			}
			if builtin {
				ledger.Mark(kind, slug)
			}
			imported++
		} else {
			// A row exists, so the name counts as seeded even if the
			// sync below fails (same rule as the hosted path).
			if builtin {
				ledger.Mark(kind, slug)
			}
			oldHash := existing.ContentHash
			changed, err := s.syncExistingHarnessConfig(ctx, existing, dirPath, hcDir, false)
			if err != nil {
				s.resourceLog.Warn("harness config bootstrap: failed to sync config, skipping",
					"config", name, "error", err)
				continue
			}
			if changed {
				updated++
				warnBootstrapOverwrite(s.resourceLog, "harness-config", name, existing.ID, dirPath, oldHash,
					s.currentHarnessConfigHash(ctx, existing.ID))
			}
		}
	}

	if imported > 0 || updated > 0 {
		s.resourceLog.Info("harness config bootstrap: sync complete",
			"imported", imported, "updated", updated)
	}

	if loadErr != nil {
		return loadErr
	}
	return s.saveBuiltinSeedLedger(ctx, ledger)
}

// bootstrapSingleHarnessConfig imports one local harness config directory into
// the Hub's database and storage backend through the shared ResourceStore.
func (s *Server) bootstrapSingleHarnessConfig(ctx context.Context, name, dirPath string, hcDir *config.HarnessConfigDir, scope, scopeID string) error {
	_, err := s.harnessConfigStore(hcDir.Config.Harness).Bootstrap(ctx, name, dirPath, scope, scopeID, "", false)
	return err
}

// isHarnessConfigDir reports whether dir looks like a harness-config directory,
// i.e. it contains a config.yaml file. Analogous to
// templateimport.IsScionTemplate (which checks for scion-agent.yaml).
func isHarnessConfigDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "config.yaml"))
	return err == nil && !info.IsDir()
}

// syncExistingHarnessConfig re-syncs a local harness config directory through
// the shared ResourceStore. Returns true if the stored content changed. When
// force is true the config is re-uploaded and storage reconciled even if the
// content hash is unchanged (used by direct imports).
func (s *Server) syncExistingHarnessConfig(ctx context.Context, existing *store.HarnessConfig, dirPath string, hcDir *config.HarnessConfigDir, force bool) (bool, error) {
	return s.harnessConfigStore(hcDir.Config.Harness).Bootstrap(ctx, existing.Name, dirPath, existing.Scope, existing.ScopeID, "", force)
}

// warnBootstrapOverwrite reports that the workstation (non-hosted) startup
// bootstrap replaced an existing hub record's content with the local
// ~/.scion copy (ptone/scion#611, workstation-mode overwrite). Any edit made
// through the hub (web UI, API) since the last import is lost at that point.
//
// The message states only what is known. Most overwrites are benign — e.g.
// a binary upgrade that changed a bundled default, which
// UpdateDefaultTemplates re-materializes on disk at every workstation start
// — and the hub cannot currently tell those apart from a lost hub-side edit:
// harness-config edit handlers do not set UpdatedBy; template edit handlers
// do, but this bootstrap never clears it, so a set UpdatedBy means "edited
// at some point", not "edited since the last import"; Updated is bumped by
// every write (including this bootstrap, storage repair and image-status
// checks); and nothing records the hash the last import wrote.
// Distinguishing them needs that marker persisted on the record
// (ResourceStore/schema work, out of scope here). Until then this stays at
// WARN because the destructive case is silent otherwise.
//
// logger is the caller's subsystem logger (resourceLog for harness-configs,
// templateLog for templates) so the WARN lands next to that bootstrap's
// other log lines.
func warnBootstrapOverwrite(logger *slog.Logger, kind, name, id, dir, oldHash, newHash string) {
	logger.Warn("workstation bootstrap: hub record replaced from local disk copy; "+
		"any hub-side edits made since the last import are lost",
		"kind", kind, "name", name, "id", id, "dir", dir,
		"oldHash", oldHash, "newHash", newHash)
}

// currentHarnessConfigHash re-reads a harness-config's stored content hash
// (for logging); returns "" when it cannot be read.
func (s *Server) currentHarnessConfigHash(ctx context.Context, id string) string {
	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil || hc == nil {
		return ""
	}
	return hc.ContentHash
}

// currentTemplateHash re-reads a template's stored content hash (for
// logging); returns "" when it cannot be read.
func (s *Server) currentTemplateHash(ctx context.Context, id string) string {
	t, err := s.store.GetTemplate(ctx, id)
	if err != nil || t == nil {
		return ""
	}
	return t.ContentHash
}
