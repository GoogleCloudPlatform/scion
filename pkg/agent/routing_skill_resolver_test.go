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
	"errors"
	"fmt"
	"testing"
	"time"

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

// echoResolver resolves each ref it is given into its own ResolvedSkill,
// preserving the ref's As alias. This makes it possible to assert that every
// alias of a URI — not just the first — reached the fallback.
type echoResolver struct {
	name   string
	called []api.SkillReference
}

func (m *echoResolver) ResolverName() string { return m.name }
func (m *echoResolver) Resolve(_ context.Context, refs []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	m.called = append(m.called, refs...)
	result := &ResolveResult{}
	for _, ref := range refs {
		result.Resolved = append(result.Resolved, ResolvedSkill{
			Name: "skill",
			URI:  ref.URI,
			As:   ref.As,
		})
	}
	return result, nil
}

// TestRoutingSkillResolver_RegisterFallback_SameURIDifferentAliases guards
// against dropping aliases when the same URI is imported under two names. The
// retry set is keyed by ref, not by URI, so both aliases must reach the
// fallback and both must come back resolved.
func TestRoutingSkillResolver_RegisterFallback_SameURIDifferentAliases(t *testing.T) {
	const uri = "gh://owner/repo/skill"

	hub := &mockSchemeResolver{
		name: "hub",
		errors: []ResolveError{
			{URI: uri, Code: "resolve_failed", Message: "hub could not resolve"},
		},
	}
	local := &echoResolver{name: "github"}
	router := NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", local)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: uri, As: "first"},
		{URI: uri, As: "second"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both aliases must be handed to the fallback.
	if len(local.called) != 2 {
		t.Fatalf("fallback received %d refs, want 2 (one per alias): %+v", len(local.called), local.called)
	}
	gotCalled := map[string]bool{}
	for _, ref := range local.called {
		gotCalled[ref.As] = true
	}
	for _, want := range []string{"first", "second"} {
		if !gotCalled[want] {
			t.Errorf("fallback was not asked about alias %q; got %+v", want, local.called)
		}
	}

	// Both aliases must come back resolved.
	if len(result.Resolved) != 2 {
		t.Fatalf("got %d resolved, want 2 (one per alias): %+v", len(result.Resolved), result.Resolved)
	}
	gotResolved := map[string]bool{}
	for _, rs := range result.Resolved {
		gotResolved[rs.As] = true
	}
	for _, want := range []string{"first", "second"} {
		if !gotResolved[want] {
			t.Errorf("alias %q missing from resolved set; got %+v", want, result.Resolved)
		}
	}

	// The hub error was fully superseded by the fallback.
	if len(result.Errors) != 0 {
		t.Errorf("expected no errors after successful fallback, got %+v", result.Errors)
	}
}

// TestRoutingSkillResolver_RegisterFallback_SilentDrop covers a primary
// resolver that returns fewer skills than it was asked for *without* reporting
// any per-URI error. Gating the fallback retry on len(sr.Errors) > 0 would miss
// this entirely and hand back a short result; gating on
// len(sr.Resolved) < len(schemeRefs) catches it.
//
// Two shapes of silent drop are exercised:
//   - a URI omitted outright, with no matching error
//   - a URI returned under only one of its two As aliases
func TestRoutingSkillResolver_RegisterFallback_SilentDrop(t *testing.T) {
	t.Run("ref omitted with no error", func(t *testing.T) {
		const kept = "gh://owner/repo/kept"
		const dropped = "gh://owner/repo/dropped"

		// Hub resolves one ref and silently forgets the other — no error at all.
		hub := &mockSchemeResolver{
			name: "hub",
			resolved: []ResolvedSkill{
				{Name: "kept", URI: kept},
			},
		}
		local := &echoResolver{name: "github"}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		result, err := router.Resolve(context.Background(), []api.SkillReference{
			{URI: kept},
			{URI: dropped},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Only the dropped ref should be retried — the resolved one must not be
		// re-fetched from the fallback.
		if len(local.called) != 1 {
			t.Fatalf("fallback received %d refs, want 1: %+v", len(local.called), local.called)
		}
		if local.called[0].URI != dropped {
			t.Errorf("fallback asked about %q, want %q", local.called[0].URI, dropped)
		}

		if len(result.Resolved) != 2 {
			t.Fatalf("got %d resolved, want 2: %+v", len(result.Resolved), result.Resolved)
		}
		gotURIs := map[string]bool{}
		for _, rs := range result.Resolved {
			gotURIs[rs.URI] = true
		}
		for _, want := range []string{kept, dropped} {
			if !gotURIs[want] {
				t.Errorf("URI %q missing from resolved set; got %+v", want, result.Resolved)
			}
		}
		if len(result.Errors) != 0 {
			t.Errorf("expected no errors, got %+v", result.Errors)
		}
	})

	t.Run("alias omitted with no error", func(t *testing.T) {
		const uri = "gh://owner/repo/skill"

		// Hub collapses two aliases of the same URI into a single entry and
		// reports no error for the alias it dropped.
		hub := &mockSchemeResolver{
			name: "hub",
			resolved: []ResolvedSkill{
				{Name: "skill", URI: uri, As: "first"},
			},
		}
		local := &echoResolver{name: "github"}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		result, err := router.Resolve(context.Background(), []api.SkillReference{
			{URI: uri, As: "first"},
			{URI: uri, As: "second"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// The alias hub already resolved must not be retried; the missing one must.
		if len(local.called) != 1 {
			t.Fatalf("fallback received %d refs, want 1 (only the dropped alias): %+v", len(local.called), local.called)
		}
		if local.called[0].As != "second" {
			t.Errorf("fallback asked about alias %q, want %q", local.called[0].As, "second")
		}

		if len(result.Resolved) != 2 {
			t.Fatalf("got %d resolved, want 2 (one per alias): %+v", len(result.Resolved), result.Resolved)
		}
		gotAliases := map[string]bool{}
		for _, rs := range result.Resolved {
			gotAliases[rs.As] = true
		}
		for _, want := range []string{"first", "second"} {
			if !gotAliases[want] {
				t.Errorf("alias %q missing from resolved set; got %+v", want, result.Resolved)
			}
		}
		if len(result.Errors) != 0 {
			t.Errorf("expected no errors, got %+v", result.Errors)
		}
	})

	t.Run("no retry when every ref is accounted for", func(t *testing.T) {
		const uri = "gh://owner/repo/skill"

		hub := &mockSchemeResolver{
			name: "hub",
			resolved: []ResolvedSkill{
				{Name: "skill", URI: uri, As: "first"},
				{Name: "skill", URI: uri, As: "second"},
			},
		}
		local := &echoResolver{name: "github"}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		result, err := router.Resolve(context.Background(), []api.SkillReference{
			{URI: uri, As: "first"},
			{URI: uri, As: "second"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(local.called) != 0 {
			t.Errorf("fallback should not have been called, got %+v", local.called)
		}
		if len(result.Resolved) != 2 {
			t.Errorf("got %d resolved, want 2: %+v", len(result.Resolved), result.Resolved)
		}
	})
}

// ctxProbeResolver records the cancellation state of the context it is called
// with, so tests can assert that the fallback is invoked with a context that
// survived the primary's deadline.
type ctxProbeResolver struct {
	name     string
	resolved []ResolvedSkill
	errors   []ResolveError

	called       []api.SkillReference
	callCtxOK    bool      // ctx.Err() == nil at call time
	sawValue     string    // value carried over from the caller's context
	callDeadline time.Time // deadline of the context the fallback was called with
	callHasDL    bool      // whether that context carried a deadline at all
	onCall       func(ctx context.Context)
}

type ctxProbeKey struct{}

func (m *ctxProbeResolver) ResolverName() string { return m.name }
func (m *ctxProbeResolver) Resolve(ctx context.Context, refs []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	m.called = append(m.called, refs...)
	m.callCtxOK = ctx.Err() == nil
	m.callDeadline, m.callHasDL = ctx.Deadline()
	if v, ok := ctx.Value(ctxProbeKey{}).(string); ok {
		m.sawValue = v
	}
	if m.onCall != nil {
		m.onCall(ctx)
		m.callCtxOK = ctx.Err() == nil
	}
	if !m.callCtxOK {
		return nil, ctx.Err()
	}
	return &ResolveResult{Resolved: m.resolved, Errors: m.errors}, nil
}

// TestRoutingSkillResolver_RegisterFallback_CancelledPrimaryContext covers I-4:
// the fallback must not inherit the primary's cancellation. If a tight caller
// deadline is consumed by the Hub call, passing the same context straight to
// the fallback makes the fallback fail instantly and defeats its purpose.
// context.WithoutCancel detaches the cancellation while keeping values.
func TestRoutingSkillResolver_RegisterFallback_CancelledPrimaryContext(t *testing.T) {
	t.Run("transport error with exhausted context", func(t *testing.T) {
		// The primary "times out": it fails, and the caller's context is dead
		// by the time the router reaches for the fallback.
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxProbeKey{}, "trace-abc"))
		hub := &mockSchemeResolver{name: "hub", hardErr: context.DeadlineExceeded}
		local := &ctxProbeResolver{
			name:     "github",
			resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
		}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		cancel() // caller's deadline is gone before the fallback runs

		result, err := router.Resolve(ctx, []api.SkillReference{
			{URI: "gh://owner/repo/skill"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(local.called) != 1 {
			t.Fatalf("fallback received %d refs, want 1", len(local.called))
		}
		if !local.callCtxOK {
			t.Error("fallback was called with an already-cancelled context; want a detached context")
		}
		// Detached, but not unbounded: the replacement context must carry its
		// own deadline so a wedged fallback cannot run forever.
		if !local.callHasDL {
			t.Error("detached fallback context has no deadline; want a bounded budget")
		} else if d := time.Until(local.callDeadline); d <= 0 || d > fallbackTimeout {
			t.Errorf("detached fallback budget = %v, want (0, %v]", d, fallbackTimeout)
		}
		if local.sawValue != "trace-abc" {
			t.Errorf("fallback context lost caller values: got %q, want %q", local.sawValue, "trace-abc")
		}
		if len(result.Resolved) != 1 || result.Resolved[0].Name != "gh-skill" {
			t.Errorf("unexpected resolved result: %+v", result.Resolved)
		}
	})

	t.Run("per-URI retry with exhausted context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxProbeKey{}, "trace-xyz"))
		hub := &mockSchemeResolver{
			name: "hub",
			errors: []ResolveError{
				{URI: "gh://owner/repo/bad", Code: "resolve_failed", Message: "hub could not resolve"},
			},
		}
		local := &ctxProbeResolver{
			name:     "github",
			resolved: []ResolvedSkill{{Name: "bad", URI: "gh://owner/repo/bad"}},
		}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		cancel()

		result, err := router.Resolve(ctx, []api.SkillReference{
			{URI: "gh://owner/repo/bad"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(local.called) != 1 {
			t.Fatalf("fallback received %d refs, want 1", len(local.called))
		}
		if !local.callCtxOK {
			t.Error("fallback retry was called with an already-cancelled context; want a detached context")
		}
		if !local.callHasDL {
			t.Error("detached fallback context has no deadline; want a bounded budget")
		} else if d := time.Until(local.callDeadline); d <= 0 || d > fallbackTimeout {
			t.Errorf("detached fallback budget = %v, want (0, %v]", d, fallbackTimeout)
		}
		if local.sawValue != "trace-xyz" {
			t.Errorf("fallback context lost caller values: got %q, want %q", local.sawValue, "trace-xyz")
		}
		if len(result.Resolved) != 1 {
			t.Fatalf("got %d resolved, want 1: %+v", len(result.Resolved), result.Resolved)
		}
		if len(result.Errors) != 0 {
			t.Errorf("expected hub error to be superseded by fallback, got %+v", result.Errors)
		}
	})

	// The detach above is a repair for a spent context, not the default. When
	// the caller's context is still healthy the fallback must inherit it, so
	// the caller's deadline still applies and a client disconnect mid-fallback
	// still stops the work.
	t.Run("transport error with healthy context inherits caller context", func(t *testing.T) {
		callerDeadline := time.Now().Add(30 * time.Second)
		ctx, cancel := context.WithDeadline(
			context.WithValue(context.Background(), ctxProbeKey{}, "trace-live"), callerDeadline)
		defer cancel()

		hub := &mockSchemeResolver{name: "hub", hardErr: errors.New("hub unreachable")}
		local := &ctxProbeResolver{
			name:     "github",
			resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
		}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		result, err := router.Resolve(ctx, []api.SkillReference{
			{URI: "gh://owner/repo/skill"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !local.callCtxOK {
			t.Fatal("fallback context was already cancelled")
		}
		if !local.callHasDL || !local.callDeadline.Equal(callerDeadline) {
			t.Errorf("fallback deadline = %v (set=%v), want caller deadline %v",
				local.callDeadline, local.callHasDL, callerDeadline)
		}
		if local.sawValue != "trace-live" {
			t.Errorf("fallback context lost caller values: got %q", local.sawValue)
		}
		if len(result.Resolved) != 1 {
			t.Errorf("got %d resolved, want 1", len(result.Resolved))
		}
	})

	t.Run("healthy context still propagates caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		hub := &mockSchemeResolver{name: "hub", hardErr: errors.New("hub unreachable")}
		local := &ctxProbeResolver{
			name:     "github",
			resolved: []ResolvedSkill{{Name: "gh-skill", URI: "gh://owner/repo/skill"}},
			// Simulate the caller disconnecting while the fallback is in flight.
			onCall: func(context.Context) { cancel() },
		}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		_, err := router.Resolve(ctx, []api.SkillReference{
			{URI: "gh://owner/repo/skill"},
		}, ResolveOpts{})
		if err == nil {
			t.Fatal("expected error when the caller cancels during the fallback")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled to propagate to the fallback", err)
		}
	})

	t.Run("per-URI retry with healthy context inherits caller context", func(t *testing.T) {
		callerDeadline := time.Now().Add(45 * time.Second)
		ctx, cancel := context.WithDeadline(
			context.WithValue(context.Background(), ctxProbeKey{}, "trace-live2"), callerDeadline)
		defer cancel()

		hub := &mockSchemeResolver{
			name: "hub",
			errors: []ResolveError{
				{URI: "gh://owner/repo/bad", Code: "resolve_failed", Message: "hub could not resolve"},
			},
		}
		local := &ctxProbeResolver{
			name:     "github",
			resolved: []ResolvedSkill{{Name: "bad", URI: "gh://owner/repo/bad"}},
		}
		router := NewRoutingSkillResolver(hub)
		router.RegisterFallback("gh", local)

		result, err := router.Resolve(ctx, []api.SkillReference{
			{URI: "gh://owner/repo/bad"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !local.callHasDL || !local.callDeadline.Equal(callerDeadline) {
			t.Errorf("fallback deadline = %v (set=%v), want caller deadline %v",
				local.callDeadline, local.callHasDL, callerDeadline)
		}
		if local.sawValue != "trace-live2" {
			t.Errorf("fallback context lost caller values: got %q", local.sawValue)
		}
		if len(result.Resolved) != 1 {
			t.Errorf("got %d resolved, want 1", len(result.Resolved))
		}
	})

	// A context with a sliver of time left is not "healthy" in any useful sense:
	// it passes ctx.Err() == nil but the fallback cannot possibly finish inside
	// it. fallbackMinBudget makes that case behave like a fully-spent context.
	t.Run("nearly spent context is detached rather than inherited", func(t *testing.T) {
		nearlySpent := time.Now().Add(fallbackMinBudget - time.Second)
		ctx, cancel := context.WithDeadline(
			context.WithValue(context.Background(), ctxProbeKey{}, "trace-sliver"), nearlySpent)
		defer cancel()

		fbCtx, fbCancel := fallbackContext(ctx)
		defer fbCancel()

		if err := fbCtx.Err(); err != nil {
			t.Fatalf("fallback context is already dead: %v", err)
		}
		dl, ok := fbCtx.Deadline()
		if !ok {
			t.Fatal("detached fallback context has no deadline; want a bounded budget")
		}
		if d := time.Until(dl); d < fallbackTimeout/2 || d > fallbackTimeout {
			t.Errorf("fallback budget = %v, want a fresh budget in [%v, %v] rather than the caller's %v",
				d, fallbackTimeout/2, fallbackTimeout, time.Until(nearlySpent))
		}
		if v, _ := fbCtx.Value(ctxProbeKey{}).(string); v != "trace-sliver" {
			t.Errorf("detached fallback context lost caller values: got %q", v)
		}
	})
}
