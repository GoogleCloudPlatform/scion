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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- ValidateNotExportRoot tests ---

func TestValidateNotExportRoot_Valid(t *testing.T) {
	tests := []struct {
		name     string
		hostPath string
		hostBase string
	}{
		{
			name:     "project subtree under mount",
			hostPath: "/mnt/nfs/ws1/projects/proj1/workspace",
			hostBase: "/mnt/nfs/ws1",
		},
		{
			name:     "shared dir under mount",
			hostPath: "/mnt/nfs/ws1/projects/proj1/shared-dirs/data",
			hostBase: "/mnt/nfs/ws1",
		},
		{
			name:     "local backend empty hostBase",
			hostPath: "/home/user/.scion.projects/my-project",
			hostBase: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateNotExportRoot(tt.hostPath, tt.hostBase); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

func TestValidateNotExportRoot_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		hostPath string
		hostBase string
	}{
		{
			name:     "path equals export root",
			hostPath: "/mnt/nfs/ws1",
			hostBase: "/mnt/nfs/ws1",
		},
		{
			name:     "path equals export root with trailing slash",
			hostPath: "/mnt/nfs/ws1/",
			hostBase: "/mnt/nfs/ws1",
		},
		{
			name:     "path not under export root",
			hostPath: "/some/other/path",
			hostBase: "/mnt/nfs/ws1",
		},
		{
			name:     "path is sibling of export root",
			hostPath: "/mnt/nfs/ws1-other/projects/proj1",
			hostBase: "/mnt/nfs/ws1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateNotExportRoot(tt.hostPath, tt.hostBase); err == nil {
				t.Error("expected error for isolation violation, got nil")
			}
		})
	}
}

// --- NFSSharedDirsToVolumeMounts tests ---

func TestNFSSharedDirsToVolumeMounts_Basic(t *testing.T) {
	resolved := ResolvedWorkspace{
		HostPath: "/mnt/nfs/ws1/projects/proj1/workspace",
		HostBase: "/mnt/nfs/ws1",
		Backend:  "nfs",
		SharedDirs: map[string]ResolvedSharedDir{
			"data": {
				HostPath:           "/mnt/nfs/ws1/projects/proj1/shared-dirs/data",
				ServerRelativePath: "projects/proj1/shared-dirs/data",
			},
			"cache": {
				HostPath:           "/mnt/nfs/ws1/projects/proj1/shared-dirs/cache",
				ServerRelativePath: "projects/proj1/shared-dirs/cache",
			},
		},
	}

	dirs := []api.SharedDir{
		{Name: "data", ReadOnly: false},
		{Name: "cache", ReadOnly: true, InWorkspace: true},
	}

	mounts, err := NFSSharedDirsToVolumeMounts(resolved, dirs, "/workspace")
	if err != nil {
		t.Fatalf("NFSSharedDirsToVolumeMounts: %v", err)
	}

	if len(mounts) != 2 {
		t.Fatalf("len(mounts) = %d, want 2", len(mounts))
	}

	// data → /scion-volumes/data
	if mounts[0].Source != "/mnt/nfs/ws1/projects/proj1/shared-dirs/data" {
		t.Errorf("mounts[0].Source = %q, want NFS path", mounts[0].Source)
	}
	if mounts[0].Target != "/scion-volumes/data" {
		t.Errorf("mounts[0].Target = %q, want /scion-volumes/data", mounts[0].Target)
	}
	if mounts[0].ReadOnly {
		t.Error("mounts[0].ReadOnly should be false")
	}

	// cache → /workspace/.scion-volumes/cache (InWorkspace=true)
	if mounts[1].Source != "/mnt/nfs/ws1/projects/proj1/shared-dirs/cache" {
		t.Errorf("mounts[1].Source = %q, want NFS path", mounts[1].Source)
	}
	if mounts[1].Target != "/workspace/.scion-volumes/cache" {
		t.Errorf("mounts[1].Target = %q, want /workspace/.scion-volumes/cache", mounts[1].Target)
	}
	if !mounts[1].ReadOnly {
		t.Error("mounts[1].ReadOnly should be true")
	}
}

func TestNFSSharedDirsToVolumeMounts_MissingDir(t *testing.T) {
	resolved := ResolvedWorkspace{
		HostBase:   "/mnt/nfs/ws1",
		Backend:    "nfs",
		SharedDirs: map[string]ResolvedSharedDir{},
	}

	dirs := []api.SharedDir{
		{Name: "nonexistent"},
	}

	_, err := NFSSharedDirsToVolumeMounts(resolved, dirs, "/workspace")
	if err == nil {
		t.Error("expected error for missing shared dir in resolution")
	}
}

func TestNFSSharedDirsToVolumeMounts_Empty(t *testing.T) {
	resolved := ResolvedWorkspace{Backend: "nfs"}
	mounts, err := NFSSharedDirsToVolumeMounts(resolved, nil, "/workspace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mounts != nil {
		t.Errorf("expected nil mounts for empty dirs, got %v", mounts)
	}
}

func TestNFSSharedDirsToVolumeMounts_IsolationGuard(t *testing.T) {
	// Simulate a bad resolution where shared dir path equals the export root.
	resolved := ResolvedWorkspace{
		HostPath: "/mnt/nfs/ws1/projects/proj1/workspace",
		HostBase: "/mnt/nfs/ws1",
		Backend:  "nfs",
		SharedDirs: map[string]ResolvedSharedDir{
			"bad": {
				HostPath: "/mnt/nfs/ws1", // equals export root — should be rejected
			},
		},
	}

	dirs := []api.SharedDir{{Name: "bad"}}

	_, err := NFSSharedDirsToVolumeMounts(resolved, dirs, "/workspace")
	if err == nil {
		t.Error("expected error when shared dir path equals export root")
	}
}

// --- NFS Realize isolation guard tests ---

func TestNFSRealize_IsolationGuard(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}
	b := NewNFSBackend(nfsCfg)

	// Valid: project subtree
	_, err := b.Realize(RealizeInput{
		Resolved: ResolvedWorkspace{
			HostPath:           "/mnt/nfs/share1/projects/proj1/workspace",
			HostBase:           "/mnt/nfs/share1",
			Backend:            "nfs",
			ServerRelativePath: "projects/proj1/workspace",
		},
		ContainerWorkspace: "/workspace",
	})
	if err != nil {
		t.Errorf("valid Realize returned error: %v", err)
	}

	// Invalid: export root itself
	_, err = b.Realize(RealizeInput{
		Resolved: ResolvedWorkspace{
			HostPath: "/mnt/nfs/share1",
			HostBase: "/mnt/nfs/share1",
			Backend:  "nfs",
		},
		ContainerWorkspace: "/workspace",
	})
	if err == nil {
		t.Error("expected error when HostPath equals export root")
	}
}

// --- NFS path resolution produces correct paths (end-to-end) ---

func TestNFSResolveRealize_EndToEnd(t *testing.T) {
	nfsCfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}

	backend := NewNFSBackend(nfsCfg)
	resolved, err := backend.Resolve(ResolveInput{
		ProjectID:      "my-project-id",
		AgentID:        "agent-1",
		Mode:           store.SharingModeSharedPlain,
		SharedDirNames: []string{"data"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Verify workspace path
	wantWorkspace := filepath.Join("/mnt/nfs", "ws1", "projects", "my-project-id", "workspace")
	if resolved.HostPath != wantWorkspace {
		t.Errorf("HostPath = %q, want %q", resolved.HostPath, wantWorkspace)
	}

	// Realize should produce a valid descriptor
	desc, err := backend.Realize(RealizeInput{
		Resolved:           resolved,
		ContainerWorkspace: "/workspace",
	})
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}
	if desc.Type != "nfs" {
		t.Errorf("desc.Type = %q, want nfs", desc.Type)
	}
	if desc.HostPath != wantWorkspace {
		t.Errorf("desc.HostPath = %q, want %q", desc.HostPath, wantWorkspace)
	}
	if desc.Target != "/workspace" {
		t.Errorf("desc.Target = %q, want /workspace", desc.Target)
	}

	// Shared dirs should produce NFS-backed mounts
	dirs := []api.SharedDir{{Name: "data"}}
	mounts, err := NFSSharedDirsToVolumeMounts(resolved, dirs, "/workspace")
	if err != nil {
		t.Fatalf("NFSSharedDirsToVolumeMounts: %v", err)
	}
	wantSDPath := filepath.Join("/mnt/nfs", "ws1", "projects", "my-project-id", "shared-dirs", "data")
	if len(mounts) != 1 || mounts[0].Source != wantSDPath {
		t.Errorf("shared dir mount source = %v, want %q", mounts, wantSDPath)
	}
}

// --- Local path resolution unchanged (zero behavior change guard) ---

func TestLocalResolveRealize_Unchanged(t *testing.T) {
	backend := NewLocalBackend()
	projectPath := "/home/scion/.scion.projects/my-project"

	resolved, err := backend.Resolve(ResolveInput{
		ProjectDir: projectPath,
		ProjectID:  "proj1",
		Mode:       store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Local backend produces the exact project path — no change.
	if resolved.HostPath != projectPath {
		t.Errorf("HostPath = %q, want %q (unchanged)", resolved.HostPath, projectPath)
	}
	if resolved.Backend != "local" {
		t.Errorf("Backend = %q, want local", resolved.Backend)
	}

	// Realize produces a local bind-mount descriptor.
	desc, err := backend.Realize(RealizeInput{
		Resolved:           resolved,
		ContainerWorkspace: "/workspace",
	})
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}
	if desc.Type != "local" {
		t.Errorf("desc.Type = %q, want local", desc.Type)
	}
	if desc.HostPath != projectPath {
		t.Errorf("desc.HostPath = %q, want %q (unchanged)", desc.HostPath, projectPath)
	}
}

// TestLocalSharedDirs_Unchanged verifies that the local backend's shared dir
// resolution is unchanged — the existing config.SharedDirsToVolumeMounts path
// is still used for local backends. This test documents the invariant.
func TestLocalSharedDirs_Unchanged(t *testing.T) {
	// The local path calls config.GetSharedDirPath which works on the
	// local filesystem. For NFS, NFSSharedDirsToVolumeMounts is used instead.
	// This test just ensures local path is still exposed for backward compat.
	backend := NewLocalBackend()
	resolved, err := backend.Resolve(ResolveInput{
		ProjectDir:     "/home/scion/.scion.projects/my-project",
		ProjectID:      "proj1",
		Mode:           store.SharingModeSharedPlain,
		SharedDirNames: []string{"logs"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Local backend resolves shared dirs via config helpers
	sd, ok := resolved.SharedDirs["logs"]
	if !ok {
		t.Fatal("shared dir 'logs' not found in local resolution")
	}
	if sd.HostPath == "" {
		t.Error("shared dir host path should not be empty for local backend")
	}
	if sd.ServerRelativePath != "" {
		t.Error("shared dir ServerRelativePath should be empty for local backend")
	}
}
