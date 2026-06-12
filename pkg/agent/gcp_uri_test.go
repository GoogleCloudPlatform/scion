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
	"testing"
)

func TestParseGCPSkillURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		want    *GCPSkillRef
		wantErr bool
	}{
		{
			name: "basic URI",
			uri:  "gcp-skill://team-skills/my-skill",
			want: &GCPSkillRef{
				Alias:   "team-skills",
				SkillID: "my-skill",
				Raw:     "gcp-skill://team-skills/my-skill",
			},
		},
		{
			name: "with version",
			uri:  "gcp-skill://team-skills/my-skill@v1",
			want: &GCPSkillRef{
				Alias:   "team-skills",
				SkillID: "my-skill",
				Version: "v1",
				Raw:     "gcp-skill://team-skills/my-skill@v1",
			},
		},
		{
			name: "different alias and skill ID",
			uri:  "gcp-skill://prod/skill-123-abc",
			want: &GCPSkillRef{
				Alias:   "prod",
				SkillID: "skill-123-abc",
				Raw:     "gcp-skill://prod/skill-123-abc",
			},
		},
		{
			name:    "missing skill ID",
			uri:     "gcp-skill://alias",
			wantErr: true,
		},
		{
			name:    "empty skill ID",
			uri:     "gcp-skill://alias/",
			wantErr: true,
		},
		{
			name:    "empty alias",
			uri:     "gcp-skill:///skill-id",
			wantErr: true,
		},
		{
			name:    "slashes in skill ID",
			uri:     "gcp-skill://alias/skill/extra",
			wantErr: true,
		},
		{
			name:    "empty version after @",
			uri:     "gcp-skill://alias/skill@",
			wantErr: true,
		},
		{
			name:    "not a gcp-skill URI",
			uri:     "gh://owner/repo/skill",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGCPSkillURI(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseGCPSkillURI(%q) = %+v, want error", tt.uri, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGCPSkillURI(%q) error: %v", tt.uri, err)
			}
			if got.Alias != tt.want.Alias {
				t.Errorf("Alias = %q, want %q", got.Alias, tt.want.Alias)
			}
			if got.SkillID != tt.want.SkillID {
				t.Errorf("SkillID = %q, want %q", got.SkillID, tt.want.SkillID)
			}
			if got.Version != tt.want.Version {
				t.Errorf("Version = %q, want %q", got.Version, tt.want.Version)
			}
			if got.Raw != tt.want.Raw {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.want.Raw)
			}
		})
	}
}
