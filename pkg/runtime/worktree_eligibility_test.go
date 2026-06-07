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

package runtime

import (
	"strings"
	"testing"
)

func TestWorktreeEligibleForVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		wantOK  bool
		wantSub string // substring expected in reason when !ok
	}{
		{
			name:    "below minimum",
			version: "2.46.0",
			wantOK:  false,
			wantSub: "2.46.0",
		},
		{
			name:   "exact minimum",
			version: "2.47.0",
			wantOK:  true,
		},
		{
			name:   "above minimum",
			version: "2.54.1",
			wantOK:  true,
		},
		{
			name:    "malformed version",
			version: "not-a-version",
			wantOK:  false,
			wantSub: "not-a-version",
		},
		{
			name:   "major version ahead",
			version: "3.0.0",
			wantOK:  true,
		},
		{
			name:    "empty string",
			version: "",
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := worktreeEligibleForVersion(tt.version)
			if ok != tt.wantOK {
				t.Errorf("worktreeEligibleForVersion(%q) ok = %v, want %v (reason: %s)", tt.version, ok, tt.wantOK, reason)
			}
			if !ok && tt.wantSub != "" && !strings.Contains(reason, tt.wantSub) {
				t.Errorf("reason %q should contain %q", reason, tt.wantSub)
			}
			if ok && reason != "" {
				t.Errorf("expected empty reason when eligible, got %q", reason)
			}
		})
	}
}
