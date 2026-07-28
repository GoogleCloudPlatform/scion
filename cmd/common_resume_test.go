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

import "testing"

// TestLocalResumeDecision pins local mode to the same contract the Hub applies
// in resumeInPlaceDecision: --force is an error-phase recovery tool only, and
// an error-phase agent is refused without it.
func TestLocalResumeDecision(t *testing.T) {
	tests := []struct {
		name         string
		savedPhase   string
		resume       bool
		force        bool
		wantResume   bool
		wantImplicit bool
		wantForced   bool
		wantErr      bool
	}{
		{
			name:         "suspended resumes implicitly on start",
			savedPhase:   "suspended",
			wantResume:   true,
			wantImplicit: true,
		},
		{
			name:       "suspended resumes explicitly without implicit notice",
			savedPhase: "suspended",
			resume:     true,
			wantResume: true,
		},
		{
			name:       "stopped gets a fresh session on resume",
			savedPhase: "stopped",
			resume:     true,
		},
		{
			// --force must not turn a clean shutdown into a session continue;
			// the Hub treats this as an ordinary resume too.
			name:       "stopped ignores force",
			savedPhase: "stopped",
			resume:     true,
			force:      true,
		},
		{
			// Mirrors the Hub's 409 on a crashed agent.
			name:       "error without force is refused",
			savedPhase: "error",
			resume:     true,
			wantErr:    true,
		},
		{
			name:       "error with force continues the interrupted session",
			savedPhase: "error",
			resume:     true,
			force:      true,
			wantResume: true,
			wantForced: true,
		},
		{
			// 'start' on a crashed agent is not a resume request, so it is not
			// refused; it simply starts fresh.
			name:       "error without resume starts fresh",
			savedPhase: "error",
			force:      true,
		},
		{
			name:       "running with force is not a forced recovery",
			savedPhase: "running",
			resume:     true,
			force:      true,
			wantResume: true,
		},
		{
			name:       "unknown phase falls through to the resume flag",
			savedPhase: "",
			resume:     true,
			wantResume: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotResume, gotImplicit, gotForced, err := localResumeDecision(tt.savedPhase, tt.resume, tt.force, "agent-x")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotResume != tt.wantResume {
				t.Errorf("effectiveResume = %v, want %v", gotResume, tt.wantResume)
			}
			if gotImplicit != tt.wantImplicit {
				t.Errorf("implicitResume = %v, want %v", gotImplicit, tt.wantImplicit)
			}
			if gotForced != tt.wantForced {
				t.Errorf("forcedRecovery = %v, want %v", gotForced, tt.wantForced)
			}
		})
	}
}
