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

package storage

import "testing"

func TestTemplateStoragePath(t *testing.T) {
	tests := []struct {
		name         string
		scope        string
		scopeID      string
		templateSlug string
		want         string
	}{
		{
			name:         "global scope",
			scope:        "global",
			scopeID:      "",
			templateSlug: "my-template",
			want:         "templates/global/my-template",
		},
		{
			name:         "grove scope",
			scope:        "grove",
			scopeID:      "grove-123",
			templateSlug: "my-template",
			want:         "templates/groves/grove-123/my-template",
		},
		{
			name:         "user scope",
			scope:        "user",
			scopeID:      "user-456",
			templateSlug: "my-template",
			want:         "templates/users/user-456/my-template",
		},
		{
			name:         "default scope",
			scope:        "unknown",
			scopeID:      "",
			templateSlug: "my-template",
			want:         "templates/my-template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TemplateStoragePath(tt.scope, tt.scopeID, tt.templateSlug)
			if got != tt.want {
				t.Errorf("TemplateStoragePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTemplateStorageURI(t *testing.T) {
	bucket := "my-bucket"
	uri := TemplateStorageURI(bucket, "grove", "grove-123", "my-template")
	want := "gs://my-bucket/templates/groves/grove-123/my-template/"
	if uri != want {
		t.Errorf("TemplateStorageURI() = %q, want %q", uri, want)
	}
}

func TestResourceStoragePath(t *testing.T) {
	tests := []struct {
		name    string
		kind    ResourceKind
		scope   string
		scopeID string
		slug    string
		want    string
	}{
		{"template global", ResourceKindTemplate, "global", "", "t1", "templates/global/t1"},
		{"template project", ResourceKindTemplate, "project", "p-1", "t1", "templates/groves/p-1/t1"},
		{"template grove (legacy)", ResourceKindTemplate, "grove", "g-1", "t1", "templates/groves/g-1/t1"},
		{"template user", ResourceKindTemplate, "user", "u-1", "t1", "templates/users/u-1/t1"},
		{"template default", ResourceKindTemplate, "weird", "", "t1", "templates/t1"},
		{"harness-config global", ResourceKindHarnessConfig, "global", "", "h1", "harness-configs/global/h1"},
		{"harness-config project", ResourceKindHarnessConfig, "project", "p-1", "h1", "harness-configs/groves/p-1/h1"},
		{"harness-config grove (legacy)", ResourceKindHarnessConfig, "grove", "g-1", "h1", "harness-configs/groves/g-1/h1"},
		{"harness-config user", ResourceKindHarnessConfig, "user", "u-1", "h1", "harness-configs/users/u-1/h1"},
		{"harness-config default", ResourceKindHarnessConfig, "weird", "", "h1", "harness-configs/h1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResourceStoragePath(tt.kind, tt.scope, tt.scopeID, tt.slug); got != tt.want {
				t.Errorf("ResourceStoragePath(%q, %q, %q, %q) = %q, want %q",
					tt.kind, tt.scope, tt.scopeID, tt.slug, got, tt.want)
			}
		})
	}
}

// TestResourceStoragePathWrappers pins the legacy per-kind path/URI helpers to
// the shared kind-keyed implementation, so the refactor that made them thin
// wrappers cannot silently change the on-storage layout for either kind.
func TestResourceStoragePathWrappers(t *testing.T) {
	const bucket = "b"
	cases := []struct{ scope, scopeID, slug string }{
		{"global", "", "x"},
		{"project", "p-1", "x"},
		{"grove", "g-1", "x"},
		{"user", "u-1", "x"},
		{"weird", "", "x"},
	}
	for _, c := range cases {
		if got, want := TemplateStoragePath(c.scope, c.scopeID, c.slug),
			ResourceStoragePath(ResourceKindTemplate, c.scope, c.scopeID, c.slug); got != want {
			t.Errorf("TemplateStoragePath(%q,%q,%q) = %q, want %q", c.scope, c.scopeID, c.slug, got, want)
		}
		if got, want := HarnessConfigStoragePath(c.scope, c.scopeID, c.slug),
			ResourceStoragePath(ResourceKindHarnessConfig, c.scope, c.scopeID, c.slug); got != want {
			t.Errorf("HarnessConfigStoragePath(%q,%q,%q) = %q, want %q", c.scope, c.scopeID, c.slug, got, want)
		}
		if got, want := TemplateStorageURI(bucket, c.scope, c.scopeID, c.slug),
			ResourceStorageURI(bucket, ResourceKindTemplate, c.scope, c.scopeID, c.slug); got != want {
			t.Errorf("TemplateStorageURI = %q, want %q", got, want)
		}
		if got, want := HarnessConfigStorageURI(bucket, c.scope, c.scopeID, c.slug),
			ResourceStorageURI(bucket, ResourceKindHarnessConfig, c.scope, c.scopeID, c.slug); got != want {
			t.Errorf("HarnessConfigStorageURI = %q, want %q", got, want)
		}
	}
}

func TestWorkspaceStoragePath(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		agentID   string
		want      string
	}{
		{
			name:      "basic path",
			projectID: "grove-abc",
			agentID:   "agent-123",
			want:      "workspaces/grove-abc/agent-123",
		},
		{
			name:      "with special characters in IDs",
			projectID: "grove_xyz",
			agentID:   "agent_456",
			want:      "workspaces/grove_xyz/agent_456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceStoragePath(tt.projectID, tt.agentID)
			if got != tt.want {
				t.Errorf("WorkspaceStoragePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkspaceStorageURI(t *testing.T) {
	tests := []struct {
		name      string
		bucket    string
		projectID string
		agentID   string
		want      string
	}{
		{
			name:      "basic URI",
			bucket:    "scion-hub-dev",
			projectID: "grove-abc",
			agentID:   "agent-123",
			want:      "gs://scion-hub-dev/workspaces/grove-abc/agent-123/",
		},
		{
			name:      "production bucket",
			bucket:    "scion-hub-prod",
			projectID: "grove-xyz",
			agentID:   "agent-456",
			want:      "gs://scion-hub-prod/workspaces/grove-xyz/agent-456/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceStorageURI(tt.bucket, tt.projectID, tt.agentID)
			if got != tt.want {
				t.Errorf("WorkspaceStorageURI() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectWorkspaceStoragePath(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		want      string
	}{
		{
			name:      "basic grove path",
			projectID: "grove-abc",
			want:      "workspaces/grove-abc/grove-workspace",
		},
		{
			name:      "with UUID grove ID",
			projectID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			want:      "workspaces/a1b2c3d4-e5f6-7890-abcd-ef1234567890/grove-workspace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProjectWorkspaceStoragePath(tt.projectID)
			if got != tt.want {
				t.Errorf("ProjectWorkspaceStoragePath() = %q, want %q", got, tt.want)
			}
		})
	}
}
