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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#4769: delete resolves an agent's state directory
// with the same Hub project ID that lookup (agentStateDir) uses.

const (
	hubRootMarkerID = "11111111-aaaa-4aaa-8aaa-111111111111"
	hubRootHubID    = "22222222-bbbb-4bbb-8bbb-222222222222"
)

// hubRootLayout is a project whose project-id marker is markerID, with a
// shared-workspace agent provisioned for Hub project ID hubID: its state
// (scion-agent.json) in the directory lookup resolves from hubID, and its
// home (agent-info.json) under the marker's external root, where
// config.GetAgentHomePath puts it.
type hubRootLayout struct {
	projectPath string // the project's .scion dir
	stateDir    string // lookup's state dir, from hubID
	markerDir   string // the marker's external agent dir (holds home/)
	agentHome   string
}

func setupHubRootProject(t *testing.T, markerID, hubID, agentName string) hubRootLayout {
	t.Helper()
	// Run the host-side delete path even when the test runs inside an
	// agent container.
	t.Setenv("SCION_HOST_UID", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)

	scionDir := filepath.Join(home, "project", ".scion")
	mustMkdirAll(t, filepath.Join(scionDir, "agents"))
	if err := config.WriteProjectID(scionDir, markerID); err != nil {
		t.Fatalf("WriteProjectID: %v", err)
	}
	extAgents, err := config.GetGitProjectExternalAgentsDir(scionDir)
	if err != nil || extAgents == "" {
		t.Fatalf("GetGitProjectExternalAgentsDir: dir=%q err=%v", extAgents, err)
	}
	stateDir, err := config.AgentDirForProject(scionDir, agentName, true, hubID)
	if err != nil {
		t.Fatalf("AgentDirForProject: %v", err)
	}
	mustMkdirAll(t, stateDir)
	for _, f := range []string{"scion-agent.json", "prompt.md"} {
		content := "task\n"
		if f == "scion-agent.json" {
			content = "{}"
		}
		if err := os.WriteFile(filepath.Join(stateDir, f), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	mustMkdirAll(t, agentHome)
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), []byte(`{"name":"`+agentName+`"}`), 0644); err != nil {
		t.Fatalf("write agent-info.json: %v", err)
	}

	// The state dir is the one lookup resolves for this agent.
	got, shared, err := agentStateDir(scionDir, agentName, false, hubID, true)
	if err != nil || !shared || got != stateDir {
		t.Fatalf("agentStateDir = %q, %v, %v; want %q, true, nil", got, shared, err, stateDir)
	}
	return hubRootLayout{
		projectPath: scionDir,
		stateDir:    stateDir,
		markerDir:   filepath.Join(extAgents, agentName),
		agentHome:   agentHome,
	}
}

func assertGone(t *testing.T, what, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s %s still exists (stat err %v)", what, path, err)
	}
}

func assertPresent(t *testing.T, what, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s %s was removed: %v", what, path, err)
	}
}

// When the Hub project ID differs from the project-id marker, delete with
// the Hub project ID removes the state dir lookup uses, and still removes
// the marker-root dir holding the agent's home.
func TestDeleteAgentFilesForHubProject_RemovesLookupStateDir(t *testing.T) {
	const agentName = "worker"
	l := setupHubRootProject(t, hubRootMarkerID, hubRootHubID, agentName)
	if l.stateDir == l.markerDir {
		t.Fatalf("test setup: state dir and marker dir are the same: %s", l.stateDir)
	}
	// Another agent's state dir under the same Hub root is not touched.
	sibling, err := config.AgentDirForProject(l.projectPath, "other", true, hubRootHubID)
	if err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, sibling)

	if _, err := DeleteAgentFilesForHubProject(agentName, l.projectPath, hubRootHubID, false); err != nil {
		t.Fatalf("DeleteAgentFilesForHubProject: %v", err)
	}
	assertGone(t, "lookup state dir", l.stateDir)
	assertGone(t, "marker-root agent dir", l.markerDir)
	assertGone(t, "agent home", l.agentHome)
	assertPresent(t, "sibling state dir", sibling)
}

// Without a Hub project ID, delete behaves as before: it removes the
// marker-root dir only.
func TestDeleteAgentFiles_NoHubProjectIDKeepsMarkerRootsOnly(t *testing.T) {
	const agentName = "worker"
	l := setupHubRootProject(t, hubRootMarkerID, hubRootHubID, agentName)

	if _, err := DeleteAgentFiles(agentName, l.projectPath, false); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	assertGone(t, "marker-root agent dir", l.markerDir)
	assertPresent(t, "Hub-root state dir", l.stateDir)
}

// When the Hub project ID equals the marker, both resolve to one dir,
// which is removed once without error.
func TestDeleteAgentFilesForHubProject_SameProjectID(t *testing.T) {
	const agentName = "worker"
	l := setupHubRootProject(t, hubRootMarkerID, hubRootMarkerID, agentName)
	if l.stateDir != l.markerDir {
		t.Fatalf("test setup: want one dir, got %s and %s", l.stateDir, l.markerDir)
	}

	if _, err := DeleteAgentFilesForHubProject(agentName, l.projectPath, hubRootMarkerID, false); err != nil {
		t.Fatalf("DeleteAgentFilesForHubProject: %v", err)
	}
	assertGone(t, "state dir", l.stateDir)
}

// The checks that run before any root is built still refuse: an agent name
// that is not a single path element, and a project path that resolves to
// no project. Neither touches the Hub-root state dir.
func TestDeleteAgentFilesForHubProject_EarlyChecksTouchNothing(t *testing.T) {
	const agentName = "worker"
	l := setupHubRootProject(t, hubRootMarkerID, hubRootHubID, agentName)

	if _, err := DeleteAgentFilesForHubProject("../"+agentName, l.projectPath, hubRootHubID, false); err == nil {
		t.Error("multi-element agent name: want an error, got nil")
	}
	missing := filepath.Join(t.TempDir(), "gone", ".scion")
	if _, err := DeleteAgentFilesForHubProject(agentName, missing, hubRootHubID, false); !errors.Is(err, ErrAgentProjectUnresolved) {
		t.Errorf("unresolved project: err = %v, want ErrAgentProjectUnresolved", err)
	}
	assertPresent(t, "Hub-root state dir", l.stateDir)
	assertPresent(t, "marker-root agent dir", l.markerDir)
}

// A manager delete takes the Hub project ID from ctx, as lookup does.
func TestAgentManagerDeleteTarget_UsesHubProjectIDFromContext(t *testing.T) {
	const agentName = "worker"
	l := setupHubRootProject(t, hubRootMarkerID, hubRootHubID, agentName)
	m := &AgentManager{}

	ctx := api.ContextWithHubProjectID(context.Background(), hubRootHubID)
	if _, err := m.DeleteTarget(ctx, agentName, runtime.RunRef{}, true, l.projectPath, false); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	assertGone(t, "lookup state dir", l.stateDir)
	assertGone(t, "marker-root agent dir", l.markerDir)
}
