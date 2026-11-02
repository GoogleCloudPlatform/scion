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
	"os"
	"path/filepath"
	"sync/atomic"

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
// toResourceFiles, computeContentHash, and the
// kind-keyed storage.ResourceStoragePath) behind one kind-generic Bootstrap.
//
// The two store models (store.Template / store.HarnessConfig) keep their typed
// payloads (AgentConfig / HarnessConfigData) — ResourceRecord is a shared
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
	// Layout is the storage layout (store.Template.Layout,
	// store.HarnessConfig.Layout).
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
// The hooks read the resource's files only through cd (collectedDir), never
// from a directory path.
type resourcePersistence interface {
	// Kind identifies the resource kind (drives storage paths).
	Kind() storage.ResourceKind
	// Label prefixes log messages (e.g. "template bootstrap").
	Label() string

	// GetBySlug returns the existing record (with its model attached to the
	// persistence instance) or (nil, nil) if not found.
	GetBySlug(ctx context.Context, slug, scope, scopeID string) (*ResourceRecord, error)
	// Create builds and persists a new pending model from rec plus any
	// metadata derived from the collected directory, retaining the model for
	// a later Update.
	Create(ctx context.Context, rec *ResourceRecord, cd *collectedDir) error
	// Update applies rec's shared fields plus metadata derived from the
	// collected directory onto the retained model and persists it.
	Update(ctx context.Context, rec *ResourceRecord, cd *collectedDir) error
	// OnHashMatch handles the unchanged path of a non-forced sync (e.g. the
	// template DefaultHarnessConfig backfill). Returns whether it changed state.
	OnHashMatch(ctx context.Context, rec *ResourceRecord, cd *collectedDir) (bool, error)
	// PostFinalize runs after a create or a content-changed update (e.g. the
	// template imports its bundled harness-configs).
	PostFinalize(ctx context.Context, rec *ResourceRecord, cd *collectedDir)
}

// preUploadChecker is an optional resourcePersistence extension: CheckDir
// validates a collected resource directory before anything is written; name
// is the resource name the caller chose. The shared store calls it once per
// resource, after collecting the files and before Create or the upload, so a
// refused resource leaves the row and storage untouched. templatePersistence implements it for bundled
// harness-configs (ptone/scion#4217).
type preUploadChecker interface {
	CheckDir(ctx context.Context, name string, cd *collectedDir) error
}

// resourceFileUploader is an optional resourcePersistence extension that
// replaces the shared path upload (uploadResourceFiles). templatePersistence
// (ptone/scion#4221) and harnessConfigPersistence (ptone/scion#4219)
// implement it to write content-addressed blobs. Their stale objects are
// removed by the blob garbage collector, never by a prefix sweep, so no
// kind reconciles storage after an upload any more: a commit that loses its
// compare-and-swap twice can no longer leave a row listing objects a
// reconcile deleted. The store calls it in both the create and the
// existing-row branches, before the persistence's Update commits the
// manifest. root is opened at the directory files were collected from, and
// every file is read through it (transfer.ReadCollectedFile /
// OpenCollectedFile).
type resourceFileUploader interface {
	UploadFiles(ctx context.Context, stor storage.Storage, root *os.Root, storagePath string, files []transfer.FileInfo) ([]store.TemplateFile, map[string]struct{}, error)
}

// uploadFiles uploads a collected resource directory: through the
// persistence's UploadFiles when it has one, otherwise one object per file
// under storagePath. root must be opened at the directory files were
// collected from; files are read only through it.
func (rs *ResourceStore) uploadFiles(ctx context.Context, stor storage.Storage, root *os.Root, storagePath string, files []transfer.FileInfo) ([]store.TemplateFile, map[string]struct{}, error) {
	if u, ok := rs.pers.(resourceFileUploader); ok {
		return u.UploadFiles(ctx, stor, root, storagePath, files)
	}
	return uploadResourceFiles(ctx, stor, root, storagePath, files, rs.pers.Label())
}

// commitUploaded commits rec (its uploaded manifest) through the
// persistence's Update. Every path that uploads and then commits an existing
// resource (Bootstrap, BootstrapSource's update and storage repair) goes
// through it. Nothing is deleted from storage here: files the new manifest
// drops keep their blobs until the blob garbage collector's grace period has
// passed (template_blob_gc.go).
func (rs *ResourceStore) commitUploaded(ctx context.Context, rec *ResourceRecord, cd *collectedDir) error {
	return rs.pers.Update(ctx, rec, cd)
}

// checkDir runs the persistence's optional pre-upload check.
func (rs *ResourceStore) checkDir(ctx context.Context, name string, cd *collectedDir) error {
	if c, ok := rs.pers.(preUploadChecker); ok {
		return c.CheckDir(ctx, name, cd)
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
	// within, when set, is the collected directory this resource directory
	// is a subdirectory of (a harness-config bundled in a template), at the
	// slash path withinPath. collectFiles then also requires the directory
	// it collected to be that subdirectory of within's root.
	within     *collectedDir
	withinPath string
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

// afterCollectHook, when set (tests only), runs in collectFiles between the
// collection (through the root) and the check that dir still names the
// directory the root was opened at.
var afterCollectHook atomic.Pointer[func(dir string)]

// requireConfigName makes the harness-config persistence check that the
// collected config.yaml carries the resource name (resourceDir
// nameFromConfig). It does nothing for other kinds.
func (rs *ResourceStore) requireConfigName() {
	switch p := rs.pers.(type) {
	case *harnessConfigPersistence:
		p.nameFromConfig = true
	case *targetHarnessConfigPersistence:
		p.nameFromConfig = true
	}
}

// collectFiles opens an os.Root at dir (openRealDirRoot) and collects the
// files of the resource directory through it, applying the kind's exclude
// patterns on top of transfer.DefaultExcludePatterns; every later read of a
// collected file goes through the same root. dir must be a directory, not a
// link, and must still name the directory the root was opened at after the
// collection (os.SameFile); a directory replaced in between fails the
// operation. With rs.within set, the root must also be the directory
// within's root has at rs.withinPath, reached without following a link.
// The caller must close the root (cd.root).
func (rs *ResourceStore) collectFiles(dir string) (*collectedDir, error) {
	return rs.openAndCollect(dir, false)
}

// normalizeAndCollect is collectFiles with the line-ending normalization
// first: the directory's text files are normalized in place through the same
// root (transfer.NormalizeRoot) before they are collected, so neither step
// reopens the directory or a file by path.
func (rs *ResourceStore) normalizeAndCollect(dir string) (*collectedDir, error) {
	return rs.openAndCollect(dir, true)
}

func (rs *ResourceStore) openAndCollect(dir string, normalize bool) (*collectedDir, error) {
	root, at, err := openRealDirRoot(dir)
	if err != nil {
		return nil, err
	}
	if normalize {
		if err := transfer.NormalizeRoot(root); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("normalize dir: %w", err)
		}
	}
	// The collection walks, stats and hashes through the root, so the
	// directory is not reopened by path.
	files, err := transfer.CollectFilesFromRoot(root, dir, rs.excludePatterns)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if hook := afterCollectHook.Load(); hook != nil {
		(*hook)(dir)
	}
	// dir must still name the directory that was collected.
	now, err := os.Lstat(dir)
	if err != nil || !os.SameFile(now, at) {
		_ = root.Close()
		return nil, fmt.Errorf("%s %w", dir, errCollectedDirChanged)
	}
	if rs.within != nil {
		if err := rs.checkWithin(at, dir); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return newCollectedDir(dir, root, files), nil
}

// afterDirLstatHook, when set (tests only), runs between openRealDirRoot's
// Lstat and its open.
var afterDirLstatHook atomic.Pointer[func(dir string)]

// openRealDirRoot opens an os.Root at dir, which must be a directory and not
// a link (os.Lstat). The parent and then the directory (os.Root.OpenRoot)
// are opened with a trailing path separator, so the kernel resolves each as
// a directory: a FIFO or other non-directory swapped in after the Lstat fails
// with ENOTDIR without being opened, so it cannot block. (Neither os.OpenRoot
// nor Root.OpenRoot passes O_DIRECTORY or O_NONBLOCK for the last component.)
// The trailing separator also makes the kernel follow a link swapped in for
// the directory, so the root must be the directory the Lstat saw (os.SameFile
// with root.Stat(".")); otherwise errCollectedDirChanged. It returns the
// root, which the caller must close, and the directory's FileInfo as seen
// through the root.
func openRealDirRoot(dir string) (*os.Root, os.FileInfo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	before, err := os.Lstat(abs)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%s is a link, not a directory", dir)
	}
	if !before.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", dir)
	}
	parentDir, base := filepath.Dir(abs), filepath.Base(abs)
	if base == string(filepath.Separator) {
		return nil, nil, fmt.Errorf("%s: no parent directory to open it through", dir)
	}
	sep := string(filepath.Separator)
	if hook := afterDirLstatHook.Load(); hook != nil {
		(*hook)(dir)
	}
	parent, err := os.OpenRoot(parentDir + sep)
	if err != nil {
		return nil, nil, fmt.Errorf("open parent of %s: %w", dir, err)
	}
	defer func() { _ = parent.Close() }()
	root, err := parent.OpenRoot(base + sep)
	if err != nil {
		return nil, nil, fmt.Errorf("open dir root %s: %w", dir, err)
	}
	at, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, fmt.Errorf("stat dir root %s: %w", dir, err)
	}
	if !os.SameFile(before, at) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("%s %w", dir, errCollectedDirChanged)
	}
	return root, at, nil
}

// checkWithin requires got (the directory dir, as stat'ed) to be the
// subdirectory rs.within's root has at rs.withinPath, reached through real
// directories only (no component a link; realSubdir).
func (rs *ResourceStore) checkWithin(got os.FileInfo, dir string) error {
	if err := rs.within.realSubdir(rs.withinPath); err != nil {
		return fmt.Errorf("%s: %w", rs.withinPath, err)
	}
	want, err := rs.within.root.Lstat(filepath.FromSlash(rs.withinPath))
	if err != nil {
		return fmt.Errorf("lstat %s within root: %w", rs.withinPath, err)
	}
	if !os.SameFile(want, got) {
		return fmt.Errorf("%s %w", dir, errCollectedDirChanged)
	}
	return nil
}

// Bootstrap imports a new resource directory or syncs an existing one into the
// storage backend + DB. When force is true it always re-uploads; when false
// it short-circuits if the aggregate content hash already matches what is
// stored. Returns whether the stored content changed.
func (rs *ResourceStore) Bootstrap(ctx context.Context, name, dir, scope, scopeID, sourceURL string, force bool) (bool, error) {
	srv := rs.srv
	p := rs.pers
	kind := p.Kind()
	stor := srv.GetStorage()
	if stor == nil {
		return false, fmt.Errorf("storage backend is not configured")
	}

	var cd *collectedDir
	if rs.within != nil {
		// A harness-config bundled in a template: it must be the template's
		// own subdirectory before anything reads or writes it. It is not
		// normalized again: the template's bootstrap normalized the whole
		// tree before collecting it.
		got, err := os.Lstat(dir)
		if err != nil {
			return false, err
		}
		if err := rs.checkWithin(got, dir); err != nil {
			return false, err
		}
		if cd, err = rs.collectFiles(dir); err != nil {
			return false, err
		}
	} else {
		var err error
		if cd, err = rs.normalizeAndCollect(dir); err != nil {
			return false, err
		}
	}
	defer func() { _ = cd.root.Close() }()
	files, root := cd.files, cd.root

	// Pre-upload content check: a refusal writes nothing (no row, no
	// storage change).
	if err := rs.checkDir(ctx, name, cd); err != nil {
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
		if err := p.Create(ctx, rec, cd); err != nil {
			return false, err
		}

		uploaded, _, err := rs.uploadFiles(ctx, stor, root, rec.StoragePath, files)
		if err != nil {
			return false, err
		}
		rec.Files = uploaded
		rec.ContentHash = rs.srv.computeContentHash(uploaded)
		rec.Status = resourceStatusActive
		if err := p.Update(ctx, rec, cd); err != nil {
			return false, err
		}

		srv.resourceLog.Info(p.Label()+": imported resource",
			"name", name, "files", len(uploaded), "harness", rec.Harness)
		p.PostFinalize(ctx, rec, cd)
		return true, nil
	}

	// Existing resource — short-circuit on unchanged content unless forced.
	if !force {
		if rs.srv.computeContentHash(toResourceFiles(files)) == existing.ContentHash {
			return p.OnHashMatch(ctx, existing, cd)
		}
	}

	storagePath := existing.StoragePath
	if storagePath == "" {
		storagePath = storage.ResourceStoragePath(rs.hubID, kind, existing.Scope, existing.ScopeID, existing.Slug)
	}

	uploaded, _, err := rs.uploadFiles(ctx, stor, root, storagePath, files)
	if err != nil {
		return false, err
	}

	newHash := rs.srv.computeContentHash(uploaded)
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
	// Commit. Objects of files the manifest drops are removed by the blob
	// garbage collector, not here.
	if err := rs.commitUploaded(ctx, existing, cd); err != nil {
		return false, err
	}

	p.PostFinalize(ctx, existing, cd)
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

func (p *templatePersistence) Create(ctx context.Context, rec *ResourceRecord, _ *collectedDir) error {
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
// the agent config and bundled harness-configs from the collected directory.
// A refused commit (an unusable bundled harness-config) returns an error and
// leaves the row
// unchanged; the bootstrap and import callers log it as a warning and skip
// the resource.
func (p *templatePersistence) Update(ctx context.Context, rec *ResourceRecord, cd *collectedDir) error {
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
	err := p.s.commitTemplateFiles(ctx, t, rec.Files, commitOpts{collected: cd, written: p.written})
	if errors.Is(err, store.ErrTemplateConflict) {
		// Another commit changed the row since it was read: re-read it and
		// commit the directory's files once more against the fresh row
		// (ptone/scion#4221). Blobs are idempotent, so nothing is
		// re-uploaded; a blob missing under the fresh row's path is written
		// from the collected directory.
		var fresh *store.Template
		fresh, err = p.rereadAfterConflict(ctx, t)
		if err == nil {
			apply(fresh)
			if err = p.s.commitTemplateFiles(ctx, fresh, rec.Files, commitOpts{collected: cd, written: p.written}); err == nil {
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

// OnHashMatch re-derives the template's index from the collected directory
// when the content is
// unchanged but the stored derived fields differ from what the files give
// (a row written before a derived field existed, or before the derivation
// changed). The re-derivation goes through commitTemplateFiles. A commit
// conflict is retried once against the re-read row while that row still has
// the directory's content hash; when it no longer does, another writer has
// replaced the content and there is nothing left to re-derive here, so the
// resource is skipped.
func (p *templatePersistence) OnHashMatch(ctx context.Context, rec *ResourceRecord, cd *collectedDir) (bool, error) {
	t := p.model
	idx, err := deriveTemplateIndexFromCollected(cd, t.Name)
	if err != nil {
		return false, fmt.Errorf("%s: failed to re-derive template %q: %w", p.Label(), t.Name, err)
	}
	if templateIndexMatches(t, idx) {
		return false, nil
	}
	prevStoragePath := t.StoragePath
	p.ensureStoragePath(t)
	err = p.s.commitTemplateFiles(ctx, t, t.Files, commitOpts{collected: cd})
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
			if err = p.s.commitTemplateFiles(ctx, fresh, fresh.Files, commitOpts{collected: cd}); err == nil {
				p.model = fresh
				t = fresh
			}
		}
	}
	if err != nil {
		t.StoragePath = prevStoragePath
		return false, fmt.Errorf("%s: failed to re-derive template %q: %w", p.Label(), t.Name, err)
	}
	p.s.importTemplateHarnessConfigs(ctx, cd, t.Scope, t.ScopeID)
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
func (p *templatePersistence) UploadFiles(ctx context.Context, stor storage.Storage, root *os.Root, _ string, files []transfer.FileInfo) ([]store.TemplateFile, map[string]struct{}, error) {
	t := p.model
	if t == nil {
		return nil, nil, fmt.Errorf("%s: no template loaded for upload", p.Label())
	}
	written, err := uploadDirBlobs(ctx, stor, root, templateBlobRow(t), p.s.templateContentBase(t), files, p.Label())
	if err != nil {
		return nil, nil, err
	}
	p.written = written
	return toResourceFiles(files), blobPathSet(written), nil
}

// uploadDirBlobs writes a collected directory's files as blobs under base
// (the row's content base: its own path for a blob row, the path its commit
// migrates it to for a legacy row) and returns the blob paths it wrote.
// Blobs the row already references are skipped while they are present;
// every other blob is written, which also refreshes one that was present but
// unreferenced, so the garbage collector cannot remove it before the commit
// (F5b). It never writes <StoragePath>/<path>, so a co-located broker's
// direct read of a blob row keeps missing. Templates and harness-configs
// share it. Each file is read through root (opened at the collected
// directory) with transfer.ReadCollectedFile, so a file that is no longer a
// regular file of the collected size and hash fails the upload.
func uploadDirBlobs(ctx context.Context, stor storage.Storage, root *os.Root, row *blobRow, base string, files []transfer.FileInfo, label string) (map[string]bool, error) {
	referenced := make(map[string]bool)
	if row.isBlobs() {
		for _, f := range row.Files {
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
			return nil, fmt.Errorf("%s: file %s has no content hash", label, fi.Path)
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
				return nil, fmt.Errorf("%s: failed to check blob of %s: %w", label, fi.Path, err)
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
			data, err := transfer.ReadCollectedFile(root, j.fi)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			if err := uploadTemplateBlob(gctx, stor, j.blobPath, j.fi.Hash, data); err != nil {
				return fmt.Errorf("%s: failed to upload file %s: %w", label, j.fi.Path, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	written := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		written[j.blobPath] = true
	}
	return written, nil
}

// blobPathSet converts uploadDirBlobs' result to the set the
// resourceFileUploader interface returns.
func blobPathSet(written map[string]bool) map[string]struct{} {
	out := make(map[string]struct{}, len(written))
	for p := range written {
		out[p] = struct{}{}
	}
	return out
}

// CheckDir refuses a template directory that bundles a harness-config with
// an unusable provisioner, before the shared store uploads anything. The
// commit path runs the same check again (defence in depth).
func (p *templatePersistence) CheckDir(ctx context.Context, _ string, cd *collectedDir) error {
	if err := checkBundledHarnessConfigs(ctx, cd.fileReader(), toResourceFiles(cd.files)); err != nil {
		return fmt.Errorf("%s: template in %q not imported: %w", p.Label(), cd.base(), err)
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

func (p *templatePersistence) PostFinalize(ctx context.Context, rec *ResourceRecord, cd *collectedDir) {
	p.s.importTemplateHarnessConfigs(ctx, cd, rec.Scope, rec.ScopeID)
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

// harnessConfigPersistence persists harness-configs for the shared
// ResourceStore. Every write of files or derived fields goes through
// commitHarnessConfigFiles (ptone/scion#4219); harness is only the fallback
// Harness of the pending row Create writes before the first commit.
type harnessConfigPersistence struct {
	s       *Server
	harness string
	model   *store.HarnessConfig
	// written lists the blob objects UploadFiles wrote, which the commit in
	// Update does not check again.
	written map[string]bool
	// nameFromConfig is set when the caller took the resource name from
	// config.yaml before collection (the import's directory scan). CheckDir
	// then requires the collected config.yaml to carry the same name.
	nameFromConfig bool
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

// Create writes the pending row. It carries no files and no derived Config:
// the Update that follows the upload commits both through
// commitHarnessConfigFiles. Harness is set because the column is non-empty;
// the commit replaces it with the derived value.
func (p *harnessConfigPersistence) Create(ctx context.Context, rec *ResourceRecord, cd *collectedDir) error {
	harnessType := p.harness
	if idx := deriveHarnessConfigIndexFromCollected(cd, rec.Name); idx.Harness != "" {
		harnessType = idx.Harness
	}
	hc := &store.HarnessConfig{
		ID:            rec.ID,
		Name:          rec.Name,
		Slug:          rec.Slug,
		Harness:       harnessType,
		Scope:         rec.Scope,
		ScopeID:       rec.ScopeID,
		Status:        rec.Status,
		StorageBucket: rec.StorageBucket,
		SourceURL:     rec.SourceURL,
	}
	// New harness-configs are created in the blob layout, with a row-unique
	// storage path (ptone/scion#4219).
	hc.Layout = store.HarnessConfigLayoutBlobs
	hc.StoragePath = p.s.harnessConfigBlobStoragePath(hc)
	hc.StorageURI = storage.StorageURIForPath(rec.StorageBucket, hc.StoragePath)
	rec.StoragePath = hc.StoragePath
	rec.StorageURI = hc.StorageURI
	rec.Layout = hc.Layout
	rec.Harness = harnessType
	p.model = hc
	return p.s.store.CreateHarnessConfig(ctx, hc)
}

// Update commits the uploaded manifest through commitHarnessConfigFiles,
// reading config.yaml from the collected directory. A commit that loses the
// compare-and-swap is
// retried once against the re-read row; a second conflict, or a refused
// commit (an unusable provisioner), returns an error and leaves the row
// unchanged, and the bootstrap and import callers log it and skip the
// resource.
func (p *harnessConfigPersistence) Update(ctx context.Context, rec *ResourceRecord, cd *collectedDir) error {
	hc := p.model
	apply := func(hc *store.HarnessConfig) {
		hc.Status = rec.Status
		if rec.SourceURL != "" {
			hc.SourceURL = rec.SourceURL
		}
	}
	// A retry against a re-read row finds the uploaded blobs again (they
	// are idempotent), and writes from the collected directory any blob
	// missing under the fresh row's content base.
	opts := harnessConfigCommitOpts{collected: cd, written: p.written}

	work := *hc
	apply(&work)
	err := p.s.commitHarnessConfigFiles(ctx, &work, rec.Files, opts)
	if errors.Is(err, store.ErrHarnessConfigConflict) {
		var fresh *store.HarnessConfig
		fresh, err = p.rereadAfterConflict(ctx, hc)
		if err == nil {
			work = *fresh
			apply(&work)
			err = p.s.commitHarnessConfigFiles(ctx, &work, rec.Files, opts)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: harness config %q not updated: %w", p.Label(), hc.Name, err)
	}
	*hc = work
	rec.Harness = hc.Harness
	return nil
}

// rereadAfterConflict reloads hc after a commit lost its compare-and-swap,
// logging the retry.
func (p *harnessConfigPersistence) rereadAfterConflict(ctx context.Context, hc *store.HarnessConfig) (*store.HarnessConfig, error) {
	p.s.resourceLog.Warn(p.Label()+": harness config changed during commit; re-reading and retrying once",
		"harness_config", hc.Name, "id", hc.ID)
	return p.s.store.GetHarnessConfig(ctx, hc.ID)
}

// OnHashMatch re-derives the index from the collected directory when the
// content is unchanged
// but the stored derived fields differ from what config.yaml gives (a row
// written before the clear-on-absent rule, or before a field was derived).
// The re-derivation goes through commitHarnessConfigFiles. A conflict is
// retried once while the re-read row still has the directory's content
// hash; otherwise another writer replaced the content and the resource is
// skipped.
func (p *harnessConfigPersistence) OnHashMatch(ctx context.Context, rec *ResourceRecord, cd *collectedDir) (bool, error) {
	hc := p.model
	if harnessConfigIndexMatches(hc, deriveHarnessConfigIndexFromCollected(cd, hc.Name)) {
		return false, nil
	}
	opts := harnessConfigCommitOpts{collected: cd}
	work := *hc
	err := p.s.commitHarnessConfigFiles(ctx, &work, work.Files, opts)
	if errors.Is(err, store.ErrHarnessConfigConflict) {
		var fresh *store.HarnessConfig
		fresh, err = p.rereadAfterConflict(ctx, hc)
		if err == nil {
			if fresh.ContentHash != rec.ContentHash {
				p.s.resourceLog.Warn(p.Label()+": harness config content replaced by another commit; skipping re-derive",
					"harness_config", hc.Name, "id", hc.ID)
				return false, nil
			}
			work = *fresh
			err = p.s.commitHarnessConfigFiles(ctx, &work, work.Files, opts)
		}
	}
	if err != nil {
		return false, fmt.Errorf("%s: failed to re-derive harness config %q: %w", p.Label(), hc.Name, err)
	}
	*hc = work
	p.s.resourceLog.Info(p.Label()+": re-derived harness config index",
		"harness_config", hc.Name, "harness", hc.Harness)
	return false, nil
}

// PostFinalize starts an image status check for the committed image. It
// writes nothing to the row: the commit derived Config.Image.
func (p *harnessConfigPersistence) PostFinalize(ctx context.Context, rec *ResourceRecord, _ *collectedDir) {
	image := effectiveHarnessConfigImage(p.model)
	if image == "" {
		return
	}
	go p.s.checkAndUpdateImageStatus(context.WithoutCancel(ctx), rec.ID, image)
}

// CheckDir refuses a harness-config directory whose config.yaml declares a
// provisioner that could never provision an agent, before the shared store
// uploads anything (no row, no storage change). It applies the one content
// check, unusableProvisionerInYAML; the commit runs it again.
func (p *harnessConfigPersistence) CheckDir(_ context.Context, name string, cd *collectedDir) error {
	data, present, err := cd.harnessConfigYAML()
	if err != nil {
		return fmt.Errorf("%s: harness config in %q not imported: %w", p.Label(), cd.base(), err)
	}
	if !present {
		// No config.yaml was collected (missing, a link or another
		// non-regular entry): the resource is refused, never stored without
		// it. Every caller requires config.yaml before collection.
		return fmt.Errorf("%s: harness config in %q not imported: no config.yaml was collected (missing, a link or not a regular file)", p.Label(), cd.base())
	}
	if p.nameFromConfig {
		// The caller named the resource from config.yaml before collection:
		// the collected config.yaml must still carry that name. A config.yaml
		// over the parse limit (data is nil) is read in full, verified, for
		// the name only, so the check holds for it too.
		nameData := data
		if nameData == nil {
			if nameData, err = cd.read(harnessConfigYAMLPath); err != nil {
				return fmt.Errorf("%s: harness config in %q not imported: %w", p.Label(), cd.base(), err)
			}
		}
		entry, perr := config.ParseHarnessConfigYAML(nameData)
		if perr != nil {
			return fmt.Errorf("%s: harness config in %q not imported: the collected config.yaml does not parse: %w", p.Label(), cd.base(), perr)
		}
		if entry.Name != name {
			return fmt.Errorf("%s: harness config in %q not imported: config.yaml names it %q, not %q as when it was read before collection", p.Label(), cd.base(), entry.Name, name)
		}
	}
	if perr := unusableProvisionerInYAML(cd.base(), data); perr != nil {
		return fmt.Errorf("%s: harness config in %q not imported: %w", p.Label(), cd.base(), &unusableHarnessConfigError{perr: perr})
	}
	return nil
}

// UploadFiles writes the directory's files as blobs under the
// harness-config's content base (uploadDirBlobs). Together with the commit
// this replaces the shared path upload and its prefix reconcile for
// harness-configs (ptone/scion#4219, ptone/scion#3170).
func (p *harnessConfigPersistence) UploadFiles(ctx context.Context, stor storage.Storage, root *os.Root, _ string, files []transfer.FileInfo) ([]store.TemplateFile, map[string]struct{}, error) {
	hc := p.model
	if hc == nil {
		return nil, nil, fmt.Errorf("%s: no harness config loaded for upload", p.Label())
	}
	written, err := uploadDirBlobs(ctx, stor, root, harnessConfigBlobRow(hc), p.s.harnessConfigContentBase(hc), files, p.Label())
	if err != nil {
		return nil, nil, err
	}
	p.written = written
	return toResourceFiles(files), blobPathSet(written), nil
}

// targetHarnessConfigPersistence is the harness-config persistence used by
// reimport (ptone/scion#4219). It always resolves the record to update by
// the target's ID, never by the slug of the source entry, so a reimport
// writes into the row it was asked to refresh even if that row's slug
// changed meanwhile, and it never creates a harness-config.
type targetHarnessConfigPersistence struct {
	*harnessConfigPersistence
	id string
}

func (p *targetHarnessConfigPersistence) GetBySlug(ctx context.Context, _, scope, scopeID string) (*ResourceRecord, error) {
	hc, err := p.s.store.GetHarnessConfig(ctx, p.id)
	if err != nil {
		return nil, fmt.Errorf("harness config to reimport could not be loaded: %w", err)
	}
	if hc.Scope != scope || hc.ScopeID != scopeID {
		return nil, errors.New("harness config scope changed during reimport")
	}
	p.model = hc
	return harnessConfigToRecord(hc), nil
}

func (p *targetHarnessConfigPersistence) Create(context.Context, *ResourceRecord, *collectedDir) error {
	return errors.New("reimporting a harness config never creates a new harness config")
}

// targetHarnessConfigStore returns a ResourceStore that writes only into the
// harness-config with the given ID.
func (s *Server) targetHarnessConfigStore(id string) *ResourceStore {
	return &ResourceStore{
		srv:             s,
		pers:            &targetHarnessConfigPersistence{harnessConfigPersistence: &harnessConfigPersistence{s: s}, id: id},
		hubID:           s.HubID(),
		excludePatterns: config.HarnessConfigTransientPatterns,
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
		Layout:        hc.Layout,
		Files:         hc.Files,
		Status:        hc.Status,
		SourceURL:     hc.SourceURL,
	}
}
