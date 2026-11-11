// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#5590: a forced move's rollback delete checks the
// move markers with the same sharer key the delete uses, so on a
// hub-managed broker a worktree or sharer registry entry provisioned under
// the hub agent ID refuses the delete, as one under the name does.

// moveSharerManager is sharerFilesManager with the real move-marker check
// (agent.AgentManager.CheckMoveMarkers), so the rollback delete runs the
// check and then the ID-keyed file removal.
type moveSharerManager struct {
	sharerFilesManager
	checker *agent.AgentManager
}

func (m *moveSharerManager) CheckMoveMarkers(ctx context.Context, projectPath, projectID, hubProjectID, agentName, sharerKey, moveID string) ([]string, error) {
	return m.checker.CheckMoveMarkers(ctx, projectPath, projectID, hubProjectID, agentName, sharerKey, moveID)
}

const sharerMoveID = "reincarnation-5590"

// newMoveSharerFixture is newSharerFixture with a moveSharerManager behind
// the server, the broker's lifecycle log in logs, and agent dev-a's
// directories marked by sharerMoveID, as a forced move's provision leaves
// them (the project agent directory and, when it differs, its home's
// parent under the external agents root).
func newMoveSharerFixture(t *testing.T) (*sharerFixture, *moveSharerManager, *bytes.Buffer) {
	t.Helper()
	f := newSharerFixture(t)
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	mgr := &moveSharerManager{checker: agent.NewManager(rt).(*agent.AgentManager)}
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	f.srv = New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	logs := &bytes.Buffer{}
	f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))
	for _, dir := range []string{
		filepath.Join(f.scionDir, "agents", "dev-a"),
		filepath.Dir(config.GetAgentHomePath(f.scionDir, "dev-a")),
	} {
		if err := os.WriteFile(filepath.Join(dir, agent.MoveMarkerFile), []byte(sharerMoveID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f, mgr, logs
}

const sharerMoveDeleteFlags = "&deleteFiles=true&removeBranch=false&" + api.MoveReincarnationIDQueryParam + "=" + sharerMoveID

// A worktree and sharer registry entry provisioned under the hub agent ID
// predate the move: the rollback delete is refused with
// move_marker_mismatch and removes nothing, though every agent directory
// carries the move's marker. The check reports the registration first (it
// is read before the worktree is looked at); the worktree-only case is
// covered by TestCheckMoveMarkers_SharerKey in pkg/agent.
func TestDeleteAgent_ForcedMoveRollback_HubAgentIDSharerStateRefused(t *testing.T) {
	f, mgr, logs := newMoveSharerFixture(t)
	wt := f.provision(t, "uuid-a", "shared")
	before := readFileBytes(t, f.marker("shared"))

	rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId=uuid-a"+sharerMoveDeleteFlags)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 for the ID-keyed sharer state refused: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"`+api.BrokerErrCodeMoveMarkerMismatch+`"`) {
		t.Fatalf("body = %s, want code %s", rec.Body.String(), api.BrokerErrCodeMoveMarkerMismatch)
	}
	// The refusal comes from the real check finding the ID-keyed sharer
	// state, not from the handler's fallbacks.
	log := logs.String()
	if !strings.Contains(log, "registered on a shared worktree") {
		t.Errorf("the refusal does not name the ID-keyed registration; log:\n%s", log)
	}
	for _, fallback := range []string{"cannot check move markers", "no project path"} {
		if strings.Contains(log, fallback) {
			t.Errorf("the refusal came from the handler's fallback %q, not the check; log:\n%s", fallback, log)
		}
	}
	if n := mgr.DeleteCalls(); n != 0 {
		t.Errorf("DeleteTarget called %d times, want 0", n)
	}
	if after := readFileBytes(t, f.marker("shared")); !bytes.Equal(before, after) {
		t.Errorf("registry file changed: %s -> %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("worktree removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.scionDir, "agents", "dev-a", agent.MoveMarkerFile)); err != nil {
		t.Errorf("agent directory removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(config.GetAgentHomePath(f.scionDir, "dev-a")), agent.MoveMarkerFile)); err != nil {
		t.Errorf("agent home's parent directory removed: %v", err)
	}
}

// Positive control: the same marked agent with no worktree or registry
// entry (all the move created) is removed by the rollback delete.
func TestDeleteAgent_ForcedMoveRollback_HubAgentIDMarkedDirRemoved(t *testing.T) {
	f, mgr, _ := newMoveSharerFixture(t)

	rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId=uuid-a"+sharerMoveDeleteFlags)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if got := mgr.LastDeleteSharerKey(); got != "uuid-a" {
		t.Errorf("sharer key %q, want uuid-a", got)
	}
	if _, err := os.Stat(filepath.Join(f.scionDir, "agents", "dev-a")); !os.IsNotExist(err) {
		t.Errorf("the move's agent directory was not removed (stat err %v)", err)
	}
}
