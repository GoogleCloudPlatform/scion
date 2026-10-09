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
// the result. With hub-namespaced storage paths, cross-hub hash interference
// is eliminated; remaining mismatches are from interrupted uploads or manual
// GCS edits.
var repairFlight singleflight.Group

// syncResourceFromStorage reconciles a resource's DB manifest hashes with
// actual GCS content. With hub-namespaced storage paths, cross-hub
// interference is eliminated — each hub owns its own storage partition.
// This repair is still needed for legitimate mismatches (interrupted
// uploads, manual GCS edits, disk corruption, etc.).
//
// In a multi-hub topology the GCS bucket is shared while each hub keeps
// its own DB; this helper downloads each file from storage, recomputes
// the SHA-256 hash, and returns the updated files + content hash when
// any file hash has drifted. Returns changed=false when no update is needed.
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
		storagePath = storage.ResourceStoragePath(s.HubID(), kind, scope, scopeID, slug)
	}

	var legacyBase string
	if s.LegacyFallbackEnabled() {
		legacyBase = storage.ResourceStoragePath("", kind, scope, scopeID, slug)
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
				// Try legacy path before dropping the file.
				if legacyBase != "" && legacyBase != storagePath {
					legacyObjPath := legacyBase + "/" + file.Path
					obj, getErr = stor.GetObject(ctx, legacyObjPath)
					if getErr == nil {
						if _, copyErr := stor.Copy(ctx, legacyObjPath, objectPath); copyErr != nil {
							s.resourceLog.Warn(label+" repair: failed to copy legacy file to namespaced path",
								"resource", name, "file", file.Path,
								"from", legacyObjPath, "to", objectPath, "error", copyErr)
						} else {
							s.resourceLog.Info(label+" repair: copied legacy file to namespaced path",
								"resource", name, "file", file.Path,
								"from", legacyObjPath, "to", objectPath)
						}
						goto hashCheck
					}
				}
				s.resourceLog.Warn(label+" repair: dropping file missing from storage",
					"resource", name, "file", file.Path)
				changed = true
				continue
			}
			return nil, "", false, fmt.Errorf("get object %q: %w", objectPath, getErr)
		}
	hashCheck:

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

// HarnessConfigRepairRef identifies the harness-config record a repair
// should target. ID is authoritative when set (an agent's
// AppliedConfig.HarnessConfigID, or the record already in hand during
// sync-all); a stale ID is "not found", never re-targeted by name.
// Name/ProjectID are used only for agents dispatched without a stamped ID:
// the name is resolved project-scope first (ProjectID), then global — the
// same rule resolveDerivedConfig uses when it stamps the ID — never "newest
// record with that name anywhere" (ptone/scion#2898).
type HarnessConfigRepairRef struct {
	ID        string
	Name      string
	ProjectID string
}

func (r HarnessConfigRepairRef) String() string {
	if r.ID != "" {
		return r.ID
	}
	return r.Name
}

// syncHarnessConfigFromStorage syncs a single harness-config's DB manifest
// from actual GCS content. The target record is resolved first, and
// concurrent calls for the same record are deduplicated via singleflight
// keyed by record ID, so same-named configs in different scopes never share
// (or block) each other's repair.
func (s *Server) syncHarnessConfigFromStorage(ctx context.Context, ref HarnessConfigRepairRef) error {
	hc, err := s.resolveHarnessConfigForRepair(ctx, ref)
	if err != nil {
		return err
	}
	if hc == nil {
		return fmt.Errorf("harness-config %q not found", ref.String())
	}
	id := hc.ID
	_, err, _ = repairFlight.Do("hc:"+id, func() (interface{}, error) {
		return nil, s.syncHarnessConfigFromStorageInner(context.WithoutCancel(ctx), id)
	})
	return err
}

func (s *Server) syncHarnessConfigFromStorageInner(ctx context.Context, id string) error {
	// Re-read inside the flight so the update is applied to the current row,
	// not to a copy read by whichever caller happened to win the flight.
	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		return fmt.Errorf("lookup harness-config %q: %w", id, err)
	}
	if hc == nil {
		return fmt.Errorf("harness-config %q not found", id)
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
		"config", hc.Name, "id", hc.ID, "scope", hc.Scope, "scopeId", hc.ScopeID,
		"contentHash", contentHash)
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
// stale manifests (e.g. interrupted uploads, manual GCS edits). With
// hub-namespaced storage paths, cross-hub interference is eliminated; this
// sync handles legitimate single-hub mismatches only.
func (s *Server) SyncAllHarnessConfigsFromStorage(ctx context.Context) {
	s.syncAllResourcesFromStorage(ctx, storage.ResourceKindHarnessConfig)
}

// SyncAllTemplatesFromStorage reconciles DB manifest hashes against actual
// GCS content for all active templates. With hub-namespaced storage paths,
// cross-hub interference is eliminated; this sync handles legitimate
// single-hub mismatches only (interrupted uploads, manual edits, etc.).
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
						// ID only: the record is in hand, and if it was
						// deleted since the list a name fallback could
						// resolve a different (e.g. global) record.
						syncErr = s.syncHarnessConfigFromStorage(gctx, HarnessConfigRepairRef{
							ID: e.rec.ID,
						})
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

// resolveHarnessConfigForRepair finds the harness-config record a repair
// should act on. When an ID is given it is authoritative: a missing record
// is "not found", with no name fallback — the dispatch retry still carries
// the stale ID and hash, so repairing some other same-named row could not
// make it succeed and would only touch an unrelated record. The name is
// used only when no ID was stamped, looked up by slug in the agent's project
// scope, then global scope. Returns (nil, nil) when nothing matches.
//
// There is deliberately no Status filter (the old name lookup required
// "active"): resolveDerivedConfig stamps the ID via the same unfiltered
// GetHarnessConfigBySlug lookup, so repair targets exactly the record
// dispatch uses. Do not "restore" the filter without changing both.
func (s *Server) resolveHarnessConfigForRepair(ctx context.Context, ref HarnessConfigRepairRef) (*store.HarnessConfig, error) {
	if ref.ID != "" {
		hc, err := s.store.GetHarnessConfig(ctx, ref.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("lookup harness-config %q: %w", ref.ID, err)
		}
		return hc, nil
	}
	if ref.Name == "" {
		return nil, nil
	}
	if ref.ProjectID != "" {
		hc, err := s.store.GetHarnessConfigBySlug(ctx, ref.Name, store.HarnessConfigScopeProject, ref.ProjectID)
		if err == nil && hc != nil {
			return hc, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("lookup project harness-config %q: %w", ref.Name, err)
		}
	}
	hc, err := s.store.GetHarnessConfigBySlug(ctx, ref.Name, store.HarnessConfigScopeGlobal, "")
	if err == nil && hc != nil {
		return hc, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("lookup global harness-config %q: %w", ref.Name, err)
	}
	return nil, nil
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
