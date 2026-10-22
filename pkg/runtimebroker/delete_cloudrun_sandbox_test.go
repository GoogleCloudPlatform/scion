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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// sandboxStateEntryJSON is one cloudrun-sandbox state file entry for agent
// "dev" of project slug/projectID, keyed by its project-prefixed sandbox name.
func sandboxStateEntryJSON(slug, projectID string) string {
	name := slug + "--dev"
	return `"` + name + `":{"sandbox_name":"` + name + `","agent_id":"` + name + `",` +
		`"project_id":"` + projectID + `","project":"` + slug + `",` +
		`"labels":{"scion.agent":"true","scion.name":"dev","scion.project_id":"` + projectID + `"}}`
}

// TestDeleteAgent_CloudRunSandbox_HubNameDeletesSandbox: a hub delete names
// the agent ("dev"), while the cloudrun-sandbox runtime records the sandbox
// under the project-prefixed container name ("proj-b--dev"). The broker's
// delete resolution must still find the sandbox, run `sandbox delete` on it
// and remove its state entry, rather than deleting only the agent's files
// and leaving the sandbox behind (ptone/scion#5156). A same-named agent of
// another project ("proj-a--dev") reports the same Name, so the delete must
// leave its sandbox, state entry and files untouched.
func TestDeleteAgent_CloudRunSandbox_HubNameDeletesSandbox(t *testing.T) {
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

	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "sandbox")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	state := "{" + sandboxStateEntryJSON("proj-a", scopeProjA) + "," +
		sandboxStateEntryJSON("proj-b", scopeProjB) + "}"
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := runtime.NewCloudRunSandboxRuntimeForTest(bin, statePath, filepath.Join(dir, "scion"))

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	srv := New(cfg, agent.NewManager(rt), rt, config.RuntimeBuildRecord{})
	// The agents' files exist, so a resolution that misses the sandbox
	// still answers 204 (a files-only delete).
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	makeHubProject(t, home, "proj-b", scopeProjB, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(calls)
	if !strings.Contains(string(got), "delete --force proj-b--dev\n") {
		t.Errorf("sandbox CLI calls = %q, want a delete --force of proj-b--dev", got)
	}
	if strings.Contains(string(got), "proj-a--dev") {
		t.Errorf("sandbox CLI calls = %q, want none for the other project's proj-a--dev", got)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(after), "proj-b--dev") {
		t.Errorf("state entry still present after the delete: %s", after)
	}
	if !strings.Contains(string(after), `"proj-a--dev"`) {
		t.Errorf("the other project's state entry was removed: %s", after)
	}
	assertUntouched(t, scionA, "dev", infoA)
}
