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

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// fakeHubSkills is a hubclient.SkillService that answers Resolve and
// records the URIs it was asked for. Other methods are not used.
type fakeHubSkills struct {
	hubclient.SkillService
	asked     []string
	projectID string
}

func (f *fakeHubSkills) Resolve(_ context.Context, req *hubclient.ResolveSkillsRequest) (*hubclient.ResolveSkillsResponse, error) {
	f.projectID = req.ProjectID
	resp := &hubclient.ResolveSkillsResponse{}
	for _, s := range req.Skills {
		f.asked = append(f.asked, s.URI)
		resp.Resolved = append(resp.Resolved, hubclient.ResolvedSkill{URI: s.URI, Name: "from-hub"})
	}
	return resp, nil
}

// fakeRegistries is a hubclient.SkillRegistryService that answers Get from a
// map and records the names it was asked for.
type fakeRegistries struct {
	hubclient.SkillRegistryService
	regs  map[string]*hubclient.SkillRegistry
	asked []string
}

func (f *fakeRegistries) Get(_ context.Context, id string) (*hubclient.SkillRegistry, error) {
	f.asked = append(f.asked, id)
	return f.regs[id], nil
}

// TestWithLocalSkillResolution checks the resolver set up for a local
// create: skill:// refs go to the Hub, gh:// refs to a GitHub resolver over
// the given cache, gcp-skill:// refs to a resolver that looks registries up
// through the Hub, and the context carries the install credentials and the
// resolve project. The returned func writes pending cache entries to disk.
func TestWithLocalSkillResolution(t *testing.T) {
	// The GitHub resolver falls back to the process GITHUB_TOKEN when the
	// token passed in is empty; clear it so the test controls the token.
	t.Setenv("GITHUB_TOKEN", "")

	const ghURI = "gh://acme/tools/s@main"
	dir := t.TempDir()
	cache, err := agent.NewGitHubResolutionCache(dir, time.Hour, agent.WithResolutionCacheSaveDelay(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Seed a fresh entry under the key the resolver uses for ghURI with no
	// credential, so the gh:// ref resolves from the cache without GitHub.
	ghRef, err := agent.ParseGitHubSkillURI(ghURI)
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := "gh://" + ghRef.Owner + "/" + ghRef.Repo + "/" + ghRef.SkillPath + "@" + ghRef.Ref
	seeded := agent.ResolvedSkill{Name: "from-cache", URI: ghURI, Version: "abc"}
	if _, err := cache.ResolveWithFetch(context.Background(), cacheKey, "seed", "seed", "seed", true, nil,
		func(context.Context) (agent.ResolvedSkill, error) { return seeded, nil }); err != nil {
		t.Fatal(err)
	}

	hub := &fakeHubSkills{}
	regs := &fakeRegistries{regs: map[string]*hubclient.SkillRegistry{
		"off": {Name: "off", Type: "gcp", Status: "disabled", Endpoint: "https://example.invalid/"},
	}}

	ctx, flush := withLocalSkillResolution(context.Background(), hub, regs, "proj-1", "", cache)

	if got := agent.ResolveProjectIDFromContext(ctx); got != "proj-1" {
		t.Errorf("resolve project = %q, want proj-1", got)
	}
	resolver := agent.SkillResolverFromContext(ctx)
	if resolver == nil {
		t.Fatal("no skill resolver in context")
	}

	res, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "skill://team/hub-skill"},
		{URI: ghURI},
		{URI: "gcp-skill://off/x"},
		{URI: "gcp-skill://missing/y"},
	}, agent.ResolveOpts{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	byURI := map[string]agent.ResolvedSkill{}
	for _, s := range res.Resolved {
		byURI[s.URI] = s
	}
	if s, ok := byURI["skill://team/hub-skill"]; !ok || s.Name != "from-hub" {
		t.Errorf("skill:// ref not resolved by the Hub: %+v", res)
	}
	if len(hub.asked) != 1 || hub.asked[0] != "skill://team/hub-skill" {
		t.Errorf("Hub asked for %v, want only the skill:// ref", hub.asked)
	}
	if hub.projectID != "proj-1" {
		t.Errorf("Hub resolve project = %q, want proj-1", hub.projectID)
	}
	if s, ok := byURI[ghURI]; !ok || s.Name != "from-cache" {
		t.Errorf("gh:// ref not resolved from the given cache: %+v", res)
	}

	errByURI := map[string]string{}
	for _, e := range res.Errors {
		errByURI[e.URI] = e.Message
	}
	if msg := errByURI["gcp-skill://off/x"]; !strings.Contains(msg, "disabled") {
		t.Errorf("gcp-skill:// ref with a disabled registry: error %q, want one naming it disabled", msg)
	}
	if msg := errByURI["gcp-skill://missing/y"]; !strings.Contains(msg, `registry "missing" not found`) {
		t.Errorf("gcp-skill:// ref with an unknown registry: error %q, want registry not found", msg)
	}
	if strings.Join(regs.asked, ",") != "off,missing" {
		t.Errorf("registries looked up = %v, want [off missing]", regs.asked)
	}

	// The returned func writes the pending cache entry now, rather than
	// after the cache's save delay.
	flush()
	data, err := os.ReadFile(filepath.Join(dir, "github-resolution-cache.json"))
	if err != nil {
		t.Fatalf("cache file not written by the returned func: %v", err)
	}
	var f struct {
		Entries map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Entries[cacheKey]; !ok {
		t.Errorf("cache file has no entry for %s: %s", cacheKey, data)
	}
}

// TestWithLocalSkillResolution_InstallCredentials checks that the context
// carries the GitHub token as the default install credential when one is
// given, and no default when none is.
func TestWithLocalSkillResolution_InstallCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	newCache := func(t *testing.T) *agent.GitHubResolutionCache {
		c, err := agent.NewGitHubResolutionCache(t.TempDir(), time.Hour, agent.WithResolutionCacheSaveDelay(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	ctx, _ := withLocalSkillResolution(context.Background(), &fakeHubSkills{}, &fakeRegistries{}, "", "cli-token", newCache(t))
	if got := agent.GitHubTokenFromContext(ctx); got != "cli-token" {
		t.Errorf("default install credential = %q, want cli-token", got)
	}
	if got := agent.ResolveProjectIDFromContext(ctx); got != "" {
		t.Errorf("resolve project = %q, want none", got)
	}

	ctx, _ = withLocalSkillResolution(context.Background(), &fakeHubSkills{}, &fakeRegistries{}, "", "", newCache(t))
	if got := agent.GitHubTokenFromContext(ctx); got != "" {
		t.Errorf("default install credential = %q, want none", got)
	}
}
