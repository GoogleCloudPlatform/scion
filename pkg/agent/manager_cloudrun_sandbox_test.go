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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestAgentManagerDelete_CloudRunSandbox_AgentName: deleting a
// cloudrun-sandbox agent by its agent name runs `sandbox delete` on the
// agent's sandbox, whose name is project-prefixed, and removes its state
// entry, instead of finding nothing and leaving the sandbox behind
// (ptone/scion#5156).
func TestAgentManagerDelete_CloudRunSandbox_AgentName(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "sandbox")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	state := `{"proj-b--dev":{"sandbox_name":"proj-b--dev","agent_id":"proj-b--dev",` +
		`"labels":{"scion.agent":"true","scion.name":"dev"}}}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := runtime.NewCloudRunSandboxRuntimeForTest(bin, statePath, filepath.Join(dir, "scion"))
	mgr := &AgentManager{Runtime: rt}

	if _, err := mgr.Delete(context.Background(), "dev", false, "", false); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	got, _ := os.ReadFile(calls)
	if !strings.Contains(string(got), "delete --force proj-b--dev\n") {
		t.Errorf("sandbox CLI calls = %q, want a delete --force of proj-b--dev", got)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(after), "proj-b--dev") {
		t.Errorf("state entry still present after Delete(): %s", after)
	}
}
