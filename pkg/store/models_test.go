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

package store

import "testing"

func TestResolveWorkspaceSharingMode(t *testing.T) {
	tests := []struct {
		label string
		want  WorkspaceSharingMode
	}{
		// Canonical label values
		{label: "shared", want: SharingModeSharedPlain},
		{label: "per-agent", want: SharingModeClonePerAgent},
		{label: "worktree-per-agent", want: SharingModeWorktreePerAgent},

		// Canonical enum values (accepted as aliases)
		{label: "shared-plain", want: SharingModeSharedPlain},
		{label: "clone-per-agent", want: SharingModeClonePerAgent},

		// Empty → default (shared-plain)
		{label: "", want: SharingModeSharedPlain},

		// Unknown → default (shared-plain)
		{label: "unknown-mode", want: SharingModeSharedPlain},
		{label: "SHARED", want: SharingModeSharedPlain}, // case-sensitive: unrecognized → default
	}

	for _, tt := range tests {
		t.Run("label="+tt.label, func(t *testing.T) {
			got := ResolveWorkspaceSharingMode(tt.label)
			if got != tt.want {
				t.Errorf("ResolveWorkspaceSharingMode(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}

func TestWorkspaceSharingMode_Constants(t *testing.T) {
	// Verify the existing label constants are unchanged (lossless migration).
	if WorkspaceModeShared != "shared" {
		t.Errorf("WorkspaceModeShared = %q, want %q", WorkspaceModeShared, "shared")
	}
	if WorkspaceModePerAgent != "per-agent" {
		t.Errorf("WorkspaceModePerAgent = %q, want %q", WorkspaceModePerAgent, "per-agent")
	}
	if LabelWorkspaceMode != "scion.dev/workspace-mode" {
		t.Errorf("LabelWorkspaceMode = %q, want %q", LabelWorkspaceMode, "scion.dev/workspace-mode")
	}

	// Verify the new typed constants have the expected string values.
	if SharingModeSharedPlain != "shared-plain" {
		t.Errorf("SharingModeSharedPlain = %q, want %q", SharingModeSharedPlain, "shared-plain")
	}
	if SharingModeClonePerAgent != "clone-per-agent" {
		t.Errorf("SharingModeClonePerAgent = %q, want %q", SharingModeClonePerAgent, "clone-per-agent")
	}
	if SharingModeWorktreePerAgent != "worktree-per-agent" {
		t.Errorf("SharingModeWorktreePerAgent = %q, want %q", SharingModeWorktreePerAgent, "worktree-per-agent")
	}
}
