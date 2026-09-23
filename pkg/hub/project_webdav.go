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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"golang.org/x/net/webdav"
)

// syncExcludePatterns defines directory/file prefixes excluded from WebDAV sync.
// These are matched against the first path component.
var syncExcludePatterns = []string{
	".git",
	".scion",
	"node_modules",
}

// syncExcludeExtensions defines file extensions excluded from WebDAV sync.
var syncExcludeExtensions = []string{
	".env",
}

// handleProjectWebDAV serves a WebDAV endpoint for project workspace file sync.
// It mounts at /api/v1/projects/{projectId}/dav/ and serves the project's workspace
// directory with file exclusion filters applied.
//
// For hub-managed and shared-workspace projects, it serves the workspace directly.
// For linked projects (workspace on a remote broker), it serves from the hub's
// cached copy. The cache is populated via the cache/refresh or cache/notify
// endpoints (Phase 3: Linked Project Relay).
func (s *Server) handleProjectWebDAV(w http.ResponseWriter, r *http.Request, projectID, davPath string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Determine workspace path based on project type
	workspacePath, err := s.resolveProjectWebDAVPath(ctx, project)
	if err != nil {
		Conflict(w, err.Error())
		return
	}

	// Open the workspace as a confined root. Every path the WebDAV handler
	// resolves below is resolved through this root, so a symlink stored in the
	// workspace cannot be used to reach a file outside it. Creating the
	// directory when it is absent is the long-standing behaviour of this
	// endpoint — a client syncs into a workspace that does not exist yet — and
	// openConfinedBase keeps that while refusing a base that is itself a link.
	root, err := openConfinedBase(workspacePath, true)
	if err != nil {
		slog.Error("failed to open project workspace directory",
			"project_id", projectID, "path", workspacePath, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	// Build the prefix that the WebDAV handler should strip.
	// The full URL path is /api/v1/{projects|projects}/{id}/dav/...
	// We need to strip everything up to and including /dav
	prefixEnd := strings.Index(r.URL.Path, "/dav/")
	if prefixEnd == -1 {
		prefixEnd = strings.Index(r.URL.Path, "/dav")
	}
	if prefixEnd == -1 {
		NotFound(w, "WebDAV endpoint")
		return
	}
	prefix := r.URL.Path[:prefixEnd+len("/dav")]

	// Phase 0 safety gate: reject write operations on Cloud Run without durable storage.
	if s.workspaceWriteBlocked() {
		switch r.Method {
		case "PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH":
			writeError(w, http.StatusServiceUnavailable, "workspace_writes_unavailable",
				errWorkspaceWritesUnavailable, nil)
			return
		}
	}

	// Look up or create a per-project lock store so that WebDAV locks
	// survive across HTTP requests within a single instance. Previously,
	// NewMemLS() was called per-request, making locks completely ephemeral.
	// Use Load first to avoid allocating a new MemLS on every request.
	var lockStore interface{}
	if ls, ok := s.webdavLocks.Load(projectID); ok {
		lockStore = ls
	} else {
		lockStore, _ = s.webdavLocks.LoadOrStore(projectID, webdav.NewMemLS())
	}

	handler := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: &filteredFS{root: &confinedDavFS{root: root}},
		LockSystem: lockStore.(webdav.LockSystem),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				slog.Debug("webdav operation", "method", r.Method, "path", r.URL.Path, "error", err)
			}
		},
	}

	handler.ServeHTTP(w, r)

	// Update sync state after successful write operations
	if r.Method == "PUT" || r.Method == "DELETE" || r.Method == "MKCOL" || r.Method == "MOVE" || r.Method == "COPY" {
		go s.updateProjectSyncState(project.ID, workspacePath)
	}
}

// updateProjectSyncState recalculates and persists file count and total bytes for a project.
func (s *Server) updateProjectSyncState(projectID, workspacePath string) {
	var fileCount int
	var totalBytes int64

	_ = walkFilteredDir(workspacePath, func(relPath string, info os.FileInfo) {
		fileCount++
		totalBytes += info.Size()
	})

	now := time.Now()
	state := &store.ProjectSyncState{
		ProjectID:    projectID,
		BrokerID:     "", // hub-managed
		LastSyncTime: &now,
		FileCount:    fileCount,
		TotalBytes:   totalBytes,
	}

	if err := s.store.UpsertProjectSyncState(context.Background(), state); err != nil {
		slog.Warn("failed to update project sync state", "project_id", projectID, "error", err)
	}
}

// resolveProjectWebDAVPath determines the filesystem path to serve via WebDAV
// for a given project. For hub-managed and shared-workspace projects, this is the
// hub-managed workspace directory. For linked projects (workspace on a remote
// broker), this is the hub's cached copy of that workspace.
func (s *Server) resolveProjectWebDAVPath(ctx context.Context, project *store.Project) (string, error) {
	// Hub-managed projects (no git remote) always have a managed workspace
	if project.GitRemote == "" {
		path, err := s.hubManagedProjectPath(project.Slug)
		if err != nil {
			return "", fmt.Errorf("failed to resolve project path")
		}
		return path, nil
	}

	// Shared-workspace git projects have a managed workspace on the hub
	if project.IsSharedWorkspace() {
		path, err := s.hubManagedProjectPath(project.Slug)
		if err != nil {
			return "", fmt.Errorf("failed to resolve project path")
		}
		return path, nil
	}

	// Linked projects: check if there's a co-located broker with a local path
	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project providers")
	}

	// Git projects with no providers have no accessible workspace — the workspace
	// lives on broker machines and no broker has registered for this project yet.
	if len(providers) == 0 {
		return "", fmt.Errorf("workspace is not available for git-anchored projects without a registered provider")
	}

	// Check for co-located (embedded) broker with a local path
	for _, p := range providers {
		if s.isEmbeddedBroker(p.BrokerID) && p.LocalPath != "" {
			// Co-located broker: serve from local filesystem directly
			return p.LocalPath, nil
		}
	}

	// Remote linked project: serve from the hub's cached copy.
	// The cache is populated via cache/refresh or cache/notify endpoints.
	cachePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project cache path")
	}

	// If cache doesn't exist yet, return the path anyway (MkdirAll will create it).
	// The client should trigger a cache/refresh to populate it.
	if !s.hasProjectCache(project.Slug) {
		slog.Debug("linked project cache not yet populated",
			"project_id", project.ID, "slug", project.Slug)
	}

	return cachePath, nil
}

// walkFilteredDir walks a directory, calling fn for each non-excluded file.
func walkFilteredDir(root string, fn func(relPath string, info os.FileInfo)) error {
	return walkFilteredDirRecursive(root, "", fn)
}

func walkFilteredDirRecursive(root, prefix string, fn func(relPath string, info os.FileInfo)) error {
	fullDir := root
	if prefix != "" {
		fullDir = root + "/" + prefix
	}

	entries, err := os.ReadDir(fullDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		relPath := name
		if prefix != "" {
			relPath = prefix + "/" + name
		}

		if isExcluded(relPath) {
			continue
		}

		if entry.IsDir() {
			_ = walkFilteredDirRecursive(root, relPath, fn)
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		fn(relPath, info)
	}
	return nil
}

// confinedDavFS serves a directory over WebDAV through an *os.Root, so that
// every path is resolved one component at a time inside that directory.
//
// It replaces webdav.Dir, which joins the request path onto the directory and
// hands the result to the os package: that check is lexical and is done once,
// before the filesystem sees the path, so it says nothing about what the path
// resolves to. A workspace holds content the hub does not control — it is a git
// checkout, and agents write into it — so a symlink in it is an expected
// condition rather than an anomaly, and following one would take a request to
// whatever the link named. Resolving through the root removes the question:
// the kernel refuses a component that leaves the directory, with no window
// between the check and the operation.
//
// Symlinks that resolve inside the workspace keep working as they did.
type confinedDavFS struct {
	root *os.Root
}

// davRelPath converts a WebDAV path, which is slash-separated and normally
// rooted, into a path relative to the root.
//
// path.Clean collapses any ".." before the root sees it, which matches what
// webdav.Dir did; it is kept as a first pass rather than relied on, since the
// root rejects an escaping component regardless.
func davRelPath(name string) (string, error) {
	if name == "" {
		return "", os.ErrNotExist
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	rel := strings.TrimPrefix(path.Clean(name), "/")
	if rel == "" {
		return ".", nil
	}
	return rel, nil
}

func (d *confinedDavFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	rel, err := davRelPath(name)
	if err != nil {
		return err
	}
	return d.root.Mkdir(rel, perm)
}

func (d *confinedDavFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	rel, err := davRelPath(name)
	if err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(rel, flag, perm)
	if err != nil {
		// Returning the error alone would discard the typed nil *os.File that
		// callers compare against nil.
		return nil, err
	}
	return f, nil
}

func (d *confinedDavFS) RemoveAll(ctx context.Context, name string) error {
	rel, err := davRelPath(name)
	if err != nil {
		return err
	}
	if rel == "." {
		// Deleting the workspace itself is not a sync operation.
		return os.ErrPermission
	}
	return d.root.RemoveAll(rel)
}

func (d *confinedDavFS) Rename(ctx context.Context, oldName, newName string) error {
	oldRel, err := davRelPath(oldName)
	if err != nil {
		return err
	}
	newRel, err := davRelPath(newName)
	if err != nil {
		return err
	}
	if oldRel == "." || newRel == "." {
		return os.ErrPermission
	}
	return d.root.Rename(oldRel, newRel)
}

// Stat resolves the path, and where it cannot, describes the link itself.
//
// Following the link is the behaviour callers depend on: a symlink inside the
// workspace is a transparent file to a sync client, and reporting it as a link
// would give a size that disagrees with the bytes a GET returns. So the follow
// is tried first and is what succeeds for anything resolvable in the workspace.
//
// When the follow fails on a symlink — it leaves the workspace, or dangles —
// returning the error would be worse than useless. WebDAV stats a path before
// deleting it, so an unstattable link could never be removed, and it is
// visible in a listing: the workspace would show an entry that no client could
// clear, and every sync would keep failing on it. Reporting the link itself
// leaves it inert and removable. Nothing is read through it either way; that
// is decided by OpenFile, which does not have this fallback.
func (d *confinedDavFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	rel, err := davRelPath(name)
	if err != nil {
		return nil, err
	}
	fi, err := d.root.Stat(rel)
	if err == nil {
		return fi, nil
	}
	if linkInfo, linkErr := d.root.Lstat(rel); linkErr == nil && linkInfo.Mode()&os.ModeSymlink != 0 {
		return linkInfo, nil
	}
	return nil, err
}

// filteredFS wraps a webdav.FileSystem to exclude sync-excluded paths.
type filteredFS struct {
	root webdav.FileSystem
}

func (fs *filteredFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if isExcluded(name) {
		return os.ErrPermission
	}
	return fs.root.Mkdir(ctx, name, perm)
}

func (fs *filteredFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if isExcluded(name) {
		return nil, os.ErrNotExist
	}

	f, err := fs.root.OpenFile(ctx, name, flag, perm)
	if err != nil {
		return f, err
	}

	// If this is a directory being opened for reading, wrap to filter children
	info, statErr := f.Stat()
	if statErr == nil && info.IsDir() {
		return &filteredDir{File: f, dirName: name}, nil
	}

	return f, nil
}

func (fs *filteredFS) RemoveAll(ctx context.Context, name string) error {
	if isExcluded(name) {
		return os.ErrPermission
	}
	return fs.root.RemoveAll(ctx, name)
}

func (fs *filteredFS) Rename(ctx context.Context, oldName, newName string) error {
	if isExcluded(oldName) || isExcluded(newName) {
		return os.ErrPermission
	}
	return fs.root.Rename(ctx, oldName, newName)
}

func (fs *filteredFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if isExcluded(name) {
		return nil, os.ErrNotExist
	}
	return fs.root.Stat(ctx, name)
}

// filteredDir wraps a webdav.File (directory) to exclude entries from Readdir.
type filteredDir struct {
	webdav.File
	dirName string
}

func (d *filteredDir) Readdir(count int) ([]os.FileInfo, error) {
	entries, err := d.File.Readdir(count)
	if err != nil {
		return entries, err
	}

	filtered := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		childPath := path.Join(d.dirName, entry.Name())
		if !isExcluded(childPath) {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

// isExcluded returns true if a path should be excluded from sync.
// name is a slash-separated path relative to the workspace root (may have a leading /).
func isExcluded(name string) bool {
	// Normalize: strip leading slash
	clean := strings.TrimPrefix(path.Clean(name), "/")
	if clean == "" || clean == "." {
		return false
	}

	// Get the top-level component
	topLevel := clean
	if idx := strings.IndexByte(clean, '/'); idx >= 0 {
		topLevel = clean[:idx]
	}

	// Check directory prefix exclusions
	for _, pattern := range syncExcludePatterns {
		if topLevel == pattern {
			return true
		}
	}

	// Check extension exclusions (on the full filename, not just top-level)
	baseName := path.Base(clean)
	for _, ext := range syncExcludeExtensions {
		if strings.HasSuffix(baseName, ext) {
			return true
		}
	}

	return false
}

// ProjectSyncStatusResponse is the response for the sync status endpoint.
type ProjectSyncStatusResponse struct {
	ProjectID  string                   `json:"projectId"`
	States     []store.ProjectSyncState `json:"states"`
	TotalFiles int                      `json:"totalFiles"`
	TotalBytes int64                    `json:"totalBytes"`
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (r ProjectSyncStatusResponse) MarshalJSON() ([]byte, error) {
	type Alias ProjectSyncStatusResponse
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId"`
	}{
		Alias:   Alias(r),
		GroveID: r.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (r *ProjectSyncStatusResponse) UnmarshalJSON(data []byte) error {
	type Alias ProjectSyncStatusResponse
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.GroveID != "" {
		r.ProjectID = aux.GroveID
	}
	return nil
}

// handleProjectSyncStatus returns the sync status for a project.
func (s *Server) handleProjectSyncStatus(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	// Verify project exists
	_, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	states, err := s.store.ListProjectSyncStates(ctx, projectID)
	if err != nil {
		InternalError(w)
		return
	}

	var totalFiles int
	var totalBytes int64
	for _, st := range states {
		totalFiles += st.FileCount
		totalBytes += st.TotalBytes
	}

	writeJSON(w, http.StatusOK, ProjectSyncStatusResponse{
		ProjectID:  projectID,
		States:     states,
		TotalFiles: totalFiles,
		TotalBytes: totalBytes,
	})
}
