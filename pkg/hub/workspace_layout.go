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
	"errors"
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errWorkspaceStorageUnavailable is returned when the hub's workspace
// storage selects a shared backend it cannot build a Layout for (no NFS
// share, a volume backend without a volume name). Resolution fails closed:
// the hub never falls back to instance-local disk for a shared backend
// (ptone/scion#1073). HTTP handlers map it to 503
// (writeWorkspaceStorageUnavailable).
var errWorkspaceStorageUnavailable = errors.New("workspace storage is not available")

// errNotHubWorkspace is returned by projectWorkspaceResolved for a project
// whose workspace the hub does not manage (a linked project in a per-agent
// git mode); the caller resolves it through its providers instead.
var errNotHubWorkspace = errors.New("project workspace is not hub-managed")

// workspaceLayout builds the hub's workspace Layout from the in-memory
// ServerConfig.WorkspaceStorageConfig (never from the file-only global
// settings loader; design-v4 §P2):
//
//   - nfs: config.NFSLayout (HostBase <mount_root>/<share id>);
//   - cloudrun-volume / gke-shared-volume: the same UUID-keyed tree under
//     the volume's mount point (<volumeMountBase>/<volume name>), with the
//     block's subpath_root;
//   - unset or local: the local backend, HostBase ~/.scion, slug-keyed.
//
// A shared backend that cannot be built returns an error wrapping
// errWorkspaceStorageUnavailable, never a local Layout.
func (s *Server) workspaceLayout() (projectfs.Layout, error) {
	return workspaceLayoutFor(s.config.WorkspaceStorageConfig)
}

// workspaceLayoutFor is workspaceLayout for an explicit config.
func workspaceLayoutFor(wsCfg *config.V1WorkspaceStorageConfig) (projectfs.Layout, error) {
	backend := ""
	if wsCfg != nil {
		backend = wsCfg.Backend
	}
	switch backend {
	case "", config.WorkspaceStorageBackendLocal:
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return projectfs.Layout{}, fmt.Errorf("failed to get global dir: %w", err)
		}
		return projectfs.Layout{Backend: projectfs.BackendLocal, HostBase: globalDir}, nil
	case config.WorkspaceStorageBackendNFS:
		layout, err := config.NFSLayout(wsCfg.NFS)
		if err != nil {
			return projectfs.Layout{}, fmt.Errorf("%w: %v", errWorkspaceStorageUnavailable, err)
		}
		return layout, nil
	case "cloudrun-volume", "gke-shared-volume":
		var subPathRoot string
		pfsBackend := projectfs.BackendCloudRunVolume
		if backend == "cloudrun-volume" {
			if wsCfg.CloudRunVolume != nil {
				subPathRoot = wsCfg.CloudRunVolume.SubPathRoot
			}
		} else {
			pfsBackend = projectfs.BackendGKESharedVolume
			if wsCfg.GKESharedVolume != nil {
				subPathRoot = wsCfg.GKESharedVolume.SubPathRoot
			}
		}
		mountRoot := workspaceMountRoot(wsCfg)
		if mountRoot == "" {
			return projectfs.Layout{}, fmt.Errorf("%w: %s has no volume_name", errWorkspaceStorageUnavailable, backend)
		}
		root, err := config.ResolveSubPathRoot(subPathRoot)
		if err != nil {
			return projectfs.Layout{}, fmt.Errorf("%w: %v", errWorkspaceStorageUnavailable, err)
		}
		return projectfs.Layout{Backend: pfsBackend, HostBase: mountRoot, SubPathRoot: root}, nil
	default:
		return projectfs.Layout{}, fmt.Errorf("%w: unknown backend %q", errWorkspaceStorageUnavailable, backend)
	}
}

// workspaceBackendShared reports whether the hub's workspace storage is a
// shared backend (the UUID-keyed tree every broker of a project mounts).
// A misconfigured shared backend still counts as shared: it must never be
// treated as the local layout.
func (s *Server) workspaceBackendShared() bool {
	wsCfg := s.config.WorkspaceStorageConfig
	return wsCfg != nil && wsCfg.Backend != "" && wsCfg.Backend != config.WorkspaceStorageBackendLocal
}

// projectWorkspaceResolved resolves the workspace Mount of a hub-managed
// (no git remote) or shared-workspace git project through the layout
// authority: <HostBase>/<subpath_root>/<uuid>/workspace on a shared backend,
// ~/.scion/projects/<slug> on the local backend (unchanged).
//
// An empty-per-agent project has no project-scoped workspace Mount and
// gets projectfs.ErrNoMount. A project whose workspace the hub does not
// manage (linked per-agent git modes) gets errNotHubWorkspace.
func (s *Server) projectWorkspaceResolved(project *store.Project) (projectfs.Resolved, error) {
	if project == nil {
		return projectfs.Resolved{}, errNotHubWorkspace
	}
	if project.IsEmptyPerAgent() {
		return projectfs.Resolved{}, projectfs.ErrNoMount
	}
	if project.GitRemote != "" && !project.IsSharedWorkspace() {
		return projectfs.Resolved{}, errNotHubWorkspace
	}
	layout, err := s.workspaceLayout()
	if err != nil {
		return projectfs.Resolved{}, err
	}
	p := projectfs.Project{ID: project.ID, Slug: project.Slug}
	if layout.Backend == projectfs.BackendLocal {
		if err := validateProjectSlug(project.Slug); err != nil {
			return projectfs.Resolved{}, err
		}
		p.Dir = filepath.Join(layout.HostBase, "projects", project.Slug)
	}
	return layout.Resolve(p, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
}

// projectConfigDir returns the directory holding a hub-managed project's
// configuration (.scion) as the hub seeds it (design-v4 §P1a rule 1): on a
// shared backend <ProjectRoot(uuid)>/.scion, beside workspace/ and inside
// no Mount; on the local backend ~/.scion/projects/<slug>/.scion, which is
// both the project root and the workspace root (unchanged).
func (s *Server) projectConfigDir(project *store.Project) (string, error) {
	layout, err := s.workspaceLayout()
	if err != nil {
		return "", err
	}
	if layout.Backend.Shared() {
		root, err := layout.ProjectRoot(project.ID)
		if err != nil {
			return "", err
		}
		return filepath.Join(root.HostPath, ".scion"), nil
	}
	if err := validateProjectSlug(project.Slug); err != nil {
		return "", err
	}
	return filepath.Join(layout.HostBase, "projects", project.Slug, ".scion"), nil
}
