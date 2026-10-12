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

package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// cliContextTestHome points HOME at a fresh directory and returns the path
// context.yaml will have.
func cliContextTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, GlobalDir, cliContextFileName)
}

func writeCLIContextFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCLIContextMissingFileIsEmpty(t *testing.T) {
	path := cliContextTestHome(t)
	c, err := LoadCLIContext()
	if err != nil {
		t.Fatalf("LoadCLIContext: %v", err)
	}
	if c.Version != CLIContextVersion || c.CurrentHub != "" || len(c.Hubs) != 0 || len(c.Projects) != 0 {
		t.Fatalf("missing file: got %+v, want an empty context", c)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadCLIContext must not create the file: stat err = %v", err)
	}
}

func TestCLIContextRoundTrip(t *testing.T) {
	cliContextTestHome(t)
	now := time.Date(2026, 10, 11, 19, 2, 11, 0, time.UTC)
	err := UpdateCLIContext(func(c *CLIContext) error {
		name, created := c.EnsureHub("https://hub.example.com", "example")
		if !created || name != "example" {
			t.Fatalf("EnsureHub = %q, %v", name, created)
		}
		c.Refreshed = map[string]time.Time{"example": now}
		return c.SelectProject(name, KnownProject{ID: "1dfdd6c7-0000-0000-0000-000000000001", Slug: "foo", Name: "Foo"}, now)
	})
	if err != nil {
		t.Fatalf("UpdateCLIContext: %v", err)
	}

	c, err := LoadCLIContext()
	if err != nil {
		t.Fatalf("LoadCLIContext: %v", err)
	}
	if c.Version != 1 || c.CurrentHub != "example" {
		t.Fatalf("got version %d current_hub %q", c.Version, c.CurrentHub)
	}
	h := c.Hubs["example"]
	if h == nil || h.Endpoint != "https://hub.example.com" {
		t.Fatalf("hub entry = %+v", h)
	}
	if h.CurrentProject == nil || h.CurrentProject.ID != "1dfdd6c7-0000-0000-0000-000000000001" || h.CurrentProject.Slug != "foo" {
		t.Fatalf("current_project = %+v", h.CurrentProject)
	}
	want := KnownProject{Hub: "example", ID: "1dfdd6c7-0000-0000-0000-000000000001", Slug: "foo", Name: "Foo", Source: KnownProjectSourceUse, LastUsed: now}
	if len(c.Projects) != 1 || !c.Projects[0].LastUsed.Equal(want.LastUsed) {
		t.Fatalf("projects = %+v, want [%+v]", c.Projects, want)
	}
	got := c.Projects[0]
	got.LastUsed = want.LastUsed
	if got != want {
		t.Fatalf("projects = %+v, want [%+v]", c.Projects, want)
	}
	if !c.Refreshed["example"].Equal(now) {
		t.Fatalf("refreshed = %v", c.Refreshed)
	}
}

func TestCLIContextFileMode(t *testing.T) {
	path := cliContextTestHome(t)
	if err := UpdateCLIContext(func(c *CLIContext) error {
		c.EnsureHub("https://hub.example.com", "example")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Join(filepath.Dir(path), cliContextLockFileName)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s: mode %o, want 600", filepath.Base(p), got)
		}
	}

	// A file that was loosened by hand is tightened by the next write.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCLIContext(func(*CLIContext) error { return nil }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("after rewrite: mode %o, want 600", got)
	}
}

func TestCLIContextNewerVersionIsReadOnly(t *testing.T) {
	path := cliContextTestHome(t)
	content := `version: 2
current_hub: prod
future_field: {x: 1}
hubs:
  prod:
    endpoint: https://hub.example.com
    current_project: {id: abc, slug: foo}
    future_hub_field: yes
`
	writeCLIContextFile(t, path, content)

	c, err := LoadCLIContext()
	if err != nil {
		t.Fatalf("LoadCLIContext: %v", err)
	}
	if c.Version != 2 || c.CurrentHub != "prod" {
		t.Fatalf("got %+v", c)
	}
	if p := c.ProjectFor("https://hub.example.com"); p == nil || p.ID != "abc" {
		t.Fatalf("ProjectFor = %+v, want the known fields to be read", p)
	}

	called := false
	err = UpdateCLIContext(func(*CLIContext) error { called = true; return nil })
	want := "context.yaml was written by a newer scion (version 2); upgrade, or remove the file"
	if err == nil || err.Error() != want {
		t.Fatalf("UpdateCLIContext err = %v, want %q", err, want)
	}
	if called {
		t.Fatal("update function ran for a newer file")
	}
	// Forcing does not override the version check either.
	if err := UpdateCLIContextForce(func(*CLIContext) error { return nil }); err == nil || err.Error() != want {
		t.Fatalf("UpdateCLIContextForce err = %v, want %q", err, want)
	}
	if got, _ := os.ReadFile(path); string(got) != content {
		t.Fatalf("file was rewritten:\n%s", got)
	}
}

func TestCLIContextCorruptFile(t *testing.T) {
	cases := map[string]string{
		"not yaml":        "version: [1\n",
		"wrong shape":     "- a\n- b\n",
		"empty":           "",
		"missing version": "current_hub: prod\n",
		"token key":       "version: 1\nhubs:\n  prod:\n    endpoint: https://hub.example.com\n    token: s3cret\n",
		"api key":         "version: 1\nhubs:\n  prod:\n    endpoint: https://hub.example.com\n    apiKey: s3cret\n",
		"null hub entry":  "version: 1\nhubs:\n  prod:\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := cliContextTestHome(t)
			writeCLIContextFile(t, path, content)

			c, err := LoadCLIContext()
			if !errors.Is(err, ErrCLIContextCorrupt) {
				t.Fatalf("LoadCLIContext err = %v, want ErrCLIContextCorrupt", err)
			}
			if c == nil || c.Version != CLIContextVersion || c.CurrentHub != "" || len(c.Hubs) != 0 {
				t.Fatalf("corrupt file: got %+v, want an empty context", c)
			}

			err = UpdateCLIContext(func(*CLIContext) error { return nil })
			if !errors.Is(err, ErrCLIContextCorrupt) {
				t.Fatalf("UpdateCLIContext err = %v, want ErrCLIContextCorrupt", err)
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Fatalf("corrupt file was rewritten without force:\n%s", got)
			}

			if err := UpdateCLIContextForce(func(c *CLIContext) error {
				if len(c.Hubs) != 0 {
					t.Errorf("forced update starts from %+v, want empty", c)
				}
				c.EnsureHub("https://hub.example.com", "example")
				return nil
			}); err != nil {
				t.Fatalf("UpdateCLIContextForce: %v", err)
			}
			c, err = LoadCLIContext()
			if err != nil || c.Hubs["example"] == nil {
				t.Fatalf("after force: %+v, %v", c, err)
			}
		})
	}
}

func TestCLIContextTokenKeyErrorNamesLogin(t *testing.T) {
	path := cliContextTestHome(t)
	writeCLIContextFile(t, path, "version: 1\nhubs:\n  prod:\n    endpoint: https://hub.example.com\n    token: s3cret\n")
	_, err := LoadCLIContext()
	if err == nil || !strings.Contains(err.Error(), "scion hub auth login") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v, want a pointer to 'scion hub auth login' without the value", err)
	}
}

func TestCLIContextUpdateErrorWritesNothing(t *testing.T) {
	path := cliContextTestHome(t)
	sentinel := errors.New("stop")
	if err := UpdateCLIContext(func(c *CLIContext) error {
		c.EnsureHub("https://hub.example.com", "example")
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written despite error: %v", err)
	}
}

// addProject appends a uniquely named project in one update.
func addProject(hub, id string) error {
	return UpdateCLIContext(func(c *CLIContext) error {
		name, _ := c.EnsureHub("https://hub.example.com", hub)
		c.Projects = append(c.Projects, KnownProject{Hub: name, ID: id, Source: KnownProjectSourceRefresh})
		return nil
	})
}

func checkAllProjects(t *testing.T, want []string) {
	t.Helper()
	c, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range c.Projects {
		got[p.ID] = true
	}
	if len(c.Hubs) != 1 {
		t.Errorf("hubs = %v, want one entry", c.Hubs)
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("update for %s was lost", id)
		}
	}
	if len(c.Projects) != len(want) {
		t.Errorf("got %d projects, want %d", len(c.Projects), len(want))
	}
}

func TestCLIContextConcurrentGoroutines(t *testing.T) {
	cliContextTestHome(t)
	const workers, each = 2, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	var want []string
	for w := 0; w < workers; w++ {
		for i := 0; i < each; i++ {
			want = append(want, fmt.Sprintf("g%d-%d", w, i))
		}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				errs <- addProject("example", fmt.Sprintf("g%d-%d", w, i))
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	checkAllProjects(t, want)
}

// TestCLIContextHelperProcess is the helper binary for
// TestCLIContextConcurrentProcesses. It does nothing unless started by it.
func TestCLIContextHelperProcess(t *testing.T) {
	prefix := os.Getenv("SCION_TEST_HELPER_CLI_CONTEXT_PREFIX")
	if prefix == "" {
		t.Skip("helper process only")
	}
	count, err := strconv.Atoi(os.Getenv("SCION_TEST_HELPER_CLI_CONTEXT_COUNT"))
	if err != nil {
		t.Fatal(err)
	}
	barrier := os.Getenv("SCION_TEST_HELPER_CLI_CONTEXT_BARRIER")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start barrier never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < count; i++ {
		if err := addProject("example", fmt.Sprintf("%s-%d", prefix, i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLIContextConcurrentProcesses(t *testing.T) {
	path := cliContextTestHome(t)
	home := filepath.Dir(filepath.Dir(path))
	const procs, each = 2, 25
	barrier := filepath.Join(t.TempDir(), "go")
	var want []string
	cmds := make([]*exec.Cmd, procs)
	outs := make([]*strings.Builder, procs)
	t.Cleanup(func() {
		for _, c := range cmds {
			if c != nil && c.Process != nil && c.ProcessState == nil {
				_ = c.Process.Kill()
			}
		}
	})
	for p := 0; p < procs; p++ {
		prefix := fmt.Sprintf("p%d", p)
		for i := 0; i < each; i++ {
			want = append(want, fmt.Sprintf("%s-%d", prefix, i))
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestCLIContextHelperProcess$")
		cmd.Env = append(os.Environ(),
			"HOME="+home,
			"SCION_TEST_HELPER_CLI_CONTEXT_PREFIX="+prefix,
			"SCION_TEST_HELPER_CLI_CONTEXT_COUNT="+strconv.Itoa(each),
			"SCION_TEST_HELPER_CLI_CONTEXT_BARRIER="+barrier,
		)
		outs[p] = &strings.Builder{}
		cmd.Stdout = outs[p]
		cmd.Stderr = outs[p]
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", p, err)
		}
		cmds[p] = cmd
	}
	// Let both helpers reach the barrier so their updates overlap.
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for p, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", p, err, outs[p])
		}
	}
	checkAllProjects(t, want)
}

func TestCLIContextProjectFor(t *testing.T) {
	c := &CLIContext{Version: 1, Hubs: map[string]*HubEntry{
		"prod":  {Endpoint: "https://Hub.Example.com/", CurrentProject: &ProjectRef{ID: "p1", Slug: "foo"}},
		"other": {Endpoint: "https://other.example.com"},
	}}
	if p := c.ProjectFor("https://hub.example.com"); p == nil || p.ID != "p1" {
		t.Errorf("ProjectFor(normalised spelling) = %+v, want p1", p)
	}
	if p := c.ProjectFor("https://other.example.com"); p != nil {
		t.Errorf("hub without a project: got %+v", p)
	}
	if p := c.ProjectFor("https://unknown.example.com"); p != nil {
		t.Errorf("unknown hub: got %+v", p)
	}
	if p := c.ProjectFor(""); p != nil {
		t.Errorf("empty endpoint: got %+v", p)
	}
}

func TestCLIContextEnsureHub(t *testing.T) {
	c := &CLIContext{Version: 1}
	if name, created := c.EnsureHub("https://hub.example.com", "hub"); name != "hub" || !created {
		t.Fatalf("first = %q %v", name, created)
	}
	if name, created := c.EnsureHub("https://hub.example.com/", "other"); name != "hub" || created {
		t.Fatalf("same endpoint = %q %v, want the existing entry", name, created)
	}
	if name, _ := c.EnsureHub("https://hub.other.com", "hub"); name != "hub-2" {
		t.Fatalf("collision = %q, want hub-2", name)
	}
	if name, _ := c.EnsureHub("https://hub.third.com", "hub"); name != "hub-3" {
		t.Fatalf("second collision = %q, want hub-3", name)
	}
	if name, _ := c.EnsureHub("https://x.example.com", "Not Valid"); name != "hub-4" {
		t.Fatalf("invalid base = %q, want hub-4", name)
	}
}

func TestDeriveHubName(t *testing.T) {
	cases := map[string]string{
		"https://hub.example.com":       "hub",
		"https://Prod.Example.com:443/": "prod",
		"http://localhost:8080":         "localhost",
		"http://127.0.0.1:8080":         "127-0-0-1",
		"http://[::1]:8080":             "1",
		"https://my_hub.example.com":    "my-hub",
		"not a url":                     "hub",
		"":                              "hub",
	}
	for in, want := range cases {
		if got := DeriveHubName(in); got != want {
			t.Errorf("DeriveHubName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCLIContextSelectAndClear(t *testing.T) {
	c := &CLIContext{Version: 1}
	name, _ := c.EnsureHub("https://hub.example.com", "prod")
	t0 := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	c.Projects = []KnownProject{{Hub: "prod", ID: "p1", Slug: "old", Source: KnownProjectSourceRefresh}}
	if err := c.SelectProject(name, KnownProject{ID: "p1", Slug: "foo", Name: "Foo"}, t0); err != nil {
		t.Fatal(err)
	}
	if len(c.Projects) != 1 || c.Projects[0].Slug != "foo" || c.Projects[0].Source != KnownProjectSourceUse {
		t.Fatalf("projects = %+v, want one updated 'use' entry", c.Projects)
	}
	if err := c.SelectProject("missing", KnownProject{ID: "p1"}, t0); err == nil {
		t.Fatal("SelectProject on an unknown hub succeeded")
	}
	hub, cleared := c.ClearCurrentProject()
	if hub != "prod" || cleared == nil || cleared.ID != "p1" {
		t.Fatalf("ClearCurrentProject = %q %+v", hub, cleared)
	}
	if c.CurrentHub != "prod" || c.Hubs["prod"].CurrentProject != nil {
		t.Fatalf("after clear: %+v", c)
	}
	if len(c.Projects) != 1 {
		t.Fatalf("clear must keep the known list: %+v", c.Projects)
	}
}

// TestCLIContextOptionalV1Fields: the optional version-1 fields (a project's
// local_path, a hub's transport, the imported record) are read from the
// documented YAML keys and survive an update that does not touch them.
func TestCLIContextOptionalV1Fields(t *testing.T) {
	path := cliContextTestHome(t)
	imported := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	writeCLIContextFile(t, path, `version: 1
current_hub: prod
hubs:
  prod:
    endpoint: https://hub.example.com
    transport: {mode: iap, audience: client-id.apps.example.com}
projects:
  - {hub: prod, id: p1, slug: foo, source: link, local_path: /src/foo}
imported:
  global: 2026-10-12T03:00:00Z
`)
	check := func(c *CLIContext) {
		t.Helper()
		h := c.Hubs["prod"]
		if h == nil || h.Transport == nil || h.Transport.Mode != "iap" || h.Transport.Audience != "client-id.apps.example.com" {
			t.Fatalf("transport = %+v", h)
		}
		if len(c.Projects) != 1 || c.Projects[0].LocalPath != "/src/foo" {
			t.Fatalf("projects = %+v", c.Projects)
		}
		if c.Imported == nil || !c.Imported.Global.Equal(imported) {
			t.Fatalf("imported = %+v", c.Imported)
		}
	}
	c, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	check(c)

	if err := UpdateCLIContext(func(c *CLIContext) error {
		c.EnsureHub("https://other.example.com", "other")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err = LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	check(c)
	if c.Hubs["other"] == nil {
		t.Fatal("the update was lost")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"transport:", "local_path: /src/foo", "imported:", "global: 2026-10-12T03:00:00Z"} {
		if !strings.Contains(string(data), key) {
			t.Errorf("written file lacks %q:\n%s", key, data)
		}
	}
}

// TestCLIContextOptionalV1FieldsOmittedWhenEmpty: a context without the
// optional fields writes none of their keys.
func TestCLIContextOptionalV1FieldsOmittedWhenEmpty(t *testing.T) {
	path := cliContextTestHome(t)
	if err := UpdateCLIContext(func(c *CLIContext) error {
		name, _ := c.EnsureHub("https://hub.example.com", "prod")
		return c.SelectProject(name, KnownProject{ID: "p1", Slug: "foo"}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"transport", "local_path", "imported"} {
		if strings.Contains(string(data), key) {
			t.Errorf("written file has %q although it is unset:\n%s", key, data)
		}
	}
}
