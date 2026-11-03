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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// withHydrationRoots sets the hydration roots for one test and restores the
// previous ones afterwards.
func withHydrationRoots(t *testing.T, roots ...string) {
	t.Helper()
	prev := SetTemplateHydrationRoots(roots...)
	t.Cleanup(func() { SetTemplateHydrationRoots(prev...) })
}

func mkTemplateDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("description: t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A project-scoped lookup (the broker's) refuses an absolute directory
// outside the hydration roots and every remote source form, without any
// fetch; it accepts a directory below a root and a template name; a link
// inside the cache is refused; the CLI form (no project path) still accepts
// an absolute directory on its own host.
func TestFindTemplateInProjectPathRefusesRequestAbsoluteAndURI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	cache := filepath.Join(base, "cache", "templates")
	hydrated := filepath.Join(cache, "abc123")
	mkTemplateDir(t, hydrated)
	outside := filepath.Join(base, "host-dir")
	mkTemplateDir(t, outside)
	if err := os.Symlink(outside, filepath.Join(cache, "linked")); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(base, "project", DotScion)
	mkTemplateDir(t, filepath.Join(project, "templates", "claude"))
	withHydrationRoots(t, cache)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	for name, ref := range map[string]string{
		"absolute outside":     outside,
		"the root itself":      cache,
		"link inside cache":    filepath.Join(cache, "linked"),
		"rclone local backend": ":local:" + outside,
		"rclone gcs backend":   ":gcs:bucket/x",
		"http source":          srv.URL + "/x.tar.gz",
		"https source":         strings.Replace(srv.URL, "http://", "https://", 1) + "/x.tgz",
	} {
		_, err := FindTemplateInProjectPath(ref, project)
		if !errors.Is(err, ErrTemplateRefNotAllowed) {
			t.Errorf("%s: error = %v, want ErrTemplateRefNotAllowed", name, err)
			continue
		}
		if strings.Contains(err.Error(), ref) {
			t.Errorf("%s: error %q echoes the reference", name, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("remote source fetched %d times from a project-scoped lookup", n)
	}

	tpl, err := FindTemplateInProjectPath(hydrated, project)
	if err != nil || tpl.Path != hydrated {
		t.Fatalf("hydrated dir: %+v, %v", tpl, err)
	}
	if tpl, err := FindTemplateInProjectPath("claude", project); err != nil || filepath.Base(tpl.Path) != "claude" {
		t.Fatalf("template name: %+v, %v", tpl, err)
	}
	if tpl, err := FindTemplateInProjectPath(outside, ""); err != nil || tpl.Path != outside {
		t.Fatalf("CLI form with an absolute dir: %+v, %v", tpl, err)
	}
}

// With no hydration roots registered, no absolute directory is accepted by
// a project-scoped lookup.
func TestFindTemplateInProjectPathNoRootsRefusesAbsolute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withHydrationRoots(t)
	dir := filepath.Join(t.TempDir(), "tpl")
	mkTemplateDir(t, dir)
	if _, err := FindTemplateInProjectPath(dir, filepath.Join(t.TempDir(), DotScion)); !errors.Is(err, ErrTemplateRefNotAllowed) {
		t.Fatalf("error = %v, want ErrTemplateRefNotAllowed", err)
	}
}

// An operator default_template may be a remote source: the operator form
// does not refuse it as a request reference (whether the fetch succeeds is
// the fetcher's business).
func TestOperatorDefaultTemplateMayBeRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), DotScion)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	ref := srv.URL + "/t.tar.gz"
	if _, err := FindOperatorTemplateInProjectPath(ref, project); errors.Is(err, ErrTemplateRefNotAllowed) {
		t.Fatalf("operator remote default refused as a request reference: %v", err)
	}
	if _, err := GetOperatorTemplateChainInProject(ref, project); errors.Is(err, ErrTemplateRefNotAllowed) {
		t.Fatalf("operator chain refused the remote default: %v", err)
	}
	if _, err := GetTemplateChainInProject(ref, project); !errors.Is(err, ErrTemplateRefNotAllowed) {
		t.Fatalf("request chain error = %v, want ErrTemplateRefNotAllowed", err)
	}
}

// Registered roots are absolute and de-duplicated; relative ones are ignored.
func TestTemplateHydrationRootsRegistration(t *testing.T) {
	withHydrationRoots(t)
	a := filepath.Join(t.TempDir(), "cache")
	AddTemplateHydrationRoot(a)
	AddTemplateHydrationRoot(a + string(filepath.Separator))
	AddTemplateHydrationRoot("relative/cache")
	AddTemplateHydrationRoot("")
	got := TemplateHydrationRoots()
	if len(got) != 1 || got[0] != a {
		t.Fatalf("roots = %v, want [%s]", got, a)
	}
}
