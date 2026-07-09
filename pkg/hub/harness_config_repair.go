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

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// repairFlight deduplicates concurrent repair attempts for the same resource.
// When multiple dispatches hit a hash mismatch simultaneously, only one
// actually downloads from GCS and updates the DB; the others wait and share
// the result.
var repairFlight singleflight.Group

// syncResourceFromStorage synchronises a resource's DB manifest hashes with
// actual GCS content. In a multi-hub topology the GCS bucket is shared while
// each hub keeps its own DB; when another hub uploads newer content, the local
// DB manifest becomes stale and causes hash-mismatch errors on broker
// hydration. This helper downloads each file from storage, recomputes the
// SHA-256 hash, and returns the updated files + content hash when any file
// hash has drifted. Returns changed=false when no update is needed.
func (s *Server) syncResourceFromStorage(
	ctx context.Context,
	kind storage.ResourceKind,
	name string,
	storagePath string,
	scope string,
	scopeID string,
	slug string,
	files []store.TemplateFile,
) (updatedFiles []store.TemplateFile, contentHash string, changed bool, err error) {
	stor := s.GetStorage()
	if stor == nil {
		return nil, "", false, fmt.Errorf("storage backend not configured")
	}

	if storagePath == "" {
		storagePath = storage.ResourceStoragePath(kind, scope, scopeID, slug)
	}

	label := string(kind)
	updated := make([]store.TemplateFile, 0, len(files))

	for _, file := range files {
		if file.Hash == "" {
			updated = append(updated, file)
			continue
		}
		objectPath := storagePath + "/" + file.Path

		obj, getErr := stor.GetObject(ctx, objectPath)
		if getErr != nil {
			if errors.Is(getErr, storage.ErrNotFound) {
				s.resourceLog.Warn(label+" repair: dropping file missing from storage",
					"resource", name, "file", file.Path)
				changed = true
				continue
			}
			return nil, "", false, fmt.Errorf("get object %q: %w", objectPath, getErr)
		}

		actualHash := objectMetadataHash(obj)
		if actualHash == "" {
			var hashErr error
			actualHash, hashErr = computeStoredHash(ctx, stor, objectPath)
			if hashErr != nil {
				return nil, "", false, fmt.Errorf("compute hash for %q: %w", objectPath, hashErr)
			}
		}

		entry := file
		if actualHash != file.Hash {
			s.resourceLog.Warn(label+" repair: updating stale file hash",
				"resource", name, "file", file.Path,
				"dbHash", file.Hash, "storageHash", actualHash)
			entry.Hash = actualHash
			changed = true
		}
		updated = append(updated, entry)
	}

	if !changed {
		return nil, "", false, nil
	}

	contentHash = computeContentHash(updated)
	return updated, contentHash, true, nil
}

// syncHarnessConfigFromStorage syncs a single harness-config's DB manifest
// from actual GCS content. Concurrent calls for the same config are
// deduplicated via singleflight.
func (s *Server) syncHarnessConfigFromStorage(ctx context.Context, hcName string) error {
	_, err, _ := repairFlight.Do("hc:"+hcName, func() (interface{}, error) {
		return nil, s.syncHarnessConfigFromStorageInner(context.WithoutCancel(ctx), hcName)
	})
	return err
}

func (s *Server) syncHarnessConfigFromStorageInner(ctx context.Context, hcName string) error {
	hc, err := s.findHarnessConfigByName(ctx, hcName)
	if err != nil {
		return err
	}
	if hc == nil {
		return fmt.Errorf("harness-config %q not found", hcName)
	}

	updated, contentHash, changed, err := s.syncResourceFromStorage(
		ctx, storage.ResourceKindHarnessConfig, hc.Name,
		hc.StoragePath, hc.Scope, hc.ScopeID, hc.Slug, hc.Files)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	hc.Files = updated
	hc.ContentHash = contentHash
	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		return fmt.Errorf("harness-config repair: update DB: %w", err)
	}
	s.resourceLog.Info("harness-config repair: synced DB manifest from storage",
		"config", hc.Name, "contentHash", contentHash)
	return nil
}

// syncTemplateFromStorage syncs a single template's DB manifest from actual
// GCS content. Concurrent calls for the same template are deduplicated via
// singleflight.
func (s *Server) syncTemplateFromStorage(ctx context.Context, templateRef string) error {
	_, err, _ := repairFlight.Do("tmpl:"+templateRef, func() (interface{}, error) {
		return nil, s.syncTemplateFromStorageInner(context.WithoutCancel(ctx), templateRef)
	})
	return err
}

func (s *Server) syncTemplateFromStorageInner(ctx context.Context, templateRef string) error {
	tmpl, err := s.findTemplateByRef(ctx, templateRef)
	if err != nil {
		return err
	}
	if tmpl == nil {
		return fmt.Errorf("template %q not found", templateRef)
	}

	updated, contentHash, changed, err := s.syncResourceFromStorage(
		ctx, storage.ResourceKindTemplate, tmpl.Name,
		tmpl.StoragePath, tmpl.Scope, tmpl.ScopeID, tmpl.Slug, tmpl.Files)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	tmpl.Files = updated
	tmpl.ContentHash = contentHash
	if err := s.store.UpdateTemplate(ctx, tmpl); err != nil {
		return fmt.Errorf("template repair: update DB: %w", err)
	}
	s.resourceLog.Info("template repair: synced DB manifest from storage",
		"template", tmpl.Name, "contentHash", contentHash)
	return nil
}

// SyncAllHarnessConfigsFromStorage reconciles DB manifest hashes against
// actual GCS content for all active harness-configs. Call at startup to catch
// stale manifests left by peer hubs that updated the shared GCS bucket.
func (s *Server) SyncAllHarnessConfigsFromStorage(ctx context.Context) {
	s.syncAllResourcesFromStorage(ctx, storage.ResourceKindHarnessConfig)
}

// SyncAllTemplatesFromStorage reconciles DB manifest hashes against actual
// GCS content for all active templates.
func (s *Server) SyncAllTemplatesFromStorage(ctx context.Context) {
	s.syncAllResourcesFromStorage(ctx, storage.ResourceKindTemplate)
}

func (s *Server) syncAllResourcesFromStorage(ctx context.Context, kind storage.ResourceKind) {
	stor := s.GetStorage()
	if stor == nil {
		return
	}

	label := string(kind)

	type resourceEntry struct {
		name    string
		harness string
		rec     *ResourceRecord
	}

	var entries []resourceEntry

	switch kind {
	case storage.ResourceKindHarnessConfig:
		result, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
			Status: store.HarnessConfigStatusActive,
		}, store.ListOptions{Limit: 1000})
		if err != nil {
			s.resourceLog.Error(label+" sync: failed to list", "error", err)
			return
		}
		if result == nil {
			return
		}
		for _, hc := range result.Items {
			if len(hc.Files) == 0 {
				continue
			}
			entries = append(entries, resourceEntry{
				name:    hc.Name,
				harness: hc.Harness,
				rec:     harnessConfigToRecord(&hc),
			})
		}
	case storage.ResourceKindTemplate:
		result, err := s.store.ListTemplates(ctx, store.TemplateFilter{
			Status: store.TemplateStatusActive,
		}, store.ListOptions{Limit: 1000})
		if err != nil {
			s.resourceLog.Error(label+" sync: failed to list", "error", err)
			return
		}
		if result == nil {
			return
		}
		for _, t := range result.Items {
			if len(t.Files) == 0 {
				continue
			}
			entries = append(entries, resourceEntry{
				name:    t.Name,
				harness: t.Harness,
				rec:     templateToRecord(&t),
			})
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(5)
	for _, e := range entries {
		e := e
		g.Go(func() error {
			var rs *ResourceStore
			switch kind {
			case storage.ResourceKindHarnessConfig:
				rs = s.harnessConfigStore(e.harness)
			case storage.ResourceKindTemplate:
				rs = s.templateStore()
			}
			if rs == nil {
				s.resourceLog.Warn(label+" sync: no resource store",
					"resource", e.name)
				return nil
			}
			report, vErr := rs.ValidateStorage(gctx, e.rec)
			if vErr != nil {
				s.resourceLog.Warn(label+" sync: validation error",
					"resource", e.name, "error", vErr)
				return nil
			}
			if report == nil {
				return nil
			}
			for _, issue := range report.Issues {
				if issue.Kind == ValidationIssueContentHashMismatch {
					var syncErr error
					switch kind {
					case storage.ResourceKindHarnessConfig:
						syncErr = s.syncHarnessConfigFromStorage(gctx, e.name)
					case storage.ResourceKindTemplate:
						syncErr = s.syncTemplateFromStorage(gctx, e.name)
					}
					if syncErr != nil {
						s.resourceLog.Warn(label+" sync: repair failed",
							"resource", e.name, "error", syncErr)
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
	if result == nil || len(result.Items) == 0 {
		return nil, nil
	}
	return &result.Items[0], nil
}

// findTemplateByRef looks up an active template by ID or name.
func (s *Server) findTemplateByRef(ctx context.Context, ref string) (*store.Template, error) {
	tmpl, err := s.store.GetTemplate(ctx, ref)
	if err == nil && tmpl != nil {
		return tmpl, nil
	}
	result, err := s.store.ListTemplates(ctx, store.TemplateFilter{
		Name:   ref,
		Status: store.TemplateStatusActive,
	}, store.ListOptions{Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("lookup template %q: %w", ref, err)
	}
	if result == nil || len(result.Items) == 0 {
		return nil, nil
	}
	return &result.Items[0], nil
}
