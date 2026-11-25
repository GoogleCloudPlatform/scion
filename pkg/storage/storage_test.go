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

import (
	"context"
	"strings"
	"testing"
)

func TestTemplateStoragePath(t *testing.T) {
	tests := []struct {
		name         string
		hubID        string
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
			name:         "project scope",
			scope:        "project",
			scopeID:      "project-123",
			templateSlug: "my-template",
			want:         "templates/groves/project-123/my-template",
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
		{
			name:         "hub-scoped global",
			hubID:        "my-hub",
			scope:        "global",
			scopeID:      "",
			templateSlug: "my-template",
			want:         "hubs/my-hub/templates/global/my-template",
		},
		{
			name:         "hub-scoped project",
			hubID:        "my-hub",
			scope:        "project",
			scopeID:      "project-123",
			templateSlug: "my-template",
			want:         "hubs/my-hub/templates/groves/project-123/my-template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TemplateStoragePath(tt.hubID, tt.scope, tt.scopeID, tt.templateSlug)
			if got != tt.want {
				t.Errorf("TemplateStoragePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTemplateStorageURI(t *testing.T) {
	bucket := "my-bucket"
	uri := TemplateStorageURI("", bucket, "project", "project-123", "my-template")
	want := "gs://my-bucket/templates/groves/project-123/my-template/"
	if uri != want {
		t.Errorf("TemplateStorageURI() = %q, want %q", uri, want)
	}

	uri = TemplateStorageURI("my-hub", bucket, "project", "project-123", "my-template")
	want = "gs://my-bucket/hubs/my-hub/templates/groves/project-123/my-template/"
	if uri != want {
		t.Errorf("TemplateStorageURI(hub-scoped) = %q, want %q", uri, want)
	}
}

func TestStorageURIForPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"plain path", "templates/global/my-template", "gs://my-bucket/templates/global/my-template/"},
		{"one trailing slash", "templates/global/my-template/", "gs://my-bucket/templates/global/my-template/"},
		{"multiple trailing slashes", "templates/global/my-template///", "gs://my-bucket/templates/global/my-template/"},
		// No caller produces an empty or all-slash path; these rows pin current behaviour.
		{"empty path", "", "gs://my-bucket//"},
		{"all-slash path", "///", "gs://my-bucket//"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StorageURIForPath("my-bucket", tt.path); got != tt.want {
				t.Errorf("StorageURIForPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}

	// ResourceStorageURI computes its path internally and never produces a
	// trailing slash on the slug, so the trim must be a no-op for it: normal
	// inputs keep producing exactly the same URI as before.
	t.Run("ResourceStorageURI unchanged for normal inputs", func(t *testing.T) {
		got := ResourceStorageURI("", "my-bucket", ResourceKindTemplate, "project", "grove-123", "my-template")
		want := "gs://my-bucket/templates/groves/grove-123/my-template/"
		if got != want {
			t.Errorf("ResourceStorageURI() = %q, want %q", got, want)
		}
	})
}

func TestResourceStoragePath(t *testing.T) {
	tests := []struct {
		name    string
		hubID   string
		kind    ResourceKind
		scope   string
		scopeID string
		slug    string
		want    string
	}{
		{"template global", "", ResourceKindTemplate, "global", "", "t1", "templates/global/t1"},
		{"template project", "", ResourceKindTemplate, "project", "p-1", "t1", "templates/groves/p-1/t1"},
		{"template user", "", ResourceKindTemplate, "user", "u-1", "t1", "templates/users/u-1/t1"},
		{"template default", "", ResourceKindTemplate, "weird", "", "t1", "templates/t1"},
		{"harness-config global", "", ResourceKindHarnessConfig, "global", "", "h1", "harness-configs/global/h1"},
		{"harness-config project", "", ResourceKindHarnessConfig, "project", "p-1", "h1", "harness-configs/groves/p-1/h1"},
		{"harness-config user", "", ResourceKindHarnessConfig, "user", "u-1", "h1", "harness-configs/users/u-1/h1"},
		{"harness-config default", "", ResourceKindHarnessConfig, "weird", "", "h1", "harness-configs/h1"},
		{"hub-scoped template global", "hub-1", ResourceKindTemplate, "global", "", "t1", "hubs/hub-1/templates/global/t1"},
		{"hub-scoped template project", "hub-1", ResourceKindTemplate, "project", "p-1", "t1", "hubs/hub-1/templates/groves/p-1/t1"},
		{"hub-scoped harness-config global", "hub-1", ResourceKindHarnessConfig, "global", "", "h1", "hubs/hub-1/harness-configs/global/h1"},
		{"hub-scoped harness-config user", "hub-1", ResourceKindHarnessConfig, "user", "u-1", "h1", "hubs/hub-1/harness-configs/users/u-1/h1"},
		{"hub-scoped skill project", "hub-1", ResourceKindSkill, "project", "g-1", "s1", "hubs/hub-1/skills/groves/g-1/s1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResourceStoragePath(tt.hubID, tt.kind, tt.scope, tt.scopeID, tt.slug); got != tt.want {
				t.Errorf("ResourceStoragePath(%q, %q, %q, %q, %q) = %q, want %q",
					tt.hubID, tt.kind, tt.scope, tt.scopeID, tt.slug, got, tt.want)
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
	for _, hubID := range []string{"", "my-hub"} {
		for _, c := range cases {
			if got, want := TemplateStoragePath(hubID, c.scope, c.scopeID, c.slug),
				ResourceStoragePath(hubID, ResourceKindTemplate, c.scope, c.scopeID, c.slug); got != want {
				t.Errorf("TemplateStoragePath(%q,%q,%q,%q) = %q, want %q", hubID, c.scope, c.scopeID, c.slug, got, want)
			}
			if got, want := HarnessConfigStoragePath(hubID, c.scope, c.scopeID, c.slug),
				ResourceStoragePath(hubID, ResourceKindHarnessConfig, c.scope, c.scopeID, c.slug); got != want {
				t.Errorf("HarnessConfigStoragePath(%q,%q,%q,%q) = %q, want %q", hubID, c.scope, c.scopeID, c.slug, got, want)
			}
			if got, want := TemplateStorageURI(hubID, bucket, c.scope, c.scopeID, c.slug),
				ResourceStorageURI(hubID, bucket, ResourceKindTemplate, c.scope, c.scopeID, c.slug); got != want {
				t.Errorf("TemplateStorageURI = %q, want %q", got, want)
			}
			if got, want := HarnessConfigStorageURI(hubID, bucket, c.scope, c.scopeID, c.slug),
				ResourceStorageURI(hubID, bucket, ResourceKindHarnessConfig, c.scope, c.scopeID, c.slug); got != want {
				t.Errorf("HarnessConfigStorageURI = %q, want %q", got, want)
			}
		}
	}
}

func TestWorkspaceStoragePath(t *testing.T) {
	tests := []struct {
		name      string
		hubID     string
		projectID string
		agentID   string
		want      string
	}{
		{
			name:      "basic path",
			projectID: "project-abc",
			agentID:   "agent-123",
			want:      "workspaces/project-abc/agent-123",
		},
		{
			name:      "with special characters in IDs",
			projectID: "project_xyz",
			agentID:   "agent_456",
			want:      "workspaces/project_xyz/agent_456",
		},
		{
			name:      "hub-scoped path",
			hubID:     "my-hub",
			projectID: "project-abc",
			agentID:   "agent-123",
			want:      "hubs/my-hub/workspaces/project-abc/agent-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceStoragePath(tt.hubID, tt.projectID, tt.agentID)
			if got != tt.want {
				t.Errorf("WorkspaceStoragePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkspaceStorageURI(t *testing.T) {
	tests := []struct {
		name      string
		hubID     string
		bucket    string
		projectID string
		agentID   string
		want      string
	}{
		{
			name:      "basic URI",
			bucket:    "scion-hub-dev",
			projectID: "project-abc",
			agentID:   "agent-123",
			want:      "gs://scion-hub-dev/workspaces/project-abc/agent-123/",
		},
		{
			name:      "production bucket",
			bucket:    "scion-hub-prod",
			projectID: "project-xyz",
			agentID:   "agent-456",
			want:      "gs://scion-hub-prod/workspaces/project-xyz/agent-456/",
		},
		{
			name:      "hub-scoped URI",
			hubID:     "my-hub",
			bucket:    "scion-hub-dev",
			projectID: "project-abc",
			agentID:   "agent-123",
			want:      "gs://scion-hub-dev/hubs/my-hub/workspaces/project-abc/agent-123/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceStorageURI(tt.hubID, tt.bucket, tt.projectID, tt.agentID)
			if got != tt.want {
				t.Errorf("WorkspaceStorageURI() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDirPrefix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hubs/h/templates/global/foo", "hubs/h/templates/global/foo/"},
		{"hubs/h/templates/global/foo/", "hubs/h/templates/global/foo/"},
		{"hubs/h/templates/global/foo//", "hubs/h/templates/global/foo/"},
		// Never "/": GCS trims the leading "/" and would match the whole
		// bucket. "" makes DeletePrefix fail with ErrInvalidPath instead.
		{"", ""},
		{"/", ""},
		{"//", ""},
	} {
		if got := DirPrefix(tc.in); got != tc.want {
			t.Errorf("DirPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLocalListPages: MaxResults and StartOffset page through a prefix in
// order, each object once.
func TestLocalListPages(t *testing.T) {
	s, err := NewLocal(Config{Provider: ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	want := []string{"p/a/1", "p/a/2", "p/b/1", "p/c", "p/d/e/f"}
	for _, name := range append([]string{"q/x"}, want...) {
		if _, err := s.Upload(ctx, name, strings.NewReader(name), UploadOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	offset := ""
	for range 10 {
		res, err := s.List(ctx, ListOptions{Prefix: "p/", MaxResults: 2, StartOffset: offset})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range res.Objects {
			got = append(got, o.Name)
		}
		if res.NextOffset == "" {
			break
		}
		offset = res.NextOffset
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pages = %v, want %v", got, want)
	}
}

// TestLocalListDelimiterPrefixes verifies that LocalStorage.List with
// Delimiter="/" returns child directory prefixes, honors MaxResults, and
// pages cleanly via StartOffset/NextOffset.
func TestLocalListDelimiterPrefixes(t *testing.T) {
	s, err := NewLocal(Config{Provider: ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, name := range []string{
		"hubs/h1/move-bundles/rid-a/manifest.json",
		"hubs/h1/move-bundles/rid-a/workspace/part-00000.tgz",
		"hubs/h1/move-bundles/rid-b/manifest.json",
		"hubs/h1/move-bundles/rid-c/home/part-00000.tgz",
		"hubs/h1/move-bundles/rid-d/manifest.json",
		"hubs/h1/other/x",
	} {
		if _, err := s.Upload(ctx, name, strings.NewReader(name), UploadOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	root := DirPrefix(MoveBundlesRoot("h1"))
	var got []string
	offset := ""
	for range 10 {
		res, err := s.List(ctx, ListOptions{
			Prefix:      root,
			Delimiter:   "/",
			MaxResults:  2,
			StartOffset: offset,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Objects) != 0 {
			t.Fatalf("expected no direct objects, got %v", res.Objects)
		}
		if len(res.Prefixes) > 2 {
			t.Fatalf("MaxResults=2 exceeded: got %d prefixes (%v)", len(res.Prefixes), res.Prefixes)
		}
		got = append(got, res.Prefixes...)
		if res.NextOffset == "" {
			break
		}
		offset = res.NextOffset
	}

	want := []string{
		"hubs/h1/move-bundles/rid-a/",
		"hubs/h1/move-bundles/rid-b/",
		"hubs/h1/move-bundles/rid-c/",
		"hubs/h1/move-bundles/rid-d/",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("prefixes = %v, want %v", got, want)
	}
}

// ResourceRootPaths covers every path ResourceStoragePath gives a template,
// in every scope and with or without a hub namespace, and no workspace path.
func TestResourceRootPaths(t *testing.T) {
	under := func(p string, roots []string) bool {
		for _, r := range roots {
			if strings.HasPrefix(p, r+"/") {
				return true
			}
		}
		return false
	}
	if got := ResourceRootPaths("", ResourceKindTemplate); len(got) != 1 || got[0] != "templates" {
		t.Fatalf("ResourceRootPaths(\"\", template) = %v, want [templates]", got)
	}
	if got := ResourceRootPaths("hub-1", ResourceKindTemplate); len(got) != 2 || got[0] != "templates" || got[1] != "hubs/hub-1/templates" {
		t.Fatalf("ResourceRootPaths(hub-1, template) = %v, want [templates hubs/hub-1/templates]", got)
	}
	for _, hubID := range []string{"", "hub-1"} {
		roots := ResourceRootPaths(hubID, ResourceKindTemplate)
		for _, scope := range []string{"global", "project", "user", "weird"} {
			if p := TemplateStoragePath(hubID, scope, "s-1", "t1"); !under(p, roots) {
				t.Errorf("template path %q is not under %v", p, roots)
			}
		}
		mbPath, err := MoveBundlePath(hubID, "reinc-1")
		if err != nil {
			t.Fatalf("MoveBundlePath: %v", err)
		}
		for _, p := range []string{
			WorkspaceStoragePath(hubID, "p-1", "a-1"),
			HarnessConfigStoragePath(hubID, "global", "", "h1"),
			SkillStoragePath(hubID, "global", "", "s1"),
			mbPath,
		} {
			if under(p, roots) {
				t.Errorf("non-template path %q is under the template roots %v", p, roots)
			}
		}
	}
}

func TestMoveBundlePaths(t *testing.T) {
	if got, want := MoveBundlesRoot(""), "move-bundles"; got != want {
		t.Errorf("MoveBundlesRoot(\"\") = %q, want %q", got, want)
	}
	if got, want := MoveBundlesRoot("hub-1"), "hubs/hub-1/move-bundles"; got != want {
		t.Errorf("MoveBundlesRoot(hub-1) = %q, want %q", got, want)
	}
	if got, want := DirPrefix(MoveBundlesRoot("hub-1")), "hubs/hub-1/move-bundles/"; got != want {
		t.Errorf("DirPrefix(MoveBundlesRoot) = %q, want %q", got, want)
	}

	if got, err := MoveBundlePath("", "rid-1"); err != nil || got != "move-bundles/rid-1" {
		t.Errorf("MoveBundlePath(\"\", rid-1) = %q, err=%v; want move-bundles/rid-1", got, err)
	}
	if got, err := MoveBundlePath("hub-1", "rid-1"); err != nil || got != "hubs/hub-1/move-bundles/rid-1" {
		t.Errorf("MoveBundlePath(hub-1, rid-1) = %q, err=%v; want hubs/hub-1/move-bundles/rid-1", got, err)
	}
	if got, _ := MoveBundlePath("hub-1", "rid-1"); DirPrefix(got) != "hubs/hub-1/move-bundles/rid-1/" {
		t.Errorf("DirPrefix(MoveBundlePath) = %q, want hubs/hub-1/move-bundles/rid-1/", DirPrefix(got))
	}

	if got, err := MoveBundleManifestPath("hub-1", "rid-1"); err != nil || got != "hubs/hub-1/move-bundles/rid-1/manifest.json" {
		t.Errorf("MoveBundleManifestPath() = %q, err=%v", got, err)
	}
	if got, err := MoveBundleManifestPath("", "rid-1"); err != nil || got != "move-bundles/rid-1/manifest.json" {
		t.Errorf("MoveBundleManifestPath(un-namespaced) = %q, err=%v", got, err)
	}

	if got, err := MoveBundlePath("hub-1", "550e8400-e29b-41d4-a716-446655440000"); err != nil || got != "hubs/hub-1/move-bundles/550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("MoveBundlePath(UUID) = %q, err=%v", got, err)
	}

	for _, badRID := range []string{"", ".", "..", ".hidden", "a/b", `a\b`, "../x", "rid/..", "a..b", "rid with space"} {
		if _, err := MoveBundlePath("hub-1", badRID); err == nil {
			t.Errorf("MoveBundlePath(hub-1, %q) = nil error, want error", badRID)
		}
		if _, err := MoveBundleManifestPath("hub-1", badRID); err == nil {
			t.Errorf("MoveBundleManifestPath(hub-1, %q) = nil error, want error", badRID)
		}
		if _, err := MoveBundlePartPath("hub-1", badRID, MoveBundleRootWorkspace, 0); err == nil {
			t.Errorf("MoveBundlePartPath(hub-1, %q, workspace, 0) = nil error, want error", badRID)
		}
	}

	// Valid part paths
	validCases := []struct {
		hubID, rid, root string
		index            int
		want             string
	}{
		{"hub-1", "rid-1", MoveBundleRootWorkspace, 0, "hubs/hub-1/move-bundles/rid-1/workspace/part-00000.tgz"},
		{"hub-1", "rid-1", MoveBundleRootWorkspace, 42, "hubs/hub-1/move-bundles/rid-1/workspace/part-00042.tgz"},
		{"hub-1", "rid-1", MoveBundleRootHome, 12345, "hubs/hub-1/move-bundles/rid-1/home/part-12345.tgz"},
		{"", "rid-1", MoveBundleRootHome, 0, "move-bundles/rid-1/home/part-00000.tgz"},
	}
	for _, tc := range validCases {
		got, err := MoveBundlePartPath(tc.hubID, tc.rid, tc.root, tc.index)
		if err != nil {
			t.Errorf("MoveBundlePartPath(%q, %q, %q, %d) unexpected err: %v", tc.hubID, tc.rid, tc.root, tc.index, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MoveBundlePartPath(%q, %q, %q, %d) = %q, want %q", tc.hubID, tc.rid, tc.root, tc.index, got, tc.want)
		}
	}

	// Invalid part paths
	invalidCases := []struct {
		name             string
		hubID, rid, root string
		index            int
	}{
		{"invalid root", "hub-1", "rid-1", "other", 0},
		{"empty root", "hub-1", "rid-1", "", 0},
		{"uppercase root", "hub-1", "rid-1", "Workspace", 0},
		{"negative index", "hub-1", "rid-1", MoveBundleRootWorkspace, -1},
		{"empty rid", "hub-1", "", MoveBundleRootWorkspace, 0},
		{"slash in rid", "hub-1", "a/b", MoveBundleRootWorkspace, 0},
		{"dotdot in rid", "hub-1", "../x", MoveBundleRootWorkspace, 0},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MoveBundlePartPath(tc.hubID, tc.rid, tc.root, tc.index); err == nil {
				t.Errorf("MoveBundlePartPath(%q, %q, %q, %d) = nil error, want error", tc.hubID, tc.rid, tc.root, tc.index)
			}
		})
	}
}
