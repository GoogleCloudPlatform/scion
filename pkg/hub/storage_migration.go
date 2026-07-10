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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MigrateStorageOnFirstBoot checks whether the hub's namespaced storage prefix
// already has content. If not (first boot with namespacing), it copies legacy
// un-namespaced GCS objects to the hub-scoped prefix and updates DB records.
// This is non-destructive: legacy objects are preserved for other hubs.
func (s *Server) MigrateStorageOnFirstBoot(ctx context.Context) {
	stor := s.GetStorage()
	if stor == nil {
		return
	}
	hubID := s.HubID()
	if hubID == "" {
		return
	}

	namespacedPrefix := "hubs/" + hubID + "/"
	objects, err := stor.List(ctx, storage.ListOptions{
		Prefix:     namespacedPrefix,
		MaxResults: 1,
	})
	if err != nil {
		s.resourceLog.Error("storage migration: failed to check namespaced prefix",
			"prefix", namespacedPrefix, "error", err)
		return
	}
	if objects != nil && len(objects.Objects) > 0 {
		return
	}

	s.resourceLog.Info("storage migration: first boot with hub-scoped paths, duplicating legacy content",
		"hub_id", hubID)

	s.migrateResourceKind(ctx, storage.ResourceKindTemplate, hubID, false, false)
	s.migrateResourceKind(ctx, storage.ResourceKindHarnessConfig, hubID, false, false)
	s.migrateResourceKind(ctx, storage.ResourceKindSkill, hubID, false, false)
}

// MigrateStorageReport holds the results of a storage migration run.
type MigrateStorageReport struct {
	Migrated int
	Skipped  int
	Failed   int
}

// MigrateStorage migrates all resource kinds from legacy to namespaced paths.
// When dryRun is true, it reports what would be migrated without making changes.
// When cleanupLegacy is true, legacy objects are deleted after successful copy.
func (s *Server) MigrateStorage(ctx context.Context, dryRun, cleanupLegacy bool) MigrateStorageReport {
	var total MigrateStorageReport
	for _, kind := range []storage.ResourceKind{
		storage.ResourceKindTemplate,
		storage.ResourceKindHarnessConfig,
		storage.ResourceKindSkill,
	} {
		r := s.migrateResourceKind(ctx, kind, s.HubID(), dryRun, cleanupLegacy)
		total.Migrated += r.Migrated
		total.Skipped += r.Skipped
		total.Failed += r.Failed
	}
	return total
}

type migratableResource struct {
	id            string
	name          string
	storagePath   string
	storageBucket string
	scope         string
	scopeID       string
	slug          string
	files         []store.TemplateFile
	kind          storage.ResourceKind
}

func (s *Server) migrateResourceKind(ctx context.Context, kind storage.ResourceKind, hubID string, dryRun, cleanupLegacy bool) MigrateStorageReport {
	stor := s.GetStorage()
	if stor == nil {
		return MigrateStorageReport{}
	}

	label := string(kind)
	var resources []migratableResource

	switch kind {
	case storage.ResourceKindTemplate:
		var cursor string
		for {
			result, err := s.store.ListTemplates(ctx, store.TemplateFilter{
				Status: store.TemplateStatusActive,
			}, store.ListOptions{Limit: 1000, Cursor: cursor})
			if err != nil {
				s.resourceLog.Error(label+" migration: failed to list", "error", err)
				return MigrateStorageReport{}
			}
			if result == nil || len(result.Items) == 0 {
				break
			}
			for _, t := range result.Items {
				resources = append(resources, migratableResource{
					id:            t.ID,
					name:          t.Name,
					storagePath:   t.StoragePath,
					storageBucket: t.StorageBucket,
					scope:         t.Scope,
					scopeID:       t.ScopeID,
					slug:          t.Slug,
					files:         t.Files,
					kind:          kind,
				})
			}
			if result.NextCursor == "" {
				break
			}
			cursor = result.NextCursor
		}

	case storage.ResourceKindHarnessConfig:
		var cursor string
		for {
			result, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
				Status: store.HarnessConfigStatusActive,
			}, store.ListOptions{Limit: 1000, Cursor: cursor})
			if err != nil {
				s.resourceLog.Error(label+" migration: failed to list", "error", err)
				return MigrateStorageReport{}
			}
			if result == nil || len(result.Items) == 0 {
				break
			}
			for _, hc := range result.Items {
				resources = append(resources, migratableResource{
					id:            hc.ID,
					name:          hc.Name,
					storagePath:   hc.StoragePath,
					storageBucket: hc.StorageBucket,
					scope:         hc.Scope,
					scopeID:       hc.ScopeID,
					slug:          hc.Slug,
					files:         hc.Files,
					kind:          kind,
				})
			}
			if result.NextCursor == "" {
				break
			}
			cursor = result.NextCursor
		}

	case storage.ResourceKindSkill:
		var cursor string
		for {
			result, err := s.store.ListSkills(ctx, store.SkillFilter{}, store.ListOptions{Limit: 1000, Cursor: cursor})
			if err != nil {
				s.resourceLog.Error(label+" migration: failed to list", "error", err)
				return MigrateStorageReport{}
			}
			if result == nil || len(result.Items) == 0 {
				break
			}
			for _, sk := range result.Items {
				resources = append(resources, migratableResource{
					id:            sk.ID,
					name:          sk.Name,
					storagePath:   sk.StoragePath,
					storageBucket: sk.StorageBucket,
					scope:         sk.Scope,
					scopeID:       sk.ScopeID,
					slug:          sk.Slug,
					kind:          kind,
				})
			}
			if result.NextCursor == "" {
				break
			}
			cursor = result.NextCursor
		}
	}

	var report MigrateStorageReport

	for _, res := range resources {
		if res.storagePath == "" {
			report.Skipped++
			continue
		}
		if strings.HasPrefix(res.storagePath, "hubs/") {
			report.Skipped++
			continue
		}

		namespacedPath := storage.ResourceStoragePath(hubID, res.kind, res.scope, res.scopeID, res.slug)
		namespacedURI := storage.ResourceStorageURI(hubID, stor.Bucket(), res.kind, res.scope, res.scopeID, res.slug)

		if dryRun {
			fileCount := s.countStorageObjects(ctx, stor, res.storagePath)
			s.resourceLog.Info(label+" migration: would migrate (dry-run)",
				"resource", res.name,
				"from", res.storagePath,
				"to", namespacedPath,
				"files", fileCount)
			report.Migrated++
			continue
		}

		if err := s.copyStoragePrefix(ctx, stor, res.storagePath, namespacedPath); err != nil {
			s.resourceLog.Error(label+" migration: copy failed",
				"resource", res.name,
				"from", res.storagePath,
				"to", namespacedPath,
				"error", err)
			report.Failed++
			continue
		}

		if err := s.updateResourceStoragePath(ctx, res, namespacedPath, namespacedURI); err != nil {
			s.resourceLog.Error(label+" migration: DB update failed",
				"resource", res.name,
				"error", err)
			report.Failed++
			continue
		}

		s.resourceLog.Info(label+" migration: migrated",
			"resource", res.name,
			"from", res.storagePath,
			"to", namespacedPath)

		if cleanupLegacy {
			if err := stor.DeletePrefix(ctx, res.storagePath+"/"); err != nil {
				s.resourceLog.Error(label+" migration: legacy cleanup failed",
					"resource", res.name,
					"path", res.storagePath,
					"error", err)
			}
		}

		report.Migrated++
	}

	s.resourceLog.Info(label+" migration: complete",
		"migrated", report.Migrated,
		"skipped", report.Skipped,
		"failed", report.Failed)

	return report
}

// copyStoragePrefix copies all objects under srcPrefix to dstPrefix.
func (s *Server) copyStoragePrefix(ctx context.Context, stor storage.Storage, srcPrefix, dstPrefix string) error {
	objects, err := stor.List(ctx, storage.ListOptions{
		Prefix: srcPrefix + "/",
	})
	if err != nil {
		return err
	}
	if objects == nil {
		return nil
	}

	for _, obj := range objects.Objects {
		relPath := strings.TrimPrefix(obj.Name, srcPrefix+"/")
		if relPath == "" || relPath == obj.Name {
			continue
		}
		dstPath := dstPrefix + "/" + relPath
		if _, err := stor.Copy(ctx, obj.Name, dstPath); err != nil {
			return err
		}
	}
	return nil
}

// countStorageObjects counts files under a storage prefix (for dry-run reporting).
func (s *Server) countStorageObjects(ctx context.Context, stor storage.Storage, prefix string) int {
	objects, err := stor.List(ctx, storage.ListOptions{
		Prefix: prefix + "/",
	})
	if err != nil || objects == nil {
		return 0
	}
	return len(objects.Objects)
}

// updateResourceStoragePath updates the DB record for a resource with the new namespaced path.
func (s *Server) updateResourceStoragePath(ctx context.Context, res migratableResource, newPath, newURI string) error {
	switch res.kind {
	case storage.ResourceKindTemplate:
		tmpl, err := s.store.GetTemplate(ctx, res.id)
		if err != nil {
			return err
		}
		if tmpl == nil {
			return nil
		}
		tmpl.StoragePath = newPath
		tmpl.StorageURI = newURI
		return s.store.UpdateTemplate(ctx, tmpl)

	case storage.ResourceKindHarnessConfig:
		hc, err := s.store.GetHarnessConfig(ctx, res.id)
		if err != nil {
			return err
		}
		if hc == nil {
			return nil
		}
		hc.StoragePath = newPath
		hc.StorageURI = newURI
		return s.store.UpdateHarnessConfig(ctx, hc)

	case storage.ResourceKindSkill:
		sk, err := s.store.GetSkill(ctx, res.id)
		if err != nil {
			return err
		}
		if sk == nil {
			return nil
		}
		sk.StoragePath = newPath
		sk.StorageURI = newURI
		return s.store.UpdateSkill(ctx, sk)
	}
	return nil
}
