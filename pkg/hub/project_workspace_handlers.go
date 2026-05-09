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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// maxUploadTotalSize is the maximum total request body size for file uploads (100MB).
const maxUploadTotalSize = 100 * 1024 * 1024

// maxUploadFileSize is the maximum size for a single uploaded file (50MB).
const maxUploadFileSize = 50 * 1024 * 1024

// maxEditableFileSize is the maximum file size the editor will serve for inline editing (1MB).
const maxEditableFileSize = 1 * 1024 * 1024

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
	TotalSize  int64                `json:"totalSize"`
	TotalCount int                  `json:"totalCount"`
	HasMore    bool                 `json:"hasMore,omitempty"`
}

// SharedDirListResponse extends the workspace list response with provider metadata.
type SharedDirListResponse struct {
	Files         []ProjectWorkspaceFile `json:"files"`
	TotalSize     int64                `json:"totalSize"`
	TotalCount    int                  `json:"totalCount"`
	HasMore       bool                 `json:"hasMore,omitempty"`
	ProviderCount int                  `json:"providerCount,omitempty"`
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
type FileSearcher interface {
	Search(root, query string, limit int) (FileSearchResult, error)
}

// defaultFileSearcher is the package-level FileSearcher used by the handlers.
var defaultFileSearcher FileSearcher = walkDirSearcher{}

// walkDirSearcher implements FileSearcher using filepath.WalkDir.
type walkDirSearcher struct{}

func (walkDirSearcher) Search(root, query string, limit int) (FileSearchResult, error) {
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

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if relPath == "." || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		totalSize += info.Size()
		if matcher == nil || matcher(relPath) {
			allFiles = append(allFiles, ProjectWorkspaceFile{
				Path:    relPath,
				Size:    info.Size(),
				ModTime: info.ModTime(),
				Mode:    info.Mode().String(),
			})
		}
		return nil
	})

	if err != nil {
		if os.IsNotExist(err) {
			return FileSearchResult{Files: []ProjectWorkspaceFile{}}, nil
		}
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

	// Resolve workspace path — supports hub-native, shared-workspace, and linked projects
	workspacePath, err := s.resolveProjectWebDAVPath(ctx, project)
	if err != nil {
		Conflict(w, err.Error())
		return
	}

	switch {
	case r.Method == http.MethodGet && filePath == "":
		s.handleProjectWorkspaceList(w, r, workspacePath)
	case r.Method == http.MethodGet && filePath != "":
		s.handleProjectWorkspaceDownload(w, r, workspacePath, filePath)
	case r.Method == http.MethodPost && filePath == "":
		s.handleProjectWorkspaceUpload(w, r, workspacePath)
	case r.Method == http.MethodPut && filePath != "":
		s.handleProjectWorkspaceWrite(w, r, workspacePath, filePath)
	case r.Method == http.MethodDelete && filePath != "":
		s.handleProjectWorkspaceDelete(w, workspacePath, filePath)
	default:
		MethodNotAllowed(w)
	}
}

// handleProjectWorkspaceList lists files in a project workspace.
// Accepts optional query parameters:
//   - q:     filter pattern (regex or fuzzy fallback, case-insensitive)
//   - limit: max results (default 500, cap 2000)
func (s *Server) handleProjectWorkspaceList(w http.ResponseWriter, r *http.Request, workspacePath string) {
	query := r.URL.Query().Get("q")
	limit := intQueryParam(r, "limit", 500, 2000)

	result, err := defaultFileSearcher.Search(workspacePath, query, limit)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, ProjectWorkspaceListResponse{
		Files:      result.Files,
		TotalSize:  result.TotalSize,
		TotalCount: result.TotalCount,
		HasMore:    result.HasMore,
	})
}

// handleSharedDirFileList lists files in a shared directory, adding provider metadata
// to the response so the frontend can show multi-broker warnings.
// Accepts the same optional query parameters as handleProjectWorkspaceList (q, limit).
func (s *Server) handleSharedDirFileList(w http.ResponseWriter, r *http.Request, sharedDirPath string, res *sharedDirResolution) {
	query := r.URL.Query().Get("q")
	limit := intQueryParam(r, "limit", 500, 2000)

	result, err := defaultFileSearcher.Search(sharedDirPath, query, limit)
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
func (s *Server) handleProjectWorkspaceUpload(w http.ResponseWriter, r *http.Request, workspacePath string) {
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

			// Create parent directories
			destPath := filepath.Join(workspacePath, relPath)
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				src.Close()
				InternalError(w)
				return
			}

			// Write file to disk
			dst, err := os.Create(destPath)
			if err != nil {
				src.Close()
				InternalError(w)
				return
			}

			written, err := io.Copy(dst, src)
			src.Close()
			dst.Close()

			if err != nil {
				InternalError(w)
				return
			}

			// Get file info for response
			info, err := os.Stat(destPath)
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
func (s *Server) handleProjectWorkspaceDownload(w http.ResponseWriter, r *http.Request, workspacePath, filePath string) {
	// Validate the file path
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	fullPath := filepath.Join(workspacePath, filePath)

	// Check file exists and is not a directory
	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "File")
			return
		}
		InternalError(w)
		return
	}
	if info.IsDir() {
		BadRequest(w, "Cannot download a directory")
		return
	}

	// JSON format: return content wrapped with metadata for the editor
	if r.URL.Query().Get("format") == "json" {
		if info.Size() > maxEditableFileSize {
			BadRequest(w, fmt.Sprintf("File too large for editing (%s). Maximum is 1MB.", formatByteSize(info.Size())))
			return
		}

		data, readErr := os.ReadFile(fullPath)
		if readErr != nil {
			InternalError(w)
			return
		}

		// Verify content is valid UTF-8 text
		if !utf8.Valid(data) {
			BadRequest(w, "File contains binary content and cannot be edited")
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
	f, err := os.Open(fullPath)
	if err != nil {
		InternalError(w)
		return
	}
	defer f.Close()

	// Determine content type from extension, default to octet-stream
	fileName := filepath.Base(filePath)
	contentType := mime.TypeByExtension(filepath.Ext(fileName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	disposition := "attachment"
	if r.URL.Query().Get("view") == "true" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, fileName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))

	io.Copy(w, f)
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

	// Resolve workspace path — supports hub-native, shared-workspace, and linked projects
	workspacePath, err := s.resolveProjectWebDAVPath(ctx, project)
	if err != nil {
		Conflict(w, err.Error())
		return
	}

	// Check workspace directory exists
	if _, err := os.Stat(workspacePath); err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "Workspace")
			return
		}
		InternalError(w)
		return
	}

	archiveName := project.Slug + "-workspace.zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

	zw := zip.NewWriter(w)
	defer zw.Close()

	err = filepath.WalkDir(workspacePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(workspacePath, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		// Skip directories
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		// Use the relative path so directory structure is preserved
		header.Name = relPath
		header.Method = zip.Deflate

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(writer, f)
		return err
	})

	if err != nil {
		// At this point we've already started writing, so we can't send an error response.
		// The zip will be truncated/corrupt, which the client will notice.
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
func (s *Server) handleProjectWorkspaceWrite(w http.ResponseWriter, r *http.Request, workspacePath, filePath string) {
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

	fullPath := filepath.Join(workspacePath, filePath)

	// Optimistic concurrency check: if expectedModTime is set, verify the file
	// has not been modified since the client loaded it.
	if req.ExpectedModTime != "" {
		expectedTime, parseErr := time.Parse(time.RFC3339Nano, req.ExpectedModTime)
		if parseErr != nil {
			BadRequest(w, "Invalid expectedModTime format — use RFC3339")
			return
		}

		info, statErr := os.Stat(fullPath)
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

	// Create parent directories if needed
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		InternalError(w)
		return
	}

	// Write the file
	if err := os.WriteFile(fullPath, []byte(req.Content), 0644); err != nil {
		InternalError(w)
		return
	}

	// Read back file info for the response
	info, err := os.Stat(fullPath)
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
func (s *Server) handleProjectWorkspaceDelete(w http.ResponseWriter, workspacePath, filePath string) {
	// Validate the file path
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	fullPath := filepath.Join(workspacePath, filePath)

	// Check file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "File")
			return
		}
		InternalError(w)
		return
	}

	// Remove the file
	if err := os.Remove(fullPath); err != nil {
		InternalError(w)
		return
	}

	// Clean up empty parent directories
	cleanEmptyDirs(workspacePath, filepath.Dir(fullPath))

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
		Conflict(w, resolveErr.Error())
		return
	}
	sharedDirPath := resolution.Path

	// Ensure the directory exists
	if err := os.MkdirAll(sharedDirPath, 0755); err != nil {
		InternalError(w)
		return
	}

	switch {
	case r.Method == http.MethodGet && filePath == "":
		s.handleSharedDirFileList(w, r, sharedDirPath, resolution)
	case r.Method == http.MethodGet && filePath != "":
		s.handleProjectWorkspaceDownload(w, r, sharedDirPath, filePath)
	case r.Method == http.MethodPost && filePath == "":
		s.handleProjectWorkspaceUpload(w, r, sharedDirPath)
	case r.Method == http.MethodPut && filePath != "":
		s.handleProjectWorkspaceWrite(w, r, sharedDirPath, filePath)
	case r.Method == http.MethodDelete && filePath != "":
		s.handleProjectWorkspaceDelete(w, sharedDirPath, filePath)
	default:
		MethodNotAllowed(w)
	}
}

// sharedDirResolution holds the resolved path and metadata for shared dir browsing.
type sharedDirResolution struct {
	Path          string
	ProviderCount int  // total project providers (for multi-broker warning)
	IsLocal       bool // true when resolved via co-located broker
}

// resolveSharedDirPath resolves the host-side path for a shared directory.
// Shared dirs always live under ~/.scion/project-configs/<slug>__<uuid>/shared-dirs/<name>,
// matching the path used by agent provisioning (config.GetSharedDirPath).
// For git-based projects with a co-located broker that has a LocalPath, the path is
// resolved via config.GetSharedDirPath(localPath, dirName). Otherwise, the path is
// resolved via the .scion marker in the hub-native workspace directory.
func (s *Server) resolveSharedDirPath(ctx context.Context, project *store.Project, dirName string) (*sharedDirResolution, error) {
	if project.GitRemote == "" {
		// Hub-native project: resolve via the .scion marker in the workspace directory
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

// resolveHubProjectSharedDirPath resolves the project-configs shared dir path for
// a project whose workspace lives at ~/.scion/projects/<slug>/. It reads the .scion
// marker (or project-id for git clones) to find the external project-configs path,
// then returns the shared-dirs/<name> subdirectory within it.
func resolveHubProjectSharedDirPath(projectSlug, dirName string) (string, error) {
	workspacePath, err := hubNativeProjectPath(projectSlug)
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

	workspacePath, err := hubNativeProjectPath(project.Slug)
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

// cleanEmptyDirs removes empty directories from targetDir up to (but not including) rootDir.
func cleanEmptyDirs(rootDir, targetDir string) {
	for targetDir != rootDir {
		entries, err := os.ReadDir(targetDir)
		if err != nil || len(entries) > 0 {
			break
		}
		os.Remove(targetDir)
		targetDir = filepath.Dir(targetDir)
	}
}
