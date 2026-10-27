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

	// Shared dirs resolve next to the workspace.
	wantSDPath := filepath.Join("/mnt/nfs", "ws1", "projects", "my-project-id", "shared-dirs", "data")
	if got := resolved.SharedDirs["data"].HostPath; got != wantSDPath {
		t.Errorf("shared dir host path = %q, want %q", got, wantSDPath)
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
	// local filesystem. For NFS, pkg/agent resolves shared dirs through projectfs.
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
