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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// resource_store.go is the §7.3 step-3b landing: the shared ResourceStore that
// the parallel template and harness-config import/sync paths route through. It
// wraps the already-shared "3a" mechanics (uploadResourceFiles,
// reconcileResourceStorage, toResourceFiles, computeContentHash, and the
// kind-keyed storage.ResourceStoragePath) behind one kind-generic Bootstrap.
//
// The two store models (store.Template / store.HarnessConfig) keep their typed
// payloads (TemplateConfig / HarnessConfigData) — ResourceRecord is a shared
// *view* over their common fields, and a per-kind resourcePersistence bridges
// the record to the concrete model so the typed payload survives a round-trip.

// ResourceRecord is the shared view over the common fields of a file-based
// resource (template, harness-config, …). It deliberately omits the typed,
// kind-specific config payload; that lives on the concrete store model and is
// preserved by the per-kind resourcePersistence, which mutates the loaded model
// in place rather than reconstructing it.
type ResourceRecord struct {
	Kind          storage.ResourceKind
	ID            string
	Name          string
	Slug          string
	Harness       string
	ContentHash   string
	Scope         string
	ScopeID       string
	StorageURI    string
	StorageBucket string
	StoragePath   string
	Files         []store.TemplateFile
	Status        string
	SourceURL     string
	Visibility    string
}

// Resource lifecycle states. Templates and harness-configs use identical string
// values for these (store.TemplateStatus* == store.HarnessConfigStatus*), so the
// shared Bootstrap drives the lifecycle generically.
const (
	resourceStatusPending = store.TemplateStatusPending
	resourceStatusActive  = store.TemplateStatusActive
)

// resourcePersistence bridges a ResourceRecord to a concrete store model. Each
// implementation owns its kind's CRUD and record↔model conversion. An instance
// is constructed fresh per Bootstrap call and may hold the loaded/created model
// so Update can mutate it in place (preserving the typed config payload).
type resourcePersistence interface {
	// Kind identifies the resource kind (drives storage paths).
	Kind() storage.ResourceKind
	// DefaultVisibility is the visibility stamped on a newly-created record.
	DefaultVisibility() string
	// Label prefixes log messages (e.g. "template bootstrap").
	Label() string

	// GetBySlug returns the existing record (with its model attached to the
	// persistence instance) or (nil, nil) if not found.
	GetBySlug(ctx context.Context, slug, scope, scopeID string) (*ResourceRecord, error)
	// Create builds and persists a new pending model from rec plus any
	// dir-derived metadata, retaining the model for a later Update.
	Create(ctx context.Context, rec *ResourceRecord, dir string) error
	// Update applies rec's shared fields plus dir-derived metadata onto the
	// retained model and persists it.
	Update(ctx context.Context, rec *ResourceRecord, dir string) error
	// OnHashMatch handles the unchanged path of a non-forced sync (e.g. the
	// template DefaultHarnessConfig backfill). Returns whether it changed state.
	OnHashMatch(ctx context.Context, rec *ResourceRecord, dir string) (bool, error)
	// PostFinalize runs after a create or a content-changed update (e.g. the
	// template imports its bundled harness-configs).
	PostFinalize(ctx context.Context, rec *ResourceRecord, dir string)
}

// ResourceStore imports/syncs a single resource directory into the Hub's storage
// backend and database. It is the kind-generic replacement for the parallel
// bootstrapSingle*/syncExisting* routines; construct it per-kind via
// Server.templateStore / Server.harnessConfigStore.
type ResourceStore struct {
	srv  *Server
	pers resourcePersistence
}

// templateStore returns a ResourceStore for templates.
func (s *Server) templateStore() *ResourceStore {
	return &ResourceStore{srv: s, pers: &templatePersistence{s: s}}
}

// harnessConfigStore returns a ResourceStore for harness-configs. harness is the
// harness type already parsed from the directory's config.yaml by the caller.
func (s *Server) harnessConfigStore(harness string) *ResourceStore {
	return &ResourceStore{srv: s, pers: &harnessConfigPersistence{s: s, harness: harness}}
}

// Bootstrap imports a new resource directory or syncs an existing one into the
// storage backend + DB. When force is true it always re-uploads and reconciles
// stale objects; when false it short-circuits if the aggregate content hash
// already matches what is stored. Returns whether the stored content changed.
func (rs *ResourceStore) Bootstrap(ctx context.Context, name, dir, scope, scopeID, sourceURL string, force bool) (bool, error) {
	srv := rs.srv
	p := rs.pers
	kind := p.Kind()
	stor := srv.GetStorage()
	if stor == nil {
		return false, fmt.Errorf("storage backend is not configured")
	}

	files, err := transfer.CollectFiles(dir, nil)
	if err != nil {
		return false, err
	}

	slug := api.Slugify(name)
	existing, err := p.GetBySlug(ctx, slug, scope, scopeID)
	if err != nil {
		return false, err
	}

	if existing == nil {
		// New resource — create a pending record, upload, then activate.
		storagePath := storage.ResourceStoragePath(kind, scope, scopeID, slug)
		rec := &ResourceRecord{
			Kind:          kind,
			ID:            api.NewUUID(),
			Name:          name,
			Slug:          slug,
			Scope:         scope,
			ScopeID:       scopeID,
			Status:        resourceStatusPending,
			StoragePath:   storagePath,
			StorageBucket: stor.Bucket(),
			StorageURI:    storage.ResourceStorageURI(stor.Bucket(), kind, scope, scopeID, slug),
			SourceURL:     sourceURL,
			Visibility:    p.DefaultVisibility(),
		}
		if err := p.Create(ctx, rec, dir); err != nil {
			return false, err
		}

		uploaded, _, err := uploadResourceFiles(ctx, stor, storagePath, files, p.Label())
		if err != nil {
			return false, err
		}
		rec.Files = uploaded
		rec.ContentHash = computeContentHash(uploaded)
		rec.Status = resourceStatusActive
		if err := p.Update(ctx, rec, dir); err != nil {
			return false, err
		}

		srv.templateLog.Info(p.Label()+": imported resource",
			"name", name, "files", len(uploaded), "harness", rec.Harness)
		p.PostFinalize(ctx, rec, dir)
		return true, nil
	}

	// Existing resource — short-circuit on unchanged content unless forced.
	if !force {
		if computeContentHash(toResourceFiles(files)) == existing.ContentHash {
			return p.OnHashMatch(ctx, existing, dir)
		}
	}

	storagePath := existing.StoragePath
	if storagePath == "" {
		storagePath = storage.ResourceStoragePath(kind, existing.Scope, existing.ScopeID, existing.Slug)
	}

	uploaded, written, err := uploadResourceFiles(ctx, stor, storagePath, files, p.Label())
	if err != nil {
		return false, err
	}

	// Reconcile storage: drop objects no longer in the manifest so removed files
	// don't linger. (Templates already did this on sync; harness-configs gain it
	// by routing through the shared path — a removed-file cleanup fix.)
	reconcileResourceStorage(ctx, stor, storagePath, existing.Name, written, srv.templateLog, p.Label())

	newHash := computeContentHash(uploaded)
	changed := newHash != existing.ContentHash
	if changed {
		srv.templateLog.Info(p.Label()+": resource re-synced",
			"name", existing.Name, "oldHash", existing.ContentHash, "newHash", newHash)
	}

	existing.Files = uploaded
	existing.ContentHash = newHash
	if sourceURL != "" {
		existing.SourceURL = sourceURL
	}
	// Activate the record now that the upload succeeded. This also recovers a
	// record left in "pending" by a prior bootstrap that failed mid-upload: the
	// retry re-syncs and flips it to active rather than leaving it stuck.
	existing.Status = resourceStatusActive
	if err := p.Update(ctx, existing, dir); err != nil {
		return false, err
	}

	p.PostFinalize(ctx, existing, dir)
	return changed, nil
}

// --- Template persistence -------------------------------------------------

type templatePersistence struct {
	s     *Server
	model *store.Template
}

func (p *templatePersistence) Kind() storage.ResourceKind { return storage.ResourceKindTemplate }
func (p *templatePersistence) DefaultVisibility() string  { return store.VisibilityPrivate }
func (p *templatePersistence) Label() string              { return "template bootstrap" }

func (p *templatePersistence) GetBySlug(ctx context.Context, slug, scope, scopeID string) (*ResourceRecord, error) {
	t, err := p.s.store.GetTemplateBySlug(ctx, slug, scope, scopeID)
	if err == store.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.model = t
	return templateToRecord(t), nil
}

func (p *templatePersistence) Create(ctx context.Context, rec *ResourceRecord, dir string) error {
	t := &store.Template{
		ID:            rec.ID,
		Name:          rec.Name,
		Slug:          rec.Slug,
		Scope:         rec.Scope,
		ScopeID:       rec.ScopeID,
		ProjectID:     rec.ScopeID, // deprecated alias kept for compatibility
		Status:        rec.Status,
		StoragePath:   rec.StoragePath,
		StorageBucket: rec.StorageBucket,
		StorageURI:    rec.StorageURI,
		SourceURL:     rec.SourceURL,
		Visibility:    rec.Visibility,
	}
	p.applyDirMeta(t, dir, rec)
	p.model = t
	return p.s.store.CreateTemplate(ctx, t)
}

func (p *templatePersistence) Update(ctx context.Context, rec *ResourceRecord, dir string) error {
	t := p.model
	t.Files = rec.Files
	t.ContentHash = rec.ContentHash
	t.Status = rec.Status
	if rec.SourceURL != "" {
		t.SourceURL = rec.SourceURL
	}
	p.applyDirMeta(t, dir, rec)
	return p.s.store.UpdateTemplate(ctx, t)
}

// applyDirMeta refreshes the template's harness type and default harness-config
// from the on-disk config, and mirrors the resolved harness back onto rec.
func (p *templatePersistence) applyDirMeta(t *store.Template, dir string, rec *ResourceRecord) {
	cfgInfo := detectHarnessFromConfig(dir, t.Name)
	t.Harness = cfgInfo.Harness
	t.DefaultHarnessConfig = cfgInfo.DefaultHarnessConfig
	rec.Harness = cfgInfo.Harness
}

func (p *templatePersistence) OnHashMatch(ctx context.Context, rec *ResourceRecord, dir string) (bool, error) {
	// Backfill DefaultHarnessConfig for templates imported before that field
	// existed, even when content is unchanged.
	t := p.model
	if t.DefaultHarnessConfig != "" {
		return false, nil
	}
	cfgInfo := detectHarnessFromConfig(dir, t.Name)
	if cfgInfo.DefaultHarnessConfig == "" {
		return false, nil
	}
	t.DefaultHarnessConfig = cfgInfo.DefaultHarnessConfig
	t.Harness = cfgInfo.Harness
	if err := p.s.store.UpdateTemplate(ctx, t); err != nil {
		return false, fmt.Errorf("template bootstrap: failed to backfill defaultHarnessConfig: %w", err)
	}
	p.s.importTemplateHarnessConfigs(ctx, dir, t.Scope, t.ScopeID)
	p.s.templateLog.Info("template bootstrap: backfilled defaultHarnessConfig",
		"template", t.Name, "defaultHarnessConfig", cfgInfo.DefaultHarnessConfig)
	return false, nil
}

func (p *templatePersistence) PostFinalize(ctx context.Context, rec *ResourceRecord, dir string) {
	p.s.importTemplateHarnessConfigs(ctx, dir, rec.Scope, rec.ScopeID)
}

func templateToRecord(t *store.Template) *ResourceRecord {
	if t == nil {
		return nil
	}
	return &ResourceRecord{
		Kind:          storage.ResourceKindTemplate,
		ID:            t.ID,
		Name:          t.Name,
		Slug:          t.Slug,
		Harness:       t.Harness,
		ContentHash:   t.ContentHash,
		Scope:         t.Scope,
		ScopeID:       t.ScopeID,
		StorageURI:    t.StorageURI,
		StorageBucket: t.StorageBucket,
		StoragePath:   t.StoragePath,
		Files:         t.Files,
		Status:        t.Status,
		SourceURL:     t.SourceURL,
		Visibility:    t.Visibility,
	}
}

// --- Harness-config persistence -------------------------------------------

type harnessConfigPersistence struct {
	s       *Server
	harness string
	model   *store.HarnessConfig
}

func (p *harnessConfigPersistence) Kind() storage.ResourceKind {
	return storage.ResourceKindHarnessConfig
}
func (p *harnessConfigPersistence) DefaultVisibility() string { return store.VisibilityPublic }
func (p *harnessConfigPersistence) Label() string             { return "harness config bootstrap" }

func (p *harnessConfigPersistence) GetBySlug(ctx context.Context, slug, scope, scopeID string) (*ResourceRecord, error) {
	hc, err := p.s.store.GetHarnessConfigBySlug(ctx, slug, scope, scopeID)
	if err == store.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.model = hc
	return harnessConfigToRecord(hc), nil
}

func (p *harnessConfigPersistence) Create(ctx context.Context, rec *ResourceRecord, dir string) error {
	hc := &store.HarnessConfig{
		ID:            rec.ID,
		Name:          rec.Name,
		Slug:          rec.Slug,
		Harness:       p.harness,
		Scope:         rec.Scope,
		ScopeID:       rec.ScopeID,
		Status:        rec.Status,
		StoragePath:   rec.StoragePath,
		StorageBucket: rec.StorageBucket,
		StorageURI:    rec.StorageURI,
		SourceURL:     rec.SourceURL,
		Visibility:    rec.Visibility,
	}
	rec.Harness = p.harness
	p.model = hc
	return p.s.store.CreateHarnessConfig(ctx, hc)
}

func (p *harnessConfigPersistence) Update(ctx context.Context, rec *ResourceRecord, dir string) error {
	hc := p.model
	hc.Files = rec.Files
	hc.ContentHash = rec.ContentHash
	hc.Status = rec.Status
	hc.Harness = p.harness
	rec.Harness = p.harness
	if rec.SourceURL != "" {
		hc.SourceURL = rec.SourceURL
	}
	return p.s.store.UpdateHarnessConfig(ctx, hc)
}

func (p *harnessConfigPersistence) OnHashMatch(ctx context.Context, rec *ResourceRecord, dir string) (bool, error) {
	return false, nil
}

func (p *harnessConfigPersistence) PostFinalize(ctx context.Context, rec *ResourceRecord, dir string) {
}

func harnessConfigToRecord(hc *store.HarnessConfig) *ResourceRecord {
	if hc == nil {
		return nil
	}
	return &ResourceRecord{
		Kind:          storage.ResourceKindHarnessConfig,
		ID:            hc.ID,
		Name:          hc.Name,
		Slug:          hc.Slug,
		Harness:       hc.Harness,
		ContentHash:   hc.ContentHash,
		Scope:         hc.Scope,
		ScopeID:       hc.ScopeID,
		StorageURI:    hc.StorageURI,
		StorageBucket: hc.StorageBucket,
		StoragePath:   hc.StoragePath,
		Files:         hc.Files,
		Status:        hc.Status,
		SourceURL:     hc.SourceURL,
		Visibility:    hc.Visibility,
	}
}
