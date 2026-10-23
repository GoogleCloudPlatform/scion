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

//go:build linux

package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

const (
	crhProject = "0f9e8d7c-6b5a-4321-8fed-cba987654321"
	crhAgentA  = "6f1c2b3a-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	crhAgentB  = "7a2d3c4b-5e6f-4a71-9bac-1d2e3f4a5b6c"
)

func crhWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func crhRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// crhLayout is an nfs workspace Layout on a temp share with legacy Cloud Run
// homes for agents A (stopped) and B (running).
func crhLayout(t *testing.T) (projectfs.Layout, string) {
	t.Helper()
	base := t.TempDir()
	l := projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: base, SubPathRoot: "projects"}
	proj := filepath.Join(base, "projects", crhProject)
	for _, id := range []string{crhAgentA, crhAgentB} {
		crhWrite(t, filepath.Join(proj, "agents", id, "home", ".bashrc"), "home of "+id)
		crhWrite(t, filepath.Join(proj, "agents", id, "secrets", "token"), "secret of "+id)
	}
	return l, proj
}

var crhAgents = []AgentRef{{ID: crhAgentA, Slug: "dev", Stopped: true}, {ID: crhAgentB, Slug: "ops", Stopped: false}}

// A stopped agent's legacy Cloud Run home and secrets move to the layout's
// agents/<slug>/{home-,secrets-}<agent ID>; a running agent's are deferred and
// left in place; a second run reports no moves for the stopped agent.
func TestCloudRunHomes_MovesStoppedDefersRunning(t *testing.T) {
	l, proj := crhLayout(t)

	plan := cloudRunHomes(l, crhProject, crhAgents, Options{DryRun: true})
	for _, r := range plan {
		if r.Failed() {
			t.Fatalf("dry run failed: %+v", r)
		}
	}
	if _, err := os.Stat(filepath.Join(proj, "agents", "dev")); !os.IsNotExist(err) {
		t.Fatal("a dry run must change nothing")
	}

	res := cloudRunHomes(l, crhProject, crhAgents, Options{})
	byTarget := map[string]Result{}
	for _, r := range res {
		if r.Failed() {
			t.Fatalf("step failed: %+v", r)
		}
		byTarget[r.Target] = r
	}
	if got := crhRead(t, filepath.Join(proj, "agents", "dev", "home-"+crhAgentA, ".bashrc")); got != "home of "+crhAgentA {
		t.Errorf("moved home = %q", got)
	}
	if got := crhRead(t, filepath.Join(proj, "agents", "dev", "secrets-"+crhAgentA, "token")); got != "secret of "+crhAgentA {
		t.Errorf("moved secrets = %q", got)
	}
	if _, err := os.Stat(filepath.Join(proj, "agents", crhAgentA, "home")); !os.IsNotExist(err) {
		t.Errorf("the stopped agent's legacy home should be gone (renamed), err %v", err)
	}
	opsHome := filepath.Join(proj, "agents", "ops", "home-"+crhAgentB)
	if r, ok := byTarget[opsHome]; !ok || r.Outcome != OutcomeDeferred || !r.Moved() {
		t.Errorf("running agent's home result = %+v, want deferred (counted as left to move)", r)
	}
	if got := crhRead(t, filepath.Join(proj, "agents", crhAgentB, "home", ".bashrc")); got != "home of "+crhAgentB {
		t.Errorf("the running agent's legacy home must be left in place, got %q", got)
	}
	if _, err := os.Stat(opsHome); !os.IsNotExist(err) {
		t.Errorf("nothing may be created for the running agent (err %v)", err)
	}

	again := cloudRunHomes(l, crhProject, []AgentRef{crhAgents[0]}, Options{})
	for _, r := range again {
		if r.Moved() || r.Failed() {
			t.Errorf("second run for the stopped agent: %+v, want no moves", r)
		}
	}

	// Once stopped, the deferred agent moves too.
	stopped := []AgentRef{{ID: crhAgentB, Slug: "ops", Stopped: true}}
	for _, r := range cloudRunHomes(l, crhProject, stopped, Options{}) {
		if r.Failed() {
			t.Fatalf("move after stop failed: %+v", r)
		}
	}
	if got := crhRead(t, filepath.Join(opsHome, ".bashrc")); got != "home of "+crhAgentB {
		t.Errorf("home after stop = %q", got)
	}
}

// An existing target is never overwritten: files it lacks are copied, a
// conflicting file is reported and the target's version kept.
func TestCloudRunHomes_NeverOverwrites(t *testing.T) {
	l, proj := crhLayout(t)
	target := filepath.Join(proj, "agents", "dev", "home-"+crhAgentA)
	crhWrite(t, filepath.Join(target, ".bashrc"), "already here")
	res := cloudRunHomes(l, crhProject, crhAgents[:1], Options{})
	var home Result
	for _, r := range res {
		if r.Target == target {
			home = r
		}
	}
	if home.Outcome != OutcomeMerged || len(home.Conflicts) != 1 || home.Conflicts[0] != ".bashrc" {
		t.Fatalf("home result = %+v, want merged with the .bashrc conflict", home)
	}
	if got := crhRead(t, filepath.Join(target, ".bashrc")); got != "already here" {
		t.Errorf("target overwritten: %q", got)
	}
}

// A symlinked legacy source is refused, and invalid agent IDs or slugs never
// build a path.
func TestCloudRunHomes_RefusesSymlinksAndBadIDs(t *testing.T) {
	l, proj := crhLayout(t)
	legacyHome := filepath.Join(proj, "agents", crhAgentA, "home")
	if err := os.RemoveAll(legacyHome); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), legacyHome); err != nil {
		t.Fatal(err)
	}
	res := cloudRunHomes(l, crhProject, crhAgents[:1], Options{})
	refused := false
	for _, r := range res {
		if strings.HasSuffix(r.Source, filepath.Join(crhAgentA, "home")) && r.Failed() && strings.Contains(r.Err.Error(), "symlink") {
			refused = true
		}
	}
	if !refused {
		t.Errorf("results = %+v, want the symlinked home refused", res)
	}

	for _, a := range []AgentRef{{ID: "agent-1", Slug: "dev", Stopped: true}, {ID: "../" + crhAgentA, Slug: "dev", Stopped: true}} {
		if got := cloudRunHomes(l, crhProject, []AgentRef{a}, Options{}); len(got) != 0 {
			t.Errorf("agent ID %q: results %+v, want none", a.ID, got)
		}
	}
	bad := cloudRunHomes(l, crhProject, []AgentRef{{ID: crhAgentA, Slug: "../x", Stopped: true}}, Options{})
	if len(bad) != 1 || !bad[0].Failed() {
		t.Errorf("bad slug: %+v, want one failure", bad)
	}
}

// Run wires step 5 for projects on a shared workspace Layout.
func TestRun_CloudRunHomesStep(t *testing.T) {
	l, proj := crhLayout(t)
	rep, err := Run(context.Background(), HubInput{
		Workspace:  l,
		SharedDirs: projectfs.Layout{Backend: projectfs.BackendLocal},
		GlobalDir:  t.TempDir(),
		Projects:   []ProjectRef{{ID: crhProject, Slug: "web", NoWorkspace: true, Agents: crhAgents[:1]}},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		t.Fatalf("report failed: %+v", rep)
	}
	if got := crhRead(t, filepath.Join(proj, "agents", "dev", "home-"+crhAgentA, ".bashrc")); got != "home of "+crhAgentA {
		t.Errorf("home via Run = %q", got)
	}
}
