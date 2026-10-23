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

package runtimebroker

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// Hub-managed projects on a shared storage backend (design-v7 §P1a rule 1,
// ptone/scion#5823 option (a)).
//
// A hub-managed project dispatched by slug runs from the broker's Layout's
// project root, <host base>/<subpath root>/<project UUID>
// (projectfs.Layout.ProjectRoot), not from a broker-local
// ~/.scion/projects/<slug>: every broker of the project reads the same
// project configuration, <ProjectRoot>/.scion, which the hub seeds with
// settings.yaml, project-id and project-slug. The broker never writes that
// directory (it records its state for the workspace configuration check,
// keyed by the project root). Broker-private state stays broker-local:
// agent state dirs live in the external agents root
// ~/.scion/project-configs/<slug>__<id8>/.scion/agents for every sharing
// mode (config.AgentsRootForProject), and the ownership store and launch
// journal are keyed by project ID and agent, never by the project path.

// Refusal texts for a shared-backend dispatch (fs-cleanup-arch rulings Q6,
// Q8, Q9). They name no host path except the share's mount point, which is
// the operator's own configuration.
const (
	sharedProjectNoIDText     = "a hub-managed project on shared project storage needs the project ID; upgrade the Hub"
	sharedProjectConfigText   = "project configuration missing or does not match; run project-storage-migrate"
	sharedProjectNoSlugText   = "project configuration has no project-slug; run project-storage-migrate"
	sharedProjectUnmountedFmt = "project storage share is not mounted on this broker (%s; mount it or set nfs.auto_mount)"
)

// sharedHubProjectPath returns the project path of a hub-managed project
// dispatched by slug when the broker's Layout is shared: the Layout's
// project root for projectID, after checking that the share is mounted and
// that <ProjectRoot>/.scion is the hub-seeded configuration of this project
// with a project-slug. ok is false (and the caller keeps today's
// ~/.scion/projects/<slug>) when the Layout is not shared. A failed check is
// a *startContextError: 503 for an unmounted share, 409 otherwise. Nothing
// is created or repaired.
func (s *Server) sharedHubProjectPath(ctx context.Context, projectID string) (path string, ok bool, sce *startContextError) {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() {
		return "", false, nil
	}
	if projectID == "" {
		return "", true, &startContextError{Status: http.StatusConflict, Message: sharedProjectNoIDText}
	}
	root, err := layout.ProjectRoot(projectID)
	if err != nil {
		return "", true, &startContextError{Status: http.StatusConflict, Message: sharedProjectConfigText, OriginalErr: err}
	}
	if err := s.ensureLayoutShareMounted(ctx, layout); err != nil {
		s.agentLifecycleLog.Error("Project storage share is not mounted on this broker", "project_id", projectID,
			"host_base", layout.HostBase, "error", err)
		return "", true, &startContextError{Status: http.StatusServiceUnavailable, Code: ErrCodeProjectStorageUnavailable,
			Message: fmt.Sprintf(sharedProjectUnmountedFmt, layout.HostBase), OriginalErr: err}
	}
	if sce := checkSharedProjectConfig(root.HostPath, projectID); sce != nil {
		s.agentLifecycleLog.Error("Shared project configuration refused", "project_id", projectID,
			"path", root.HostPath, "reason", sce.Message)
		return "", true, sce
	}
	return root.HostPath, true, nil
}

// checkSharedProjectConfig checks that projectRoot is a directory (not a
// symlink) holding the hub-seeded configuration of projectID:
// <projectRoot>/.scion is a directory whose project-id names projectID
// (config.IsSharedProjectConfigDir) and that has a valid project-slug.
func checkSharedProjectConfig(projectRoot, projectID string) *startContextError {
	if info, err := os.Lstat(projectRoot); err != nil || !info.IsDir() {
		return &startContextError{Status: http.StatusConflict, Message: sharedProjectConfigText, OriginalErr: err}
	}
	scionDir := filepath.Join(projectRoot, config.DotScion)
	if filepath.Base(projectRoot) != projectID || !config.IsSharedProjectConfigDir(scionDir) {
		return &startContextError{Status: http.StatusConflict, Message: sharedProjectConfigText}
	}
	if _, err := config.ReadProjectSlug(scionDir); err != nil {
		return &startContextError{Status: http.StatusConflict, Message: sharedProjectNoSlugText, OriginalErr: err}
	}
	return nil
}

// ensureLayoutShareMounted checks that the Layout's share is mounted at its
// host base: for nfs through the broker's NFS mount reconciler when it has
// one (EnsureShareMounted, which mounts only with auto_mount on), otherwise
// by the host base being a directory. A broker without the mount refuses
// shared-backend dispatches (fs-cleanup-arch ruling Q6).
func (s *Server) ensureLayoutShareMounted(ctx context.Context, layout projectfs.Layout) error {
	if layout.Backend == projectfs.BackendNFS && s.nfsMountReconciler != nil {
		if err := s.nfsMountReconciler.EnsureShareMounted(ctx, layout.Share.ID); err != nil {
			return err
		}
	}
	info, err := os.Stat(layout.HostBase)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", layout.HostBase)
	}
	return nil
}

// isSharedHubProjectRoot reports whether projectPath is a hub-managed
// project root on a shared backend: its .scion is a shared project
// configuration (config.IsSharedProjectConfigDir: the UUID-keyed tree only
// the hub seeds).
func isSharedHubProjectRoot(projectPath string) bool {
	if projectPath == "" || filepath.Base(projectPath) == config.DotScion {
		return false
	}
	return config.IsSharedProjectConfigDir(filepath.Join(projectPath, config.DotScion))
}

// findAgentInSharedProject returns the configuration dir
// (<ProjectRoot>/.scion) of projectID's shared project root when the
// broker's Layout is shared and the agent has state in it (its external
// agents root, or its legacy in-project dir from before the move), else "".
// It only reads the filesystem; a lookup without a project ID finds
// nothing here (the root is keyed by ID).
func (s *Server) findAgentInSharedProject(agentName, projectID string) string {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() || projectID == "" || !isSingleCleanPathElement(agentName) {
		return ""
	}
	root, err := layout.ProjectRoot(projectID)
	if err != nil {
		return ""
	}
	scionDir := filepath.Join(root.HostPath, config.DotScion)
	if !config.IsSharedProjectConfigDir(scionDir) {
		return ""
	}
	if hubManagedProjectHasAgent(scionDir, agentName) || config.LegacySharedProjectAgentDir(scionDir, agentName) != "" {
		return scionDir
	}
	return ""
}

// sharedProjectsWithLocalAgents returns the configuration dirs
// (<ProjectRoot>/.scion) of the projects on the broker's shared Layout for
// which this broker keeps agent state (a non-empty external agents root),
// so start-up discovery reads their settings as it reads the broker-local
// hub-managed projects'. It lists the Layout's project tree once; nil when
// the Layout is not shared or the tree cannot be read.
func (s *Server) sharedProjectsWithLocalAgents() []string {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() {
		return nil
	}
	// The project tree is the parent of any project root; the Layout
	// validates and defaults the subpath root.
	probe, err := layout.ProjectRoot("00000000-0000-0000-0000-000000000000")
	if err != nil {
		return nil
	}
	tree := filepath.Dir(probe.HostPath)
	entries, err := os.ReadDir(tree)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		scionDir := filepath.Join(tree, e.Name(), config.DotScion)
		if !config.IsSharedProjectConfigDir(scionDir) {
			continue
		}
		root, err := config.AgentsRootForProject(scionDir, false, e.Name())
		if err != nil {
			continue
		}
		if agents, err := os.ReadDir(root); err == nil && len(agents) > 0 {
			out = append(out, scionDir)
		}
	}
	return out
}
