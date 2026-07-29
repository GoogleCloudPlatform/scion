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

package runtimebroker

import (
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// recordingResolver is a SkillResolver test double that records the refs it was
// asked about and replays a canned outcome.
type recordingResolver struct {
	name     string
	resolved []agent.ResolvedSkill
	errors   []agent.ResolveError
	hardErr  error

	called []api.SkillReference
	// calls counts Resolve invocations, which len(called) cannot distinguish
	// from a single multi-ref call. Tests use it to prove the Hub was consulted
	// at all — the assertion that actually fails if gh:// is reverted from
	// RegisterFallback back to Register.
	calls int
}

func (r *recordingResolver) ResolverName() string { return r.name }

func (r *recordingResolver) Resolve(_ context.Context, refs []api.SkillReference, _ agent.ResolveOpts) (*agent.ResolveResult, error) {
	r.calls++
	r.called = append(r.called, refs...)
	if r.hardErr != nil {
		return nil, r.hardErr
	}
	return &agent.ResolveResult{Resolved: r.resolved, Errors: r.errors}, nil
}

const ghURI = "gh://owner/repo/skill"

// TestBuildSkillRouter_GHRoutesToHubFirst pins the Phase 3 flip: gh:// URIs
// must reach the Hub, not the local GitHub resolver, so the Hub's DB-backed
// cache is what serves repeat resolutions. Reverting the wiring to
// router.Register("gh", ...) fails this test.
func TestBuildSkillRouter_GHRoutesToHubFirst(t *testing.T) {
	hub := &recordingResolver{
		name:     "hub",
		resolved: []agent.ResolvedSkill{{Name: "gh-skill", URI: ghURI}},
	}
	gh := &recordingResolver{
		name:     "github",
		resolved: []agent.ResolvedSkill{{Name: "local-skill", URI: ghURI}},
	}
	gcp := &recordingResolver{name: "gcp"}

	result, err := buildSkillRouter(hub, gh, gcp).Resolve(
		context.Background(),
		[]api.SkillReference{{URI: ghURI}},
		agent.ResolveOpts{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hub.calls != 1 {
		t.Fatalf("hub was called %d time(s), want exactly 1 — gh:// is not routed Hub-first", hub.calls)
	}
	if len(hub.called) != 1 {
		t.Fatalf("hub received %d refs, want 1 — gh:// is not routed Hub-first", len(hub.called))
	}
	if hub.called[0].URI != ghURI {
		t.Errorf("hub got URI %q, want %q", hub.called[0].URI, ghURI)
	}
	if len(gh.called) != 0 {
		t.Errorf("local GitHub resolver was called %d time(s) despite Hub success; want 0", len(gh.called))
	}
	if len(result.Resolved) != 1 || result.Resolved[0].Name != "gh-skill" {
		t.Errorf("result did not come from Hub: %+v", result.Resolved)
	}
	if len(result.Errors) != 0 {
		t.Errorf("got %d errors, want 0: %+v", len(result.Errors), result.Errors)
	}
}

// TestBuildSkillRouter_GHFallsBackOnHubPerURIError checks the backstop half of
// the flip: a Hub that cannot resolve a particular gh:// URI must not fail the
// request, because the local GitHub resolver is still wired behind it.
func TestBuildSkillRouter_GHFallsBackOnHubPerURIError(t *testing.T) {
	hub := &recordingResolver{
		name: "hub",
		errors: []agent.ResolveError{
			{URI: ghURI, Code: "resolve_failed", Message: "hub could not resolve"},
		},
	}
	gh := &recordingResolver{
		name:     "github",
		resolved: []agent.ResolvedSkill{{Name: "local-skill", URI: ghURI}},
	}
	gcp := &recordingResolver{name: "gcp"}

	result, err := buildSkillRouter(hub, gh, gcp).Resolve(
		context.Background(),
		[]api.SkillReference{{URI: ghURI}},
		agent.ResolveOpts{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Without this the test passes even if gh:// is reverted to Register("gh",
	// ghResolver): the local resolver would be called first and still return
	// "local-skill". Asserting the Hub was tried is what makes it a flip guard.
	if hub.calls != 1 {
		t.Fatalf("hub was called %d time(s), want exactly 1 — gh:// must be tried Hub-first", hub.calls)
	}
	if len(gh.called) != 1 {
		t.Fatalf("local GitHub resolver received %d refs, want 1", len(gh.called))
	}
	if len(result.Resolved) != 1 || result.Resolved[0].Name != "local-skill" {
		t.Fatalf("result did not come from the fallback: %+v", result.Resolved)
	}
	if len(result.Errors) != 0 {
		t.Errorf("hub error should be superseded by the fallback, got %+v", result.Errors)
	}
}

// TestBuildSkillRouter_GHFallsBackOnHubTransportError covers a Hub that is
// entirely unreachable: gh:// provisioning must degrade to direct GitHub
// resolution rather than failing outright.
func TestBuildSkillRouter_GHFallsBackOnHubTransportError(t *testing.T) {
	hub := &recordingResolver{name: "hub", hardErr: fmt.Errorf("connection refused")}
	gh := &recordingResolver{
		name:     "github",
		resolved: []agent.ResolvedSkill{{Name: "local-skill", URI: ghURI}},
	}
	gcp := &recordingResolver{name: "gcp"}

	result, err := buildSkillRouter(hub, gh, gcp).Resolve(
		context.Background(),
		[]api.SkillReference{{URI: ghURI}},
		agent.ResolveOpts{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// As above: proves the Hub was attempted before the local resolver, so a
	// revert of the routing flip fails here rather than passing coincidentally.
	if hub.calls != 1 {
		t.Fatalf("hub was called %d time(s), want exactly 1 — gh:// must be tried Hub-first", hub.calls)
	}
	if len(gh.called) != 1 {
		t.Fatalf("local GitHub resolver received %d refs, want 1", len(gh.called))
	}
	if len(result.Resolved) != 1 || result.Resolved[0].Name != "local-skill" {
		t.Errorf("result did not come from the fallback: %+v", result.Resolved)
	}
}

// TestBuildSkillRouter_OtherSchemes guards the schemes the flip must leave
// alone: skill:// and bare names still go to the Hub, and gcp-skill:// still
// bypasses the Hub entirely.
func TestBuildSkillRouter_OtherSchemes(t *testing.T) {
	tests := []struct {
		name        string
		uri         string
		wantHub     int
		wantGCP     int
		resolvedURI string
	}{
		{name: "skill scheme goes to hub", uri: "skill://scion/core/s", wantHub: 1, resolvedURI: "skill://scion/core/s"},
		{name: "bare name goes to hub", uri: "code-review", wantHub: 1, resolvedURI: "code-review"},
		{name: "gcp-skill bypasses hub", uri: "gcp-skill://alias/ID", wantGCP: 1, resolvedURI: "gcp-skill://alias/ID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := &recordingResolver{
				name:     "hub",
				resolved: []agent.ResolvedSkill{{Name: "s", URI: tt.resolvedURI}},
			}
			gh := &recordingResolver{name: "github"}
			gcp := &recordingResolver{
				name:     "gcp",
				resolved: []agent.ResolvedSkill{{Name: "s", URI: tt.resolvedURI}},
			}

			if _, err := buildSkillRouter(hub, gh, gcp).Resolve(
				context.Background(),
				[]api.SkillReference{{URI: tt.uri}},
				agent.ResolveOpts{},
			); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(hub.called) != tt.wantHub {
				t.Errorf("hub received %d refs, want %d", len(hub.called), tt.wantHub)
			}
			// Each case sends a single ref, so the expected ref count doubles as
			// the expected number of Resolve invocations: 1 for the Hub-routed
			// schemes, 0 for the scheme that must bypass the Hub entirely.
			if hub.calls != tt.wantHub {
				t.Errorf("hub was called %d time(s), want %d", hub.calls, tt.wantHub)
			}
			if len(gcp.called) != tt.wantGCP {
				t.Errorf("gcp resolver received %d refs, want %d", len(gcp.called), tt.wantGCP)
			}
			if gcp.calls != tt.wantGCP {
				t.Errorf("gcp resolver was called %d time(s), want %d", gcp.calls, tt.wantGCP)
			}
			if len(gh.called) != 0 {
				t.Errorf("github resolver received %d refs, want 0", len(gh.called))
			}
		})
	}
}
