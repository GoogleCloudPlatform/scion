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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
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
		// Relative, but with the right suffix: refused as not absolute.
		{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: "rel/" + scopeRel("workspace")},
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

// nfsBindScopeFixture creates <tmp>/<dir>/share1/projects/<pid>/workspace
// and returns the share base and the workspace.
func nfsBindScopeFixture(t *testing.T, dir string) (base, ws string) {
	t.Helper()
	base = filepath.Join(t.TempDir(), dir, "share1")
	ws = filepath.Join(base, filepath.FromSlash(scopeRel("workspace")))
	mkdirs(t, ws)
	return base, ws
}

// A named volume is the container runtime's own storage, never the share,
// whatever the broker's working directory; a relative path that is not a
// named volume is refused rather than resolved against it.
func TestNFSBindScope_NamedVolumeAndRelativeSource(t *testing.T) {
	base, ws := nfsBindScopeFixture(t, "d")
	s, err := newNFSBindScope(RunConfig{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{base, ws} {
		t.Chdir(cwd)
		for _, named := range []string{scopeRoot, "cache", "workspace"} {
			if err := s.check(named); err != nil {
				t.Errorf("cwd %s: check(named volume %q) = %v, want nil", cwd, named, err)
			}
		}
		for _, rel := range []string{"./" + scopeRoot, scopeRel(".scion"), "../share1", "."} {
			if err := s.check(rel); !errors.Is(err, errNFSMountScope) || !strings.Contains(err.Error(), "not an absolute path") {
				t.Errorf("cwd %s: check(%q) = %v, want a refusal of a relative source", cwd, rel, err)
			}
		}
	}
}

// A source on the share that cannot be resolved (a dangling link) is
// refused, not treated as off the share.
func TestNFSBindScope_UnresolvableSourceRefused(t *testing.T) {
	_, ws := nfsBindScopeFixture(t, "d")
	s, err := newNFSBindScope(RunConfig{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(ws, "dangling")
	if err := os.Symlink(filepath.Join(ws, "missing"), dangling); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := s.check(dangling); !errors.Is(err, errNFSMountScope) || !strings.Contains(err.Error(), "cannot be resolved") {
		t.Errorf("check(dangling link) = %v, want a refusal", err)
	}
}

// A share base reached through a link is resolved once; sources are
// compared with its canonical form whichever spelling they use.
func TestNFSBindScope_ShareBaseThroughLink(t *testing.T) {
	realBase, _ := nfsBindScopeFixture(t, "real")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realBase, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	ws := filepath.Join(link, filepath.FromSlash(scopeRel("workspace")))
	s, err := newNFSBindScope(RunConfig{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if want := layoutRealPath(t, realBase); s.hostBase != want {
		t.Fatalf("hostBase = %q, want the canonical %q", s.hostBase, want)
	}
	for _, refused := range []string{
		filepath.Join(realBase, filepath.FromSlash(scopeRel())),
		filepath.Join(link, filepath.FromSlash(scopeRel())),
		filepath.Join(link, filepath.FromSlash(scopeRel(".scion"))),
	} {
		if err := s.check(refused); !errors.Is(err, errNFSMountScope) {
			t.Errorf("check(%s) = %v, want a refusal", refused, err)
		}
	}
	for _, allowed := range []string{
		filepath.Join(realBase, filepath.FromSlash(scopeRel("workspace"))),
		filepath.Join(link, filepath.FromSlash(scopeRel("workspace"))),
	} {
		if err := s.check(allowed); err != nil {
			t.Errorf("check(%s) = %v, want nil", allowed, err)
		}
	}
}

// A share base that is only a volume name becomes that volume's root, so
// a bare drive name never resolves against the working directory on the
// drive. Windows forms are passed in, so every case runs on every OS.
func TestVolumeRooted(t *testing.T) {
	for _, tc := range []struct {
		base, vol string
		sep       byte
		want      string
	}{
		{base: "", vol: "", sep: '/', want: "/"},
		{base: "/mnt/scion/share1", vol: "", sep: '/', want: "/mnt/scion/share1"},
		{base: "Z:", vol: "Z:", sep: '\\', want: `Z:\`},
		{base: `Z:\mnt\share1`, vol: "Z:", sep: '\\', want: `Z:\mnt\share1`},
		{base: `\\server\share`, vol: `\\server\share`, sep: '\\', want: `\\server\share\`},
		{base: `\\server\share\scion`, vol: `\\server\share`, sep: '\\', want: `\\server\share\scion`},
	} {
		if got := volumeRooted(tc.base, tc.vol, tc.sep); got != tc.want {
			t.Errorf("volumeRooted(%q, %q, %q) = %q, want %q", tc.base, tc.vol, tc.sep, got, tc.want)
		}
	}
}

// A share mounted at the root of the file system: the host base is the
// root, and the project root is refused.
func TestNewNFSBindScope_ShareAtVolumeRoot(t *testing.T) {
	root := string(filepath.Separator)
	if v := filepath.VolumeName(t.TempDir()); v != "" {
		root = v + root
	}
	ws := filepath.Join(root, filepath.FromSlash(scopeRel("workspace")))
	s, err := newNFSBindScope(RunConfig{WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := pathcanon.Resolve(root); s.hostBase != want {
		t.Fatalf("hostBase = %q, want the volume root %q", s.hostBase, want)
	}
	if err := s.check(filepath.Join(root, filepath.FromSlash(scopeRel()))); !errors.Is(err, errNFSMountScope) {
		t.Errorf("check(project root at the volume root) = %v, want a refusal", err)
	}
}
