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
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

type mockSchemeResolver struct {
	name     string
	resolved []ResolvedSkill
	errors   []ResolveError
	hardErr  error
	called   []api.SkillReference
}

func (m *mockSchemeResolver) ResolverName() string { return m.name }
func (m *mockSchemeResolver) Resolve(_ context.Context, refs []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	m.called = append(m.called, refs...)
	if m.hardErr != nil {
		return nil, m.hardErr
	}
	return &ResolveResult{Resolved: m.resolved, Errors: m.errors}, nil
}

func TestDetectScheme(t *testing.T) {
	tests := []struct {
		uri    string
		scheme string
	}{
		{"gh://owner/repo/skill", "gh"},
		{"gh://owner/repo/skill@v1.0", "gh"},
		{"gcp-skill://alias/SKILL_ID", "gcp-skill"},
		{"https://github.com/owner/repo/tree/main/skills/s", "gh"},
		{"http://github.com/owner/repo/tree/main/skills/s", "gh"},
		{"skill://scion/core/my-skill", "skill"},
		{"skill://scion/core/my-skill@1.0", "skill"},
		{"my-skill", "skill"},
		{"code-review", "skill"},
		{"ftp://example.com/skill", "ftp"},
		{"", "skill"},
	}
	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			got := detectScheme(tt.uri)
			if got != tt.scheme {
				t.Errorf("detectScheme(%q) = %q, want %q", tt.uri, got, tt.scheme)
			}
		})
	}
}

func TestRoutingSkillResolver_FallbackRouting(t *testing.T) {
	hub := &mockSchemeResolver{
		name: "hub",
		resolved: []ResolvedSkill{
			{Name: "my-skill", URI: "skill://scion/core/my-skill"},
		},
	}
	router := NewRoutingSkillResolver(hub)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "skill://scion/core/my-skill"},
		{URI: "my-skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hub.called) != 2 {
		t.Errorf("hub received %d refs, want 2", len(hub.called))
	}
	if len(result.Resolved) != 1 {
		t.Errorf("got %d resolved, want 1", len(result.Resolved))
	}
}

func TestRoutingSkillResolver_SchemeDispatch(t *testing.T) {
	hub := &mockSchemeResolver{name: "hub"}
	ghMock := &mockSchemeResolver{
		name:     "gh",
		resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
	}
	router := NewRoutingSkillResolver(hub)
	router.Register("gh", ghMock)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ghMock.called) != 1 {
		t.Fatalf("gh mock received %d refs, want 1", len(ghMock.called))
	}
	if ghMock.called[0].URI != "gh://owner/repo/skill" {
		t.Errorf("gh mock got URI %q, want %q", ghMock.called[0].URI, "gh://owner/repo/skill")
	}
	if len(hub.called) != 0 {
		t.Errorf("hub received %d refs, want 0", len(hub.called))
	}
	if len(result.Resolved) != 1 || result.Resolved[0].Name != "gh-skill" {
		t.Errorf("unexpected resolved result: %+v", result.Resolved)
	}
}

func TestRoutingSkillResolver_MixedBatch(t *testing.T) {
	hub := &mockSchemeResolver{
		name:     "hub",
		resolved: []ResolvedSkill{{Name: "hub-skill", URI: "skill://scion/core/hub-skill"}},
	}
	ghMock := &mockSchemeResolver{
		name:     "gh",
		resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
	}
	router := NewRoutingSkillResolver(hub)
	router.Register("gh", ghMock)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "skill://scion/core/hub-skill"},
		{URI: "gh://owner/repo/skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hub.called) != 1 {
		t.Errorf("hub received %d refs, want 1", len(hub.called))
	}
	if len(ghMock.called) != 1 {
		t.Errorf("gh mock received %d refs, want 1", len(ghMock.called))
	}
	if len(result.Resolved) != 2 {
		t.Errorf("got %d resolved, want 2", len(result.Resolved))
	}
}

func TestRoutingSkillResolver_UnsupportedScheme(t *testing.T) {
	hub := &mockSchemeResolver{name: "hub"}
	router := NewRoutingSkillResolver(hub)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "foo://bar"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if result.Errors[0].Code != "unsupported_scheme" {
		t.Errorf("error code = %q, want %q", result.Errors[0].Code, "unsupported_scheme")
	}
}

func TestRoutingSkillResolver_NilFallback(t *testing.T) {
	router := NewRoutingSkillResolver(nil)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "skill://scion/core/my-skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if result.Errors[0].Code != "unsupported_scheme" {
		t.Errorf("error code = %q, want %q", result.Errors[0].Code, "unsupported_scheme")
	}
}

func TestRoutingSkillResolver_HardErrorPropagation(t *testing.T) {
	hub := &mockSchemeResolver{
		name:    "hub",
		hardErr: fmt.Errorf("connection refused"),
	}
	router := NewRoutingSkillResolver(hub)

	_, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "my-skill"},
	}, ResolveOpts{})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != `resolver for scheme "skill" failed: connection refused` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestRoutingSkillResolver_EmptyRefs(t *testing.T) {
	hub := &mockSchemeResolver{name: "hub"}
	router := NewRoutingSkillResolver(hub)

	result, err := router.Resolve(context.Background(), nil, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Resolved) != 0 {
		t.Errorf("got %d resolved, want 0", len(result.Resolved))
	}
	if len(result.Errors) != 0 {
		t.Errorf("got %d errors, want 0", len(result.Errors))
	}
}

func TestRoutingSkillResolver_ResolverName(t *testing.T) {
	router := NewRoutingSkillResolver(nil)
	if got := router.ResolverName(); got != "routing" {
		t.Errorf("ResolverName() = %q, want %q", got, "routing")
	}
}

func TestRoutingSkillResolver_RegisterPanics(t *testing.T) {
	router := NewRoutingSkillResolver(nil)

	t.Run("empty scheme", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for empty scheme")
			}
		}()
		router.Register("", &mockSchemeResolver{})
	})

	t.Run("duplicate scheme", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for duplicate scheme")
			}
		}()
		r2 := NewRoutingSkillResolver(nil)
		r2.Register("gh", &mockSchemeResolver{})
		r2.Register("gh", &mockSchemeResolver{})
	})
}

func TestRoutingSkillResolver_GitHubFullURL(t *testing.T) {
	hub := &mockSchemeResolver{name: "hub"}
	ghMock := &mockSchemeResolver{
		name:     "gh",
		resolved: []ResolvedSkill{{Name: "gh-skill", URI: "https://github.com/owner/repo/tree/main/skills/s"}},
	}
	router := NewRoutingSkillResolver(hub)
	router.Register("gh", ghMock)

	_, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "https://github.com/owner/repo/tree/main/skills/s"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ghMock.called) != 1 {
		t.Errorf("gh mock received %d refs, want 1", len(ghMock.called))
	}
	if len(hub.called) != 0 {
		t.Errorf("hub received %d refs, want 0", len(hub.called))
	}
}
