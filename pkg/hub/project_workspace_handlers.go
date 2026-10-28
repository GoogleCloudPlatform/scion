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
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
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

// untrustedContentSandboxCSP is sent on hub responses whose bytes are written
// by users or agents rather than by the hub: workspace and shared-dir files
// (above all ?view=true HTML and SVG, which would otherwise render on the
// hub's origin with the viewer's session), and chat attachments. Anything that
// can write into a project, including its agents, could otherwise act as
// whoever opens the file. A sandboxed document gets an opaque origin: its
// scripts still run, so generated reports and visualisations keep working,
// but it cannot use the viewer's cookies against the hub or read the
// responses. allow-same-origin must never be added: it would undo exactly
// that isolation. The header has no effect when the response is loaded as a
// subresource such as an <img>, so inline image previews are unaffected.
//
// frame-ancestors 'none': these pages are never embedded in other pages,
// with or without the web frontend in front of the hub.
//
// allow-modals lets a document call print() (a report's Print button) and
// show alert/confirm/prompt dialogs. A dialog gives the document no reach
// beyond its own opaque origin: it is UI the document could draw itself.
//
// Never add allow-top-navigation-to-custom-protocols either: it would let the
// content launch desktop protocol handlers (mailto:, ms-word:, vscode:)
// without a click.
const untrustedContentSandboxCSP = "sandbox " + untrustedContentSandboxTokens + sandboxCSPSuffix

// agentServedPageSandboxCSP is the same policy with the tokens interactive
// apps need added to its sandbox directive. It is sent on pages an agent
// serves live through the port-forward and conduit proxies, which are apps
// (dashboards with search boxes and filters, 3D and map viewers) rather than
// stored files.
const agentServedPageSandboxCSP = "sandbox " + untrustedContentSandboxTokens + " " + agentServedPageSandboxTokens + sandboxCSPSuffix

// untrustedContentSandboxTokens are the sandbox tokens for every response
// whose bytes are written by users or agents. allow-same-origin must never
// be added.
const untrustedContentSandboxTokens = "allow-scripts allow-popups allow-popups-to-escape-sandbox allow-downloads allow-modals"

// agentServedPageSandboxTokens are added on agent-served pages only. None of
// them reaches past the page's opaque origin:
//   - allow-forms: a POST form the page submits is a cross-site request and
//     carries no SameSite=Lax session cookie; a GET form is a top-level
//     navigation a script can already make.
//   - allow-pointer-lock: needs a user gesture and is always released by Esc.
//   - allow-orientation-lock: locks screen rotation, normally only in
//     fullscreen.
//   - allow-presentation: the browser always asks which screen to use.
const agentServedPageSandboxTokens = "allow-forms allow-pointer-lock allow-orientation-lock allow-presentation"

// sandboxCSPSuffix follows the sandbox directive in every sandbox policy, so
// tokens are added to the token lists above, never appended to a finished
// policy string.
const sandboxCSPSuffix = "; frame-ancestors 'none'"

// untrustedContentPermissionsPolicy turns off the powerful browser features
// for user- and agent-written content. Browsers already deny most of them to
// an opaque origin; the header makes that hold independently of browser
// behaviour. It replaces webPermissionsPolicy on these responses, so it
// denies everything that one does. clipboard-read is off so a page cannot
// read what the viewer copied elsewhere; writing to the clipboard stays
// allowed.
const untrustedContentPermissionsPolicy = "accelerometer=(), gyroscope=(), magnetometer=(), " +
	"camera=(), microphone=(), geolocation=(), display-capture=(), " +
	"usb=(), serial=(), hid=(), bluetooth=(), payment=(), " +
	"publickey-credentials-get=(), publickey-credentials-create=(), clipboard-read=()"

// setUntrustedContentHeaders sets the sandbox csp and the Permissions-Policy
// on a hub response whose bytes are written by users or agents.
func setUntrustedContentHeaders(h http.Header, csp string) {
	h.Set("Content-Security-Policy", csp)
	h.Set("Permissions-Policy", untrustedContentPermissionsPolicy)
}

// contentDisposition builds a Content-Disposition header value with
// mime.FormatMediaType, as the skill and template handlers do, so non-ASCII
// filenames get the RFC 2231 filename* form. FormatMediaType returns "" for
// input it cannot encode; rather than drop the header (and with it the
// attachment disposition), fall back to a quoted filename with backslash and
// double-quote escaped so a crafted name cannot close the quoted string and
// append parameters (RFC 6266 §4.3). Go's header writer already neutralises
// CR and LF.
func contentDisposition(disposition, filename string) string {
	if v := mime.FormatMediaType(disposition, map[string]string{"filename": filename}); v != "" {
		return v
	}
	safeName := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(filename)
	return fmt.Sprintf(`%s; filename="%s"`, disposition, safeName)
}

// isCloudRunEnv checks K_SERVICE to determine if we're running on Cloud Run.
// In production, K_SERVICE is set once at container startup and never changes,
// so calling os.Getenv is effectively a cached lookup (the C library caches
// the environment block). This avoids the complexity of sync.Once while
// remaining test-friendly via t.Setenv.
func isCloudRunEnv() bool {
	return cloudrunenv.IsService()
}

// isCloudRunInstance reports whether the hub is running on a Cloud Run Instance.
// CLOUD_RUN_INSTANCE is set by the platform on Instances but NOT on Cloud Run
// Services (which use K_SERVICE instead). See design doc section 4.6.
func isCloudRunInstance() bool {
	return cloudrunenv.IsInstance()
}

// workspaceWriteBlocked reports whether writes to the hub's own workspace
// layout must be refused with 503: on a Cloud Run Service, unless the
// workspace Layout is a durable (shared) backend. An unbuildable Layout
// (for example a volume backend without a volume name, ptone/scion#1073)
// and an unrecognized backend fail closed. Self-hosted hubs and Cloud Run
// Instances (Tier 0, ephemeral by design; see cloudRunRequiresDurableWrites)
// are never blocked.
func (s *Server) workspaceWriteBlocked() bool {
	if !cloudRunRequiresDurableWrites() {
		return false
	}
	layout, err := s.workspaceLayout()
	return err != nil || !layout.Backend.Durable()
}

// errSharedDirWritesUnavailable is returned when a shared-dir write is
// refused because the shared dir resolved to storage that is not durable on
// this hub (Cloud Run Service with shared dirs on instance-local disk).
const errSharedDirWritesUnavailable = "Shared directory writes are not available: shared directories are not on durable storage on this hub. Configure server.project_storage with a shared backend (nfs, cloudrun-volume or gke-shared-volume)."

// writeGate is the Cloud Run durability gate for one write target. The zero
// value allows writes.
type writeGate struct {
	code    string
	message string
}

// refuse answers 503 and reports true when the gate is closed.
func (g writeGate) refuse(w http.ResponseWriter) bool {
	if g.code == "" {
		return false
	}
	writeError(w, http.StatusServiceUnavailable, g.code, g.message, nil)
	return true
}

// workspaceWriteGate gates writes to a project workspace on the backend the
// workspace actually resolved to: on a Cloud Run
// Service a workspace on instance-local disk is refused, including a linked
// project's co-located broker path.
func (s *Server) workspaceWriteGate(resolved projectfs.Resolved) writeGate {
	if cloudRunRequiresDurableWrites() && !resolved.Backend.Durable() {
		return writeGate{code: "workspace_writes_unavailable", message: errWorkspaceWritesUnavailable}
	}
	return writeGate{}
}

// sharedDirWriteGate gates writes to a shared dir on the backend the shared
// dir actually resolved to, not on the workspace's: on a Cloud Run Service a
// shared dir that resolved to instance-local disk is refused even when the
// workspace is on NFS.
func (s *Server) sharedDirWriteGate(resolution *sharedDirResolution) writeGate {
	if !cloudRunRequiresDurableWrites() || resolution.Resolved.Backend.Durable() {
		return writeGate{}
	}
	return writeGate{code: "shared_dir_writes_unavailable", message: errSharedDirWritesUnavailable}
}

// cloudRunRequiresDurableWrites reports whether the hub must refuse writes
// to non-durable storage: true only on a Cloud Run Service. Self-hosted hubs
// write to local disk, and a Cloud Run Instance (Tier 0) is ephemeral by
// design (see workspaceWriteBlocked).
func cloudRunRequiresDurableWrites() bool {
	return isCloudRunEnv() && !isCloudRunInstance()
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
	// MigrationNotice is the Files-tab banner while legacy workspace trees
	// await the project storage migration (ptone/scion#5819); nil when there is none.
	MigrationNotice *WorkspaceMigrationNotice `json:"migrationNotice,omitempty"`
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
// TODO(P2): fold into projectfs.openConfinedDir
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

// resolveProjectWorkspace determines where a project's workspace lives as
// the hub serves it (file API, archive, resource import).
//
//   - Hub-managed and shared-workspace projects resolve through the layout
//     authority (projectWorkspaceResolved): the same projects/<uuid>/workspace
//     tree agents mount on a shared backend, ~/.scion/projects/<slug> on the
//     local backend. An empty-per-agent project has no project-scoped
//     workspace and gets errWorkspaceNoMount (409).
//   - Linked projects resolve to the co-located (embedded) broker's local
//     path when there is one. A linked project served only by remote brokers
//     has no workspace the hub can reach and gets a conflict error; there is
//     no hub-side copy.
//
// Errors are safe to show to clients (no filesystem paths). A storage
// misconfiguration wraps errWorkspaceStorageUnavailable (503).
func (s *Server) resolveProjectWorkspace(ctx context.Context, project *store.Project) (projectfs.Resolved, error) {
	// A project whose workspace lives on its remote brokers
	// (workspaceOnRemoteBrokers) has no workspace the hub can reach unless the
	// co-located broker serves it: 409, and the hub never creates its own copy.
	if s.workspaceOnRemoteBrokers(project) {
		embedded, err := s.hasEmbeddedProvider(ctx, project)
		if err != nil {
			return projectfs.Resolved{}, fmt.Errorf("failed to resolve project providers")
		}
		if !embedded {
			return projectfs.Resolved{}, errLinkedWorkspaceRemote
		}
	}
	res, err := s.projectWorkspaceResolved(project)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, projectfs.ErrNoMount):
		return projectfs.Resolved{}, errWorkspaceNoMount
	case errors.Is(err, errWorkspaceStorageUnavailable):
		s.projectsLogger().Warn("workspace storage is misconfigured; not resolving project workspace",
			"project_id", project.ID, "error", err)
		return projectfs.Resolved{}, fmt.Errorf("failed to resolve project path: %w", errWorkspaceStorageUnavailable)
	case !errors.Is(err, errNotHubWorkspace):
		return projectfs.Resolved{}, errors.New("failed to resolve project path")
	}

	// Linked projects: only a co-located broker's local path is reachable.
	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		return projectfs.Resolved{}, fmt.Errorf("failed to resolve project providers")
	}

	// Git projects with no providers have no accessible workspace — the workspace
	// lives on broker machines and no broker has registered for this project yet.
	if len(providers) == 0 {
		return projectfs.Resolved{}, fmt.Errorf("workspace is not available for git-anchored projects without a registered provider")
	}

	for _, p := range providers {
		if s.isEmbeddedBroker(p.BrokerID) && p.LocalPath != "" {
			workspaceRoot := filepath.Clean(p.LocalPath)
			// Providers may register either the checkout or its .scion directory.
			// Keep the global project confined to its existing root.
			if !isGlobalHubProject(project.Slug) && filepath.Base(workspaceRoot) == config.DotScion {
				workspaceRoot = filepath.Dir(workspaceRoot)
			}
			return projectfs.Resolved{
				Backend:  projectfs.BackendLocal,
				HostPath: workspaceRoot,
				Target:   projectfs.DefaultContainerWorkspace,
				Kind:     projectfs.KindWorkspace,
				Name:     "workspace",
			}, nil
		}
	}

	return projectfs.Resolved{}, errLinkedWorkspaceRemote
}

// sharedBaseMissing reports whether resolved is on a shared backend whose
// host base (the share's mount point) does not exist on this host: the
// storage is unavailable, which must not read as an empty workspace.
func sharedBaseMissing(resolved projectfs.Resolved) bool {
	if !resolved.Backend.Shared() || resolved.HostBase == "" {
		return false
	}
	_, err := os.Stat(resolved.HostBase)
	return errors.Is(err, fs.ErrNotExist)
}

// hasEmbeddedProvider reports whether the co-located (embedded) broker
// provides project.
func (s *Server) hasEmbeddedProvider(ctx context.Context, project *store.Project) (bool, error) {
	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		return false, err
	}
	for _, p := range providers {
		if s.isEmbeddedBroker(p.BrokerID) {
			return true, nil
		}
	}
	return false, nil
}

// errWorkspaceNoMount is returned for a project without a project-scoped
// workspace (empty-per-agent: each agent has its own private directory).
var errWorkspaceNoMount = errors.New("this project has no shared workspace: each agent gets its own private workspace directory")

// errLinkedWorkspaceRemote is returned for a linked project whose workspace
// lives only on remote brokers, which the hub cannot reach.
var errLinkedWorkspaceRemote = errors.New("workspace is not available: this linked project's workspace lives on remote runtime brokers without shared storage")

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
//
// An empty-per-agent project has no shared workspace and gets 409
// (see resolveProjectWorkspace).
func (s *Server) handleProjectWorkspace(w http.ResponseWriter, r *http.Request, project *store.Project, filePath string) {
	ctx := r.Context()

	// Resolve workspace — supports hub-managed, shared-workspace, and linked projects
	resolved, err := s.resolveProjectWorkspace(ctx, project)
	if err != nil {
		if !writeWorkspaceStorageUnavailable(w, err) {
			Conflict(w, err.Error())
		}
		return
	}

	var notice *WorkspaceMigrationNotice
	if r.Method == http.MethodGet && filePath == "" {
		notice = s.workspaceMigrationNotice(ctx, project)
	}

	// Every verb below operates through this Root and never on a path joined
	// onto the workspace path. Uploads and writes may still materialize the
	// workspace on first use, as they always have; reads and deletes do not.
	// Writes are refused before anything is created when the write gate is
	// closed.
	createIfMissing := r.Method == http.MethodPost || r.Method == http.MethodPut
	gate := s.workspaceWriteGate(resolved)
	if createIfMissing && gate.refuse(w) {
		return
	}
	root, err := resolved.Open(createIfMissing)
	if err != nil {
		if sharedBaseMissing(resolved) {
			// The share is not mounted: storage unavailable, not an empty
			// workspace.
			writeWorkspaceStorageUnavailable(w, errWorkspaceStorageUnavailable)
			return
		}
		if errors.Is(err, fs.ErrNotExist) {
			if r.Method == http.MethodGet && filePath == "" {
				writeJSON(w, http.StatusOK, ProjectWorkspaceListResponse{Files: []ProjectWorkspaceFile{}, MigrationNotice: notice})
				return
			}
			NotFound(w, "File")
			return
		}
		slog.ErrorContext(ctx, "failed to open project workspace",
			"project_id", project.ID, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	switch {
	case r.Method == http.MethodGet && filePath == "":
		s.handleProjectWorkspaceList(w, r, root, notice)
	case r.Method == http.MethodGet && filePath != "":
		s.handleProjectWorkspaceDownload(w, r, root, filePath)
	case r.Method == http.MethodPost && filePath == "":
		s.handleProjectWorkspaceUpload(w, r, root, gate)
	case r.Method == http.MethodPut && filePath != "":
		s.handleProjectWorkspaceWrite(w, r, root, filePath, gate)
	case r.Method == http.MethodDelete && filePath != "":
		s.handleProjectWorkspaceDelete(w, root, filePath, gate)
	default:
		if filePath == "" {
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
		} else {
			MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		}
	}
}

// handleProjectWorkspaceList lists files in a project workspace.
// Accepts optional query parameters:
//   - q:     filter pattern (regex or fuzzy fallback, case-insensitive)
//   - limit: max results (default 500, cap 2000)
func (s *Server) handleProjectWorkspaceList(w http.ResponseWriter, r *http.Request, root *os.Root, notice *WorkspaceMigrationNotice) {
	query := r.URL.Query().Get("q")
	limit := intQueryParam(r, "limit", 500, 2000)

	result, err := defaultFileSearcher.Search(root, query, limit)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, ProjectWorkspaceListResponse{
		Files:           result.Files,
		TotalSize:       result.TotalSize,
		TotalCount:      result.TotalCount,
		HasMore:         result.HasMore,
		MigrationNotice: notice,
	})
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
func (s *Server) handleProjectWorkspaceUpload(w http.ResponseWriter, r *http.Request, root *os.Root, gate writeGate) {
	// Phase 0 safety gate: reject writes on Cloud Run to storage that is not
	// durable for this target (the caller's gate).
	if gate.refuse(w) {
		return
	}

	// Apply total request body size limit
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadTotalSize)

	// Parse multipart form
	cleanup, err := parseMultipartUpload(r, s.uploadFormMaxMemory())
	defer cleanup()
	if err != nil {
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
	// Set on attachments too, so the file stays sandboxed if a browser ever
	// renders one instead of downloading it.
	setUntrustedContentHeaders(w.Header(), untrustedContentSandboxCSP)
	disposition := "attachment"
	if r.URL.Query().Get("view") == "true" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", contentDisposition(disposition, fileName))
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
func (s *Server) handleProjectWorkspaceArchive(w http.ResponseWriter, r *http.Request, project *store.Project) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	// Resolve workspace — supports hub-managed, shared-workspace, and linked projects
	resolved, err := s.resolveProjectWorkspace(ctx, project)
	if err != nil {
		if !writeWorkspaceStorageUnavailable(w, err) {
			Conflict(w, err.Error())
		}
		return
	}

	// Archiving never creates the workspace — an archive of nothing is just
	// "not found".
	root, err := resolved.Open(false)
	if err != nil {
		if sharedBaseMissing(resolved) {
			writeWorkspaceStorageUnavailable(w, errWorkspaceStorageUnavailable)
			return
		}
		if errors.Is(err, fs.ErrNotExist) {
			NotFound(w, "Workspace")
			return
		}
		slog.ErrorContext(ctx, "failed to open project workspace for archive",
			"project_id", project.ID, "error", err)
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
		slog.WarnContext(ctx, "failed to complete workspace archive", "project_id", project.ID, "error", err)
		return
	}
}

// handleProjectSharedDirArchive creates a zip archive of a shared directory and serves it for download.
// The project has already been loaded and authorized by
// handleProjectSharedDirRoutes.
func (s *Server) handleProjectSharedDirArchive(w http.ResponseWriter, r *http.Request, project *store.Project, dirName string) {
	ctx := r.Context()
	projectID := project.ID

	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
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
func (s *Server) handleProjectWorkspaceWrite(w http.ResponseWriter, r *http.Request, root *os.Root, filePath string, gate writeGate) {
	// Phase 0 safety gate: reject writes on Cloud Run to storage that is not
	// durable for this target (the caller's gate).
	if gate.refuse(w) {
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
func (s *Server) handleProjectWorkspaceDelete(w http.ResponseWriter, root *os.Root, filePath string, gate writeGate) {
	// Phase 0 safety gate: reject writes on Cloud Run to storage that is not
	// durable for this target (the caller's gate).
	if gate.refuse(w) {
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
//   - PUT  (filePath!="") → write file
//   - DELETE (filePath!="") → delete file
//
// The project has already been loaded and authorized by
// handleProjectSharedDirRoutes.
func (s *Server) handleSharedDirFiles(w http.ResponseWriter, r *http.Request, project *store.Project, dirName, filePath string) {
	ctx := r.Context()
	projectID := project.ID

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

	// Refuse a routed write (upload, write, delete) before opening, and so
	// before creating, the directory when its storage is not durable on this
	// hub. Unrouted method/path combinations still answer 405 below. The
	// write handlers check the same gate again as defense in depth.
	isWrite := (r.Method == http.MethodPost && filePath == "") ||
		(r.Method == http.MethodPut && filePath != "") ||
		(r.Method == http.MethodDelete && filePath != "")
	if isWrite && s.sharedDirWriteGate(resolution).refuse(w) {
		return
	}

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
		s.handleProjectWorkspaceUpload(w, r, root, s.sharedDirWriteGate(resolution))
	case r.Method == http.MethodPut && filePath != "":
		s.handleProjectWorkspaceWrite(w, r, root, filePath, s.sharedDirWriteGate(resolution))
	case r.Method == http.MethodDelete && filePath != "":
		s.handleProjectWorkspaceDelete(w, root, filePath, s.sharedDirWriteGate(resolution))
	default:
		if filePath == "" {
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
		} else {
			MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		}
	}
}

// sharedDirResolution holds the resolved path and metadata for shared dir browsing.
type sharedDirResolution struct {
	Path          string
	ProviderCount int  // total project providers (for multi-broker warning)
	IsLocal       bool // true when the hub itself can reach Path
	// Resolved is the layout authority's answer for this shared dir
	// (pkg/projectfs): its Backend, RelPath, HostBase and SubPathRoot are the
	// one source for how the directory is opened, gated and removed. Every
	// handle on the directory is opened through Resolved.Open.
	Resolved projectfs.Resolved
	// Source names the server block the Layout came from.
	Source sharedDirLayoutSource
}

// onSharedBackend reports whether the shared dir is on the shared tree
// (nfs or a shared volume), <subpath_root>/<project UUID>/shared-dirs/<name>.
func (r *sharedDirResolution) onSharedBackend() bool {
	return r.Resolved.Backend.Shared()
}

// openSharedDirRoot opens the resolved shared dir through the layout
// authority (projectfs.Resolved.Open): on NFS, pkg/shareddirs' anchored
// O_NOFOLLOW walk (and, with createIfMissing, shareddirs.EnsureLeaf first);
// on local disk, a root that refuses a symlinked directory. No caller joins
// paths and opens them itself.
func (s *Server) openSharedDirRoot(resolution *sharedDirResolution, createIfMissing bool) (*os.Root, error) {
	return resolution.Resolved.Open(createIfMissing)
}

// sharedDirLayoutSource names where a shared-dir Layout came from.
type sharedDirLayoutSource string

const (
	sharedDirLayoutFromProjectStorage sharedDirLayoutSource = "server.project_storage"
	sharedDirLayoutLocal              sharedDirLayoutSource = "local"
)

// sharedDirLayout returns the Layout the hub resolves shared-dir Mounts on:
// projectStorageLayout, labelled for logs.
func (s *Server) sharedDirLayout() (projectfs.Layout, sharedDirLayoutSource, error) {
	full, err := s.projectStorageLayout()
	if err != nil {
		return projectfs.Layout{}, sharedDirLayoutFromProjectStorage, err
	}
	layout := full.SharedDirLayout()
	if layout.Backend.Shared() {
		return layout, sharedDirLayoutFromProjectStorage, nil
	}
	return layout, sharedDirLayoutLocal, nil
}

// projectStorageLayout returns the hub's projectfs Layout, built from its
// in-memory server.project_storage (ServerConfig.ProjectStorageConfig, as
// resolved at startup; design fs-cleanup §P2) through
// config.ProjectStorageLayout, the translation the brokers use too. It is
// never built from a per-request settings-file read, the DB or project
// settings. One backend places the workspace and the shared dirs: nfs, or
// the cloudrun-volume / gke-shared-volume volume mounted at
// <volumeMountBase>/<volume_name>, gives the UUID-keyed tree; local (and no
// block) gives today's slug-keyed layout. The hub serves files from its own
// mount of the share, so a shared Layout's host base must be absolute.
func (s *Server) projectStorageLayout() (projectfs.Layout, error) {
	layout, err := config.ProjectStorageLayout(s.config.ProjectStorageConfig, volumeMountBase)
	if err != nil {
		return projectfs.Layout{}, err
	}
	for _, l := range []projectfs.Layout{layout, layout.SharedDirLayout()} {
		if l.Backend.Shared() && !filepath.IsAbs(l.HostBase) {
			return projectfs.Layout{}, fmt.Errorf("%s: the share must be mounted at an absolute path on the hub (host base %q)", config.ProjectStorageKey, l.HostBase)
		}
	}
	return layout, nil
}

// errSharedDirNotDeclared is returned for a shared dir the project does not
// declare (projectfs.ErrNoMount). Its message is fixed and carries no path.
var errSharedDirNotDeclared = errors.New("shared directory is not declared on this project")

// resolveSharedDirPath resolves where a project's shared directory lives,
// through the layout authority (pkg/projectfs) on the Layout from
// sharedDirLayout.
//
// On a shared Layout the shared dir is the one agents mount, on every
// runtime that can mount the shared tree:
// <subpath_root>/<project UUID>/shared-dirs/<name>, for every project,
// whether or not it has a co-located broker.
//
// On the local Layout the path is today's slug-keyed layout: for a git
// project with a co-located provider that has a LocalPath, the project's own
// project-config root (config.GetSharedDirPath); otherwise the hub's project
// record (resolveHubProjectSharedDirPath).
func (s *Server) resolveSharedDirPath(ctx context.Context, project *store.Project, dirName string) (*sharedDirResolution, error) {
	mount, err := projectfs.DeclaredSharedDir(project.SharedDirs, dirName, "")
	if err != nil {
		return nil, errSharedDirNotDeclared
	}
	return s.resolveSharedDirMount(ctx, project, mount)
}

// resolveSharedDirMount is resolveSharedDirPath for a shared-dir Mount the
// caller already holds (the declaration DELETE resolves the declaration it
// has just removed).
func (s *Server) resolveSharedDirMount(ctx context.Context, project *store.Project, mount projectfs.Mount) (*sharedDirResolution, error) {
	dirName := mount.Name
	layout, source, err := s.sharedDirLayout()
	if err != nil {
		// May carry configuration detail; never shown to the client.
		return nil, &sharedDirStorageResolveError{err: err}
	}

	if layout.Backend.Shared() {
		resolution, err := resolveNFSSharedDir(layout, source, project, mount)
		if err != nil {
			// Every error here is marked as a storage resolution failure:
			// its message may contain a resolved host path, and must not
			// reach the client directly.
			return nil, &sharedDirStorageResolveError{err: err}
		}
		return resolution, nil
	}

	if project.GitRemote == "" {
		// Hub-managed project: derive the project-configs path from the
		// project record.
		sdPath, err := resolveHubProjectSharedDirPath(project, dirName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve shared directory path: %w", err)
		}
		return localSharedDirResolution(sdPath, 0, mount), nil
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
				return localSharedDirResolution(sdPath, providerCount, mount), nil
			}
		}
	}

	// Fallback: embedded broker is a provider but has no LocalPath recorded
	// (e.g. auto-linked or shared-workspace project). Derive the project-configs
	// path from the project record.
	if embeddedIsProvider {
		sdPath, err := resolveHubProjectSharedDirPath(project, dirName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve shared directory path: %w", err)
		}
		return localSharedDirResolution(sdPath, providerCount, mount), nil
	}

	return nil, fmt.Errorf("shared directory file browsing requires a co-located runtime broker")
}

// localSharedDirResolution wraps a local-layout shared dir path, already
// resolved through projectfs by its caller, for the shared-dir handlers.
func localSharedDirResolution(sdPath string, providerCount int, mount projectfs.Mount) *sharedDirResolution {
	return &sharedDirResolution{
		Path:          sdPath,
		ProviderCount: providerCount,
		IsLocal:       true,
		Resolved: projectfs.Resolved{
			Backend: projectfs.BackendLocal, HostPath: sdPath,
			Target: mount.Target, Kind: mount.Kind, Name: mount.Name,
		},
		Source: sharedDirLayoutLocal,
	}
}

// sharedDirStorageUnavailableMessage is the fixed, detail-free message
// returned to the client when resolveSharedDirPath fails because the shared
// storage resolution itself failed. The underlying error can carry the
// resolved host path or a configuration error, neither of which belongs in
// an HTTP response; the detail goes to the server log instead.
const sharedDirStorageUnavailableMessage = "shared directory storage is misconfigured or unavailable; see hub logs"

// sharedDirStorageResolveError marks an error as having come from the
// shared (nfs) storage resolution path.
type sharedDirStorageResolveError struct{ err error }

func (e *sharedDirStorageResolveError) Error() string { return e.err.Error() }
func (e *sharedDirStorageResolveError) Unwrap() error { return e.err }

// resolveNFSSharedDir resolves a shared dir Mount for project on a shared
// Layout: Layout.Resolve validates the shared dir name and the project ID
// and confines the leaf to the project's shared-dirs/ (shareddirs.ConfineLeaf),
// then Resolved.VerifyNoSymlinks refuses a leaf that already exists behind a
// symlinked structural component. A leaf that does not exist yet is returned
// as is: it can only be created through Resolved.Open, whose O_NOFOLLOW walk
// refuses links on its own.
func resolveNFSSharedDir(layout projectfs.Layout, source sharedDirLayoutSource, project *store.Project, mount projectfs.Mount) (*sharedDirResolution, error) {
	resolved, err := layout.Resolve(projectfs.Project{ID: project.ID}, "", mount)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	path, err := resolved.VerifyNoSymlinks()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &sharedDirResolution{
		Path:     path,
		IsLocal:  true,
		Resolved: resolved,
		Source:   source,
	}, nil
}

// errSharedDirProjectRecordMismatch is returned when the project identity
// recorded in a hub-managed workspace resolves to a different shared directory
// than the hub's own project record. Its message is fixed and carries no path.
var errSharedDirProjectRecordMismatch = errors.New("workspace project identity does not match the project record")

// errSharedDirProjectRecordRequired is returned when no project record is
// supplied. Its message is fixed and carries no path.
var errSharedDirProjectRecordRequired = errors.New("project record is required")

// projectRecordSharedDirPath returns the host path of shared dir dirName for a
// project, derived only from the hub's project record (slug and ID):
// ~/.scion/project-configs/<slug>__<short-id>/shared-dirs/<dirName>.
func projectRecordSharedDirPath(project *store.Project, dirName string) (string, error) {
	if project == nil {
		return "", errSharedDirProjectRecordRequired
	}
	if err := validateProjectSlug(project.Slug); err != nil {
		return "", err
	}
	if !shareddirs.ValidProjectID(project.ID) {
		return "", fmt.Errorf("invalid project ID")
	}
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: dirName}}); err != nil {
		return "", err
	}
	marker := config.ProjectMarker{ProjectID: project.ID, ProjectSlug: project.Slug}
	externalPath, err := marker.ExternalProjectPath()
	if err != nil {
		return "", err
	}
	localLayout := projectfs.Layout{Backend: projectfs.BackendLocal}
	res, err := localLayout.Resolve(
		projectfs.Project{ConfigRoot: filepath.Dir(externalPath)}, "",
		projectfs.SharedDirMount(api.SharedDir{Name: dirName}, ""))
	if err != nil {
		return "", err
	}
	return res.HostPath, nil
}

// resolveHubProjectSharedDirPath resolves the shared dir path for a project
// whose workspace lives at ~/.scion/projects/<slug>/. The path always comes from
// the hub's project record (projectRecordSharedDirPath).
//
// The workspace's .scion entry (a marker file, or a directory holding a
// project-id file) is consulted only as a consistency check: when it records a
// project identity, the shared dir that identity resolves to must equal the
// record-derived path, otherwise errSharedDirProjectRecordMismatch is
// returned. A workspace that records no identity yet uses the record-derived
// path.
func resolveHubProjectSharedDirPath(project *store.Project, dirName string) (string, error) {
	expected, err := projectRecordSharedDirPath(project, dirName)
	if err != nil {
		return "", err
	}
	workspacePath, err := localProjectPath(project.Slug)
	if err != nil {
		return "", err
	}
	scionPath := filepath.Join(workspacePath, config.DotScion)
	hasIdentity, err := workspaceRecordsProjectIdentity(scionPath)
	if err != nil {
		return "", errSharedDirProjectRecordMismatch
	}
	if !hasIdentity {
		return expected, nil
	}
	projectDir, _, err := config.ResolveProjectPath(scionPath)
	if err != nil {
		return "", errSharedDirProjectRecordMismatch
	}
	recorded, err := config.GetSharedDirPath(projectDir, dirName)
	if err != nil || filepath.Clean(recorded) != expected {
		return "", errSharedDirProjectRecordMismatch
	}
	return expected, nil
}

// workspaceRecordsProjectIdentity reports whether the .scion entry at
// scionPath records a project identity: either it is a marker file, or it is
// a directory holding a project-id. Only a project-id that does not exist
// counts as no identity; an empty project-id or any other error reading it is
// returned as an error.
func workspaceRecordsProjectIdentity(scionPath string) (bool, error) {
	if config.IsProjectMarkerFile(scionPath) {
		return true, nil
	}
	id, err := config.ReadProjectID(scionPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if id == "" {
		return false, errors.New("empty project-id")
	}
	return true, nil
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
func (s *Server) handleProjectWorkspacePull(w http.ResponseWriter, r *http.Request, project *store.Project) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
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

	if !project.IsSharedWorkspace() {
		Conflict(w, "Pull is only available for shared-workspace git projects")
		return
	}

	// Pull from the project's clone URL as the hub knows it, not from the
	// remote configured in the workspace, which agents can change. The
	// fetch runs on the hub host: the URL is checked before any workspace
	// or git work and refused (never rewritten) unless it is an https, ssh
	// or git URL, as for the clone.
	cloneURL := resolveCloneURL(project.Labels[store.LabelCloneURL], project.GitRemote)
	localFixture := localCloneFixtureAllowed(cloneURL)
	if !localFixture {
		if err := provision.CheckHostCloneURL(cloneURL); err != nil {
			if !writeSharedCloneSourceRefused(w, err) {
				InternalError(w)
			}
			return
		}
	}

	resolved, err := s.resolveProjectWorkspace(ctx, project)
	if err != nil {
		if !writeWorkspaceStorageUnavailable(w, err) {
			InternalError(w)
		}
		return
	}
	// The workspace can be agent-writable: confirm through the confined
	// open (the anchored O_NOFOLLOW walk on a shared backend; no symlinked
	// base locally) that it resolves to the project's own directory before
	// git runs in it. git's own safe.directory check stays in force.
	root, err := resolved.Open(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			Conflict(w, "The shared workspace has not been provisioned yet")
			return
		}
		slog.WarnContext(ctx, "shared workspace pull: workspace not accessible",
			"project_id", project.ID, "error", err)
		BadRequest(w, "Workspace not accessible")
		return
	}
	_ = root.Close()
	workspacePath := resolved.HostPath

	// The fetch runs hardened, and the project's token goes only to its own
	// recorded remote (hostGitAccess).
	var pullResult *util.PullResult
	if localFixture {
		pullResult, err = util.PullSharedWorkspace(workspacePath, cloneURL, "")
	} else {
		token, hardening := s.hostGitAccess(ctx, project, cloneURL)
		pullResult, err = util.PullSharedWorkspaceHardened(workspacePath, cloneURL, token, hardening)
	}
	if err != nil {
		slog.Warn("shared workspace pull failed",
			"project_id", project.ID, "error", err.Error())

		// git's text can name host paths (and the safe.directory guidance
		// does): the response carries fixed text by kind, and the detail
		// stays in the log.
		statusCode := http.StatusConflict
		errorCode := ErrCodePullFailed
		message := "git pull failed in the shared workspace"
		var details map[string]interface{}
		var gitErr *util.GitError
		if errors.As(err, &gitErr) {
			if guidance := gitErr.UserGuidance(); guidance != "" {
				details = map[string]interface{}{"guidance": guidance}
			}
			switch gitErr.Kind {
			case util.GitErrAuth:
				statusCode = http.StatusUnauthorized
				message = "git pull failed: the repository did not accept the project's git credential"
			case util.GitErrNetwork:
				statusCode = http.StatusBadGateway
				message = "git pull failed: the repository could not be reached"
			case util.GitErrNotFound:
				message = "git pull failed: repository or branch not found"
			case util.GitErrNonFastForward:
				message = "git pull failed: the workspace has local commits that diverge from the remote"
			}
		}
		writeError(w, statusCode, errorCode, message, details)
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

// isProjectWorkspaceSubPath reports whether a project subpath addresses the
// project workspace. Membership in this set is what routes a request through
// handleProjectWorkspaceRoutes, and therefore through its authorization gate.
func isProjectWorkspaceSubPath(subPath string) bool {
	return subPath == "workspace" ||
		strings.HasPrefix(subPath, "workspace/")
}

// projectWorkspaceAction maps an HTTP method onto the permission a workspace
// request needs.
//
// Anything that is not a plain read is treated as a write. That is deliberately
// the blunt choice: a verb this function has never heard of is far more likely
// to be a mutation than a read. Being wrong in the restrictive direction
// returns a 403 to someone who should have been allowed; being wrong in the
// permissive direction lets a read-only principal change the workspace.
// Reclassifying a verb as a read is a one-line change here and in
// TestProjectWorkspaceAction.
func projectWorkspaceAction(method string) Action {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ActionRead
	default:
		return ActionUpdate
	}
}

// handleProjectWorkspaceRoutes is the single authorized entry point for every
// project workspace subtree.
//
// The gate below is the whole point of this function: it runs before the switch,
// so it cannot be bypassed by any arm of the switch, and a new arm added later
// is gated by construction rather than by the author remembering.
func (s *Server) handleProjectWorkspaceRoutes(w http.ResponseWriter, r *http.Request, projectID, subPath string) {
	project, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: CheckAccess — authorize this specific project before any
	// workspace path is resolved, opened, listed, read, written or deleted.
	// Without this, any authenticated caller reaches another project's files:
	// the route is classified RoutePolicy, which passes through unconditionally
	// and delegates enforcement here.
	if !s.authorize(w, r, projectResource(project), projectWorkspaceAction(r.Method)) {
		return
	}

	switch {
	case subPath == "workspace/pull":
		s.handleProjectWorkspacePull(w, r, project)

	case subPath == "workspace/archive":
		s.handleProjectWorkspaceArchive(w, r, project)

	case strings.HasPrefix(subPath, "workspace/files"):
		filePath := strings.TrimPrefix(subPath, "workspace/files")
		filePath = strings.TrimPrefix(filePath, "/")
		s.handleProjectWorkspace(w, r, project, filePath)

	default:
		// Reached only for a workspace subpath with no handler, e.g.
		// "workspace" alone or "workspace/nope". Previously these fell through
		// to handleProjectByIDInternal, which answered with this same 404.
		NotFound(w, "Project resource")
	}
}
