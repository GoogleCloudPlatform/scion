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
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
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

func (s *Server) bootstrapSingleHarnessConfigScoped(ctx context.Context, name, dirPath string, hcDir *config.HarnessConfigDir, stor storage.Storage, scope, scopeID string) error {
	files, err := transfer.CollectFiles(dirPath, nil)
	if err != nil {
		return err
	}

	slug := api.Slugify(name)
	storagePath := storage.HarnessConfigStoragePath(scope, scopeID, slug)

	hc := &store.HarnessConfig{
		ID:            api.NewUUID(),
		Name:          name,
		Slug:          slug,
		Harness:       hcDir.Config.Harness,
		Scope:         scope,
		ScopeID:       scopeID,
		Status:        store.HarnessConfigStatusPending,
		StoragePath:   storagePath,
		StorageBucket: stor.Bucket(),
		StorageURI:    storage.HarnessConfigStorageURI(stor.Bucket(), scope, scopeID, slug),
		Visibility:    store.VisibilityPublic,
	}

	if err := s.store.CreateHarnessConfig(ctx, hc); err != nil {
		return err
	}

	var hcFiles []store.TemplateFile
	for _, fi := range files {
		objectPath := storagePath + "/" + fi.Path

		f, err := os.Open(fi.FullPath)
		if err != nil {
			s.templateLog.Warn("harness config bootstrap: failed to open file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		_, err = stor.Upload(ctx, objectPath, f, storage.UploadOptions{})
		f.Close()
		if err != nil {
			s.templateLog.Warn("harness config bootstrap: failed to upload file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		hcFiles = append(hcFiles, store.TemplateFile{
			Path: fi.Path,
			Size: fi.Size,
			Hash: fi.Hash,
			Mode: fi.Mode,
		})
	}

	contentHash := computeContentHash(hcFiles)
	hc.Files = hcFiles
	hc.ContentHash = contentHash
	hc.Status = store.HarnessConfigStatusActive

	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		return err
	}

	s.templateLog.Info("harness config bootstrap: imported config",
		"name", name, "files", len(hcFiles), "harness", hcDir.Config.Harness)
	return nil
}

// syncExistingHarnessConfig checks if a local harness config directory has
// changed compared to what is stored in the database. If so, it re-uploads
// the files and updates the database record. Returns true if an update occurred.
func (s *Server) syncExistingHarnessConfig(ctx context.Context, existing *store.HarnessConfig, dirPath string, hcDir *config.HarnessConfigDir, stor storage.Storage) (bool, error) {
	files, err := transfer.CollectFiles(dirPath, nil)
	if err != nil {
		return false, err
	}

	var hcFiles []store.TemplateFile
	for _, fi := range files {
		hcFiles = append(hcFiles, store.TemplateFile{
			Path: fi.Path,
			Size: fi.Size,
			Hash: fi.Hash,
			Mode: fi.Mode,
		})
	}
	newHash := computeContentHash(hcFiles)

	if newHash == existing.ContentHash {
		return false, nil
	}

	s.templateLog.Info("harness config bootstrap: local config changed, re-syncing",
		"config", existing.Name, "oldHash", existing.ContentHash, "newHash", newHash)

	storagePath := existing.StoragePath
	if storagePath == "" {
		storagePath = storage.HarnessConfigStoragePath(existing.Scope, "", existing.Slug)
	}

	var uploadedFiles []store.TemplateFile
	for _, fi := range files {
		objectPath := storagePath + "/" + fi.Path

		f, err := os.Open(fi.FullPath)
		if err != nil {
			s.templateLog.Warn("harness config bootstrap: failed to open file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		_, err = stor.Upload(ctx, objectPath, f, storage.UploadOptions{})
		f.Close()
		if err != nil {
			s.templateLog.Warn("harness config bootstrap: failed to upload file, skipping",
				"file", fi.Path, "error", err)
			continue
		}

		uploadedFiles = append(uploadedFiles, store.TemplateFile{
			Path: fi.Path,
			Size: fi.Size,
			Hash: fi.Hash,
			Mode: fi.Mode,
		})
	}

	existing.Files = uploadedFiles
	existing.ContentHash = newHash
	existing.Harness = hcDir.Config.Harness

	if err := s.store.UpdateHarnessConfig(ctx, existing); err != nil {
		return false, err
	}

	return true, nil
}
