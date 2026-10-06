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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// moveCheckingManager is a provision-capturing manager that also confirms a
// moved agent's NFS workspace, returning err.
type moveCheckingManager struct {
	provisionCapturingManager
	err   error
	calls []string
}

func (m *moveCheckingManager) CheckNFSMoveWorkspace(projectPath, projectID, agentName, kind string) (string, error) {
	m.calls = append(m.calls, projectID+"|"+agentName+"|"+kind)
	return "/export/projects/" + projectID + "/agents/" + agentName + "/workspace", m.err
}

func postMoveProvision(t *testing.T, srv *Server, expect string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{
		"name": "moved-agent",
		"id": "agent-uuid-moved",
		"slug": "moved-agent",
		"projectId": "p1",
		"provisionOnly": true,
		"expectExistingNfsWorkspace": %q,
		"config": {"template": "claude"}
	}`, expect)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func newMoveCheckServer(mgr *moveCheckingManager) *Server {
	srv, _ := newTestServerWithProvisionCapture()
	srv.manager = mgr
	return srv
}

// A provision for a moved agent whose workspace is not on this broker's
// export is refused with 409 before anything is provisioned (design A9).
func TestCreateAgentProvisionOnly_MovedWorkspaceMissing_Returns409(t *testing.T) {
	mgr := &moveCheckingManager{err: fmt.Errorf("%w: missing", agent.ErrMoveWorkspaceMissing)}
	srv := newMoveCheckServer(mgr)

	w := postMoveProvision(t, srv, agent.MoveWorkspaceAgentDir)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.provisionCalled || mgr.reprovisionCalled {
		t.Fatal("nothing may be provisioned when the moved workspace is missing")
	}
	if len(mgr.calls) != 1 || mgr.calls[0] != "p1|moved-agent|agent-dir" {
		t.Fatalf("CheckNFSMoveWorkspace calls = %q", mgr.calls)
	}
}

// With the workspace present, the moved agent is provisioned.
func TestCreateAgentProvisionOnly_MovedWorkspacePresent_Provisions(t *testing.T) {
	mgr := &moveCheckingManager{}
	srv := newMoveCheckServer(mgr)

	w := postMoveProvision(t, srv, agent.MoveWorkspaceProject)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.provisionCalled {
		t.Fatal("expected the moved agent to be provisioned")
	}
	if len(mgr.calls) != 1 || mgr.calls[0] != "p1|moved-agent|project" {
		t.Fatalf("CheckNFSMoveWorkspace calls = %q", mgr.calls)
	}
}

// A manager that cannot confirm the workspace fails closed with 409.
func TestCreateAgentProvisionOnly_MovedWorkspaceUnconfirmable_Returns409(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	w := postMoveProvision(t, srv, agent.MoveWorkspaceAgentDir)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.provisionCalled {
		t.Fatal("nothing may be provisioned without confirming the moved workspace")
	}
}
