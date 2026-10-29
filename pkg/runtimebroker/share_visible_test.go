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
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
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
		if want := scionrt.ProjectShareUnavailableText(scionrt.ProjectShareMountPathField, true); sce.Message != want {
			t.Errorf("message = %q, want %q", sce.Message, want)
		}
		assertNoPathValues(t, f, sce.Message)
		if !errors.Is(sce.OriginalErr, fs.ErrNotExist) {
			t.Errorf("OriginalErr = %v, want the share directory's not-exist error", sce.OriginalErr)
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
	err := f.srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", "")
	if want := scionrt.ProjectShareUnavailableText(scionrt.ProjectShareMountPathField, true); err == nil || err.Error() != want {
		t.Fatalf("checkNFSForDispatch with the share directory absent = %v, want %q", err, want)
	}
	assertNoPathValues(t, f, err.Error())
}

// shareTreeListing lists every path under dir (relative), sorted, for checking that
// a refused dispatch wrote nothing.
func shareTreeListing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertNoPathValues fails when a refusal body names a path or source value
// of the layout (D328: non-admin bodies carry field names only).
func assertNoPathValues(t *testing.T, f *shareVisibleFixture, body string) {
	t.Helper()
	for _, v := range []string{f.hostBase, filepath.Dir(f.hostBase), "10.0.0.2", "/scion-workspaces", "nfs-server"} {
		if strings.Contains(body, v) {
			t.Errorf("refusal body %q carries the layout value %q", body, v)
		}
	}
}

// The main wrong-share case: a verify-only broker whose mount point is an
// empty directory (the share is not really mounted there; no subpath root)
// is refused with 503, the share is not visible, whatever the mount table
// says, with a body that names no path, and nothing is written under the
// mount point.
func TestSharedHubProjectPath_VerifyOnlyEmptyMountPointRefusedNoWrite(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
	if err := os.RemoveAll(filepath.Join(f.hostBase, "projects")); err != nil {
		t.Fatal(err)
	}
	before := shareTreeListing(t, f.hostBase)
	_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
	if !ok || sce == nil || sce.Status != http.StatusServiceUnavailable || sce.Code != ErrCodeProjectStorageUnavailable {
		t.Fatalf("sharedHubProjectPath on an empty mount point = %v, %+v; want the 503 %s refusal", ok, sce, ErrCodeProjectStorageUnavailable)
	}
	if want := scionrt.ProjectShareUnavailableText(scionrt.ProjectShareMountPathField, true); sce.Message != want {
		t.Errorf("message = %q, want the not-visible text %q", sce.Message, want)
	}
	assertNoPathValues(t, f, sce.Message)
	// The stat error is kept for the broker log (OriginalErr); the 503
	// body is sce.Message only.
	if !errors.Is(sce.OriginalErr, fs.ErrNotExist) {
		t.Errorf("OriginalErr = %v, want the subpath root's not-exist error", sce.OriginalErr)
	}
	after := shareTreeListing(t, f.hostBase)
	if len(before) != len(after) {
		t.Fatalf("the refused dispatch wrote under the mount point: before %v, after %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("the refused dispatch changed the mount point: before %v, after %v", before, after)
		}
	}
}

// With the subpath root present but no configuration of this project under
// it, a verify-only broker refuses with 409 ("run project-storage-migrate"
// stays), the body names no path, and nothing is written.
func TestSharedHubProjectPath_VerifyOnlySubpathRootWithoutProjectConfig(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
	if err := os.RemoveAll(filepath.Join(f.hostBase, "projects", sharedRootProjectID)); err != nil {
		t.Fatal(err)
	}
	before := shareTreeListing(t, f.hostBase)
	_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
	if !ok || sce == nil || sce.Status != http.StatusConflict || sce.Message != sharedProjectConfigText {
		t.Fatalf("sharedHubProjectPath = %v, %+v; want the 409 configuration refusal", ok, sce)
	}
	assertNoPathValues(t, f, sce.Message)
	if after := shareTreeListing(t, f.hostBase); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("the refused dispatch changed the mount point: before %v, after %v", before, after)
	}
}

// A broker that mounts its shares gets the mounting form of the
// not-visible text when the subpath root is missing.
func TestSharedHubProjectPath_MountingBrokerMissingSubpathRoot(t *testing.T) {
	f := newShareVisibleFixture(t, "docker", "10.0.0.2:/scion-workspaces")
	if err := os.RemoveAll(filepath.Join(f.hostBase, "projects")); err != nil {
		t.Fatal(err)
	}
	_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
	if !ok || sce == nil || sce.Status != http.StatusServiceUnavailable {
		t.Fatalf("sharedHubProjectPath = %v, %+v; want 503", ok, sce)
	}
	if want := scionrt.ProjectShareUnavailableText(scionrt.ProjectShareMountPathField, false); sce.Message != want {
		t.Errorf("message = %q, want %q", sce.Message, want)
	}
	assertNoPathValues(t, f, sce.Message)
}

// A broker that mounts its shares itself (auto_mount on) and fails to mount
// the share answers with the mount-failure text, never the advice to set
// auto_mount, and the body names no path.
func TestSharedHubProjectPath_MountingBrokerMountFails(t *testing.T) {
	f := newShareVisibleFixture(t, "docker", "") // not in the mount table; the gate server's mounts fail
	if !f.srv.nfsMountReconciler.MountsShares() {
		t.Fatal("a docker broker with auto_mount must mount its shares")
	}
	_, ok, sce := f.srv.sharedHubProjectPath(context.Background(), sharedRootProjectID)
	if !ok || sce == nil || sce.Status != http.StatusServiceUnavailable || sce.Code != ErrCodeProjectStorageUnavailable {
		t.Fatalf("sharedHubProjectPath = %v, %+v; want the 503 %s refusal", ok, sce, ErrCodeProjectStorageUnavailable)
	}
	if want := scionrt.ProjectShareMountFailedText(scionrt.ProjectShareMountPathField); sce.Message != want {
		t.Errorf("message = %q, want %q", sce.Message, want)
	}
	if strings.Contains(sce.Message, "auto_mount") {
		t.Errorf("a broker whose auto_mount is on must not be told to set it: %q", sce.Message)
	}
	assertNoPathValues(t, f, sce.Message)
}

// The NFS dispatch pre-flight of a broker that mounts its shares keeps the
// exact source match: a Kubernetes dispatch on a shared Layout whose share
// is mounted from a cosmetically different source, which the broker failed
// to remount, is refused.
func TestCheckNFSForDispatch_MountingBrokerCosmeticSourceRefused(t *testing.T) {
	mountRoot := t.TempDir()
	nfs := &config.V1NFSConfig{
		MountRoot: mountRoot,
		AutoMount: true,
		Shares:    []config.V1NFSShare{{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"}},
	}
	srv, mc := newGateServer(t, nfs, "docker", "kubernetes")
	hostBase := filepath.Join(mountRoot, "ws1")
	if err := os.MkdirAll(hostBase, 0o755); err != nil {
		t.Fatal(err)
	}
	mc.setMounted(hostBase, "10.0.0.2:/scion-workspaces/")
	srv.config.StorageLayout = projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: hostBase, SubPathRoot: "projects", Share: projectfs.Share{ID: "ws1"}}
	if srv.nfsMountReconciler.VerifyOnly() || !srv.nfsMountReconciler.MountsShares() {
		t.Fatal("a docker broker with auto_mount must mount its shares")
	}
	// Record the share's status: the remount of the mismatched source fails.
	if err := srv.nfsMountReconciler.EnsureShareMounted(context.Background(), "ws1"); err == nil {
		t.Fatal("EnsureShareMounted on a mounting broker with a mismatched source and failing mounts = nil")
	}
	projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\n")
	if err := srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", ""); err == nil {
		t.Fatal("checkNFSForDispatch on a mounting broker with a mismatched source = nil, want the refusal")
	}
}

// shareVisible on a verify-only broker does not take the advisory path when
// the caller's context is done: the reconciler's error is returned, so a
// cancelled check never admits a dispatch.
func TestShareVisible_DoneContextReturnsReconcilerError(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := shareVisible(ctx, f.srv.config.StorageLayout, f.srv.nfsMountReconciler); err == nil {
		t.Fatal("shareVisible with a done context = nil, want the reconciler's error")
	}
	// With a live context the same broker is admitted (the advisory path).
	if err := shareVisible(context.Background(), f.srv.config.StorageLayout, f.srv.nfsMountReconciler); err != nil {
		t.Fatalf("shareVisible with a live context = %v, want nil", err)
	}
}
