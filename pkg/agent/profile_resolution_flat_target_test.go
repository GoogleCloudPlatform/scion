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

package agent

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Flat Runtime Broker instance storage policy on agent start
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC5, FC9): the
// instance's runtime_target shared-dir and home storage backends take the
// profile tier's rung for new agents, and an agent's recorded decisions
// are kept.

// flatPolicies strictly loads the instances of the global settings file
// and returns each one's policy by key, as the host builds them.
func flatPolicies(t *testing.T) map[string]*config.FlatInstancePolicy {
	t.Helper()
	instances, err := config.LoadRuntimeBrokerInstances("")
	require.NoError(t, err)
	out := map[string]*config.FlatInstancePolicy{}
	for _, inst := range instances {
		out[inst.Key] = config.NewFlatInstancePolicy(inst)
	}
	return out
}

func flatStorageOpts(f sharedDirStorageRunFixture, agent, agentID string, sharedDirs ...string) api.StartOptions {
	opts := api.StartOptions{
		Name:        agent,
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         map[string]string{"SCION_AGENT_ID": agentID, "SCION_PROJECT_ID": hsTestProjectID},
	}
	for _, d := range sharedDirs {
		opts.SharedDirs = append(opts.SharedDirs, api.SharedDir{Name: d})
	}
	return opts
}

// TestFlatPolicy_HomeStorageNoSharedDirs (fixture
// home_storage_no_shared_dirs): an agent with no shared dirs resolves its
// home from the home-storage settings load, not from the shared-dir
// snapshot. On a flat instance that load must carry the instance's policy:
// the agent gets, and records, the instance's local home, not the active
// profile's NFS home.
func TestFlatPolicy_HomeStorageNoSharedDirs(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	settings, err := os.ReadFile(filepath.Join("..", "config", "testdata", "profile_conversion", "home_storage_no_shared_dirs", "settings.yaml"))
	require.NoError(t, err)
	f.writeRawGlobalSettings(t, string(settings))
	policy := flatPolicies(t)["k8s"]
	require.NotNil(t, policy)

	var c sdsCapture
	ctx := config.WithFlatInstancePolicy(withNFSHomeExperiment(context.Background()), policy)
	_, err = NewManager(newSDSMockRuntime("kubernetes", &c)).Start(ctx, flatStorageOpts(f, "flat-agent", hsTestAgentID))
	require.NoError(t, err)
	require.Equal(t, 1, c.ran)
	rec := startHomeRecord(t, f, "flat-agent")
	if rec == nil || rec.Backend != "local" {
		t.Fatalf("flat agent home record is %+v, want backend local (the instance's), not the active profile's nfs", rec)
	}
	assert.Empty(t, c.cfg.HomeStorageBackend, "the flat agent gets a local home")

	// Control: a legacy start of the same settings takes the active
	// profile's NFS home.
	var legacy sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &legacy)).Start(withNFSHomeExperiment(context.Background()),
		flatStorageOpts(f, "legacy-agent", "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"))
	require.NoError(t, err)
	assert.Equal(t, runtime.HomeStorageNFS, legacy.cfg.HomeStorageBackend)
	assert.Equal(t, "nfs", startHomeRecord(t, f, "legacy-agent").Backend)
}

// flatTwoInstanceSettings: one process, a Docker instance on the local
// backend and a Kubernetes instance on NFS for shared dirs and home. The
// active profile selects nfs (which the Docker instance must not see) and
// the global backend is local (which the Kubernetes instance's own value
// must override).
func flatTwoInstanceSettings(mountRoot string) string {
	return fmt.Sprintf(`schema_version: "1"
active_profile: batch
runtimes:
  cluster:
    type: kubernetes
profiles:
  batch:
    runtime: cluster
    shared_dir_storage_backend: nfs
server:
  home_storage:
    allow_incomplete_phases: true
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          pv_name: pv-1
  broker:
    instances:
      - key: docker-1
        name: docker-1
        runtime_target:
          type: docker
          shared_dir_storage_backend: local
      - key: k8s-nfs-1
        name: k8s-nfs-1
        runtime_target:
          type: kubernetes
          namespace: agents
          shared_dir_size: 40Gi
          shared_dir_storage_backend: nfs
          home_storage_backend: nfs
`, mountRoot)
}

// TestFlatPolicy_DockerLocalAndKubernetesNFSOneProcess: creates on the two
// instances run concurrently in one process. The Docker agent records the
// local shared-dir backend and keeps a local home; the Kubernetes agent
// records NFS shared dirs and an NFS home, with the instance's 40Gi PVC
// default.
func TestFlatPolicy_DockerLocalAndKubernetesNFSOneProcess(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o775))
	f.writeRawGlobalSettings(t, flatTwoInstanceSettings(mountRoot))
	policies := flatPolicies(t)
	require.Len(t, policies, 2)

	var docker, k8s sdsCapture
	runs := []struct {
		capture *sdsCapture
		runtime string
		policy  *config.FlatInstancePolicy
		opts    api.StartOptions
	}{
		{&docker, "docker", policies["docker-1"], flatStorageOpts(f, "docker-agent", "2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6d", "data")},
		{&k8s, "kubernetes", policies["k8s-nfs-1"], flatStorageOpts(f, "k8s-agent", hsTestAgentID, "data")},
	}
	errs := make([]error, len(runs))
	var wg sync.WaitGroup
	for i, r := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := config.WithFlatInstancePolicy(withNFSHomeExperiment(context.Background()), r.policy)
			_, errs[i] = NewManager(newSDSMockRuntime(r.runtime, r.capture)).Start(ctx, r.opts)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "start on %s", runs[i].runtime)
	}

	// Docker instance: local shared dirs and a local home, no NFS path.
	require.Equal(t, 1, docker.ran)
	assert.Nil(t, docker.cfg.SharedDirStorage)
	assert.NotContains(t, sdsDataDirSource(docker.cfg), mountRoot, "the docker instance keeps the local layout")
	assert.Equal(t, "local", sdsRecordedBackend(t, f, "docker-agent"))
	assert.Empty(t, docker.cfg.HomeStorageBackend)

	// Kubernetes instance: NFS shared dirs and an NFS home, both recorded.
	require.Equal(t, 1, k8s.ran)
	require.NotNil(t, k8s.cfg.SharedDirStorage, "the kubernetes instance resolves its own nfs backend")
	assert.Equal(t, "nfs", k8s.cfg.SharedDirStorage.Backend)
	assert.Equal(t, "pv-1", k8s.cfg.SharedDirStorage.PVClaimName)
	assert.Equal(t, "nfs", sdsRecordedBackend(t, f, "k8s-agent"))
	assert.Equal(t, runtime.HomeStorageNFS, k8s.cfg.HomeStorageBackend)
	assert.Equal(t, &homeStorageRecord{Backend: "nfs", Leaf: "pod", ShareID: "share-1", PVClaimName: "pv-1", SubPathRoot: "projects"},
		startHomeRecord(t, f, "k8s-agent"))
	require.NotNil(t, k8s.cfg.Kubernetes)
	assert.Equal(t, "40Gi", k8s.cfg.Kubernetes.SharedDirSize)
}

// TestFlatPolicy_SavedStorageRecordsKept: an agent that recorded NFS shared
// dirs and an NFS home under a legacy profile keeps both when a flat
// instance whose policy says local serves it. The policy only decides a
// new agent's first start.
func TestFlatPolicy_SavedStorageRecordsKept(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o775))
	f.writeRawGlobalSettings(t, fmt.Sprintf(`schema_version: "1"
active_profile: batch
runtimes:
  cluster:
    type: kubernetes
profiles:
  batch:
    runtime: cluster
    shared_dir_storage_backend: nfs
    home_storage_backend: nfs
server:
  home_storage:
    allow_incomplete_phases: true
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          pv_name: pv-1
  broker:
    instances:
      - key: k8s-local
        name: k8s-local
        runtime_target:
          type: kubernetes
          shared_dir_storage_backend: local
          home_storage_backend: local
`, mountRoot))
	ctx := withNFSHomeExperiment(context.Background())
	opts := flatStorageOpts(f, "agent", hsTestAgentID, "data")

	var legacy sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &legacy)).Start(ctx, opts)
	require.NoError(t, err)
	require.Equal(t, "nfs", sdsRecordedBackend(t, f, "agent"))
	require.Equal(t, "nfs", startHomeRecord(t, f, "agent").Backend)

	var flat sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &flat)).Start(config.WithFlatInstancePolicy(ctx, flatPolicies(t)["k8s-local"]), opts)
	require.NoError(t, err)
	require.Equal(t, 1, flat.ran)
	require.NotNil(t, flat.cfg.SharedDirStorage, "the recorded nfs backend is kept")
	assert.Equal(t, "nfs", flat.cfg.SharedDirStorage.Backend)
	assert.Equal(t, "nfs", sdsRecordedBackend(t, f, "agent"))
	assert.Equal(t, runtime.HomeStorageNFS, flat.cfg.HomeStorageBackend, "the recorded nfs home is kept")
	assert.Equal(t, "nfs", startHomeRecord(t, f, "agent").Backend)
}

// --- Source scan: settings loads on the start, provision and reprovision
// paths (B3). ---

// nonForSettingsLoaders are the pkg/config settings loaders that return
// the loaded settings without the request's view, so a flat instance's
// policy never reaches what they return.
var nonForSettingsLoaders = map[string]bool{
	"LoadEffectiveSettings": true, "LoadGlobalSettings": true, "LoadGlobalSettingsWithOverlay": true,
	"LoadSettings": true, "LoadVersionedSettings": true, "LoadSettingsKoanf": true,
	"LoadSettingsIgnoringEnvProjectID": true, "LoadSettingsFromDir": true, "LoadSingleFileVersioned": true,
}

// nonForLoadAllowList is every reviewed non-For settings load in pkg/agent,
// keyed "file func loader", with the reason it needs no flat policy.
var nonForLoadAllowList = map[string]string{
	"list.go lazyHubProjectID LoadEffectiveSettings":                       "listing: reads only the project's hub settings",
	"nfs_worktree.go nfsExportWorkspaceOnAnyRuntime LoadEffectiveSettings": "reads only server.workspace_storage, which has no profile tier",
	"run.go AgentManager.Start LoadSettings":                               "reads only the project's hub settings (legacy settings type)",
}

// TestSettingsLoads_FlatPathsUseRequestView: every settings load in
// pkg/agent's production code goes through a For loader (which applies the
// flat instance policy of the request), except the reviewed sites in
// nonForLoadAllowList. A new or reverted load fails here.
func TestSettingsLoads_FlatPathsUseRequestView(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	found := map[string]bool{}
	var unreviewed []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fnName := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					fnName = id.Name + "." + fnName
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "config" && nonForSettingsLoaders[sel.Sel.Name] {
					key := name + " " + fnName + " " + sel.Sel.Name
					if _, ok := nonForLoadAllowList[key]; ok {
						found[key] = true
					} else {
						unreviewed = append(unreviewed, fmt.Sprintf("%s (%s)", key, fset.Position(sel.Pos())))
					}
				}
				return true
			})
		}
	}
	sort.Strings(unreviewed)
	if len(unreviewed) > 0 {
		t.Fatalf("settings loads that bypass the request's flat instance view (use LoadEffectiveSettingsFor or LoadGlobalSettingsWithOverlayFor, or review the site and add it to nonForLoadAllowList):\n%s",
			strings.Join(unreviewed, "\n"))
	}
	for key := range nonForLoadAllowList {
		if !found[key] {
			t.Errorf("allow-list entry %q matches no load; remove it", key)
		}
	}
}
