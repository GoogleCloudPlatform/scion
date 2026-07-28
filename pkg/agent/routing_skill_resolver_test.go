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

func TestRoutingSkillResolver_RegisterFallback_HubSuccess(t *testing.T) {
	hub := &mockSchemeResolver{
		name:     "hub",
		resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
	}
	local := &mockSchemeResolver{
		name:     "github",
		resolved: []ResolvedSkill{{Name: "local-skill", URI: "gh://owner/repo/skill"}},
	}
	router := NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", local)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hub.called) != 1 {
		t.Fatalf("hub received %d refs, want 1", len(hub.called))
	}
	if hub.called[0].URI != "gh://owner/repo/skill" {
		t.Errorf("hub got URI %q, want %q", hub.called[0].URI, "gh://owner/repo/skill")
	}
	if len(local.called) != 0 {
		t.Errorf("local resolver received %d refs, want 0", len(local.called))
	}
	if len(result.Resolved) != 1 || result.Resolved[0].Name != "gh-skill" {
		t.Errorf("unexpected resolved result: %+v", result.Resolved)
	}
	if len(result.Errors) != 0 {
		t.Errorf("got %d errors, want 0", len(result.Errors))
	}
}

func TestRoutingSkillResolver_RegisterFallback_HubTransportError(t *testing.T) {
	hub := &mockSchemeResolver{name: "hub", hardErr: fmt.Errorf("connection refused")}
	local := &mockSchemeResolver{
		name: "github",
		resolved: []ResolvedSkill{
			{Name: "a", URI: "gh://owner/repo/a"},
			{Name: "b", URI: "gh://owner/repo/b"},
		},
	}
	router := NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", local)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/a"},
		{URI: "gh://owner/repo/b"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(local.called) != 2 {
		t.Fatalf("local resolver received %d refs, want 2 (all group refs)", len(local.called))
	}
	if len(result.Resolved) != 2 {
		t.Errorf("got %d resolved, want 2", len(result.Resolved))
	}
	if len(result.Errors) != 0 {
		t.Errorf("got %d errors, want 0: %+v", len(result.Errors), result.Errors)
	}
}

func TestRoutingSkillResolver_RegisterFallback_BothFail(t *testing.T) {
	hub := &mockSchemeResolver{name: "hub", hardErr: fmt.Errorf("connection refused")}
	local := &mockSchemeResolver{name: "github", hardErr: fmt.Errorf("github unreachable")}
	router := NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", local)

	_, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/a"},
	}, ResolveOpts{})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != `fallback resolver for scheme "gh" failed: github unreachable` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestRoutingSkillResolver_RegisterFallback_HubPerURIError(t *testing.T) {
	hub := &mockSchemeResolver{
		name:     "hub",
		resolved: []ResolvedSkill{{Name: "ok", URI: "gh://owner/repo/ok"}},
		errors: []ResolveError{
			{URI: "gh://owner/repo/bad", Code: "resolve_failed", Message: "hub could not resolve"},
		},
	}
	local := &mockSchemeResolver{
		name:     "github",
		resolved: []ResolvedSkill{{Name: "bad", URI: "gh://owner/repo/bad"}},
	}
	router := NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", local)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/ok"},
		{URI: "gh://owner/repo/bad"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(local.called) != 1 {
		t.Fatalf("local resolver received %d refs, want 1 (only the errored URI)", len(local.called))
	}
	if local.called[0].URI != "gh://owner/repo/bad" {
		t.Errorf("local resolver got URI %q, want %q", local.called[0].URI, "gh://owner/repo/bad")
	}
	if len(result.Resolved) != 2 {
		t.Fatalf("got %d resolved, want 2: %+v", len(result.Resolved), result.Resolved)
	}
	if len(result.Errors) != 0 {
		t.Errorf("hub per-URI error should be replaced by fallback result, got %+v", result.Errors)
	}
}

func TestRoutingSkillResolver_RegisterFallback_FallbackAlsoErrors(t *testing.T) {
	hub := &mockSchemeResolver{
		name: "hub",
		errors: []ResolveError{
			{URI: "gh://owner/repo/bad", Code: "resolve_failed", Message: "hub could not resolve"},
		},
	}
	local := &mockSchemeResolver{
		name: "github",
		errors: []ResolveError{
			{URI: "gh://owner/repo/bad", Code: "not_found", Message: "no such skill"},
		},
	}
	router := NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", local)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/bad"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1: %+v", len(result.Errors), result.Errors)
	}
	if result.Errors[0].Code != "not_found" {
		t.Errorf("error code = %q, want %q (fallback error should win)", result.Errors[0].Code, "not_found")
	}
}

func TestRoutingSkillResolver_RegisterFallback_NoHubUsesFallbackDirectly(t *testing.T) {
	local := &mockSchemeResolver{
		name:     "github",
		resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
	}
	router := NewRoutingSkillResolver(nil)
	router.RegisterFallback("gh", local)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(local.called) != 1 {
		t.Fatalf("local resolver received %d refs, want 1", len(local.called))
	}
	if len(result.Resolved) != 1 {
		t.Errorf("got %d resolved, want 1", len(result.Resolved))
	}
}

func TestRoutingSkillResolver_RegisterFallback_PrimaryTakesPrecedence(t *testing.T) {
	hub := &mockSchemeResolver{name: "hub"}
	primary := &mockSchemeResolver{
		name:     "gh-primary",
		resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
	}
	local := &mockSchemeResolver{name: "github-fallback"}
	router := NewRoutingSkillResolver(hub)
	router.Register("gh", primary)
	router.RegisterFallback("gh", local)

	if _, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/skill"},
	}, ResolveOpts{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(primary.called) != 1 {
		t.Errorf("primary received %d refs, want 1", len(primary.called))
	}
	if len(hub.called) != 0 {
		t.Errorf("hub received %d refs, want 0", len(hub.called))
	}
	if len(local.called) != 0 {
		t.Errorf("fallback received %d refs, want 0", len(local.called))
	}
}

func TestRoutingSkillResolver_RegisterFallback_MixedBatch(t *testing.T) {
	hub := &mockSchemeResolver{
		name:     "hub",
		resolved: []ResolvedSkill{{Name: "hub-skill", URI: "skill://scion/core/hub-skill"}},
	}
	local := &mockSchemeResolver{name: "github"}
	router := NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", local)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "skill://scion/core/hub-skill"},
		{URI: "gh://owner/repo/skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both groups go to Hub, in two separate calls.
	if len(hub.called) != 2 {
		t.Errorf("hub received %d refs, want 2", len(hub.called))
	}
	if len(local.called) != 0 {
		t.Errorf("local resolver received %d refs, want 0", len(local.called))
	}
	if len(result.Resolved) == 0 {
		t.Error("expected at least one resolved skill")
	}
}

func TestRoutingSkillResolver_RegisterFallbackPanics(t *testing.T) {
	t.Run("empty scheme", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for empty scheme")
			}
		}()
		router := NewRoutingSkillResolver(nil)
		router.RegisterFallback("", &mockSchemeResolver{})
	})

	t.Run("duplicate scheme", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for duplicate scheme")
			}
		}()
		router := NewRoutingSkillResolver(nil)
		router.RegisterFallback("gh", &mockSchemeResolver{})
		router.RegisterFallback("gh", &mockSchemeResolver{})
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
