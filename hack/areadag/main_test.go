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

package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/hack/areadag/dag"
)

const fixtureHub = "example.com/m/pkg/hub"

// runFixture runs areadag over testdata/<name>/{dag.txt,list.json}.
func runFixture(t *testing.T, name string) (int, string, string) {
	t.Helper()
	dir := filepath.Join("testdata", name)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-dag", filepath.Join(dir, "dag.txt"),
		"-list", filepath.Join(dir, "list.json"),
		"-hub", fixtureHub,
	}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

var okLine = regexp.MustCompile(`^areadag: OK: ([1-9][0-9]*) module packages \(([1-9][0-9]*) from go list, (\d+) tag-only directories parsed\), ([1-9][0-9]*) pkg/hub packages, ([1-9][0-9]*) declared \(\d+ leaf, \d+ area, ([1-9][0-9]*) testsupport\), ([1-9][0-9]*) alias files, ([1-9][0-9]*) pkg/hub edges checked, ([1-9][0-9]*) package main closures checked\n$`)

// TestPassingFixtures: each fixture is clean, and the OK line reports a
// nonzero count for every input, so a pass is never vacuous.
func TestPassingFixtures(t *testing.T) {
	for _, name := range []string{
		"ok",
		"testimport",       // a _test.go import of testsupport, default and tagged
		"testsupportchain", // testsupport importing testsupport in non-test code
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runFixture(t, name)
			if code != 0 {
				t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			m := okLine.FindStringSubmatch(stdout)
			if m == nil {
				t.Fatalf("stdout %q does not match the OK line with nonzero counts", stdout)
			}
			// ok also holds a directory go list does not list: the walk must parse it.
			if name == "ok" && m[3] != "1" {
				t.Fatalf("ok: %s tag-only directories parsed, want 1", m[3])
			}
		})
	}
}

// TestFailingFixtures: each fixture plants one violation, which must fail
// the check with exactly that violation named.
func TestFailingFixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"areaarea", []string{"area b imports area a in non-test code, which its dag.txt line does not list"}},
		{"leafarea", []string{"leaf l imports area a in non-test code, which its dag.txt line does not list"}},
		{"leaflistsarea", []string{
			"leaf l lists area b, but b imports a in non-test code",
			"leaf l lists area b, but b imports l in non-test code",
		}},
		{"areahub", []string{"area a imports pkg/hub in test code (no declared package may import pkg/hub)"}},
		{"undeclaredimport", []string{"area a imports undeclared pkg/hub/feature in non-test code"}},
		{"aliasundeclared", []string{"alias file pkg/hub/zz_alias_c_test.go imports pkg/hub/c, which dag.txt does not declare"}},
		{"prodimport", []string{"example.com/m/pkg/x imports testsupport ts in non-test code (x.go)"}},
		{"prodimporttagged", []string{"example.com/m/pkg/x imports testsupport ts in non-test code (x_integration.go)"}},
		// The head of a testsupport chain is still testsupport: production may not import it.
		{"prodimportchain", []string{"example.com/m/pkg/x imports testsupport ts2 in non-test code (x.go)"}},
		{"prodimportcmd", []string{
			"example.com/m/cmd/tool imports testsupport ts in non-test code (main.go)",
			"package main example.com/m/cmd/tool reaches testsupport ts through non-test imports (example.com/m/cmd/tool -> example.com/m/pkg/hub/ts)",
		}},
		// A package go list does not list (every file tagged) is walked and parsed.
		{"prodimportalltagged", []string{"example.com/m/pkg/x imports testsupport ts in non-test code (x_integration.go)"}},
		// Production importers inside pkg/hub, the only ones Go's internal rule
		// admits for an internal testsupport package.
		{"prodimportdeclared", []string{"example.com/m/pkg/hub/a imports testsupport ts in non-test code (a.go)"}},
		{"prodimporthub", []string{
			"example.com/m/pkg/hub imports testsupport ts in non-test code (hub.go)",
			"example.com/m/pkg/hub/feature imports testsupport ts in non-test code (feature.go)",
		}},
		// Everything at or under a testsupport path is testsupport.
		{"tssubpkg", []string{"example.com/m/pkg/x imports testsupport ts/fakes in non-test code (x.go)"}},
		// Rule 6 through walked packages: a tag-only package main (its directory
		// mixes package names) reaches ts through a tag-only library.
		{"walkedmain", []string{
			"example.com/m/pkg/y imports testsupport ts in non-test code (y_linux.go)",
			"package main example.com/m/cmd/tagged reaches testsupport ts through non-test imports (example.com/m/cmd/tagged -> example.com/m/pkg/y -> example.com/m/pkg/hub/ts)",
		}},
		// A production package mislabelled testsupport fails once a binary links it.
		{"mainclosure", []string{
			"example.com/m/pkg/lib imports testsupport mislabelled in non-test code (lib.go)",
			"package main example.com/m/cmd/tool reaches testsupport mislabelled through non-test imports (example.com/m/cmd/tool -> example.com/m/pkg/lib -> example.com/m/pkg/hub/mislabelled)",
		}},
		// The testsupport class comes only from dag.txt: a test-like name or
		// directory does not grant the testsupport-to-testsupport exemption.
		{"selfclaimed", []string{
			"example.com/m/pkg/hub/internal/footest imports testsupport ts in non-test code (footest.go)",
			"example.com/m/pkg/testsupport/bar imports testsupport ts in non-test code (bar.go)",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runFixture(t, tc.name)
			if code != 1 {
				t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				if strings.Contains(line, "violation(s);") || strings.Contains(line, "fix the import") {
					continue
				}
				got = append(got, line)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d violations, want %d:\n%s", len(got), len(tc.want), stdout)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i], w)
				}
			}
			if !strings.Contains(stdout, strconv.Itoa(len(tc.want))+" violation(s); ") {
				t.Errorf("summary line does not count %d violation(s):\n%s", len(tc.want), stdout)
			}
		})
	}
}

// TestUnusableInputs: an input that would make the check vacuous, or that
// contradicts the tree, exits 2 instead of passing.
func TestUnusableInputs(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"nosupport", "no testsupport declarations"},
		{"noalias", "no zz_alias_*.go files"},
		{"stale", "declared area gone is not a package of the module"},
		{"emptydag", "no declarations"},
		{"tsmain", "testsupport ts is a package main"},
		{"nomain", "no package main in the module"},
		// An import of a _ directory: go list ./... and the walk skip it, so
		// the check fails closed.
		{"hiddenimport", "import of module path example.com/m/pkg/hub/_shim that go list and the walk did not see (example.com/m/pkg/hub imports it in non-test code"},
		// An import of the bare module path, which holds no package.
		{"rootimport", "import of module path example.com/m that go list and the walk did not see (example.com/m/pkg/hub imports it in non-test code"},
		{"hiddentestimport", "import of module path example.com/m/pkg/hub/_shim that go list and the walk did not see (example.com/m/pkg/hub/b imports it in test code"},
		{"emptylist", "no packages"},
		{"nohub", "example.com/m/pkg/hub is not in the list"},
		{"noedges", "no pkg/hub import edges examined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runFixture(t, tc.name)
			if code != 2 {
				t.Fatalf("exit %d, want 2\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr %q does not contain %q", stderr, tc.want)
			}
		})
	}
}

// copyFixture copies testdata/<name> into a temp directory and returns it.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if de.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func runDir(dir string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-dag", filepath.Join(dir, "dag.txt"),
		"-list", filepath.Join(dir, "list.json"),
		"-hub", fixtureHub,
	}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSymlinkImport: pkg/hub/htu is a symlink to the testsupport directory
// ts, and pkg/hub imports pkg/hub/htu. The walk does not follow the symlink
// (as go ./... does not), and isSupport does not match htu, so only the
// fail-closed unresolved-import check stops it. The symlink is made in a temp
// copy, so the repository carries none.
func TestSymlinkImport(t *testing.T) {
	dir := copyFixture(t, "symlinkimport")
	if err := os.Symlink("ts", filepath.Join(dir, "pkg", "hub", "htu")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	code, stdout, stderr := runDir(dir)
	want := "import of module path example.com/m/pkg/hub/htu that go list and the walk did not see"
	if code != 2 || !strings.Contains(stderr, want) {
		t.Fatalf("exit %d, want 2 and %q\nstdout:\n%s\nstderr:\n%s", code, want, stdout, stderr)
	}
}

// TestWalkSkips: directories the go command skips for ./... (testdata, names
// starting with _ or ., vendor, and a nested module) are not walked, so their
// non-test imports of testsupport do not fail the ok fixture, which still
// reports exactly its one tag-only directory. Built in a temp copy so the
// repository carries no nested go.mod or vendor directory.
func TestWalkSkips(t *testing.T) {
	dir := copyFixture(t, "ok")
	imp := "package x\n\nimport _ \"example.com/m/pkg/hub/ts\"\n"
	for _, f := range []string{
		"pkg/hub/testdata/x.go",
		"pkg/_x/x.go",
		"pkg/.x/x.go",
		"vendor/example.com/v/x.go",
		"nested/x.go",
	} {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(f)), imp)
	}
	writeFile(t, filepath.Join(dir, "nested", "go.mod"), "module example.com/m/nested\n")
	code, stdout, stderr := runDir(dir)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	m := okLine.FindStringSubmatch(stdout)
	if m == nil || m[3] != "1" {
		t.Fatalf("stdout %q: want the OK line with 1 tag-only directory parsed", stdout)
	}
}

// TestSymlinkedGoFile: go/build compiles a symlinked .go file, so one in a
// walked (tag-only) directory makes the run fail closed instead of being
// skipped unseen. The symlinks are made in temp copies of ok; the targets sit
// in a _ directory, which the walk skips.
func TestSymlinkedGoFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		regular string // a regular file in the same directory, or ""
		link    string // the symlinked .go file
		target  string // its content
	}{
		{"mixed tag-only dir", "pkg/z/z_linux.go", "pkg/z/w_linux.go", "package z\n\nimport _ \"example.com/m/pkg/hub/ts\"\n"},
		{"all-symlink tag-only main", "", "cmd/winsvc/main_windows.go", "package main\n\nimport _ \"example.com/m/pkg/hub/ts\"\n\nfunc main() {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyFixture(t, "ok")
			if tc.regular != "" {
				writeFile(t, filepath.Join(dir, filepath.FromSlash(tc.regular)), "package z\n")
			}
			target := filepath.Join(dir, "_src", "target.go.txt")
			writeFile(t, target, tc.target)
			link := filepath.Join(dir, filepath.FromSlash(tc.link))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("cannot create a symlink here: %v", err)
			}
			code, stdout, stderr := runDir(dir)
			want := "symlinked .go file " + link + " in a walked directory: areadag cannot vouch for it"
			if code != 2 || !strings.Contains(stderr, want) {
				t.Fatalf("exit %d, want 2 and %q\nstdout:\n%s\nstderr:\n%s", code, want, stdout, stderr)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-dag", "x"},
		{"-list", "x"},
		{"-dag", "x", "-list", "y", "extra"},
		{"-bogus"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
}

func TestMalformedList(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dagFile := write("dag.txt", "testsupport ts\n")
	for _, tc := range []struct{ name, list, want string }{
		{"bad json", "{", "unexpected EOF"},
		{"no dir", `{"ImportPath":"example.com/m/pkg/hub"}`, "without ImportPath or Dir"},
		{"duplicate", `{"ImportPath":"a","Dir":"/a"}{"ImportPath":"a","Dir":"/a"}`, "a is listed twice"},
		{"go list error", `{"ImportPath":"a","Dir":"/a","Error":{"Err":"boom"}}`, "go list reported an error for a: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"-dag", dagFile, "-list", write("list.json", tc.list), "-hub", fixtureHub}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want 2 and %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// TestRepoDag checks the real hack/areadag/dag.txt without running go: it
// parses, it declares at least one leaf, area and testsupport package, and
// every declared package is a directory under pkg/hub holding Go files.
func TestRepoDag(t *testing.T) {
	d, err := dag.ReadFile("dag.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []dag.Kind{dag.Leaf, dag.Area, dag.TestSupport} {
		if d.Count(k) == 0 {
			t.Errorf("dag.txt declares no %s", k)
		}
	}
	for _, e := range d.Entries {
		files, err := filepath.Glob(filepath.Join("..", "..", "pkg", "hub", filepath.FromSlash(e.Rel), "*.go"))
		if err != nil || len(files) == 0 {
			t.Errorf("dag.txt:%d: %s %s has no Go files under pkg/hub (err %v)", e.Line, e.Kind, e.Rel, err)
		}
	}
}
