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

package cmd

import (
	"strings"
	"testing"
)

// TestResolveReincarnateTarget is the design §3.4 Amendment A3.11
// table test for the self/handoff-required rule: self-migration (no
// argument, or an explicit argument matching $SCION_AGENT_NAME) requires
// --handoff-file, unless the request is a dry run (which migrates nothing).
// Migrating another agent never requires a handoff.
func TestResolveReincarnateTarget(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		selfName       string
		hasHandoffFile bool
		dryRun         bool
		wantAgentName  string
		wantIsSelf     bool
		wantErrSubstr  string // "" = no error
	}{
		{
			name:          "explicit other agent, no self context: no handoff needed",
			args:          []string{"other-agent"},
			selfName:      "",
			wantAgentName: "other-agent",
			wantIsSelf:    false,
		},
		{
			name:          "explicit other agent, inside an agent container: still not self",
			args:          []string{"other-agent"},
			selfName:      "me",
			wantAgentName: "other-agent",
			wantIsSelf:    false,
		},
		{
			name:          "no argument, no self context: error, not a handoff error",
			args:          nil,
			selfName:      "",
			wantErrSubstr: "specify an agent name",
		},
		{
			name:          "no argument, inside an agent container, no handoff: self-migration requires one",
			args:          nil,
			selfName:      "me",
			wantErrSubstr: "self-migration requires --handoff-file",
		},
		{
			name:           "no argument, inside an agent container, with handoff: allowed",
			args:           nil,
			selfName:       "me",
			hasHandoffFile: true,
			wantAgentName:  "me",
			wantIsSelf:     true,
		},
		{
			name:          "no argument, inside an agent container, dry-run, no handoff: allowed",
			args:          nil,
			selfName:      "me",
			dryRun:        true,
			wantAgentName: "me",
			wantIsSelf:    true,
		},
		{
			name:          "explicit argument equal to self, no handoff: self-migration requires one",
			args:          []string{"me"},
			selfName:      "me",
			wantErrSubstr: "self-migration requires --handoff-file",
		},
		{
			name:           "explicit argument equal to self, with handoff: allowed",
			args:           []string{"me"},
			selfName:       "me",
			hasHandoffFile: true,
			wantAgentName:  "me",
			wantIsSelf:     true,
		},
		{
			name:          "explicit argument equal to self, dry-run, no handoff: allowed",
			args:          []string{"me"},
			selfName:      "me",
			dryRun:        true,
			wantAgentName: "me",
			wantIsSelf:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentName, isSelf, err := resolveReincarnateTarget(tc.args, tc.selfName, tc.hasHandoffFile, tc.dryRun)

			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (agentName=%q isSelf=%v)", tc.wantErrSubstr, agentName, isSelf)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if agentName != tc.wantAgentName {
				t.Errorf("agentName = %q, want %q", agentName, tc.wantAgentName)
			}
			if isSelf != tc.wantIsSelf {
				t.Errorf("isSelf = %v, want %v", isSelf, tc.wantIsSelf)
			}
		})
	}
}
