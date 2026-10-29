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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/templateimport"
	"github.com/GoogleCloudPlatform/scion/pkg/ghauth"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// resourceImportConcurrency bounds how many resources import in parallel within
// one request (Phase 4). Each resource is an independent DB row + storage
// prefix, so the per-resource loop is embarrassingly parallel; imports are
// rarely more than ~a dozen items (design Q7), so a small bound is both safe and
// sufficient. Per-resource file uploads parallelize independently
// (fileUploadConcurrency).
const resourceImportConcurrency = 6

// resource_import.go is the Phase-1 landing of the resource-import refactor: a
// single kind-generic, source-generic import driver that sits *above* the
// shared ResourceStore.Bootstrap and *replaces* the four near-identical
// import*From{Remote,Workspace} functions that templates and harness-configs
// used to carry separately.
//
// The per-kind quirks stay in small closures bundled in resourceImportKind
// (which marker file identifies a resource directory, and which ResourceStore
// persists it); everything else — remote fetch + auth, discovery, leaf-vs-parent
// naming, the create-or-sync loop — is shared here.

// resourceDir pairs a derived resource name with its on-disk directory.
type resourceDir struct {
	name, path, sourceURL string
	// nameFromConfig is set when name was read from the directory's
	// config.yaml before collection; the import then requires the collected
	// config.yaml to carry the same name.
	nameFromConfig bool
}

// skippedDir pairs a child directory name with the reason it was skipped during
// discovery (e.g. it lacks the kind's marker file, so it is not a resource).
type skippedDir struct{ name, reason string }

// ResourceImportEventType enumerates the lifecycle events emitted during an
// import (see ResourceImportEvent).
type ResourceImportEventType string

const (
	// ImportEventDiscovered is emitted once after discovery with the total count
	// and the names of all resources that will be imported.
	ImportEventDiscovered ResourceImportEventType = "discovered"
	// ImportEventStarted is emitted when a worker begins importing a resource.
	ImportEventStarted ResourceImportEventType = "started"
	// ImportEventCompleted is emitted when a resource finishes importing.
	ImportEventCompleted ResourceImportEventType = "completed"
	// ImportEventFailed is emitted when a discovered resource fails to import.
	ImportEventFailed ResourceImportEventType = "failed"
	// ImportEventSkipped is emitted for a child folder that is not a resource of
	// the kind (no marker file); these do not count toward the total.
	ImportEventSkipped ResourceImportEventType = "skipped"
	// ImportEventDone is the final event carrying the imported/skipped/failed
	// name lists.
	ImportEventDone ResourceImportEventType = "done"
	// ImportEventError is emitted when the import fails before reaching the
	// per-resource phase (e.g. fetch failure, nothing found). Carries Reason.
	ImportEventError ResourceImportEventType = "error"
)

// ResourceImportEvent is one progress event emitted during a resource import.
// Events are serialized to NDJSON on the streaming import endpoints; the field
// set used depends on Type (see the constants above).
type ResourceImportEvent struct {
	Type ResourceImportEventType `json:"type"`
	// Name is the resource (or skipped folder) the event concerns; unset on
	// discovered/done/error.
	Name string `json:"name,omitempty"`
	// Reason carries the failure/skip explanation on failed/skipped/error.
	Reason string `json:"reason,omitempty"`
	// Code is the machine-readable error code on an error event, when the
	// failure has one: harness_config_unusable for a refused harness-config,
	// matching the non-streaming import answer.
	Code string `json:"code,omitempty"`
	// Completed is the monotonic count of finished resources (any terminal
	// status) at the time of the event; set on completed/failed.
	Completed int `json:"completed,omitempty"`
	// Total is the number of resources to import (excludes skipped folders); set
	// on discovered/completed/failed.
	Total int `json:"total,omitempty"`
	// Names lists the resources to import; set on discovered.
	Names []string `json:"names,omitempty"`
	// Imported/Skipped/Failed are the final name lists; set on done.
	Imported []string `json:"imported,omitempty"`
	Skipped  []string `json:"skipped,omitempty"`
	Failed   []string `json:"failed,omitempty"`
}

// importProgressFunc receives import progress events. It may be nil (the
// non-streaming JSON callers pass nil and rely on the returned name slice).
// Implementations must be safe to call from multiple goroutines, since the
// per-resource loop may run concurrently (the streaming handler serializes
// writes through a mutex).
type importProgressFunc func(ResourceImportEvent)

// emit invokes progress if it is non-nil.
func emit(progress importProgressFunc, ev ResourceImportEvent) {
	if progress != nil {
		progress(ev)
	}
}

// resourceImportKind bundles the per-kind knobs the shared import driver needs.
// Construct one via Server.templateImportKind / Server.harnessConfigImportKind.
type resourceImportKind struct {
	// noun names the kind in log lines and "no scion <noun> found" errors
	// (e.g. "templates", "harness-configs").
	noun string
	// marker names the kind's marker file (scion-agent.yaml / config.yaml); used
	// in skipped-folder reasons.
	marker string
	// isResourceDir reports whether a directory is a resource of this kind, by
	// checking for the kind's marker file (scion-agent.yaml / config.yaml).
	isResourceDir func(dir string) bool
	// newStore builds the ResourceStore that persists a directory of this kind.
	// For harness-configs this loads config.yaml to resolve the harness type, so
	// it can fail; failures cause that directory to be skipped.
	newStore func(dir string) (*ResourceStore, error)
	// checkContent, when set, validates the content of every directory
	// selected for import before any directory is persisted. A non-nil error
	// refuses the whole import, so a refused import or reimport writes
	// nothing. It reports every refused directory, not just the first.
	checkContent func(dirs []resourceDir) error
}

// templateImportKind returns the import knobs for templates.
func (s *Server) templateImportKind() resourceImportKind {
	return resourceImportKind{
		noun:          "templates",
		marker:        "scion-agent.yaml",
		isResourceDir: templateimport.IsScionTemplate,
		newStore:      func(string) (*ResourceStore, error) { return s.templateStore(), nil },
	}
}

// harnessConfigImportKind returns the import knobs for harness-configs.
func (s *Server) harnessConfigImportKind() resourceImportKind {
	return resourceImportKind{
		noun:          "harness-configs",
		marker:        "config.yaml",
		isResourceDir: isHarnessConfigDir,
		newStore: func(dir string) (*ResourceStore, error) {
			hcDir, err := loadLocalHarnessConfigDir(dir)
			if err != nil {
				return nil, err
			}
			return s.harnessConfigStore(hcDir.Config.Harness), nil
		},
		checkContent: checkHarnessConfigDirsProvisioner,
	}
}

// importFromRemote fetches a remote source URL, discovers resources of the given
// kind within it, and create-or-syncs each into the store under the project
// scope. Returns the names of all resources imported or updated. When progress
// is non-nil, lifecycle events are emitted as the import proceeds.
//
// projectID is the scope ID: the project ID, the user ID for user scope, or ""
// for global scope. The fetch authenticates with that scope's credentials (see
// importCredential).
//
// When nameFilter is non-nil and non-empty, only resources whose names appear in
// the filter are imported; all others are skipped. When nil or empty, all
// discovered resources are imported (backward compatible).
func (s *Server) importFromRemote(ctx context.Context, projectID, sourceURL, scope string, kind resourceImportKind, progress importProgressFunc, nameFilter []string) ([]string, error) {
	return s.importFromRemoteSelected(ctx, projectID, sourceURL, scope, kind, progress,
		func(dirs []resourceDir, skipped []skippedDir) ([]resourceDir, []skippedDir) {
			return applyNameFilter(dirs, skipped, nameFilter)
		}, fmt.Sprintf("no scion %s matched the requested names", kind.noun))
}

// importFromRemoteSelected is importFromRemote with a caller-supplied selection
// of the discovered dirs; dirs it drops must be appended to skipped. noMatch is
// the error message returned when the selection keeps no dirs.
func (s *Server) importFromRemoteSelected(ctx context.Context, projectID, sourceURL, scope string, kind resourceImportKind, progress importProgressFunc, selectDirs func([]resourceDir, []skippedDir) ([]resourceDir, []skippedDir), noMatch string) ([]string, error) {
	if !config.IsRemoteURI(sourceURL) {
		return nil, fmt.Errorf("source must be a remote URI (http://, https://, or rclone)")
	}
	if s.GetStorage() == nil {
		return nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	cachePath, err := s.fetchRemoteForImport(ctx, scope, projectID, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch remote %s: %w", kind.noun, err)
	}
	defer func() { _ = os.RemoveAll(cachePath) }()

	dirs, skipped, err := discoverResourceDirs(cachePath, sourceURL, kind)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion %s found at %s", kind.noun, sourceURL)
	}

	dirs, skipped = selectDirs(dirs, skipped)
	if len(dirs) == 0 {
		return nil, errors.New(noMatch)
	}

	if err := checkResourceDirsContent(dirs, kind); err != nil {
		return nil, err
	}

	return s.importResourceDirs(ctx, dirs, skipped, scope, projectID, kind, progress), nil
}

// importFromWorkspace imports resources of the given kind from a path within the
// project's workspace filesystem. workspacePath is relative to the project's
// workspace root (e.g. "/.scion/templates"). When progress is non-nil,
// lifecycle events are emitted as the import proceeds.
//
// When nameFilter is non-nil and non-empty, only resources whose names appear in
// the filter are imported; all others are skipped. When nil or empty, all
// discovered resources are imported (backward compatible).
func (s *Server) importFromWorkspace(ctx context.Context, project *store.Project, workspacePath, scope string, kind resourceImportKind, progress importProgressFunc, nameFilter []string) ([]string, error) {
	if s.GetStorage() == nil {
		return nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	// Snapshot the workspace path through a confined root (the workspace
	// can be agent-writable; see snapshotWorkspaceDir).
	resourcesDir, cleanup, err := s.snapshotWorkspaceDir(ctx, project, workspacePath)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Workspace dirs are real directories, so pass "" as sourceURL: the leaf's
	// own base name is correct (no content-hash cache directory in play).
	dirs, skipped, err := discoverResourceDirs(resourcesDir, "", kind)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion %s found at workspace path %s", kind.noun, workspacePath)
	}

	dirs, skipped = applyNameFilter(dirs, skipped, nameFilter)
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion %s matched the requested names", kind.noun)
	}

	if err := checkResourceDirsContent(dirs, kind); err != nil {
		return nil, err
	}

	return s.importResourceDirs(ctx, dirs, skipped, scope, project.ID, kind, progress), nil
}

// checkResourceDirsContent runs the kind's content check over every directory
// selected for import. It runs before importResourceDirs so that a refusal
// persists nothing.
func checkResourceDirsContent(dirs []resourceDir, kind resourceImportKind) error {
	if kind.checkContent == nil {
		return nil
	}
	return kind.checkContent(dirs)
}

// fetchRemoteForImport fetches a remote source URL to a local cache directory,
// authenticating against GitHub with the credential importCredential selects
// for the import's scope ("global", "project" or "user") and scopeID. It
// applies to both resource kinds (templates and harness-configs) and to skill
// directory discovery. The caller owns the returned cache path and must
// remove it.
func (s *Server) fetchRemoteForImport(ctx context.Context, scope, scopeID, sourceURL string) (string, error) {
	return config.FetchRemoteTemplate(ctx, sourceURL, s.importCredential(ctx, scope, scopeID, sourceURL))
}

// importCredential returns the GitHub credential for fetching sourceURL into
// the given resource scope, or "" to fetch unauthenticated. Each scope reads
// only its own credentials:
//
//   - project (scopeID is the project ID): the repo-specific project credential
//     GH_{OWNER}__{REPO}, then the owner-level GH_{OWNER} (see pkg/ghauth),
//     then a GitHub App installation token for the project, if it has one,
//     then the project's GITHUB_TOKEN credential. This mirrors the skill
//     resolver's project-scope cascade.
//   - global (hub-level; scopeID is ignored): the same names as hub credentials,
//     GH_{OWNER}__{REPO}, GH_{OWNER}, then GITHUB_TOKEN. There is no GitHub
//     App step: App installations belong to projects.
//   - user (scopeID is the user ID): the same names as the user's own
//     credentials, GH_{OWNER}__{REPO}, GH_{OWNER}, then GITHUB_TOKEN, and only
//     when the caller in ctx is that user, so an import never spends another
//     user's credential.
//
// Other sources (archives, even on github.com, rclone and other hosts) get
// "": FetchRemoteTemplate sends a credential only for GitHub folders, so
// neither an App token is minted nor a credential read for them. Credential
// values are never logged; only the credential name is.
func (s *Server) importCredential(ctx context.Context, scope, scopeID, sourceURL string) string {
	owner, repo, ok := config.GitHubRepoForURL(sourceURL)
	if !ok {
		return ""
	}
	// scope is a template or a harness-config scope: harness-config reimport
	// passes store.HarnessConfigScope*, whose values equal the TemplateScope*
	// ones matched here (pinned by TestImportCredential_ScopeConstantsAgree).
	switch scope {
	case store.TemplateScopeProject:
		if scopeID == "" {
			return ""
		}
		return s.projectImportCredential(ctx, scopeID, owner, repo)
	case store.TemplateScopeGlobal:
		return s.scopedImportCredential(ctx, secret.ScopeHub, s.HubID(), owner, repo)
	case store.TemplateScopeUser:
		user := GetUserIdentityFromContext(ctx)
		if scopeID == "" || user == nil || user.ID() != scopeID {
			return ""
		}
		return s.scopedImportCredential(ctx, secret.ScopeUser, scopeID, owner, repo)
	}
	return ""
}

// projectImportCredential is the project-scope cascade of importCredential.
func (s *Server) projectImportCredential(ctx context.Context, projectID, owner, repo string) string {
	if val, key := s.ghConventionCredential(ctx, secret.ScopeProject, projectID, owner, repo); val != "" {
		s.resourceLog.Info("using project credential for resource import", "projectID", projectID, "secret", key)
		return val
	}

	// Prefer a GitHub App installation token if the project has one.
	project, err := s.store.GetProject(ctx, projectID)
	if err == nil && project != nil && project.GitHubInstallationID != nil {
		if token, _, mintErr := s.MintGitHubAppTokenForProject(ctx, project); mintErr == nil && token != "" {
			return token
		}
	}

	// Fall back to the project GITHUB_TOKEN credential if no App token was minted.
	if val := s.credentialForImport(ctx, secret.ScopeProject, projectID, ghauth.DefaultKey); val != "" {
		s.resourceLog.Info("using project credential for resource import", "projectID", projectID, "secret", ghauth.DefaultKey)
		return val
	}
	return ""
}

// scopedImportCredential is the hub- and user-scope cascade of
// importCredential: GH_{OWNER}__{REPO}, GH_{OWNER}, then GITHUB_TOKEN, all
// read from credScope/scopeID.
func (s *Server) scopedImportCredential(ctx context.Context, credScope, scopeID, owner, repo string) string {
	val, key := s.ghConventionCredential(ctx, credScope, scopeID, owner, repo)
	if val == "" {
		val, key = s.credentialForImport(ctx, credScope, scopeID, ghauth.DefaultKey), ghauth.DefaultKey
	}
	if val == "" {
		return ""
	}
	s.resourceLog.Info("using credential for resource import", "scope", credScope, "scopeID", scopeID, "secret", key)
	return val
}

// ghConventionCredential returns the first GH_ convention credential set for
// owner/repo in credScope/scopeID, and its name (see ghauth.Lookup).
func (s *Server) ghConventionCredential(ctx context.Context, credScope, scopeID, owner, repo string) (value, name string) {
	return ghauth.Lookup(owner, repo, func(key string) string {
		return s.credentialForImport(ctx, credScope, scopeID, key)
	})
}

// credentialForImport returns the value of the credential named key in
// credScope/scopeID, or "" if it is unset, empty, or no credential backend is
// configured. A lookup failure other than not-found is logged (without any
// value) and treated as unset, so the cascade falls through to the next
// source.
func (s *Server) credentialForImport(ctx context.Context, credScope, scopeID, key string) string {
	sb := s.GetSecretBackend()
	if sb == nil {
		return ""
	}
	sec, err := sb.Get(ctx, key, credScope, scopeID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.resourceLog.Warn("Failed to retrieve credential for resource import", "scope", credScope, "scopeID", scopeID, "secret", key, "error", err)
		}
		return ""
	}
	if sec == nil {
		return ""
	}
	return sec.Value
}

// discoverResourceDirs returns the resource directories at root, classifying by
// the kind's marker file (via isResourceDir). It owns both the leaf-vs-parent
// decision and per-branch naming:
//
//   - Leaf: root itself is a resource. Its name comes from the source URL's leaf
//     segment when sourceURL is set (the remote cache dir is a content hash, so
//     filepath.Base(root) would be the hash); for workspace imports pass
//     sourceURL == "" so the real directory's base name is used.
//   - Parent: root's immediate children are scanned; each child that is a
//     resource is named by its own directory name. Children without the marker
//     file are returned as skipped (reported to the user) rather than imported.
func discoverResourceDirs(root, sourceURL string, kind resourceImportKind) ([]resourceDir, []skippedDir, error) {
	if kind.isResourceDir(root) {
		name := filepath.Base(root)
		if sourceURL != "" {
			if derived := config.DeriveResourceName(sourceURL); derived != "" {
				name = derived
			}
		}
		nameFromConfig := false
		if kind.marker == "config.yaml" {
			if hcDir, err := loadLocalHarnessConfigDir(root); err == nil && hcDir.Config.Name != "" {
				name = hcDir.Config.Name
				nameFromConfig = true
			}
		}
		return []resourceDir{{name: name, path: root, sourceURL: sourceURL, nameFromConfig: nameFromConfig}}, nil, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var dirs []resourceDir
	var skipped []skippedDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Skip hidden/system directories (e.g. .git, .github, .scion). These are
		// never resources and reporting them as "skipped (no marker)" would be
		// noise, so drop them silently.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if kind.isResourceDir(dir) {
			childName := entry.Name()
			nameFromConfig := false
			if kind.marker == "config.yaml" {
				if hcDir, err := loadLocalHarnessConfigDir(dir); err == nil && hcDir.Config.Name != "" {
					childName = hcDir.Config.Name
					nameFromConfig = true
				}
			}
			childSourceURL := sourceURL
			if sourceURL != "" {
				childSourceURL = strings.TrimRight(sourceURL, "/") + "/" + entry.Name()
			}
			dirs = append(dirs, resourceDir{name: childName, path: dir, sourceURL: childSourceURL, nameFromConfig: nameFromConfig})
		} else {
			skipped = append(skipped, skippedDir{entry.Name(), "no " + kind.marker})
		}
	}
	return dirs, skipped, nil
}

// importResourceDirs create-or-syncs each discovered directory into the store
// under the given scope, emitting progress events. Directories that fail to
// build a store (e.g. an unreadable harness config.yaml) or fail to import are
// logged and reported as failed; child folders lacking the marker file
// (skippedDirs from discovery) are reported as skipped. Returns the names
// successfully imported or updated.
//
// Each directory is force-synced (force=true): a re-import always re-uploads and
// reconciles storage, matching the prior direct-import behavior. For a
// not-yet-existing resource the force flag is irrelevant — Bootstrap creates it.
//
// The progress model separates the aggregate (a monotonic completed counter over
// total) from per-item detail so it stays correct under parallelism (Phase 4):
// completed is assigned as each item finishes, independent of order.
//
// The per-resource loop runs concurrently with a bounded worker pool
// (resourceImportConcurrency). Each worker writes its outcome into per-index
// slots, so the returned (and reported) imported/failed lists stay in discovery
// order; the completed counter is an atomic so the aggregate reported on each
// event is consistent across workers. Progress events themselves are serialized
// by the streaming handler's mutex (importProgressFunc is documented
// goroutine-safe), so emit may be called from any worker.
func (s *Server) importResourceDirs(ctx context.Context, dirs []resourceDir, skipped []skippedDir, scope, scopeID string, kind resourceImportKind, progress importProgressFunc) []string {
	total := len(dirs)
	names := make([]string, 0, total)
	for _, rd := range dirs {
		names = append(names, rd.name)
	}
	emit(progress, ResourceImportEvent{Type: ImportEventDiscovered, Total: total, Names: names})

	skippedNames := make([]string, 0, len(skipped))
	for _, sd := range skipped {
		skippedNames = append(skippedNames, sd.name)
		emit(progress, ResourceImportEvent{Type: ImportEventSkipped, Name: sd.name, Reason: sd.reason})
	}

	importedSlots := make([]string, total)
	failedSlots := make([]string, total)
	var completed atomic.Int64

	var g errgroup.Group
	g.SetLimit(resourceImportConcurrency)
	for i, rd := range dirs {
		i, rd := i, rd
		g.Go(func() error {
			// Stop starting new imports once the request is cancelled (e.g. the
			// client disconnected mid-stream); report the rest as failed so the
			// completed counter still reaches total.
			if ctxErr := ctx.Err(); ctxErr != nil {
				done := int(completed.Add(1))
				failedSlots[i] = rd.name
				emit(progress, ResourceImportEvent{
					Type: ImportEventFailed, Name: rd.name, Reason: ctxErr.Error(),
					Completed: done, Total: total,
				})
				return nil
			}

			emit(progress, ResourceImportEvent{Type: ImportEventStarted, Name: rd.name})

			rstore, err := kind.newStore(rd.path)
			if err == nil {
				if rd.nameFromConfig {
					rstore.requireConfigName()
				}
				_, err = rstore.Bootstrap(ctx, rd.name, rd.path, scope, scopeID, rd.sourceURL, true)
			}
			done := int(completed.Add(1))
			if err != nil {
				s.resourceLog.Warn(kind.noun+" import: failed to import resource, skipping",
					"name", rd.name, "error", err)
				failedSlots[i] = rd.name
				emit(progress, ResourceImportEvent{
					Type: ImportEventFailed, Name: rd.name, Reason: err.Error(),
					Completed: done, Total: total,
				})
				return nil
			}
			importedSlots[i] = rd.name
			emit(progress, ResourceImportEvent{
				Type: ImportEventCompleted, Name: rd.name,
				Completed: done, Total: total,
			})
			return nil
		})
	}
	_ = g.Wait()

	imported := compactNames(importedSlots)
	failed := compactNames(failedSlots)

	emit(progress, ResourceImportEvent{
		Type: ImportEventDone, Imported: imported, Skipped: skippedNames, Failed: failed,
	})
	return imported
}

// compactNames returns the non-empty entries of slots, preserving order. The
// parallel import loop writes each resource's name into its own slot (empty if
// it took the other branch), so compacting yields the imported/failed lists in
// discovery order.
func compactNames(slots []string) []string {
	out := make([]string, 0, len(slots))
	for _, n := range slots {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// applyNameFilter restricts dirs to only those whose names appear in filter.
// Filtered-out dirs are appended to skipped. When filter is nil or empty, dirs
// and skipped are returned unchanged (import-all behavior).
func applyNameFilter(dirs []resourceDir, skipped []skippedDir, filter []string) ([]resourceDir, []skippedDir) {
	if len(filter) == 0 {
		return dirs, skipped
	}
	allowed := make(map[string]struct{}, len(filter))
	for _, n := range filter {
		allowed[n] = struct{}{}
	}
	var filtered []resourceDir
	for _, d := range dirs {
		if _, ok := allowed[d.name]; ok {
			filtered = append(filtered, d)
		} else {
			skipped = append(skipped, skippedDir{d.name, "not in requested names"})
		}
	}
	return filtered, skipped
}

// applySlugFilter restricts dirs to those whose names slugify to slug, the
// key under which ResourceStore.Bootstrap persists them. Filtered-out dirs are
// appended to skipped.
func applySlugFilter(dirs []resourceDir, skipped []skippedDir, slug string) ([]resourceDir, []skippedDir) {
	var filtered []resourceDir
	for _, d := range dirs {
		if api.Slugify(d.name) == slug {
			filtered = append(filtered, d)
		} else {
			skipped = append(skipped, skippedDir{d.name, fmt.Sprintf("does not match slug %q", slug)})
		}
	}
	return filtered, skipped
}

// discoverFromRemote fetches a remote source URL, discovers resources of the
// given kind within it, and returns their names without importing. The caller
// receives the discovered names and skipped names for preview purposes. The
// fetch authenticates with the credentials of scope and scopeID (see
// importCredential).
func (s *Server) discoverFromRemote(ctx context.Context, scope, scopeID, sourceURL string, kind resourceImportKind) ([]string, []string, error) {
	if !config.IsRemoteURI(sourceURL) {
		return nil, nil, fmt.Errorf("source must be a remote URI (http://, https://, or rclone)")
	}
	if s.GetStorage() == nil {
		return nil, nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	cachePath, err := s.fetchRemoteForImport(ctx, scope, scopeID, sourceURL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch remote %s: %w", kind.noun, err)
	}
	defer func() { _ = os.RemoveAll(cachePath) }()

	dirs, skipped, err := discoverResourceDirs(cachePath, sourceURL, kind)
	if err != nil {
		return nil, nil, err
	}
	if len(dirs) == 0 {
		return nil, nil, fmt.Errorf("no scion %s found at %s", kind.noun, sourceURL)
	}

	names := make([]string, len(dirs))
	for i, d := range dirs {
		names[i] = d.name
	}
	skippedNames := make([]string, len(skipped))
	for i, sd := range skipped {
		skippedNames[i] = sd.name
	}
	return names, skippedNames, nil
}

// discoverFromWorkspace discovers resources of the given kind from a path within
// the project's workspace filesystem and returns their names without importing.
func (s *Server) discoverFromWorkspace(ctx context.Context, project *store.Project, workspacePath string, kind resourceImportKind) ([]string, []string, error) {
	if s.GetStorage() == nil {
		return nil, nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	resourcesDir, cleanup, err := s.snapshotWorkspaceDir(ctx, project, workspacePath)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()

	dirs, skipped, err := discoverResourceDirs(resourcesDir, "", kind)
	if err != nil {
		return nil, nil, err
	}
	if len(dirs) == 0 {
		return nil, nil, fmt.Errorf("no scion %s found at workspace path %s", kind.noun, workspacePath)
	}

	names := make([]string, len(dirs))
	for i, d := range dirs {
		names[i] = d.name
	}
	skippedNames := make([]string, len(skipped))
	for i, sd := range skipped {
		skippedNames[i] = sd.name
	}
	return names, skippedNames, nil
}

// Bounds of a workspace snapshot for resource import.
var (
	workspaceSnapshotMaxFiles = 10000
	workspaceSnapshotMaxBytes = int64(256 << 20)
)

// errWorkspaceSnapshotTooLarge refuses a workspace import over the bounds.
var errWorkspaceSnapshotTooLarge = errors.New("workspace path is too large to import")
