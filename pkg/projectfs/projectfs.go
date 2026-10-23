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

// Package projectfs is the single authority for where a project's storage
// lives (design fs-cleanup design-v4 §P1, ptone/scion#5805). It answers
// "host path / subPath / server path for (project, agent, Mount)" for every
// component, keyed by the project UUID on shared backends and by today's
// slug-keyed layout on the local backend.
//
// It is a leaf package: it imports only pkg/api and pkg/shareddirs (both
// leaves) and the standard library, never pkg/config, pkg/hub, pkg/agent,
// pkg/runtime or pkg/runtimebroker. Callers translate their own
// configuration into a Layout.
//
// Phase 1 covers the nfs and local backends, the workspace and shared-dir
// Mounts, and Resolved.Open for project-scoped directories. Homes,
// provisioning, deletion and the other backends come in later phases.
package projectfs

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// Backend is the storage backend of a Layout.
type Backend string

const (
	// BackendLocal is node-local disk with today's slug-keyed layout
	// (workstation server, single-node Cloud Run instance).
	BackendLocal Backend = "local"
	// BackendNFS is a shared NFS export mounted at HostBase, with the
	// UUID-keyed tree <SubPathRoot>/<projectID>/...
	BackendNFS Backend = "nfs"
	// BackendCloudRunVolume is a platform volume declared on a Cloud Run
	// service and mounted at HostBase (/mnt/<volume name>), with the same
	// UUID-keyed tree as nfs.
	BackendCloudRunVolume Backend = "cloudrun-volume"
	// BackendGKESharedVolume is a shared RWX volume mounted into GKE pods at
	// HostBase (/mnt/<volume name>), with the same UUID-keyed tree as nfs.
	BackendGKESharedVolume Backend = "gke-shared-volume"
)

// Shared reports whether the backend is storage shared by the hub and every
// broker of a project, laid out as the UUID-keyed tree
// <SubPathRoot>/<projectID>/... under HostBase.
func (b Backend) Shared() bool {
	switch b {
	case BackendNFS, BackendCloudRunVolume, BackendGKESharedVolume:
		return true
	}
	return false
}

// Durable reports whether data written to this backend survives the loss
// of the process's own instance (a Cloud Run instance recycle). Only shared
// backends are durable; local disk is not.
func (b Backend) Durable() bool {
	return b.Shared()
}

// Scope says whether a Mount is one directory for all agents of a project or
// one directory per agent.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeAgent   Scope = "agent"
)

// Owner says who manages a Mount's directory.
type Owner string

const (
	// OwnerScion: the directory lives in the project tree the authority
	// manages.
	OwnerScion Owner = "scion"
	// OwnerUser: a host path or bucket the user supplies (volumes: entries).
	OwnerUser Owner = "user"
)

// Provision says how a scion-owned directory is first populated.
type Provision string

const (
	ProvisionNone        Provision = "none"
	ProvisionEmpty       Provision = "empty"
	ProvisionGitClone    Provision = "git-clone"
	ProvisionGitWorktree Provision = "git-worktree"
)

// Kind discriminates the Mount species. It is explicit, rather than
// inferred from Name, because "workspace" is also a valid shared-dir name.
type Kind string

const (
	KindWorkspace Kind = "workspace"
	KindSharedDir Kind = "shared-dir"
)

// Mode is a workspace sharing mode. The values equal
// store.WorkspaceSharingMode's; this package keeps its own type so that it
// does not depend on pkg/store.
type Mode string

const (
	ModeSharedPlain      Mode = "shared-plain"
	ModeClonePerAgent    Mode = "clone-per-agent"
	ModeWorktreePerAgent Mode = "worktree-per-agent"
	ModeEmptyPerAgent    Mode = "empty-per-agent"
)

// Mount is a directory Scion places in an agent container. The workspace and
// shared dirs (scratchpad included) are instances.
type Mount struct {
	Kind Kind
	// Name is "workspace" for the workspace, or the shared dir's name.
	Name      string
	Scope     Scope
	Owner     Owner
	Provision Provision
	// Mode is the workspace sharing mode (KindWorkspace only).
	Mode Mode
	// Target is the container path the Mount is placed at.
	Target   string
	ReadOnly bool
	// Required: present by construction and cannot be undeclared.
	Required bool
	// UserSource is the host path or bucket of an Owner=user Mount.
	UserSource string
}

// DefaultContainerWorkspace is the container path of the workspace when the
// caller does not name another one.
const DefaultContainerWorkspace = "/workspace"

// SharedDirTarget is the container path of shared dir name: /scion-volumes/<name>,
// or <containerWorkspace>/.scion-volumes/<name> when inWorkspace is set.
func SharedDirTarget(name string, inWorkspace bool, containerWorkspace string) string {
	if containerWorkspace == "" {
		containerWorkspace = DefaultContainerWorkspace
	}
	if inWorkspace {
		return fmt.Sprintf("%s/.scion-volumes/%s", containerWorkspace, name)
	}
	return fmt.Sprintf("/scion-volumes/%s", name)
}

// WorkspaceMount returns the workspace Mount for a sharing mode. Per-agent
// modes give an agent-scoped Mount: resolved with an agent, it names the
// agent's own directory.
func WorkspaceMount(mode Mode, containerWorkspace string) Mount {
	if containerWorkspace == "" {
		containerWorkspace = DefaultContainerWorkspace
	}
	m := Mount{
		Kind:     KindWorkspace,
		Name:     "workspace",
		Scope:    ScopeProject,
		Owner:    OwnerScion,
		Mode:     mode,
		Target:   containerWorkspace,
		Required: true,
	}
	switch mode {
	case ModeClonePerAgent:
		m.Scope, m.Provision = ScopeAgent, ProvisionGitClone
	case ModeEmptyPerAgent:
		m.Scope, m.Provision = ScopeAgent, ProvisionEmpty
	case ModeWorktreePerAgent:
		m.Scope, m.Provision = ScopeAgent, ProvisionGitWorktree
	default:
		m.Provision = ProvisionNone
	}
	return m
}

// SharedDirMount returns the Mount for a declared shared dir.
func SharedDirMount(sd api.SharedDir, containerWorkspace string) Mount {
	return Mount{
		Kind:      KindSharedDir,
		Name:      sd.Name,
		Scope:     ScopeProject,
		Owner:     OwnerScion,
		Provision: ProvisionEmpty,
		Target:    SharedDirTarget(sd.Name, sd.InWorkspace, containerWorkspace),
		ReadOnly:  sd.ReadOnly,
	}
}

// Realize (design §P1) is deferred to phase 3 (fs-cleanup-arch R3): its
// first callers are the k8s buildPod fold (ptone/scion#5823) and the
// docker/podman/apple run arguments. The intended shape, so phases 3 and 5
// implement one function rather than two:
//
//	func (l Layout) Realize(r Resolved, runtime RuntimeKind) (Realized, error)
//
// where Realized carries either a host bind mount (source HostPath, target
// r.Target, read-only) or a pod volume plus volume mount (claim, SubPath,
// target), and container targets are never computed by callers.

// ErrNoMount is returned for a Mount the project does not declare.
var ErrNoMount = errors.New("projectfs: mount is not declared for this project")

// DeclaredSharedDir returns the Mount of shared dir name when declared lists
// it, and ErrNoMount otherwise.
func DeclaredSharedDir(declared []api.SharedDir, name, containerWorkspace string) (Mount, error) {
	for _, sd := range declared {
		if sd.Name == name {
			return SharedDirMount(sd, containerWorkspace), nil
		}
	}
	return Mount{}, ErrNoMount
}

// Share is the NFS share a shared Layout is mounted from.
type Share struct {
	ID     string
	Server string
	Export string
	PVName string
}

// Layout is a resolved storage configuration. Callers build it from their
// own configuration; Resolve is a pure function of (Layout, inputs).
type Layout struct {
	Backend Backend
	// HostBase is, for nfs, where the share is mounted on this host
	// (<mount_root>/<share id>). For local it is the global Scion dir
	// (~/.scion), used to derive project-config roots from slug and ID.
	HostBase string
	// SubPathRoot is the export-relative prefix of every project tree (nfs).
	SubPathRoot string
	Share       Share
	UID, GID    int
}

// Project identifies the project a Mount is resolved for.
type Project struct {
	// ID is the hub project UUID. Required on shared backends; on the local
	// backend it is used, with Slug, to derive the project-config root.
	ID string
	// Slug is the project slug (local backend only).
	Slug string
	// Dir is the host project directory (local backend only): the
	// workspace root for shared-plain, the parent of the worktree base for
	// worktree-per-agent.
	Dir string
	// ConfigRoot is the project-config root holding shared-dirs/ (local
	// backend only), as resolved by the caller from the project's on-disk
	// marker. Empty derives <HostBase>/project-configs/<slug>__<short id>.
	ConfigRoot string
}

// Resolved is where a Mount lives.
type Resolved struct {
	Backend Backend
	// HostBase is the Layout's HostBase for shared backends; empty for local.
	HostBase string
	// HostPath is the directory on this host.
	HostPath string
	// RelPath is HostPath relative to HostBase (shared backends).
	RelPath string
	// SubPathRoot is the Layout's effective subpath root (shared backends).
	SubPathRoot string
	// ServerPath is <export>/<RelPath> (shared backends with an export).
	ServerPath string
	// SubPath is the Kubernetes subPath (RelPath with forward slashes).
	SubPath string
	// Target is the Mount's container path.
	Target string
	// Kind and Name are the resolved Mount's.
	Kind Kind
	Name string
}

const (
	sharedDirsDir     = "shared-dirs"
	workspaceDir      = "workspace"
	agentsDir         = "agents"
	worktreesDir      = "worktrees"
	projectConfigsDir = "project-configs"
	// DefaultSubPathRoot is used when a shared Layout names none.
	DefaultSubPathRoot = "projects"
)

// subPathRoot returns l's effective, structurally checked subpath root.
// Configuration loaders validate it with their own messages first; this is
// the authority's own guard for any Layout handed to it.
func (l Layout) subPathRoot() (string, error) {
	root := l.SubPathRoot
	if root == "" {
		root = DefaultSubPathRoot
	}
	slashed := filepath.ToSlash(root)
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(root) {
		return "", fmt.Errorf("projectfs: subpath_root %q must be relative", root)
	}
	for _, comp := range strings.Split(slashed, "/") {
		if comp == "" || comp == "." || comp == ".." {
			return "", fmt.Errorf("projectfs: subpath_root %q must be a clean relative path", root)
		}
	}
	return root, nil
}

// validProjectID checks a project ID used as a path segment.
func validProjectID(id string) error {
	if !shareddirs.ValidProjectID(id) {
		return fmt.Errorf("projectfs: invalid project ID %q", id)
	}
	return nil
}

// validAgentSlug checks an agent identifier used as a path segment: it must
// already be an agent slug.
func validAgentSlug(agent string) error {
	if slug, err := api.ValidateAgentName(agent); err != nil || slug != agent {
		return fmt.Errorf("projectfs: agent name %q is not an agent slug", agent)
	}
	return nil
}

// validSharedDirName checks a shared dir name used as a path segment.
func validSharedDirName(name string) error {
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
		return fmt.Errorf("projectfs: shared dir %q: %w", name, err)
	}
	return nil
}

// ProjectRoot resolves the root of a project's tree on a shared backend:
// <SubPathRoot>/<projectID>.
func (l Layout) ProjectRoot(projectID string) (Resolved, error) {
	if !l.Backend.Shared() {
		return Resolved{}, fmt.Errorf("projectfs: ProjectRoot is defined for shared backends, not %q", l.Backend)
	}
	if err := validProjectID(projectID); err != nil {
		return Resolved{}, err
	}
	root, err := l.subPathRoot()
	if err != nil {
		return Resolved{}, err
	}
	return l.shared(filepath.Join(root, projectID), Mount{}), nil
}

// ProjectConfigDirName is the name of a project's configuration directory.
const ProjectConfigDirName = ".scion"

// ProjectConfig resolves a project's configuration directory on a shared
// backend: <SubPathRoot>/<projectID>/.scion, beside workspace/ and inside
// no Mount (design-v4 §P1a rule 1). Hub-seeded: project settings, the hub
// connection and project identity. A .scion inside the workspace is
// repository content and never supplies those.
func (l Layout) ProjectConfig(projectID string) (Resolved, error) {
	pr, err := l.ProjectRoot(projectID)
	if err != nil {
		return Resolved{}, err
	}
	return l.shared(filepath.Join(pr.RelPath, ProjectConfigDirName), Mount{}), nil
}

// AgentRoot resolves an agent's directory on a shared backend:
// <SubPathRoot>/<projectID>/agents/<agent slug>.
func (l Layout) AgentRoot(projectID, agent string) (Resolved, error) {
	pr, err := l.ProjectRoot(projectID)
	if err != nil {
		return Resolved{}, err
	}
	if err := validAgentSlug(agent); err != nil {
		return Resolved{}, err
	}
	return l.shared(filepath.Join(pr.RelPath, agentsDir, agent), Mount{}), nil
}

// shared builds a Resolved for rel under a shared Layout.
func (l Layout) shared(rel string, m Mount) Resolved {
	root, _ := l.subPathRoot() // checked by every caller (ProjectRoot)
	r := Resolved{
		Backend:     l.Backend,
		HostBase:    l.HostBase,
		HostPath:    filepath.Join(l.HostBase, rel),
		RelPath:     rel,
		SubPathRoot: root,
		SubPath:     filepath.ToSlash(rel),
		Target:      m.Target,
		Kind:        m.Kind,
		Name:        m.Name,
	}
	if l.Share.Export != "" {
		r.ServerPath = path.Join(l.Share.Export, filepath.ToSlash(rel))
	}
	return r
}

// Resolve answers where Mount m lives for project p and agent (an agent
// slug, or "" for the project-level directory). It is pure: no I/O, no
// lookups. Project IDs, agent slugs and shared-dir names are validated here,
// so every caller gets the same path-escape guard.
//
// For the workspace with agent "", the result is the project-level
// workspace directory whatever the mode: the shared tree for shared-plain,
// the shared base checkout for worktree-per-agent, and, for the per-agent
// modes, the project workspace path that no agent mounts (callers derive
// the agent's own directory, or pass the agent).
func (l Layout) Resolve(p Project, agent string, m Mount) (Resolved, error) {
	switch m.Kind {
	case KindWorkspace, KindSharedDir:
	default:
		return Resolved{}, fmt.Errorf("projectfs: unsupported mount kind %q", m.Kind)
	}
	if m.Owner == OwnerUser {
		return Resolved{}, fmt.Errorf("projectfs: user-owned mount %q is not in the project tree", m.Name)
	}
	if m.Kind == KindSharedDir {
		if err := validSharedDirName(m.Name); err != nil {
			return Resolved{}, err
		}
	}
	if agent != "" {
		if err := validAgentSlug(agent); err != nil {
			return Resolved{}, err
		}
	}
	switch {
	case l.Backend.Shared():
		return l.resolveShared(p, agent, m)
	case l.Backend == BackendLocal:
		return l.resolveLocal(p, agent, m)
	default:
		return Resolved{}, fmt.Errorf("projectfs: unsupported backend %q", l.Backend)
	}
}

func (l Layout) resolveShared(p Project, agent string, m Mount) (Resolved, error) {
	if p.ID == "" {
		return Resolved{}, fmt.Errorf("projectfs: project ID is required on the %s backend", l.Backend)
	}
	pr, err := l.ProjectRoot(p.ID)
	if err != nil {
		return Resolved{}, err
	}
	root, _ := l.subPathRoot() // checked by ProjectRoot

	switch m.Kind {
	case KindSharedDir:
		r := l.shared(filepath.Join(pr.RelPath, sharedDirsDir, m.Name), m)
		// Defense in depth alongside the validation above: the leaf must sit
		// directly under this project's shared-dirs/.
		if err := shareddirs.ConfineLeaf(r.HostPath, l.HostBase, root, p.ID, m.Name); err != nil {
			return Resolved{}, err
		}
		return r, nil
	default: // KindWorkspace
		ws := filepath.Join(pr.RelPath, workspaceDir)
		if agent == "" || m.Scope != ScopeAgent {
			return l.shared(ws, m), nil
		}
		switch m.Mode {
		case ModeWorktreePerAgent:
			return l.shared(filepath.Join(ws, worktreesDir, agent), m), nil
		case ModeClonePerAgent, ModeEmptyPerAgent:
			return l.shared(filepath.Join(pr.RelPath, agentsDir, agent, workspaceDir), m), nil
		default:
			return Resolved{}, fmt.Errorf("projectfs: mode %q has no agent-scoped workspace", m.Mode)
		}
	}
}

func (l Layout) resolveLocal(p Project, agent string, m Mount) (Resolved, error) {
	local := func(hostPath string) Resolved {
		return Resolved{Backend: BackendLocal, HostPath: hostPath, Target: m.Target, Kind: m.Kind, Name: m.Name}
	}
	switch m.Kind {
	case KindSharedDir:
		configRoot := p.ConfigRoot
		if configRoot == "" {
			var err error
			if configRoot, err = l.localConfigRoot(p); err != nil {
				return Resolved{}, err
			}
		}
		return local(filepath.Join(configRoot, sharedDirsDir, m.Name)), nil
	default: // KindWorkspace
		if agent != "" && m.Scope == ScopeAgent {
			// The agent manager still places these on the local backend
			// (phase 3 moves it behind the authority).
			return Resolved{}, fmt.Errorf("projectfs: agent-scoped workspace on the local backend is not resolved here yet")
		}
		if p.Dir == "" {
			return Resolved{}, errors.New("projectfs: project directory is required on the local backend")
		}
		if m.Mode == ModeWorktreePerAgent {
			// The shared base checkout lives under <Dir>/workspace, so each
			// project has its own sentinel dir (filepath.Dir == Dir).
			return local(filepath.Join(p.Dir, workspaceDir)), nil
		}
		return local(p.Dir), nil
	}
}

// localConfigRoot derives <HostBase>/project-configs/<slug>__<short id>, the
// local project-config root keyed by slug and the first 8 hex digits of
// the project ID.
func (l Layout) localConfigRoot(p Project) (string, error) {
	if l.HostBase == "" {
		return "", errors.New("projectfs: local layout has no host base")
	}
	if p.Slug == "" || api.Slugify(p.Slug) != p.Slug {
		return "", fmt.Errorf("projectfs: invalid project slug %q", p.Slug)
	}
	if err := validProjectID(p.ID); err != nil {
		return "", err
	}
	return filepath.Join(l.HostBase, projectConfigsDir, LocalConfigDirName(p.Slug, p.ID)), nil
}

// LocalConfigDirName is the local project-config directory name for a
// project: <slug>__<first 8 characters of the ID without dashes>.
func LocalConfigDirName(slug, projectID string) string {
	short := strings.ReplaceAll(projectID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("%s__%s", slug, short)
}
