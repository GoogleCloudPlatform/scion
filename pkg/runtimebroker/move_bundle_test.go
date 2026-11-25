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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// newMoveBundleTestAgentDir sets up a local project + agent dir with workspace
// and home subdirectories and returns (projectPath, agentDir, workspaceDir, homeDir).
func newMoveBundleTestAgentDir(t *testing.T, agentSlug string) (string, string, string, string) {
	t.Helper()
	projectRoot := t.TempDir()
	scionDir := filepath.Join(projectRoot, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))

	agentDir := config.GetAgentDir(scionDir, agentSlug, false)
	wsDir := filepath.Join(agentDir, "workspace")
	homeDir := config.GetAgentHomePath(scionDir, agentSlug)
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	return scionDir, agentDir, wsDir, homeDir
}

func writeTestHomeRecord(t *testing.T, agentDir string, cfg scionrt.RunConfig) {
	t.Helper()
	if cfg.UnixUsername == "" {
		cfg.UnixUsername = "scion"
	}
	require.NoError(t, agent.WriteMoveBundleHomeRecord(agentDir, cfg))
}

func writeTestMoveMarker(t *testing.T, agentDir, rid string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, agent.MoveMarkerFile), []byte(rid+"\n"), 0o600))
}

func doMoveBundleReq(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func waitForMoveBundleJob(t *testing.T, srv *Server, agentSlug, projectID, kind, rid string) api.MoveBundleJobStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/%s/%s", agentSlug, kind, rid)
	if projectID != "" {
		path += "?projectId=" + projectID
	}
	for time.Now().Before(deadline) {
		rec := doMoveBundleReq(t, srv, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var st api.MoveBundleJobStatus
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		if st.State != api.MoveBundleJobStateRunning {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s job %s", kind, rid)
	return api.MoveBundleJobStatus{}
}

func TestBrokerCapabilities_AdvertiseAgentMoveBundle(t *testing.T) {
	// Supported runtimes advertise AgentMoveBundle; unsupported/nil/degraded do not.
	for _, tc := range []struct {
		name string
		rt   scionrt.Runtime
		want bool
	}{
		{"docker", scionrt.NewDockerRuntime(), true},
		{"podman", &scionrt.PodmanRuntime{}, true},
		{"apple", scionrt.NewAppleContainerRuntime(), true},
		{"kubernetes", &scionrt.KubernetesRuntime{}, true},
		{"cloudrun", &scionrt.CloudRunRuntime{}, false},
		{"cloudrun-sandbox", &scionrt.CloudRunSandboxRuntime{}, false},
		{"nil", nil, false},
		{"error-runtime", &scionrt.ErrorRuntime{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StaticCapabilities(tc.rt, "").AgentMoveBundle)
			if tc.want {
				assert.Contains(t, StaticCapabilityNames(tc.rt), "agentMoveBundle")
			} else {
				assert.NotContains(t, StaticCapabilityNames(tc.rt), "agentMoveBundle")
			}
		})
	}

	// /api/v1/info advertises AgentMoveBundle when default or auxiliary runtime is supported.
	srv := newTestServer(t)
	srv.runtime = &scionrt.KubernetesRuntime{}
	rec := doMoveBundleReq(t, srv, http.MethodGet, "/api/v1/info", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp BrokerInfoResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Capabilities)
	assert.True(t, resp.Capabilities.AgentMoveBundle, "/api/v1/info must advertise AgentMoveBundle for Kubernetes broker")

	srvUnsupported := newTestServer(t)
	srvUnsupported.runtime = &scionrt.CloudRunRuntime{}
	rec2 := doMoveBundleReq(t, srvUnsupported, http.MethodGet, "/api/v1/info", nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	var resp2 BrokerInfoResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.NotNil(t, resp2.Capabilities)
	assert.False(t, resp2.Capabilities.AgentMoveBundle, "/api/v1/info must not advertise AgentMoveBundle when only unsupported runtimes are configured")
}

func TestMoveBundlePrecheck_DurabilityAndEstimate(t *testing.T) {
	projectPath, agentDir, wsDir, homeDir := newMoveBundleTestAgentDir(t, "dev")
	writeTestHomeRecord(t, agentDir, scionrt.RunConfig{
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "ws_env", Type: "file", Target: "/workspace/.env"},
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "hello.txt"), []byte("hello world"), 0o644))
	// Excluded workspace file secret should not be counted in precheck estimate (R1).
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, ".env"), bytes.Repeat([]byte("e"), 4096), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, "notes.txt"), []byte("my notes"), 0o600))
	// Excluded secret in home should not be counted.
	require.NoError(t, os.MkdirAll(filepath.Join(homeDir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".scion", "secrets.json"), bytes.Repeat([]byte("s"), 4096), 0o600))

	t.Run("incomplete limits rejected with 400 (S1)", func(t *testing.T) {
		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: projectPath,
			Limits:      api.MoveBundleLimits{WorkspaceMaxBytes: 1 << 20}, // missing other fields
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("docker durable and within limits", func(t *testing.T) {
		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		lim := api.DefaultMoveBundleLimits()
		lim.WorkspaceMaxBytes = 1 << 20
		lim.HomeMaxBytes = 1 << 20
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: projectPath,
			Limits:      lim,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.True(t, resp.Durable)
		assert.Empty(t, resp.Reason)
		assert.Empty(t, resp.Over)
		assert.Equal(t, int64(11), resp.Workspace.Bytes)
		assert.Equal(t, int64(8), resp.Home.Bytes)
	})

	t.Run("podman and apple container durable", func(t *testing.T) {
		for _, rt := range []string{"podman", "container", "apple"} {
			srv := newTestServer(t)
			srv.moveBundle.durabilityRuntime = rt
			rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/precheck", api.MoveBundlePrecheckRequest{
				ProjectPath: projectPath,
				Limits:      api.DefaultMoveBundleLimits(),
			})
			require.Equal(t, http.StatusOK, rec.Code)
			var resp api.MoveBundlePrecheckResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.True(t, resp.Durable, "runtime %s should be durable", rt)
		}
	})

	t.Run("plain kubernetes and cloudrun refused", func(t *testing.T) {
		srvK8s := newTestServer(t)
		srvK8s.moveBundle.durabilityRuntime = "kubernetes"
		recK8s := doMoveBundleReq(t, srvK8s, http.MethodPost, "/api/v1/agents/dev/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: projectPath,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, recK8s.Code)
		var respK8s api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(recK8s.Body.Bytes(), &respK8s))
		assert.False(t, respK8s.Durable, "plain kubernetes without NFS must not be durable")
		assert.Equal(t, "workspace and home live only in the pod and are deleted on stop", respK8s.Reason)

		for _, rt := range []string{"cloudrun", "cloudrun-sandbox"} {
			srv := newTestServer(t)
			srv.moveBundle.durabilityRuntime = rt
			rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/precheck", api.MoveBundlePrecheckRequest{
				ProjectPath: projectPath,
				Limits:      api.DefaultMoveBundleLimits(),
			})
			require.Equal(t, http.StatusOK, rec.Code)
			var resp api.MoveBundlePrecheckResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.Durable, "runtime %s should not be durable yet", rt)
			assert.Equal(t, "not yet supported", resp.Reason)
		}
	})

	t.Run("over-limit early stop", func(t *testing.T) {
		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		lim := api.DefaultMoveBundleLimits()
		lim.WorkspaceMaxBytes = 5
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: projectPath,
			Limits:      lim,
		})
		require.Equal(t, http.StatusOK, rec.Code)
		var resp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.True(t, resp.Durable)
		assert.Equal(t, "bytes", resp.Workspace.Over)
		assert.Equal(t, "workspace.bytes", resp.Over)
	})

	t.Run("invalid path returns durable=false with class-only reason and no host path (N2)", func(t *testing.T) {
		badProj, badAgentDir, badWS, _ := newMoveBundleTestAgentDir(t, "dev-badpath")
		writeTestHomeRecord(t, badAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(badWS, `bad\name.txt`), []byte("x"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-badpath/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: badProj,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.False(t, resp.Durable)
		assert.Equal(t, "source workspace contains an unsupported path", resp.Reason)
		assert.NotContains(t, resp.Reason, badWS)
		assert.NotContains(t, resp.Reason, `bad\name.txt`)
		assert.Empty(t, resp.Over)
		assert.Empty(t, resp.Workspace.Over)
	})

	t.Run("missing agent dir returns 404 on precheck instead of restart hint (N3)", func(t *testing.T) {
		emptyProj := filepath.Join(t.TempDir(), ".scion")
		require.NoError(t, os.MkdirAll(emptyProj, 0o755))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/ghost-agent/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: emptyProj,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), moveBundleHomeRecordMissingReason)
	})

	t.Run("shared layout workspace refused on precheck and export (S2)", func(t *testing.T) {
		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"

		home := os.Getenv("HOME")
		mountRoot := filepath.Join(t.TempDir(), "nfs")
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		settingsYAML := fmt.Sprintf(`schema_version: "1"
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: ws-pv
`, mountRoot)
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))

		const sharedProjID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		sharedRoot := filepath.Join(mountRoot, "share-1", "projects", sharedProjID)
		sharedScion := filepath.Join(sharedRoot, ".scion")
		require.NoError(t, os.MkdirAll(sharedScion, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sharedScion, "project-id"), []byte(sharedProjID+"\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(sharedScion, "project-slug"), []byte("web\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(sharedScion, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o644))

		preRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-shared/move-bundle/precheck?projectId="+sharedProjID, api.MoveBundlePrecheckRequest{
			ProjectID:   sharedProjID,
			ProjectPath: sharedRoot,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, preRec.Code, preRec.Body.String())
		var preResp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(preRec.Body.Bytes(), &preResp))
		assert.False(t, preResp.Durable)
		assert.Equal(t, "shared layout workspace is not yet supported for bundle moves", preResp.Reason)

		expRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-shared/move-bundle/export?projectId="+sharedProjID, api.MoveBundleExportRequest{
			ReincarnationID: "rid-shared",
			ProjectID:       sharedProjID,
			ProjectPath:     sharedRoot,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, expRec.Code, expRec.Body.String())
		assert.Contains(t, expRec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)

		// O2: import also refuses shared-layout targets with 409 move_bundle_unsupported.
		dummyPart := bundle.Part{Index: 0, Size: 1, Hash: "sha256:" + strings.Repeat("a", 64)}
		emptyManifest := bundle.Manifest{
			Version:         bundle.ManifestVersion,
			ReincarnationID: "rid-shared",
			Roots: map[string]bundle.RootManifest{
				bundle.RootWorkspace: {PartSize: 64 << 20, Parts: []bundle.Part{dummyPart}},
				bundle.RootHome:      {PartSize: 64 << 20, Parts: []bundle.Part{dummyPart}},
			},
		}
		_, emptyHash, err := bundle.CanonicalManifest(emptyManifest)
		require.NoError(t, err)
		impRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-shared/move-bundle/import?projectId="+sharedProjID, api.MoveBundleImportRequest{
			ReincarnationID: "rid-shared",
			ProjectID:       sharedProjID,
			ProjectPath:     sharedRoot,
			Manifest:        emptyManifest,
			ManifestHash:    emptyHash,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, impRec.Code, impRec.Body.String())
		assert.Contains(t, impRec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)
	})

	t.Run("worktree workspace refused on precheck (O1)", func(t *testing.T) {
		wtProj, wtAgentDir, wtWS, _ := newMoveBundleTestAgentDir(t, "dev-wt-pre")
		writeTestHomeRecord(t, wtAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(wtWS, ".git"), []byte("gitdir: /host/repo/.git/worktrees/dev-wt-pre\n"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-wt-pre/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: wtProj,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.False(t, resp.Durable)
		assert.Equal(t, "worktree workspace is not supported for bundle moves", resp.Reason)
	})

	t.Run("missing home record fails closed on precheck and export (C1c, C1f)", func(t *testing.T) {
		noRecProj, _, noRecWS, _ := newMoveBundleTestAgentDir(t, "dev-norecord")
		require.NoError(t, os.WriteFile(filepath.Join(noRecWS, "a.txt"), []byte("a"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		preRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-norecord/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: noRecProj,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, preRec.Code, preRec.Body.String())
		var preResp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(preRec.Body.Bytes(), &preResp))
		assert.False(t, preResp.Durable)
		assert.Equal(t, moveBundleHomeRecordMissingReason, preResp.Reason)

		expRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-norecord/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-norecord",
			ProjectID:       "p1",
			ProjectPath:     noRecProj,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, expRec.Code, expRec.Body.String())
		assert.Contains(t, expRec.Body.String(), api.BrokerErrCodeMoveBundleHomeRecordMissing)
		assert.Contains(t, expRec.Body.String(), moveBundleHomeRecordMissingReason)
	})

	t.Run("global project dir uses <agentDir>/workspace (R2)", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		agentDir := config.GetAgentDir(globalDir, "global-dev", false)
		wsDir := filepath.Join(agentDir, "workspace")
		homeDir := config.GetAgentHomePath(globalDir, "global-dev")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		require.NoError(t, os.MkdirAll(homeDir, 0o755))
		writeTestHomeRecord(t, agentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(wsDir, "from-agent-dir.txt"), []byte("1234567"), 0o644))

		// Plant a stale Case-3 directory at ~/.scion/workspace/<slug> to ensure it is NOT read.
		staleDir := filepath.Join(globalDir, "workspace", "global-dev")
		require.NoError(t, os.MkdirAll(staleDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(staleDir, "stale.txt"), bytes.Repeat([]byte("z"), 999), 0o644))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/global-dev/move-bundle/precheck", api.MoveBundlePrecheckRequest{
			ProjectPath: globalDir,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.True(t, resp.Durable)
		assert.Equal(t, int64(7), resp.Workspace.Bytes, "must walk <agentDir>/workspace, not <globalDir>/workspace/<slug>")
	})

	t.Run("absolute in-home symlinked secret is not bundled", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, srcHome := newMoveBundleTestAgentDir(t, "dev-symlink")
		// No "user" in scion-agent.json (the real default); home record supplies UnixUsername/HomeAbs.
		require.NoError(t, os.WriteFile(filepath.Join(srcAgentDir, "scion-agent.json"), []byte(`{"harness":"claude"}`), 0o600))
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "w.txt"), []byte("w"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(srcHome, "dotfiles", "config", "gcloud"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, "dotfiles", "config", "gcloud", "credentials.db"), []byte("super-secret-gcloud-db"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, "dotfiles", "config", "app.conf"), []byte("portable=true\n"), 0o644))
		require.NoError(t, os.Symlink("/home/scion/dotfiles/config", filepath.Join(srcHome, ".config")))

		var storeMu sync.Mutex
		objects := map[string][]byte{}
		objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.URL.Path, "/")
			switch r.Method {
			case http.MethodPut:
				data, _ := io.ReadAll(r.Body)
				storeMu.Lock()
				objects[key] = data
				storeMu.Unlock()
				w.WriteHeader(http.StatusOK)
			case http.MethodGet:
				storeMu.Lock()
				data := objects[key]
				storeMu.Unlock()
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
			}
		}))
		defer objSrv.Close()

		srcSrv := newTestServer(t)
		srcSrv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			client:            objSrv.Client(),
			fetchUploadURL: func(_ context.Context, _, _, rid string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				return &api.MoveBundleUploadURLResponse{
					URL:     fmt.Sprintf("%s/%s/%s/part-%05d.tgz", objSrv.URL, rid, req.Root, req.PartIndex),
					Method:  http.MethodPut,
					Expires: time.Now().Add(15 * time.Minute),
				}, nil
			},
		}

		rec := doMoveBundleReq(t, srcSrv, http.MethodPost, "/api/v1/agents/dev-symlink/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-symlink-sec",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		st := waitForMoveBundleJob(t, srcSrv, "dev-symlink", "p1", api.MoveBundleJobKindExport, "rid-symlink-sec")
		require.Equal(t, api.MoveBundleJobStateSucceeded, st.State, "error=%s", st.Error)

		dstProject, dstAgentDir, _, dstHome := newMoveBundleTestAgentDir(t, "dev-symlink")
		writeTestMoveMarker(t, dstAgentDir, "rid-symlink-sec")
		_, manifestHash, err := bundle.CanonicalManifest(*st.Manifest)
		require.NoError(t, err)
		dstSrv := newTestServer(t)
		dstSrv.moveBundle = moveBundleSettings{
			client: objSrv.Client(),
			fetchDownloadURLs: func(_ context.Context, _, _, rid string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
				var parts []api.MoveBundleDownloadPartURL
				for rootName, rm := range st.Manifest.Roots {
					for _, p := range rm.Parts {
						parts = append(parts, api.MoveBundleDownloadPartURL{
							Root:    rootName,
							Index:   p.Index,
							URL:     fmt.Sprintf("%s/%s/%s/part-%05d.tgz", objSrv.URL, rid, rootName, p.Index),
							Expires: time.Now().Add(15 * time.Minute),
						})
					}
				}
				return &api.MoveBundleDownloadURLsResponse{Parts: parts, ExpiresInSeconds: 900}, nil
			},
		}
		impRec := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev-symlink/move-bundle/import?projectId=p1", api.MoveBundleImportRequest{
			ReincarnationID: "rid-symlink-sec",
			ProjectID:       "p1",
			ProjectPath:     dstProject,
			Manifest:        *st.Manifest,
			ManifestHash:    manifestHash,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, impRec.Code, impRec.Body.String())
		impSt := waitForMoveBundleJob(t, dstSrv, "dev-symlink", "p1", api.MoveBundleJobKindImport, "rid-symlink-sec")
		require.Equal(t, api.MoveBundleJobStateSucceeded, impSt.State, "error=%s", impSt.Error)

		_, err = os.Stat(filepath.Join(dstHome, "dotfiles", "config", "gcloud", "credentials.db"))
		assert.True(t, os.IsNotExist(err), "absolute in-home symlinked secret credentials.db must NOT be bundled")
		gotConf, err := os.ReadFile(filepath.Join(dstHome, "dotfiles", "config", "app.conf"))
		require.NoError(t, err)
		assert.Equal(t, "portable=true\n", string(gotConf))
	})
}

// TestMoveBundle_T6_ExportImportJobModelAndTransport covers design §9 T6 and
// C1f (scion-agent.json with no "user" + broker-owned record excludes auth,
// declared file secret, and hub-supplied file secret).
func TestMoveBundle_T6_ExportImportJobModelAndTransport(t *testing.T) {
	t.Run("roundtrip export and import with no-user scion-agent.json plus idempotent POST and 404 after restart", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, srcHome := newMoveBundleTestAgentDir(t, "dev")
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "main.go"), []byte("package main\n"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(srcWS, ".git", "refs"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".bash_history"), []byte("git status\n"), 0o600))

		// C1f: scion-agent.json intentionally has NO "user" field (the real default).
		// The broker-owned start record carries the resolved UnixUsername, file secret, and auth file.
		require.NoError(t, os.WriteFile(filepath.Join(srcAgentDir, "scion-agent.json"),
			[]byte(`{"harness":"claude","secrets":[{"key":"my_api_key","type":"file","target":"~/.config/custom-app/secret.key"}]}`), 0o600))
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{
			UnixUsername: "scion",
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "my_api_key", Type: "file", Target: "~/.config/custom-app/secret.key"},
				{Name: "ws_env_secret", Type: "file", Target: "/workspace/.env"},
			},
			ResolvedAuth: &api.ResolvedAuth{
				Files: []api.FileMapping{
					{SourcePath: "/host/claude-creds", ContainerPath: "~/.claude/.credentials.json"},
				},
			},
		})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, ".env"), []byte("WS_SECRET=do-not-bundle\n"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(srcWS, "config"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "config", "hub-sa.json"), []byte(`{"private_key":"hub-ws-secret"}`), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".config", "custom-app"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".config", "custom-app", "secret.key"), []byte("injected-secret-value"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".config", "hub-secret"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".config", "hub-secret", "token.pem"), []byte("hub-injected-secret"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".claude"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".claude", ".credentials.json"), []byte("claude-oauth-token"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".kube"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".kube", "config"), []byte("secret-kubeconfig"), 0o600))

		var storeMu sync.Mutex
		objects := map[string][]byte{}
		objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.URL.Path, "/")
			switch r.Method {
			case http.MethodPut:
				data, err := io.ReadAll(r.Body)
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				storeMu.Lock()
				objects[key] = data
				storeMu.Unlock()
				w.WriteHeader(http.StatusOK)
			case http.MethodGet:
				storeMu.Lock()
				data, ok := objects[key]
				storeMu.Unlock()
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		}))
		defer objSrv.Close()

		srcSrv := newTestServer(t)
		srcSrv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			client:            objSrv.Client(),
			fetchUploadURL: func(_ context.Context, _, _, rid string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				u := fmt.Sprintf("%s/%s/%s/part-%05d.tgz?X-Goog-Signature=TOPSECRETUPLOAD", objSrv.URL, rid, req.Root, req.PartIndex)
				return &api.MoveBundleUploadURLResponse{
					URL:     u,
					Method:  http.MethodPut,
					Headers: map[string]string{"Content-Type": "application/octet-stream"},
					Expires: time.Now().Add(15 * time.Minute),
				}, nil
			},
		}

		lim := api.DefaultMoveBundleLimits()
		lim.PartSize = 1024
		exportReq := api.MoveBundleExportRequest{
			ReincarnationID:   "rid-1",
			AgentID:           "agent-uuid-1",
			ProjectID:         "proj-1",
			ProjectPath:       srcProject,
			SecretFileTargets: []string{"~/.config/hub-secret/token.pem", "/workspace/config/hub-sa.json"},
			Limits:            lim,
		}
		rec := doMoveBundleReq(t, srcSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/export?projectId=proj-1", exportReq)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		// Idempotent POST with the same rid returns 202 with the same job.
		rec2 := doMoveBundleReq(t, srcSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/export?projectId=proj-1", exportReq)
		require.Equal(t, http.StatusAccepted, rec2.Code, rec2.Body.String())

		st := waitForMoveBundleJob(t, srcSrv, "dev", "proj-1", api.MoveBundleJobKindExport, "rid-1")
		require.Equal(t, api.MoveBundleJobStateSucceeded, st.State, "error=%s", st.Error)
		require.NotNil(t, st.Manifest)

		// Simulated broker restart (fresh server) returns 404 for lost job.
		restartedSrv := newTestServer(t)
		rec404 := doMoveBundleReq(t, restartedSrv, http.MethodGet, "/api/v1/agents/dev/move-bundle/export/rid-1?projectId=proj-1", nil)
		assert.Equal(t, http.StatusNotFound, rec404.Code)

		// Import onto a target broker.
		dstProject, dstAgentDir, dstWS, dstHome := newMoveBundleTestAgentDir(t, "dev")
		writeTestMoveMarker(t, dstAgentDir, "rid-1")
		_, manifestHash, err := bundle.CanonicalManifest(*st.Manifest)
		require.NoError(t, err)

		dstSrv := newTestServer(t)
		dstSrv.moveBundle = moveBundleSettings{
			client: objSrv.Client(),
			fetchDownloadURLs: func(_ context.Context, _, _, rid string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
				var parts []api.MoveBundleDownloadPartURL
				for rootName, rm := range st.Manifest.Roots {
					for _, p := range rm.Parts {
						parts = append(parts, api.MoveBundleDownloadPartURL{
							Root:    rootName,
							Index:   p.Index,
							URL:     fmt.Sprintf("%s/%s/%s/part-%05d.tgz?X-Goog-Signature=TOPSECRETDL", objSrv.URL, rid, rootName, p.Index),
							Expires: time.Now().Add(15 * time.Minute),
						})
					}
				}
				return &api.MoveBundleDownloadURLsResponse{
					Parts:            parts,
					Expires:          time.Now().Add(15 * time.Minute),
					ExpiresInSeconds: 900,
				}, nil
			},
		}

		importReq := api.MoveBundleImportRequest{
			ReincarnationID: "rid-1",
			AgentID:         "agent-uuid-1",
			ProjectID:       "proj-1",
			ProjectPath:     dstProject,
			Manifest:        *st.Manifest,
			ManifestHash:    manifestHash,
			Limits:          lim,
		}
		impRec := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=proj-1", importReq)
		require.Equal(t, http.StatusAccepted, impRec.Code, impRec.Body.String())

		impSt := waitForMoveBundleJob(t, dstSrv, "dev", "proj-1", api.MoveBundleJobKindImport, "rid-1")
		require.Equal(t, api.MoveBundleJobStateSucceeded, impSt.State, "error=%s", impSt.Error)

		gotMain, err := os.ReadFile(filepath.Join(dstWS, "main.go"))
		require.NoError(t, err)
		assert.Equal(t, "package main\n", string(gotMain))
		gotHist, err := os.ReadFile(filepath.Join(dstHome, ".bash_history"))
		require.NoError(t, err)
		assert.Equal(t, "git status\n", string(gotHist))
		// D6 excluded credential, declared file secret, hub-supplied file secret, and harness auth file were never copied.
		_, err = os.Stat(filepath.Join(dstHome, ".kube", "config"))
		assert.True(t, os.IsNotExist(err))
		_, err = os.Stat(filepath.Join(dstHome, ".config", "custom-app", "secret.key"))
		assert.True(t, os.IsNotExist(err), "declared file-type secret target must be excluded from bundle")
		_, err = os.Stat(filepath.Join(dstHome, ".config", "hub-secret", "token.pem"))
		assert.True(t, os.IsNotExist(err), "hub-supplied secret file target must be excluded from bundle")
		_, err = os.Stat(filepath.Join(dstHome, ".claude", ".credentials.json"))
		assert.True(t, os.IsNotExist(err), "harness auth file target must be excluded from bundle")
		// R1: workspace-targeted file secrets (record + hub-supplied) must never be bundled.
		_, err = os.Stat(filepath.Join(dstWS, ".env"))
		assert.True(t, os.IsNotExist(err), "record workspace file-secret /workspace/.env must be excluded from bundle")
		_, err = os.Stat(filepath.Join(dstWS, "config", "hub-sa.json"))
		assert.True(t, os.IsNotExist(err), "hub-supplied workspace file-secret /workspace/config/hub-sa.json must be excluded from bundle")
	})

	t.Run("409 conflict on different rid while running", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		block := make(chan struct{})
		entered := make(chan struct{}, 2)
		srv := newTestServer(t)
		srv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			fetchUploadURL: func(ctx context.Context, _, _, _ string, _ *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-block:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return nil, context.Canceled
			},
		}

		rec1 := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-a",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, rec1.Code)
		<-entered

		// Different rid while rid-a is running -> 409 Conflict.
		rec2 := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-b",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, rec2.Code)
		assert.Contains(t, rec2.Body.String(), api.BrokerErrCodeMoveBundleJobConflict)

		close(block)
		_ = waitForMoveBundleJob(t, srv, "dev", "p1", api.MoveBundleJobKindExport, "rid-a")
	})

	t.Run("job timeout transitions to failed", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			fetchUploadURL: func(ctx context.Context, _, _, _ string, _ *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}

		lim := api.DefaultMoveBundleLimits()
		lim.TimeoutSeconds = 1
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-timeout",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          lim,
		})
		require.Equal(t, http.StatusAccepted, rec.Code)

		st := waitForMoveBundleJob(t, srv, "dev", "p1", api.MoveBundleJobKindExport, "rid-timeout")
		assert.Equal(t, api.MoveBundleJobStateFailed, st.State)
		assert.Contains(t, st.Error, "timeout")
	})

	t.Run("URL redaction in export and import errors", func(t *testing.T) {
		const secretToken = "SUPER-SECRET-SIGNED-URL-TOKEN-98765"
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("SignatureDoesNotMatch " + secretToken))
		}))
		defer objSrv.Close()

		srv := newTestServer(t)
		srv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			client:            objSrv.Client(),
			partBackoff:       []time.Duration{},
			fetchUploadURL: func(_ context.Context, _, _, _ string, _ *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				return &api.MoveBundleUploadURLResponse{
					URL:     objSrv.URL + "/part?X-Goog-Signature=" + secretToken,
					Method:  http.MethodPut,
					Expires: time.Now().Add(15 * time.Minute),
				}, nil
			},
		}

		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-redact",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, rec.Code)

		st := waitForMoveBundleJob(t, srv, "dev", "p1", api.MoveBundleJobKindExport, "rid-redact")
		assert.Equal(t, api.MoveBundleJobStateFailed, st.State)
		assert.NotContains(t, st.Error, secretToken, "signed URL token must never appear in job error")
		assert.NotContains(t, st.Error, objSrv.URL, "signed URL host must never appear in job error")
		assert.Contains(t, st.Error, "http_403")
	})

	t.Run("signed-URL refresh at expires-60s and on 403", func(t *testing.T) {
		_, srcAgentDir, srcWS, srcHome := newMoveBundleTestAgentDir(t, "dev")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "big.bin"), bytes.Repeat([]byte("abcdefgh12345678"), 256), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(srcHome, "h.txt"), []byte("home"), 0o644))

		partsData := map[string][]byte{}
		wsRoot, err := os.OpenRoot(srcWS)
		require.NoError(t, err)
		wsRM, err := bundle.Write(context.Background(), wsRoot, t.TempDir(), bundle.WorkspaceExcluder(), bundle.Limits{
			MaxBytes: 1 << 20, MaxEntries: 100, MaxDepth: 16, MaxPathLen: 256, PartSize: 64,
		}, partSinkFunc(func(_ context.Context, idx int, r io.ReadSeeker, _ int64, _ string) error {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			partsData[fmt.Sprintf("workspace/%d", idx)] = b
			return nil
		}))
		require.NoError(t, err)
		_ = wsRoot.Close()
		require.GreaterOrEqual(t, len(wsRM.Parts), 2, "need at least 2 workspace parts to test URL refresh")

		homeEx, err := loadMoveBundleHomeExcluder(srcAgentDir, nil)
		require.NoError(t, err)
		homeRoot, err := os.OpenRoot(srcHome)
		require.NoError(t, err)
		homeRM, err := bundle.Write(context.Background(), homeRoot, t.TempDir(), homeEx, bundle.Limits{
			MaxBytes: 1 << 20, MaxEntries: 100, MaxDepth: 16, MaxPathLen: 256, PartSize: 64,
		}, partSinkFunc(func(_ context.Context, idx int, r io.ReadSeeker, _ int64, _ string) error {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			partsData[fmt.Sprintf("home/%d", idx)] = b
			return nil
		}))
		require.NoError(t, err)
		_ = homeRoot.Close()

		manifest := bundle.Manifest{
			Version:         bundle.ManifestVersion,
			ReincarnationID: "rid-refresh",
			Roots: map[string]bundle.RootManifest{
				bundle.RootWorkspace: wsRM,
				bundle.RootHome:      homeRM,
			},
		}
		_, manifestHash, err := bundle.CanonicalManifest(manifest)
		require.NoError(t, err)

		var fetchCount atomic.Int32
		objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("gen") == "1" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			key := strings.TrimPrefix(r.URL.Path, "/")
			data, ok := partsData[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		}))
		defer objSrv.Close()

		dstProject, dstAgentDir, dstWS, _ := newMoveBundleTestAgentDir(t, "dev")
		writeTestMoveMarker(t, dstAgentDir, "rid-refresh")
		dstSrv := newTestServer(t)
		dstSrv.moveBundle = moveBundleSettings{
			client: objSrv.Client(),
			fetchDownloadURLs: func(_ context.Context, _, _, _ string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
				gen := fetchCount.Add(1)
				expSec := int64(900)
				if gen == 1 {
					expSec = 30
				}
				var parts []api.MoveBundleDownloadPartURL
				for rootName, rm := range manifest.Roots {
					for _, p := range rm.Parts {
						parts = append(parts, api.MoveBundleDownloadPartURL{
							Root:    rootName,
							Index:   p.Index,
							URL:     fmt.Sprintf("%s/%s/%d?gen=%d&sig=SECRET", objSrv.URL, rootName, p.Index, gen),
							Expires: time.Now().Add(time.Duration(expSec) * time.Second),
						})
					}
				}
				return &api.MoveBundleDownloadURLsResponse{
					Parts:            parts,
					Expires:          time.Now().Add(time.Duration(expSec) * time.Second),
					ExpiresInSeconds: expSec,
				}, nil
			},
		}

		lim := api.DefaultMoveBundleLimits()
		lim.PartSize = 64
		rec := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=p1", api.MoveBundleImportRequest{
			ReincarnationID: "rid-refresh",
			ProjectID:       "p1",
			ProjectPath:     dstProject,
			Manifest:        manifest,
			ManifestHash:    manifestHash,
			Limits:          lim,
		})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		st := waitForMoveBundleJob(t, dstSrv, "dev", "p1", api.MoveBundleJobKindImport, "rid-refresh")
		require.Equal(t, api.MoveBundleJobStateSucceeded, st.State, "error=%s", st.Error)
		assert.GreaterOrEqual(t, fetchCount.Load(), int32(2), "expected signed URLs to be refreshed")

		got, err := os.ReadFile(filepath.Join(dstWS, "big.bin"))
		require.NoError(t, err)
		assert.Equal(t, bytes.Repeat([]byte("abcdefgh12345678"), 256), got)
	})

	t.Run("job registry GC removes finished jobs after 1h TTL", func(t *testing.T) {
		now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		reg := newMoveBundleJobRegistry(func() time.Time { return now }, time.Hour)
		key := moveBundleAgentKey{ProjectID: "p1", AgentSlug: "dev", Kind: api.MoveBundleJobKindExport}
		job, conflict, created := reg.start(key, "rid-gc", func() {})
		require.True(t, created)
		require.False(t, conflict)
		job.markSucceeded(nil, nil, now)

		now = now.Add(59 * time.Minute)
		_, ok := reg.get(key, "rid-gc")
		assert.True(t, ok)

		now = now.Add(2 * time.Minute)
		_, ok = reg.get(key, "rid-gc")
		assert.False(t, ok)
	})

	t.Run("export refuses non-durable runtime (S3)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-nondurable")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "kubernetes"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-nondurable/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-nondurable",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)
	})

	t.Run("export refuses running agent (S3)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-running")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		mgr := &mockManager{
			agents: []api.AgentInfo{
				{
					ID:              "container-running",
					Name:            "dev-running",
					ProjectID:       "p1",
					Phase:           "running",
					ContainerStatus: "Up 2 minutes",
				},
			},
		}
		srv := newTestServerWithManager(t, mgr)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-running/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-running",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), api.BrokerErrCodeMoveBundleJobConflict)
		assert.Contains(t, rec.Body.String(), "agent must be stopped before bundle export")
	})

	t.Run("export refuses with 409 when no runtime manager resolves (O3)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-nomgr")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		srv := newTestServerWithManager(t, nil)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-nomgr/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-nomgr",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), api.BrokerErrCodeMoveBundleJobConflict)
	})

	t.Run("export refuses git worktree workspace (O4)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-worktree")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, ".git"), []byte("gitdir: /host/repo/.git/worktrees/dev-worktree\n"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-worktree/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-worktree",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)
	})

	t.Run("job status resolves projectId consistently with POST (N1)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-n1")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			partBackoff:       []time.Duration{},
			fetchUploadURL: func(_ context.Context, _, _, _ string, _ *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				return nil, fmt.Errorf("stop early")
			},
		}

		// Mismatched query vs body projectId is rejected with 400.
		badRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-n1/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-n1-bad",
			ProjectID:       "p2",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		assert.Equal(t, http.StatusBadRequest, badRec.Code, badRec.Body.String())

		// POST with body ProjectID only (no query projectId) is retrievable by GET with no query projectId.
		postRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-n1/move-bundle/export", api.MoveBundleExportRequest{
			ReincarnationID: "rid-n1-ok",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, postRec.Code, postRec.Body.String())

		st := waitForMoveBundleJob(t, srv, "dev-n1", "", api.MoveBundleJobKindExport, "rid-n1-ok")
		assert.Equal(t, "rid-n1-ok", st.ReincarnationID)
	})

	t.Run("job errors are class-only and carry no host path (Part D S1)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-hostpath")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		// Remove workspace dir so runMoveBundleExport hits an os.OpenRoot *fs.PathError
		// containing the absolute host path srcWS.
		require.NoError(t, os.RemoveAll(srcWS))

		srv := newTestServer(t)
		srv.moveBundle.durabilityRuntime = "docker"
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-hostpath/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-hostpath",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		st := waitForMoveBundleJob(t, srv, "dev-hostpath", "p1", api.MoveBundleJobKindExport, "rid-hostpath")
		assert.Equal(t, api.MoveBundleJobStateFailed, st.State)
		assert.Equal(t, "bundle export failed: filesystem operation failed", st.Error)
		assert.NotContains(t, st.Error, srcWS, "job error must not leak absolute host workspace path")
		assert.NotContains(t, st.Error, "/", "job error must not contain host path separators")
	})

	t.Run("precheck refuses while a move-bundle job is still running (Part D S2)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-busy")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "a.txt"), []byte("data"), 0o644))

		block := make(chan struct{})
		entered := make(chan struct{}, 2)
		srv := newTestServer(t)
		srv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			partBackoff:       []time.Duration{},
			fetchUploadURL: func(ctx context.Context, _, _, _ string, _ *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-block:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return nil, context.Canceled
			},
		}

		expRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-busy/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-busy-1",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, expRec.Code, expRec.Body.String())
		<-entered

		preRec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-busy/move-bundle/precheck?projectId=p1", api.MoveBundlePrecheckRequest{
			ProjectID:   "p1",
			ProjectPath: srcProject,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, preRec.Code, preRec.Body.String())
		var preResp api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(preRec.Body.Bytes(), &preResp))
		assert.False(t, preResp.Durable)
		assert.Equal(t, "a previous move-bundle job is still running", preResp.Reason)

		close(block)
		_ = waitForMoveBundleJob(t, srv, "dev-busy", "p1", api.MoveBundleJobKindExport, "rid-busy-1")

		// Once the job finishes, precheck succeeds again.
		preRec2 := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-busy/move-bundle/precheck?projectId=p1", api.MoveBundlePrecheckRequest{
			ProjectID:   "p1",
			ProjectPath: srcProject,
			Limits:      api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusOK, preRec2.Code, preRec2.Body.String())
		var preResp2 api.MoveBundlePrecheckResponse
		require.NoError(t, json.Unmarshal(preRec2.Body.Bytes(), &preResp2))
		assert.True(t, preResp2.Durable)
	})

	t.Run("limit exceeded job error names the root (O3)", func(t *testing.T) {
		srcProject, srcAgentDir, srcWS, _ := newMoveBundleTestAgentDir(t, "dev-limroot")
		writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
		require.NoError(t, os.WriteFile(filepath.Join(srcWS, "over.txt"), []byte("12345678901"), 0o644))

		srv := newTestServer(t)
		srv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			partBackoff:       []time.Duration{},
			fetchUploadURL: func(ctx context.Context, _, _, _ string, _ *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}

		lim := api.DefaultMoveBundleLimits()
		lim.WorkspaceMaxBytes = 5
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents/dev-limroot/move-bundle/export?projectId=p1", api.MoveBundleExportRequest{
			ReincarnationID: "rid-limroot",
			ProjectID:       "p1",
			ProjectPath:     srcProject,
			Limits:          lim,
		})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		st := waitForMoveBundleJob(t, srv, "dev-limroot", "p1", api.MoveBundleJobKindExport, "rid-limroot")
		assert.Equal(t, api.MoveBundleJobStateFailed, st.State)
		assert.True(t, strings.HasPrefix(st.Error, "bundle exceeds limit: workspace "), "got error=%q", st.Error)
	})

	t.Run("startup sweeps stale move-bundle scratch directories (O4)", func(t *testing.T) {
		srv := newTestServer(t)
		srv.config.Host = "127.0.0.1"
		stateDir := t.TempDir()
		srv.stateDir = stateDir

		staleExport := filepath.Join(stateDir, "move-bundle-scratch", "mb-export-stale1")
		staleImport := filepath.Join(stateDir, "move-bundle-scratch", "mb-import-stale2")
		require.NoError(t, os.MkdirAll(staleExport, 0o700))
		require.NoError(t, os.MkdirAll(staleImport, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(staleExport, "part.tgz"), []byte("stale"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(staleImport, "part.tgz"), []byte("stale"), 0o600))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, srv.StartServices(ctx))
		defer func() { _ = srv.Shutdown(context.Background()) }()

		_, err := os.Stat(staleExport)
		assert.True(t, os.IsNotExist(err), "stale export scratch dir must be swept on startup")
		_, err = os.Stat(staleImport)
		assert.True(t, os.IsNotExist(err), "stale import scratch dir must be swept on startup")
	})
}

type partSinkFunc func(ctx context.Context, index int, part io.ReadSeeker, size int64, hash string) error

func (f partSinkFunc) Put(ctx context.Context, index int, part io.ReadSeeker, size int64, hash string) error {
	return f(ctx, index, part, size, hash)
}
