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

			w := postMoveProvision(t, srv, agent.MoveWorkspaceFreshAgentDir)
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

	w := postMoveProvision(t, srv, agent.MoveWorkspaceFreshAgentDir)
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
	w := postMoveProvision(t, srv, agent.MoveWorkspaceFreshAgentDir)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.provisionCalled {
		t.Fatal("nothing may be provisioned without confirming the agent has no files here")
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
