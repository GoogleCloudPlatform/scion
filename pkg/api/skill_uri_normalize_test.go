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

package api

import (
	"strings"
	"testing"
)

func TestNormalizeSkillURI(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string // empty means error expected
		wantErr string // substring that must appear in the error
	}{
		// ── gh:// passthrough / validation ──────────────────────────────────
		{
			name:  "gh shorthand no ref",
			input: "gh://org/repo/my-skill",
			want:  "gh://org/repo/my-skill",
		},
		{
			name:  "gh shorthand with ref",
			input: "gh://org/repo/my-skill@main",
			want:  "gh://org/repo/my-skill@main",
		},
		{
			name:  "gh shorthand with sha ref",
			input: "gh://org/repo/my-skill@a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			want:  "gh://org/repo/my-skill@a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		},
		{
			name:  "gh shorthand with token",
			input: "gh://org/repo/my-skill?token=SKILLS_TOKEN",
			want:  "gh://org/repo/my-skill?token=SKILLS_TOKEN",
		},
		{
			name:  "gh shorthand with ref and token",
			input: "gh://org/repo/my-skill@v1.2.3?token=PARTNER_TOKEN",
			want:  "gh://org/repo/my-skill@v1.2.3?token=PARTNER_TOKEN",
		},
		{
			name:    "gh shorthand too few segments",
			input:   "gh://org/repo",
			wantErr: "expected gh://owner/repo/skill-name",
		},
		{
			name:    "gh shorthand too many segments",
			input:   "gh://org/repo/skills/my-skill",
			wantErr: "expected gh://owner/repo/skill-name",
		},
		{
			name:    "gh shorthand empty skill name",
			input:   "gh://org/repo/@main",
			wantErr: "empty path component",
		},
		{
			name:    "gh shorthand invalid owner chars",
			input:   "gh://org name/repo/skill",
			wantErr: "invalid owner",
		},
		{
			name:    "gh shorthand dot as owner rejected",
			input:   "gh://./repo/skill",
			wantErr: "invalid owner",
		},
		{
			name:    "gh shorthand dotdot as owner rejected",
			input:   "gh://../repo/skill",
			wantErr: "invalid owner",
		},
		{
			name:    "gh shorthand dot as repo rejected",
			input:   "gh://org/./skill",
			wantErr: "invalid repo",
		},
		{
			name:    "gh shorthand dotdot as repo rejected",
			input:   "gh://org/../skill",
			wantErr: "invalid repo",
		},
		{
			name:    "gh shorthand dotdot in skill",
			input:   "gh://org/repo/..evil",
			wantErr: "invalid skill name",
		},
		{
			name:    "gh shorthand bad token name",
			input:   "gh://org/repo/skill?token=lowercase",
			wantErr: "must match [A-Z][A-Z0-9_]*",
		},
		{
			name:    "gh shorthand empty token value",
			input:   "gh://org/repo/skill?token=",
			wantErr: "empty ?token= value",
		},
		{
			name:    "gh shorthand unsupported query param",
			input:   "gh://org/repo/skill?foo=bar",
			wantErr: "unsupported query parameter",
		},
		{
			name:    "gh shorthand empty ref after @",
			input:   "gh://org/repo/skill@",
			wantErr: "empty ref after @",
		},

		// ── GitHub tree URL → gh:// shorthand ───────────────────────────────
		{
			name:  "github tree skills/skill-name",
			input: "https://github.com/scion-frontiers/scion-repo-contrib/tree/main/skills/harness-qa",
			want:  "gh://scion-frontiers/scion-repo-contrib/harness-qa@main",
		},
		{
			name:  "github tree with sha ref",
			input: "https://github.com/org/repo/tree/a1b2c3d/skills/my-skill",
			want:  "gh://org/repo/my-skill@a1b2c3d",
		},
		{
			name:  "github tree with token param",
			input: "https://github.com/org/repo/tree/main/skills/my-skill?token=SKILLS_TOKEN",
			want:  "gh://org/repo/my-skill@main?token=SKILLS_TOKEN",
		},
		{
			name:  "github tree http scheme normalized to https",
			input: "http://github.com/org/repo/tree/main/skills/my-skill",
			want:  "gh://org/repo/my-skill@main",
		},

		// ── GitHub tree URL, non-standard path → full URL kept ───────────────
		{
			name:  "github tree non-skills prefix kept as full url",
			input: "https://github.com/org/repo/tree/main/contrib/deep/my-skill",
			want:  "https://github.com/org/repo/tree/main/contrib/deep/my-skill",
		},
		{
			name:  "github tree single-segment non-skills path kept",
			input: "https://github.com/org/repo/tree/main/my-skill",
			want:  "https://github.com/org/repo/tree/main/my-skill",
		},

		// ── GitHub blob URL → strip filename → apply tree rules ─────────────
		{
			name:  "github blob skills SKILL.md",
			input: "https://github.com/org/repo/blob/main/skills/my-skill/SKILL.md",
			want:  "gh://org/repo/my-skill@main",
		},
		{
			name:  "github blob skills README.md",
			input: "https://github.com/org/repo/blob/main/skills/my-skill/README.md",
			want:  "gh://org/repo/my-skill@main",
		},
		{
			name:  "github blob skills Makefile (no extension — still strips)",
			input: "https://github.com/org/repo/blob/main/skills/my-skill/Makefile",
			want:  "gh://org/repo/my-skill@main",
		},
		{
			name:  "github blob non-standard path kept as tree url",
			input: "https://github.com/org/repo/blob/main/contrib/my-skill/SKILL.md",
			want:  "https://github.com/org/repo/tree/main/contrib/my-skill",
		},
		{
			name:    "github blob root-level file (no parent dir)",
			input:   "https://github.com/org/repo/blob/main/SKILL.md",
			wantErr: "cannot determine skill directory",
		},

		// ── GitHub URL error cases ───────────────────────────────────────────
		{
			name:    "github bare repo URL",
			input:   "https://github.com/org/repo",
			wantErr: "expected https://github.com/owner/repo/tree/ref/path",
		},
		{
			name:    "github repo with ref but no skill path",
			input:   "https://github.com/org/repo/tree/main",
			wantErr: "missing skill path",
		},
		{
			name:    "github unsupported path segment",
			input:   "https://github.com/org/repo/issues/123",
			wantErr: "expected /tree/ or /blob/",
		},
		{
			name:    "github pull request URL",
			input:   "https://github.com/org/repo/pull/42",
			wantErr: "expected /tree/ or /blob/",
		},
		{
			name:    "github dotdot path traversal",
			input:   "https://github.com/org/repo/tree/main/skills/../secret",
			wantErr: "must not contain '..'",
		},
		{
			name:    "github dot as owner rejected",
			input:   "https://github.com/./repo/tree/main/skills/my-skill",
			wantErr: "invalid owner",
		},
		{
			name:    "github dotdot as owner rejected",
			input:   "https://github.com/../repo/tree/main/skills/my-skill",
			wantErr: "invalid owner",
		},
		{
			name:    "github dot as repo rejected",
			input:   "https://github.com/org/./tree/main/skills/my-skill",
			wantErr: "invalid repo",
		},
		{
			name:    "github dotdot as repo rejected",
			input:   "https://github.com/org/../tree/main/skills/my-skill",
			wantErr: "invalid repo",
		},
		{
			name:    "github dot as ref rejected",
			input:   "https://github.com/org/repo/tree/./skills/my-skill",
			wantErr: "invalid ref",
		},
		{
			name:    "github dotdot as ref rejected",
			input:   "https://github.com/org/repo/tree/../skills/my-skill",
			wantErr: "invalid ref",
		},

		// ── scion:// rejected ────────────────────────────────────────────────
		{
			name:    "scion scheme rejected",
			input:   "scion://my-skill",
			wantErr: "scion:// is not a supported scheme",
		},
		{
			name:    "scion scheme with path rejected",
			input:   "scion://core/my-skill",
			wantErr: "scion:// is not a supported scheme",
		},

		// ── skill:// passthrough ─────────────────────────────────────────────
		{
			name:  "skill with registry and scope",
			input: "skill://scion/core/my-skill",
			want:  "skill://scion/core/my-skill",
		},
		{
			name:  "skill alias form",
			input: "skill://project/my-skill",
			want:  "skill://project/my-skill",
		},
		{
			name:    "skill:// with only one segment treated as registry missing name",
			input:   "skill://my-skill",
			wantErr: "missing skill name",
		},

		// ── bare name passthrough ────────────────────────────────────────────
		{
			name:  "bare skill name",
			input: "my-skill",
			want:  "my-skill",
		},
		{
			name:  "bare skill name with digits",
			input: "security-audit-v2",
			want:  "security-audit-v2",
		},
		{
			name:    "bare name with uppercase rejected",
			input:   "MySkill",
			wantErr: "kebab-case",
		},

		// ── unknown schemes ──────────────────────────────────────────────────
		{
			name:    "gcp-skill scheme rejected",
			input:   "gcp-skill://something",
			wantErr: "unsupported scheme",
		},
		{
			name:    "ftp scheme rejected",
			input:   "ftp://example.com/something",
			wantErr: "unsupported scheme",
		},

		// ── whitespace trimming ──────────────────────────────────────────────
		{
			name:  "leading/trailing whitespace trimmed",
			input: "  gh://org/repo/skill@main  ",
			want:  "gh://org/repo/skill@main",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: "must not be empty",
		},
		{
			name:    "whitespace only",
			input:   "   ",
			wantErr: "must not be empty",
		},

		// ── idempotency ──────────────────────────────────────────────────────
		{
			name:  "canonical gh:// is idempotent",
			input: "gh://org/repo/skill@main",
			want:  "gh://org/repo/skill@main",
		},
		{
			name:  "canonical skill:// is idempotent",
			input: "skill://scion/core/my-skill",
			want:  "skill://scion/core/my-skill",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSkillURI(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeSkillURI(%q) = %q, want error containing %q", tt.input, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("NormalizeSkillURI(%q) error = %q, want it to contain %q", tt.input, err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSkillURI(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeSkillURI(%q) = %q, want %q", tt.input, got, tt.want)
			}

			// Idempotency check: normalizing the output again must produce the same result.
			got2, err2 := NormalizeSkillURI(got)
			if err2 != nil {
				t.Errorf("NormalizeSkillURI(output=%q) round-trip error: %v", got, err2)
			} else if got2 != got {
				t.Errorf("NormalizeSkillURI is not idempotent: NormalizeSkillURI(%q) = %q, then NormalizeSkillURI(%q) = %q", tt.input, got, got, got2)
			}
		})
	}
}
