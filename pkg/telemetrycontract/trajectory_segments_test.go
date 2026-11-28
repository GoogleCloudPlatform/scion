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

package telemetrycontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTrajectorySegmentPaths(t *testing.T) {
	if got, want := TrajectorySegmentsPrepareAgentPath("agent-1"), "/api/v1/agents/agent-1/trajectory/segments:prepare"; got != want {
		t.Errorf("PreparePath = %q, want %q", got, want)
	}
	if got, want := TrajectorySegmentsCommitAgentPath("agent-1"), "/api/v1/agents/agent-1/trajectory/segments:commit"; got != want {
		t.Errorf("CommitPath = %q, want %q", got, want)
	}
	if got, want := TrajectorySegmentContentAgentPath("agent-1", "seg-1"), "/api/v1/agents/agent-1/trajectory/segments/seg-1/content"; got != want {
		t.Errorf("ContentPath = %q, want %q", got, want)
	}

	obj := TrajectoryRawObjectPath("hub-1", "proj-1", "agent-1", "run-1", "claude", ".claude/projects/-workspace/abc.jsonl", 2, 4096)
	wantObj := "hub-1/trajectories/v1/raw/project=proj-1/agent=agent-1/run=run-1/claude/.claude/projects/-workspace/abc.jsonl/g2-0000000000004096.seg.gz"
	if obj != wantObj {
		t.Errorf("ObjectPath = %q, want %q", obj, wantObj)
	}
	if got, want := TrajectoryRawManifestPath(obj), wantObj+".manifest.json"; got != want {
		t.Errorf("ManifestPath = %q, want %q", got, want)
	}
}

func TestValidateTrajectoryRelativePath(t *testing.T) {
	valid := []string{
		".claude/projects/-workspace/0b6f1234.jsonl",
		".claude/projects/-workspace/sess/subagents/agent-abc.jsonl",
	}
	for _, p := range valid {
		if err := ValidateTrajectoryRelativePath("claude", p); err != nil {
			t.Errorf("expected valid path %q, got error: %v", p, err)
		}
	}

	invalid := []struct {
		name    string
		harness string
		path    string
	}{
		{"empty", "claude", ""},
		{"absolute", "claude", "/.claude/projects/a.jsonl"},
		{"trailing slash", "claude", ".claude/projects/a.jsonl/"},
		{"dotdot", "claude", ".claude/projects/../etc/passwd.jsonl"},
		{"dot", "claude", ".claude/projects/./a.jsonl"},
		{"empty element", "claude", ".claude/projects//a.jsonl"},
		{"backslash", "claude", `.claude/projects\a.jsonl`},
		{"nul byte", "claude", ".claude/projects/a\x00.jsonl"},
		{"control char", "claude", ".claude/projects/a\n.jsonl"},
		{"del char", "claude", ".claude/projects/a\x7f.jsonl"},
		{"too long", "claude", ".claude/projects/" + strings.Repeat("a", TrajectoryMaxRelativePathBytes) + ".jsonl"},
		{"wrong prefix", "claude", ".codex/sessions/a.jsonl"},
		{"wrong suffix", "claude", ".claude/projects/-workspace/a.json"},
		{"bare suffix", "claude", ".claude/projects/.jsonl"},
		{"seg.gz element", "claude", ".claude/projects/a.jsonl/g0-0000000000000000.seg.gz/x.jsonl"},
		{"manifest.json element", "claude", ".claude/projects/a.jsonl/g0-0000000000000000.seg.gz.manifest.json/x.jsonl"},
		{"unsupported harness", "codex", ".claude/projects/-workspace/a.jsonl"},
	}
	for _, tc := range invalid {
		if err := ValidateTrajectoryRelativePath(tc.harness, tc.path); err == nil {
			t.Errorf("%s: expected error for (%q, %q), got nil", tc.name, tc.harness, tc.path)
		}
	}
}

func TestTrajectoryCapturePolicyValidationAndStatusJSON(t *testing.T) {
	for _, valid := range []string{TrajectoryCapturePolicyFull, TrajectoryCapturePolicyMetadataOnly, TrajectoryCapturePolicyOff} {
		if !IsValidTrajectoryCapturePolicy(valid) {
			t.Errorf("IsValidTrajectoryCapturePolicy(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"", "FULL", "partial", "on", "disabled"} {
		if IsValidTrajectoryCapturePolicy(invalid) {
			t.Errorf("IsValidTrajectoryCapturePolicy(%q) = true, want false", invalid)
		}
	}

	raw, err := json.Marshal(TrajectoryCapturePolicyStatus{
		Policy:        TrajectoryCapturePolicyFull,
		EnforcedByHub: false,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got, want := string(raw), `{"policy":"full","enforced_by_hub":false}`; got != want {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}
