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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const sharedRootProjectID = "0f9e8d7c-6b5a-4321-8fed-cba987654321"

// sharedRootFixture is a broker whose Layout is nfs on a share "mounted" at
// a temp dir, with the hub-seeded configuration of sharedRootProjectID.
type sharedRootFixture struct {
	srv      *Server
	mgr      *provisionCapturingManager
	layout   projectfs.Layout
	root     string // <host base>/projects/<id>
	scionDir string // <root>/.scion
}

func newSharedRootFixture(t *testing.T, seed bool) *sharedRootFixture {
	t.Helper()
	setupTestScionEnv(t)
	hostBase := filepath.Join(t.TempDir(), "share-a")
	layout := projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: hostBase, SubPathRoot: "projects",
		Share: projectfs.Share{ID: "share-a", Server: "10.0.0.2", Export: "/vol-a"}, Homes: projectfs.BackendLocal}
	f := &sharedRootFixture{layout: layout}
	f.root = filepath.Join(hostBase, "projects", sharedRootProjectID)
	f.scionDir = filepath.Join(f.root, config.DotScion)
	if err := os.MkdirAll(hostBase, 0o755); err != nil {
		t.Fatal(err)
	}
	if seed {
		writeSharedSeed(t, f.scionDir, sharedRootProjectID, "web")
	}
	f.srv, f.mgr = newSharedLayoutServer(t, layout)
	return f
}

func newSharedLayoutServer(t *testing.T, layout projectfs.Layout) (*Server, *provisionCapturingManager) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	cfg.StorageLayout = layout
	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	return srv, mgr
}

// writeSharedSeed writes what the hub seeds at <ProjectRoot>/.scion.
func writeSharedSeed(t *testing.T, scionDir, projectID, slug string) {
	t.Helper()
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		projectkeys.ProjectIDFile: projectID + "\n",
		"settings.yaml":           "schema_version: \"1\"\n",
	}
	if slug != "" {
		files[projectkeys.ProjectSlugFile] = slug + "\n"
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(scionDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// treeListing lists every path under dir with its size, for a no-write check.
func treeListing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %d %v", strings.TrimPrefix(p, dir), info.Size(), info.Mode()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func postSharedCreate(t *testing.T, srv *Server, projectID, agent string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{
		"name": %q, "id": "agent-uuid-%s", "slug": %q,
		"projectId": %q, "projectSlug": "web",
		"provisionOnly": true,
		"config": {"template": "claude", "sharedWorkspace": true}
	}`, agent, agent, agent, projectID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// On a shared Layout a hub-managed project dispatched by slug runs from
// Layout.ProjectRoot(uuid) (ptone/scion#5823 option (a)); the broker writes
// nothing into the hub-seeded configuration, records its state keyed by the
// project root, and keeps the agents root broker-local.
func TestCreateAgent_SharedLayoutRunsFromProjectRoot(t *testing.T) {
	f := newSharedRootFixture(t, true)
	before := treeListing(t, f.scionDir)

	w := postSharedCreate(t, f.srv, sharedRootProjectID, "a1")
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if !f.mgr.provisionCalled || f.mgr.lastOpts.ProjectPath != f.root {
		t.Fatalf("ProjectPath = %q, want the Layout's project root %q", f.mgr.lastOpts.ProjectPath, f.root)
	}
	if after := treeListing(t, f.scionDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("the broker wrote into the shared project configuration:\nbefore %v\nafter  %v", before, after)
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(globalDir, "projects", "web")); !os.IsNotExist(err) {
		t.Errorf("no broker-local ~/.scion/projects/<slug> may be created on a shared Layout (err %v)", err)
	}
	agents := filepath.Join(globalDir, config.ProjectConfigsDir, "web__0f9e8d7c", config.DotScion, "agents")
	if info, err := os.Stat(agents); err != nil || !info.IsDir() {
		t.Errorf("the broker-local external agents root %s should exist: %v", agents, err)
	}

	canonical, err := CanonicalWorkspacePath(f.root)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(canonical))
	record := filepath.Join(globalDir, workspaceConfigRecordsDir, hex.EncodeToString(sum[:16])+".json")
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the workspace configuration record keyed by the project root is missing: %v", err)
	}
	if !strings.Contains(string(data), `"kind":"directory"`) || !strings.Contains(string(data), sharedRootProjectID) {
		t.Errorf("record = %s, want the directory state with the project ID", data)
	}
}

// Refusals (ptone/scion#5823): nothing is created or
// repaired, and Provision is never called.
func TestCreateAgent_SharedLayoutRefusals(t *testing.T) {
	cases := []struct {
		name       string
		prepare    func(t *testing.T, f *sharedRootFixture)
		projectID  string
		wantStatus int
		wantText   string
	}{
		{"no configuration", func(t *testing.T, f *sharedRootFixture) {}, sharedRootProjectID, http.StatusConflict, sharedProjectConfigText},
		{"project-id of another project", func(t *testing.T, f *sharedRootFixture) {
			writeSharedSeed(t, f.scionDir, "11111111-2222-3333-4444-555555555555", "web")
		}, sharedRootProjectID, http.StatusConflict, sharedProjectConfigText},
		{"no project-slug", func(t *testing.T, f *sharedRootFixture) {
			writeSharedSeed(t, f.scionDir, sharedRootProjectID, "")
		}, sharedRootProjectID, http.StatusConflict, sharedProjectNoSlugText},
		{"configuration is a symlink", func(t *testing.T, f *sharedRootFixture) {
			elsewhere := filepath.Join(t.TempDir(), sharedRootProjectID, config.DotScion)
			writeSharedSeed(t, elsewhere, sharedRootProjectID, "web")
			if err := os.MkdirAll(f.root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, f.scionDir); err != nil {
				t.Fatal(err)
			}
		}, sharedRootProjectID, http.StatusConflict, sharedProjectConfigText},
		{"share not mounted", func(t *testing.T, f *sharedRootFixture) {
			if err := os.RemoveAll(f.layout.HostBase); err != nil {
				t.Fatal(err)
			}
		}, sharedRootProjectID, http.StatusServiceUnavailable, "project storage share is not mounted on this broker"},
		{"no project ID", func(t *testing.T, f *sharedRootFixture) {
			writeSharedSeed(t, f.scionDir, sharedRootProjectID, "web")
		}, "", http.StatusConflict, sharedProjectNoIDText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSharedRootFixture(t, false)
			tc.prepare(t, f)
			w := postSharedCreate(t, f.srv, tc.projectID, "a1")
			if w.Code != tc.wantStatus || !strings.Contains(w.Body.String(), tc.wantText) {
				t.Fatalf("create = %d %s, want %d containing %q", w.Code, w.Body.String(), tc.wantStatus, tc.wantText)
			}
			if f.mgr.provisionCalled {
				t.Error("Provision must not run after a refusal")
			}
			if tc.name == "no configuration" {
				if _, err := os.Lstat(f.scionDir); !os.IsNotExist(err) {
					t.Errorf("the broker must not create the configuration (err %v)", err)
				}
			}
		})
	}
}

// A start by slug resolves the same project root (buildStartContext).
func TestStartAgent_SharedLayoutRunsFromProjectRoot(t *testing.T) {
	f := newSharedRootFixture(t, true)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/start?projectId="+sharedRootProjectID, strings.NewReader(`{"projectSlug": "web"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start = %d: %s", w.Code, w.Body.String())
	}
	if !f.mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	if got := f.mgr.lastOpts.ProjectPath; got != f.root {
		t.Fatalf("start ProjectPath = %q, want %q", got, f.root)
	}
}

// Two brokers of one project share its Mounts (the same project root and
// configuration) and never each other's broker-private state: agent state
// roots and the ownership store live under each broker's own HOME, keyed by
// project ID.
func TestSharedLayout_TwoBrokersShareMountsNotJournals(t *testing.T) {
	f := newSharedRootFixture(t, true)
	homeA, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if w := postSharedCreate(t, f.srv, sharedRootProjectID, "a1"); w.Code != http.StatusCreated {
		t.Fatalf("broker A create = %d: %s", w.Code, w.Body.String())
	}
	pathA := f.mgr.lastOpts.ProjectPath

	homeB := t.TempDir()
	t.Setenv("HOME", homeB)
	srvB, mgrB := newSharedLayoutServer(t, f.layout)
	if w := postSharedCreate(t, srvB, sharedRootProjectID, "b1"); w.Code != http.StatusCreated {
		t.Fatalf("broker B create = %d: %s", w.Code, w.Body.String())
	}
	if mgrB.lastOpts.ProjectPath != pathA {
		t.Fatalf("broker B ProjectPath = %q, want the same project root %q", mgrB.lastOpts.ProjectPath, pathA)
	}
	rootA := filepath.Join(homeA, config.GlobalDir, config.ProjectConfigsDir, "web__0f9e8d7c", config.DotScion, "agents")
	rootB := filepath.Join(homeB, config.GlobalDir, config.ProjectConfigsDir, "web__0f9e8d7c", config.DotScion, "agents")
	t.Setenv("HOME", homeA)
	gotA, err := config.AgentsRootForProject(f.scionDir, false, sharedRootProjectID)
	if err != nil || gotA != rootA {
		t.Fatalf("broker A agents root = (%q, %v), want %q", gotA, err, rootA)
	}
	t.Setenv("HOME", homeB)
	gotB, err := config.AgentsRootForProject(f.scionDir, false, sharedRootProjectID)
	if err != nil || gotB != rootB {
		t.Fatalf("broker B agents root = (%q, %v), want %q", gotB, err, rootB)
	}
	if strings.HasPrefix(f.srv.stateDir, homeB) || strings.HasPrefix(srvB.stateDir, homeA) || f.srv.stateDir == srvB.stateDir {
		t.Fatalf("broker state dirs (ownership store, launch journal) must be private: A %q, B %q", f.srv.stateDir, srvB.stateDir)
	}
	for _, p := range []string{rootA, rootB, f.srv.stateDir, srvB.stateDir} {
		if strings.HasPrefix(p, f.layout.HostBase) {
			t.Errorf("broker-private state %q is on the share", p)
		}
	}
}

// Agent look-ups for delete find a shared project's agent by its state in
// the external root.
func TestFindAgentInSharedProject(t *testing.T) {
	f := newSharedRootFixture(t, true)
	if got := f.srv.findAgentInSharedProject("a1", sharedRootProjectID); got != "" {
		t.Fatalf("no agent yet: got %q", got)
	}
	root, err := config.AgentsRootForProject(f.scionDir, false, sharedRootProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.findAgentInSharedProject("a1", sharedRootProjectID); got != f.scionDir {
		t.Fatalf("findAgentInSharedProject = %q, want %q", got, f.scionDir)
	}
	if got := f.srv.findAgentInSharedProject("a1", ""); got != "" {
		t.Errorf("an unscoped look-up must find nothing here, got %q", got)
	}
	if got := f.srv.findAgentInSharedProject("../a1", sharedRootProjectID); got != "" {
		t.Errorf("a non-element agent name must find nothing, got %q", got)
	}
}

// Start-up discovery reads the settings of shared projects this broker has
// agents in, and only those.
func TestSharedProjectsWithLocalAgents(t *testing.T) {
	f := newSharedRootFixture(t, true)
	other := filepath.Join(f.layout.HostBase, "projects", "11111111-2222-3333-4444-555555555555", config.DotScion)
	writeSharedSeed(t, other, "11111111-2222-3333-4444-555555555555", "other")
	if got := f.srv.sharedProjectsWithLocalAgents(); len(got) != 0 {
		t.Fatalf("no local agents yet: got %v", got)
	}
	root, err := config.AgentsRootForProject(f.scionDir, false, sharedRootProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := f.srv.sharedProjectsWithLocalAgents()
	if len(got) != 1 || got[0] != f.scionDir {
		t.Fatalf("sharedProjectsWithLocalAgents = %v, want [%s]", got, f.scionDir)
	}
}

// On a shared Layout a warn-only (Kubernetes, Cloud Run) dispatch whose share
// is not mounted on the broker is refused (ptone/scion#5823); on a
// local Layout it is still only warned about.
func TestCheckNFSForDispatch_WarnOnlyRefusedOnSharedLayout(t *testing.T) {
	for _, shared := range []bool{true, false} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			srv, _ := newGateServer(t, nfsCfg(true), "docker", "kubernetes")
			if shared {
				srv.config.StorageLayout = projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: "/mnt/nfs/ws1", SubPathRoot: "projects"}
			}
			projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\n")
			err := srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", "")
			if shared {
				if err == nil || !strings.Contains(err.Error(), "project storage share is not mounted on this broker (/mnt/nfs/ws1;") {
					t.Fatalf("checkNFSForDispatch = %v, want the not-mounted refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkNFSForDispatch on a local Layout = %v, want nil (warn only)", err)
			}
		})
	}
}
