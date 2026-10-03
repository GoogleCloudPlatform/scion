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
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests that an agent delete removes per-agent runtime objects when the
// agent's container was already removed outside scion, and only then.

type cleanupCall struct{ agentName, projectID string }

// cleanupRecordingManager is a filteringMockManager whose runtime can
// remove leftover per-agent objects; it records each request.
type cleanupRecordingManager struct {
	filteringMockManager
	mu         sync.Mutex
	calls      []cleanupCall
	cleanupErr error
}

func (m *cleanupRecordingManager) CleanupAgentResources(_ context.Context, agentName, projectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, cleanupCall{agentName, projectID})
	return m.cleanupErr
}

func (m *cleanupRecordingManager) cleanupCalls() []cleanupCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cleanupCall(nil), m.calls...)
}

var _ agentResourceCleaner = (*cleanupRecordingManager)(nil)
var _ agentResourceCleaner = (*agent.AgentManager)(nil)

func newCleanupTestServer(t *testing.T, mgr agent.Manager) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	return New(cfg, mgr, rt), home
}

func assertCleanupCalls(t *testing.T, got []cleanupCall, want ...cleanupCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("cleanup calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cleanup call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDeleteAgent_ContainerGone_FilesPresent_CleansLeftoverObjects(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, home := newCleanupTestServer(t, mgr)
	makeHubProject(t, home, "proj-b", scopeProjB, "dev")

	// The container may have run on an auxiliary runtime; it is asked too.
	aux := &cleanupRecordingManager{}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["k8s-aux"] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }},
		Manager: aux,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "" {
		t.Fatalf("file-only delete must not target a container, got %q", mgr.LastDeleteContainerID())
	}
	assertCleanupCalls(t, mgr.cleanupCalls(), cleanupCall{"dev", scopeProjB})
	assertCleanupCalls(t, aux.cleanupCalls(), cleanupCall{"dev", scopeProjB})
}

func TestDeleteAgent_ContainerAndFilesGone_CleansLeftoverObjects(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, _ := newCleanupTestServer(t, mgr)

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete call, got %d", mgr.DeleteCalls())
	}
	assertCleanupCalls(t, mgr.cleanupCalls(), cleanupCall{"dev", scopeProjB})
}

func TestDeleteAgent_ContainerPresent_NoLeftoverCleanup(t *testing.T) {
	// The runtime's own Delete removes the objects with the container.
	mgr := &cleanupRecordingManager{}
	srv, _ := newCleanupTestServer(t, mgr)
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, "")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-b" {
		t.Errorf("deleted container %q, want cid-b", mgr.LastDeleteContainerID())
	}
	assertCleanupCalls(t, mgr.cleanupCalls())
}

func TestDeleteAgent_NoProjectID_NoLeftoverCleanup(t *testing.T) {
	// Without a project there is no safe label scope.
	mgr := &cleanupRecordingManager{}
	srv, _ := newCleanupTestServer(t, mgr)

	rec := doDelete(t, srv, "dev", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	assertCleanupCalls(t, mgr.cleanupCalls())
}

func TestDeleteAgent_LeftoverCleanupFailure_ResponseUnchanged(t *testing.T) {
	// The cleanup is best effort: a failure is logged and does not change
	// the delete's response.
	t.Run("file-only 204", func(t *testing.T) {
		mgr := &cleanupRecordingManager{cleanupErr: errors.New("list failed")}
		srv, home := newCleanupTestServer(t, mgr)
		makeHubProject(t, home, "proj-b", scopeProjB, "dev")

		rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
		}
		if mgr.DeleteCalls() != 1 {
			t.Errorf("expected the file delete to run, got %d calls", mgr.DeleteCalls())
		}
		assertCleanupCalls(t, mgr.cleanupCalls(), cleanupCall{"dev", scopeProjB})
	})
	t.Run("not found 404", func(t *testing.T) {
		mgr := &cleanupRecordingManager{cleanupErr: errors.New("list failed")}
		srv, _ := newCleanupTestServer(t, mgr)

		rec := doDelete(t, srv, "dev", "projectId="+scopeProjB)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
		assertCleanupCalls(t, mgr.cleanupCalls(), cleanupCall{"dev", scopeProjB})
	})
}

// TestDeleteAgent_RecordedRuntime_CleansOnlyThatRuntime pins that a delete
// carrying a recorded runtime type (ptone/scion#2748) removes leftover
// objects only from runtimes of that type.
func TestDeleteAgent_RecordedRuntime_CleansOnlyThatRuntime(t *testing.T) {
	for _, tc := range []struct {
		recorded             string
		wantDefault, wantK8s bool
	}{
		{"kubernetes", false, true},
		{"docker", true, false},
		{"", true, true},
	} {
		t.Run("runtime="+tc.recorded, func(t *testing.T) {
			mgr := &cleanupRecordingManager{}
			srv, home := newCleanupTestServer(t, mgr)
			srv.runtime = &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
			makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			aux := &cleanupRecordingManager{}
			srv.auxiliaryRuntimesMu.Lock()
			srv.auxiliaryRuntimes["k8s-aux"] = auxiliaryRuntime{
				Runtime: &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }},
				Manager: aux,
			}
			srv.auxiliaryRuntimesMu.Unlock()

			query := "projectId=" + scopeProjB + "&deleteFiles=true"
			if tc.recorded != "" {
				query += "&" + api.RecordedRuntimeQueryParam + "=" + tc.recorded
			}
			rec := doDelete(t, srv, "dev", query)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			want := []cleanupCall{{"dev", scopeProjB}}
			if tc.wantDefault {
				assertCleanupCalls(t, mgr.cleanupCalls(), want...)
			} else {
				assertCleanupCalls(t, mgr.cleanupCalls())
			}
			if tc.wantK8s {
				assertCleanupCalls(t, aux.cleanupCalls(), want...)
			} else {
				assertCleanupCalls(t, aux.cleanupCalls())
			}
		})
	}
}
