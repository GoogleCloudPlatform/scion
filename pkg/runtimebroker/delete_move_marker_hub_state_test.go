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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for ptone/scion#5955: a forced move's rollback delete also removes
// the agent's hub-project state directory (keyed by the agent name and the
// Hub project ID the delete uses), so the move-marker check covers it,
// with the same project ID.

const hubStateMoveID = "reincarnation-5955"

// newHubStateMoveFixture is a plain project (no project-id file, so no
// external agents root) whose agent dev runs in a container labelled with
// scopeProjB, its agent directory marked by hubStateMoveID, and the
// agent's hub-project state directory for scopeProjB created with a file
// in it. It returns the server, the runtime entries deleted, the agent
// directory and the hub-project state directory.
func newHubStateMoveFixture(t *testing.T) (*Server, *[]string, string, string) {
	t.Helper()
	var entries []api.AgentInfo
	deleted := &[]string{}
	srv, home := newProvisionDirsServer(t, &entries, deleted)
	scionDir := filepath.Join(home, "plainproj", config.DotScion)
	agentDir := filepath.Join(scionDir, "agents", "dev")
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, agent.MoveMarkerFile), []byte(hubStateMoveID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectDir, err := config.GetResolvedProjectDir(scionDir)
	if err != nil {
		t.Fatalf("resolve the project: %v", err)
	}
	hubDir, err := config.AgentDirForProject(projectDir, "dev", true, scopeProjB)
	if err != nil {
		t.Fatalf("hub-project state dir: %v", err)
	}
	if hubDir == agentDir || strings.HasPrefix(hubDir, scionDir+string(filepath.Separator)) {
		t.Fatalf("setup: the hub-project state dir %s is not a directory of its own", hubDir)
	}
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries = []api.AgentInfo{{
		Name: "dev", ContainerID: "cid-dev", ProjectID: scopeProjB, ProjectPath: scionDir,
		Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project_id": scopeProjB},
	}}
	return srv, deleted, agentDir, hubDir
}

const hubStateMoveDeleteQuery = "deleteFiles=true&removeBranch=false&projectId=" + scopeProjB + "&" + api.MoveReincarnationIDQueryParam + "=" + hubStateMoveID

// The hub-project state directory exists without the move's marker: the
// rollback delete is refused with move_marker_mismatch and removes nothing.
func TestDeleteAgent_ForcedMoveRollback_UnmarkedHubStateDirRefused(t *testing.T) {
	srv, deleted, agentDir, hubDir := newHubStateMoveFixture(t)

	rec := doDelete(t, srv, "dev", hubStateMoveDeleteQuery)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 for the unmarked hub-project state dir: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"`+api.BrokerErrCodeMoveMarkerMismatch+`"`) {
		t.Fatalf("body = %s, want code %s", rec.Body.String(), api.BrokerErrCodeMoveMarkerMismatch)
	}
	if len(*deleted) != 0 {
		t.Errorf("runtime entries deleted on a refused rollback delete: %v", *deleted)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "scion-agent.json")); err != nil {
		t.Errorf("hub-project state dir removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, agent.MoveMarkerFile)); err != nil {
		t.Errorf("agent directory removed: %v", err)
	}
}

// Positive control: the hub-project state directory carries the move's
// marker, so the rollback delete removes it with the agent directory.
func TestDeleteAgent_ForcedMoveRollback_MarkedHubStateDirRemoved(t *testing.T) {
	srv, deleted, agentDir, hubDir := newHubStateMoveFixture(t)
	if err := os.WriteFile(filepath.Join(hubDir, agent.MoveMarkerFile), []byte(hubStateMoveID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", hubStateMoveDeleteQuery)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if len(*deleted) != 1 {
		t.Errorf("runtime entries deleted = %v, want the agent's one", *deleted)
	}
	if _, err := os.Stat(hubDir); !os.IsNotExist(err) {
		t.Errorf("the marked hub-project state dir was not removed (stat err %v)", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("the agent directory was not removed (stat err %v)", err)
	}
}
