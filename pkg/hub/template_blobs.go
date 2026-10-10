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
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// template_blobs.go holds the content-addressed ("blob") storage layout of
// template files (ptone/scion#4221 part 2).
//
// A blob-layout row (store.TemplateLayoutBlobs) has a row-unique storage path
//
//	<TemplateStoragePath(hub, scope, scopeID, slug)>.<template id>
//
// and stores each file once, by content, at <StoragePath>.blobs/<sha256 hex>.
// Uploads are staged at <StoragePath>.staging/<upload id>/<file path> and
// moved into .blobs by the commit, which hashes them on the hub. Nothing ever
// writes <StoragePath>/ itself, so a co-located broker's direct read of that
// directory misses and the broker hydrates through the hub's download URLs.
//
// Blob objects are never overwritten with different content and never
// deleted by a commit: a hydration that read the old manifest keeps finding
// its blobs, so it sees the complete old version or the complete new one.
// Blobs no manifest references are deleted by the template blob garbage
// collector after a grace period (template_blob_gc.go).
//
// A legacy row (Layout "") keeps its files at <StoragePath>/<file path>
// until its next commit, which migrates it: the commit copies every file
// into the new row-unique path's .blobs, writes the new StoragePath and
// Layout in one compare-and-swap, and then removes the legacy tree, but only
// when no other row has the same StoragePath. That removal is immediate (a
// one-time effect of the migration): a hydration of the legacy version that
// is in flight at that moment can fail once and is retried.

const (
	templateBlobsSuffix   = ".blobs"
	templateStagingSuffix = ".staging"
)

// templateBlobHex returns the hex digest of a manifest hash in the
// "sha256:<64 lowercase hex>" form, and false for any other value.
func templateBlobHex(hash string) (string, bool) {
	if !transfer.IsContentHash(hash) {
		return "", false
	}
	return strings.TrimPrefix(hash, transfer.HashPrefix), true
}

// isBlobLayout reports whether t stores its files as blobs.
func isBlobLayout(t *store.Template) bool {
	return t != nil && t.Layout == store.TemplateLayoutBlobs
}

// templateBlobStoragePath is the row-unique storage path a template gets when
// it enters the blob layout. Slugs cannot contain ".", so it never collides
// with another template's slug path or a clone's <slug>/<id> path.
func (s *Server) templateBlobStoragePath(t *store.Template) string {
	return storage.TemplateStoragePath(s.HubID(), t.Scope, t.ScopeID, t.Slug) + "." + t.ID
}

// templateContentBase is the storage path that holds t's blobs and staged
// uploads: its own StoragePath for a blob row, and the path its next commit
// migrates it to for a legacy row.
func (s *Server) templateContentBase(t *store.Template) string {
	if isBlobLayout(t) {
		return t.StoragePath
	}
	return s.templateBlobStoragePath(t)
}

// templateBlobPath is the object path of the blob with the given hex digest.
func templateBlobPath(base, hex string) string {
	return base + templateBlobsSuffix + "/" + hex
}

// templateBlobPrefix is the List/DeletePrefix prefix of base's blobs.
func templateBlobPrefix(base string) string {
	return storage.DirPrefix(base + templateBlobsSuffix)
}

// templateStagingPrefix is the List/DeletePrefix prefix of base's staged
// uploads.
func templateStagingPrefix(base string) string {
	return storage.DirPrefix(base + templateStagingSuffix)
}

// templateStagedObjectPath is where an upload with the given ID stages a file.
func templateStagedObjectPath(base, uploadID, filePath string) string {
	return templateStagingPrefix(base) + uploadID + "/" + filePath
}

// templateObjectPath is the storage object that holds file f of t: its blob
// for a blob row, its path object for a legacy row. It is the one place
// readers map a manifest entry to an object.
func templateObjectPath(t *store.Template, f store.TemplateFile) string {
	if isBlobLayout(t) {
		if hex, ok := templateBlobHex(f.Hash); ok {
			return templateBlobPath(t.StoragePath, hex)
		}
	}
	return t.StoragePath + "/" + f.Path
}

// templateFileByPath returns the manifest entry for p.
func templateFileByPath(files []store.TemplateFile, p string) (store.TemplateFile, bool) {
	for _, f := range files {
		if f.Path == p {
			return f, true
		}
	}
	return store.TemplateFile{}, false
}

// validTemplateUploadID reports whether id can name a staging directory: a
// short token of letters, digits and dashes (the hub issues UUIDs).
func validTemplateUploadID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// hashMismatchError reports staged content whose server-computed hash does
// not match the manifest. It maps to 400.
type hashMismatchError struct {
	path string
}

func (e *hashMismatchError) Error() string {
	return "uploaded content does not match the manifest hash: " + e.path
}

// invalidFileHashError reports a manifest entry whose hash is not in the
// "sha256:<hex>" form, so its blob cannot be located. It maps to 400.
type invalidFileHashError struct{}

func (e *invalidFileHashError) Error() string {
	return "manifest.files[].hash is not a sha256 content hash"
}

// readTemplateObject downloads an object, refusing more than limit bytes.
func readTemplateObject(ctx context.Context, stor storage.Storage, objectPath string, limit int64) ([]byte, error) {
	reader, _, err := stor.Download(ctx, objectPath)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, storage.ErrNotFound
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("object %s: %w (%d bytes)", objectPath, errTemplateObjectTooLarge, limit)
	}
	return data, nil
}

// errTemplateObjectTooLarge marks an object larger than a read allows.
var errTemplateObjectTooLarge = errors.New("object exceeds the size limit")

// uploadTemplateBlob writes data as the blob at blobPath. Writing a blob is
// idempotent (the path names the content), and every write bumps the
// object's age, which the garbage collector relies on (F5b).
func uploadTemplateBlob(ctx context.Context, stor storage.Storage, blobPath, hash string, data []byte) error {
	_, err := stor.Upload(ctx, blobPath, bytes.NewReader(data), storage.UploadOptions{
		ContentType: "application/octet-stream",
		Metadata:    map[string]string{"sha256": hash},
	})
	return err
}

// writeTemplateFileBlob stores data as a blob under t's content base and
// returns the manifest hash and the blob's object path. The file APIs call
// it before committing; the write always happens, so a blob that already
// existed unreferenced is refreshed (F5b).
func (s *Server) writeTemplateFileBlob(ctx context.Context, stor storage.Storage, t *store.Template, data []byte) (string, string, error) {
	hash := transfer.HashBytes(data)
	hex, _ := templateBlobHex(hash)
	blobPath := templateBlobPath(s.templateContentBase(t), hex)
	if err := uploadTemplateBlob(ctx, stor, blobPath, hash, data); err != nil {
		return "", "", err
	}
	return hash, blobPath, nil
}

// templateBlobResolver makes sure every blob a commit introduces exists under
// the commit's content base before the row is written.
type templateBlobResolver struct {
	s    *Server
	stor storage.Storage
	base string
	opts commitOpts
	// src is where a missing blob is copied from: opts.copyFrom (clones),
	// or the row itself when it is migrating from the legacy layout.
	src *store.Template

	stagedLoaded bool
	staged       map[string][]string // file path -> staged object names (old clients)
}

// ensureTemplateBlobs returns next with every introduced entry backed by a
// blob under base, plus the introduced entries. An entry is introduced when
// its blob is not referenced by tmpl as read: for a blob row that is only
// what this commit adds, so the cost of a commit scales with what it
// changes, not with the manifest (and unrelated missing objects never block
// it). For a legacy row or a new row every entry is introduced.
//
// A legacy source whose object no longer matches its manifest hash is
// stored under its actual hash and the entry is corrected, so the blob store
// never holds content under the wrong name.
func (s *Server) ensureTemplateBlobs(ctx context.Context, stor storage.Storage, tmpl *store.Template, base string, next []store.TemplateFile, opts commitOpts) ([]store.TemplateFile, []store.TemplateFile, error) {
	referenced := make(map[string]bool)
	if isBlobLayout(tmpl) {
		for _, f := range tmpl.Files {
			if hex, ok := templateBlobHex(f.Hash); ok {
				referenced[hex] = true
			}
		}
	}
	r := &templateBlobResolver{s: s, stor: stor, base: base, opts: opts, src: opts.copyFrom}
	if r.src == nil && !isBlobLayout(tmpl) && tmpl.StoragePath != "" {
		legacy := *tmpl
		r.src = &legacy
	}

	out := append([]store.TemplateFile(nil), next...)
	var introduced []store.TemplateFile
	done := make(map[string]bool)
	for i := range out {
		e := &out[i]
		hex, ok := templateBlobHex(e.Hash)
		if ok && referenced[hex] {
			continue
		}
		if ok && done[hex] {
			introduced = append(introduced, *e)
			continue
		}
		got, err := r.resolve(ctx, e, hex, ok)
		if err != nil {
			return nil, nil, err
		}
		done[got] = true
		introduced = append(introduced, *e)
	}
	return out, introduced, nil
}

// resolve makes the blob of entry e exist, trying in order: a blob this
// commit already wrote, a staged upload, the commit's local directory, an
// existing blob (refreshed), and the copy source. It returns the hex digest
// the entry ends up with.
func (r *templateBlobResolver) resolve(ctx context.Context, e *store.TemplateFile, hex string, validHash bool) (string, error) {
	sawMismatch := false
	if validHash {
		blobPath := templateBlobPath(r.base, hex)
		if r.opts.written[blobPath] {
			return hex, nil
		}
		if r.opts.fromStaging {
			found, mismatch, err := r.fromStaging(ctx, e, blobPath)
			if err != nil {
				return "", err
			}
			if found {
				return hex, nil
			}
			sawMismatch = mismatch
		}
		if r.opts.dir != "" {
			found, err := r.fromDir(ctx, e, blobPath)
			if err != nil {
				return "", err
			}
			if found {
				return hex, nil
			}
		}
		found, err := r.refreshExisting(ctx, e, blobPath)
		if err != nil {
			return "", err
		}
		if found {
			return hex, nil
		}
		if r.src != nil && isBlobLayout(r.src) && r.src.StoragePath != "" {
			found, err := r.copyBlob(ctx, templateBlobPath(r.src.StoragePath, hex), blobPath, e.Hash)
			if err != nil {
				return "", err
			}
			if found {
				return hex, nil
			}
		}
	}
	if r.src != nil && !isBlobLayout(r.src) {
		got, err := r.copyLegacy(ctx, e)
		if err == nil {
			return got, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return "", err
		}
	}
	if !validHash {
		return "", &invalidFileHashError{}
	}
	if sawMismatch {
		return "", &hashMismatchError{path: e.Path}
	}
	return "", &fileNotFoundError{path: e.Path}
}

// fromStaging moves a staged upload of e into its blob after checking its
// hash on the hub. With opts.uploadID it looks only in that upload's
// directory, and a mismatch is an error. Without one (clients that predate
// upload IDs) it takes any staged object at */<path> whose hash matches; a
// mismatch is only reported if nothing else supplies the blob.
func (r *templateBlobResolver) fromStaging(ctx context.Context, e *store.TemplateFile, blobPath string) (found, mismatch bool, err error) {
	if r.opts.uploadID != "" {
		staged := templateStagedObjectPath(r.base, r.opts.uploadID, e.Path)
		ok, err := r.moveStaged(ctx, staged, blobPath, e.Hash)
		if errors.Is(err, storage.ErrNotFound) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		if !ok {
			return false, true, &hashMismatchError{path: e.Path}
		}
		return true, false, nil
	}
	if err := r.loadStaged(ctx); err != nil {
		return false, false, err
	}
	names := r.staged[e.Path]
	for i, name := range names {
		ok, err := r.moveStaged(ctx, name, blobPath, e.Hash)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, false, err
		}
		if ok {
			r.staged[e.Path] = append(names[:i:i], names[i+1:]...)
			return true, false, nil
		}
		mismatch = true
	}
	return false, mismatch, nil
}

// loadStaged indexes every staged object under the base by file path, once
// per commit. Its cost is proportional to the uploads in flight, not to the
// manifest.
func (r *templateBlobResolver) loadStaged(ctx context.Context) error {
	if r.stagedLoaded {
		return nil
	}
	r.stagedLoaded = true
	r.staged = make(map[string][]string)
	prefix := templateStagingPrefix(r.base)
	res, err := r.stor.List(ctx, storage.ListOptions{Prefix: prefix})
	if err != nil {
		return fmt.Errorf("list staged uploads: %w", err)
	}
	for _, obj := range res.Objects {
		rest := strings.TrimPrefix(obj.Name, prefix)
		i := strings.Index(rest, "/")
		if i <= 0 || i == len(rest)-1 {
			continue
		}
		p := rest[i+1:]
		r.staged[p] = append(r.staged[p], obj.Name)
	}
	return nil
}

// moveStaged hashes the staged object and, when it matches hash, writes it
// as the blob and deletes the staged copy. ok is false on a mismatch, which
// leaves both objects unchanged.
func (r *templateBlobResolver) moveStaged(ctx context.Context, staged, blobPath, hash string) (bool, error) {
	data, err := readTemplateObject(ctx, r.stor, staged, maxUploadFileSize)
	if err != nil {
		return false, err
	}
	if transfer.HashBytes(data) != hash {
		return false, nil
	}
	if err := uploadTemplateBlob(ctx, r.stor, blobPath, hash, data); err != nil {
		return false, fmt.Errorf("write blob: %w", err)
	}
	if err := r.stor.Delete(ctx, staged); err != nil && !errors.Is(err, storage.ErrNotFound) {
		r.s.templateLog.Warn("template commit: failed to delete staged upload", "object", staged, "error", err)
	}
	return true, nil
}

// fromDir writes e's blob from the commit's local directory (import,
// reimport and bootstrap), when the file is there with the expected hash.
func (r *templateBlobResolver) fromDir(ctx context.Context, e *store.TemplateFile, blobPath string) (bool, error) {
	fp := filepath.Join(r.opts.dir, filepath.FromSlash(e.Path))
	fi, err := os.Stat(fp)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxUploadFileSize {
		return false, nil
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return false, nil
	}
	if transfer.HashBytes(data) != e.Hash {
		return false, nil
	}
	if err := uploadTemplateBlob(ctx, r.stor, blobPath, e.Hash, data); err != nil {
		return false, fmt.Errorf("write blob %s: %w", e.Path, err)
	}
	return true, nil
}

// refreshExisting handles a blob that is already present but not referenced
// by the row as read: it is rewritten with its own (verified) content, which
// bumps its age, so a garbage collection pass that listed it as old and
// unreferenced cannot delete it before this commit lands (F5b). A blob whose
// content does not match its name is treated as missing.
func (r *templateBlobResolver) refreshExisting(ctx context.Context, e *store.TemplateFile, blobPath string) (bool, error) {
	data, err := readTemplateObject(ctx, r.stor, blobPath, maxUploadFileSize)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read blob %s: %w", e.Path, err)
	}
	if transfer.HashBytes(data) != e.Hash {
		r.s.templateLog.Warn("template commit: blob content does not match its name; rewriting it", "object", blobPath)
		return false, nil
	}
	if err := uploadTemplateBlob(ctx, r.stor, blobPath, e.Hash, data); err != nil {
		return false, fmt.Errorf("refresh blob %s: %w", e.Path, err)
	}
	return true, nil
}

// copyBlob copies a blob from another blob row (clones). A server-side copy
// is tried first; providers that cannot copy fall back to read and write.
func (r *templateBlobResolver) copyBlob(ctx context.Context, src, dst, hash string) (bool, error) {
	_, err := r.stor.Copy(ctx, src, dst)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	data, rerr := readTemplateObject(ctx, r.stor, src, maxUploadFileSize)
	if errors.Is(rerr, storage.ErrNotFound) {
		return false, nil
	}
	if rerr != nil {
		return false, fmt.Errorf("copy blob: %w (read: %v)", err, rerr)
	}
	if err := uploadTemplateBlob(ctx, r.stor, dst, hash, data); err != nil {
		return false, fmt.Errorf("copy blob: %w", err)
	}
	return true, nil
}

// copyLegacy copies e from a legacy-layout source (<StoragePath>/<path>, or
// the un-namespaced legacy fallback path) into its blob, hashing it on the
// way. A source whose content no longer matches the manifest is stored
// under its actual hash, and e is corrected. It returns storage.ErrNotFound
// when the source has no such object.
func (r *templateBlobResolver) copyLegacy(ctx context.Context, e *store.TemplateFile) (string, error) {
	if !isCanonicalResourceFilePath(e.Path) {
		return "", storage.ErrNotFound
	}
	paths := []string{r.src.StoragePath + "/" + e.Path}
	if fb := r.s.legacyFallbackPath(r.src.StoragePath); fb != "" {
		paths = append(paths, fb+"/"+e.Path)
	}
	var data []byte
	var err error
	for _, p := range paths {
		data, err = readTemplateObject(ctx, r.stor, p, maxUploadFileSize)
		if !errors.Is(err, storage.ErrNotFound) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	actual := transfer.HashBytes(data)
	if actual != e.Hash {
		r.s.templateLog.Warn("template commit: legacy object does not match its manifest hash; storing its actual content",
			"path", e.Path, "manifestHash", e.Hash, "actualHash", actual)
		e.Hash = actual
		e.Size = int64(len(data))
	}
	hex, _ := templateBlobHex(actual)
	if err := uploadTemplateBlob(ctx, r.stor, templateBlobPath(r.base, hex), actual, data); err != nil {
		return "", fmt.Errorf("write blob %s: %w", e.Path, err)
	}
	return hex, nil
}

// blobFileReader reads template files by manifest path from their blobs
// under base (the derivation step of a commit).
func blobFileReader(stor storage.Storage, base string, files []store.TemplateFile) templateFileReader {
	return func(ctx context.Context, p string) ([]byte, error) {
		f, ok := templateFileByPath(files, p)
		if !ok {
			return nil, storage.ErrNotFound
		}
		hex, ok := templateBlobHex(f.Hash)
		if !ok {
			return nil, storage.ErrNotFound
		}
		data, err := readTemplateObject(ctx, stor, templateBlobPath(base, hex), maxTemplateFileSize)
		if errors.Is(err, errTemplateObjectTooLarge) {
			return nil, errTemplateFileTooLarge
		}
		return data, err
	}
}

// removeLegacyTemplateTree removes a migrated row's legacy tree
// (<oldPath>/), unless another row still has that storage path: renamed and
// re-created templates can share a legacy path, and the other row may still
// be legacy and reading from it. DeletePrefix removes the directory itself
// on local storage, so a co-located broker's direct read of the old path
// misses instead of finding an empty directory.
func (s *Server) removeLegacyTemplateTree(ctx context.Context, stor storage.Storage, tmpl *store.Template, oldPath string) {
	if oldPath == "" || oldPath == tmpl.StoragePath || storage.DirPrefix(oldPath) == "" {
		return
	}
	others, err := s.store.ListTemplates(ctx, store.TemplateFilter{StoragePath: oldPath}, store.ListOptions{Limit: 10, SkipTotalCount: true})
	if err != nil {
		s.templateLog.Warn("template migration: cannot check for rows sharing the legacy path; keeping it",
			"template", tmpl.Name, "id", tmpl.ID, "error", err)
		return
	}
	if others == nil {
		return
	}
	for _, o := range others.Items {
		if o.ID != tmpl.ID {
			s.templateLog.Info("template migration: legacy path is shared with another template; keeping it",
				"template", tmpl.Name, "id", tmpl.ID, "other", o.ID)
			return
		}
	}
	if err := stor.DeletePrefix(ctx, storage.DirPrefix(oldPath)); err != nil {
		s.templateLog.Warn("template migration: failed to remove the legacy tree",
			"template", tmpl.Name, "id", tmpl.ID, "error", err)
	}
}

// deleteTemplateStorage deletes everything a template row owns in storage:
// its blobs and staged uploads, and its legacy tree for a legacy row
// (including blobs or staged uploads already written for its migration).
func (s *Server) deleteTemplateStorage(ctx context.Context, stor storage.Storage, t *store.Template) error {
	var prefixes []string
	if isBlobLayout(t) {
		if t.StoragePath == "" {
			return nil
		}
		prefixes = append(prefixes, templateBlobPrefix(t.StoragePath), templateStagingPrefix(t.StoragePath))
	} else {
		if t.StoragePath != "" {
			prefixes = append(prefixes, storage.DirPrefix(t.StoragePath))
		}
		if t.ID != "" && t.Slug != "" {
			base := s.templateBlobStoragePath(t)
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

// generateTemplateDownloadURLs returns signed GET URLs for a template's
// files. A blob row signs each file's blob (no legacy fallback and no
// manifest.json); a legacy row keeps the path layout with its legacy
// fallback.
func (s *Server) generateTemplateDownloadURLs(ctx context.Context, stor storage.Storage, t *store.Template) ([]DownloadURLInfo, string, time.Time, error) {
	if !isBlobLayout(t) {
		return generateDownloadURLs(ctx, stor, t.StoragePath, s.legacyFallbackPath(t.StoragePath), t.Files)
	}
	expires := time.Now().Add(SignedURLExpiry)
	out := make([]DownloadURLInfo, 0, len(t.Files))
	for _, f := range t.Files {
		hex, ok := templateBlobHex(f.Hash)
		if !ok {
			return nil, "", expires, fmt.Errorf("storage object missing: %s (run validate to check storage consistency): invalid hash", f.Path)
		}
		signed, err := stor.GenerateSignedURL(ctx, templateBlobPath(t.StoragePath, hex), storage.SignedURLOptions{
			Method:  "GET",
			Expires: SignedURLExpiry,
		})
		if err != nil {
			return nil, "", expires, fmt.Errorf("storage object missing: %s (run validate to check storage consistency): %w", f.Path, err)
		}
		out = append(out, DownloadURLInfo{Path: f.Path, URL: signed.URL, Size: f.Size, Hash: f.Hash})
	}
	return out, "", expires, nil
}

// rewriteLocalTemplateDownloadURLs is rewriteLocalDownloadURLs plus, for a
// blob row, the file's hash in the URL. The raw read then serves that blob
// of this template even after a later commit replaced the file, so a
// hydration that spans a commit still gets one consistent version.
func rewriteLocalTemplateDownloadURLs(urls []DownloadURLInfo, hubEndpoint, templateID string, blobs bool) []DownloadURLInfo {
	local := make([]bool, len(urls))
	for i := range urls {
		local[i] = strings.HasPrefix(urls[i].URL, "file://")
	}
	urls = rewriteLocalDownloadURLs(urls, hubEndpoint, "templates", templateID)
	if !blobs || hubEndpoint == "" {
		return urls
	}
	for i := range urls {
		if local[i] && urls[i].Hash != "" {
			urls[i].URL += "&hash=" + url.QueryEscape(urls[i].Hash)
		}
	}
	return urls
}

// generateTemplateUploadURLs returns upload URLs that stage files for the
// upload ID under t's content base. The commit (finalize) hashes and moves
// them; nothing is visible to readers until then. With local storage the URL
// is the hub's raw file endpoint with ?uploadId=, which stages without
// committing.
func (s *Server) generateTemplateUploadURLs(ctx context.Context, stor storage.Storage, t *store.Template, files []FileUploadRequest, uploadID, hubEndpoint string) ([]UploadURLInfo, error) {
	if err := validateUploadFilePaths(files); err != nil {
		return nil, err
	}
	base := s.templateContentBase(t)
	out := make([]UploadURLInfo, 0, len(files))
	for _, f := range files {
		signed, err := stor.GenerateSignedURL(ctx, templateStagedObjectPath(base, uploadID, f.Path), storage.SignedURLOptions{
			Method:  "PUT",
			Expires: SignedURLExpiry,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to generate signed URL for file %q: %w", f.Path, err)
		}
		out = append(out, UploadURLInfo{
			Path:    f.Path,
			URL:     signed.URL,
			Method:  signed.Method,
			Headers: signed.Headers,
			Expires: signed.Expires,
		})
	}
	if stor.Provider() == storage.ProviderLocal && hubEndpoint != "" {
		local := make([]bool, len(out))
		for i := range out {
			local[i] = strings.HasPrefix(out[i].URL, "file://")
		}
		out = rewriteLocalUploadURLs(out, hubEndpoint, "templates", t.ID)
		for i := range out {
			if local[i] {
				out[i].URL += "?uploadId=" + url.QueryEscape(uploadID)
			}
		}
	}
	return out, nil
}
