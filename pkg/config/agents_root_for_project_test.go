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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newAgentsRootProject(t *testing.T, markerID string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	projectDir := filepath.Join(t.TempDir(), "my-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if markerID != "" {
		if err := WriteProjectID(projectDir, markerID); err != nil {
			t.Fatal(err)
		}
	}
	return projectDir
}

func TestAgentsRootForProject_NotShared(t *testing.T) {
	projectDir := newAgentsRootProject(t, "11111111-1111-1111-1111-111111111111")
	root, err := AgentsRootForProject(projectDir, false, "22222222-2222-2222-2222-222222222222")
	if err != nil || root != filepath.Join(projectDir, "agents") {
		t.Fatalf("root = %q, %v; want <projectDir>/agents", root, err)
	}
}

// TestAgentsRootForProject_HubProjectIDWinsOverMarker pins C-ROOT-5: with a
// Hub-supplied project ID, the external root derives from that ID, not from
// the project-id marker inside the project dir.
func TestAgentsRootForProject_HubProjectIDWinsOverMarker(t *testing.T) {
	const hubID = "22222222-2222-2222-2222-222222222222"
	projectDir := newAgentsRootProject(t, hubID)
	want, err := GetGitProjectExternalAgentsDir(projectDir)
	if err != nil || want == "" {
		t.Fatalf("precondition: marker-based external dir = %q, %v", want, err)
	}
	// Rewrite the marker to name another project.
	if err := WriteProjectID(projectDir, "99999999-9999-9999-9999-999999999999"); err != nil {
		t.Fatal(err)
	}
	got, err := AgentsRootForProject(projectDir, true, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("external root = %q, want %q (derived from the Hub project ID, unchanged by a marker that names another project)", got, want)
	}
	if strings.HasPrefix(got, projectDir) {
		t.Fatalf("external root %q must not be under the project dir", got)
	}
}

func TestAgentsRootForProject_LocalUsesMarker(t *testing.T) {
	projectDir := newAgentsRootProject(t, "33333333-3333-3333-3333-333333333333")
	want, _ := GetGitProjectExternalAgentsDir(projectDir)
	got, err := AgentsRootForProject(projectDir, true, "")
	if err != nil || got != want || want == "" {
		t.Fatalf("root = %q, %v; want the marker-based %q", got, err, want)
	}
}

func TestAgentsRootForProject_SharedWithoutExternalRootFails(t *testing.T) {
	projectDir := newAgentsRootProject(t, "")
	_, err := AgentsRootForProject(projectDir, true, "")
	if !errors.Is(err, ErrAgentStateDirUnavailable) {
		t.Fatalf("err = %v, want ErrAgentStateDirUnavailable (never <projectDir>/agents)", err)
	}
}

func TestAgentDirForProject_RejectsNonSingleElement(t *testing.T) {
	projectDir := newAgentsRootProject(t, "")
	for _, name := range []string{"../escape", "a/b", "x/../y"} {
		if _, err := AgentDirForProject(projectDir, name, false, ""); err == nil {
			t.Errorf("AgentDirForProject(%q) accepted a non-single path element", name)
		}
	}
	dir, err := AgentDirForProject(projectDir, "ok-agent", false, "")
	if err != nil || dir != filepath.Join(projectDir, "agents", "ok-agent") {
		t.Fatalf("dir = %q, %v", dir, err)
	}
}
