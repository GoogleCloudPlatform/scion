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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildTestBundleParts(t *testing.T, rid, wsContent, homeContent string) (bundle.Manifest, string, map[string][]byte) {
	t.Helper()
	_, srcAgentDir, srcWS, srcHome := newMoveBundleTestAgentDir(t, "src-builder")
	writeTestHomeRecord(t, srcAgentDir, scionrt.RunConfig{UnixUsername: "scion"})
	require.NoError(t, os.WriteFile(filepath.Join(srcWS, "x.txt"), []byte(wsContent), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, "y.txt"), []byte(homeContent), 0o644))

	partsData := map[string][]byte{}
	wsRoot, err := os.OpenRoot(srcWS)
	require.NoError(t, err)
	wsRM, err := bundle.Write(context.Background(), wsRoot, t.TempDir(), bundle.WorkspaceExcluder(), bundle.DefaultWorkspaceLimits(),
		partSinkFunc(func(_ context.Context, idx int, r io.ReadSeeker, _ int64, _ string) error {
			b, err := io.ReadAll(r)
			partsData[fmt.Sprintf("workspace/%d", idx)] = b
			return err
		}))
	require.NoError(t, err)
	_ = wsRoot.Close()

	homeEx, err := loadMoveBundleHomeExcluder(srcAgentDir, nil)
	require.NoError(t, err)
	homeRoot, err := os.OpenRoot(srcHome)
	require.NoError(t, err)
	homeRM, err := bundle.Write(context.Background(), homeRoot, t.TempDir(), homeEx, bundle.DefaultHomeLimits(),
		partSinkFunc(func(_ context.Context, idx int, r io.ReadSeeker, _ int64, _ string) error {
			b, err := io.ReadAll(r)
			partsData[fmt.Sprintf("home/%d", idx)] = b
			return err
		}))
	require.NoError(t, err)
	_ = homeRoot.Close()

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
	return manifest, manifestHash, partsData
}

// TestMoveBundle_T7_CommitSemanticsAndMarker covers design §9 T7, R3, S2, S4.
func TestMoveBundle_T7_CommitSemanticsAndMarker(t *testing.T) {
	t.Run("workspace commit into empty dir, all-or-nothing conflict check (R3), and identity name refusal (S2)", func(t *testing.T) {
		_, _, wsDir, _ := newMoveBundleTestAgentDir(t, "dev")
		stageName, err := newMoveBundleStageName(filepath.Base(wsDir))
		require.NoError(t, err)
		stagePath := filepath.Join(filepath.Dir(wsDir), stageName)
		require.NoError(t, os.MkdirAll(filepath.Join(stagePath, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stagePath, "a.txt"), []byte("alpha"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(stagePath, "sub", "b.txt"), []byte("beta"), 0o644))

		require.NoError(t, commitMoveBundleWorkspace(wsDir, stageName))
		gotA, err := os.ReadFile(filepath.Join(wsDir, "a.txt"))
		require.NoError(t, err)
		assert.Equal(t, "alpha", string(gotA))
		gotB, err := os.ReadFile(filepath.Join(wsDir, "sub", "b.txt"))
		require.NoError(t, err)
		assert.Equal(t, "beta", string(gotB))

		// R3: stage has "00-new.txt" (sorts first) and "a.txt" (collides).
		// Neither may be moved into wsDir.
		stage2, err := newMoveBundleStageName(filepath.Base(wsDir))
		require.NoError(t, err)
		stage2Path := filepath.Join(filepath.Dir(wsDir), stage2)
		require.NoError(t, os.MkdirAll(stage2Path, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stage2Path, "00-new.txt"), []byte("partial"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(stage2Path, "a.txt"), []byte("overwrite"), 0o644))

		err = commitMoveBundleWorkspace(wsDir, stage2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
		_, statErr := os.Stat(filepath.Join(wsDir, "00-new.txt"))
		assert.True(t, os.IsNotExist(statErr), "conflict on a later entry must not leave earlier entries partially committed")

		// S2: stage containing root ".scion" (workspace identity name) must be refused before any rename.
		_, _, emptyWS, _ := newMoveBundleTestAgentDir(t, "dev-id")
		stage3, err := newMoveBundleStageName(filepath.Base(emptyWS))
		require.NoError(t, err)
		stage3Path := filepath.Join(filepath.Dir(emptyWS), stage3)
		require.NoError(t, os.MkdirAll(filepath.Join(stage3Path, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stage3Path, "00-ok.txt"), []byte("ok"), 0o644))

		err = commitMoveBundleWorkspace(emptyWS, stage3)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reserved workspace identity")
		entries, err := os.ReadDir(emptyWS)
		require.NoError(t, err)
		assert.Empty(t, entries, "refused identity entry must leave workspace untouched")
	})

	t.Run("home merge where provision wins and skips are counted", func(t *testing.T) {
		_, _, _, homeDir := newMoveBundleTestAgentDir(t, "dev")
		require.NoError(t, os.MkdirAll(filepath.Join(homeDir, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".scion", "agent-info.json"), []byte(`{"target":true}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte("[user]\n\tname = target\n"), 0o644))

		stageName, err := newMoveBundleStageName(filepath.Base(homeDir))
		require.NoError(t, err)
		stagePath := filepath.Join(filepath.Dir(homeDir), stageName)
		require.NoError(t, os.MkdirAll(filepath.Join(stagePath, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stagePath, ".scion", "agent-info.json"), []byte(`{"source":true}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(stagePath, ".scion", "history.log"), []byte("from-source\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(stagePath, ".gitconfig"), []byte("[user]\n\tname = source\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(stagePath, "notes.txt"), []byte("agent notes"), 0o600))

		skipped, err := commitMoveBundleHome(homeDir, stageName)
		require.NoError(t, err)
		assert.Equal(t, int64(2), skipped, "agent-info.json and .gitconfig must be skipped")

		gotInfo, err := os.ReadFile(filepath.Join(homeDir, ".scion", "agent-info.json"))
		require.NoError(t, err)
		assert.Equal(t, `{"target":true}`, string(gotInfo))
		gotGit, err := os.ReadFile(filepath.Join(homeDir, ".gitconfig"))
		require.NoError(t, err)
		assert.Contains(t, string(gotGit), "target")

		gotHist, err := os.ReadFile(filepath.Join(homeDir, ".scion", "history.log"))
		require.NoError(t, err)
		assert.Equal(t, "from-source\n", string(gotHist))
		gotNotes, err := os.ReadFile(filepath.Join(homeDir, "notes.txt"))
		require.NoError(t, err)
		assert.Equal(t, "agent notes", string(gotNotes))
	})

	t.Run("import requires matching move marker (R3)", func(t *testing.T) {
		manifest, manifestHash, _ := buildTestBundleParts(t, "rid-expect", "x", "y")
		dstProject, dstAgentDir, _, _ := newMoveBundleTestAgentDir(t, "dev")
		dstSrv := newTestServer(t)
		dstSrv.moveBundle.durabilityRuntime = "docker"

		impReq := api.MoveBundleImportRequest{
			ReincarnationID: "rid-expect",
			ProjectID:       "p1",
			ProjectPath:     dstProject,
			Manifest:        manifest,
			ManifestHash:    manifestHash,
			Limits:          api.DefaultMoveBundleLimits(),
		}

		// No move marker at all -> 409 move_marker_mismatch.
		rec := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=p1", impReq)
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), api.BrokerErrCodeMoveMarkerMismatch)

		// Move marker for a different rid -> 409 move_marker_mismatch.
		writeTestMoveMarker(t, dstAgentDir, "rid-other")
		rec2 := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=p1", impReq)
		assert.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())
		assert.Contains(t, rec2.Body.String(), api.BrokerErrCodeMoveMarkerMismatch)
	})

	t.Run("imported marker prevents repeat import with 409", func(t *testing.T) {
		manifest, manifestHash, partsData := buildTestBundleParts(t, "rid-marker", "x", "y")
		objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.URL.Path, "/")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(partsData[key])
		}))
		defer objSrv.Close()

		dstProject, dstAgentDir, _, _ := newMoveBundleTestAgentDir(t, "dev")
		writeTestMoveMarker(t, dstAgentDir, "rid-marker")
		dstSrv := newTestServer(t)
		dstSrv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			client:            objSrv.Client(),
			fetchDownloadURLs: func(_ context.Context, _, _, _ string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
				var parts []api.MoveBundleDownloadPartURL
				for rootName, rm := range manifest.Roots {
					for _, p := range rm.Parts {
						parts = append(parts, api.MoveBundleDownloadPartURL{
							Root:    rootName,
							Index:   p.Index,
							URL:     fmt.Sprintf("%s/%s/%d", objSrv.URL, rootName, p.Index),
							Expires: time.Now().Add(15 * time.Minute),
						})
					}
				}
				return &api.MoveBundleDownloadURLsResponse{Parts: parts, ExpiresInSeconds: 900}, nil
			},
		}

		impReq := api.MoveBundleImportRequest{
			ReincarnationID: "rid-marker",
			ProjectID:       "p1",
			ProjectPath:     dstProject,
			Manifest:        manifest,
			ManifestHash:    manifestHash,
			Limits:          api.DefaultMoveBundleLimits(),
		}
		rec := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=p1", impReq)
		require.Equal(t, http.StatusAccepted, rec.Code)
		st := waitForMoveBundleJob(t, dstSrv, "dev", "p1", api.MoveBundleJobKindImport, "rid-marker")
		require.Equal(t, api.MoveBundleJobStateSucceeded, st.State, "error=%s", st.Error)

		ok, err := hasMoveBundleImportedMarker(dstAgentDir, "rid-marker")
		require.NoError(t, err)
		assert.True(t, ok)

		// Repeat import on a fresh server (simulating restart) must be refused with 409.
		dstSrv2 := newTestServer(t)
		dstSrv2.moveBundle.durabilityRuntime = "docker"
		rec2 := doMoveBundleReq(t, dstSrv2, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=p1", impReq)
		assert.Equal(t, http.StatusConflict, rec2.Code)
		assert.Contains(t, rec2.Body.String(), api.BrokerErrCodeMoveBundleAlreadyImported)
	})

	t.Run("crash mid-commit leaves .bundle-* siblings removed by marker-guarded delete", func(t *testing.T) {
		manifest, manifestHash, partsData := buildTestBundleParts(t, "rid-crash", "w", "h")
		objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.URL.Path, "/")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(partsData[key])
		}))
		defer objSrv.Close()

		dstProject, dstAgentDir, _, _ := newMoveBundleTestAgentDir(t, "dev")
		writeTestMoveMarker(t, dstAgentDir, "rid-crash")

		prevHook := testHookMoveBundleCommit
		testHookMoveBundleCommit = func(step string) error {
			if step == "before_home_commit" {
				return errMoveBundleCommitCrash
			}
			return nil
		}
		t.Cleanup(func() { testHookMoveBundleCommit = prevHook })

		dstSrv := newTestServer(t)
		dstSrv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			client:            objSrv.Client(),
			fetchDownloadURLs: func(_ context.Context, _, _, _ string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
				var parts []api.MoveBundleDownloadPartURL
				for rootName, rm := range manifest.Roots {
					for _, p := range rm.Parts {
						parts = append(parts, api.MoveBundleDownloadPartURL{
							Root:    rootName,
							Index:   p.Index,
							URL:     fmt.Sprintf("%s/%s/%d", objSrv.URL, rootName, p.Index),
							Expires: time.Now().Add(15 * time.Minute),
						})
					}
				}
				return &api.MoveBundleDownloadURLsResponse{Parts: parts, ExpiresInSeconds: 900}, nil
			},
		}

		rec := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=p1", api.MoveBundleImportRequest{
			ReincarnationID: "rid-crash",
			ProjectID:       "p1",
			ProjectPath:     dstProject,
			Manifest:        manifest,
			ManifestHash:    manifestHash,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, rec.Code)
		st := waitForMoveBundleJob(t, dstSrv, "dev", "p1", api.MoveBundleJobKindImport, "rid-crash")
		assert.Equal(t, api.MoveBundleJobStateFailed, st.State)

		entries, err := os.ReadDir(dstAgentDir)
		require.NoError(t, err)
		var stageCount int
		for _, e := range entries {
			if isMoveBundleStageName(e.Name(), "workspace") || isMoveBundleStageName(e.Name(), "home") {
				stageCount++
			}
		}
		assert.Equal(t, 2, stageCount, "expected .bundle-* siblings after simulated crash")
		imported, err := hasMoveBundleImportedMarker(dstAgentDir, "rid-crash")
		require.NoError(t, err)
		assert.False(t, imported, "imported marker must not be written before both roots commit")

		_, err = agent.DeleteHubAgentFiles("dev", "dev", dstProject, "", false)
		require.NoError(t, err)
		_, err = os.Stat(dstAgentDir)
		assert.True(t, os.IsNotExist(err), "agentDir and .bundle-* siblings must be removed by rollback delete")
	})

	t.Run("agent delete cancels in-flight move-bundle import and waits for it to stop (S4)", func(t *testing.T) {
		manifest, manifestHash, _ := buildTestBundleParts(t, "rid-cancel-del", "w", "h")
		dstProject, dstAgentDir, _, _ := newMoveBundleTestAgentDir(t, "dev")
		writeTestMoveMarker(t, dstAgentDir, "rid-cancel-del")

		entered := make(chan struct{}, 2)
		canceled := make(chan struct{}, 2)
		dstSrv := newTestServer(t)
		dstSrv.moveBundle = moveBundleSettings{
			durabilityRuntime: "docker",
			fetchDownloadURLs: func(ctx context.Context, _, _, _ string, _ *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-ctx.Done()
				select {
				case canceled <- struct{}{}:
				default:
				}
				return nil, ctx.Err()
			},
		}

		rec := doMoveBundleReq(t, dstSrv, http.MethodPost, "/api/v1/agents/dev/move-bundle/import?projectId=p1", api.MoveBundleImportRequest{
			ReincarnationID: "rid-cancel-del",
			ProjectID:       "p1",
			ProjectPath:     dstProject,
			Manifest:        manifest,
			ManifestHash:    manifestHash,
			Limits:          api.DefaultMoveBundleLimits(),
		})
		require.Equal(t, http.StatusAccepted, rec.Code)
		<-entered

		// Trigger delete on the broker; it must cancel the in-flight import and wait for it to stop.
		delRec := doMoveBundleReq(t, dstSrv, http.MethodDelete, "/api/v1/agents/dev?deleteFiles=true&projectId=p1", nil)
		_ = delRec // may be 204 or 404 depending on mock container state; key invariant is job stopped before return.

		select {
		case <-canceled:
		default:
			t.Fatal("delete returned before canceling the in-flight move-bundle job")
		}
		stRec := doMoveBundleReq(t, dstSrv, http.MethodGet, "/api/v1/agents/dev/move-bundle/import/rid-cancel-del?projectId=p1", nil)
		require.Equal(t, http.StatusOK, stRec.Code)
		assert.Contains(t, stRec.Body.String(), `"state":"failed"`, "in-flight job must have finished before delete returned")
	})

	t.Run("ProvisionOnly validation for WorkspaceSource=move-bundle", func(t *testing.T) {
		srv := newTestServer(t)
		rec := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:            "dev",
			ProvisionOnly:   true,
			WorkspaceSource: api.WorkspaceSourceMoveBundle,
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		rec2 := doMoveBundleReq(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:                       "dev",
			ProvisionOnly:              true,
			ExpectExistingNFSWorkspace: agent.MoveWorkspaceFreshAgentDir,
			MoveReincarnationID:        "rid-1",
			WorkspaceSource:            "bogus",
		})
		assert.Equal(t, http.StatusBadRequest, rec2.Code)
	})
}
