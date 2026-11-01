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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

func TestCheckNFSMountScope(t *testing.T) {
	p := scopeRel
	for _, tc := range []struct {
		rel, root, pid string
		wantWhy        string // "" allows
	}{
		// Allowed leaves.
		{rel: p("workspace"), pid: scopeProjectID},
		{rel: p("workspace", ".scion"), pid: scopeProjectID},
		{rel: p("workspace", ".git", "hooks"), pid: scopeProjectID},
		{rel: p("workspace", "worktrees", scopeAgent), pid: scopeProjectID},
		{rel: p("agents", scopeAgent), pid: scopeProjectID},
		{rel: p("agents", scopeAgent, "workspace"), pid: scopeProjectID},
		{rel: p("agents", scopeAgent, "home-"+scopeAgentID), pid: scopeProjectID},
		{rel: p("shared-dirs", "scratch"), pid: scopeProjectID},
		{rel: p("provision"), pid: scopeProjectID},
		{rel: p(".scion-x"), pid: scopeProjectID},
		{rel: "team/projects/" + scopeProjectID + "/workspace", root: "team/projects", pid: scopeProjectID},
		// Refused.
		{rel: "", pid: scopeProjectID, wantWhy: "the export root"},
		{rel: ".", pid: scopeProjectID, wantWhy: "the export root"},
		{rel: "projects", pid: scopeProjectID, wantWhy: "a parent of the project root"},
		{rel: "team", root: "team/projects", pid: scopeProjectID, wantWhy: "a parent of the project root"},
		{rel: p(), pid: scopeProjectID, wantWhy: "the project root"},
		{rel: p(".scion"), pid: scopeProjectID, wantWhy: "the project configuration directory"},
		{rel: p(".scion", "templates"), pid: scopeProjectID, wantWhy: "the project configuration directory"},
		{rel: p("agents"), pid: scopeProjectID, wantWhy: "the parent of every entry in agents/"},
		{rel: p("shared-dirs"), pid: scopeProjectID, wantWhy: "the parent of every entry in shared-dirs/"},
		{rel: "projects/other/workspace", pid: scopeProjectID, wantWhy: "outside project"},
		{rel: "elsewhere/workspace", pid: scopeProjectID, wantWhy: "outside project"},
		{rel: p("workspace") + "/../.scion", pid: scopeProjectID, wantWhy: "not a clean path"},
		{rel: p("workspace") + "/", pid: scopeProjectID, wantWhy: "not a clean path"},
		{rel: "/" + p("workspace"), pid: scopeProjectID, wantWhy: "not a relative path"},
		{rel: p("workspace"), pid: "", wantWhy: "not checkable"},
		{rel: p("workspace"), pid: "a/b", wantWhy: "not checkable"},
		{rel: p("workspace"), root: "/abs", pid: scopeProjectID, wantWhy: "not checkable"},
	} {
		err := checkNFSMountScope(tc.rel, tc.root, tc.pid)
		switch {
		case tc.wantWhy == "" && err != nil:
			t.Errorf("checkNFSMountScope(%q, %q, %q) = %v, want nil", tc.rel, tc.root, tc.pid, err)
		case tc.wantWhy != "" && (!errors.Is(err, errNFSMountScope) || !strings.Contains(err.Error(), tc.wantWhy)):
			t.Errorf("checkNFSMountScope(%q, %q, %q) = %v, want %q", tc.rel, tc.root, tc.pid, err, tc.wantWhy)
		}
	}
}

func TestNewNFSBindScope(t *testing.T) {
	ws := "/mnt/scion/share1/" + scopeRel("workspace")
	s, err := newNFSBindScope(RunConfig{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: ws})
	if err != nil || s == nil || s.hostBase != filepath.FromSlash("/mnt/scion/share1") {
		t.Fatalf("newNFSBindScope = %+v, %v; want host base /mnt/scion/share1", s, err)
	}
	if err := s.check("/home/u/.scion/agents/a/home"); err != nil {
		t.Errorf("a source off the share is not the check's concern: %v", err)
	}
	if err := s.check("/mnt/scion/share1/" + scopeRel()); !errors.Is(err, errNFSMountScope) {
		t.Errorf("check(project root) = %v, want refusal", err)
	}
	if err := s.check("/mnt/scion/share1"); !errors.Is(err, errNFSMountScope) {
		t.Errorf("check(share root) = %v, want refusal", err)
	}
	// A host base that itself contains <subpath root>/<project ID>: only
	// the workspace suffix is removed.
	nested := "/srv/" + scopeRel("mnt", "share1", scopeRel("workspace"))
	if s, err := newNFSBindScope(RunConfig{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: nested}); err != nil ||
		s.hostBase != filepath.FromSlash("/srv/"+scopeRel("mnt", "share1")) {
		t.Errorf("nested host base: newNFSBindScope(%s) = %+v, %v", nested, s, err)
	}
	if s, err := newNFSBindScope(RunConfig{WorkspaceBackendName: "local", Workspace: "/x"}); s != nil || err != nil {
		t.Errorf("local backend: newNFSBindScope = %+v, %v; want no check", s, err)
	}
	for _, cfg := range []RunConfig{
		{WorkspaceBackendName: "nfs", Workspace: ws},
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: "/srv/agent/workspace"},
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: "/mnt/scion/share1/" + scopeRel()},
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, NFSSubPathRoot: "other", Workspace: ws},
		// Another project whose ID has this one as a prefix.
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: "/mnt/scion/share1/projects/" + scopeProjectID + "x/workspace"},
		// Not the project workspace itself.
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: ws + "/sub"},
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: "/mnt/scion/share1/" + scopeRel("agents", scopeAgent, "workspace")},
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: scopeRel("workspace")},
	} {
		if _, err := newNFSBindScope(cfg); !errors.Is(err, errNFSMountScope) {
			t.Errorf("newNFSBindScope(%+v) = %v, want refusal", cfg, err)
		}
	}
}

// buildPod refuses a workspace-share mount of the export root, the project
// root or the project configuration directory, with a claim (no NFS home,
// so no other check applies) and inline.
func TestBuildPod_NFSMountScopeRefused(t *testing.T) {
	for _, share := range []k8sScopeShare{{name: "claim-pod-home"}, {name: "inline-pod-home", inline: true}} {
		for name, subPath := range map[string]string{
			"export root":           "",
			"project root":          scopeRel(),
			"project configuration": scopeRel(".scion"),
		} {
			t.Run(share.name+"/"+name, func(t *testing.T) {
				cfg := k8sScopeConfig(share, "")
				cfg.NFSSubPath = subPath
				_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
				if !errors.Is(err, errNFSMountScope) {
					t.Fatalf("buildPod = %v, want a mount-scope refusal", err)
				}
			})
		}
	}
}

// buildCommonRunArgs refuses a bind of the project root or the project
// configuration directory, here a configured volume, and a workspace that
// is not in the project's tree.
func TestBuildCommonRunArgs_NFSMountScopeRefused(t *testing.T) {
	for name, rel := range map[string][]string{
		"project root":          nil,
		"project configuration": {projectfs.ProjectConfigDirName},
		"agents parent":         {"agents"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDockerScopeFixture(t)
			ws := f.workspace(t, projectfs.ModeSharedPlain, "")
			src := filepath.Join(append([]string{f.layout.HostBase, scopeRoot, scopeProjectID}, rel...)...)
			mkdirs(t, src)
			_, err := buildCommonRunArgs(RunConfig{
				Harness: &harness.Generic{}, Name: scopeAgent, UnixUsername: "scion", Image: "scion-agent:latest",
				RuntimeName: "docker", WorkspaceBackendName: "nfs", ProjectID: scopeProjectID,
				HomeDir: f.home, Workspace: ws,
				Volumes: []api.VolumeMount{{Source: src, Target: "/extra", ReadOnly: true}},
			})
			if !errors.Is(err, errNFSMountScope) {
				t.Fatalf("buildCommonRunArgs = %v, want a mount-scope refusal", err)
			}
		})
	}
	t.Run("workspace outside the project tree", func(t *testing.T) {
		f := newDockerScopeFixture(t)
		ws := filepath.Join(t.TempDir(), "elsewhere")
		mkdirs(t, ws)
		_, err := buildCommonRunArgs(RunConfig{
			Harness: &harness.Generic{}, Name: scopeAgent, UnixUsername: "scion", Image: "scion-agent:latest",
			RuntimeName: "docker", WorkspaceBackendName: "nfs", ProjectID: scopeProjectID,
			HomeDir: f.home, Workspace: ws,
		})
		if !errors.Is(err, errNFSMountScope) {
			t.Fatalf("buildCommonRunArgs = %v, want a mount-scope refusal", err)
		}
	})
}

// With the subpath root known, a shared-dir subPath must be in the
// project's tree below it, not only <project>/shared-dirs/<name> in shape.
// On the transitional split (no subpath root) the shape check alone applies.
func TestSharedDirClaimSubPath_MountScope(t *testing.T) {
	r := &SharedDirRealization{
		Backend: "nfs", PVClaimName: scopeClaim, ProjectID: scopeProjectID, SubPathRoot: scopeRoot,
		SubPaths: map[string]string{
			"scratch": scopeRel("shared-dirs", "scratch"),
			"other":   "elsewhere/" + scopeProjectID + "/shared-dirs/other",
		},
	}
	if _, _, err := sharedDirClaimSubPath(r, "scratch"); err != nil {
		t.Errorf("sharedDirClaimSubPath(scratch) = %v, want nil", err)
	}
	if _, _, err := sharedDirClaimSubPath(r, "other"); !errors.Is(err, errNFSMountScope) {
		t.Errorf("sharedDirClaimSubPath(other) = %v, want a mount-scope refusal", err)
	}
	split := *r
	split.SubPathRoot = ""
	if _, _, err := sharedDirClaimSubPath(&split, "other"); err != nil {
		t.Errorf("transitional split: sharedDirClaimSubPath(other) = %v, want the shape check only", err)
	}
}
