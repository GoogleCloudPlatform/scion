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
	"strings"
	"testing"
)

// TestParseGitHubSkillURI_ErrorsOmitQuery checks the whole error text for
// query-related rejections: it names the URI without its query and never
// contains the token value or any other query content.
func TestParseGitHubSkillURI_ErrorsOmitQuery(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "gh unsupported parameter",
			input:   "gh://acme/repo/alpha-skill@main?access_token=made-up-value-1",
			wantErr: `invalid gh:// URI "gh://acme/repo/alpha-skill@main": unsupported query parameter; only ?token=SECRET_NAME is supported`,
		},
		{
			name:    "gh invalid token value",
			input:   "gh://acme/repo/alpha-skill@main?token=made-up-value-5",
			wantErr: `invalid gh:// URI "gh://acme/repo/alpha-skill@main": the token secret name must match [A-Z][A-Z0-9_]* (uppercase env-var style)`,
		},
		{
			name:    "gh token plus unknown",
			input:   "gh://acme/repo/alpha-skill@main?token=GOOD&access_token=made-up-value-1",
			wantErr: `invalid gh:// URI "gh://acme/repo/alpha-skill@main": the token secret name must match [A-Z][A-Z0-9_]* (uppercase env-var style)`,
		},
		{
			name:    "gh empty token value",
			input:   "gh://acme/repo/alpha-skill@main?token=",
			wantErr: `invalid gh:// URI "gh://acme/repo/alpha-skill@main": empty ?token= value`,
		},
		{
			name:    "gh bad shape with token",
			input:   "gh://acme/alpha-skill?token=SKILLS_TOKEN",
			wantErr: `invalid gh:// URI "gh://acme/alpha-skill": expected gh://owner/repo/skill-name[@ref][?token=SECRET_NAME]`,
		},
		{
			name:    "github unsupported parameter",
			input:   "https://github.com/acme/repo/tree/main/skills/alpha-skill?access_token=made-up-value-1",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main/skills/alpha-skill": unsupported query parameter; only ?token=SECRET_NAME is supported`,
		},
		{
			name:    "github invalid token value",
			input:   "https://github.com/acme/repo/tree/main/skills/alpha-skill?token=made-up-value-5",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main/skills/alpha-skill": the token secret name must match [A-Z][A-Z0-9_]* (uppercase env-var style)`,
		},
		{
			name:    "github empty token value",
			input:   "https://github.com/acme/repo/tree/main/skills/alpha-skill?token=",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main/skills/alpha-skill": empty ?token= value`,
		},
		{
			name:    "github bad path with token",
			input:   "https://github.com/acme/repo/blob/main/skills/alpha-skill?token=SKILLS_TOKEN",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/blob/main/skills/alpha-skill": expected https://github.com/owner/repo/tree/ref/path/to/skill`,
		},
		{
			name:    "not a GitHub URI",
			input:   "https://example.com/acme/repo?access_token=made-up-value-1",
			wantErr: `not a GitHub skill URI: "https://example.com/acme/repo"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGitHubSkillURI(tt.input)
			if err == nil {
				t.Fatalf("ParseGitHubSkillURI(%q) succeeded, want an error", tt.input)
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q\nwant    %q", err.Error(), tt.wantErr)
			}
			if strings.Contains(err.Error(), "made-up-value") {
				t.Errorf("error contains the query value: %q", err.Error())
			}
		})
	}
}
