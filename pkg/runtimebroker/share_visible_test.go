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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// shareVisibleFixture is a broker with a shared nfs Layout whose share is
// served at <tmp>/ws1 (server 10.0.0.2, export /scion-workspaces), with the
// hub-seeded configuration of sharedRootProjectID under the project root.
type shareVisibleFixture struct {
	srv      *Server
	mc       *syncMountChecker
	hostBase string
	scionDir string
}

// newShareVisibleFixture builds the fixture for a broker whose default
// runtime is brokerRuntime ("kubernetes" gives a verify-only reconciler,
// "docker" with auto_mount a mounting one). mountSource, when not empty, is
// what the mount table shows at the share's mount point.
func newShareVisibleFixture(t *testing.T, brokerRuntime, mountSource string) *shareVisibleFixture {
	t.Helper()
	mountRoot := t.TempDir()
	nfs := &config.V1NFSConfig{
		MountRoot: mountRoot,
		AutoMount: true,
		Shares:    []config.V1NFSShare{{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"}},
	}
	srv, mc := newGateServer(t, nfs, brokerRuntime, brokerRuntime)
	f := &shareVisibleFixture{srv: srv, mc: mc, hostBase: filepath.Join(mountRoot, "ws1")}
	srv.config.StorageLayout = projectfs.Layout{
		Backend:     projectfs.BackendNFS,
		HostBase:    f.hostBase,
		SubPathRoot: "projects",
		Share:       projectfs.Share{ID: "ws1"},
	}
	f.scionDir = filepath.Join(f.hostBase, "projects", sharedRootProjectID, config.DotScion)
	writeSharedSeed(t, f.scionDir, sharedRootProjectID, "web")
	if mountSource != "" {
		mc.setMounted(f.hostBase, mountSource)
	}
	return f
}

// A broker that never mounts (default runtime Kubernetes or Cloud Run) treats
// the mount-table source match as advisory: a share mounted from a source
// that differs from server:export only in form, with a visible share
// directory holding this project's configuration, is admitted. A broker
// that mounts the share keeps the exact match and refuses.
func TestSharedHubProjectPath_CosmeticMountSource(t *testing.T) {
	sources := map[string]string{
		"trailing slash":              "10.0.0.2:/scion-workspaces/",
		"host name for IP":            "nfs-server.internal:/scion-workspaces",
		"not in mount table":          "", // for example a volume the platform bind-mounts
		"exact (unchanged)":           "10.0.0.2:/scion-workspaces",
		"different export (advisory)": "10.0.0.2:/elsewhere", // identity is the project-id check
	}
	for name, src := range sources {
		t.Run("verify-only/"+name, func(t *testing.T) {
			f := newShareVisibleFixture(t, "kubernetes", src)
			if !f.srv.nfsMountReconciler.VerifyOnly() {
				t.Fatal("a kubernetes broker's reconciler must be verify-only")
			}
			path, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
			if sce != nil || !ok {
				t.Fatalf("sharedHubProjectPath = %q, %v, %+v; want admitted", path, ok, sce)
			}
			if want := filepath.Dir(f.scionDir); path != want {
				t.Errorf("project path = %q, want %q", path, want)
			}
			if mounts, _, unmounts := f.mc.counts(); mounts != 0 || unmounts != 0 {
				t.Errorf("a verify-only broker must never mount or unmount (mounts %d, unmounts %d)", mounts, unmounts)
			}
		})
	}
	for _, name := range []string{"trailing slash", "host name for IP"} {
		t.Run("mounting/"+name, func(t *testing.T) {
			f := newShareVisibleFixture(t, "docker", sources[name])
			if f.srv.nfsMountReconciler.VerifyOnly() || !f.srv.nfsMountReconciler.MountsShares() {
				t.Fatal("a docker broker with auto_mount must mount its shares")
			}
			_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
			if !ok || sce == nil || sce.Status != http.StatusServiceUnavailable || sce.Code != ErrCodeProjectStorageUnavailable {
				t.Fatalf("sharedHubProjectPath = %v, %+v; want the 503 %s refusal", ok, sce, ErrCodeProjectStorageUnavailable)
			}
		})
	}
}

// A broker that never mounts still refuses when the share directory is not
// visible (503), and when the directory does not hold this project's
// configuration (409, unchanged).
func TestSharedHubProjectPath_VerifyOnlyRefusals(t *testing.T) {
	t.Run("share directory absent", func(t *testing.T) {
		f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
		if err := os.RemoveAll(f.hostBase); err != nil {
			t.Fatal(err)
		}
		_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
		if !ok || sce == nil || sce.Status != http.StatusServiceUnavailable || sce.Code != ErrCodeProjectStorageUnavailable {
			t.Fatalf("sharedHubProjectPath = %v, %+v; want the 503 %s refusal", ok, sce, ErrCodeProjectStorageUnavailable)
		}
	})
	t.Run("project-id of another project", func(t *testing.T) {
		f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
		writeSharedSeed(t, f.scionDir, "11111111-2222-3333-4444-555555555555", "web")
		_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
		if !ok || sce == nil || sce.Status != http.StatusConflict || sce.Message != sharedProjectConfigText {
			t.Fatalf("sharedHubProjectPath = %v, %+v; want the 409 configuration refusal", ok, sce)
		}
	})
	t.Run("share path is a file", func(t *testing.T) {
		f := newShareVisibleFixture(t, "kubernetes", "")
		if err := os.RemoveAll(f.hostBase); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.hostBase, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
		if !ok || sce == nil || sce.Status != http.StatusServiceUnavailable {
			t.Fatalf("sharedHubProjectPath = %v, %+v; want 503", ok, sce)
		}
	})
}

// A non-shared Layout is untouched: no share check, the caller keeps its
// broker-local project path.
func TestSharedHubProjectPath_NonSharedLayoutUntouched(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "")
	f.srv.config.StorageLayout = projectfs.Layout{Backend: projectfs.BackendLocal}
	if err := os.RemoveAll(f.hostBase); err != nil {
		t.Fatal(err)
	}
	if path, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID); ok || sce != nil || path != "" {
		t.Fatalf("sharedHubProjectPath on a local Layout = %q, %v, %+v; want not handled", path, ok, sce)
	}
}

// The agent look-up on a shared Layout uses the same rule.
func TestFindAgentInSharedProject_VerifyOnlyCosmeticSource(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
	dir, handled, err := f.srv.findAgentInSharedProject(context.Background(), "a1", sharedRootProjectID, true)
	if !handled || err != nil || dir != "" {
		t.Fatalf("findAgentInSharedProject = %q, %v, %v; want handled with no error and no agent state", dir, handled, err)
	}
	f = newShareVisibleFixture(t, "docker", "10.0.0.2:/scion-workspaces/")
	if _, handled, err := f.srv.findAgentInSharedProject(context.Background(), "a1", sharedRootProjectID, true); !handled || err == nil {
		t.Fatalf("mounting broker: findAgentInSharedProject = %v, %v; want a fail-closed error", handled, err)
	}
}

// The NFS dispatch pre-flight of a broker that never mounts admits a
// shared-Layout dispatch when the share directory is visible, whatever the
// reconciler's status, and refuses it when the directory is absent.
func TestCheckNFSForDispatch_VerifyOnlySharedLayout(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
	projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\n")
	if err := f.srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", ""); err != nil {
		t.Fatalf("checkNFSForDispatch with a visible share directory = %v, want nil", err)
	}
	if err := os.RemoveAll(f.hostBase); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", ""); err == nil {
		t.Fatal("checkNFSForDispatch with the share directory absent = nil, want the not-mounted refusal")
	}
}
