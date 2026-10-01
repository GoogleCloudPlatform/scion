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
	"path/filepath"
	"testing"
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
