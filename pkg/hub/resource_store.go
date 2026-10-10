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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	// Layout is the template storage layout (store.Template.Layout); empty
	// for other kinds.
	Layout    string
	Files     []store.TemplateFile
	Status    string
	SourceURL string
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

// preUploadChecker is an optional resourcePersistence extension: CheckDir
// validates a collected resource directory before anything is written. The
// shared store calls it once per resource, after collecting the files and
// before Create, the upload or the reconcile, so a refused resource leaves the
// row and storage untouched. templatePersistence implements it for bundled
// harness-configs (ptone/scion#4217).
type preUploadChecker interface {
	CheckDir(ctx context.Context, dir string, files []transfer.FileInfo) error
}

// resourceFileUploader is an optional resourcePersistence extension that
// replaces the shared path upload (uploadResourceFiles). templatePersistence
// implements it to write content-addressed blobs (ptone/scion#4221); a kind
// that implements it also skips the shared reconcile, because its stale
// objects are handled by garbage collection rather than a prefix sweep.
// The store calls it where it would call uploadResourceFiles, in both the
// create and the existing-row branches, before the persistence's Update
// commits the manifest.
type resourceFileUploader interface {
	UploadFiles(ctx context.Context, stor storage.Storage, storagePath string, files []transfer.FileInfo) ([]store.TemplateFile, map[string]struct{}, error)
}

// uploadFiles uploads a collected resource directory: through the
// persistence's UploadFiles when it has one, otherwise one object per file
// under storagePath.
func (rs *ResourceStore) uploadFiles(ctx context.Context, stor storage.Storage, storagePath string, files []transfer.FileInfo) ([]store.TemplateFile, map[string]struct{}, error) {
	if u, ok := rs.pers.(resourceFileUploader); ok {
		return u.UploadFiles(ctx, stor, storagePath, files)
	}
	return uploadResourceFiles(ctx, stor, storagePath, files, rs.pers.Label())
}

// reconcileStorage deletes objects under storagePath that the upload did
// not write, except for kinds with their own UploadFiles (see
// resourceFileUploader).
func (rs *ResourceStore) reconcileStorage(ctx context.Context, stor storage.Storage, storagePath, name string, keep map[string]struct{}) {
	if _, ok := rs.pers.(resourceFileUploader); ok {
		return
	}
	reconcileResourceStorage(ctx, stor, storagePath, name, keep, rs.srv.resourceLog, rs.pers.Label())
}

// checkDir runs the persistence's optional pre-upload check.
func (rs *ResourceStore) checkDir(ctx context.Context, dir string, files []transfer.FileInfo) error {
	if c, ok := rs.pers.(preUploadChecker); ok {
		return c.CheckDir(ctx, dir, files)
	}
	return nil
}

// ResourceStore imports/syncs a single resource directory into the Hub's storage
// backend and database. It is the kind-generic replacement for the parallel
// bootstrapSingle*/syncExisting* routines; construct it per-kind via
// Server.templateStore / Server.harnessConfigStore.
type ResourceStore struct {
	srv   *Server
	pers  resourcePersistence
	hubID string
	// excludePatterns are extra transfer.CollectFiles exclude patterns for
	// this kind (e.g. harness-config backups and temp files).
	excludePatterns []string
}

// templateStore returns a ResourceStore for templates.
func (s *Server) templateStore() *ResourceStore {
	return &ResourceStore{srv: s, pers: &templatePersistence{s: s}, hubID: s.HubID()}
}

// harnessConfigStore returns a ResourceStore for harness-configs. harness is the
// harness type already parsed from the directory's config.yaml by the caller.
func (s *Server) harnessConfigStore(harness string) *ResourceStore {
	return &ResourceStore{
		srv:             s,
		pers:            &harnessConfigPersistence{s: s, harness: harness},
		hubID:           s.HubID(),
		excludePatterns: config.HarnessConfigTransientPatterns,
	}
}

// collectFiles collects the files of a resource directory, applying the
// kind's exclude patterns on top of transfer.DefaultExcludePatterns.
func (rs *ResourceStore) collectFiles(dir string) ([]transfer.FileInfo, error) {
	return transfer.CollectFiles(dir, rs.excludePatterns)
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

	if err := transfer.NormalizeDir(dir); err != nil {
		return false, fmt.Errorf("normalize dir: %w", err)
	}
	files, err := rs.collectFiles(dir)
	if err != nil {
		return false, err
	}

	// Pre-upload content check: a refusal writes nothing (no row, no
	// storage change).
	if err := rs.checkDir(ctx, dir, files); err != nil {
		return false, err
	}

	slug := api.Slugify(name)
	existing, err := p.GetBySlug(ctx, slug, scope, scopeID)
	if err != nil {
		return false, err
	}

	if existing == nil {
		// New resource — create a pending record, upload, then activate.
		storagePath := storage.ResourceStoragePath(rs.hubID, kind, scope, scopeID, slug)
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
			StorageURI:    storage.ResourceStorageURI(rs.hubID, stor.Bucket(), kind, scope, scopeID, slug),
			SourceURL:     sourceURL,
		}
		if err := p.Create(ctx, rec, dir); err != nil {
			return false, err
		}

		uploaded, _, err := rs.uploadFiles(ctx, stor, rec.StoragePath, files)
		if err != nil {
			return false, err
		}
		rec.Files = uploaded
		rec.ContentHash = computeContentHash(uploaded)
		rec.Status = resourceStatusActive
		if err := p.Update(ctx, rec, dir); err != nil {
			return false, err
		}

		srv.resourceLog.Info(p.Label()+": imported resource",
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
		storagePath = storage.ResourceStoragePath(rs.hubID, kind, existing.Scope, existing.ScopeID, existing.Slug)
	}

	uploaded, written, err := rs.uploadFiles(ctx, stor, storagePath, files)
	if err != nil {
		return false, err
	}

	// Reconcile storage: drop objects no longer in the manifest so removed files
	// don't linger. (Harness-configs gain it by routing through the shared
	// path — a removed-file cleanup fix. Templates store blobs, which the
	// blob garbage collector removes instead.)
	rs.reconcileStorage(ctx, stor, storagePath, existing.Name, written)

	newHash := computeContentHash(uploaded)
	changed := newHash != existing.ContentHash
	if changed {
		srv.resourceLog.Info(p.Label()+": resource re-synced",
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
	// written lists the blob objects UploadFiles wrote, which the commit in
	// Update does not check again.
	written map[string]bool
}

func (p *templatePersistence) Kind() storage.ResourceKind { return storage.ResourceKindTemplate }
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
		StorageBucket: rec.StorageBucket,
		SourceURL:     rec.SourceURL,
	}
	// New templates are created in the blob layout, with a row-unique
	// storage path (ptone/scion#4221).
	t.Layout = store.TemplateLayoutBlobs
	t.StoragePath = p.s.templateBlobStoragePath(t)
	t.StorageURI = storage.StorageURIForPath(rec.StorageBucket, t.StoragePath)
	rec.StoragePath = t.StoragePath
	rec.StorageURI = t.StorageURI
	// For user-scoped templates imported via the resource pipeline, set
	// OwnerID and CreatedBy from the scope ID (which IS the user ID for
	// user scope). This mirrors handleCreateTemplate's behavior.
	if rec.Scope == store.TemplateScopeUser && rec.ScopeID != "" {
		t.OwnerID = rec.ScopeID
		t.CreatedBy = rec.ScopeID
	}
	// The pending row carries no files and no derived fields: the Update
	// that follows the upload commits both through commitTemplateFiles.
	p.model = t
	return p.s.store.CreateTemplate(ctx, t)
}

// Update commits the uploaded manifest through commitTemplateFiles, reading
// the agent config and bundled harness-configs from dir. A refused commit
// (an unusable bundled harness-config) returns an error and leaves the row
// unchanged; the bootstrap and import callers log it as a warning and skip
// the resource.
func (p *templatePersistence) Update(ctx context.Context, rec *ResourceRecord, dir string) error {
	t := p.model
	prevStatus, prevSourceURL, prevStoragePath := t.Status, t.SourceURL, t.StoragePath
	apply := func(t *store.Template) {
		t.Status = rec.Status
		if rec.SourceURL != "" {
			t.SourceURL = rec.SourceURL
		}
		p.ensureStoragePath(t)
	}
	apply(t)
	err := p.s.commitTemplateFiles(ctx, t, rec.Files, commitOpts{dir: dir, written: p.written})
	if errors.Is(err, store.ErrTemplateConflict) {
		// Another commit changed the row since it was read: re-read it and
		// commit the directory's files once more against the fresh row
		// (ptone/scion#4221). Blobs are idempotent, so nothing is
		// re-uploaded; a blob missing under the fresh row's path is written
		// from dir.
		var fresh *store.Template
		fresh, err = p.rereadAfterConflict(ctx, t)
		if err == nil {
			apply(fresh)
			if err = p.s.commitTemplateFiles(ctx, fresh, rec.Files, commitOpts{dir: dir, written: p.written}); err == nil {
				p.model = fresh
				t = fresh
			}
		}
	}
	if err != nil {
		t.Status, t.SourceURL, t.StoragePath = prevStatus, prevSourceURL, prevStoragePath
		return fmt.Errorf("%s: template %q not updated: %w", p.Label(), t.Name, err)
	}
	rec.Harness = t.Harness
	return nil
}

// rereadAfterConflict reloads t after a commit lost its compare-and-swap,
// logging the retry. The second conflict, if any, is returned to the caller,
// which logs it and skips the resource.
func (p *templatePersistence) rereadAfterConflict(ctx context.Context, t *store.Template) (*store.Template, error) {
	p.s.templateLog.Warn(p.Label()+": template changed during commit; re-reading and retrying once",
		"template", t.Name, "id", t.ID)
	return p.s.store.GetTemplate(ctx, t.ID)
}

// OnHashMatch re-derives the template's index from dir when the content is
// unchanged but the stored derived fields differ from what the files give
// (a row written before a derived field existed, or before the derivation
// changed). The re-derivation goes through commitTemplateFiles. A commit
// conflict is retried once against the re-read row while that row still has
// the directory's content hash; when it no longer does, another writer has
// replaced the content and there is nothing left to re-derive here, so the
// resource is skipped.
func (p *templatePersistence) OnHashMatch(ctx context.Context, rec *ResourceRecord, dir string) (bool, error) {
	t := p.model
	idx := deriveTemplateIndexFromDir(dir, t.Name)
	if templateIndexMatches(t, idx) {
		return false, nil
	}
	prevStoragePath := t.StoragePath
	p.ensureStoragePath(t)
	err := p.s.commitTemplateFiles(ctx, t, t.Files, commitOpts{dir: dir})
	if errors.Is(err, store.ErrTemplateConflict) {
		var fresh *store.Template
		fresh, err = p.rereadAfterConflict(ctx, t)
		if err == nil {
			if fresh.ContentHash != rec.ContentHash {
				p.s.templateLog.Warn(p.Label()+": template content replaced by another commit; skipping re-derive",
					"template", t.Name, "id", t.ID)
				t.StoragePath = prevStoragePath
				return false, nil
			}
			p.ensureStoragePath(fresh)
			if err = p.s.commitTemplateFiles(ctx, fresh, fresh.Files, commitOpts{dir: dir}); err == nil {
				p.model = fresh
				t = fresh
			}
		}
	}
	if err != nil {
		t.StoragePath = prevStoragePath
		return false, fmt.Errorf("%s: failed to re-derive template %q: %w", p.Label(), t.Name, err)
	}
	p.s.importTemplateHarnessConfigs(ctx, dir, t.Scope, t.ScopeID)
	p.s.templateLog.Info(p.Label()+": re-derived template index",
		"template", t.Name, "harness", t.Harness, "defaultHarnessConfig", t.DefaultHarnessConfig)
	return false, nil
}

// UploadFiles writes the directory's files as blobs under the template's
// content base (its own path for a blob row, the path its commit migrates it
// to for a legacy row). Blobs the row already references are skipped while
// they are present; every other blob is written, which also refreshes one that was present but
// unreferenced, so the garbage collector cannot remove it before the commit
// (F5b). It never writes <StoragePath>/<path>, so a co-located broker's
// direct read of a blob row keeps missing.
func (p *templatePersistence) UploadFiles(ctx context.Context, stor storage.Storage, _ string, files []transfer.FileInfo) ([]store.TemplateFile, map[string]struct{}, error) {
	t := p.model
	if t == nil {
		return nil, nil, fmt.Errorf("%s: no template loaded for upload", p.Label())
	}
	base := p.s.templateContentBase(t)
	referenced := make(map[string]bool)
	if isBlobLayout(t) {
		for _, f := range t.Files {
			if hex, ok := templateBlobHex(f.Hash); ok {
				referenced[hex] = true
			}
		}
	}

	type job struct {
		fi       transfer.FileInfo
		blobPath string
	}
	var jobs []job
	seen := make(map[string]bool)
	for _, fi := range files {
		hex, ok := templateBlobHex(fi.Hash)
		if !ok {
			return nil, nil, fmt.Errorf("%s: file %s has no content hash", p.Label(), fi.Path)
		}
		blobPath := templateBlobPath(base, hex)
		if seen[blobPath] {
			continue
		}
		if referenced[hex] {
			// A blob the row references is skipped only while it is
			// present, so a forced sync or a storage repair restores a
			// missing one.
			exists, err := stor.Exists(ctx, blobPath)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: failed to check blob of %s: %w", p.Label(), fi.Path, err)
			}
			if exists {
				continue
			}
		}
		seen[blobPath] = true
		jobs = append(jobs, job{fi: fi, blobPath: blobPath})
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fileUploadConcurrency)
	for _, j := range jobs {
		j := j
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			data, err := os.ReadFile(j.fi.FullPath)
			if err != nil {
				return fmt.Errorf("%s: failed to open file %s: %w", p.Label(), j.fi.Path, err)
			}
			if transfer.HashBytes(data) != j.fi.Hash {
				return fmt.Errorf("%s: file %s changed while it was uploaded", p.Label(), j.fi.Path)
			}
			if err := uploadTemplateBlob(gctx, stor, j.blobPath, j.fi.Hash, data); err != nil {
				return fmt.Errorf("%s: failed to upload file %s: %w", p.Label(), j.fi.Path, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	p.written = make(map[string]bool, len(jobs))
	written := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		p.written[j.blobPath] = true
		written[j.blobPath] = struct{}{}
	}
	return toResourceFiles(files), written, nil
}

// CheckDir refuses a template directory that bundles a harness-config with
// an unusable provisioner, before the shared store uploads anything. The
// commit path runs the same check again (defence in depth).
func (p *templatePersistence) CheckDir(ctx context.Context, dir string, files []transfer.FileInfo) error {
	if err := checkBundledHarnessConfigs(ctx, dirFileReader(dir), toResourceFiles(files)); err != nil {
		return fmt.Errorf("%s: template in %q not imported: %w", p.Label(), filepath.Base(dir), err)
	}
	return nil
}

// ensureStoragePath gives a legacy row without a storage path the computed
// path the shared ResourceStore uploads to and reads from, so the commit
// verifies (and records) that path.
func (p *templatePersistence) ensureStoragePath(t *store.Template) {
	if t.StoragePath == "" {
		t.StoragePath = storage.ResourceStoragePath(p.s.HubID(), p.Kind(), t.Scope, t.ScopeID, t.Slug)
	}
}

// templateIndexMatches reports whether t's stored derived fields equal idx.
// The snapshots are compared in their stored (JSON) form.
func templateIndexMatches(t *store.Template, idx templateIndex) bool {
	if t.Harness != idx.Harness || t.DefaultHarnessConfig != idx.DefaultHarnessConfig {
		return false
	}
	if (t.AgentConfig == nil) != (idx.AgentConfig == nil) {
		return false
	}
	if t.AgentConfig == nil {
		return true
	}
	a, errA := json.Marshal(t.AgentConfig)
	b, errB := json.Marshal(idx.AgentConfig)
	return errA == nil && errB == nil && bytes.Equal(a, b)
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
		Layout:        t.Layout,
		Files:         t.Files,
		Status:        t.Status,
		SourceURL:     t.SourceURL,
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
func (p *harnessConfigPersistence) Label() string { return "harness config bootstrap" }

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
	}
	extractNoAuthBehavior(hc, dir)
	extractAuthMeta(hc, dir)
	extractModelConfig(hc, dir)
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
	extractNoAuthBehavior(hc, dir)
	extractAuthMeta(hc, dir)
	extractModelConfig(hc, dir)
	return p.s.store.UpdateHarnessConfig(ctx, hc)
}

func (p *harnessConfigPersistence) OnHashMatch(ctx context.Context, rec *ResourceRecord, dir string) (bool, error) {
	return false, nil
}

func (p *harnessConfigPersistence) PostFinalize(ctx context.Context, rec *ResourceRecord, dir string) {
	image := p.extractImage(dir)
	if image == "" {
		return
	}

	if p.model != nil && (p.model.Config == nil || p.model.Config.Image != image) {
		if p.model.Config == nil {
			p.model.Config = &store.HarnessConfigData{}
		}
		p.model.Config.Image = image
		if err := p.s.store.UpdateHarnessConfig(ctx, p.model); err != nil {
			slog.Error("failed to persist image in harness config", "id", rec.ID, "error", err)
		}
	}

	go p.s.checkAndUpdateImageStatus(context.WithoutCancel(ctx), rec.ID, image)
}

func (p *harnessConfigPersistence) extractImage(dir string) string {
	configPath := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err == nil {
		entry, err := config.ParseHarnessConfigYAML(data)
		if err == nil && entry.Image != "" {
			return entry.Image
		}
	}
	if p.model != nil && p.model.Config != nil {
		return p.model.Config.Image
	}
	return ""
}

// extractNoAuthBehavior loads config.yaml from dir and stamps the
// no_auth.behavior value onto the HarnessConfig's Config data so the
// hub can use it for auto no-auth fallback decisions.
func extractNoAuthBehavior(hc *store.HarnessConfig, dir string) {
	if dir == "" {
		return
	}
	hcDir, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		return
	}
	if hcDir.Config.NoAuthConfig != nil && hcDir.Config.NoAuthConfig.Behavior != "" {
		if hc.Config == nil {
			hc.Config = &store.HarnessConfigData{}
		}
		hc.Config.NoAuthBehavior = hcDir.Config.NoAuthConfig.Behavior
	} else if hc.Config != nil {
		hc.Config.NoAuthBehavior = ""
	}
}

// extractAuthMeta loads config.yaml from dir and stamps the declarative
// auth metadata onto the HarnessConfig's Config data so the hub can
// evaluate required_files (including SkippedWhenGCPServiceAccountAssigned)
// at pre-dispatch time.
func extractAuthMeta(hc *store.HarnessConfig, dir string) {
	if dir == "" {
		return
	}
	hcDir, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		return
	}
	if hcDir != nil && hcDir.Config.Auth != nil && len(hcDir.Config.Auth.Types) > 0 {
		if hc.Config == nil {
			hc.Config = &store.HarnessConfigData{}
		}
		hc.Config.AuthMeta = hcDir.Config.Auth
	} else if hc.Config != nil {
		hc.Config.AuthMeta = nil
	}
}

// extractModelConfig loads config.yaml from dir and stamps its default
// model and model_aliases onto the HarnessConfig's Config data, so the hub's
// stored record reflects config.yaml as the source of truth for the
// harness's default model and size-alias table. resolveModelAliasForAgent
// (harness_capabilities.go) reads these instead of falling back to the
// alias table baked into the hub binary at build time
// (harness.DefaultModelAliases), which otherwise goes stale the moment
// config.yaml's aliases are updated without a hub rebuild/redeploy.
//
// Like extractImage, a field is only overwritten when config.yaml declares
// it (non-empty/non-nil) — an absent field preserves whatever value is
// already stored, so a hub-side manual edit to Config.Model or
// Config.ModelAliases (e.g. via the harness-config API) survives a re-sync
// of a config.yaml that doesn't mention that field. This mirrors the
// contract already exercised by TestSyncHarnessConfig_PreservesTypedConfig.
//
// One consequence: deleting model_aliases (or model) from config.yaml does
// NOT clear the stored value — the record keeps resolving with the last
// aliases it saw. If stale-alias drift shows up again, check whether
// config.yaml actually still declares model_aliases before assuming this
// stamping is broken; an intentional removal needs an explicit clear (e.g.
// a hub-side PATCH), not just deleting the key from config.yaml.
func extractModelConfig(hc *store.HarnessConfig, dir string) {
	if dir == "" {
		return
	}
	hcDir, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		return
	}
	applyModelConfigFromEntry(hc, hcDir.Config)
}

// applyModelConfigFromEntry stamps the Model and ModelAliases fields from a
// parsed config.yaml entry onto hc.Config, initializing it if necessary.
// Fields config.yaml doesn't declare are left untouched (see
// extractModelConfig for why). Shared by the directory-based sync path
// (extractModelConfig above) and the storage/content-based harness-config
// file write, upload, and finalize handlers, so every path that can update a
// harness config's config.yaml keeps Model/ModelAliases in sync.
func applyModelConfigFromEntry(hc *store.HarnessConfig, entry config.HarnessConfigEntry) {
	if entry.Model == "" && len(entry.ModelAliases) == 0 {
		return
	}
	if hc.Config == nil {
		hc.Config = &store.HarnessConfigData{}
	}
	if entry.Model != "" {
		hc.Config.Model = entry.Model
	}
	if len(entry.ModelAliases) > 0 {
		hc.Config.ModelAliases = entry.ModelAliases
	}
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
	}
}
