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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func writeAgentConfig(t *testing.T, projectDir, agentName string, cfg api.ScionConfig) string {
	t.Helper()
	agentDir := config.GetAgentDir(projectDir, agentName, false)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}
	return cfgPath
}

func readAgentSkills(t *testing.T, cfgPath string) []api.SkillReference {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read scion-agent.json: %v", err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to parse scion-agent.json: %v", err)
	}
	return cfg.Skills
}

// TestApplyInlineConfigUpdate_RepeatedStartsDoNotGrowSkills verifies that
// applying the same inline config on every start leaves the skill list in
// scion-agent.json unchanged instead of appending another copy each time.
func TestApplyInlineConfigUpdate_RepeatedStartsDoNotGrowSkills(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "skills-agent"

	inline := &api.ScionConfig{Skills: []api.SkillReference{
		{URI: "skill://scion/global/alpha", Scope: "hub"},
		{URI: "gh://example/repo/skills/beta"},
		// Same URI under a different install name is kept as its own entry.
		{URI: "gh://example/repo/skills/beta", As: "beta-2"},
	}}

	// The provisioned file already holds the skills from the create-time
	// inline config (plus one template skill the Hub does not resend).
	provisioned := config.MergeScionConfig(&api.ScionConfig{
		Harness: "claude",
		Skills:  []api.SkillReference{{URI: "skill://scion/global/from-template", Scope: "template"}},
	}, inline)
	cfgPath := writeAgentConfig(t, projectDir, agentName, *provisioned)
	want := readAgentSkills(t, cfgPath)
	if len(want) != 4 {
		t.Fatalf("fixture: expected 4 provisioned skills, got %d: %+v", len(want), want)
	}

	for i := 0; i < 3; i++ { // start, restart, restart
		srv.applyInlineConfigUpdate(agentName, projectDir, inline, false)
		got := readAgentSkills(t, cfgPath)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after apply #%d: skills changed\n got: %+v\nwant: %+v", i+1, got, want)
		}
	}
}

// TestApplyInlineConfigUpdate_CollapsesExistingDuplicates verifies that a
// scion-agent.json already carrying repeated skill entries is reduced to one
// entry per URI and install name on the next start.
func TestApplyInlineConfigUpdate_CollapsesExistingDuplicates(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "dup-agent"

	alpha := api.SkillReference{URI: "skill://scion/global/alpha", Scope: "template"}
	beta := api.SkillReference{URI: "skill://scion/global/beta", Scope: "template"}
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{
		Skills: []api.SkillReference{alpha, beta, alpha, beta, alpha, beta},
	})

	srv.applyInlineConfigUpdate(agentName, projectDir, &api.ScionConfig{
		Skills: []api.SkillReference{{URI: alpha.URI}, {URI: beta.URI}},
	}, false)

	got := readAgentSkills(t, cfgPath)
	if want := []api.SkillReference{alpha, beta}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skills not collapsed\n got: %+v\nwant: %+v", got, want)
	}
}

// TestApplyInlineConfigUpdate_HubEntryReplacesExisting verifies, through
// MergeScionConfig as well as the collapse, that the entry the Hub sends
// replaces an existing entry for the same skill whatever the scopes, and
// moves to its own position at the end.
func TestApplyInlineConfigUpdate_HubEntryReplacesExisting(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "replace-agent"

	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{
		Skills: []api.SkillReference{
			{URI: "skill://scion/global/alpha", Scope: "template"},
			{URI: "skill://scion/global/beta", Scope: "project"},
			{URI: "skill://scion/global/gamma", Scope: "template"},
		},
	})

	srv.applyInlineConfigUpdate(agentName, projectDir, &api.ScionConfig{
		Skills: []api.SkillReference{
			{URI: "skill://scion/global/alpha", Optional: true, Scope: "project"},
			// A lower-ranked scope than the existing entry still replaces it.
			{URI: "skill://scion/global/beta", Optional: true, Scope: "hub"},
		},
	}, false)

	want := []api.SkillReference{
		{URI: "skill://scion/global/gamma", Scope: "template"},
		{URI: "skill://scion/global/alpha", Optional: true, Scope: "project"},
		{URI: "skill://scion/global/beta", Optional: true, Scope: "hub"},
	}
	if got := readAgentSkills(t, cfgPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("skills after apply\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDedupeSkillReferences(t *testing.T) {
	tests := []struct {
		name string
		in   []api.SkillReference
		want []api.SkillReference
	}{
		{name: "nil", in: nil, want: nil},
		{
			name: "single",
			in:   []api.SkillReference{{URI: "a"}},
			want: []api.SkillReference{{URI: "a"}},
		},
		{
			name: "distinct URIs kept in order",
			in:   []api.SkillReference{{URI: "b"}, {URI: "a"}},
			want: []api.SkillReference{{URI: "b"}, {URI: "a"}},
		},
		{
			name: "same URI different As kept",
			in:   []api.SkillReference{{URI: "a"}, {URI: "a", As: "x"}, {URI: "a", As: "y"}},
			want: []api.SkillReference{{URI: "a"}, {URI: "a", As: "x"}, {URI: "a", As: "y"}},
		},
		{
			name: "final occurrence kept at its own position",
			in: []api.SkillReference{
				{URI: "a", Scope: "template"},
				{URI: "b"},
				{URI: "a", Optional: true, Scope: "hub"},
			},
			want: []api.SkillReference{
				{URI: "b"},
				{URI: "a", Optional: true, Scope: "hub"},
			},
		},
		{
			name: "later lower-ranked scope replaces an earlier higher-ranked one",
			in: []api.SkillReference{
				{URI: "a", Scope: "project"},
				{URI: "a", Optional: true, Scope: "template"},
			},
			want: []api.SkillReference{
				{URI: "a", Optional: true, Scope: "template"},
			},
		},
		{
			name: "later required entry replaces an earlier optional one",
			in: []api.SkillReference{
				{URI: "a", Optional: true, Scope: "hub"},
				{URI: "a", Scope: "user"},
			},
			want: []api.SkillReference{
				{URI: "a", Scope: "user"},
			},
		},
		{
			name: "survivors keep the latest relative order",
			in: []api.SkillReference{
				{URI: "skill://scion/global/foo", Scope: "hub"},
				{URI: "skill://scion/project/p/foo", Scope: "hub"},
				{URI: "skill://scion/project/p/foo", Scope: "hub"},
				{URI: "skill://scion/global/foo", Scope: "hub"},
			},
			want: []api.SkillReference{
				{URI: "skill://scion/project/p/foo", Scope: "hub"},
				{URI: "skill://scion/global/foo", Scope: "hub"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dedupeSkillReferences(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// resolveEchoSkillService is a hub skill service whose Resolve reports every
// requested reference as resolved, one entry per reference in request order,
// as the Hub's resolve handler does.
type resolveEchoSkillService struct {
	hubclient.SkillService
}

func (resolveEchoSkillService) Resolve(_ context.Context, req *hubclient.ResolveSkillsRequest) (*hubclient.ResolveSkillsResponse, error) {
	resp := &hubclient.ResolveSkillsResponse{}
	for _, ref := range req.Skills {
		resp.Resolved = append(resp.Resolved, hubclient.ResolvedSkill{
			URI: ref.URI, Name: filepath.Base(ref.URI), ResolvedVersion: "1", ContentHash: "h-" + ref.URI,
		})
	}
	return resp, nil
}

// installView reduces a resolve result to what install keeps of it. The Hub
// resolver gives every entry for a URI the same metadata (from the last
// reference for that URI), so those entries share a destination name and a
// scope, and the destination-name collapse at install keeps the later one at
// its own position. Keeping the final entry per URI is therefore the install
// outcome for same-URI duplicates.
func installView(res *agent.ResolveResult) []agent.ResolvedSkill {
	last := map[string]int{}
	for i, rs := range res.Resolved {
		last[rs.URI] = i
	}
	var out []agent.ResolvedSkill
	for i, rs := range res.Resolved {
		if last[rs.URI] == i {
			out = append(out, rs)
		}
	}
	return out
}

// TestDedupeSkillReferences_HubResolveUnchanged pins the property the dedupe
// relies on: the Hub resolver collapses same-URI references last-wins, so
// the collapsed list installs exactly what the original list did, including
// As, Scope, Optional and order.
func TestDedupeSkillReferences_HubResolveUnchanged(t *testing.T) {
	lists := map[string][]api.SkillReference{
		"user required then hub optional": {
			{URI: "skill://scion/global/x", Scope: "user"},
			{URI: "skill://scion/global/x", Optional: true, Scope: "hub"},
		},
		"project then template": {
			{URI: "skill://scion/global/x", Scope: "project"},
			{URI: "skill://scion/global/y", Scope: "template"},
			{URI: "skill://scion/global/x", Optional: true, Scope: "template"},
		},
		"different install names": {
			{URI: "skill://scion/global/x", As: "one", Scope: "hub"},
			{URI: "skill://scion/global/x", As: "two", Optional: true, Scope: "hub"},
			{URI: "skill://scion/global/x", As: "one", Scope: "project"},
		},
		"repeated appends": {
			{URI: "skill://scion/global/x", Scope: "template"},
			{URI: "skill://scion/global/y", Scope: "hub"},
			{URI: "skill://scion/global/x", Scope: "hub"},
			{URI: "skill://scion/global/y", Optional: true, Scope: "hub"},
			{URI: "skill://scion/global/x", Optional: true, Scope: "hub"},
		},
	}
	resolver := agent.NewHubSkillResolver(resolveEchoSkillService{})
	for name, refs := range lists {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			before, err := resolver.Resolve(ctx, refs, agent.ResolveOpts{})
			if err != nil {
				t.Fatalf("resolve original: %v", err)
			}
			deduped := dedupeSkillReferences(refs)
			if len(deduped) >= len(refs) {
				t.Fatalf("fixture: expected duplicates to be collapsed, got %+v", deduped)
			}
			after, err := resolver.Resolve(ctx, deduped, agent.ResolveOpts{})
			if err != nil {
				t.Fatalf("resolve collapsed: %v", err)
			}
			if b, a := installView(before), installView(after); !reflect.DeepEqual(b, a) {
				t.Fatalf("install outcome changed by the collapse\nbefore: %+v\n after: %+v", b, a)
			}
		})
	}
}
