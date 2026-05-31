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
			changed, err := s.syncExistingHarnessConfig(ctx, existing, dirPath, hcDir, stor)
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

// syncExistingHarnessConfig re-syncs a local harness config directory through
// the shared ResourceStore. Returns true if the stored content changed.
func (s *Server) syncExistingHarnessConfig(ctx context.Context, existing *store.HarnessConfig, dirPath string, hcDir *config.HarnessConfigDir, _ storage.Storage) (bool, error) {
	return s.harnessConfigStore(hcDir.Config.Harness).Bootstrap(ctx, existing.Name, dirPath, existing.Scope, existing.ScopeID, false)
}
