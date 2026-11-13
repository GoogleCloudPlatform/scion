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
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestBuildStartContext_CloudRunRefusesWorktreePerAgent_NoSideEffects(t *testing.T) {
	for _, op := range []startOperation{opCreate, opHTTPStart, opHTTPRestart} {
		t.Run(string(op), func(t *testing.T) {
			srv := newTestServerForStartContextRuntime(t, ServerConfig{}, cloudRunRuntimeType)
			projectDir := t.TempDir()

			in := startContextInputs{
				Name:        "cr-wt-agent",
				Slug:        "cr-wt-agent",
				AgentID:     "agent-cr-wt",
				ProjectID:   "proj-cr-wt",
				ProjectSlug: "proj-cr-wt",
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
				// Restart carries the mode via ResolvedEnv only; create and start
				// also set WorkspaceMode on the wire.
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
			for _, want := range []string{"worktree-per-agent", "clone-per-agent", "shared-plain"} {
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

	// Regression: clone-per-agent and shared-plain remain allowed on Cloud Run.
	for _, mode := range []string{string(store.SharingModeClonePerAgent), store.WorkspaceModePerAgent, store.WorkspaceModeShared, string(store.SharingModeSharedPlain)} {
		t.Run("allowed/"+mode, func(t *testing.T) {
			srv := newTestServerForStartContextRuntime(t, ServerConfig{}, cloudRunRuntimeType)
			projectDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(projectDir, ".scion"), 0755); err != nil {
				t.Fatal(err)
			}
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:          "cr-ok-agent",
				Slug:          "cr-ok-agent",
				AgentID:       "agent-cr-ok",
				ProjectID:     "proj-cr-ok",
				ProjectSlug:   "proj-cr-ok",
				ProjectPath:   projectDir,
				Operation:     opCreate,
				WorkspaceMode: mode,
				Config: &CreateAgentConfig{
					Template: "default",
					GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
				},
			})
			if err != nil {
				t.Fatalf("buildStartContext(%q) unexpected error: %v", mode, err)
			}
			if sc == nil {
				t.Fatalf("buildStartContext(%q) returned nil startContext", mode)
			}
		})
	}
}

func TestBuildStartContext_MultiProfile_CloudRunRefusedDockerAllowed(t *testing.T) {
	srv, dotScion := newTestServerForStartContextMultiProfile(t, ServerConfig{}, "docker", "cr-prof", cloudRunRuntimeType)
	projectDir := filepath.Dir(dotScion)

	// Cloud Run profile on a Docker-default broker is refused before worktree provisioning.
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "mp-cr-agent",
		Slug:          "mp-cr-agent",
		AgentID:       "agent-mp-cr",
		ProjectID:     "proj-mp",
		ProjectSlug:   "proj-mp",
		ProjectPath:   projectDir,
		Operation:     opCreate,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config: &CreateAgentConfig{
			Profile:  "cr-prof",
			Template: "default",
			GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
		},
	})
	sce := requireStartContextError(t, err)
	if sce.Status != http.StatusPreconditionFailed || sce.Code != api.BrokerErrCodeUnsupportedCapability {
		t.Fatalf("Cloud Run profile: got status=%d code=%q, want 412 %q (%v)", sce.Status, sce.Code, api.BrokerErrCodeUnsupportedCapability, err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "workspace")); !os.IsNotExist(statErr) {
		t.Errorf("host workspace/worktree must not be created for Cloud Run profile, stat err = %v", statErr)
	}

	// Docker profile on the same broker is NOT refused with 412 unsupported_capability
	// (we pass GitClone: nil so tryProvisionWorktree skips network git clone while
	// proving the Cloud Run workspace-mode gate does not block Docker).
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "mp-docker-agent",
		Slug:          "mp-docker-agent",
		AgentID:       "agent-mp-docker",
		ProjectID:     "proj-mp",
		ProjectSlug:   "proj-mp",
		ProjectPath:   projectDir,
		Operation:     opCreate,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{Profile: "local", Template: "default"},
	})
	if err != nil {
		t.Fatalf("Docker profile unexpected error: %v", err)
	}
	if sc == nil {
		t.Fatal("Docker profile expected non-nil startContext")
	}
}

func TestFlatCloudRun_RefusesWorktreePerAgent_NoSideEffects(t *testing.T) {
	for _, op := range e10Ops {
		t.Run("buildStartContext/"+string(op), func(t *testing.T) {
			e := newE10Server(t)
			if op != opCreate {
				e.seedLiveRecord(t)
			}
			recordsBefore := e.records(t)
			homeBefore := e10TreeSnapshot(t, e.home)

			in := e10Inputs(op, nil, map[string]string{
				"SCION_WORKSPACE_MODE": string(store.SharingModeWorktreePerAgent),
				"SCION_WORKSPACE_GIT":  "true",
			}, false)
			if op != opHTTPRestart {
				in.WorkspaceMode = store.WorkspaceModeWorktreePerAgent
			}
			if op == opCreate {
				in.ResolvedEnv = map[string]string{
					"SCION_WORKSPACE_MODE": string(store.SharingModeWorktreePerAgent),
					"SCION_WORKSPACE_GIT":  "true",
				}
			}

			sc, err := e.srv.buildStartContext(context.Background(), in)
			if sc != nil {
				t.Fatal("expected refusal, got startContext")
			}
			sce := requireStartContextError(t, err)
			if sce.Status != http.StatusPreconditionFailed || sce.Code != api.BrokerErrCodeUnsupportedCapability {
				t.Fatalf("got status=%d code=%q, want 412 %q (%v)", sce.Status, sce.Code, api.BrokerErrCodeUnsupportedCapability, err)
			}
			if got := e.records(t); !reflect.DeepEqual(got, recordsBefore) {
				t.Errorf("ownership records changed:\nbefore %+v\nafter  %+v", recordsBefore, got)
			}
			if got := e10TreeSnapshot(t, e.home); !maps.Equal(got, homeBefore) {
				t.Errorf("HOME tree modified (%d paths before, %d after)", len(homeBefore), len(got))
			}
			if _, statErr := os.Lstat(e.projectPath()); !os.IsNotExist(statErr) {
				t.Errorf("fresh project path exists after refusal: %v", statErr)
			}
			if *e.runs != 0 {
				t.Errorf("runtime Run calls = %d, want 0", *e.runs)
			}
		})
	}

	// HTTP createAgent handler (both GatherEnv=false and GatherEnv=true): must
	// refuse with 412 unsupported_capability before ownership recording,
	// worktree provisioning, or 202 env-gather.
	for _, gatherEnv := range []bool{false, true} {
		t.Run(fmt.Sprintf("http-create/gatherEnv=%t", gatherEnv), func(t *testing.T) {
			e := newE10Server(t)
			recordsBefore := e.records(t)

			body, err := json.Marshal(CreateAgentRequest{
				ID:                      e10AgentID,
				Name:                    e10Agent,
				Slug:                    e10Agent,
				ProjectID:               e10ProjectID,
				ProjectSlug:             e10ProjectSlug,
				ExpectedRuntimeTargetID: "flat-cr-target",
				WorkspaceMode:           store.WorkspaceModeWorktreePerAgent,
				GatherEnv:               gatherEnv,
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

			e.srv.createAgent(rec, req)

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
			for _, want := range []string{"worktree-per-agent", "clone-per-agent", "shared-plain"} {
				if !strings.Contains(errResp.Error.Message, want) {
					t.Errorf("error.message %q must mention %q", errResp.Error.Message, want)
				}
			}
			if got := e.records(t); !reflect.DeepEqual(got, recordsBefore) {
				t.Errorf("ownership records changed: %+v -> %+v", recordsBefore, got)
			}
			for _, sub := range []string{"workspace", "worktrees", ".git"} {
				if _, statErr := os.Lstat(filepath.Join(e.projectPath(), sub)); !os.IsNotExist(statErr) {
					t.Errorf("host %s created after refusal: %v", sub, statErr)
				}
			}
			if *e.runs != 0 {
				t.Errorf("runtime Run calls = %d, want 0", *e.runs)
			}
		})
	}
}

type capturedLogBuffer struct {
	mu   sync.Mutex
	msgs []string
}

func (b *capturedLogBuffer) writer() *slog.Logger {
	return slog.New(&sliceLogHandler{buf: b})
}

func (b *capturedLogBuffer) contains(sub string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

type sliceLogHandler struct {
	buf *capturedLogBuffer
}

func (h *sliceLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *sliceLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.buf.bufLockAppend(r.Message)
	return nil
}
func (h *sliceLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *sliceLogHandler) WithGroup(string) slog.Handler      { return h }

func (b *capturedLogBuffer) bufLockAppend(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, msg)
}

// Regression test for ptone/scion#7877 R1 (ptone/scion#1799 ordering invariant):
// both createAgent and buildStartContext must verify the workspace .scion
// record BEFORE resolving runtime/profile settings through .scion. A tampered
// or unrecorded .scion entry must return the 409 workspace-config conflict,
// never read settings.yaml through the unverified entry, and never return 412.
func TestWorktreePerAgent_WorkspaceConfigRecordCheckPrecedesCloudRunRefusal(t *testing.T) {
	t.Run("tampered recorded .scion marker returns 409 without reading settings or returning 412", func(t *testing.T) {
		for _, settingsBody := range []struct {
			name string
			yaml string
		}{
			{
				name: "settings-pointing-to-cloudrun",
				yaml: "schema_version: \"1\"\nactive_profile: cr\nprofiles:\n  cr:\n    runtime: cr\nruntimes:\n  cr:\n    type: cloudrun\n",
			},
			{
				name: "corrupt-settings-detecting-read",
				yaml: ":\n\tinvalid_yaml_if_read: [\n",
			},
		} {
			t.Run(settingsBody.name, func(t *testing.T) {
				// Use a multi-profile server (ForceRuntime == "") so resolveRuntimeNameForOpts
				// genuinely reads project settings whenever reached, and create hubWorkspace
				// AFTER server setup so its HOME is the active HOME.
				srv, _ := newTestServerForStartContextMultiProfile(t, ServerConfig{}, "docker", "cr", cloudRunRuntimeType)
				ws := hubWorkspace(t)
				entryMarker(t, ws)
				if err := recordWorkspaceConfigState(ws, recordHubID); err != nil {
					t.Fatal(err)
				}
				// Tamper: replace the recorded .scion marker with a .scion directory
				// holding settings.yaml.
				entryDir(recordHubID)(t, ws)
				if err := os.WriteFile(filepath.Join(ws, ".scion", "settings.yaml"), []byte(settingsBody.yaml), 0o600); err != nil {
					t.Fatal(err)
				}

				logs := &capturedLogBuffer{}
				srv.agentLifecycleLog = logs.writer()

				for _, gatherEnv := range []bool{false, true} {
					body, err := json.Marshal(CreateAgentRequest{
						ID:            "id-tampered",
						Name:          "tampered-agent",
						Slug:          "tampered-agent",
						ProjectID:     recordHubID,
						ProjectSlug:   "proj",
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

					if rec.Code != http.StatusConflict {
						t.Fatalf("createAgent(gatherEnv=%t): got status %d, want 409: %s", gatherEnv, rec.Code, rec.Body.String())
					}
					if !strings.Contains(rec.Body.String(), "not in the state the Runtime Broker recorded") {
						t.Errorf("createAgent(gatherEnv=%t): unexpected 409 body: %s", gatherEnv, rec.Body.String())
					}
					if strings.Contains(rec.Body.String(), api.BrokerErrCodeUnsupportedCapability) {
						t.Errorf("createAgent(gatherEnv=%t): returned unsupported_capability instead of workspace-config 409: %s", gatherEnv, rec.Body.String())
					}
				}

				for _, op := range []startOperation{opCreate, opHTTPStart, opHTTPRestart} {
					sc, err := srv.buildStartContext(context.Background(), startContextInputs{
						Name:          "tampered-agent",
						Slug:          "tampered-agent",
						AgentID:       "id-tampered",
						ProjectID:     recordHubID,
						ProjectSlug:   "proj",
						ProjectPath:   ws,
						Operation:     op,
						WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
						ResolvedEnv: map[string]string{
							"SCION_WORKSPACE_MODE": string(store.SharingModeWorktreePerAgent),
						},
						Config: &CreateAgentConfig{
							GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
						},
					})
					if sc != nil {
						t.Fatalf("buildStartContext(%s): expected error, got startContext", op)
					}
					sce := requireStartContextError(t, err)
					if sce.Status != http.StatusConflict {
						t.Fatalf("buildStartContext(%s): status = %d, want 409 (%v)", op, sce.Status, err)
					}
					if !strings.Contains(sce.Message, "not in the state the Runtime Broker recorded") {
						t.Errorf("buildStartContext(%s): message = %q, want workspace-config conflict", op, sce.Message)
					}
				}

				if logs.contains("failed to load project settings") {
					t.Errorf("project settings were read through tampered .scion before record check: logs=%v", logs.msgs)
				}
			})
		}
	})

	t.Run("shared storage layout tampered or unseeded .scion returns 409 without reading settings or returning 412", func(t *testing.T) {
		f := newShareVisibleFixture(t, "docker", "10.0.0.2:/scion-workspaces")
		f.srv.config.ForceRuntime = "" // ensure resolveRuntimeNameForOpts reads .scion/settings.yaml if reached
		_ = hubWorkspace(t)            // isolate HOME under t.TempDir() for workspace-config records
		logs := &capturedLogBuffer{}
		f.srv.agentLifecycleLog = logs.writer()

		// Case A: record state as marker while valid shared seed is on disk so
		// sharedHubProjectPath passes and withCheckedWorkspaceConfig catches the
		// recorded-vs-found mismatch before settings.yaml is read.
		projectRoot := filepath.Dir(f.scionDir)
		canonical, recordPath, ok, err := workspaceConfigRecordFor(projectRoot, sharedRootProjectID)
		if err != nil || !ok {
			t.Fatalf("workspaceConfigRecordFor: ok=%v err=%v", ok, err)
		}
		if err := writeWorkspaceConfigRecord(canonical, recordPath, sharedRootProjectID, workspaceConfigState{Kind: workspaceConfigMarker, Detail: "recorded-digest"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.scionDir, "settings.yaml"), []byte(":\n\tinvalid_yaml_if_read: [\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		for _, gatherEnv := range []bool{false, true} {
			body, err := json.Marshal(CreateAgentRequest{
				ID:            "id-shared-tampered",
				Name:          "shared-tampered-agent",
				Slug:          "shared-tampered-agent",
				ProjectID:     sharedRootProjectID,
				ProjectSlug:   "web",
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
			f.srv.createAgent(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("shared tampered createAgent(gatherEnv=%t): got status %d, want 409: %s", gatherEnv, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "not in the state the Runtime Broker recorded") {
				t.Errorf("shared tampered createAgent(gatherEnv=%t): unexpected 409 body: %s", gatherEnv, rec.Body.String())
			}
		}

		// Case B: unrecorded .scion symlink on shared storage layout (previously
		// candidateCreateProjectPath handed this path straight to
		// resolveRuntimeNameForOpts before sharedHubProjectPath checked it).
		_ = os.Remove(recordPath)
		entrySymlink("redirected-cfg")(t, projectRoot)
		if err := os.WriteFile(filepath.Join(projectRoot, "redirected-cfg", "settings.yaml"), []byte(":\n\tinvalid_yaml_if_read: [\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		for _, gatherEnv := range []bool{false, true} {
			body, err := json.Marshal(CreateAgentRequest{
				ID:            "id-shared-unrec",
				Name:          "shared-unrec-agent",
				Slug:          "shared-unrec-agent",
				ProjectID:     sharedRootProjectID,
				ProjectSlug:   "web",
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
			f.srv.createAgent(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("shared unseeded createAgent(gatherEnv=%t): got status %d, want 409: %s", gatherEnv, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), api.BrokerErrCodeUnsupportedCapability) {
				t.Errorf("shared unseeded createAgent(gatherEnv=%t): returned 412 instead of 409: %s", gatherEnv, rec.Body.String())
			}
		}

		if logs.contains("failed to load project settings") {
			t.Errorf("project settings were read through tampered/unseeded shared .scion before check: logs=%v", logs.msgs)
		}
	})

	t.Run("unrecorded shared-workspace identity mismatch returns 409 without reading settings or returning 412", func(t *testing.T) {
		srv, _ := newTestServerForStartContextMultiProfile(t, ServerConfig{}, "docker", "cr", cloudRunRuntimeType)
		ws := hubWorkspace(t)
		// Unrecorded .scion directory naming a different project ID and holding
		// both a cloudrun active_profile and invalid YAML.
		otherID := "77777777-7777-7777-7777-777777777777"
		entryDir(otherID)(t, ws)
		if err := os.WriteFile(filepath.Join(ws, ".scion", "settings.yaml"), []byte(":\n\tinvalid_yaml_if_read: [\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		logs := &capturedLogBuffer{}
		srv.agentLifecycleLog = logs.writer()

		for _, gatherEnv := range []bool{false, true} {
			body, err := json.Marshal(CreateAgentRequest{
				ID:                 "id-unrec-relink",
				Name:               "unrec-relink-agent",
				Slug:               "unrec-relink-agent",
				ProjectID:          recordHubID,
				ProjectPath:        ws,
				PreviousProjectIDs: []string{otherID},
				WorkspaceMode:      store.WorkspaceModeWorktreePerAgent,
				GatherEnv:          gatherEnv,
				Config: &CreateAgentConfig{
					SharedWorkspace: true,
					GitClone:        &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.createAgent(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("unrecorded relink createAgent(gatherEnv=%t): got status %d, want 409: %s", gatherEnv, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), api.BrokerErrCodeUnsupportedCapability) {
				t.Errorf("unrecorded relink createAgent(gatherEnv=%t): returned 412 instead of 409: %s", gatherEnv, rec.Body.String())
			}
		}

		if logs.contains("failed to load project settings") {
			t.Errorf("project settings were read through unrecorded .scion before identity check: logs=%v", logs.msgs)
		}
	})
}
