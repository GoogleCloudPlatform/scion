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

package hub

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fixtureCensusRepoRoot returns the repository root (two levels above
// pkg/hub, where go test runs this package).
func fixtureCensusRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}
	return root
}

// fixtureCensusMatch is one source line matching a census pattern.
type fixtureCensusMatch struct {
	file string // repository-relative, slash-separated
	line int
	text string
	src  string // whole file
	off  int    // byte offset of the line in src
}

// fixtureCensusScan returns every line of every non-test, non-generated Go
// file in the repository that matches re. pkg/ent (generated code,
// including the schema package, which has no create calls), vendored and
// web trees are skipped.
func fixtureCensusScan(t *testing.T, re *regexp.Regexp) []fixtureCensusMatch {
	t.Helper()
	root := fixtureCensusRepoRoot(t)
	var out []fixtureCensusMatch
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case rel == ".git", rel == "pkg/ent", rel == "web", rel == "docs-site",
				strings.HasPrefix(d.Name(), ".") && rel != ".",
				d.Name() == "node_modules", d.Name() == "vendor", d.Name() == "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(data)
		off := 0
		for i, line := range strings.Split(src, "\n") {
			if re.MatchString(line) {
				out = append(out, fixtureCensusMatch{file: rel, line: i + 1, text: line, src: src, off: off})
			}
			off += len(line) + 1
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// enclosingFunc returns the source of the top-level function containing m.
func (m fixtureCensusMatch) enclosingFunc() string {
	start := strings.LastIndex(m.src[:m.off], "\nfunc ")
	if start < 0 {
		start = 0
	}
	end := strings.Index(m.src[m.off:], "\nfunc ")
	if end < 0 {
		return m.src[start:]
	}
	return m.src[start : m.off+end]
}

// TestFixtureProvenanceCensus_RawCreateBuilders: the only raw ent create
// builders for agents, projects, schedules and groups are the four store
// create methods, which write the stamp the model carries (and nothing
// else can write it: the columns are Immutable). There is no CreateBulk or
// MapCreateBulk for them anywhere.
func TestFixtureProvenanceCensus_RawCreateBuilders(t *testing.T) {
	re := regexp.MustCompile(`\.(Agent|Project|Schedule|Group)\.(Create|CreateBulk|MapCreateBulk)\(`)
	allowed := map[string]string{
		"pkg/store/entadapter/agent_store.go":    "s.client.Agent.Create()",
		"pkg/store/entadapter/project_store.go":  "s.client.Project.Create()",
		"pkg/store/entadapter/schedule_store.go": "s.client.Schedule.Create()",
		"pkg/store/entadapter/group_store.go":    "s.client.Group.Create()",
	}
	seen := map[string]int{}
	for _, m := range fixtureCensusScan(t, re) {
		want, ok := allowed[m.file]
		if !ok || !strings.Contains(m.text, want) {
			t.Errorf("unclassified raw create builder at %s:%d: %s\n"+
				"An agent/project/schedule/group row must be created through the store's create method, "+
				"which writes the fixture provenance stamp; see pkg/hub/fixture_provenance.go.",
				m.file, m.line, strings.TrimSpace(m.text))
			continue
		}
		seen[m.file]++
	}
	for f := range allowed {
		if seen[f] != 1 {
			t.Errorf("%s: want exactly one raw create builder, found %d", f, seen[f])
		}
	}
}

// fixtureCreateSite classifies one call of a store create method (or a
// same-named client method) outside tests.
type fixtureCreateSite struct {
	file       string
	lineSubstr string
	// stamped: the enclosing function must call a stamp helper from
	// fixture_provenance.go before the create.
	stamped bool
	why     string
}

// fixtureCreateSites is the census of every non-test call of CreateAgent,
// CreateProject, CreateGroup and CreateSchedule in the repository. It is
// the create-path list of ptone/scion#4240 Phase 3a: each entry says how
// the row is stamped, or why it cannot be rooted at a test identity.
var fixtureCreateSites = []fixtureCreateSite{
	// ── Stamped hub create paths ──────────────────────────────────────
	{"pkg/hub/agent_create_tx.go", "tx.CreateAgent(ctx, agent)", true,
		"commitAgentCreate: every agent row (both HTTP create routes and the scheduled dispatch_agent fire); stampAgent from the edge delegator, in the create transaction"},
	{"pkg/hub/handlers_projects_core.go", "tx.CreateProject(ctx, project)", true,
		"createProjectWithOwner: project create, register and clone; stampProject from the request principal, in the create transaction"},
	{"pkg/hub/handlers_projects_core.go", "s.store.CreateGroup(ctx, projectGroup)", true,
		"createProjectGroup: the project's implicit agents group; stampProjectGroup copies the project's stamp"},
	{"pkg/hub/handlers_projects_core.go", "s.store.CreateGroup(ctx, membersGroup)", true,
		"createProjectMembersGroup: the project's members group; stampProjectGroup copies the project's stamp"},
	{"pkg/hub/handlers_groups.go", "s.store.CreateGroup(ctx, group)", true,
		"group API create; stampGroup from the request principal"},
	{"pkg/hub/handlers_schedules.go", "s.store.CreateSchedule(r.Context(), &schedule)", true,
		"schedule API create; stampSchedule from the request principal"},

	// ── System paths: no principal, so never fixture-rooted ───────────
	{"pkg/hub/flat_runtime_brokers.go", "s.store.CreateProject(ctx, p)", false,
		"ensure the global project at startup/broker registration; system-created, no principal"},
	{"pkg/hub/seed.go", "s.CreateGroup(ctx, group)", false,
		"hub seeding of built-in groups at startup; system-created, no principal"},
	{"cmd/server_broker.go", "s.CreateProject(ctx, globalProject)", false,
		"combo-server bootstrap of the global project; system-created, no principal"},
	{"perf/bench/seed/main.go", "s.CreateProject(ctx, project)", false,
		"benchmark seeding tool writing a store directly; no hub principal exists"},
	{"perf/bench/seed/main.go", "s.CreateAgent(ctx, agent)", false,
		"benchmark seeding tool writing a store directly; no hub principal exists"},

	// ── Not store creates (broker / hub client RPCs of the same name) ─
	{"pkg/hub/brokerclient.go", "c.transport.CreateAgent(", false, "broker RPC: starts an agent already created by commitAgentCreate"},
	{"pkg/hub/httpdispatcher.go", "c.transport.CreateAgent(", false, "broker RPC"},
	{"pkg/hub/httpdispatcher.go", "d.client.CreateAgent(", false, "broker RPC"},
	{"pkg/hub/httpdispatcher.go", "d.client.CreateAgent(", false, "broker RPC (retry)"},
	{"pkg/hub/controlchannel_client.go", "c.controlChannel.CreateAgent(", false, "broker RPC over the control channel"},
	{"pkg/hub/controlchannel_client.go", "c.httpClient.CreateAgent(", false, "broker RPC (HTTP fallback)"},
	{"pkg/hub/controlchannel_client.go", "c.httpClient.CreateAgent(", false, "broker RPC (HTTP)"},
	{"pkg/managedagent/google/backend.go", "b.client.CreateAgent(", false, "managed-agent backend API, not a hub row"},
	{"extras/scion-discord/internal/discord/commands.go", "h.hubClient.CreateAgent(", false, "hub API client; the hub stamps the row"},
}

// TestFixtureProvenanceCensus_CreateCallSites: every non-test call of a
// store create method for agents, projects, schedules and groups is in
// fixtureCreateSites, and every hub path classified as stamped calls a
// stamp helper. A new create path fails here until it is stamped or
// classified. pkg/store/storetest is the store conformance suite (test
// support code writing a raw store) and is not a hub path.
func TestFixtureProvenanceCensus_CreateCallSites(t *testing.T) {
	re := regexp.MustCompile(`\.(CreateAgent|CreateProject|CreateGroup|CreateSchedule)\(`)
	stampCall := regexp.MustCompile(`\bstamp(Agent|Project|Group|ProjectGroup|Schedule)\(`)
	used := make([]bool, len(fixtureCreateSites))
	for _, m := range fixtureCensusScan(t, re) {
		if strings.HasPrefix(m.file, "pkg/store/storetest/") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(m.text), "//") || strings.Contains(m.text, "func (") {
			continue
		}
		found := -1
		for i, site := range fixtureCreateSites {
			if !used[i] && site.file == m.file && strings.Contains(m.text, site.lineSubstr) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Errorf("unclassified create call at %s:%d: %s\n"+
				"Stamp the row through a helper in pkg/hub/fixture_provenance.go, or classify the site in fixtureCreateSites.",
				m.file, m.line, strings.TrimSpace(m.text))
			continue
		}
		used[found] = true
		if fixtureCreateSites[found].stamped && !stampCall.MatchString(m.enclosingFunc()) {
			t.Errorf("%s:%d is classified as stamped but its function calls no stamp helper: %s",
				m.file, m.line, strings.TrimSpace(m.text))
		}
	}
	for i, site := range fixtureCreateSites {
		if !used[i] {
			t.Errorf("stale census entry (no matching call): %s %q", site.file, site.lineSubstr)
		}
	}
}

// TestFixtureProvenanceCensus_StampWriters: outside tests, only
// pkg/hub/fixture_provenance.go assigns a model's Fixture stamp. The store
// adapters read it back from the columns.
func TestFixtureProvenanceCensus_StampWriters(t *testing.T) {
	re := regexp.MustCompile(`\.Fixture\s*=[^=]|\bFixture:\s`)
	allowed := map[string]bool{
		"pkg/hub/fixture_provenance.go":          true,
		"pkg/store/entadapter/agent_store.go":    true, // read mapping
		"pkg/store/entadapter/project_store.go":  true, // read mapping
		"pkg/store/entadapter/schedule_store.go": true, // read mapping
		"pkg/store/entadapter/group_store.go":    true, // read mapping
	}
	for _, m := range fixtureCensusScan(t, re) {
		if strings.HasPrefix(strings.TrimSpace(m.text), "//") {
			continue
		}
		if allowed[m.file] {
			if m.file != "pkg/hub/fixture_provenance.go" && !strings.Contains(m.text, "fixtureProvenanceFromColumns(") {
				t.Errorf("%s:%d: the store may only read the stamp back from its columns: %s", m.file, m.line, strings.TrimSpace(m.text))
			}
			continue
		}
		t.Errorf("fixture provenance stamp written outside pkg/hub/fixture_provenance.go at %s:%d: %s",
			m.file, m.line, strings.TrimSpace(m.text))
	}
}
