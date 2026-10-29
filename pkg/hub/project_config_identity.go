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
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project configuration identity on shared backends: the hub itself ensures that every project's
// <ProjectRoot>/.scion holds project-id and project-slug, at project create,
// on slug rename and in a bounded background pass at hub start, so a broker
// is never presented with an identity-less project configuration, whether
// or not the storage migration has run.
//
// Rules: idempotent; the directory is created only through the confined
// EnsureProjectConfigDir path; a missing project-id is written; a differing
// project-id is never overwritten: it is reported as an identity mismatch
// (log line and the project_config_identity health check), not repaired;
// project-slug is content and is rewritten to the hub's current slug.

// projectConfigIdentityHealthKey is the non-critical health check that
// reports project configuration identity mismatches.
const projectConfigIdentityHealthKey = "project_config_identity"

// projectConfigIdentityTimeout bounds the startup pass. A variable so
// tests can shorten it.
var projectConfigIdentityTimeout = 5 * time.Minute

// projectIDFileMax bounds the bytes read from a project-id file.
const projectIDFileMax = 4 << 10

// identityOutcome is the result of ensuring one project's identity files.
type identityOutcome string

const (
	identityEnsured  identityOutcome = "ensured"  // files present and matching (possibly just written)
	identityMismatch identityOutcome = "mismatch" // project-id differs; nothing was changed
)

// projectConfigIdentityState records the projects whose configuration
// carries a differing project-id, for the health check.
type projectConfigIdentityState struct {
	mu         sync.Mutex
	mismatched map[string]bool
}

func (st *projectConfigIdentityState) set(projectID string, mismatch bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if mismatch {
		if st.mismatched == nil {
			st.mismatched = map[string]bool{}
		}
		st.mismatched[projectID] = true
		return
	}
	delete(st.mismatched, projectID)
}

func (st *projectConfigIdentityState) list() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	ids := make([]string, 0, len(st.mismatched))
	for id := range st.mismatched {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ensureProjectConfigIdentity ensures <ProjectRoot>/.scion/project-id and
// project-slug for project on a shared backend (see the file comment). It
// returns identityMismatch, with nothing changed, when project-id names
// another project. Errors carry no host path beyond what the confined
// helpers report.
func (s *Server) ensureProjectConfigIdentity(project *store.Project) (identityOutcome, error) {
	layout, err := s.workspaceLayout()
	if err != nil {
		return "", err
	}
	dir, err := layout.EnsureProjectConfigDir(project.ID)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	recorded, err := readRootProjectID(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := writeRootFile(root, projectkeys.ProjectIDFile, project.ID+"\n"); err != nil {
			return "", fmt.Errorf("write project-id: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("read project-id: %w", err)
	case recorded != project.ID:
		s.projectConfigIdentity.set(project.ID, true)
		s.projectsLogger().Error("project configuration identity mismatch: project-id names another project; left unchanged",
			"project_id", project.ID, "recorded_project_id", recorded)
		return identityMismatch, nil
	}
	s.projectConfigIdentity.set(project.ID, false)

	slug, err := readRootSmallFile(root, projectkeys.ProjectSlugFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read project-slug: %w", err)
	}
	if err != nil || slug != project.Slug {
		if err := writeProjectSlugFile(root, project.Slug); err != nil {
			return "", fmt.Errorf("write project-slug: %w", err)
		}
	}
	return identityEnsured, nil
}

// readRootProjectID reads the trimmed project-id in root.
func readRootProjectID(root *os.Root) (string, error) {
	return readRootSmallFile(root, projectkeys.ProjectIDFile)
}

// readRootSmallFile reads a one-line file in root (bounded), trimmed. A
// symbolic link leading out of root is refused by os.Root.
func readRootSmallFile(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, projectIDFileMax))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// startProjectConfigIdentityEnsure runs ensureProjectConfigIdentities in
// the background at hub start; it never blocks start or readiness.
func (s *Server) startProjectConfigIdentityEnsure(ctx context.Context) {
	if !s.workspaceBackendShared() {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("project configuration identity: recovered from panic", "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, projectConfigIdentityTimeout)
		defer cancel()
		s.ensureProjectConfigIdentities(ctx)
	}()
}

// projectConfigIdentityCounts summarizes one ensureProjectConfigIdentities pass.
type projectConfigIdentityCounts struct {
	ensured, mismatched, failed, notReached int
	// workspacesProvisioned and workspacesFailed count the hub-native
	// workspace leaves the pass provisioned (backfillHubNativeWorkspace).
	workspacesProvisioned, workspacesFailed int
}

// ensureProjectConfigIdentities ensures the identity files of every
// project on a shared backend and logs one summary line. A project that
// fails is logged by ID and retried on the next hub start.
func (s *Server) ensureProjectConfigIdentities(ctx context.Context) projectConfigIdentityCounts {
	var counts projectConfigIdentityCounts
	if !s.workspaceBackendShared() {
		return counts
	}
	logger := s.projectsLogger()
	cursor := ""
	total, seen := 0, 0
	for {
		result, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor})
		if err != nil {
			if ctx.Err() != nil {
				counts.notReached = total - seen
				break
			}
			logger.Warn("project configuration identity: failed to list projects", "error", err.Error())
			counts.failed++
			break
		}
		if cursor == "" {
			total = result.TotalCount
		}
		for i := range result.Items {
			if ctx.Err() != nil {
				counts.notReached = max(total-seen, len(result.Items)-i)
				logger.Warn("project configuration identity: pass stopped before all projects were reached",
					"ensured", counts.ensured, "mismatched", counts.mismatched, "failed", counts.failed, "not_reached", counts.notReached)
				return counts
			}
			seen++
			project := &result.Items[i]
			outcome, err := s.ensureProjectConfigIdentity(project)
			switch {
			case err != nil:
				counts.failed++
				logger.Warn("project configuration identity: could not ensure the identity files",
					"project_id", project.ID, "error", sanitizedStorageError(err))
			case outcome == identityMismatch:
				counts.mismatched++
			default:
				counts.ensured++
				// A hub-native project whose workspace leaf is absent gets it
				// now (best-effort; ids only in the log). Never for a git
				// project, and never next to a mismatched identity.
				switch created, err := s.backfillHubNativeWorkspace(ctx, project); {
				case err != nil:
					counts.workspacesFailed++
					logger.Warn("project workspace: could not provision the hub-native workspace leaf", "project_id", project.ID)
				case created:
					counts.workspacesProvisioned++
					logger.Info("project workspace: provisioned the hub-native workspace leaf", "project_id", project.ID)
				}
			}
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	logger.Info("project configuration identity: pass finished",
		"ensured", counts.ensured, "mismatched", counts.mismatched, "failed", counts.failed, "not_reached", counts.notReached,
		"workspaces_provisioned", counts.workspacesProvisioned, "workspaces_failed", counts.workspacesFailed)
	return counts
}

// backfillHubNativeWorkspace provisions the workspace leaf of an existing
// hub-native project on a shared backend when the leaf is absent, with the
// create-time provisioning (provisionHubNativeWorkspace). It reports whether
// it provisioned one. A present leaf is left alone, whatever it holds; a git
// project, a linked project, an empty-per-agent project and the global
// project get nothing (hubNativeWorkspaceProject).
func (s *Server) backfillHubNativeWorkspace(ctx context.Context, project *store.Project) (bool, error) {
	if !s.hubNativeWorkspaceProject(ctx, project) {
		return false, nil
	}
	res, err := s.projectWorkspaceResolved(project)
	if err != nil {
		return false, err
	}
	base, err := res.ResolvedHostBase()
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(filepath.Join(base, res.RelPath)); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := s.provisionSharedWorkspace(ctx, project); err != nil {
		return false, err
	}
	return true, nil
}

// checkProjectConfigIdentityHealth reports identity mismatches as a
// non-critical (degraded) check on shared backends. The health endpoint is
// unauthenticated, so it carries only a count; the project IDs are in the
// Error log line ensureProjectConfigIdentity writes.
func (s *Server) checkProjectConfigIdentityHealth(checks map[string]string) {
	if !s.workspaceBackendShared() {
		return
	}
	n := len(s.projectConfigIdentity.list())
	if n == 0 {
		checks[projectConfigIdentityHealthKey] = HealthStatusHealthy
		return
	}
	checks[projectConfigIdentityHealthKey] = fmt.Sprintf("%s: project-id mismatch in %d project configuration(s)", HealthStatusDegraded, n)
}
