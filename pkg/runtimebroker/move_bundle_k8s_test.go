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

//go:build unix

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	k8sBrokerTestProjectID = "11111111-2222-3333-4444-555555555555"
	k8sBrokerTestAgentID   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func setupK8sBrokerNFSFixture(t *testing.T) (srv *Server, mountRoot, hostBase, projectPath string) {
	t.Helper()
	// Create test server first because newTestServer overrides HOME.
	srv = newK8sTestServer(t)

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))

	mountRoot = filepath.Join(tmp, "nfs-mounts")
	hostBase = filepath.Join(mountRoot, "share-1")
	require.NoError(t, os.MkdirAll(hostBase, 0o755))

	globalDir := filepath.Join(tmp, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: gke
runtimes:
  kubernetes:
    type: kubernetes
profiles:
  gke:
    runtime: kubernetes
server:
  project_storage:
    backend: nfs
    homes: nfs
    homes_options:
      leaf: pod
      allow_incomplete_phases: true
    nfs:
      mount_root: %s
      subpath_root: projects
      uid: 2000
      gid: 3000
      shares:
        - id: share-1
          server: 10.0.0.10
          export: /export
          pv_name: pv-1
`, mountRoot)
	require.NoError(t, config.InitGlobal(nil, config.InitMachineOpts{SkipRuntimeCheck: true}))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))

	projectRoot := filepath.Join(globalDir, "projects", "demo")
	require.NoError(t, os.MkdirAll(projectRoot, 0o755))
	marker := &config.ProjectMarker{
		ProjectID:   k8sBrokerTestProjectID,
		ProjectName: "demo",
		ProjectSlug: "demo",
	}
	require.NoError(t, config.WriteProjectMarker(filepath.Join(projectRoot, config.DotScion), marker))
	extDir, err := marker.ExternalProjectPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(extDir, 0o755))
	projectPath = projectRoot
	return srv, mountRoot, hostBase, projectPath
}

func newK8sTestServer(t *testing.T) *Server {
	t.Helper()
	rt := &scionrt.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
	}
	mgr := agent.NewManager(rt)
	srv := newTestServer(t)
	srv.runtime = rt
	srv.manager = mgr
	return srv
}

func TestMoveBundle_K8sNFS_SourcePrecheckExportAndSymlinks(t *testing.T) {
	srv, _, hostBase, projectPath := setupK8sBrokerNFSFixture(t)
	slug := "k8s-src"

	// Create broker-local agentDir with home-storage.json (backend=nfs) and .scion-move-bundle-home.json.
	dirs, err := agent.ResolveMoveBundleAgentDirs(context.Background(), "kubernetes", api.StartOptions{
		Name:         slug,
		ProjectPath:  projectPath,
		HubProjectID: k8sBrokerTestProjectID,
		BrokerMode:   true,
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dirs.AgentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dirs.AgentDir, "home-storage.json"), []byte(`{
  "backend": "nfs",
  "leaf": "pod",
  "share_id": "share-1",
  "pv_claim_name": "pv-1",
  "subpath_root": "projects"
}
`), 0o600))
	writeTestHomeRecord(t, dirs.AgentDir, scionrt.RunConfig{
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "api_key", Type: "file", Target: "~/.config/app/secret.key"},
		},
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{
				{SourcePath: "/host/creds", ContainerPath: "~/.claude/.credentials.json"},
			},
		},
	})

	// Populate K8s NFS workspace and NFS home leaf (home-<agentID>).
	realHostBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	exportAgentDir := filepath.Join(realHostBase, "projects", k8sBrokerTestProjectID, "agents", slug)
	wsDir := filepath.Join(exportAgentDir, "workspace")
	homeDir := filepath.Join(exportAgentDir, "home-"+k8sBrokerTestAgentID)
	require.NoError(t, os.MkdirAll(filepath.Join(wsDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "app.go"), []byte("package app\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(homeDir, ".config", "app"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, "notes.txt"), []byte("k8s notes"), 0o600))

	// Staged secrets and auth on K8s are leaf symlinks pointing to /scion-staging/...
	// (outside home). They must be excluded without failing precheck or export.
	require.NoError(t, os.Symlink("/scion-staging/secret-0/secret.key", filepath.Join(homeDir, ".config", "app", "secret.key")))
	require.NoError(t, os.Symlink("/scion-staging/auth-0/credentials.json", filepath.Join(homeDir, ".claude", ".credentials.json")))
	// Another absolute symlink to /scion-staging/... (outside home) is skipped as an escaping symlink, not failed.
	require.NoError(t, os.Symlink("/scion-staging/other/file", filepath.Join(homeDir, "extra-staged-link")))

	// 1a. Precheck without agentId in URL or body is refused (cannot resolve home-<agentID>).
	prePath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/precheck?projectId=%s", slug, k8sBrokerTestProjectID)
	recNoID := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, recNoID.Code, recNoID.Body.String())
	var preRespNoID api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(recNoID.Body.Bytes(), &preRespNoID))
	assert.False(t, preRespNoID.Durable, "K8s precheck without agentId must fail closed")

	// 1b. Precheck with AgentID in JSON body (no agentId URL query param, matching Hub dispatch) reports durable=true.
	rec := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		AgentID:     k8sBrokerTestAgentID,
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preResp api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preResp))
	assert.True(t, preResp.Durable, "K8s NFS workspace + NFS home must be durable")
	assert.Empty(t, preResp.Reason)
	assert.Equal(t, int64(len("package app\n")), preResp.Workspace.Bytes)
	assert.Equal(t, int64(len("k8s notes")), preResp.Home.Bytes)

	// 2. Export succeeds and uploads only non-excluded workspace + home bytes.
	var mu sync.Mutex
	parts := map[string][]byte{}
	objStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.Method == http.MethodPut {
			var buf []byte
			buf, _ = readAllBody(r)
			mu.Lock()
			parts[key] = buf
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer objStore.Close()

	srv.moveBundle.fetchUploadURL = func(_ context.Context, _, _, _ string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
		return &api.MoveBundleUploadURLResponse{
			URL:    fmt.Sprintf("%s/%s/%d", objStore.URL, req.Root, req.PartIndex),
			Method: http.MethodPut,
		}, nil
	}

	rid := "rid-k8s-export-1"
	expPath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/export?projectId=%s&agentId=%s", slug, k8sBrokerTestProjectID, k8sBrokerTestAgentID)
	expRec := doMoveBundleReq(t, srv, http.MethodPost, expPath, api.MoveBundleExportRequest{
		ReincarnationID: rid,
		AgentID:         k8sBrokerTestAgentID,
		ProjectID:       k8sBrokerTestProjectID,
		ProjectPath:     projectPath,
		Limits:          api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusAccepted, expRec.Code, expRec.Body.String())

	st := waitForMoveBundleJob(t, srv, slug, k8sBrokerTestProjectID, api.MoveBundleJobKindExport, rid)
	require.Equal(t, api.MoveBundleJobStateSucceeded, st.State, "error=%s", st.Error)
	require.NotNil(t, st.Manifest)
	assert.Equal(t, int64(len("package app\n")), st.Manifest.Roots[bundle.RootWorkspace].Bytes)
	assert.Equal(t, int64(len("k8s notes")), st.Manifest.Roots[bundle.RootHome].Bytes)
	assert.GreaterOrEqual(t, st.Manifest.Roots[bundle.RootHome].Skipped.Excluded, int64(2))
	assert.Equal(t, int64(1), st.Manifest.Roots[bundle.RootHome].Skipped.EscapingSymlinks)

	// 3. Ancestor symlink to /scion-staging/... fails closed in precheck.
	require.NoError(t, os.RemoveAll(filepath.Join(homeDir, ".claude")))
	require.NoError(t, os.Symlink("/scion-staging/auth-dir", filepath.Join(homeDir, ".claude")))
	recAnc := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		AgentID:     k8sBrokerTestAgentID,
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, recAnc.Code)
	var ancResp api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(recAnc.Body.Bytes(), &ancResp))
	assert.False(t, ancResp.Durable, "ancestor symlink to /scion-staging/... must fail closed")
	assert.Equal(t, "source home contains an unportable symlink or hardlink", ancResp.Reason)
}

func TestMoveBundle_K8sWithoutNFSHome_Refused(t *testing.T) {
	srv, _, _, projectPath := setupK8sBrokerNFSFixture(t)
	slug := "k8s-no-nfshome"

	dirs, err := agent.ResolveMoveBundleAgentDirs(context.Background(), "kubernetes", api.StartOptions{
		Name:         slug,
		ProjectPath:  projectPath,
		HubProjectID: k8sBrokerTestProjectID,
		BrokerMode:   true,
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dirs.AgentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dirs.AgentDir, "home-storage.json"), []byte(`{"backend":"local"}`), 0o600))

	prePath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/precheck?projectId=%s", slug, k8sBrokerTestProjectID)
	rec := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		AgentID:     k8sBrokerTestAgentID,
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var preResp api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preResp))
	assert.False(t, preResp.Durable)
	assert.Equal(t, "agent home lives only in the pod and is deleted on stop", preResp.Reason)

	expPath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/export?projectId=%s&agentId=%s", slug, k8sBrokerTestProjectID, k8sBrokerTestAgentID)
	expRec := doMoveBundleReq(t, srv, http.MethodPost, expPath, api.MoveBundleExportRequest{
		ReincarnationID: "rid-no-nfshome",
		AgentID:         k8sBrokerTestAgentID,
		ProjectID:       k8sBrokerTestProjectID,
		ProjectPath:     projectPath,
		Limits:          api.DefaultMoveBundleLimits(),
	})
	assert.Equal(t, http.StatusConflict, expRec.Code)
	assert.Contains(t, expRec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)
}

func TestMoveBundle_PlainK8s_Refused(t *testing.T) {
	projectPath, _, _, _ := newMoveBundleTestAgentDir(t, "plain-k8s")
	srv := newK8sTestServer(t)

	prePath := "/api/v1/agents/plain-k8s/move-bundle/precheck"
	rec := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var preResp api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preResp))
	assert.False(t, preResp.Durable)
	assert.Equal(t, "workspace and home live only in the pod and are deleted on stop", preResp.Reason)

	expRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/plain-k8s/move-bundle/export", api.MoveBundleExportRequest{
		ReincarnationID: "rid-plain-k8s",
		ProjectPath:     projectPath,
		Limits:          api.DefaultMoveBundleLimits(),
	})
	assert.Equal(t, http.StatusConflict, expRec.Code)
	assert.Contains(t, expRec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)
}

func TestMoveBundle_K8sNFS_TargetProvisionImportCommitAndRollback(t *testing.T) {
	srv, _, hostBase, projectPath := setupK8sBrokerNFSFixture(t)
	slug := "k8s-dst"
	rid := "rid-k8s-import-1"

	// Build a valid bundle in memory (workspace + home) from a temp source.
	// Include .tmux.conf with source content so we can verify target-rendered
	// files win over same-named source files through import + seed-over (C1).
	srcWS := t.TempDir()
	srcHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcWS, "README.md"), []byte("hello from source\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, "history.txt"), []byte("cmd1\ncmd2\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".tmux.conf"), []byte("# from-source-only\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".scion", "agent-info.json"), []byte(`{"from":"source"}`), 0o644))

	parts := map[string][]byte{}
	wsRoot, err := os.OpenRoot(srcWS)
	require.NoError(t, err)
	defer func() { _ = wsRoot.Close() }()
	wsRM, err := bundle.Write(context.Background(), wsRoot, t.TempDir(), bundle.WorkspaceExcluder(), bundle.DefaultWorkspaceLimits(), &mapPartSink{root: bundle.RootWorkspace, parts: parts})
	require.NoError(t, err)

	homeRoot, err := os.OpenRoot(srcHome)
	require.NoError(t, err)
	defer func() { _ = homeRoot.Close() }()
	homeRM, err := bundle.Write(context.Background(), homeRoot, t.TempDir(), bundle.HomeExcluder(bundle.HomeExcluderOptions{}), bundle.DefaultHomeLimits(), &mapPartSink{root: bundle.RootHome, parts: parts})
	require.NoError(t, err)

	manifest := bundle.Manifest{
		Version:         bundle.ManifestVersion,
		ReincarnationID: rid,
		Roots: map[string]bundle.RootManifest{
			bundle.RootWorkspace: wsRM,
			bundle.RootHome:      homeRM,
		},
	}
	_, manifestHash, err := bundle.CanonicalManifest(manifest)
	require.NoError(t, err)

	objStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := parts[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer objStore.Close()

	srv.moveBundle.fetchDownloadURLs = func(_ context.Context, _, _, _ string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
		var out []api.MoveBundleDownloadPartURL
		for rootName, rm := range manifest.Roots {
			for _, p := range rm.Parts {
				out = append(out, api.MoveBundleDownloadPartURL{
					Root:  rootName,
					Index: p.Index,
					URL:   fmt.Sprintf("%s/%s/%d", objStore.URL, rootName, p.Index),
				})
			}
		}
		return &api.MoveBundleDownloadURLsResponse{Parts: out, ExpiresInSeconds: 900}, nil
	}

	// 1. Target provision with WorkspaceSource=move-bundle on K8s NFS.
	provCtx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
		Experiments: []string{experiments.K8sNFSHome},
	})
	_, err = srv.manager.Provision(provCtx, api.StartOptions{
		Name:                slug,
		ProjectPath:         projectPath,
		HubProjectID:        k8sBrokerTestProjectID,
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: rid,
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		Env: map[string]string{
			"SCION_PROJECT_ID": k8sBrokerTestProjectID,
			"SCION_AGENT_ID":   k8sBrokerTestAgentID,
		},
	})
	require.NoError(t, err)

	// Record post-commit chown calls so we can verify target NFS ownership
	// without needing root privileges in unit tests.
	type commitChown struct {
		uid int
		gid int
	}
	var chownMu sync.Mutex
	chowned := map[string]commitChown{}
	origCommitLchown := moveBundleCommitLchown
	moveBundleCommitLchown = func(name string, uid, gid int) error {
		chownMu.Lock()
		chowned[name] = commitChown{uid: uid, gid: gid}
		chownMu.Unlock()
		return nil
	}
	t.Cleanup(func() { moveBundleCommitLchown = origCommitLchown })

	// 2. Import bundle into K8s NFS target layout.
	impPath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/import?projectId=%s&agentId=%s", slug, k8sBrokerTestProjectID, k8sBrokerTestAgentID)
	impRec := doMoveBundleReq(t, srv, http.MethodPost, impPath, api.MoveBundleImportRequest{
		ReincarnationID: rid,
		AgentID:         k8sBrokerTestAgentID,
		ProjectID:       k8sBrokerTestProjectID,
		ProjectPath:     projectPath,
		Manifest:        manifest,
		ManifestHash:    manifestHash,
		Limits:          api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusAccepted, impRec.Code, impRec.Body.String())

	st := waitForMoveBundleJob(t, srv, slug, k8sBrokerTestProjectID, api.MoveBundleJobKindImport, rid)
	require.Equal(t, api.MoveBundleJobStateSucceeded, st.State, "error=%s", st.Error)

	realHostBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	exportAgentDir := filepath.Join(realHostBase, "projects", k8sBrokerTestProjectID, "agents", slug)
	exportWS := filepath.Join(exportAgentDir, "workspace")
	exportHome := filepath.Join(exportAgentDir, "home-"+k8sBrokerTestAgentID)

	wsData, err := os.ReadFile(filepath.Join(exportWS, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "hello from source\n", string(wsData))

	histData, err := os.ReadFile(filepath.Join(exportHome, "history.txt"))
	require.NoError(t, err)
	assert.Equal(t, "cmd1\ncmd2\n", string(histData))

	// Post-commit chown must have run on imported workspace and home entries.
	// Fixture configures nfs.uid=2000 and nfs.gid=3000; on Kubernetes pods
	// always run as K8sContainerUID (1000) with fsGroup DefaultOwnerID(nfs.gid)
	// = 3000 (R1), while home uses K8sContainerUID:homeStorageGID (1000:1000).
	wantWSOwner := commitChown{uid: int(scionrt.K8sContainerUID), gid: 3000}
	wantHomeOwner := commitChown{uid: int(scionrt.K8sContainerUID), gid: 1000}
	chownMu.Lock()
	assert.Equal(t, wantWSOwner, chowned[exportWS], "committed workspace dir must be chowned to K8sContainerUID:DefaultOwnerID(nfs.gid)")
	assert.Equal(t, wantWSOwner, chowned[filepath.Join(exportWS, "README.md")], "committed workspace file must be chowned to K8sContainerUID:DefaultOwnerID(nfs.gid)")
	assert.Equal(t, wantHomeOwner, chowned[exportHome], "committed home dir must be chowned to K8sContainerUID:homeStorageGID")
	assert.Equal(t, wantHomeOwner, chowned[filepath.Join(exportHome, "history.txt")], "committed home file must be chowned to K8sContainerUID:homeStorageGID")
	chownMu.Unlock()

	// Sentinel written by provision must survive import, and homeprep.Decide
	// must choose ModeSeedOver so target Start transfers broker-rendered files
	// over source files while keeping imported source files like history.txt (C1).
	sentinelPath := filepath.Join(exportHome, homeprep.SentinelName)
	assert.FileExists(t, sentinelPath)
	sentinelBytes, err := os.ReadFile(sentinelPath)
	require.NoError(t, err)
	var sent homeprep.Sentinel
	require.NoError(t, json.Unmarshal(sentinelBytes, &sent))
	homeEntries, err := os.ReadDir(exportHome)
	require.NoError(t, err)
	names := make([]string, 0, len(homeEntries))
	for _, e := range homeEntries {
		names = append(names, e.Name())
	}
	decision, err := homeprep.Decide(homeprep.SentinelRead{Sentinel: &sent}, k8sBrokerTestAgentID, int(scionrt.K8sContainerUID), names, "")
	require.NoError(t, err)
	assert.Equal(t, homeprep.ModeSeedOver, decision.Mode, "imported K8s NFS home must trigger homeprep seed-over on target Start")

	// Exercise seed-over precedence unconditionally: verify Provision rendered
	// .tmux.conf in broker-local targetHome with target content distinct from
	// source, apply seed-over transfer from targetHome onto exportHome, and
	// assert target .tmux.conf wins while imported history.txt survives.
	targetDirs, err := agent.ResolveMoveBundleAgentDirs(context.Background(), "kubernetes", api.StartOptions{
		Name:         slug,
		ProjectPath:  projectPath,
		HubProjectID: k8sBrokerTestProjectID,
		BrokerMode:   true,
	})
	require.NoError(t, err)
	targetTmuxBytes, err := os.ReadFile(filepath.Join(targetDirs.HomeDir, ".tmux.conf"))
	require.NoError(t, err, "target provision must render .tmux.conf in broker-local agent home")
	require.NotEqual(t, "# from-source-only\n", string(targetTmuxBytes))

	// Before seed-over, exportHome/.tmux.conf has the imported source content.
	importedTmux, err := os.ReadFile(filepath.Join(exportHome, ".tmux.conf"))
	require.NoError(t, err)
	assert.Equal(t, "# from-source-only\n", string(importedTmux))

	// Simulate Start's ModeSeedOver transfer of broker-rendered targetHome over exportHome.
	require.NoError(t, filepath.WalkDir(targetDirs.HomeDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || p == targetDirs.HomeDir {
			return walkErr
		}
		rel, relErr := filepath.Rel(targetDirs.HomeDir, p)
		if relErr != nil {
			return relErr
		}
		dst := filepath.Join(exportHome, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if d.Type().IsRegular() {
			data, rErr := os.ReadFile(p)
			if rErr != nil {
				return rErr
			}
			if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil {
				return mkErr
			}
			return os.WriteFile(dst, data, 0o644)
		}
		return nil
	}))
	seededTmux, err := os.ReadFile(filepath.Join(exportHome, ".tmux.conf"))
	require.NoError(t, err)
	assert.Equal(t, string(targetTmuxBytes), string(seededTmux), "target-rendered .tmux.conf must win over source after seed-over")
	histAfterSeed, err := os.ReadFile(filepath.Join(exportHome, "history.txt"))
	require.NoError(t, err)
	assert.Equal(t, "cmd1\ncmd2\n", string(histAfterSeed), "source-only history.txt must survive seed-over")

	// 3. Rollback delete (marker-guarded) removes NFS workspace, NFS home leaf,
	//    .bundle-* staging siblings, and the now-empty export agent dir (N3).
	leftoverStage := filepath.Join(exportAgentDir, ".home-"+k8sBrokerTestAgentID+".bundle-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.MkdirAll(leftoverStage, 0o755))

	delPath := fmt.Sprintf("/api/v1/agents/%s?deleteFiles=true&projectId=%s&agentId=%s&moveReincarnationId=%s&projectPath=%s",
		slug, k8sBrokerTestProjectID, k8sBrokerTestAgentID, rid, projectPath)
	delRec := doMoveBundleReq(t, srv, http.MethodDelete, delPath, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	assert.NoDirExists(t, exportWS, "rollback delete must remove NFS workspace")
	assert.NoDirExists(t, exportHome, "rollback delete must remove NFS home leaf")
	assert.NoDirExists(t, leftoverStage, "rollback delete must remove .bundle-* staging siblings")
	assert.NoDirExists(t, exportAgentDir, "rollback delete must remove empty marked export agent dir (N3)")
}

func TestMoveBundle_DockerDefaultWithCloudRunAgentFailsClosed(t *testing.T) {
	projectPath, _, _, _ := newMoveBundleTestAgentDir(t, "cr-agent")
	srv := newTestServer(t)
	srv.runtime = scionrt.NewDockerRuntime()
	// Broker advertises AgentMoveBundle from its Docker default runtime.
	require.True(t, StaticCapabilities(srv.runtime, "").AgentMoveBundle)

	// Register cloudrun as an auxiliary runtime (matching a multi-runtime broker).
	crRT := &scionrt.MockRuntime{NameFunc: func() string { return "cloudrun" }}
	crMgr := agent.NewManager(crRT)

	// 1. Move-bundle provision for a cloudrun agent must fail closed.
	_, err := crMgr.Provision(context.Background(), api.StartOptions{
		Name:                "cr-agent",
		ProjectPath:         projectPath,
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: "rid-cr-fail",
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
	})
	require.Error(t, err, "move-bundle provision on cloudrun must fail closed")
	assert.Contains(t, err.Error(), "not supported")

	// 2. Move-bundle import resolving to cloudrun must return an error (400),
	//    never 202 followed by silently writing broker-local dirs.
	dummyPart := bundle.Part{
		Index: 0,
		Size:  1,
		Hash:  "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	manifest := bundle.Manifest{
		Version:         bundle.ManifestVersion,
		ReincarnationID: "rid-cr-fail",
		Roots: map[string]bundle.RootManifest{
			bundle.RootWorkspace: {PartSize: 1, Parts: []bundle.Part{dummyPart}},
			bundle.RootHome:      {PartSize: 1, Parts: []bundle.Part{dummyPart}},
		},
	}
	_, manifestHash, err := bundle.CanonicalManifest(manifest)
	require.NoError(t, err)

	impPath := fmt.Sprintf("/api/v1/agents/cr-agent/move-bundle/import?%s=cloudrun", api.RecordedRuntimeQueryParam)
	rec := doMoveBundleReq(t, srv, http.MethodPost, impPath, api.MoveBundleImportRequest{
		ReincarnationID: "rid-cr-fail",
		ProjectPath:     projectPath,
		Manifest:        manifest,
		ManifestHash:    manifestHash,
		Limits:          api.DefaultMoveBundleLimits(),
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "import for unregistered runtime must return 400, not 202: %s", rec.Body.String())
}

func TestMoveBundle_K8sNFS_PrecheckReadabilityAndBounds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0000 read permissions")
	}
	srv, _, hostBase, projectPath := setupK8sBrokerNFSFixture(t)
	slug := "k8s-bounds"

	dirs, err := agent.ResolveMoveBundleAgentDirs(context.Background(), "kubernetes", api.StartOptions{
		Name:         slug,
		ProjectPath:  projectPath,
		HubProjectID: k8sBrokerTestProjectID,
		BrokerMode:   true,
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dirs.AgentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dirs.AgentDir, "home-storage.json"), []byte(`{
  "backend": "nfs",
  "leaf": "pod",
  "share_id": "share-1",
  "pv_claim_name": "pv-1",
  "subpath_root": "projects"
}
`), 0o600))
	writeTestHomeRecord(t, dirs.AgentDir, scionrt.RunConfig{UnixUsername: "scion"})

	realHostBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	exportAgentDir := filepath.Join(realHostBase, "projects", k8sBrokerTestProjectID, "agents", slug)
	wsDir := filepath.Join(exportAgentDir, "workspace")
	homeDir := filepath.Join(exportAgentDir, "home-"+k8sBrokerTestAgentID)
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "main.go"), []byte("package main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(homeDir, ".cache", "pip"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, "ok.txt"), []byte("hello"), 0o644))

	// 1. Unreadable 0000 file inside excluded .cache/ must NOT refuse the move.
	cachedFile := filepath.Join(homeDir, ".cache", "pip", "wheel.bin")
	require.NoError(t, os.WriteFile(cachedFile, []byte("cached"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(cachedFile, 0o600) })

	prePath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/precheck?projectId=%s", slug, k8sBrokerTestProjectID)
	recCache := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		AgentID:     k8sBrokerTestAgentID,
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, recCache.Code, recCache.Body.String())
	var respCache api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(recCache.Body.Bytes(), &respCache))
	assert.True(t, respCache.Durable, "unreadable file under excluded .cache/ must not refuse precheck")
	assert.Empty(t, respCache.Over)

	// 2. Non-excluded unreadable 0000 file refuses precheck with class-only reason.
	privFile := filepath.Join(homeDir, "zz-private.txt")
	require.NoError(t, os.WriteFile(privFile, []byte("secret"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(privFile, 0o600) })

	recPriv := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		AgentID:     k8sBrokerTestAgentID,
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, recPriv.Code)
	var respPriv api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(recPriv.Body.Bytes(), &respPriv))
	assert.False(t, respPriv.Durable, "unreadable non-excluded file must refuse precheck")
	assert.Equal(t, "broker lacks read permission on source home", respPriv.Reason)
	assert.NotContains(t, respPriv.Reason, homeDir, "reason must be class-only")

	// 3. Home over home_max_entries stops early and returns over="home.entries"
	//    without visiting entries past the limit (including zz-private.txt).
	for i := 0; i < 6; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(homeDir, fmt.Sprintf("a-%02d.txt", i)), []byte("x"), 0o644))
	}
	lim := api.DefaultMoveBundleLimits()
	lim.HomeMaxEntries = 3
	recOver := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		AgentID:     k8sBrokerTestAgentID,
		ProjectPath: projectPath,
		Limits:      lim,
	})
	require.Equal(t, http.StatusOK, recOver.Code)
	var respOver api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(recOver.Body.Bytes(), &respOver))
	assert.True(t, respOver.Durable, "over-limit home should report over=home.entries rather than visiting unreadable entries past the bound")
	assert.Equal(t, "home.entries", respOver.Over)
}

func TestMoveBundle_K8sNFS_NormalDeletePreservesHomeLeaf(t *testing.T) {
	srv, _, hostBase, projectPath := setupK8sBrokerNFSFixture(t)
	slug := "k8s-normal-del"
	rid := "rid-k8s-normal-del-1"

	provCtx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
		Experiments: []string{experiments.K8sNFSHome},
	})
	_, err := srv.manager.Provision(provCtx, api.StartOptions{
		Name:                slug,
		ProjectPath:         projectPath,
		HubProjectID:        k8sBrokerTestProjectID,
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: rid,
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		Env: map[string]string{
			"SCION_PROJECT_ID": k8sBrokerTestProjectID,
			"SCION_AGENT_ID":   k8sBrokerTestAgentID,
		},
	})
	require.NoError(t, err)

	realHostBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	exportAgentDir := filepath.Join(realHostBase, "projects", k8sBrokerTestProjectID, "agents", slug)
	exportHome := filepath.Join(exportAgentDir, "home-"+k8sBrokerTestAgentID)
	require.DirExists(t, exportHome)

	// Normal delete (no moveReincarnationId) must NOT remove NFS home leaf (Hub owns RemoveAgentStorage).
	delPath := fmt.Sprintf("/api/v1/agents/%s?deleteFiles=true&projectId=%s&agentId=%s&projectPath=%s",
		slug, k8sBrokerTestProjectID, k8sBrokerTestAgentID, projectPath)
	delRec := doMoveBundleReq(t, srv, http.MethodDelete, delPath, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	assert.DirExists(t, exportHome, "normal delete without moveReincarnationId must leave NFS home leaf intact")
}

type mapPartSink struct {
	root  string
	parts map[string][]byte
}

func (s *mapPartSink) Put(_ context.Context, index int, part io.ReadSeeker, _ int64, _ string) error {
	data, err := io.ReadAll(part)
	if err != nil {
		return err
	}
	s.parts[fmt.Sprintf("/%s/%d", s.root, index)] = data
	return nil
}

func readAllBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}
