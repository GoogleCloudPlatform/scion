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

package projectfs

import (
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

const testPID = "550e8400-e29b-41d4-a716-446655440000"

func nfsLayout() Layout {
	return Layout{
		Backend:     BackendNFS,
		HostBase:    "/mnt/nfs/hub-a",
		SubPathRoot: "projects",
		Share:       Share{ID: "hub-a", Server: "10.0.0.2", Export: "/share/hub-a", PVName: "hub-a-nfs"},
	}
}

func localLayout() Layout {
	return Layout{Backend: BackendLocal, HostBase: "/home/u/.scion"}
}

func TestResolveMatrix(t *testing.T) {
	scratch := SharedDirMount(api.SharedDir{Name: "scratchpad"}, "")
	custom := SharedDirMount(api.SharedDir{Name: "build-cache", InWorkspace: true}, "/repo-root/ws")
	localProject := Project{ID: testPID, Slug: "acme", Dir: "/home/u/.scion/projects/acme"}

	tests := []struct {
		name       string
		layout     Layout
		project    Project
		agent      string
		mount      Mount
		wantHost   string
		wantRel    string
		wantServer string
		wantTarget string
	}{
		// nfs: workspace, project level, every mode.
		{"nfs/workspace/shared-plain", nfsLayout(), Project{ID: testPID}, "", WorkspaceMount(ModeSharedPlain, ""),
			"/mnt/nfs/hub-a/projects/" + testPID + "/workspace", "projects/" + testPID + "/workspace",
			"/share/hub-a/projects/" + testPID + "/workspace", "/workspace"},
		{"nfs/workspace/worktree base", nfsLayout(), Project{ID: testPID}, "", WorkspaceMount(ModeWorktreePerAgent, ""),
			"/mnt/nfs/hub-a/projects/" + testPID + "/workspace", "projects/" + testPID + "/workspace",
			"/share/hub-a/projects/" + testPID + "/workspace", "/workspace"},
		{"nfs/workspace/empty-per-agent project path", nfsLayout(), Project{ID: testPID}, "", WorkspaceMount(ModeEmptyPerAgent, ""),
			"/mnt/nfs/hub-a/projects/" + testPID + "/workspace", "projects/" + testPID + "/workspace",
			"/share/hub-a/projects/" + testPID + "/workspace", "/workspace"},
		// nfs: workspace, agent level.
		{"nfs/workspace/worktree agent", nfsLayout(), Project{ID: testPID}, "dev-1", WorkspaceMount(ModeWorktreePerAgent, ""),
			"/mnt/nfs/hub-a/projects/" + testPID + "/workspace/worktrees/dev-1", "projects/" + testPID + "/workspace/worktrees/dev-1",
			"/share/hub-a/projects/" + testPID + "/workspace/worktrees/dev-1", "/workspace"},
		{"nfs/workspace/clone agent", nfsLayout(), Project{ID: testPID}, "dev-1", WorkspaceMount(ModeClonePerAgent, ""),
			"/mnt/nfs/hub-a/projects/" + testPID + "/agents/dev-1/workspace", "projects/" + testPID + "/agents/dev-1/workspace",
			"/share/hub-a/projects/" + testPID + "/agents/dev-1/workspace", "/workspace"},
		{"nfs/workspace/empty agent", nfsLayout(), Project{ID: testPID}, "dev-1", WorkspaceMount(ModeEmptyPerAgent, ""),
			"/mnt/nfs/hub-a/projects/" + testPID + "/agents/dev-1/workspace", "projects/" + testPID + "/agents/dev-1/workspace",
			"/share/hub-a/projects/" + testPID + "/agents/dev-1/workspace", "/workspace"},
		{"nfs/workspace/shared-plain ignores agent", nfsLayout(), Project{ID: testPID}, "dev-1", WorkspaceMount(ModeSharedPlain, ""),
			"/mnt/nfs/hub-a/projects/" + testPID + "/workspace", "projects/" + testPID + "/workspace",
			"/share/hub-a/projects/" + testPID + "/workspace", "/workspace"},
		// nfs: shared dirs (the uuid tree agents mount).
		{"nfs/scratchpad", nfsLayout(), Project{ID: testPID}, "", scratch,
			"/mnt/nfs/hub-a/projects/" + testPID + "/shared-dirs/scratchpad", "projects/" + testPID + "/shared-dirs/scratchpad",
			"/share/hub-a/projects/" + testPID + "/shared-dirs/scratchpad", "/scion-volumes/scratchpad"},
		{"nfs/custom in workspace", nfsLayout(), Project{ID: testPID}, "", custom,
			"/mnt/nfs/hub-a/projects/" + testPID + "/shared-dirs/build-cache", "projects/" + testPID + "/shared-dirs/build-cache",
			"/share/hub-a/projects/" + testPID + "/shared-dirs/build-cache", "/repo-root/ws/.scion-volumes/build-cache"},
		{"nfs/custom subpath root", Layout{Backend: BackendNFS, HostBase: "/mnt/nfs/x", SubPathRoot: "team/projects"}, Project{ID: "proj1"}, "", scratch,
			"/mnt/nfs/x/team/projects/proj1/shared-dirs/scratchpad", "team/projects/proj1/shared-dirs/scratchpad", "", "/scion-volumes/scratchpad"},
		{"nfs/default subpath root", Layout{Backend: BackendNFS, HostBase: "/mnt/nfs/x"}, Project{ID: "proj1"}, "", scratch,
			"/mnt/nfs/x/projects/proj1/shared-dirs/scratchpad", "projects/proj1/shared-dirs/scratchpad", "", "/scion-volumes/scratchpad"},
		// local: unchanged slug-keyed paths.
		{"local/workspace/shared-plain", localLayout(), localProject, "", WorkspaceMount(ModeSharedPlain, ""),
			"/home/u/.scion/projects/acme", "", "", "/workspace"},
		{"local/workspace/worktree base", localLayout(), localProject, "", WorkspaceMount(ModeWorktreePerAgent, ""),
			"/home/u/.scion/projects/acme/workspace", "", "", "/workspace"},
		{"local/workspace/clone project path", localLayout(), localProject, "", WorkspaceMount(ModeClonePerAgent, ""),
			"/home/u/.scion/projects/acme", "", "", "/workspace"},
		{"local/scratchpad derived root", localLayout(), localProject, "", scratch,
			"/home/u/.scion/project-configs/acme__550e8400/shared-dirs/scratchpad", "", "", "/scion-volumes/scratchpad"},
		{"local/custom explicit config root", localLayout(), Project{ConfigRoot: "/repo/x/project-configs/r__1234abcd"}, "", custom,
			"/repo/x/project-configs/r__1234abcd/shared-dirs/build-cache", "", "", "/repo-root/ws/.scion-volumes/build-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.layout.Resolve(tt.project, tt.agent, tt.mount)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.HostPath != tt.wantHost {
				t.Errorf("HostPath = %q, want %q", got.HostPath, tt.wantHost)
			}
			if got.RelPath != tt.wantRel {
				t.Errorf("RelPath = %q, want %q", got.RelPath, tt.wantRel)
			}
			if got.SubPath != tt.wantRel {
				t.Errorf("SubPath = %q, want %q", got.SubPath, tt.wantRel)
			}
			if got.ServerPath != tt.wantServer {
				t.Errorf("ServerPath = %q, want %q", got.ServerPath, tt.wantServer)
			}
			if got.Target != tt.wantTarget {
				t.Errorf("Target = %q, want %q", got.Target, tt.wantTarget)
			}
			if got.Backend != tt.layout.Backend {
				t.Errorf("Backend = %q, want %q", got.Backend, tt.layout.Backend)
			}
		})
	}
}

func TestResolveRejects(t *testing.T) {
	scratch := SharedDirMount(api.SharedDir{Name: "scratchpad"}, "")
	named := func(n string) Mount { return SharedDirMount(api.SharedDir{Name: n}, "") }
	tests := []struct {
		name    string
		layout  Layout
		project Project
		agent   string
		mount   Mount
		want    string
	}{
		{"nfs/empty project id", nfsLayout(), Project{}, "", scratch, "project ID is required"},
		{"nfs/traversal project id", nfsLayout(), Project{ID: "../other"}, "", scratch, "invalid project ID"},
		{"nfs/dot project id", nfsLayout(), Project{ID: ".."}, "", scratch, "invalid project ID"},
		{"nfs/slash project id", nfsLayout(), Project{ID: "a/b"}, "", scratch, "invalid project ID"},
		{"nfs/traversal dir name", nfsLayout(), Project{ID: testPID}, "", named("../../x/workspace"), "invalid name"},
		{"nfs/upper dir name", nfsLayout(), Project{ID: testPID}, "", named("Scratch"), "invalid name"},
		{"nfs/empty dir name", nfsLayout(), Project{ID: testPID}, "", named(""), "missing required field"},
		{"nfs/slash dir name", nfsLayout(), Project{ID: testPID}, "", named("a/b"), "invalid name"},
		{"nfs/agent not slug", nfsLayout(), Project{ID: testPID}, "../x", WorkspaceMount(ModeClonePerAgent, ""), "not an agent slug"},
		{"nfs/agent upper", nfsLayout(), Project{ID: testPID}, "Dev", WorkspaceMount(ModeWorktreePerAgent, ""), "not an agent slug"},
		{"nfs/absolute subpath root", Layout{Backend: BackendNFS, HostBase: "/m", SubPathRoot: "/projects"}, Project{ID: testPID}, "", scratch, "must be relative"},
		{"nfs/dotdot subpath root", Layout{Backend: BackendNFS, HostBase: "/m", SubPathRoot: "a/../.."}, Project{ID: testPID}, "", scratch, "clean relative"},
		{"nfs/unknown mode agent", nfsLayout(), Project{ID: testPID}, "dev", Mount{Kind: KindWorkspace, Scope: ScopeAgent, Mode: "bogus"}, "no agent-scoped workspace"},
		{"local/traversal dir name", localLayout(), Project{ID: testPID, Slug: "acme"}, "", named(".."), "invalid name"},
		{"local/bad slug", localLayout(), Project{ID: testPID, Slug: "../acme"}, "", scratch, "invalid project slug"},
		{"local/bad id", localLayout(), Project{ID: "../x", Slug: "acme"}, "", scratch, "invalid project ID"},
		{"local/no host base", Layout{Backend: BackendLocal}, Project{ID: testPID, Slug: "acme"}, "", scratch, "no host base"},
		{"local/no project dir", localLayout(), Project{ID: testPID}, "", WorkspaceMount(ModeSharedPlain, ""), "project directory is required"},
		{"local/agent workspace", localLayout(), Project{Dir: "/p"}, "dev", WorkspaceMount(ModeClonePerAgent, ""), "not resolved here yet"},
		{"unknown backend", Layout{Backend: "gcs"}, Project{ID: testPID}, "", scratch, "unsupported backend"},
		{"unknown kind", nfsLayout(), Project{ID: testPID}, "", Mount{Kind: "volume", Name: "v"}, "unsupported mount kind"},
		{"user owned", nfsLayout(), Project{ID: testPID}, "", Mount{Kind: KindSharedDir, Name: "v", Owner: OwnerUser}, "user-owned"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.layout.Resolve(tt.project, tt.agent, tt.mount)
			if err == nil {
				t.Fatalf("Resolve = %+v, want error containing %q", got, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Resolve error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestProjectAndAgentRoot(t *testing.T) {
	l := nfsLayout()
	pr, err := l.ProjectRoot(testPID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/mnt/nfs/hub-a/projects/" + testPID; pr.HostPath != want {
		t.Errorf("ProjectRoot.HostPath = %q, want %q", pr.HostPath, want)
	}
	if pr.SubPathRoot != "projects" {
		t.Errorf("ProjectRoot.SubPathRoot = %q, want projects", pr.SubPathRoot)
	}
	if d, err := (Layout{Backend: BackendNFS, HostBase: "/m"}).ProjectRoot(testPID); err != nil || d.SubPathRoot != DefaultSubPathRoot {
		t.Errorf("default SubPathRoot = %q, %v", d.SubPathRoot, err)
	}
	ar, err := l.AgentRoot(testPID, "dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/" + testPID + "/agents/dev-1"; ar.RelPath != want {
		t.Errorf("AgentRoot.RelPath = %q, want %q", ar.RelPath, want)
	}
	if _, err := l.AgentRoot(testPID, "../x"); err == nil {
		t.Error("AgentRoot accepted a non-slug agent")
	}
	if _, err := l.ProjectRoot("../x"); err == nil {
		t.Error("ProjectRoot accepted a traversal project ID")
	}
	if _, err := localLayout().ProjectRoot(testPID); err == nil {
		t.Error("ProjectRoot on the local backend should be refused")
	}
}

func TestDeclaredSharedDir(t *testing.T) {
	declared := []api.SharedDir{{Name: "scratchpad"}, {Name: "ro", ReadOnly: true, InWorkspace: true}}
	m, err := DeclaredSharedDir(declared, "ro", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if !m.ReadOnly || m.Target != "/ws/.scion-volumes/ro" || m.Kind != KindSharedDir || m.Scope != ScopeProject {
		t.Errorf("DeclaredSharedDir = %+v", m)
	}
	if _, err := DeclaredSharedDir(declared, "other", ""); !errors.Is(err, ErrNoMount) {
		t.Errorf("undeclared dir: err = %v, want ErrNoMount", err)
	}
}

func TestBackendDurable(t *testing.T) {
	for _, b := range []Backend{BackendNFS, BackendCloudRunVolume, BackendGKESharedVolume} {
		if !b.Durable() || !b.Shared() {
			t.Errorf("%q must be shared and durable", b)
		}
	}
	for _, b := range []Backend{BackendLocal, "", "bogus"} {
		if b.Durable() || b.Shared() {
			t.Errorf("%q must be neither shared nor durable", b)
		}
	}
}

// The platform volume backends resolve the same UUID-keyed tree as nfs
// under their mount point.
func TestResolve_VolumeBackendsUseUUIDTree(t *testing.T) {
	for _, b := range []Backend{BackendCloudRunVolume, BackendGKESharedVolume} {
		l := Layout{Backend: b, HostBase: "/mnt/ws", SubPathRoot: "projects"}
		ws, err := l.Resolve(Project{ID: testPID, Slug: "acme"}, "", WorkspaceMount(ModeSharedPlain, ""))
		if err != nil {
			t.Fatalf("%s: Resolve workspace: %v", b, err)
		}
		if want := "/mnt/ws/projects/" + testPID + "/workspace"; ws.HostPath != want || ws.Backend != b {
			t.Errorf("%s: workspace = %+v, want HostPath %q", b, ws, want)
		}
		root, err := l.ProjectRoot(testPID)
		if err != nil {
			t.Fatalf("%s: ProjectRoot: %v", b, err)
		}
		if want := "/mnt/ws/projects/" + testPID; root.HostPath != want {
			t.Errorf("%s: ProjectRoot = %q, want %q", b, root.HostPath, want)
		}
		if _, err := l.Resolve(Project{Slug: "acme"}, "", WorkspaceMount(ModeSharedPlain, "")); err == nil {
			t.Errorf("%s: Resolve without a project ID must fail", b)
		}
	}
}

func TestLocalConfigDirName(t *testing.T) {
	if got := LocalConfigDirName("acme", testPID); got != "acme__550e8400" {
		t.Errorf("LocalConfigDirName = %q", got)
	}
	if got := LocalConfigDirName("acme", "abc"); got != "acme__abc" {
		t.Errorf("LocalConfigDirName short = %q", got)
	}
}

// TestLayoutKey: the key is a value identity, equal for equal Layouts
// including the SharedDirs split (a pointer), and different when any field
// or the split differs.
func TestLayoutKey(t *testing.T) {
	mk := func() Layout {
		sd := Layout{Backend: BackendLocal}
		return Layout{Backend: BackendGKESharedVolume, HostBase: "/mnt/v", SubPathRoot: "projects", Share: Share{ID: "v", PVName: "c"}, SharedDirs: &sd}
	}
	a, b := mk(), mk()
	if a.Key() != b.Key() {
		t.Errorf("equal Layouts with distinct SharedDirs pointers must share a key: %q vs %q", a.Key(), b.Key())
	}
	c := mk()
	c.SharedDirs = nil
	if c.Key() == a.Key() {
		t.Error("the split must be part of the key")
	}
	d := mk()
	d.SharedDirs.Backend = BackendNFS
	if d.Key() == a.Key() {
		t.Error("the split's fields must be part of the key")
	}
	e := mk()
	e.Share.Export = "/other"
	if e.Key() == a.Key() {
		t.Error("every field must be part of the key")
	}
}
