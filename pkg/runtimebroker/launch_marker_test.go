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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestLaunchMarker_WriteReadMatch(t *testing.T) {
	projectDir := t.TempDir()

	if got := readLaunchMarker(projectDir, false, "agent-1"); got != "" {
		t.Fatalf("expected no marker before any write, got %q", got)
	}
	if launchMarkerMatches(projectDir, false, "agent-1", "L1") {
		t.Fatal("expected no match before any write")
	}

	if err := writeLaunchMarker(projectDir, false, "agent-1", "L1"); err != nil {
		t.Fatalf("writeLaunchMarker: %v", err)
	}
	if got := readLaunchMarker(projectDir, false, "agent-1"); got != "L1" {
		t.Fatalf("readLaunchMarker = %q, want L1", got)
	}
	if !launchMarkerMatches(projectDir, false, "agent-1", "L1") {
		t.Fatal("expected a match for the launch ID just written")
	}
	if launchMarkerMatches(projectDir, false, "agent-1", "L-other") {
		t.Fatal("expected no match for a different launch ID")
	}

	// The marker lives outside agents/, as a sibling directory (design
	// t1-async-create-v11.md §3.8.2 step 5.2, F5).
	markerPath := filepath.Join(projectDir, "launch-markers", "agent-1")
	if got := markerPath; filepath.Dir(got) == filepath.Join(projectDir, "agents") {
		t.Fatal("marker must not live under agents/")
	}
}

// TestLaunchMarker_NewerLaunchWins covers design §3.8.4: "A newer launch, on
// any replica sharing the storage, overwrites it before creating files, so
// an older launch then keeps the files" -- i.e. the older launch's
// launchMarkerMatches goes false once a newer write lands.
func TestLaunchMarker_NewerLaunchWins(t *testing.T) {
	projectDir := t.TempDir()

	if err := writeLaunchMarker(projectDir, false, "agent-1", "L-old"); err != nil {
		t.Fatalf("writeLaunchMarker(old): %v", err)
	}
	if err := writeLaunchMarker(projectDir, false, "agent-1", "L-new"); err != nil {
		t.Fatalf("writeLaunchMarker(new): %v", err)
	}
	if launchMarkerMatches(projectDir, false, "agent-1", "L-old") {
		t.Fatal("the old launch must no longer match after a newer write")
	}
	if !launchMarkerMatches(projectDir, false, "agent-1", "L-new") {
		t.Fatal("the new launch must match its own write")
	}

	// The old launch's removeLaunchMarkerIfMatches must be a no-op: it must
	// not remove the newer launch's marker.
	removeLaunchMarkerIfMatches(projectDir, false, "agent-1", "L-old")
	if !launchMarkerMatches(projectDir, false, "agent-1", "L-new") {
		t.Fatal("an older launch's removal must not disturb the newer launch's marker")
	}

	removeLaunchMarkerIfMatches(projectDir, false, "agent-1", "L-new")
	if readLaunchMarker(projectDir, false, "agent-1") != "" {
		t.Fatal("the current launch's own removal must clear the marker")
	}
}

// TestLaunchMarker_ResolvesUnderProjectScionDir covers design
// t1-async-create-v11.md §3.8.2 step 5.2 and §3.8.4: the marker must live on
// the same storage as the agent files, under the project's resolved .scion
// dir, not under whatever root path the caller happened to pass in. A bare
// t.TempDir() with nothing under it resolves to itself (no .scion to find),
// which is why the other tests in this file cannot see a resolution bug; this
// test creates a real .scion dir so config.GetResolvedProjectDir has
// something to resolve to.
func TestLaunchMarker_ResolvesUnderProjectScionDir(t *testing.T) {
	root := t.TempDir()
	scionDir := filepath.Join(root, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0755); err != nil {
		t.Fatalf("mkdir .scion/agents: %v", err)
	}

	// Pass the project root, exactly as lc.opts.ProjectPath carries it — not
	// the already-resolved .scion dir.
	if err := writeLaunchMarker(root, false, "agent-1", "L1"); err != nil {
		t.Fatalf("writeLaunchMarker: %v", err)
	}

	wantPath := filepath.Join(scionDir, "launch-markers", "agent-1")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected marker at %s, got: %v", wantPath, err)
	}

	wrongPath := filepath.Join(root, "launch-markers", "agent-1")
	if _, err := os.Stat(wrongPath); err == nil {
		t.Fatalf("marker must not be written at the unresolved root (%s)", wrongPath)
	}

	if got := readLaunchMarker(root, false, "agent-1"); got != "L1" {
		t.Fatalf("readLaunchMarker = %q, want L1", got)
	}
	if !launchMarkerMatches(root, false, "agent-1", "L1") {
		t.Fatal("expected a match for the launch ID just written")
	}

	removeLaunchMarkerIfMatches(root, false, "agent-1", "L1")
	if _, err := os.Stat(wantPath); err == nil {
		t.Fatal("expected the marker to be removed")
	}
}

// TestLaunchMarker_SharedWorkspaceExternalLayout covers the shared-workspace
// case: SelectAgentsRoot resolves to the external project-configs agents dir
// (GetGitProjectExternalAgentsDir), so the marker must sit alongside that
// external agents dir, not under the in-repo root.
func TestLaunchMarker_SharedWorkspaceExternalLayout(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	root := filepath.Join(tmpDir, "project")
	scionDir := filepath.Join(root, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir .scion: %v", err)
	}
	if err := config.WriteProjectID(scionDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatalf("WriteProjectID: %v", err)
	}

	extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir)
	if err != nil || extAgentsDir == "" {
		t.Fatalf("GetGitProjectExternalAgentsDir: dir=%q err=%v", extAgentsDir, err)
	}
	if err := os.MkdirAll(extAgentsDir, 0755); err != nil {
		t.Fatalf("mkdir external agents dir: %v", err)
	}

	if err := writeLaunchMarker(root, true, "agent-1", "L1"); err != nil {
		t.Fatalf("writeLaunchMarker: %v", err)
	}

	wantPath := filepath.Join(filepath.Dir(extAgentsDir), "launch-markers", "agent-1")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected marker at %s, got: %v", wantPath, err)
	}

	if got := readLaunchMarker(root, true, "agent-1"); got != "L1" {
		t.Fatalf("readLaunchMarker = %q, want L1", got)
	}
}

// TestLaunchMarker_FailedRenameRemovesTempFile covers writeLaunchMarker
// removing its temp file when the final rename fails: the marker path is a
// non-empty directory, so renaming the temp file onto it fails, and no
// "<slug>.*.tmp" file may be left behind in the markers directory.
func TestLaunchMarker_FailedRenameRemovesTempFile(t *testing.T) {
	projectDir := t.TempDir()
	const slug = "agent-rename-fails"

	dir, err := launchMarkersDir(projectDir, false)
	if err != nil {
		t.Fatalf("launchMarkersDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, slug), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug, "keep"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := writeLaunchMarker(projectDir, false, slug, "L-1"); err == nil {
		t.Fatal("expected writeLaunchMarker to fail when the marker path is a non-empty directory")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, slug+".*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("expected the temp file to be removed after a failed rename, found %v", leftovers)
	}
}
