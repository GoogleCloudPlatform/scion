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

package runtime

import (
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- SelectWorkspaceBackend truth table ---

func TestSelectWorkspaceBackend(t *testing.T) {
	nfsCfg := &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot:   "/mnt/nfs",
			SubPathRoot: "projects",
			Shares: []config.V1NFSShare{
				{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
			},
		},
	}
	localCfg := &config.V1WorkspaceStorageConfig{
		Backend: "local",
	}
	emptyCfg := &config.V1WorkspaceStorageConfig{
		Backend: "",
	}

	tests := []struct {
		name        string
		cfg         *config.V1WorkspaceStorageConfig
		mode        store.WorkspaceSharingMode
		wantBackend string
	}{
		// Backend=local → always local for all modes
		{
			name:        "local+SharedPlain",
			cfg:         localCfg,
			mode:        store.SharingModeSharedPlain,
			wantBackend: "local",
		},
		{
			name:        "local+WorktreePerAgent",
			cfg:         localCfg,
			mode:        store.SharingModeWorktreePerAgent,
			wantBackend: "local",
		},
		{
			name:        "local+ClonePerAgent",
			cfg:         localCfg,
			mode:        store.SharingModeClonePerAgent,
			wantBackend: "local",
		},

		// Backend="" → always local for all modes
		{
			name:        "empty+SharedPlain",
			cfg:         emptyCfg,
			mode:        store.SharingModeSharedPlain,
			wantBackend: "local",
		},
		{
			name:        "empty+WorktreePerAgent",
			cfg:         emptyCfg,
			mode:        store.SharingModeWorktreePerAgent,
			wantBackend: "local",
		},
		{
			name:        "empty+ClonePerAgent",
			cfg:         emptyCfg,
			mode:        store.SharingModeClonePerAgent,
			wantBackend: "local",
		},

		// nil config → always local
		{
			name:        "nil+SharedPlain",
			cfg:         nil,
			mode:        store.SharingModeSharedPlain,
			wantBackend: "local",
		},
		{
			name:        "nil+ClonePerAgent",
			cfg:         nil,
			mode:        store.SharingModeClonePerAgent,
			wantBackend: "local",
		},

		// Backend=nfs + SharedPlain → nfs
		{
			name:        "nfs+SharedPlain",
			cfg:         nfsCfg,
			mode:        store.SharingModeSharedPlain,
			wantBackend: "nfs",
		},
		// Backend=nfs + WorktreePerAgent → nfs
		{
			name:        "nfs+WorktreePerAgent",
			cfg:         nfsCfg,
			mode:        store.SharingModeWorktreePerAgent,
			wantBackend: "nfs",
		},
		// Backend=nfs + ClonePerAgent → local (deliberate node-local escape hatch)
		{
			name:        "nfs+ClonePerAgent",
			cfg:         nfsCfg,
			mode:        store.SharingModeClonePerAgent,
			wantBackend: "local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := SelectWorkspaceBackend(tt.cfg, tt.mode)
			if got := backend.Name(); got != tt.wantBackend {
				t.Errorf("SelectWorkspaceBackend(%q, %q) = %q, want %q",
					backendStr(tt.cfg), tt.mode, got, tt.wantBackend)
			}
		})
	}
}

func backendStr(cfg *config.V1WorkspaceStorageConfig) string {
	if cfg == nil {
		return "<nil>"
	}
	return cfg.Backend
}

// --- NFS Resolve tests ---

func TestNFSBackendResolve(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}

	tests := []struct {
		name               string
		input              ResolveInput
		wantHostPath       string
		wantServerRelPath  string
		wantHostBase       string
		wantSharedDirPaths map[string]string // name → hostPath
		wantSharedDirRels  map[string]string // name → serverRelPath
		wantErr            bool
	}{
		{
			name: "basic workspace path",
			input: ResolveInput{
				ProjectID: "proj-abc-123",
				AgentID:   "agent-xyz",
				Mode:      store.SharingModeSharedPlain,
			},
			wantHostPath:      filepath.Join("/mnt/nfs", "share1", "projects", "proj-abc-123", "workspace"),
			wantServerRelPath: filepath.Join("projects", "proj-abc-123", "workspace"),
			wantHostBase:      filepath.Join("/mnt/nfs", "share1"),
		},
		{
			name: "workspace with shared dirs",
			input: ResolveInput{
				ProjectID:      "proj-abc-123",
				AgentID:        "agent-xyz",
				Mode:           store.SharingModeSharedPlain,
				SharedDirNames: []string{"data", "cache"},
			},
			wantHostPath:      filepath.Join("/mnt/nfs", "share1", "projects", "proj-abc-123", "workspace"),
			wantServerRelPath: filepath.Join("projects", "proj-abc-123", "workspace"),
			wantHostBase:      filepath.Join("/mnt/nfs", "share1"),
			wantSharedDirPaths: map[string]string{
				"data":  filepath.Join("/mnt/nfs", "share1", "projects", "proj-abc-123", "shared-dirs", "data"),
				"cache": filepath.Join("/mnt/nfs", "share1", "projects", "proj-abc-123", "shared-dirs", "cache"),
			},
			wantSharedDirRels: map[string]string{
				"data":  filepath.Join("projects", "proj-abc-123", "shared-dirs", "data"),
				"cache": filepath.Join("projects", "proj-abc-123", "shared-dirs", "cache"),
			},
		},
		{
			name: "worktree-per-agent mode same workspace path",
			input: ResolveInput{
				ProjectID: "proj-abc-123",
				AgentID:   "agent-xyz",
				Mode:      store.SharingModeWorktreePerAgent,
			},
			wantHostPath:      filepath.Join("/mnt/nfs", "share1", "projects", "proj-abc-123", "workspace"),
			wantServerRelPath: filepath.Join("projects", "proj-abc-123", "workspace"),
			wantHostBase:      filepath.Join("/mnt/nfs", "share1"),
		},
		{
			name: "missing project ID",
			input: ResolveInput{
				AgentID: "agent-xyz",
				Mode:    store.SharingModeSharedPlain,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewNFSBackend(nfsCfg)
			got, err := b.Resolve(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.HostPath != tt.wantHostPath {
				t.Errorf("HostPath = %q, want %q", got.HostPath, tt.wantHostPath)
			}
			if got.ServerRelativePath != tt.wantServerRelPath {
				t.Errorf("ServerRelativePath = %q, want %q", got.ServerRelativePath, tt.wantServerRelPath)
			}
			if got.HostBase != tt.wantHostBase {
				t.Errorf("HostBase = %q, want %q", got.HostBase, tt.wantHostBase)
			}
			if got.Backend != "nfs" {
				t.Errorf("Backend = %q, want %q", got.Backend, "nfs")
			}

			// Verify shared dirs
			for name, wantPath := range tt.wantSharedDirPaths {
				sd, ok := got.SharedDirs[name]
				if !ok {
					t.Errorf("shared dir %q not found in result", name)
					continue
				}
				if sd.HostPath != wantPath {
					t.Errorf("SharedDirs[%q].HostPath = %q, want %q", name, sd.HostPath, wantPath)
				}
			}
			for name, wantRel := range tt.wantSharedDirRels {
				sd, ok := got.SharedDirs[name]
				if !ok {
					continue // already reported above
				}
				if sd.ServerRelativePath != wantRel {
					t.Errorf("SharedDirs[%q].ServerRelativePath = %q, want %q", name, sd.ServerRelativePath, wantRel)
				}
			}
		})
	}
}

// TestNFSResolve_Deterministic verifies that calling Resolve twice with the
// same inputs produces identical output — the fundamental contract for
// cross-replica consistency.
func TestNFSResolve_Deterministic(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}

	input := ResolveInput{
		ProjectID:      "550e8400-e29b-41d4-a716-446655440000",
		AgentID:        "660e8400-e29b-41d4-a716-446655440001",
		Mode:           store.SharingModeSharedPlain,
		SharedDirNames: []string{"artifacts", "cache"},
	}

	b := NewNFSBackend(nfsCfg)

	r1, err := b.Resolve(input)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	r2, err := b.Resolve(input)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}

	if r1.HostPath != r2.HostPath {
		t.Errorf("HostPath not deterministic: %q vs %q", r1.HostPath, r2.HostPath)
	}
	if r1.ServerRelativePath != r2.ServerRelativePath {
		t.Errorf("ServerRelativePath not deterministic: %q vs %q", r1.ServerRelativePath, r2.ServerRelativePath)
	}
	if r1.HostBase != r2.HostBase {
		t.Errorf("HostBase not deterministic: %q vs %q", r1.HostBase, r2.HostBase)
	}
	for name, sd1 := range r1.SharedDirs {
		sd2, ok := r2.SharedDirs[name]
		if !ok {
			t.Errorf("shared dir %q missing from second Resolve", name)
			continue
		}
		if sd1.HostPath != sd2.HostPath {
			t.Errorf("SharedDirs[%q].HostPath not deterministic: %q vs %q", name, sd1.HostPath, sd2.HostPath)
		}
		if sd1.ServerRelativePath != sd2.ServerRelativePath {
			t.Errorf("SharedDirs[%q].ServerRelativePath not deterministic: %q vs %q", name, sd1.ServerRelativePath, sd2.ServerRelativePath)
		}
	}
}

// TestNFSResolve_PathsAreUnderMountNotExportRoot verifies that resolved
// paths are under <MountRoot>/<shareID>, never under the NFS export root.
func TestNFSResolve_PathsAreUnderMountNotExportRoot(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}

	b := NewNFSBackend(nfsCfg)
	res, err := b.Resolve(ResolveInput{
		ProjectID:      "proj1",
		Mode:           store.SharingModeSharedPlain,
		SharedDirNames: []string{"data"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	exportRoot := "/scion-workspaces"

	// Workspace host path must not start with the export root
	if len(res.HostPath) >= len(exportRoot) && res.HostPath[:len(exportRoot)] == exportRoot {
		t.Errorf("HostPath %q starts with export root %q — should be under MountRoot/shareID", res.HostPath, exportRoot)
	}

	// It must start with the mount root + share ID
	wantPrefix := filepath.Join("/mnt/nfs", "ws1")
	if len(res.HostPath) < len(wantPrefix) || res.HostPath[:len(wantPrefix)] != wantPrefix {
		t.Errorf("HostPath %q does not start with expected prefix %q", res.HostPath, wantPrefix)
	}

	// Same for shared dirs
	for name, sd := range res.SharedDirs {
		if len(sd.HostPath) >= len(exportRoot) && sd.HostPath[:len(exportRoot)] == exportRoot {
			t.Errorf("SharedDirs[%q].HostPath %q starts with export root", name, sd.HostPath)
		}
		if len(sd.HostPath) < len(wantPrefix) || sd.HostPath[:len(wantPrefix)] != wantPrefix {
			t.Errorf("SharedDirs[%q].HostPath %q does not start with expected prefix %q", name, sd.HostPath, wantPrefix)
		}
	}
}

// TestNFSResolve_ServerRelativePathFormat verifies the exact server-relative
// layout matches the design: projects/<pid>/workspace and
// projects/<pid>/shared-dirs/<name>.
func TestNFSResolve_ServerRelativePathFormat(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}

	b := NewNFSBackend(nfsCfg)
	res, err := b.Resolve(ResolveInput{
		ProjectID:      "my-project-id",
		Mode:           store.SharingModeSharedPlain,
		SharedDirNames: []string{"logs"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	wantWorkspaceRel := filepath.Join("projects", "my-project-id", "workspace")
	if res.ServerRelativePath != wantWorkspaceRel {
		t.Errorf("workspace ServerRelativePath = %q, want %q", res.ServerRelativePath, wantWorkspaceRel)
	}

	wantSharedDirRel := filepath.Join("projects", "my-project-id", "shared-dirs", "logs")
	if sd, ok := res.SharedDirs["logs"]; !ok {
		t.Error("shared dir 'logs' not found")
	} else if sd.ServerRelativePath != wantSharedDirRel {
		t.Errorf("shared dir ServerRelativePath = %q, want %q", sd.ServerRelativePath, wantSharedDirRel)
	}
}

// TestNFSResolve_NoShares verifies that Resolve returns an error when no
// shares are configured.
func TestNFSResolve_NoShares(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares:      nil,
	}
	b := NewNFSBackend(nfsCfg)
	_, err := b.Resolve(ResolveInput{
		ProjectID: "proj1",
		Mode:      store.SharingModeSharedPlain,
	})
	if err == nil {
		t.Error("expected error for no shares, got nil")
	}
}

// TestNFSResolve_NilConfig verifies that Resolve returns an error when the
// NFS config is nil.
func TestNFSResolve_NilConfig(t *testing.T) {
	b := NewNFSBackend(nil)
	_, err := b.Resolve(ResolveInput{
		ProjectID: "proj1",
		Mode:      store.SharingModeSharedPlain,
	})
	if err == nil {
		t.Error("expected error for nil config, got nil")
	}
}

// TestNFSResolve_SubPathRootDefault verifies that an empty SubPathRoot
// defaults to "projects".
func TestNFSResolve_SubPathRootDefault(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "", // should default to "projects"
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}

	b := NewNFSBackend(nfsCfg)
	res, err := b.Resolve(ResolveInput{
		ProjectID: "proj1",
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	wantRel := filepath.Join("projects", "proj1", "workspace")
	if res.ServerRelativePath != wantRel {
		t.Errorf("ServerRelativePath = %q, want %q (default SubPathRoot)", res.ServerRelativePath, wantRel)
	}
}

// --- Local Backend Resolve tests ---

func TestLocalBackendResolve(t *testing.T) {
	tests := []struct {
		name        string
		input       ResolveInput
		wantErr     bool
		wantPath    string
		wantBackend string
	}{
		{
			name: "basic local resolve",
			input: ResolveInput{
				ProjectDir: "/home/user/.scion.projects/my-project",
				ProjectID:  "proj1",
				Mode:       store.SharingModeSharedPlain,
			},
			wantPath:    "/home/user/.scion.projects/my-project",
			wantBackend: "local",
		},
		{
			name: "local resolve clone-per-agent",
			input: ResolveInput{
				ProjectDir: "/home/user/.scion.projects/my-project",
				ProjectID:  "proj1",
				Mode:       store.SharingModeClonePerAgent,
			},
			wantPath:    "/home/user/.scion.projects/my-project",
			wantBackend: "local",
		},
		{
			name: "missing ProjectDir",
			input: ResolveInput{
				ProjectID: "proj1",
				Mode:      store.SharingModeSharedPlain,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewLocalBackend()
			got, err := b.Resolve(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.HostPath != tt.wantPath {
				t.Errorf("HostPath = %q, want %q", got.HostPath, tt.wantPath)
			}
			if got.Backend != tt.wantBackend {
				t.Errorf("Backend = %q, want %q", got.Backend, tt.wantBackend)
			}
			if got.ServerRelativePath != "" {
				t.Errorf("ServerRelativePath = %q, want empty for local", got.ServerRelativePath)
			}
			if got.HostBase != "" {
				t.Errorf("HostBase = %q, want empty for local", got.HostBase)
			}
		})
	}
}

// TestLocalBackendResolve_MatchesPreExisting asserts that localBackend.Resolve
// produces the same host path that the existing broker path resolution would
// produce for a hub-native project. This is the "zero behavior change" guard.
func TestLocalBackendResolve_MatchesPreExisting(t *testing.T) {
	// The existing broker resolution for a hub-native project sets
	// ProjectPath = ~/.scion.projects/<slug>. localBackend.Resolve must
	// return exactly that path as HostPath.
	projectPath := "/home/scion/.scion.projects/my-project-slug"

	b := NewLocalBackend()
	res, err := b.Resolve(ResolveInput{
		ProjectDir: projectPath,
		ProjectID:  "proj-uuid",
		Mode:       store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if res.HostPath != projectPath {
		t.Errorf("localBackend.Resolve HostPath = %q, want %q (pre-existing path)", res.HostPath, projectPath)
	}
}

// --- Realize tests ---

func TestLocalBackendRealize(t *testing.T) {
	b := NewLocalBackend()
	desc, err := b.Realize(RealizeInput{
		Resolved: ResolvedWorkspace{
			HostPath: "/home/scion/.scion.projects/my-project",
			Backend:  "local",
		},
		ContainerWorkspace: "/workspace",
	})
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}

	if desc.Type != "local" {
		t.Errorf("Type = %q, want %q", desc.Type, "local")
	}
	if desc.HostPath != "/home/scion/.scion.projects/my-project" {
		t.Errorf("HostPath = %q, want the resolved path", desc.HostPath)
	}
	if desc.Target != "/workspace" {
		t.Errorf("Target = %q, want %q", desc.Target, "/workspace")
	}
}

func TestLocalBackendRealize_DefaultTarget(t *testing.T) {
	b := NewLocalBackend()
	desc, err := b.Realize(RealizeInput{
		Resolved: ResolvedWorkspace{
			HostPath: "/some/path",
			Backend:  "local",
		},
		ContainerWorkspace: "", // should default to /workspace
	})
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}
	if desc.Target != "/workspace" {
		t.Errorf("Target = %q, want %q (default)", desc.Target, "/workspace")
	}
}

func TestNFSBackendRealize(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}
	b := NewNFSBackend(nfsCfg)

	desc, err := b.Realize(RealizeInput{
		Resolved: ResolvedWorkspace{
			HostPath:           "/mnt/nfs/share1/projects/proj1/workspace",
			ServerRelativePath: "projects/proj1/workspace",
			HostBase:           "/mnt/nfs/share1",
			Backend:            "nfs",
		},
		ContainerWorkspace: "/workspace",
	})
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}

	if desc.Type != "nfs" {
		t.Errorf("Type = %q, want %q", desc.Type, "nfs")
	}
	if desc.HostPath != "/mnt/nfs/share1/projects/proj1/workspace" {
		t.Errorf("HostPath = %q, want the NFS host path", desc.HostPath)
	}
	if desc.Target != "/workspace" {
		t.Errorf("Target = %q, want %q", desc.Target, "/workspace")
	}
	if desc.SubPath != "projects/proj1/workspace" {
		t.Errorf("SubPath = %q, want the server-relative path", desc.SubPath)
	}
}

// --- Backend Name tests ---

func TestBackendNames(t *testing.T) {
	local := NewLocalBackend()
	if local.Name() != "local" {
		t.Errorf("local backend Name() = %q, want %q", local.Name(), "local")
	}

	nfsCfg := &config.V1NFSConfig{
		MountRoot: "/mnt/nfs",
		Shares: []config.V1NFSShare{
			{ID: "s1", Server: "10.0.0.2", Export: "/ws"},
		},
	}
	nfs := NewNFSBackend(nfsCfg)
	if nfs.Name() != "nfs" {
		t.Errorf("nfs backend Name() = %q, want %q", nfs.Name(), "nfs")
	}
}

// --- Provision stub tests ---

func TestLocalBackendProvision_NoOp(t *testing.T) {
	b := NewLocalBackend()
	err := b.Provision(ProvisionInput{
		ProjectID: "proj1",
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Errorf("localBackend.Provision should be no-op, got error: %v", err)
	}
}

func TestNFSBackendProvision_NonGit(t *testing.T) {
	mountRoot := t.TempDir()
	nfsCfg := &config.V1NFSConfig{
		MountRoot: mountRoot,
		Shares: []config.V1NFSShare{
			{ID: "s1", Server: "10.0.0.2", Export: "/ws"},
		},
	}
	b := NewNFSBackend(nfsCfg)
	res, err := b.Resolve(ResolveInput{
		ProjectID: "proj1",
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = b.Provision(ProvisionInput{
		Resolved:  res,
		ProjectID: "proj1",
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Errorf("nfsBackend.Provision for non-git project should succeed, got error: %v", err)
	}
}
