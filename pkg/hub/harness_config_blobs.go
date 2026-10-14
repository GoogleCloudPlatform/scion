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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// harness_config_blobs.go holds the content-addressed ("blob") storage
// layout of harness-config files (ptone/scion#4219 part 2). It is the
// template blob layout (template_blobs.go) applied to harness-configs, and it
// shares that file's resolver, staging, signing and garbage-collection
// machinery through blobRow:
//
//   - A blob-layout row (store.HarnessConfigLayoutBlobs) has the row-unique
//     storage path <HarnessConfigStoragePath(hub, scope, scopeID, slug)>.<id>
//     and stores each file once, by content, at <StoragePath>.blobs/<sha256>.
//   - Uploads are staged at <StoragePath>.staging/<upload id>/<file path> and
//     moved into .blobs by the commit, which hashes them on the hub.
//   - Nothing writes <StoragePath>/ itself, so a co-located broker's direct
//     read of that directory misses and the broker hydrates through the
//     hub's download URLs.
//   - A commit never deletes a blob. Blobs no manifest references are
//     deleted by the blob garbage collector after its grace period
//     (template_blob_gc.go), which replaces the prefix reconcile that the
//     legacy layout used for harness-configs (ptone/scion#3170).
//   - A legacy row (Layout "") keeps its files at <StoragePath>/<path> until
//     its next commit migrates it, and its legacy tree is then removed only
//     when no other harness-config shares the path or is nested under it.

// isHarnessConfigBlobLayout reports whether hc stores its files as blobs.
func isHarnessConfigBlobLayout(hc *store.HarnessConfig) bool {
	return hc != nil && hc.Layout == store.HarnessConfigLayoutBlobs
}

// harnessConfigBlobRow is hc's blobRow, nil for a nil hc.
func harnessConfigBlobRow(hc *store.HarnessConfig) *blobRow {
	if hc == nil {
		return nil
	}
	return &blobRow{ID: hc.ID, StoragePath: hc.StoragePath, Layout: hc.Layout, Files: hc.Files}
}

// harnessConfigBlobStoragePath is the row-unique storage path a
// harness-config gets when it enters the blob layout. Slugs cannot contain
// ".", so it never collides with another harness-config's slug path or a
// legacy clone's <slug>/<id> path.
func (s *Server) harnessConfigBlobStoragePath(hc *store.HarnessConfig) string {
	return storage.HarnessConfigStoragePath(s.HubID(), hc.Scope, hc.ScopeID, hc.Slug) + "." + hc.ID
}

// harnessConfigContentBase is the storage path that holds hc's blobs and
// staged uploads: its own StoragePath for a blob row, and the path its next
// commit migrates it to for a legacy row.
func (s *Server) harnessConfigContentBase(hc *store.HarnessConfig) string {
	if isHarnessConfigBlobLayout(hc) {
		return hc.StoragePath
	}
	return s.harnessConfigBlobStoragePath(hc)
}

// harnessConfigLegacyPath is where a legacy row's files live: its storage
// path, or for a row written without one the computed path the shared
// ResourceStore used for it.
func (s *Server) harnessConfigLegacyPath(hc *store.HarnessConfig) string {
	if hc.StoragePath != "" {
		return hc.StoragePath
	}
	return storage.ResourceStoragePath(s.HubID(), storage.ResourceKindHarnessConfig, hc.Scope, hc.ScopeID, hc.Slug)
}

// harnessConfigObjectPath is the storage object that holds file f of hc: its
// blob for a blob row, its path object for a legacy row. Every reader maps a
// manifest entry to an object through it.
func (s *Server) harnessConfigObjectPath(hc *store.HarnessConfig, f store.TemplateFile) string {
	if isHarnessConfigBlobLayout(hc) {
		if hex, ok := templateBlobHex(f.Hash); ok {
			return templateBlobPath(hc.StoragePath, hex)
		}
	}
	return s.harnessConfigLegacyPath(hc) + "/" + f.Path
}

// harnessConfigFileObjectPath is harnessConfigObjectPath for callers without
// a Server (the image-build executors): a legacy row's object is read from
// its recorded storage path.
func harnessConfigFileObjectPath(hc *store.HarnessConfig, f store.TemplateFile) string {
	if isHarnessConfigBlobLayout(hc) {
		if hex, ok := templateBlobHex(f.Hash); ok {
			return templateBlobPath(hc.StoragePath, hex)
		}
	}
	return hc.StoragePath + "/" + f.Path
}

// readHarnessConfigFile reads manifest file p of hc, refusing more than
// limit bytes. A path the manifest does not list is storage.ErrNotFound.
func (s *Server) readHarnessConfigFile(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig, p string, limit int64) ([]byte, error) {
	f, ok := templateFileByPath(hc.Files, p)
	if !ok {
		return nil, storage.ErrNotFound
	}
	return readTemplateObject(ctx, stor, s.harnessConfigObjectPath(hc, f), limit)
}

// writeHarnessConfigFileBlob stores data as a blob under hc's content base
// and returns the manifest hash and the blob's object path. The file APIs
// call it before committing; the write always happens, so a blob that
// already existed unreferenced is refreshed (F5b).
func (s *Server) writeHarnessConfigFileBlob(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig, data []byte) (string, string, error) {
	hash := transfer.HashBytes(data)
	hex, _ := templateBlobHex(hash)
	blobPath := templateBlobPath(s.harnessConfigContentBase(hc), hex)
	if err := uploadTemplateBlob(ctx, stor, blobPath, hash, data); err != nil {
		return "", "", err
	}
	return hash, blobPath, nil
}

// harnessConfigStagedObjectPath is where an upload with the given ID stages
// file p of hc.
func (s *Server) harnessConfigStagedObjectPath(hc *store.HarnessConfig, uploadID, p string) string {
	return templateStagedObjectPath(s.harnessConfigContentBase(hc), uploadID, p)
}

// generateHarnessConfigUploadURLs returns upload URLs that stage files for
// the upload ID under hc's content base. The finalize commit hashes and
// moves them; nothing is visible to readers until then. With local storage
// the URL is the hub's file endpoint with ?uploadId=, which stages without
// committing (handleHarnessConfigFileWrite).
func (s *Server) generateHarnessConfigUploadURLs(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig, files []FileUploadRequest, uploadID, hubEndpoint string) ([]UploadURLInfo, error) {
	return generateStagingUploadURLs(ctx, stor, s.harnessConfigContentBase(hc), "harness-configs", hc.ID, files, uploadID, hubEndpoint)
}

// generateHarnessConfigDownloadURLs returns signed GET URLs for hc's files:
// each file's blob for a blob row (no legacy fallback, no manifest.json),
// the path layout with its legacy fallback for a legacy row.
func (s *Server) generateHarnessConfigDownloadURLs(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig) ([]DownloadURLInfo, string, time.Time, error) {
	row := harnessConfigBlobRow(hc)
	if !row.isBlobs() {
		row.StoragePath = s.harnessConfigLegacyPath(hc)
	}
	return s.generateBlobRowDownloadURLs(ctx, stor, row)
}

// removeLegacyHarnessConfigTree removes a migrated row's legacy tree
// (<oldPath>/), unless another harness-config still uses it: a row with the
// same storage path (a renamed config and a new one with the old name can
// share a legacy path), or a row whose storage path is nested under it
// (legacy clones live at <slug path>/<clone id>, so DeletePrefix of the
// parent's path would remove them too). DeletePrefix removes the directory
// itself on local storage, so a co-located broker's direct read of the old
// path misses instead of finding an empty directory.
func (s *Server) removeLegacyHarnessConfigTree(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig, oldPath string) {
	if oldPath == "" || oldPath == hc.StoragePath || storage.DirPrefix(oldPath) == "" {
		return
	}
	for _, filter := range []store.HarnessConfigFilter{
		{StoragePath: oldPath},
		{StoragePathPrefix: storage.DirPrefix(oldPath)},
	} {
		others, err := s.store.ListHarnessConfigs(ctx, filter, store.ListOptions{Limit: 10, SkipTotalCount: true})
		if err != nil {
			s.resourceLog.Warn("harness-config migration: cannot check for rows using the legacy path; keeping it",
				"harness_config", hc.Name, "id", hc.ID, "error", err)
			return
		}
		if others == nil {
			s.resourceLog.Warn("harness-config migration: no result checking for rows using the legacy path; keeping it",
				"harness_config", hc.Name, "id", hc.ID)
			return
		}
		for _, o := range others.Items {
			if o.ID != hc.ID {
				s.resourceLog.Info("harness-config migration: legacy path is still used by another harness-config; keeping it",
					"harness_config", hc.Name, "id", hc.ID, "other", o.ID, "otherPath", o.StoragePath)
				return
			}
		}
	}
	if err := stor.DeletePrefix(ctx, storage.DirPrefix(oldPath)); err != nil {
		s.resourceLog.Warn("harness-config migration: failed to remove the legacy tree",
			"harness_config", hc.Name, "id", hc.ID, "error", err)
	}
}

// deleteHarnessConfigStorage deletes everything a harness-config row owns in
// storage: its blobs and staged uploads, and for a legacy row its legacy
// tree plus any blobs or staged uploads already written for its migration.
func (s *Server) deleteHarnessConfigStorage(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig) error {
	var prefixes []string
	if isHarnessConfigBlobLayout(hc) {
		if hc.StoragePath == "" {
			return nil
		}
		prefixes = append(prefixes, templateBlobPrefix(hc.StoragePath), templateStagingPrefix(hc.StoragePath))
	} else {
		if hc.StoragePath != "" {
			prefixes = append(prefixes, storage.DirPrefix(hc.StoragePath))
		}
		if hc.ID != "" && hc.Slug != "" {
			base := s.harnessConfigBlobStoragePath(hc)
			prefixes = append(prefixes, templateBlobPrefix(base), templateStagingPrefix(base))
		}
	}
	var firstErr error
	for _, p := range prefixes {
		if err := stor.DeletePrefix(ctx, p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// checkHarnessConfigBlobs logs every manifest entry of a blob row whose blob
// is missing. It changes nothing: a missing blob needs a new push of that
// file, and repair never drops a blob row's entries.
func (s *Server) checkHarnessConfigBlobs(ctx context.Context, hc *store.HarnessConfig) {
	stor := s.GetStorage()
	if stor == nil {
		return
	}
	for _, f := range hc.Files {
		hex, ok := templateBlobHex(f.Hash)
		if !ok {
			s.resourceLog.Warn("harness-config repair: manifest entry has no content hash",
				"harness_config", hc.Name, "id", hc.ID, "file", f.Path)
			continue
		}
		exists, err := stor.Exists(ctx, templateBlobPath(hc.StoragePath, hex))
		if err != nil {
			s.resourceLog.Warn("harness-config repair: cannot check blob",
				"harness_config", hc.Name, "id", hc.ID, "file", f.Path, "error", err)
			continue
		}
		if !exists {
			s.resourceLog.Warn("harness-config repair: blob missing from storage; push the file again",
				"harness_config", hc.Name, "id", hc.ID, "file", f.Path, "hash", f.Hash)
		}
	}
}

// errHarnessConfigBlobRead wraps a failure to read a committed blob.
var errHarnessConfigBlobRead = errors.New("read harness-config blob")

// readHarnessConfigBlob reads the blob of manifest entry p under base (the
// derivation step of a commit). A manifest without p is storage.ErrNotFound.
func readHarnessConfigBlob(ctx context.Context, stor storage.Storage, base string, files []store.TemplateFile, p string, limit int64) ([]byte, error) {
	f, ok := templateFileByPath(files, p)
	if !ok {
		return nil, storage.ErrNotFound
	}
	hex, ok := templateBlobHex(f.Hash)
	if !ok {
		return nil, &invalidFileHashError{}
	}
	data, err := readTemplateObject(ctx, stor, templateBlobPath(base, hex), limit)
	if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, errTemplateObjectTooLarge) {
		return nil, fmt.Errorf("%w %s: %w", errHarnessConfigBlobRead, p, err)
	}
	return data, err
}
