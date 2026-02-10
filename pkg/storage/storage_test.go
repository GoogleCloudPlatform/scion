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

func TestWorkspaceStoragePath(t *testing.T) {
	tests := []struct {
		name    string
		groveID string
		agentID string
		want    string
	}{
		{
			name:    "basic path",
			groveID: "grove-abc",
			agentID: "agent-123",
			want:    "workspaces/grove-abc/agent-123",
		},
		{
			name:    "with special characters in IDs",
			groveID: "grove_xyz",
			agentID: "agent_456",
			want:    "workspaces/grove_xyz/agent_456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceStoragePath(tt.groveID, tt.agentID)
			if got != tt.want {
				t.Errorf("WorkspaceStoragePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkspaceStorageURI(t *testing.T) {
	tests := []struct {
		name    string
		bucket  string
		groveID string
		agentID string
		want    string
	}{
		{
			name:    "basic URI",
			bucket:  "scion-hub-dev",
			groveID: "grove-abc",
			agentID: "agent-123",
			want:    "gs://scion-hub-dev/workspaces/grove-abc/agent-123/",
		},
		{
			name:    "production bucket",
			bucket:  "scion-hub-prod",
			groveID: "grove-xyz",
			agentID: "agent-456",
			want:    "gs://scion-hub-prod/workspaces/grove-xyz/agent-456/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceStorageURI(tt.bucket, tt.groveID, tt.agentID)
			if got != tt.want {
				t.Errorf("WorkspaceStorageURI() = %q, want %q", got, tt.want)
			}
		})
	}
}
