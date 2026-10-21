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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// freshMoveManager is a provision-capturing manager that also answers the
// forced move's absence check with err, reporting freshMovePaths, and
// records any call to the NFS move check.
type freshMoveManager struct {
	provisionCapturingManager
	err         error
	absentCalls []api.StartOptions
	nfsCalls    int
}

var freshMovePaths = []string{"/srv/scion/projects/p1/agents/moved-agent", "/export/projects/p1/agents/moved-agent"}

func (m *freshMoveManager) CheckMoveWorkspaceAbsent(_ context.Context, opts api.StartOptions) ([]string, error) {
	m.absentCalls = append(m.absentCalls, opts)
	return freshMovePaths, m.err
}

func (m *freshMoveManager) CheckNFSMoveWorkspace(string, string, string, string) (string, error) {
	m.nfsCalls++
	return "", nil
}

func newFreshMoveServer(mgr *freshMoveManager) (*Server, *bytes.Buffer) {
	srv, _ := newTestServerWithProvisionCapture()
	srv.manager = mgr
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logBuf, nil))
	return srv, &logBuf
}

// A forced move's provision is refused with 409 when the broker already has
// files for the agent, or cannot confirm it has none. Nothing is
// provisioned, the body names no path (the paths are logged on the broker
// only), and the NFS move check is never used for this expectation.
func TestCreateAgentProvisionOnly_FreshAgentDir_Refused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantBody string
	}{
		{"files present", fmt.Errorf("%w: %s exists", agent.ErrMoveWorkspacePresent, freshMovePaths[0]),
			"Failed to provision moved agent: the agent already has files on this broker; refusing to recreate over them"},
		{"unconfirmed", fmt.Errorf("%w: stat %s: permission denied", agent.ErrMoveWorkspaceUnconfirmed, freshMovePaths[1]),
			"Failed to provision moved agent: could not confirm that the agent has no files on this broker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &freshMoveManager{err: tc.err}
			srv, logBuf := newFreshMoveServer(mgr)

			w := postFreshMoveProvision(t, srv, "reincarnation-1")
			if w.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
			}
			if mgr.provisionCalled || mgr.reprovisionCalled {
				t.Fatal("nothing may be provisioned over the agent's existing files")
			}
			if len(mgr.absentCalls) != 1 || mgr.absentCalls[0].Name != "moved-agent" {
				t.Fatalf("CheckMoveWorkspaceAbsent calls = %+v", mgr.absentCalls)
			}
			if mgr.nfsCalls != 0 {
				t.Fatalf("CheckNFSMoveWorkspace called %d times for a forced move", mgr.nfsCalls)
			}
			body := w.Body.String()
			if !strings.Contains(body, `"code":"`+api.BrokerErrCodeMoveTargetNotFresh+`"`) {
				t.Fatalf("body = %s, want error code %s", body, api.BrokerErrCodeMoveTargetNotFresh)
			}
			if !strings.Contains(body, tc.wantBody) {
				t.Fatalf("body = %s, want it to contain %q", body, tc.wantBody)
			}
			for _, p := range append([]string{"/srv/", "/export/"}, freshMovePaths...) {
				if strings.Contains(body, p) {
					t.Fatalf("body leaks the broker path %q: %s", p, body)
				}
			}
			if !strings.Contains(logBuf.String(), freshMovePaths[0]) || !strings.Contains(logBuf.String(), freshMovePaths[1]) {
				t.Fatalf("the broker log must name the checked paths: %s", logBuf.String())
			}
		})
	}
}

// With nothing for the agent on the broker, the forced move's agent is
// provisioned.
func TestCreateAgentProvisionOnly_FreshAgentDir_Provisions(t *testing.T) {
	mgr := &freshMoveManager{}
	srv, _ := newFreshMoveServer(mgr)

	w := postFreshMoveProvision(t, srv, "reincarnation-1")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.provisionCalled {
		t.Fatal("expected the moved agent to be provisioned")
	}
	if len(mgr.absentCalls) != 1 || mgr.nfsCalls != 0 {
		t.Fatalf("absence checks = %d, NFS checks = %d; want 1 and 0", len(mgr.absentCalls), mgr.nfsCalls)
	}
}

// A manager that cannot run the absence check fails closed with 409.
func TestCreateAgentProvisionOnly_FreshAgentDir_Unsupported_Returns409(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	w := postFreshMoveProvision(t, srv, "reincarnation-1")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.provisionCalled {
		t.Fatal("nothing may be provisioned without confirming the agent has no files here")
	}
	if !strings.Contains(w.Body.String(), api.BrokerErrCodeMoveTargetNotFresh) {
		t.Fatalf("body = %s, want error code %s", w.Body.String(), api.BrokerErrCodeMoveTargetNotFresh)
	}
}

// The broker advertises that it can be the target of a forced move, in
// /api/v1/info and in its heartbeat (the static set it also registers).
func TestBrokerCapabilities_AdvertiseAgentMoveRecreate(t *testing.T) {
	srv := newTestServer(t)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/info", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp BrokerInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Capabilities == nil || !resp.Capabilities.AgentMoveRecreate {
		t.Fatalf("/api/v1/info capabilities = %+v, want agentMoveRecreate", resp.Capabilities)
	}
	if !StaticCapabilities(nil, "").AgentMoveRecreate {
		t.Fatal("StaticCapabilities must advertise AgentMoveRecreate")
	}
}

// A forced move's rollback delete, as the hub sends it when the target
// recorded no run (deleteFiles, never the branch, project-scoped, no run
// ID), removes the provisioned-but-never-started agent's own directory by
// name within the project, and leaves a sibling agent's directory and the
// project's files in place.
func TestDeleteAgent_ForcedMoveRollbackWithoutRun_LeavesSibling(t *testing.T) {
	var entries []api.AgentInfo
	var deleted []string
	srv, home := newProvisionDirsServer(t, &entries, &deleted)
	_, agentDir := makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "dev")
	_, siblingDir := makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "sib")
	projectFile := filepath.Join(home, ".scion", "projects", "proj-b", "README.md")
	if err := os.WriteFile(projectFile, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", "deleteFiles=true&removeBranch=false&projectId="+scopeProjB)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("agent directory %s not removed (stat err %v)", agentDir, err)
	}
	for _, p := range []string{filepath.Join(siblingDir, "scion-agent.json"), projectFile} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}

// postFreshMoveProvision posts a forced move's provision (the fresh-agent-dir
// expectation) with the move's reincarnation ID moveID ("" for none).
func postFreshMoveProvision(t *testing.T, srv *Server, moveID string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{
		"name": "moved-agent",
		"id": "agent-uuid-moved",
		"slug": "moved-agent",
		"projectId": "p1",
		"provisionOnly": true,
		"expectExistingNfsWorkspace": %q,
		"moveReincarnationId": %q,
		"config": {"template": "claude"}
	}`, agent.MoveWorkspaceFreshAgentDir, moveID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// A forced move's provision needs its reincarnation ID, which reaches the
// manager; an ID without the fresh-agent-dir expectation is refused too.
func TestCreateAgentProvisionOnly_FreshAgentDir_ReincarnationID(t *testing.T) {
	t.Run("passed to the provision", func(t *testing.T) {
		mgr := &freshMoveManager{}
		srv, _ := newFreshMoveServer(mgr)
		w := postFreshMoveProvision(t, srv, "reincarnation-1")
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
		if mgr.lastOpts.MoveReincarnationID != "reincarnation-1" {
			t.Fatalf("provision MoveReincarnationID = %q, want reincarnation-1", mgr.lastOpts.MoveReincarnationID)
		}
	})
	t.Run("missing", func(t *testing.T) {
		mgr := &freshMoveManager{}
		srv, _ := newFreshMoveServer(mgr)
		w := postFreshMoveProvision(t, srv, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if mgr.provisionCalled || len(mgr.absentCalls) != 0 {
			t.Fatal("nothing may run without the reincarnation ID")
		}
	})
	t.Run("without the fresh-agent-dir expectation", func(t *testing.T) {
		mgr := &freshMoveManager{}
		srv, _ := newFreshMoveServer(mgr)
		body := `{"name":"moved-agent","id":"agent-uuid-moved","slug":"moved-agent","projectId":"p1","provisionOnly":true,"moveReincarnationId":"reincarnation-1","config":{"template":"claude"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if mgr.provisionCalled {
			t.Fatal("nothing may be provisioned")
		}
	})
}

// A forced move's rollback delete, through the real delete handler and
// manager, removes the agent's directory only when it carries that move's
// marker. An unmarked directory or another move's marker is refused with
// 409 move_marker_mismatch, a path-free body, and nothing removed: not the
// files, and not the agent's runtime entry (seeded in those rows). An agent
// with no files is a plain no-op (404). A sibling agent is never touched.
func TestDeleteAgent_ForcedMoveRollbackNeedsMoveMarker(t *testing.T) {
	const moveID = "reincarnation-1"
	for _, tc := range []struct {
		name      string
		marker    string // "" = no marker
		noFiles   bool
		container bool // seed a runtime entry for the agent
		wantCode  int
		removed   bool
	}{
		{name: "marked by this move", marker: moveID, wantCode: http.StatusNoContent, removed: true},
		{name: "unmarked", wantCode: http.StatusConflict, container: true},
		{name: "marked by another move", marker: "reincarnation-0", wantCode: http.StatusConflict, container: true},
		{name: "no files", noFiles: true, wantCode: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []api.AgentInfo
			var deleted []string
			srv, home := newProvisionDirsServer(t, &entries, &deleted)
			var agentDir, extDir string
			if !tc.noFiles {
				extDir, agentDir = makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "dev")
			}
			if tc.container {
				entries = []api.AgentInfo{{
					Name: "dev", ContainerID: "cid-b", ProjectID: scopeProjB, ProjectPath: extDir,
					Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project_id": scopeProjB},
				}}
			}
			_, siblingDir := makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "sib")
			if tc.marker != "" {
				if err := os.WriteFile(filepath.Join(agentDir, agent.MoveMarkerFile), []byte(tc.marker+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			rec := doDelete(t, srv, "dev", "deleteFiles=true&removeBranch=false&projectId="+scopeProjB+"&"+api.MoveReincarnationIDQueryParam+"="+moveID)
			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			if tc.wantCode == http.StatusConflict {
				if len(deleted) != 0 {
					t.Fatalf("runtime entries deleted on a refused rollback delete: %v", deleted)
				}
				if !strings.Contains(rec.Body.String(), `"code":"`+api.BrokerErrCodeMoveMarkerMismatch+`"`) {
					t.Fatalf("body = %s, want code %s", rec.Body.String(), api.BrokerErrCodeMoveMarkerMismatch)
				}
				if strings.Contains(rec.Body.String(), home) || strings.Contains(rec.Body.String(), "/") {
					t.Fatalf("body names a path: %s", rec.Body.String())
				}
			}
			if agentDir != "" {
				_, err := os.Stat(agentDir)
				if tc.removed && !os.IsNotExist(err) {
					t.Errorf("agent directory %s not removed (stat err %v)", agentDir, err)
				}
				if !tc.removed && err != nil {
					t.Errorf("agent directory %s removed: %v", agentDir, err)
				}
			}
			if _, err := os.Stat(filepath.Join(siblingDir, "scion-agent.json")); err != nil {
				t.Errorf("sibling agent touched: %v", err)
			}
		})
	}
}

// A forced move's rollback delete must be a project-scoped files delete.
func TestDeleteAgent_MoveReincarnationIDValidation(t *testing.T) {
	var entries []api.AgentInfo
	var deleted []string
	srv, _ := newProvisionDirsServer(t, &entries, &deleted)
	for _, q := range []string{
		"deleteFiles=false&projectId=" + scopeProjB,
		"deleteFiles=true&localOnly=true&projectId=" + scopeProjB,
		"deleteFiles=true",
	} {
		rec := doDelete(t, srv, "dev", q+"&"+api.MoveReincarnationIDQueryParam+"=reincarnation-1")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", q, rec.Code, rec.Body.String())
		}
	}
}

// A forced move's reincarnation ID on a create that is not provision-only
// is refused (400) before anything runs.
func TestCreateAgent_MoveReincarnationIDOnFullCreate_Returns400(t *testing.T) {
	mgr := &freshMoveManager{}
	srv, _ := newFreshMoveServer(mgr)
	body := `{"name":"moved-agent","id":"agent-uuid-moved","slug":"moved-agent","projectId":"p1","moveReincarnationId":"reincarnation-1","config":{"template":"claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.provisionCalled || mgr.startCalled {
		t.Fatal("nothing may be provisioned or started")
	}
}
