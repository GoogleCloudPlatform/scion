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

	"golang.org/x/sync/errgroup"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// syncHarnessConfigFromStorage synchronises a single harness-config's DB
// manifest hashes with the actual GCS content. In a multi-hub topology the
// GCS bucket is shared while each hub keeps its own DB; when another hub
// uploads newer content, the local DB manifest becomes stale and causes
// hash-mismatch errors on broker hydration. This method downloads each file
// from storage, recomputes the SHA-256 hash, and updates the DB record when
// any file hash has drifted.
func (s *Server) syncHarnessConfigFromStorage(ctx context.Context, hcName string) error {
	hc, err := s.findHarnessConfigByName(ctx, hcName)
	if err != nil {
		return err
	}
	if hc == nil {
		return fmt.Errorf("harness-config %q not found", hcName)
	}

	stor := s.GetStorage()
	if stor == nil {
		return fmt.Errorf("storage backend not configured")
	}

	storagePath := hc.StoragePath
	if storagePath == "" {
		storagePath = storage.ResourceStoragePath(
			storage.ResourceKindHarnessConfig, hc.Scope, hc.ScopeID, hc.Slug)
	}

	changed := false
	for i, file := range hc.Files {
		if file.Hash == "" {
			continue
		}
		objectPath := storagePath + "/" + file.Path

		obj, getErr := stor.GetObject(ctx, objectPath)
		if getErr != nil {
			s.resourceLog.Warn("harness-config repair: cannot stat object",
				"config", hcName, "file", file.Path, "error", getErr)
			continue
		}

		actualHash := objectMetadataHash(obj)
		if actualHash == "" {
			var hashErr error
			actualHash, hashErr = computeStoredHash(ctx, stor, objectPath)
			if hashErr != nil {
				s.resourceLog.Warn("harness-config repair: cannot hash object",
					"config", hcName, "file", file.Path, "error", hashErr)
				continue
			}
		}

		if actualHash != file.Hash {
			s.resourceLog.Warn("harness-config repair: updating stale file hash",
				"config", hcName, "file", file.Path,
				"dbHash", file.Hash, "storageHash", actualHash)
			hc.Files[i].Hash = actualHash
			changed = true
		}
	}

	if !changed {
		return nil
	}

	hc.ContentHash = computeContentHash(hc.Files)
	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		return fmt.Errorf("harness-config repair: update DB: %w", err)
	}

	s.resourceLog.Info("harness-config repair: synced DB manifest from storage",
		"config", hcName, "contentHash", hc.ContentHash)
	return nil
}

// SyncAllHarnessConfigsFromStorage reconciles DB manifest hashes against
// actual GCS content for all active harness-configs. Call at startup to catch
// stale manifests left by peer hubs that updated the shared GCS bucket.
func (s *Server) SyncAllHarnessConfigsFromStorage(ctx context.Context) {
	stor := s.GetStorage()
	if stor == nil {
		return
	}

	result, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Status: store.HarnessConfigStatusActive,
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		s.resourceLog.Error("harness-config sync: failed to list configs", "error", err)
		return
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(5)
	for _, hc := range result.Items {
		if len(hc.Files) == 0 {
			continue
		}
		name := hc.Name
		g.Go(func() error {
			rs := s.harnessConfigStore(hc.Harness)
			rec := harnessConfigToRecord(&hc)
			report, vErr := rs.ValidateStorage(gctx, rec)
			if vErr != nil {
				s.resourceLog.Warn("harness-config sync: validation error",
					"config", name, "error", vErr)
				return nil
			}
			for _, issue := range report.Issues {
				if issue.Kind == ValidationIssueContentHashMismatch {
					if syncErr := s.syncHarnessConfigFromStorage(gctx, name); syncErr != nil {
						s.resourceLog.Warn("harness-config sync: repair failed",
							"config", name, "error", syncErr)
					}
					return nil
				}
			}
			return nil
		})
	}
	_ = g.Wait()
}

// findHarnessConfigByName looks up an active harness-config by its display name.
func (s *Server) findHarnessConfigByName(ctx context.Context, name string) (*store.HarnessConfig, error) {
	result, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Name:   name,
		Status: store.HarnessConfigStatusActive,
	}, store.ListOptions{Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("lookup harness-config %q: %w", name, err)
	}
	if len(result.Items) == 0 {
		return nil, nil
	}
	return &result.Items[0], nil
}
