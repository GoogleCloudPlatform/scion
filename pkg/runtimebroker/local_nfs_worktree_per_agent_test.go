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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestBuildStartContext_LocalContainerNFSRefusesWorktreePerAgent_NoSideEffects(t *testing.T) {
	for _, rt := range []string{"docker", "podman", "container"} {
		for _, op := range []startOperation{opCreate, opHTTPStart, opHTTPRestart} {
			t.Run(rt+"/"+string(op), func(t *testing.T) {
				srv := newTestServerForStartContextRuntime(t, ServerConfig{
					StorageLayout: projectfs.Layout{
						Backend:     projectfs.BackendNFS,
						HostBase:    filepath.Join(t.TempDir(), "nfs"),
						SubPathRoot: "projects",
					},
					WorkspaceStorageBackend: "nfs",
				}, rt)
				projectDir := t.TempDir()

				in := startContextInputs{
					Name:        "nfs-wt-agent",
					Slug:        "nfs-wt-agent",
					AgentID:     "agent-nfs-wt",
					ProjectID:   "proj-nfs-wt",
					ProjectSlug: "proj-nfs-wt",
					ProjectPath: projectDir,
					Operation:   op,
					Config: &CreateAgentConfig{
						GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
					},
					ResolvedEnv: map[string]string{
						"SCION_WORKSPACE_MODE": string(store.SharingModeWorktreePerAgent),
						"SCION_WORKSPACE_GIT":  "true",
					},
				}
				if op != opHTTPRestart {
					in.WorkspaceMode = store.WorkspaceModeWorktreePerAgent
				}

				sc, err := srv.buildStartContext(context.Background(), in)
				if sc != nil {
					t.Fatalf("expected nil startContext, got %+v", sc)
				}
				sce := requireStartContextError(t, err)
				if sce.Status != http.StatusPreconditionFailed {
					t.Errorf("status = %d, want %d (412)", sce.Status, http.StatusPreconditionFailed)
				}
				if sce.Code != api.BrokerErrCodeUnsupportedCapability {
					t.Errorf("code = %q, want %q", sce.Code, api.BrokerErrCodeUnsupportedCapability)
				}
				for _, want := range []string{"worktree-per-agent", "nfs", "clone-per-agent", "project_storage"} {
					if !strings.Contains(sce.Message, want) {
						t.Errorf("message %q must mention %q", sce.Message, want)
					}
				}
				for _, sub := range []string{"workspace", "worktrees", ".git"} {
					if _, statErr := os.Lstat(filepath.Join(projectDir, sub)); !os.IsNotExist(statErr) {
						t.Errorf("host %s created on refusal: stat err = %v", sub, statErr)
					}
				}
			})
		}
	}

	// Regression: local project storage (non-NFS) still allows worktree-per-agent on Docker/Podman/Apple.
	for _, rt := range []string{"docker", "podman", "container"} {
		t.Run("local-storage-allowed/"+rt, func(t *testing.T) {
			srv := newTestServerForStartContextRuntime(t, ServerConfig{
				StorageLayout:           projectfs.Layout{Backend: projectfs.BackendLocal},
				WorkspaceStorageBackend: "local",
			}, rt)
			projectDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(projectDir, ".scion"), 0o755); err != nil {
				t.Fatal(err)
			}
			// Pass GitClone: nil so tryProvisionWorktree skips network git clone while
			// proving the NFS worktree gate does not block local storage.
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:          "local-wt-ok",
				Slug:          "local-wt-ok",
				AgentID:       "agent-local-wt",
				ProjectID:     "proj-local-wt",
				ProjectSlug:   "proj-local-wt",
				ProjectPath:   projectDir,
				Operation:     opCreate,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config:        &CreateAgentConfig{Template: "default"},
			})
			if err != nil {
				t.Fatalf("buildStartContext(local, %s) unexpected error: %v", rt, err)
			}
			if sc == nil {
				t.Fatalf("buildStartContext(local, %s) returned nil startContext", rt)
			}
		})
	}

	// Regression: clone-per-agent and shared-plain remain allowed on Docker/Podman/Apple with NFS.
	for _, rt := range []string{"docker", "podman", "container"} {
		for _, mode := range []string{
			string(store.SharingModeClonePerAgent),
			store.WorkspaceModePerAgent,
			store.WorkspaceModeShared,
			string(store.SharingModeSharedPlain),
		} {
			t.Run("nfs-allowed/"+rt+"/"+mode, func(t *testing.T) {
				srv := newTestServerForStartContextRuntime(t, ServerConfig{
					StorageLayout: projectfs.Layout{
						Backend:     projectfs.BackendNFS,
						HostBase:    filepath.Join(t.TempDir(), "nfs"),
						SubPathRoot: "projects",
					},
					WorkspaceStorageBackend: "nfs",
				}, rt)
				projectDir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(projectDir, ".scion"), 0o755); err != nil {
					t.Fatal(err)
				}
				sc, err := srv.buildStartContext(context.Background(), startContextInputs{
					Name:          "nfs-ok-agent",
					Slug:          "nfs-ok-agent",
					AgentID:       "agent-nfs-ok",
					ProjectID:     "proj-nfs-ok",
					ProjectSlug:   "proj-nfs-ok",
					ProjectPath:   projectDir,
					Operation:     opCreate,
					WorkspaceMode: mode,
					Config: &CreateAgentConfig{
						Template: "default",
						GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
					},
				})
				if err != nil {
					t.Fatalf("buildStartContext(nfs, %s, %q) unexpected error: %v", rt, mode, err)
				}
				if sc == nil {
					t.Fatalf("buildStartContext(nfs, %s, %q) returned nil startContext", rt, mode)
				}
			})
		}
	}
}

func TestCreateAgent_LocalContainerNFSRefusesWorktreePerAgent(t *testing.T) {
	for _, rt := range []string{"docker", "podman", "container"} {
		for _, gatherEnv := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gatherEnv=%t", rt, gatherEnv), func(t *testing.T) {
				srv := newTestServerForStartContextRuntime(t, ServerConfig{
					StorageLayout: projectfs.Layout{
						Backend:     projectfs.BackendNFS,
						HostBase:    filepath.Join(t.TempDir(), "nfs"),
						SubPathRoot: "projects",
					},
					WorkspaceStorageBackend: "nfs",
				}, rt)
				projectDir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(projectDir, ".scion"), 0o755); err != nil {
					t.Fatal(err)
				}

				body, err := json.Marshal(CreateAgentRequest{
					ID:            "agent-http-nfs-wt",
					Name:          "http-nfs-wt",
					Slug:          "http-nfs-wt",
					ProjectID:     "proj-http-nfs-wt",
					ProjectSlug:   "proj-http-nfs-wt",
					ProjectPath:   projectDir,
					WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
					GatherEnv:     gatherEnv,
					Config: &CreateAgentConfig{
						GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()

				srv.createAgent(rec, req)

				if rec.Code != http.StatusPreconditionFailed {
					t.Fatalf("HTTP status = %d, want 412: %s", rec.Code, rec.Body.String())
				}
				var errResp ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("unmarshal ErrorResponse: %v (%s)", err, rec.Body.String())
				}
				if errResp.Error.Code != api.BrokerErrCodeUnsupportedCapability {
					t.Errorf("error.code = %q, want %q", errResp.Error.Code, api.BrokerErrCodeUnsupportedCapability)
				}
				for _, want := range []string{"worktree-per-agent", "nfs", "clone-per-agent", "project_storage"} {
					if !strings.Contains(errResp.Error.Message, want) {
						t.Errorf("error.message %q must mention %q", errResp.Error.Message, want)
					}
				}
				for _, sub := range []string{"workspace", "worktrees", ".git"} {
					if _, statErr := os.Lstat(filepath.Join(projectDir, sub)); !os.IsNotExist(statErr) {
						t.Errorf("host %s created after refusal: %v", sub, statErr)
					}
				}
			})
		}
	}
}

func TestBuildStartContext_MultiProfile_NFSLocalContainerRefused(t *testing.T) {
	srv, dotScion := newTestServerForStartContextMultiProfile(t, ServerConfig{
		StorageLayout: projectfs.Layout{
			Backend:     projectfs.BackendNFS,
			HostBase:    filepath.Join(t.TempDir(), "nfs"),
			SubPathRoot: "projects",
		},
		WorkspaceStorageBackend: "nfs",
	}, "kubernetes", "podman-prof", "podman")
	projectDir := filepath.Dir(dotScion)

	// Podman profile on an NFS broker is refused before worktree provisioning.
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "mp-podman-nfs-wt",
		Slug:          "mp-podman-nfs-wt",
		AgentID:       "agent-mp-podman",
		ProjectID:     "proj-mp-nfs",
		ProjectSlug:   "proj-mp-nfs",
		ProjectPath:   projectDir,
		Operation:     opCreate,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config: &CreateAgentConfig{
			Profile:  "podman-prof",
			Template: "default",
			GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
		},
	})
	sce := requireStartContextError(t, err)
	if sce.Status != http.StatusPreconditionFailed || sce.Code != api.BrokerErrCodeUnsupportedCapability {
		t.Fatalf("Podman NFS profile: got status=%d code=%q, want 412 %q (%v)", sce.Status, sce.Code, api.BrokerErrCodeUnsupportedCapability, err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "workspace")); !os.IsNotExist(statErr) {
		t.Errorf("host workspace/worktree must not be created for Podman NFS profile, stat err = %v", statErr)
	}
}
