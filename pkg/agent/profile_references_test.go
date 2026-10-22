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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const (
	refsColocated = "broker-colocated"
	refsOther     = "broker-other"
)

// writeRefsAgentInfo writes agent slug's agent-info.json with body in the
// broker project path projectPath.
func writeRefsAgentInfo(t *testing.T, projectPath, slug, body string) {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(projectDir, slug)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sumCounted is the counted agents per profile.
func sumCounted(counted []CountedAgent) map[string]int {
	m := map[string]int{}
	for _, c := range counted {
		m[c.Profile]++
	}
	return m
}

// The core counts as the hub's count did: the disk decides only for a
// co-located agent whose project exists and resolves to a path; unplaced
// agents and agents of a missing project count their applied profile with
// no tally; other-broker agents count nowhere; the counted list sums to
// refs.Agents.
func TestBuildProfileReferencesDetailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir := t.TempDir()
	linked := filepath.Join(t.TempDir(), "linked", config.DotScion)
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	in := ProfileRefInputs{
		Providers: []ProfileRefProvider{
			{ProjectID: "p-linked", LocalPath: linked},
			{ProjectID: "p-native"},
			{ProjectID: "p-global", LocalPath: globalDir},
		},
		Projects: []ProfileRefProject{
			{ID: "p-linked", Slug: "linked", ActiveProfile: "local"},
			{ID: "p-native", Slug: "native"},
			{ID: "p-global", Slug: config.GlobalProjectSlug},
			{ID: "p-noprov", Slug: "noprov", ActiveProfile: "remote"},
			{ID: "p-noslug"},
		},
		Agents: []ProfileRefAgentRow{
			{Slug: "found", ProjectID: "p-linked", RuntimeBrokerID: refsColocated, AppliedProfile: "hubval"},
			{Slug: "emptyfile", ProjectID: "p-native", RuntimeBrokerID: refsColocated, AppliedProfile: "local"},
			{Slug: "missing", ProjectID: "p-native", RuntimeBrokerID: refsColocated, AppliedProfile: "hubval"},
			{Slug: "corrupt", ProjectID: "p-native", RuntimeBrokerID: refsColocated, AppliedProfile: "local"},
			{Slug: "global", ProjectID: "p-global", RuntimeBrokerID: refsColocated},
			{Slug: "unplaced", ProjectID: "p-native", AppliedProfile: "hubval"},
			{Slug: "other", ProjectID: "p-native", RuntimeBrokerID: refsOther, AppliedProfile: "remote"},
			{Slug: "noproject", ProjectID: "p-gone", RuntimeBrokerID: refsColocated, AppliedProfile: "orphan"},
			{Slug: "nopath", ProjectID: "p-noslug", RuntimeBrokerID: refsColocated, AppliedProfile: "nopath"},
			{Slug: "noprov", ProjectID: "p-noprov", RuntimeBrokerID: refsColocated, AppliedProfile: "np"},
		},
	}
	writeRefsAgentInfo(t, linked, "found", `{"profile":"local"}`)
	nativePath := config.BrokerProjectPath(globalDir, "", "native")
	writeRefsAgentInfo(t, nativePath, "emptyfile", `{"name":"emptyfile"}`)
	writeRefsAgentInfo(t, nativePath, "corrupt", `{not json`)
	// The global project keeps its slug, so its path is the provider path.
	writeRefsAgentInfo(t, globalDir, "global", `{"profile":"remote"}`)
	// An agent on another broker is never read: a file here must not count.
	writeRefsAgentInfo(t, nativePath, "other", `{"profile":"disk-other"}`)

	refs, counted, ok := BuildProfileReferencesDetailed(globalDir, refsColocated, in)
	if !ok {
		t.Fatal("ok = false with a co-located broker ID")
	}
	wantAgents := map[string]int{"local": 1, "": 2, "hubval": 2, "remote": 1, "orphan": 1, "nopath": 1, "np": 1}
	if !reflect.DeepEqual(refs.Agents, wantAgents) {
		t.Fatalf("refs.Agents = %v, want %v", refs.Agents, wantAgents)
	}
	if want := map[string]int{"local": 1}; !reflect.DeepEqual(refs.Projects, want) {
		t.Fatalf("refs.Projects = %v, want %v (only provided projects count)", refs.Projects, want)
	}
	// missing: "missing" and "noprov" (its hub-managed path has no file);
	// unreadable: "corrupt". Unplaced, project-less and path-less agents
	// raise neither.
	if refs.AgentInfoMissing != 2 || refs.AgentInfoUnreadable != 1 {
		t.Fatalf("missing %d unreadable %d, want 2 and 1", refs.AgentInfoMissing, refs.AgentInfoUnreadable)
	}
	if got := sumCounted(counted); !reflect.DeepEqual(got, refs.Agents) {
		t.Fatalf("counted list sums to %v, refs.Agents is %v", got, refs.Agents)
	}
	var orphan CountedAgent
	for _, c := range counted {
		if c.AgentSlug == "noproject" {
			orphan = c
		}
	}
	if orphan != (CountedAgent{ProjectID: "p-gone", AgentSlug: "noproject", Profile: "orphan"}) {
		t.Fatalf("project-less agent counted as %+v, want no project slug", orphan)
	}

	plain, plainOK := BuildProfileReferences(globalDir, refsColocated, in)
	if !plainOK || !reflect.DeepEqual(plain, refs) {
		t.Fatalf("BuildProfileReferences = %+v, want the detailed refs %+v", plain, refs)
	}
	if r, c, ok := BuildProfileReferencesDetailed(globalDir, "", in); ok || c != nil || r.Agents != nil {
		t.Fatalf("empty broker ID = (%+v, %v, %v), want zero, nil, false", r, c, ok)
	}
}

// A shared-workspace project's unreadable agent-info.json falls back to the
// provisioned profile in the external agents root: SharedWorkspace decides
// where that record is read.
func TestBuildProfileReferencesDetailed_SharedWorkspaceProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir := t.TempDir()
	// A linked project, so its own agents root and the external agents
	// root differ.
	projectPath := filepath.Join(t.TempDir(), "shared", config.DotScion)
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRefsAgentInfo(t, projectPath, "sh", `{not json`)
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	agentDir, _, err := AgentStateDir(projectDir, "sh", true, "p-shared", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeImageProvenance(agentDir, imageProvenance{Profile: "shprov"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		shared bool
		want   string
	}{{true, "shprov"}, {false, ""}} {
		in := ProfileRefInputs{
			Providers: []ProfileRefProvider{{ProjectID: "p-shared", LocalPath: projectPath}},
			Projects:  []ProfileRefProject{{ID: "p-shared", Slug: "shared", SharedWorkspace: c.shared}},
			Agents:    []ProfileRefAgentRow{{Slug: "sh", ProjectID: "p-shared", RuntimeBrokerID: refsColocated, AppliedProfile: "hubval"}},
		}
		refs, _, _ := BuildProfileReferencesDetailed(globalDir, refsColocated, in)
		if want := map[string]int{c.want: 1}; !reflect.DeepEqual(refs.Agents, want) || refs.AgentInfoUnreadable != 1 {
			t.Fatalf("shared=%v: refs.Agents %v unreadable %d, want %v and 1", c.shared, refs.Agents, refs.AgentInfoUnreadable, want)
		}
	}
}

func TestCountsOnBroker(t *testing.T) {
	cases := []struct {
		agentBroker string
		want        bool
	}{
		{"", true},          // unplaced
		{"co-1", true},      // on the co-located broker
		{"remote-2", false}, // on another broker
	}
	for _, c := range cases {
		if got := CountsOnBroker(c.agentBroker, "co-1"); got != c.want {
			t.Errorf("CountsOnBroker(%q, %q) = %v, want %v", c.agentBroker, "co-1", got, c.want)
		}
	}
}
