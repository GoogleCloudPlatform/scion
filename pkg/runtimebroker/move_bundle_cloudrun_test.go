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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	crBrokerTestProjectUUID = "22222222-3333-4444-5555-666666666666"
	crBrokerTestAgentUUID   = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
)

func setupCloudRunBrokerNFSFixture(t *testing.T) (srv *Server, hostBase, projectPath string) {
	t.Helper()
	// Create test server first because newTestServer overrides HOME.
	srv = newTestServer(t)
	srv.moveBundle.durabilityRuntime = "cloudrun"

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))

	mountRoot := filepath.Join(tmp, "nfs-mounts")
	hostBase = filepath.Join(mountRoot, "cr-share")
	require.NoError(t, os.MkdirAll(hostBase, 0o755))

	globalDir := filepath.Join(tmp, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsYAML := `schema_version: "1"
active_profile: cr
runtimes:
  cloudrun:
    type: cloudrun
profiles:
  cr:
    runtime: cloudrun
server:
  project_storage:
    backend: nfs
    nfs:
      mount_root: ` + mountRoot + `
      subpath_root: projects
      uid: 2000
      gid: 3000
      shares:
        - id: cr-share
          server: 10.1.0.10
          export: /filestore
`
	require.NoError(t, config.InitGlobal(nil, config.InitMachineOpts{SkipRuntimeCheck: true}))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))

	projectPath = filepath.Join(globalDir, "projects", "demo", ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	return srv, hostBase, projectPath
}

func TestMoveBundle_CloudRunNFS_CapabilitiesGating(t *testing.T) {
	// Without NFS project storage, Cloud Run does not advertise AgentMoveBundle.
	tmpPlain := t.TempDir()
	t.Setenv("HOME", tmpPlain)
	t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmpPlain, ".scion"))
	assert.False(t, StaticCapabilities(&scionrt.CloudRunRuntime{}, "").AgentMoveBundle)
	assert.NotContains(t, StaticCapabilityNames(&scionrt.CloudRunRuntime{}), "agentMoveBundle")

	srvPlain := newTestServer(t)
	srvPlain.runtime = &scionrt.CloudRunRuntime{}
	recPlain := doMoveBundleReq(t, srvPlain, http.MethodGet, "/api/v1/info", nil)
	require.Equal(t, http.StatusOK, recPlain.Code)
	var infoPlain BrokerInfoResponse
	require.NoError(t, json.Unmarshal(recPlain.Body.Bytes(), &infoPlain))
	assert.False(t, infoPlain.Capabilities.AgentMoveBundle)

	// With NFS project storage configured (no K8s homes/pv_name needed), Cloud Run advertises AgentMoveBundle.
	srvCR, _, _ := setupCloudRunBrokerNFSFixture(t)
	srvCR.runtime = &scionrt.CloudRunRuntime{}
	assert.True(t, StaticCapabilities(srvCR.runtime, "").AgentMoveBundle)
	assert.Contains(t, StaticCapabilityNames(srvCR.runtime), "agentMoveBundle")

	recCR := doMoveBundleReq(t, srvCR, http.MethodGet, "/api/v1/info", nil)
	require.Equal(t, http.StatusOK, recCR.Code)
	var infoCR BrokerInfoResponse
	require.NoError(t, json.Unmarshal(recCR.Body.Bytes(), &infoCR))
	assert.True(t, infoCR.Capabilities.AgentMoveBundle)

	// Cloud Run Sandbox never advertises AgentMoveBundle even when NFS is configured.
	assert.False(t, StaticCapabilities(&scionrt.CloudRunSandboxRuntime{}, "").AgentMoveBundle)
	assert.NotContains(t, StaticCapabilityNames(&scionrt.CloudRunSandboxRuntime{}), "agentMoveBundle")
}

func TestMoveBundle_CloudRunNFS_SourcePrecheckExportAndTargetImportCommit(t *testing.T) {
	srv, hostBase, projectPath := setupCloudRunBrokerNFSFixture(t)

	// 1. Set up a Cloud Run NFS source agent.
	srcSlug := "cr-src"
	srcIn := agent.MoveBundleRuntimeInput{
		RuntimeName: "cloudrun",
		AgentSlug:   srcSlug,
		AgentID:     crBrokerTestAgentUUID,
		ProjectPath: projectPath,
		ProjectID:   crBrokerTestProjectUUID,
		BrokerMode:  true,
	}
	srcDirs, err := agent.ResolveMoveBundleDirsForRuntime(context.Background(), srcIn)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(srcDirs.AgentDir, 0o755))
	require.NoError(t, os.MkdirAll(srcDirs.WorkspaceDir, 0o755))
	require.NoError(t, os.MkdirAll(srcDirs.HomeDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDirs.AgentDir, "cloudrun-agent-workspace.json"), []byte(`{"layout":"agent-workspace"}`), 0o600))
	writeTestHomeRecord(t, srcDirs.AgentDir, scionrt.RunConfig{
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "tok", Type: "file", Target: "~/.config/secret.key", Value: "shh"},
		},
	})

	require.NoError(t, os.WriteFile(filepath.Join(srcDirs.WorkspaceDir, "README.md"), []byte("hello cloudrun\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDirs.HomeDir, "history.txt"), []byte("cmd1\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(srcDirs.HomeDir, ".config"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDirs.HomeDir, ".config", "secret.key"), []byte("must-not-export"), 0o600))

	// Precheck via HTTP (carrying AgentID in JSON body, not query string).
	prePath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/precheck?projectId=%s", srcSlug, crBrokerTestProjectUUID)
	rec := doMoveBundleReq(t, srv, http.MethodPost, prePath, api.MoveBundlePrecheckRequest{
		AgentID:     crBrokerTestAgentUUID,
		ProjectPath: projectPath,
		Limits:      api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var preResp api.MoveBundlePrecheckResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preResp))
	assert.True(t, preResp.Durable, "Cloud Run NFS source must be durable (reason=%q)", preResp.Reason)
	assert.Equal(t, int64(len("hello cloudrun\n")), preResp.Workspace.Bytes)
	assert.Equal(t, int64(len("cmd1\n")), preResp.Home.Bytes)

	// In-memory object store for export PUTs and import GETs.
	var mu sync.Mutex
	parts := map[string][]byte{}
	objStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		switch r.Method {
		case http.MethodPut:
			buf, _ := readAllBody(r)
			mu.Lock()
			parts[key] = buf
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			mu.Lock()
			buf, ok := parts[key]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buf)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer objStore.Close()

	srv.moveBundle.fetchUploadURL = func(_ context.Context, _, _, _ string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
		return &api.MoveBundleUploadURLResponse{
			URL:    fmt.Sprintf("%s/%s/%d", objStore.URL, req.Root, req.PartIndex),
			Method: http.MethodPut,
		}, nil
	}

	rid := "rid-cr-e2e-1"
	expPath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/export?projectId=%s&agentId=%s", srcSlug, crBrokerTestProjectUUID, crBrokerTestAgentUUID)
	expRec := doMoveBundleReq(t, srv, http.MethodPost, expPath, api.MoveBundleExportRequest{
		ReincarnationID: rid,
		AgentID:         crBrokerTestAgentUUID,
		ProjectID:       crBrokerTestProjectUUID,
		ProjectPath:     projectPath,
		Limits:          api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusAccepted, expRec.Code, "body=%s", expRec.Body.String())
	expStatus := waitForMoveBundleJob(t, srv, srcSlug, crBrokerTestProjectUUID, api.MoveBundleJobKindExport, rid)
	require.Equal(t, api.MoveBundleJobStateSucceeded, expStatus.State, "err=%s", expStatus.Error)
	require.NotNil(t, expStatus.Manifest)

	// 2. Provision a fresh Cloud Run NFS target agent with WorkspaceSource=move-bundle.
	dstSlug := "cr-dst"
	crRT := &scionrt.MockRuntime{NameFunc: func() string { return "cloudrun" }}
	dstMgr := agent.NewManager(crRT).(*agent.AgentManager)
	_, err = dstMgr.Provision(context.Background(), api.StartOptions{
		Name:                dstSlug,
		ProjectPath:         projectPath,
		HubProjectID:        crBrokerTestProjectUUID,
		Profile:             "cr",
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: rid,
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		Env: map[string]string{
			"SCION_PROJECT_ID":     crBrokerTestProjectUUID,
			"SCION_AGENT_ID":       crBrokerTestAgentUUID,
			"SCION_WORKSPACE_MODE": "clone-per-agent",
		},
	})
	require.NoError(t, err)

	// Capture commit lchown calls to verify Cloud Run 2000:3000 ownership.
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

	srv.moveBundle.fetchDownloadURLs = func(_ context.Context, _, _, _ string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
		var urls []api.MoveBundleDownloadPartURL
		for rootName, rm := range expStatus.Manifest.Roots {
			for _, p := range rm.Parts {
				urls = append(urls, api.MoveBundleDownloadPartURL{
					Root:    rootName,
					Index:   p.Index,
					URL:     fmt.Sprintf("%s/%s/%d", objStore.URL, rootName, p.Index),
					Expires: time.Now().Add(15 * time.Minute),
				})
			}
		}
		return &api.MoveBundleDownloadURLsResponse{Parts: urls}, nil
	}

	_, manifestHash, err := bundle.CanonicalManifest(*expStatus.Manifest)
	require.NoError(t, err)

	impPath := fmt.Sprintf("/api/v1/agents/%s/move-bundle/import?projectId=%s&agentId=%s", dstSlug, crBrokerTestProjectUUID, crBrokerTestAgentUUID)
	impRec := doMoveBundleReq(t, srv, http.MethodPost, impPath, api.MoveBundleImportRequest{
		ReincarnationID: rid,
		AgentID:         crBrokerTestAgentUUID,
		ProjectID:       crBrokerTestProjectUUID,
		ProjectPath:     projectPath,
		Manifest:        *expStatus.Manifest,
		ManifestHash:    manifestHash,
		Limits:          api.DefaultMoveBundleLimits(),
	})
	require.Equal(t, http.StatusAccepted, impRec.Code, "body=%s", impRec.Body.String())
	impStatus := waitForMoveBundleJob(t, srv, dstSlug, crBrokerTestProjectUUID, api.MoveBundleJobKindImport, rid)
	require.Equal(t, api.MoveBundleJobStateSucceeded, impStatus.State, "err=%s", impStatus.Error)

	resolvedBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	dstExportAgentDir := filepath.Join(resolvedBase, "projects", crBrokerTestProjectUUID, "agents", dstSlug)
	dstWS := filepath.Join(dstExportAgentDir, "workspace")
	dstHome := filepath.Join(dstExportAgentDir, "home-"+crBrokerTestAgentUUID)

	// Workspace and home imported; excluded secret did NOT cross.
	gotWS, err := os.ReadFile(filepath.Join(dstWS, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "hello cloudrun\n", string(gotWS))

	gotHist, err := os.ReadFile(filepath.Join(dstHome, "history.txt"))
	require.NoError(t, err)
	assert.Equal(t, "cmd1\n", string(gotHist))
	assert.NoFileExists(t, filepath.Join(dstHome, ".config", "secret.key"))

	// Ownership applied to BOTH workspace and home with Cloud Run's configured 2000:3000.
	wantOwner := commitChown{uid: 2000, gid: 3000}
	chownMu.Lock()
	assert.Equal(t, wantOwner, chowned[dstWS], "committed workspace dir must be chowned to configured Cloud Run uid:gid")
	assert.Equal(t, wantOwner, chowned[filepath.Join(dstWS, "README.md")], "committed workspace file must be chowned to configured Cloud Run uid:gid")
	assert.Equal(t, wantOwner, chowned[dstHome], "committed home dir must be chowned to configured Cloud Run uid:gid")
	assert.Equal(t, wantOwner, chowned[filepath.Join(dstHome, "history.txt")], "committed home file must be chowned to configured Cloud Run uid:gid")
	chownMu.Unlock()

	// Rollback cleans up workspace, home leaf, and empty exportAgentDir.
	rbCtx := agent.WithDeleteMoveReincarnationID(agent.WithDeleteHubAgentID(context.Background(), crBrokerTestAgentUUID), rid)
	_, err = dstMgr.RemoveNFSAgentFiles(rbCtx, projectPath, crBrokerTestProjectUUID, dstSlug)
	require.NoError(t, err)
	assert.NoDirExists(t, dstWS)
	assert.NoDirExists(t, dstHome)
	assert.NoDirExists(t, dstExportAgentDir)
}
