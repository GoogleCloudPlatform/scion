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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const (
	preURI     = "skill://scion/global/private-skill@latest"
	deniedURI  = "skill://scion/global/denied-skill"
	ghURI      = "gh://owner/repo/skills/x@main"
	unusedURI  = "skill://scion/global/not-requested"
	hubBaseURL = "http://127.0.0.1:9810"
)

func preResolvedFixture() *hubclient.ResolveSkillsResponse {
	return &hubclient.ResolveSkillsResponse{
		Resolved: []hubclient.ResolvedSkill{
			{
				URI: preURI, Name: "private-skill", ResolvedVersion: "1.0.0", ContentHash: "sha256:abc",
				Files: []hubclient.DownloadURLInfo{
					{Path: "SKILL.md", URL: "https://storage.example.com/signed/SKILL.md", Hash: "sha256:f1", Size: 3},
				},
			},
			{URI: unusedURI, Name: "not-requested", ResolvedVersion: "2.0.0"},
		},
		Errors: []hubclient.ResolveSkillError{
			{URI: deniedURI, Code: "forbidden", Message: "the agent's creator does not have permission to access this skill"},
		},
	}
}

// Pre-resolved skills are served without touching the next resolver, and
// per-ref metadata (As, Scope, Optional) comes from the requested refs.
func TestPreResolvedSkillResolver_ServesPreResolved(t *testing.T) {
	next := &mockSchemeResolver{name: "router"}
	r := NewPreResolvedSkillResolver(preResolvedFixture(), next, hubBaseURL)

	res, err := r.Resolve(context.Background(), []api.SkillReference{
		{URI: preURI, As: "alias", Scope: "template"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(next.called) != 0 {
		t.Fatalf("next resolver must not be called for pre-resolved refs, got %v", next.called)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if len(res.Resolved) != 1 {
		t.Fatalf("want 1 resolved (unrequested entries dropped), got %d", len(res.Resolved))
	}
	got := res.Resolved[0]
	if got.URI != preURI || got.Version != "1.0.0" || got.Hash != "sha256:abc" {
		t.Errorf("unexpected resolved skill: %+v", got)
	}
	if got.As != "alias" || got.Scope != "template" {
		t.Errorf("ref metadata not carried over: As=%q Scope=%q", got.As, got.Scope)
	}
	if len(got.Files) != 1 || got.Files[0].URL != "https://storage.example.com/signed/SKILL.md" {
		t.Errorf("absolute URL must be unchanged: %+v", got.Files)
	}
}

// A pre-resolved error is authoritative: it is surfaced as a per-skill error
// and is never retried through the broker's own (denied) identity.
func TestPreResolvedSkillResolver_PreResolvedErrorIsAuthoritative(t *testing.T) {
	next := &mockSchemeResolver{name: "router", resolved: []ResolvedSkill{{URI: deniedURI, Name: "denied-skill"}}}
	r := NewPreResolvedSkillResolver(preResolvedFixture(), next, hubBaseURL)

	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: deniedURI}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(next.called) != 0 {
		t.Fatalf("pre-resolved error must not fall through to next, got %v", next.called)
	}
	if len(res.Resolved) != 0 || len(res.Errors) != 1 {
		t.Fatalf("want exactly one error, got resolved=%v errors=%v", res.Resolved, res.Errors)
	}
	if res.Errors[0].Code != "forbidden" || res.Errors[0].URI != deniedURI {
		t.Errorf("unexpected error: %+v", res.Errors[0])
	}
}

// Refs the Hub did not cover go to the next resolver; results are merged.
func TestPreResolvedSkillResolver_MixedBatch(t *testing.T) {
	next := &mockSchemeResolver{name: "router", resolved: []ResolvedSkill{{URI: ghURI, Name: "x"}}}
	r := NewPreResolvedSkillResolver(preResolvedFixture(), next, hubBaseURL)

	res, err := r.Resolve(context.Background(), []api.SkillReference{
		{URI: preURI}, {URI: ghURI}, {URI: deniedURI, Optional: true},
	}, ResolveOpts{ProjectID: "p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(next.called) != 1 || next.called[0].URI != ghURI {
		t.Fatalf("next should receive only the uncovered ref, got %v", next.called)
	}
	if len(res.Resolved) != 2 {
		t.Fatalf("want 2 resolved, got %+v", res.Resolved)
	}
	if len(res.Errors) != 1 || res.Errors[0].URI != deniedURI {
		t.Fatalf("want denied error, got %+v", res.Errors)
	}
}

func TestPreResolvedSkillResolver_NextHardErrorPropagates(t *testing.T) {
	next := &mockSchemeResolver{name: "router", hardErr: errors.New("hub down")}
	r := NewPreResolvedSkillResolver(preResolvedFixture(), next, hubBaseURL)
	if _, err := r.Resolve(context.Background(), []api.SkillReference{{URI: ghURI}}, ResolveOpts{}); err == nil {
		t.Fatal("expected hard error from next resolver")
	}
}

func TestPreResolvedSkillResolver_NilNextFailsClosedPerSkill(t *testing.T) {
	r := NewPreResolvedSkillResolver(preResolvedFixture(), nil, hubBaseURL)
	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: preURI}, {URI: ghURI}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].URI != preURI {
		t.Fatalf("pre-resolved skill should still resolve: %+v", res.Resolved)
	}
	if len(res.Errors) != 1 || res.Errors[0].URI != ghURI || res.Errors[0].Code != "no_resolver" {
		t.Fatalf("uncovered ref should fail per-skill: %+v", res.Errors)
	}
}

func TestPreResolvedSkillResolver_NilPreDelegatesEverything(t *testing.T) {
	next := &mockSchemeResolver{name: "router", resolved: []ResolvedSkill{{URI: preURI}}}
	r := NewPreResolvedSkillResolver(nil, next, "")
	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: preURI}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(next.called) != 1 || len(res.Resolved) != 1 {
		t.Fatalf("nil pre should delegate: called=%v resolved=%v", next.called, res.Resolved)
	}
}

// Local-storage skills come back from the Hub as Hub-relative paths; the
// broker absolutizes them against its own Hub endpoint.
func TestPreResolvedSkillResolver_AbsolutizesHubRelativeURLs(t *testing.T) {
	pre := &hubclient.ResolveSkillsResponse{Resolved: []hubclient.ResolvedSkill{{
		URI: preURI, Name: "private-skill", ResolvedVersion: "1.0.0",
		Files: []hubclient.DownloadURLInfo{{Path: "SKILL.md", URL: "/api/v1/skills/id-1/files/SKILL.md?raw=1"}},
	}}}

	res, err := NewPreResolvedSkillResolver(pre, nil, hubBaseURL+"/").
		Resolve(context.Background(), []api.SkillReference{{URI: preURI}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := hubBaseURL + "/api/v1/skills/id-1/files/SKILL.md?raw=1"
	if len(res.Resolved) != 1 || res.Resolved[0].Files[0].URL != want {
		t.Fatalf("want URL %q, got %+v", want, res.Resolved)
	}

	// No endpoint known: the skill fails per-skill rather than installing
	// from an unusable URL.
	res, err = NewPreResolvedSkillResolver(pre, nil, "").
		Resolve(context.Background(), []api.SkillReference{{URI: preURI}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 0 || len(res.Errors) != 1 || res.Errors[0].Code != "invalid_url" {
		t.Fatalf("want invalid_url error, got resolved=%+v errors=%+v", res.Resolved, res.Errors)
	}
}

func TestPreResolvedSkillResolver_ResolverName(t *testing.T) {
	if got := NewPreResolvedSkillResolver(nil, nil, "").ResolverName(); got != "hub" {
		t.Errorf("ResolverName = %q, want hub", got)
	}
}
