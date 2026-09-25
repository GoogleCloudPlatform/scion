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
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// errWorkspaceWritesUnavailable is the error message returned when workspace
// writes are blocked because the deployment has no durable storage backend.
const errWorkspaceWritesUnavailable = "Workspace writes are not available in this deployment configuration. Configure durable workspace storage (NFS) to enable file editing."

// maxUploadTotalSize is the maximum total request body size for file uploads (100MB).
const maxUploadTotalSize = 100 * 1024 * 1024

// maxUploadFileSize is the maximum size for a single uploaded file (50MB).
const maxUploadFileSize = 50 * 1024 * 1024

// maxEditableFileSize is the maximum file size the editor will serve for inline editing (1MB).
const maxEditableFileSize = 1 * 1024 * 1024

// maxPreviewFileSize is the maximum file size for read-only preview (50MB).
const maxPreviewFileSize = 50 * 1024 * 1024

// isCloudRunEnv checks K_SERVICE to determine if we're running on Cloud Run.
// In production, K_SERVICE is set once at container startup and never changes,
// so calling os.Getenv is effectively a cached lookup (the C library caches
// the environment block). This avoids the complexity of sync.Once while
// remaining test-friendly via t.Setenv.
func isCloudRunEnv() bool {
	return os.Getenv("K_SERVICE") != ""
}

// isCloudRunInstance reports whether the hub is running on a Cloud Run Instance.
// CLOUD_RUN_INSTANCE is set by the platform on Instances but NOT on Cloud Run
// Services (which use K_SERVICE instead). See design doc section 4.6.
func isCloudRunInstance() bool {
	return os.Getenv("CLOUD_RUN_INSTANCE") != ""
}

// workspaceWriteBlocked returns true when workspace writes should be rejected
// with 503 Service Unavailable. This happens when the hub is running on Cloud
// Run (K_SERVICE is set) and no durable storage backend is configured.
//
// Uses an allowlist of known-durable backends rather than a blocklist, so that
// an unrecognized backend value fails closed (blocked) rather than silently
// writing to ephemeral storage.
func (s *Server) workspaceWriteBlocked() bool {
	// Not on Cloud Run (K_SERVICE) and not on a Cloud Run Instance
	// → writes are fine (self-hosted with local disk)
	if !isCloudRunEnv() && !isCloudRunInstance() {
		return false
	}

	// Cloud Run Instance (single-node hosted tier, Tier 0): writes are
	// permitted to ephemeral storage. This is a deliberate, documented
	// decision -- the tier is ephemeral by design (workspaces are lost on
	// redeploy) and the UI banner (below) makes this visible to users.
	// Without this explicit check, writes happen to work because K_SERVICE
	// is not set on Instances, but that is an accident that would break the
	// first time someone makes isCloudRunEnv() aware of Instances.
	// See design doc section 5.2 and section 4.6.
	if isCloudRunInstance() {
		return false
	}

	// On Cloud Run Services — only allow writes if a known durable backend
	// is configured
	wsCfg := s.config.WorkspaceStorageConfig
	if wsCfg != nil {
		switch wsCfg.Backend {
		case "nfs", "cloudrun-volume", "gke-shared-volume":
			return false // Known durable backend → writes allowed
		}
	}
	// No config, empty backend, "local", or unrecognized → block writes
	return true
}

// ProjectWorkspaceFile represents a file in a project workspace.
type ProjectWorkspaceFile struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	Mode    string    `json:"mode"`
}

// ProjectWorkspaceListResponse is the response for listing project workspace files.
type ProjectWorkspaceListResponse struct {
	Files      []ProjectWorkspaceFile `json:"files"`
	TotalSize  int64                  `json:"totalSize"`
	TotalCount int                    `json:"totalCount"`
	HasMore    bool                   `json:"hasMore,omitempty"`
}

// SharedDirListResponse extends the workspace list response with provider metadata.
type SharedDirListResponse struct {
	Files         []ProjectWorkspaceFile `json:"files"`
	TotalSize     int64                  `json:"totalSize"`
	TotalCount    int                    `json:"totalCount"`
	HasMore       bool                   `json:"hasMore,omitempty"`
	ProviderCount int                    `json:"providerCount,omitempty"`
}

// FileSearchResult holds the result of a workspace file search.
type FileSearchResult struct {
	Files      []ProjectWorkspaceFile
	TotalSize  int64
	TotalCount int
	HasMore    bool
}

// FileSearcher searches a workspace directory for files matching an optional query.
// The interface exists so that an indexed implementation can be swapped in later
// without touching the HTTP handlers.
//
// The directory is passed as an *os.Root, not a path, so that the walk is
// confined to that directory: a symlink inside it that points elsewhere on the
// host cannot be walked through, and no name the walk produces can be resolved
// outside the root.
type FileSearcher interface {
	Search(root *os.Root, query string, limit int) (FileSearchResult, error)
}

// defaultFileSearcher is the package-level FileSearcher used by the handlers.
var defaultFileSearcher FileSearcher = walkDirSearcher{}

// walkDirSearcher implements FileSearcher by walking root.FS().
//
// fs.WalkDir reports a symlink as a leaf entry (fs.ModeSymlink, never IsDir)
// and does not descend into it, and the entry's Info comes from an lstat of
// the link itself — so a symlinked directory is listed but not followed, and a
// symlinked file is reported with the link's own metadata rather than its
// target's. Listing therefore describes the tree as it actually is, without
// reading anything through a link.
type walkDirSearcher struct{}

func (walkDirSearcher) Search(root *os.Root, query string, limit int) (FileSearchResult, error) {
	var matcher func(string) bool
	if query != "" {
		if re, err := regexp.Compile("(?i)" + query); err == nil {
			matcher = re.MatchString
		} else {
			matcher = fuzzyMatch(query)
		}
	}

	var allFiles []ProjectWorkspaceFile
	var totalSize int64

	err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single entry that can't be read (a directory the hub lacks
			// permission on, a name os.Root refuses to resolve) must not fail
			// the whole listing — skip it and keep walking.
			return nil
		}
		if path == "." || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		totalSize += info.Size()
		if matcher == nil || matcher(path) {
			allFiles = append(allFiles, ProjectWorkspaceFile{
				Path:    path,
				Size:    info.Size(),
				ModTime: info.ModTime(),
				Mode:    info.Mode().String(),
			})
		}
		return nil
	})

	if err != nil {
		return FileSearchResult{}, err
	}

	if allFiles == nil {
		allFiles = []ProjectWorkspaceFile{}
	}

	// Sort by modTime descending (most recently modified first).
	sort.Slice(allFiles, func(i, j int) bool {
		return allFiles[i].ModTime.After(allFiles[j].ModTime)
	})

	hasMore := len(allFiles) > limit
	files := allFiles
	if hasMore {
		files = allFiles[:limit]
	}

	return FileSearchResult{
		Files:      files,
		TotalSize:  totalSize,
		TotalCount: len(allFiles),
		HasMore:    hasMore,
	}, nil
}

// fuzzyMatch returns a matcher that checks whether every character in pattern
// appears in order (case-insensitive) in the candidate string.
func fuzzyMatch(pattern string) func(string) bool {
	lower := strings.ToLower(pattern)
	return func(s string) bool {
		haystack := strings.ToLower(s)
		si := 0
		for _, ch := range lower {
			idx := strings.IndexRune(haystack[si:], ch)
			if idx == -1 {
				return false
			}
			si += idx + utf8.RuneLen(ch)
		}
		return true
	}
}

// intQueryParam parses an integer query parameter, applying a default and a cap.
func intQueryParam(r *http.Request, name string, defaultVal, maxVal int) int {
	s := r.URL.Query().Get(name)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return defaultVal
	}
	if v > maxVal {
		return maxVal
	}
	return v
}

// ProjectWorkspaceUploadResponse is the response for uploading files to a project workspace.
type ProjectWorkspaceUploadResponse struct {
	Files []ProjectWorkspaceFile `json:"files"`
}

// openConfinedBase opens base as an *os.Root, through which every subsequent
// file operation on user-supplied sub-paths is performed.
//
// The point of the Root is that its confinement is enforced by the kernel at
// each path component, not by inspecting the string beforehand: a name that
// would resolve outside base — including one that gets there by traversing a
// symlink stored inside base — is refused. The contents of these directories
// are attacker-controlled by design (a workspace is a git checkout, a shared
// dir is mounted read-write into every agent in the project), so a symlink
// appearing inside one is an expected condition rather than an anomaly, and
// the lexical validateWorkspaceFilePath check cannot see it at all.
//
// Symlinks that resolve to a location still inside base are followed, which is
// the ordinary behavior a file browser should have: they stay within the same
// project's tree.
//
// base itself is deliberately treated differently from the paths below it.
// os.OpenRoot follows symlinks in the path it is handed, so rooting at a base
// whose own final component is a symlink would confine everything to that
// link's target — wherever an attacker pointed it — which is not confinement
// at all. That case is refused here. Intermediate components of base are not
// checked: they are hub-owned (~/.scion/..., or a configured storage mount)
// and a deployment whose data directory legitimately sits behind a symlinked
// parent, such as a macOS /var, must keep working.
//
// createIfMissing creates base when it does not exist, preserving the
// pre-existing behavior in which a first upload or write materializes a
// workspace or shared directory. Read and delete verbs pass false and get back
// an fs.ErrNotExist the caller renders as an empty listing or a 404, so that
// browsing a project can no longer bring directories into being as a side
// effect.
func openConfinedBase(base string, createIfMissing bool) (*os.Root, error) {
	fi, err := os.Lstat(base)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing to serve %q: base path is a symlink", base)
		}
	case os.IsNotExist(err):
		if !createIfMissing {
			return nil, err
		}
		if mkErr := os.MkdirAll(base, 0755); mkErr != nil {
			return nil, mkErr
		}
	default:
		return nil, err
	}

	return os.OpenRoot(base)
}

// notAccessible responds to a path that os.Root refused to resolve.
//
// os.Root reports a refused symlink escape as a *fs.PathError that is
// deliberately not fs.ErrNotExist, so it never reaches the not-found branches
// above this call. Answering "not accessible" rather than "not found" avoids
// turning the handler into an oracle for whether the symlink's target exists
// on the host, and the warning log gives operators a signal that something in
// the tree is pointing out of it.
func notAccessible(w http.ResponseWriter, what string, path string, err error) {
	slog.Warn("refused a workspace path that does not resolve inside its base",
		"kind", what, "path", path, "error", err)
	BadRequest(w, "File not accessible")
}

// handleProjectWorkspace dispatches project workspace file operations.
// Routes:
//   - GET  (filePath="")  → list files
//   - POST (filePath="")  → upload files
//   - DELETE (filePath!="") → delete file
func (s *Server) handleProjectWorkspace(w http.ResponseWriter, r *http.Request, projectID, filePath string) {
	ctx := r.Context()

	// Look up the project
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve workspace path — supports hub-managed, shared-workspace, and linked projects
	workspacePath, err := s.resolveProjectWebDAVPath(ctx, project)
	if err != nil {
		Conflict(w, err.Error())
		return
	}

	// Every verb below operates through this Root and never on a path joined
	// onto workspacePath. Uploads and writes may still materialize the
	// workspace on first use, as they always have; reads and deletes no
	// longer do.
	createIfMissing := r.Method == http.MethodPost || r.Method == http.MethodPut
	root, err := openConfinedBase(workspacePath, createIfMissing)
	if err != nil {
		if os.IsNotExist(err) {
			if r.Method == http.MethodGet && filePath == "" {
				writeJSON(w, http.StatusOK, ProjectWorkspaceListResponse{Files: []ProjectWorkspaceFile{}})
				return
			}
			NotFound(w, "File")
			return
		}
		slog.ErrorContext(ctx, "failed to open project workspace",
			"project_id", projectID, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	switch {
	case r.Method == http.MethodGet && filePath == "":
		s.handleProjectWorkspaceList(w, r, root)
	case r.Method == http.MethodGet && filePath != "":
		s.handleProjectWorkspaceDownload(w, r, root, filePath)
	case r.Method == http.MethodPost && filePath == "":
		s.handleProjectWorkspaceUpload(w, r, root)
	case r.Method == http.MethodPut && filePath != "":
		s.handleProjectWorkspaceWrite(w, r, root, filePath)
	case r.Method == http.MethodDelete && filePath != "":
		s.handleProjectWorkspaceDelete(w, root, filePath)
	default:
		MethodNotAllowed(w)
	}
}

// handleProjectWorkspaceList lists files in a project workspace.
// Accepts optional query parameters:
//   - q:     filter pattern (regex or fuzzy fallback, case-insensitive)
//   - limit: max results (default 500, cap 2000)
func (s *Server) handleProjectWorkspaceList(w http.ResponseWriter, r *http.Request, root *os.Root) {
	query := r.URL.Query().Get("q")
	limit := intQueryParam(r, "limit", 500, 2000)

	result, err := defaultFileSearcher.Search(root, query, limit)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, ProjectWorkspaceListResponse(result))
}

// handleSharedDirFileList lists files in a shared directory, adding provider metadata
// to the response so the frontend can show multi-broker warnings.
// Accepts the same optional query parameters as handleProjectWorkspaceList (q, limit).
func (s *Server) handleSharedDirFileList(w http.ResponseWriter, r *http.Request, root *os.Root, res *sharedDirResolution) {
	query := r.URL.Query().Get("q")
	limit := intQueryParam(r, "limit", 500, 2000)

	result, err := defaultFileSearcher.Search(root, query, limit)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, SharedDirListResponse{
		Files:         result.Files,
		TotalSize:     result.TotalSize,
		TotalCount:    result.TotalCount,
		HasMore:       result.HasMore,
		ProviderCount: res.ProviderCount,
	})
}

// handleProjectWorkspaceUpload handles file uploads to a project workspace.
func (s *Server) handleProjectWorkspaceUpload(w http.ResponseWriter, r *http.Request, root *os.Root) {
	// Phase 0 safety gate: reject writes on Cloud Run without durable storage
	if s.workspaceWriteBlocked() {
		writeError(w, http.StatusServiceUnavailable, "workspace_writes_unavailable", errWorkspaceWritesUnavailable, nil)
		return
	}

	// Apply total request body size limit
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadTotalSize)

	// Parse multipart form
	if err := r.ParseMultipartForm(maxUploadTotalSize); err != nil {
		if err.Error() == "http: request body too large" {
			BadRequest(w, "Request body exceeds 100MB limit")
			return
		}
		BadRequest(w, "Invalid multipart form: "+err.Error())
		return
	}

	if r.MultipartForm == nil || len(r.MultipartForm.File) == 0 {
		ValidationError(w, "No files provided", nil)
		return
	}

	var uploaded []ProjectWorkspaceFile

	for fieldName, fileHeaders := range r.MultipartForm.File {
		for _, fh := range fileHeaders {
			// The field name is the relative file path
			relPath := fieldName

			// Validate the file path
			if err := validateWorkspaceFilePath(relPath); err != nil {
				BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", relPath, err.Error()))
				return
			}

			// Check per-file size limit
			if fh.Size > maxUploadFileSize {
				BadRequest(w, fmt.Sprintf("File %q exceeds 50MB limit", relPath))
				return
			}

			// Open the uploaded file
			src, err := fh.Open()
			if err != nil {
				InternalError(w)
				return
			}

			// Create parent directories, confined to the root: a component
			// that is, or leads through, a symlink out of the workspace is
			// refused by root.MkdirAll rather than followed.
			if dir := filepath.Dir(relPath); dir != "." {
				if err := root.MkdirAll(dir, 0755); err != nil {
					_ = src.Close()
					notAccessible(w, "upload-mkdir", relPath, err)
					return
				}
			}

			// Write file to disk
			dst, err := root.Create(relPath)
			if err != nil {
				_ = src.Close()
				notAccessible(w, "upload", relPath, err)
				return
			}

			written, err := io.Copy(dst, src)
			_ = src.Close()
			_ = dst.Close()

			if err != nil {
				InternalError(w)
				return
			}

			// Get file info for response
			info, err := root.Stat(relPath)
			if err != nil {
				InternalError(w)
				return
			}

			uploaded = append(uploaded, ProjectWorkspaceFile{
				Path:    relPath,
				Size:    written,
				ModTime: info.ModTime(),
				Mode:    info.Mode().String(),
			})
		}
	}

	writeJSON(w, http.StatusOK, ProjectWorkspaceUploadResponse{
		Files: uploaded,
	})
}

// handleProjectWorkspaceDownload serves a single file from a project workspace.
// When the query parameter "view=true" is set, the file is served inline for
// in-browser preview; otherwise the response forces a download.
// When "format=json" is set, the file content is returned as a JSON object
// with metadata, suitable for the inline file editor.
func (s *Server) handleProjectWorkspaceDownload(w http.ResponseWriter, r *http.Request, root *os.Root, filePath string) {
	// Validate the file path. This lexical check is kept as defence in depth:
	// it rejects the obvious shapes early, but it cannot see symlinks, so the
	// confinement that actually matters is the Root below.
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	// Check file exists and is not a directory
	info, err := root.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "File")
			return
		}
		notAccessible(w, "download", filePath, err)
		return
	}
	if info.IsDir() {
		BadRequest(w, "Cannot download a directory")
		return
	}

	// JSON format: return content wrapped with metadata for the editor
	if r.URL.Query().Get("format") == "json" {
		mode := r.URL.Query().Get("mode")
		sizeLimit := maxEditableFileSize
		sizeLimitLabel := "1MB"
		if mode == "preview" {
			sizeLimit = maxPreviewFileSize
			sizeLimitLabel = "50MB"
		}
		action := "editing"
		if mode == "preview" {
			action = "preview"
		}
		if info.Size() > int64(sizeLimit) {
			BadRequest(w, fmt.Sprintf("File too large for %s (%s). Maximum is %s.",
				action, formatByteSize(info.Size()), sizeLimitLabel))
			return
		}

		data, readErr := root.ReadFile(filePath)
		if readErr != nil {
			InternalError(w)
			return
		}

		// Verify content is valid UTF-8 text
		if !utf8.Valid(data) {
			actionPast := "edited"
			if mode == "preview" {
				actionPast = "previewed"
			}
			BadRequest(w, fmt.Sprintf("File contains binary content and cannot be %s", actionPast))
			return
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"path":     filePath,
			"content":  string(data),
			"size":     info.Size(),
			"modTime":  info.ModTime(),
			"encoding": "utf-8",
		})
		return
	}

	// Open the file
	f, err := root.Open(filePath)
	if err != nil {
		InternalError(w)
		return
	}
	defer func() { _ = f.Close() }()

	// Determine content type from extension, default to octet-stream
	fileName := filepath.Base(filePath)
	contentType := mime.TypeByExtension(filepath.Ext(fileName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disposition := "attachment"
	if r.URL.Query().Get("view") == "true" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, fileName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))

	_, _ = io.Copy(w, f)
}

// writeDirectoryToZip walks root and writes all regular files into the given
// zip.Writer, preserving directory structure relative to the root.
//
// Symlinks are skipped outright rather than followed. This is the one verb
// where following an in-root symlink would still be wrong even though os.Root
// permits it: the archive is handed to the caller, so a link resolving to
// another file in the tree would silently duplicate that file's contents under
// a second name, and a link resolving outside the tree — which os.Root refuses
// to open at all — would otherwise have produced a read error mid-stream,
// after the zip had already started and the status code was long gone. Leaving
// links out keeps the archive to what it can faithfully represent.
func writeDirectoryToZip(zw *zip.Writer, root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable entry should not truncate the whole archive.
			return nil
		}

		if path == "." || d.IsDir() {
			return nil
		}

		// Skip anything that is not a regular file: symlinks (see above),
		// and also sockets, fifos and devices, which have no meaningful zip
		// representation and could block forever on read.
		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		// Use the relative path so directory structure is preserved
		header.Name = path
		header.Method = zip.Deflate

		// Open before writing the header, so that a file we turn out not to
		// be able to read does not leave a stray empty entry in the archive.
		f, err := root.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}

		_, err = io.Copy(writer, f)
		return err
	})
}

// handleProjectWorkspaceArchive creates a zip archive of the entire workspace and serves it for download.
func (s *Server) handleProjectWorkspaceArchive(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Look up the project
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve workspace path — supports hub-managed, shared-workspace, and linked projects
	workspacePath, err := s.resolveProjectWebDAVPath(ctx, project)
	if err != nil {
		Conflict(w, err.Error())
		return
	}

	// Archiving never creates the workspace — an archive of nothing is just
	// "not found".
	root, err := openConfinedBase(workspacePath, false)
	if err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "Workspace")
			return
		}
		slog.ErrorContext(ctx, "failed to open project workspace for archive",
			"project_id", projectID, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	archiveName := project.Slug + "-workspace.zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	if err := writeDirectoryToZip(zw, root); err != nil {
		// At this point we've already started writing, so we can't send an error response.
		// The zip will be truncated/corrupt, which the client will notice.
		slog.WarnContext(ctx, "failed to complete workspace archive", "project_id", projectID, "error", err)
		return
	}
}

// handleProjectSharedDirArchive creates a zip archive of a shared directory and serves it for download.
func (s *Server) handleProjectSharedDirArchive(w http.ResponseWriter, r *http.Request, projectID, dirName string) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify the shared dir is declared on this project
	found := false
	for _, d := range project.SharedDirs {
		if d.Name == dirName {
			found = true
			break
		}
	}
	if !found {
		NotFound(w, "Shared directory")
		return
	}

	resolution, resolveErr := s.resolveSharedDirPath(ctx, project, dirName)
	if resolveErr != nil {
		var nfsErr *sharedDirStorageResolveError
		if errors.As(resolveErr, &nfsErr) {
			// This error can carry the resolved host path or a settings
			// parse error. Log it server-side and return a fixed message.
			slog.ErrorContext(ctx, "failed to resolve shared directory path for archive",
				"project_id", projectID, "dir", dirName, "error", resolveErr)
			Conflict(w, sharedDirStorageUnavailableMessage)
			return
		}
		Conflict(w, resolveErr.Error())
		return
	}
	archiveName := project.Slug + "-" + dirName + ".zip"

	// As above: archiving never creates the shared directory.
	root, err := s.openSharedDirRoot(resolution, false)
	if err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "Shared directory")
			return
		}
		slog.ErrorContext(ctx, "failed to open shared directory for archive",
			"project_id", projectID, "dir", dirName, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	if err := writeDirectoryToZip(zw, root); err != nil {
		slog.WarnContext(ctx, "failed to complete shared dir archive", "project_id", projectID, "dir", dirName, "error", err)
		return
	}
}

// ProjectWorkspaceWriteRequest is the request body for writing file content.
type ProjectWorkspaceWriteRequest struct {
	Content         string `json:"content"`
	ExpectedModTime string `json:"expectedModTime,omitempty"` // optional optimistic concurrency
}

// handleProjectWorkspaceWrite writes (creates or overwrites) a file in a project workspace.
// The content is provided as a JSON request body. If expectedModTime is set, the
// server checks that the file has not been modified since that time and returns
// 409 Conflict if it has.
func (s *Server) handleProjectWorkspaceWrite(w http.ResponseWriter, r *http.Request, root *os.Root, filePath string) {
	// Phase 0 safety gate: reject writes on Cloud Run without durable storage
	if s.workspaceWriteBlocked() {
		writeError(w, http.StatusServiceUnavailable, "workspace_writes_unavailable", errWorkspaceWritesUnavailable, nil)
		return
	}

	// Validate the file path
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	var req ProjectWorkspaceWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Optimistic concurrency check: if expectedModTime is set, verify the file
	// has not been modified since the client loaded it.
	if req.ExpectedModTime != "" {
		expectedTime, parseErr := time.Parse(time.RFC3339Nano, req.ExpectedModTime)
		if parseErr != nil {
			BadRequest(w, "Invalid expectedModTime format — use RFC3339")
			return
		}

		info, statErr := root.Stat(filePath)
		if statErr == nil {
			// File exists — check mod time. Allow a 1-second tolerance for
			// filesystem timestamp granularity.
			if info.ModTime().Sub(expectedTime) > time.Second {
				Conflict(w, "File has been modified since you loaded it. Reload and try again.")
				return
			}
		}
		// If file doesn't exist (new file creation), skip the check
	}

	// Create parent directories if needed, confined to the root.
	if dir := filepath.Dir(filePath); dir != "." {
		if err := root.MkdirAll(dir, 0755); err != nil {
			notAccessible(w, "write-mkdir", filePath, err)
			return
		}
	}

	// Write the file
	if err := root.WriteFile(filePath, []byte(req.Content), 0644); err != nil {
		notAccessible(w, "write", filePath, err)
		return
	}

	// Read back file info for the response
	info, err := root.Stat(filePath)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, ProjectWorkspaceFile{
		Path:    filePath,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    info.Mode().String(),
	})
}

// handleProjectWorkspaceDelete deletes a file from a project workspace.
//
// Existence is checked with Lstat and the removal is a Root.Remove, both of
// which act on the named entry itself rather than on what it points to. A
// symlink is therefore deletable — it shows up in the listing, so refusing to
// delete it would strand it — and deleting it unlinks the link and never the
// file at the other end, inside or outside the workspace.
func (s *Server) handleProjectWorkspaceDelete(w http.ResponseWriter, root *os.Root, filePath string) {
	// Phase 0 safety gate: reject writes on Cloud Run without durable storage
	if s.workspaceWriteBlocked() {
		writeError(w, http.StatusServiceUnavailable, "workspace_writes_unavailable", errWorkspaceWritesUnavailable, nil)
		return
	}

	// Validate the file path
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	// Check the entry exists. Lstat, not Stat: a dangling or outward-pointing
	// symlink still exists as an entry and is still removable.
	if _, err := root.Lstat(filePath); err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "File")
			return
		}
		notAccessible(w, "delete", filePath, err)
		return
	}

	// Remove the file
	if err := root.Remove(filePath); err != nil {
		notAccessible(w, "delete", filePath, err)
		return
	}

	// Clean up empty parent directories
	cleanEmptyDirs(root, filepath.Dir(filePath))

	w.WriteHeader(http.StatusNoContent)
}

// handleSharedDirFiles dispatches shared directory file operations.
// Routes:
//   - GET  (filePath="")  → list files
//   - POST (filePath="")  → upload files
//   - GET  (filePath!="") → download file
//   - DELETE (filePath!="") → delete file
func (s *Server) handleSharedDirFiles(w http.ResponseWriter, r *http.Request, projectID, dirName, filePath string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify the shared dir is declared on this project
	found := false
	for _, d := range project.SharedDirs {
		if d.Name == dirName {
			found = true
			break
		}
	}
	if !found {
		NotFound(w, "Shared directory")
		return
	}

	// Resolve shared dir host path based on project type
	resolution, resolveErr := s.resolveSharedDirPath(ctx, project, dirName)
	if resolveErr != nil {
		var nfsErr *sharedDirStorageResolveError
		if errors.As(resolveErr, &nfsErr) {
			// Don't hand the client the resolved host path or a settings
			// parse error; log it server-side instead.
			slog.ErrorContext(ctx, "failed to resolve shared directory path",
				"project_id", projectID, "dir", dirName, "error", resolveErr)
			Conflict(w, sharedDirStorageUnavailableMessage)
			return
		}
		Conflict(w, resolveErr.Error())
		return
	}
	// The unconditional os.MkdirAll that used to stand here is gone. Browsing
	// a project should not bring directories into existence as a side effect,
	// and creating one on a GET meant the hub reached out along a path whose
	// components it had not established were safe to follow. An upload or a
	// write still creates the directory on first use, which is the case that
	// needed it; a list of a directory that does not exist yet is an empty
	// list, and a read or delete in one is a 404.
	createIfMissing := r.Method == http.MethodPost || r.Method == http.MethodPut

	root, err := s.openSharedDirRoot(resolution, createIfMissing)
	if err != nil {
		if os.IsNotExist(err) {
			if r.Method == http.MethodGet && filePath == "" {
				writeJSON(w, http.StatusOK, SharedDirListResponse{
					Files:         []ProjectWorkspaceFile{},
					ProviderCount: resolution.ProviderCount,
				})
				return
			}
			NotFound(w, "Shared directory")
			return
		}
		slog.ErrorContext(ctx, "failed to open shared directory",
			"project_id", projectID, "dir", dirName, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	switch {
	case r.Method == http.MethodGet && filePath == "":
		s.handleSharedDirFileList(w, r, root, resolution)
	case r.Method == http.MethodGet && filePath != "":
		s.handleProjectWorkspaceDownload(w, r, root, filePath)
	case r.Method == http.MethodPost && filePath == "":
		s.handleProjectWorkspaceUpload(w, r, root)
	case r.Method == http.MethodPut && filePath != "":
		s.handleProjectWorkspaceWrite(w, r, root, filePath)
	case r.Method == http.MethodDelete && filePath != "":
		s.handleProjectWorkspaceDelete(w, root, filePath)
	default:
		MethodNotAllowed(w)
	}
}

// sharedDirResolution holds the resolved path and metadata for shared dir browsing.
type sharedDirResolution struct {
	Path          string
	ProviderCount int  // total project providers (for multi-broker warning)
	IsLocal       bool // true when resolved via co-located broker
	// Backend is "nfs" when this resolution came from
	// resolveNFSSharedDirPath; empty otherwise. Backend is consulted at
	// openSharedDirRoot, to choose how the leaf is opened, and at
	// ensureNFSSharedDirLeaf, for how a missing leaf gets created.
	Backend string
	// NFSHostBase and NFSRelPath are populated only when Backend == "nfs":
	// the resolved (symlink-free) host base, and Path's path relative to
	// it. They let a caller (re)create the leaf via shareddirs.EnsureLeaf,
	// the same confined entry point the broker itself uses, without ever
	// trusting Path's own on-disk state for that decision.
	NFSHostBase string
	NFSRelPath  string
	// NFSSubPathRoot is the resolved server.shared_dir_storage.nfs.subpath_root
	// value (defaulted to "projects" if unset), populated only when
	// Backend == "nfs". Kept separate from NFSRelPath (which already
	// includes it as a prefix) because shareddirs.DeleteSharedDir needs
	// subPathRoot and the project ID as distinct path components, not the
	// single already-joined relative path EnsureLeaf/OpenAnchoredRoot want.
	NFSSubPathRoot string
}

// ensureNFSSharedDirLeaf creates resolution's NFS leaf via
// shareddirs.EnsureLeaf, applying the leaf's modes/ACL hardening through
// EnsureLeaf's no-follow component walk. A no-op (by EnsureLeaf's own alreadyExisted
// contract) if the leaf already exists.
func (s *Server) ensureNFSSharedDirLeaf(resolution *sharedDirResolution) error {
	leafFd, _, err := shareddirs.EnsureLeaf(resolution.NFSHostBase, resolution.NFSRelPath)
	if err != nil {
		return fmt.Errorf("create shared dir: %w", err)
	}
	return shareddirs.CloseFd(leafFd)
}

// openSharedDirRoot opens resolution.Path as an *os.Root, choosing the
// confinement mechanism by backend. NFS-backed shared dirs are opened via
// OpenAnchoredRoot, which re-verifies the leaf's identity (device + inode)
// against an independent walk immediately before opening it; other backends
// use openConfinedBase.
//
// For NFS, createIfMissing=true creates the leaf first via
// ensureNFSSharedDirLeaf (shareddirs.EnsureLeaf's own O_NOFOLLOW walk), and
// OpenAnchoredRoot is then called with false.
func (s *Server) openSharedDirRoot(resolution *sharedDirResolution, createIfMissing bool) (*os.Root, error) {
	if resolution.Backend != "nfs" {
		return openConfinedBase(resolution.Path, createIfMissing)
	}
	if createIfMissing {
		if err := s.ensureNFSSharedDirLeaf(resolution); err != nil {
			// Not an fs.ErrNotExist-compatible error, so the caller's own
			// os.IsNotExist branch won't fire for this -- it logs and
			// returns InternalError on any other error, which covers this.
			return nil, err
		}
	}
	return shareddirs.OpenAnchoredRoot(resolution.NFSHostBase, resolution.NFSRelPath)
}

// resolveSharedDirPath resolves the host-side path for a shared directory.
// Shared dirs always live under ~/.scion/project-configs/<slug>__<uuid>/shared-dirs/<name>,
// matching the path used by agent provisioning (config.GetSharedDirPath).
// For git-based projects with a co-located broker that has a LocalPath, the path is
// resolved via config.GetSharedDirPath(localPath, dirName). Otherwise, the path is
// resolved via the .scion marker in the hub-managed workspace directory.
func (s *Server) resolveSharedDirPath(ctx context.Context, project *store.Project, dirName string) (*sharedDirResolution, error) {
	// Phase 2 item 4 (design deploy-config-explore §3.2.5): if the hub's
	// own global settings carry server.shared_dir_storage: nfs, every
	// project's shared dirs live on the NFS export, keyed by hub project
	// ID — independent of whether this project has a co-located broker at
	// all, and checked FIRST, before any of the local-layout resolution
	// below. resolveNFSSharedDirPath's second return value tells us
	// whether this branch even applies (nfs is configured); when it
	// doesn't, fall through unchanged to today's local-layout resolution.
	if resolution, applicable, err := s.resolveNFSSharedDirPath(dirName, project.ID); applicable {
		if err != nil {
			// Every error resolveNFSSharedDirPath returns is wrapped here,
			// in the one place both of its callers pass through, marking it
			// as an NFS resolution failure: its message may contain a
			// resolved host path or a settings parse error, and must not
			// reach the client directly.
			err = &sharedDirStorageResolveError{err: err}
		}
		return resolution, err
	}

	if project.GitRemote == "" {
		// Hub-managed project: resolve via the .scion marker in the workspace directory
		// to find the project-configs path where shared dirs actually live.
		sdPath, err := resolveHubProjectSharedDirPath(project.Slug, dirName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve shared directory path: %w", err)
		}
		return &sharedDirResolution{
			Path:    sdPath,
			IsLocal: true,
		}, nil
	}

	// Git-based project: find the co-located broker's local path for this project
	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		slog.Warn("failed to get project providers for shared dir browsing", "project_id", project.ID, "error", err)
		return nil, fmt.Errorf("failed to resolve project providers")
	}

	providerCount := len(providers)
	embeddedIsProvider := false

	// Find a provider on the embedded (co-located) broker
	for _, p := range providers {
		if s.isEmbeddedBroker(p.BrokerID) {
			embeddedIsProvider = true
			if p.LocalPath != "" {
				sdPath, err := config.GetSharedDirPath(p.LocalPath, dirName)
				if err != nil {
					return nil, fmt.Errorf("failed to resolve shared directory path")
				}
				return &sharedDirResolution{
					Path:          sdPath,
					ProviderCount: providerCount,
					IsLocal:       true,
				}, nil
			}
		}
	}

	// Fallback: embedded broker is a provider but has no LocalPath recorded
	// (e.g. auto-linked or shared-workspace project). Resolve via the .scion marker
	// in the hub workspace to find the project-configs path.
	if embeddedIsProvider {
		sdPath, err := resolveHubProjectSharedDirPath(project.Slug, dirName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve shared directory path: %w", err)
		}
		return &sharedDirResolution{
			Path:          sdPath,
			ProviderCount: providerCount,
			IsLocal:       true,
		}, nil
	}

	return nil, fmt.Errorf("shared directory file browsing requires a co-located runtime broker")
}

// sharedDirStorageUnavailableMessage is the fixed, detail-free message
// returned to the client when resolveSharedDirPath fails because
// server.shared_dir_storage=nfs resolution itself failed. The underlying
// error can carry the resolved host path or a settings parse error, neither
// of which belongs in an HTTP response; the detail goes to the server log
// instead.
const sharedDirStorageUnavailableMessage = "shared directory storage is misconfigured or unavailable; see hub logs"

// sharedDirStorageResolveError marks an error as having come from the
// server.shared_dir_storage=nfs resolution path (resolveNFSSharedDirPath).
type sharedDirStorageResolveError struct{ err error }

func (e *sharedDirStorageResolveError) Error() string { return e.err.Error() }
func (e *sharedDirStorageResolveError) Unwrap() error { return e.err }

// resolveNFSSharedDirPath resolves dirName's path for projectID via the
// same confined resolver used by agent Start (pkg/shareddirs,
// runtime.NewNFSBackend), when the hub's own global settings carry
// server.shared_dir_storage: nfs (design deploy-config-explore §3.2.5,
// Phase 2 item 4).
//
// The second return value, applicable, is false whenever this branch does
// not apply at all — the global settings failed to load, no
// shared_dir_storage block is configured, or its backend is "" / "local" —
// in which case resolveSharedDirPath must fall through unchanged to the
// pre-existing local-layout resolution (byte-identical to today). When
// applicable is true, the caller must return (resolution, err) directly:
// backend "nfs" is configured, so the local-layout resolution below does
// NOT apply, whether this call succeeded or failed.
//
// Settings are read via config.LoadGlobalSettings() — the same env-free,
// global-only loader the broker's Start path uses — so a project's own
// settings.yaml (including in-repo content from a cloned git project) can
// never influence where the hub looks for its shared dirs, exactly like
// the creation-time resolver.
func (s *Server) resolveNFSSharedDirPath(dirName, projectID string) (resolution *sharedDirResolution, applicable bool, err error) {
	// A broken or legacy-format global settings file that plausibly
	// configured shared_dir_storage fails closed here, matching the
	// broker's own Start path (pkg/agent/run.go), rather than resolving
	// successfully for what the operator intended to be an NFS-backed
	// project.
	globalSettings, _, gErr := config.LoadGlobalSettings()
	if gErr != nil {
		if config.GlobalSettingsMentions("shared_dir_storage") {
			return nil, true, fmt.Errorf("server.shared_dir_storage: global settings unreadable: %w", gErr)
		}
		return nil, false, nil
	}
	if globalSettings.Server == nil || globalSettings.Server.SharedDirStorage == nil {
		// A file that loaded successfully as v1 is authoritative: no block
		// means genuinely unset. Only a LEGACY-format file that mentions the
		// key can silently drop it, gated by GlobalSettingsIsLegacyFormat.
		if config.GlobalSettingsIsLegacyFormat() && config.GlobalSettingsMentions("shared_dir_storage") {
			return nil, true, fmt.Errorf(
				"server.shared_dir_storage: global settings mention it but it was not loaded (missing schema_version \"1\"?)")
		}
		return nil, false, nil
	}
	sdCfg := globalSettings.Server.SharedDirStorage
	if sdCfg.Backend != "nfs" {
		return nil, false, nil
	}

	// From here on, nfs IS the configured backend: every error below is
	// returned to the caller directly, never treated as "not applicable".
	if err := sdCfg.Validate(); err != nil {
		return nil, true, fmt.Errorf("server.shared_dir_storage: %w", err)
	}
	// Same validation as the creation-time resolver: dirName and projectID
	// are both path segments in the NFS layout and are checked before any
	// path is constructed from them.
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: dirName}}); err != nil {
		return nil, true, fmt.Errorf("server.shared_dir_storage: %w", err)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return nil, true, fmt.Errorf("server.shared_dir_storage: invalid hub project ID %q", projectID)
	}

	res, resolveErr := runtime.NewNFSBackend(sdCfg.NFS).Resolve(runtime.ResolveInput{
		ProjectID:      projectID,
		SharedDirNames: []string{dirName},
	})
	if resolveErr != nil {
		return nil, true, fmt.Errorf("server.shared_dir_storage: resolve: %w", resolveErr)
	}
	sd, ok := res.SharedDirs[dirName]
	if !ok {
		return nil, true, fmt.Errorf("server.shared_dir_storage: shared dir %q not found in NFS resolution", dirName)
	}

	subPathRoot := sdCfg.NFS.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}
	// Defense in depth alongside the name/ID validation above, exactly as
	// resolveSharedDirs does at creation time.
	if err := shareddirs.ConfineLeaf(sd.HostPath, res.HostBase, subPathRoot, projectID, dirName); err != nil {
		return nil, true, fmt.Errorf("server.shared_dir_storage: %w", err)
	}

	// Symlink-escape backstop on the resolved PATH this function returns.
	// Read, write and list on this resolution go through OpenAnchoredRoot
	// (via openSharedDirRoot), which re-verifies the leaf's identity
	// immediately before opening it; removing the shared dir's declaration
	// instead uses DeleteSharedDir's own no-follow walk. This check exists
	// so that the returned Path itself, and NFSHostBase/NFSRelPath (used to
	// safely CREATE the leaf if it doesn't exist yet), are never handed back
	// pointing through a symlinked structural component when the leaf
	// already exists.
	resolvedHostBase, err := filepath.EvalSymlinks(res.HostBase)
	if err != nil {
		return nil, true, fmt.Errorf("server.shared_dir_storage: resolve host base symlinks: %w", err)
	}
	wantResolvedLeaf := filepath.Join(resolvedHostBase, sd.ServerRelativePath)
	resolvedLeaf, evalErr := filepath.EvalSymlinks(sd.HostPath)
	if evalErr != nil {
		if os.IsNotExist(evalErr) {
			// Nothing has created this shared dir on the export yet (e.g.
			// no agent has started in this project). That's not a security
			// concern by itself — only "nothing to browse yet" — but it
			// also means EvalSymlinks couldn't confirm the structural
			// components are symlink-free. Callers must NOT create
			// anything at the returned Path directly: they must go through
			// ensureNFSSharedDirLeaf (shareddirs.EnsureLeaf), which
			// independently refuses a symlinked structural component via
			// its own O_NOFOLLOW walk regardless of what this function
			// assumed here.
			return &sharedDirResolution{
				Path:           wantResolvedLeaf,
				IsLocal:        true,
				Backend:        "nfs",
				NFSHostBase:    resolvedHostBase,
				NFSRelPath:     sd.ServerRelativePath,
				NFSSubPathRoot: subPathRoot,
			}, true, nil
		}
		return nil, true, fmt.Errorf("server.shared_dir_storage: resolve shared dir %q: %w", dirName, evalErr)
	}
	if resolvedLeaf != wantResolvedLeaf {
		return nil, true, fmt.Errorf(
			"server.shared_dir_storage: shared dir %q resolves through a symlink to %q, want %q",
			dirName, resolvedLeaf, wantResolvedLeaf)
	}

	return &sharedDirResolution{
		Path:           resolvedLeaf,
		IsLocal:        true,
		Backend:        "nfs",
		NFSHostBase:    resolvedHostBase,
		NFSRelPath:     sd.ServerRelativePath,
		NFSSubPathRoot: subPathRoot,
	}, true, nil
}

// resolveHubProjectSharedDirPath resolves the project-configs shared dir path for
// a project whose workspace lives at ~/.scion/projects/<slug>/. It reads the .scion
// marker (or project-id for git clones) to find the external project-configs path,
// then returns the shared-dirs/<name> subdirectory within it.
func resolveHubProjectSharedDirPath(projectSlug, dirName string) (string, error) {
	workspacePath, err := hubManagedProjectPath(projectSlug)
	if err != nil {
		return "", err
	}
	scionPath := filepath.Join(workspacePath, config.DotScion)
	projectDir, _, err := config.ResolveProjectPath(scionPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project path for %s: %w", projectSlug, err)
	}
	return config.GetSharedDirPath(projectDir, dirName)
}

// validateWorkspaceFilePath validates that a file path is safe for workspace operations.
// It rejects empty paths, absolute paths, and path traversal.
func validateWorkspaceFilePath(path string) error {
	if path == "" {
		return fmt.Errorf("path is empty")
	}

	// Reject absolute paths
	if filepath.IsAbs(path) {
		return fmt.Errorf("absolute paths not allowed")
	}

	// Clean the path and check for traversal
	cleaned := filepath.Clean(path)
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, string(filepath.Separator)+"..") {
		return fmt.Errorf("path traversal not allowed")
	}

	return nil
}

// handleProjectWorkspacePull performs a `git pull --ff-only` on a shared-workspace project.
func (s *Server) handleProjectWorkspacePull(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Phase 0 safety gate: reject pull on Cloud Run without durable storage.
	// Pull modifies the workspace filesystem, so it is a write operation.
	if s.workspaceWriteBlocked() {
		writeError(w, http.StatusServiceUnavailable, "workspace_writes_unavailable",
			errWorkspaceWritesUnavailable, nil)
		return
	}

	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if !project.IsSharedWorkspace() {
		Conflict(w, "Pull is only available for shared-workspace git projects")
		return
	}

	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		InternalError(w)
		return
	}

	token := s.resolveCloneToken(ctx, project)

	pullResult, err := util.PullSharedWorkspace(workspacePath, token)
	if err != nil {
		slog.Warn("shared workspace pull failed",
			"project_id", project.ID, "error", err.Error())

		statusCode := http.StatusConflict
		errorCode := ErrCodePullFailed
		var details map[string]interface{}
		var gitErr *util.GitError
		if errors.As(err, &gitErr) {
			if guidance := gitErr.UserGuidance(); guidance != "" {
				details = map[string]interface{}{"guidance": guidance}
			}
			switch gitErr.Kind {
			case util.GitErrAuth:
				statusCode = http.StatusUnauthorized
			case util.GitErrNetwork:
				statusCode = http.StatusBadGateway
			}
		}
		writeError(w, statusCode, errorCode, err.Error(), details)
		return
	}

	writeJSON(w, http.StatusOK, struct {
		Status  string                `json:"status"`
		Updated bool                  `json:"updated"`
		Commits []util.PullCommitInfo `json:"commits,omitempty"`
	}{
		Status:  "ok",
		Updated: pullResult.Updated,
		Commits: pullResult.Commits,
	})
}

// formatByteSize formats a byte count as a human-readable string.
func formatByteSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// cleanEmptyDirs removes empty directories from targetDir upwards, stopping at
// the root itself. targetDir is relative to root, and every step goes through
// the Root, so the walk upwards cannot leave the tree — and because it stops at
// ".", it can never remove the workspace or shared directory itself.
func cleanEmptyDirs(root *os.Root, targetDir string) {
	for targetDir != "." && targetDir != "" && targetDir != string(filepath.Separator) {
		entries, err := fs.ReadDir(root.FS(), targetDir)
		if err != nil || len(entries) > 0 {
			break
		}
		if err := root.Remove(targetDir); err != nil {
			break
		}
		targetDir = filepath.Dir(targetDir)
	}
}
