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
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// ResourceSource provides the metadata and files for a resource to bootstrap
// into the Hub's storage backend and database.
type ResourceSource interface {
	Kind() storage.ResourceKind
	Name() string
	Scope() (scope string, scopeID string)
	SourceURL() string
	Files(ctx context.Context) ([]transfer.FileInfo, error)
	Metadata(ctx context.Context) (ResourceMetadata, error)
}

// ResourceMetadata holds the identity fields of a resource source.
type ResourceMetadata struct {
	Kind      storage.ResourceKind
	Name      string
	Scope     string
	ScopeID   string
	SourceURL string
	Harness   string
}

// BootstrapOptions controls how BootstrapSource creates or updates resources.
type BootstrapOptions struct {
	Force           bool
	RepairStorage   bool
	AdoptExisting   bool
	OverwritePolicy OverwritePolicy
}

// OverwritePolicy determines which existing resources BootstrapSource may overwrite.
type OverwritePolicy int

const (
	OverwriteBuiltinManaged OverwritePolicy = iota // only overwrite built-in-managed resources
	OverwriteAlways                                // admin force
	OverwriteNever                                 // read-only
)

// BootstrapResult reports what BootstrapSource did for a single resource.
type BootstrapResult struct {
	Created  int
	Updated  int
	Repaired int
	Skipped  int
	Failed   int
}

// IsBuiltinManaged returns true if sourceURL identifies a resource managed by
// the built-in bundled catalog.
func IsBuiltinManaged(sourceURL string) bool {
	return strings.HasPrefix(sourceURL, "builtin://scion/")
}

// FSResourceSource implements ResourceSource for a bundled BundledResource.
// Files are read from the embedded fs.FS and staged to a temporary directory
// when BootstrapSource needs to upload them.
type FSResourceSource struct {
	resource resources.BundledResource
}

// NewFSResourceSource returns a ResourceSource backed by a BundledResource.
func NewFSResourceSource(r resources.BundledResource) *FSResourceSource {
	return &FSResourceSource{resource: r}
}

func (s *FSResourceSource) Kind() storage.ResourceKind { return s.resource.Kind }
func (s *FSResourceSource) Name() string               { return s.resource.Name }

func (s *FSResourceSource) Scope() (string, string) {
	return s.resource.Scope, s.resource.ScopeID
}

func (s *FSResourceSource) SourceURL() string { return s.resource.SourceURL }

// Files walks the embedded fs.FS rooted at Root and returns file metadata with
// computed content hashes. The returned FileInfo entries have empty FullPath
// since they are not backed by the local filesystem.
func (s *FSResourceSource) Files(ctx context.Context) ([]transfer.FileInfo, error) {
	var files []transfer.FileInfo
	fsys := s.resource.FS
	root := s.resource.Root

	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		relPath := path
		if root != "." && root != "" {
			relPath = strings.TrimPrefix(path, root+"/")
		}
		relPath = filepath.ToSlash(relPath)

		data, readErr := fs.ReadFile(fsys, filepath.ToSlash(path))
		if readErr != nil {
			return readErr
		}

		hash := sha256.Sum256(data)
		files = append(files, transfer.FileInfo{
			Path: relPath,
			Size: int64(len(data)),
			Hash: fmt.Sprintf("sha256:%x", hash),
			Mode: "0644",
		})
		return nil
	})

	return files, err
}

// Metadata returns the identity fields of the bundled resource.
func (s *FSResourceSource) Metadata(ctx context.Context) (ResourceMetadata, error) {
	r := s.resource
	return ResourceMetadata{
		Kind:      r.Kind,
		Name:      r.Name,
		Scope:     r.Scope,
		ScopeID:   r.ScopeID,
		SourceURL: r.SourceURL,
	}, nil
}

// stageResourceSource writes a ResourceSource's files to a temporary directory
// so they can be uploaded by the existing uploadResourceFiles helper. The caller
// must invoke the returned cleanup function when done.
func stageResourceSource(src ResourceSource) (dir string, cleanup func(), err error) {
	fsSrc, ok := src.(*FSResourceSource)
	if !ok {
		return "", nil, fmt.Errorf("unsupported resource source type %T", src)
	}

	dir, err = os.MkdirTemp("", "scion-bootstrap-*")
	if err != nil {
		return "", nil, err
	}

	fsys := fsSrc.resource.FS
	root := fsSrc.resource.Root

	walkErr := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath := path
		if root != "." && root != "" {
			relPath = strings.TrimPrefix(path, root+"/")
		}
		relPath = filepath.ToSlash(relPath)
		if relPath == "" || relPath == root {
			return nil
		}

		target := filepath.Join(dir, relPath)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		data, readErr := fs.ReadFile(fsys, filepath.ToSlash(path))
		if readErr != nil {
			return readErr
		}

		if mkErr := os.MkdirAll(filepath.Dir(target), 0755); mkErr != nil {
			return mkErr
		}
		return os.WriteFile(target, data, 0644)
	})

	if walkErr != nil {
		_ = os.RemoveAll(dir)
		return "", nil, walkErr
	}

	cleanup = func() { _ = os.RemoveAll(dir) }
	return dir, cleanup, nil
}

// BootstrapSource imports a resource from a ResourceSource into the Hub's
// storage backend and database. It applies the OverwritePolicy to decide
// whether to create, update, or skip existing resources.
func (rs *ResourceStore) BootstrapSource(ctx context.Context, src ResourceSource, opts BootstrapOptions) (BootstrapResult, error) {
	result := BootstrapResult{}
	srv := rs.srv
	p := rs.pers
	stor := srv.GetStorage()
	if stor == nil {
		result.Failed++
		return result, fmt.Errorf("storage backend is not configured")
	}

	meta, err := src.Metadata(ctx)
	if err != nil {
		result.Failed++
		return result, err
	}

	slug := api.Slugify(meta.Name)
	existing, err := p.GetBySlug(ctx, slug, meta.Scope, meta.ScopeID)
	if err != nil {
		result.Failed++
		return result, err
	}

	if existing != nil {
		switch opts.OverwritePolicy {
		case OverwriteNever:
			result.Skipped++
			return result, nil
		case OverwriteBuiltinManaged:
			if !IsBuiltinManaged(existing.SourceURL) && !opts.Force {
				srv.templateLog.Warn(p.Label()+": skipping non-built-in conflict",
					"name", meta.Name, "existingSource", existing.SourceURL)
				result.Skipped++
				return result, nil
			}
		case OverwriteAlways:
			// proceed
		}
	}

	dir, cleanup, err := stageResourceSource(src)
	if err != nil {
		result.Failed++
		return result, fmt.Errorf("%s: stage source: %w", p.Label(), err)
	}
	defer cleanup()

	files, err := transfer.CollectFiles(dir, nil)
	if err != nil {
		result.Failed++
		return result, err
	}

	if existing == nil {
		return rs.bootstrapSourceCreate(ctx, meta, slug, dir, files, &result)
	}
	return rs.bootstrapSourceUpdate(ctx, meta, existing, dir, files, opts, &result)
}

// bootstrapSourceCreate handles the create path for a new resource.
func (rs *ResourceStore) bootstrapSourceCreate(
	ctx context.Context,
	meta ResourceMetadata,
	slug, dir string,
	files []transfer.FileInfo,
	result *BootstrapResult,
) (BootstrapResult, error) {
	srv := rs.srv
	p := rs.pers
	kind := p.Kind()
	stor := srv.GetStorage()

	storagePath := storage.ResourceStoragePath(kind, meta.Scope, meta.ScopeID, slug)
	rec := &ResourceRecord{
		Kind:          kind,
		ID:            api.NewUUID(),
		Name:          meta.Name,
		Slug:          slug,
		Scope:         meta.Scope,
		ScopeID:       meta.ScopeID,
		Status:        resourceStatusPending,
		StoragePath:   storagePath,
		StorageBucket: stor.Bucket(),
		StorageURI:    storage.ResourceStorageURI(stor.Bucket(), kind, meta.Scope, meta.ScopeID, slug),
		SourceURL:     meta.SourceURL,
		Visibility:    p.DefaultVisibility(),
	}
	if err := p.Create(ctx, rec, dir); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			srv.templateLog.Info(p.Label()+": duplicate create race, re-reading",
				"name", meta.Name)
			existing, rerr := p.GetBySlug(ctx, slug, meta.Scope, meta.ScopeID)
			if rerr != nil || existing == nil {
				result.Failed++
				return *result, fmt.Errorf("%s: create race recovery failed: %w", p.Label(), err)
			}
			return rs.bootstrapSourceUpdate(ctx, meta, existing, dir, files, BootstrapOptions{Force: true}, result)
		}
		result.Failed++
		return *result, fmt.Errorf("%s: create failed: %w", p.Label(), err)
	}

	uploaded, _, err := uploadResourceFiles(ctx, stor, storagePath, files, p.Label())
	if err != nil {
		result.Failed++
		return *result, err
	}
	rec.Files = uploaded
	rec.ContentHash = computeContentHash(uploaded)
	rec.Status = resourceStatusActive
	if err := p.Update(ctx, rec, dir); err != nil {
		result.Failed++
		return *result, err
	}

	srv.templateLog.Info(p.Label()+": created resource from source",
		"name", meta.Name, "files", len(uploaded))
	p.PostFinalize(ctx, rec, dir)
	result.Created++
	return *result, nil
}

// bootstrapSourceUpdate handles the update path for an existing resource.
func (rs *ResourceStore) bootstrapSourceUpdate(
	ctx context.Context,
	meta ResourceMetadata,
	existing *ResourceRecord,
	dir string,
	files []transfer.FileInfo,
	opts BootstrapOptions,
	result *BootstrapResult,
) (BootstrapResult, error) {
	srv := rs.srv
	p := rs.pers
	kind := p.Kind()
	stor := srv.GetStorage()

	if !opts.Force {
		newHash := computeContentHash(toResourceFiles(files))
		if newHash == existing.ContentHash {
			if opts.RepairStorage && IsBuiltinManaged(existing.SourceURL) {
				repaired, err := rs.repairStorageIfNeeded(ctx, existing, dir, files)
				if err != nil {
					result.Failed++
					return *result, err
				}
				if repaired {
					result.Repaired++
					return *result, nil
				}
			}
			result.Skipped++
			return *result, nil
		}
	}

	storagePath := existing.StoragePath
	if storagePath == "" {
		storagePath = storage.ResourceStoragePath(kind, existing.Scope, existing.ScopeID, existing.Slug)
	}

	uploaded, written, err := uploadResourceFiles(ctx, stor, storagePath, files, p.Label())
	if err != nil {
		result.Failed++
		return *result, err
	}

	reconcileResourceStorage(ctx, stor, storagePath, existing.Name, written, srv.templateLog, p.Label())

	existing.Files = uploaded
	existing.ContentHash = computeContentHash(uploaded)
	existing.SourceURL = meta.SourceURL
	existing.Status = resourceStatusActive
	if err := p.Update(ctx, existing, dir); err != nil {
		result.Failed++
		return *result, err
	}

	srv.templateLog.Info(p.Label()+": updated resource from source",
		"name", meta.Name)
	p.PostFinalize(ctx, existing, dir)
	result.Updated++
	return *result, nil
}

// repairStorageIfNeeded validates storage for a built-in-managed resource and
// re-uploads any missing objects from the staged source directory.
func (rs *ResourceStore) repairStorageIfNeeded(
	ctx context.Context,
	rec *ResourceRecord,
	dir string,
	files []transfer.FileInfo,
) (bool, error) {
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		return false, fmt.Errorf("repair validate: %w", err)
	}
	if len(report.Issues) == 0 {
		return false, nil
	}

	srv := rs.srv
	p := rs.pers
	stor := srv.GetStorage()
	kind := p.Kind()

	storagePath := rec.StoragePath
	if storagePath == "" {
		storagePath = storage.ResourceStoragePath(kind, rec.Scope, rec.ScopeID, rec.Slug)
	}

	srv.templateLog.Info(p.Label()+": repairing storage",
		"name", rec.Name, "issues", len(report.Issues))

	uploaded, written, err := uploadResourceFiles(ctx, stor, storagePath, files, p.Label())
	if err != nil {
		return false, fmt.Errorf("repair upload: %w", err)
	}

	reconcileResourceStorage(ctx, stor, storagePath, rec.Name, written, srv.templateLog, p.Label())

	rec.Files = uploaded
	rec.ContentHash = computeContentHash(uploaded)
	rec.Status = resourceStatusActive
	if err := p.Update(ctx, rec, dir); err != nil {
		return false, fmt.Errorf("repair update: %w", err)
	}

	srv.templateLog.Info(p.Label()+": repaired storage",
		"name", rec.Name, "files", len(uploaded))
	return true, nil
}
