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
	"errors"
	"flag"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// A golden case runs pkgmove on a copy of testdata/<fixture>/in, moving files
// from hub/ to hub/sub/. Successful cases compare the resulting tree with
// testdata/<fixture>/want and the output with testdata/<fixture>/stdout.golden.
// Failing cases must leave the tree untouched.
type goldenCase struct {
	fixture    string
	files      []string
	git        bool
	allowField bool
	tmSupport  string // -testmain-support
	wantErr    bool
	rewrite    bool              // -rewrite-aliases (files must be empty)
	dst        string            // -to, relative to the fixture root (default hub/sub; none for rewrite runs when empty)
	renames    map[string]string // -rename overrides

	// Behaviour (successful cases only): TestBehaviour runs the fixture's
	// own tests (with tags) before and after the move. When changes is set,
	// the report must contain it and the tests must fail afterwards (unless
	// afterPasses: the change is silent to the tests, e.g. a skip).
	// Otherwise the tests must still pass.
	tags        string
	env         string // extra NAME=value for the fixture's tests
	changes     string
	afterPasses bool
}

var goldenCases = []goldenCase{
	{fixture: "basic", files: []string{"maint.go", "maint_test.go"}, git: true},
	{fixture: "tags", files: []string{"move_sqlite.go", "move_plain.go"}},
	{fixture: "xtest", files: []string{"move.go", "move_test.go", "onlymoved_test.go", "data.txt"}},
	{fixture: "embedallow", files: []string{"move.go"}, allowField: true, changes: "== HIGH: exported struct field"},
	{fixture: "iface", files: []string{"move.go"}},
	{fixture: "fields", files: []string{"move.go"}, allowField: true},
	{fixture: "generic", files: []string{"move.go"}},
	{fixture: "initorder", files: []string{"move.go"}},
	{fixture: "samename", files: []string{"move.go"}},
	{fixture: "typenames", files: []string{"move.go"}},
	{fixture: "registry", files: []string{"move.go"}, changes: "== HIGH: staying var initialiser calls moved code"},
	{fixture: "iife", files: []string{"move.go"}, changes: "registered = ... calls register:"},
	{fixture: "transitive", files: []string{"move.go"}, changes: "calls registerBuiltins (which reaches moved register)"},
	{fixture: "purity", files: []string{"move.go"}},
	{fixture: "funcvalue", files: []string{"move.go"}},
	{fixture: "funcname", files: []string{"move.go"}, changes: "hub.defaultHook is now example.com/fx/hub/sub.DefaultHook for runtime.FuncForPC"},
	{fixture: "importervalue", files: []string{"move.go", "move_ptr.go"}},
	{fixture: "excludedmethod", files: []string{"move.go"}, tags: "integration", changes: "method run matches an unexported method of a moved type or interface"},
	{fixture: "excludediface", files: []string{"move.go"}, tags: "integration", changes: "interface method run matches an unexported method"},
	{fixture: "testmain", files: []string{"move.go", "move_test.go"}, changes: "== HIGH: TestMain separation"},
	{fixture: "testmainsupport", files: []string{"move.go", "move_test.go"}, tmSupport: "example.com/fx/hubtest"},
	{fixture: "testdatadir", files: []string{"move.go", "move_test.go"}, changes: "== WARN: moved test reads package-relative files", afterPasses: true},
	{fixture: "sourcescan", files: []string{"move.go"}, env: "STRICT_GUARD=1", changes: "== HIGH: source-scanning test does not cover the target", afterPasses: true}, // the alias file replaces the moved file in the scan count
	// scan-covers markers clear the HIGH (INFO instead); the guards really scan hub/sub.
	{fixture: "sourcescancovered", files: []string{"move.go"}},
	// A marker without a reason line, and the "//pkgmove:" directive form, clear nothing.
	{fixture: "sourcescannoreason", files: []string{"move.go"}, env: "STRICT_GUARD=1", changes: "marker at hub/guard_test.go:14 has no reason line", afterPasses: true},
	{fixture: "sourcescanwrongdir", files: []string{"move.go"}, env: "STRICT_GUARD=1", changes: "its pkgmove:scan-covers markers do not cover hub/sub", afterPasses: true},
	{fixture: "aliasresolve", files: []string{"handlers.go", "handlers_typed.go", "handlers_shadow.go", "handlers_test.go"}},
	{fixture: "rewritealiases", rewrite: true, dst: "apierr", git: true},
	{fixture: "intoexisting", files: []string{"policy_a_test.go", "policy_x_test.go"}, git: true},
	{fixture: "intoexistingtestmain", files: []string{"policy_a_test.go"}, changes: "== HIGH: TestMain separation"},
	{fixture: "intoexistingtestmaindeps", files: []string{"mode_test.go"}, changes: "or calls helpers that are not equivalent"},             // same TestMain text, different setup helper
	{fixture: "intoexistingtestmaintags", files: []string{"mode_test.go"}, tags: "integration", changes: "TestMain has build-tag variants"}, // the integration TestMain has no target counterpart
	// Comments: doc comments of renamed declarations and renamed identifiers in moved comments.
	{fixture: "doccomments", files: []string{"move.go"}},
	// -rename overrides for package-level names and a method group (interface and every implementer).
	{fixture: "renameoverride", files: []string{"move.go"}, renames: map[string]string{"httpStatus": "HTTPStatus", "apiKey": "APIKey", "apiClient": "APIClient"}},
	// Moved functions that call log/slog (WARN), including a moved external test (by import name).
	{fixture: "logging", files: []string{"move.go", "move_ext_test.go"}},
	// Rejections.
	{fixture: "renamereject", files: []string{"move.go"}, wantErr: true, renames: map[string]string{"helper": "Other", "label": "String", "nosuch": "NoSuch"}}, // collision, dynamic method name, unused override
	{fixture: "methods", files: []string{"move.go"}, wantErr: true},
	{fixture: "backref", files: []string{"move.go"}, wantErr: true},
	{fixture: "testsep", files: []string{"a.go", "a_test.go"}, wantErr: true},
	{fixture: "collide", files: []string{"move.go"}, wantErr: true},
	{fixture: "dynamic", files: []string{"move.go"}, wantErr: true},
	{fixture: "cgo", files: []string{"move.go"}, wantErr: true},
	{fixture: "embed", files: []string{"move.go"}, wantErr: true}, // embedded-field export needs -allow-field-export
	{fixture: "asm", files: []string{"move.go"}, wantErr: true},
	{fixture: "linkname", files: []string{"move.go"}, wantErr: true},
	{fixture: "aliasreject", files: []string{"move.go"}, wantErr: true},                                                         // hand-written wrapper and var, assigned var alias, embedded alias
	{fixture: "intoexistingreject", files: []string{"policy_a_test.go", "policy_c_test.go", "policy_d_test.go"}, wantErr: true}, // collision, non-equivalent helper, import cycles, shadowed bare name
	{fixture: "intoexistinghelpertags", files: []string{"limit_test.go"}, wantErr: true},                                        // a helper with build-tag variants is never reused
}

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not in PATH")
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// readTree returns every file under root (excluding .git) keyed by slash path.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runFixture copies the fixture into a temp dir and runs one move there.
func runFixture(t *testing.T, c goldenCase, dryRun bool) (dir, stdout string, err error) {
	t.Helper()
	dir = t.TempDir()
	copyTree(t, filepath.Join("testdata", c.fixture, "in"), dir)
	if c.git {
		if _, lookErr := exec.LookPath("git"); lookErr != nil {
			t.Skip("git not in PATH")
		}
		gitRun(t, dir, "init", "-q")
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "fixture")
	}
	var buf bytes.Buffer
	dst := filepath.Join(dir, "hub", "sub")
	switch {
	case c.dst != "":
		dst = filepath.Join(dir, filepath.FromSlash(c.dst))
	case c.rewrite:
		dst = ""
	}
	cfg := &Config{
		SrcDir:           filepath.Join(dir, "hub"),
		DstDir:           dst,
		RewriteAliases:   c.rewrite,
		Files:            c.files,
		NoGit:            !c.git,
		DryRun:           dryRun,
		Typecheck:        true,
		AllowFieldExport: c.allowField,
		TestMainSupport:  c.tmSupport,
		RenameOverrides:  c.renames,
		Stdout:           &buf,
	}
	err = run(cfg)
	return dir, buf.String(), err
}

func compareTrees(t *testing.T, got, want map[string]string) {
	t.Helper()
	var names []string
	seen := map[string]bool{}
	for k := range got {
		names = append(names, k)
		seen[k] = true
	}
	for k := range want {
		if !seen[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		g, gok := got[n]
		w, wok := want[n]
		switch {
		case !gok:
			t.Errorf("missing file %s", n)
		case !wok:
			t.Errorf("unexpected file %s:\n%s", n, g)
		case g != w:
			t.Errorf("file %s differs:\n--- got ---\n%s\n--- want ---\n%s", n, g, w)
		}
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGolden(t *testing.T) {
	requireGo(t)
	for _, c := range goldenCases {
		t.Run(c.fixture, func(t *testing.T) {
			dir, stdout, err := runFixture(t, c, false)
			if c.wantErr {
				if !errors.Is(err, errPlan) {
					t.Fatalf("want a plan error, got %v\n%s", err, stdout)
				}
				compareTrees(t, readTree(t, dir), readTree(t, filepath.Join("testdata", c.fixture, "in")))
			} else if err != nil {
				t.Fatalf("run: %v\n%s", err, stdout)
			}
			goldenOut := filepath.Join("testdata", c.fixture, "stdout.golden")
			wantDir := filepath.Join("testdata", c.fixture, "want")
			if *update {
				if err := os.WriteFile(goldenOut, []byte(stdout), 0o644); err != nil {
					t.Fatal(err)
				}
				if !c.wantErr {
					writeTree(t, wantDir, readTree(t, dir))
				}
				return
			}
			want, err := os.ReadFile(goldenOut)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if stdout != string(want) {
				t.Errorf("stdout differs:\n--- got ---\n%s\n--- want ---\n%s", stdout, want)
			}
			if !c.wantErr {
				compareTrees(t, readTree(t, dir), readTree(t, wantDir))
			}
		})
	}
}

// TestDeterministic runs the same move several times on fresh copies and requires
// byte-identical trees and output.
func TestDeterministic(t *testing.T) {
	requireGo(t)
	const runs = 6
	for _, c := range goldenCases {
		if c.wantErr {
			continue
		}
		t.Run(c.fixture, func(t *testing.T) {
			dir1, out1, err := runFixture(t, c, false)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			tree1 := readTree(t, dir1)
			for i := 1; i < runs; i++ {
				dir, out, err := runFixture(t, c, false)
				if err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				if out != out1 {
					t.Fatalf("run %d output differs:\n%s\n---\n%s", i, out, out1)
				}
				compareTrees(t, readTree(t, dir), tree1)
			}
		})
	}
}

// TestDryRun requires -dry-run to leave the tree untouched and to print the
// same plan and report as the real run.
func TestDryRun(t *testing.T) {
	requireGo(t)
	c := goldenCases[0]
	dir, dryOut, err := runFixture(t, c, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	compareTrees(t, readTree(t, dir), readTree(t, filepath.Join("testdata", c.fixture, "in")))
	_, realOut, err := runFixture(t, c, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(realOut, dryOut) {
		t.Errorf("real-run output does not start with the dry-run output:\n--- dry ---\n%s\n--- real ---\n%s", dryOut, realOut)
	}
}

// TestFieldExportRefusedByDefault checks that exporting a struct field needs
// the explicit flag.
func TestFieldExportRefusedByDefault(t *testing.T) {
	requireGo(t)
	c := goldenCase{fixture: "fields", files: []string{"move.go"}}
	_, out, err := runFixture(t, c, true)
	if !errors.Is(err, errPlan) || !strings.Contains(out, "field Job.name is used across the boundary") {
		t.Fatalf("want a field-export error, got %v\n%s", err, out)
	}
}

// TestEmbedNeedsAssets checks that a go:embed asset must move with its file.
func TestEmbedNeedsAssets(t *testing.T) {
	requireGo(t)
	c := goldenCase{fixture: "xtest", files: []string{"move.go", "move_test.go", "onlymoved_test.go"}}
	_, out, err := runFixture(t, c, true)
	if !errors.Is(err, errPlan) || !strings.Contains(out, `go:embed pattern "data.txt" matches hub/data.txt, which is not in the move set`) {
		t.Fatalf("want an embed error, got %v\n%s", err, out)
	}
}

func TestUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runMain(nil, &out, &errOut); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage: pkgmove") {
		t.Fatalf("missing usage text: %s", errOut.String())
	}
}

func TestExportName(t *testing.T) {
	for in, want := range map[string]string{
		"fooBar": "FooBar", "x": "X", "écrire": "Écrire", "Foo": "", "_foo": "", "日本": "",
	} {
		if got := exportName(in); got != want {
			t.Errorf("exportName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEffectiveConstraint(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"a.go", "package a\n", ""},
		{"a.go", "//go:build !no_sqlite\n\npackage a\n", "!no_sqlite"},
		{"a_linux.go", "package a\n", "linux"},
		{"a_linux_amd64_test.go", "package a\n", "linux && amd64"},
		{"a_unix.go", "package a\n", ""}, // unix is not a file-name tag
		{"a_windows.go", "// Copyright\n\n//go:build a || b\n\npackage a\n", "(a || b) && windows"},
		{"a.go", "package a\n\n//go:build ignored\n", ""},
	} {
		got, err := effectiveConstraint(tc.name, []byte(tc.src))
		if err != nil || got != tc.want {
			t.Errorf("effectiveConstraint(%q) = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestGitStaging checks that a git run stages renames, edits and alias files
// and leaves the safety report unstaged.
func TestGitStaging(t *testing.T) {
	requireGo(t)
	dir, out, err := runFixture(t, goldenCases[0], false)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"R  hub/maint.go -> hub/sub/maint.go",
		"R  hub/maint_test.go -> hub/sub/maint_test.go",
		"M  hub/core.go",
		"A  hub/zz_alias_sub.go",
		"A  hub/zz_alias_sub_test.go",
		"?? hub/zz_alias_sub_safety.txt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("git status missing %q:\n%s", want, got)
		}
	}
}

// TestStrictRejectsHigh checks that -strict turns HIGH findings (init() and
// a var initialiser calling package code in the basic fixture) into errors.
func TestStrictRejectsHigh(t *testing.T) {
	requireGo(t)
	for _, tc := range []struct {
		fixture string
		files   []string
		want    []string
	}{
		{"basic", []string{"maint.go", "maint_test.go"}, []string{
			"-strict: init() in moved file", "-strict: package-level var initialiser calls package code"}},
		{"sourcescan", []string{"move.go"}, []string{"-strict: source-scanning test does not cover the target"}},
		{"sourcescanwrongdir", []string{"move.go"}, []string{"hub/guard_test.go: -strict: source-scanning test does not cover the target"}},
		{"sourcescannoreason", []string{"move.go"}, []string{
			"hub/guard_test.go: -strict: source-scanning test does not cover the target",
			"marker at hub/guard_test.go:14 has no reason line",
			"hub/nospace_test.go: -strict: source-scanning test does not cover the target"}},
		{"intoexistingtestmain", []string{"policy_a_test.go"}, []string{"-strict: TestMain separation"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			dir := t.TempDir()
			copyTree(t, filepath.Join("testdata", tc.fixture, "in"), dir)
			var buf bytes.Buffer
			err := run(&Config{
				SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", "sub"),
				Files: tc.files, NoGit: true, DryRun: true, Strict: true, Stdout: &buf,
			})
			out := buf.String()
			if !errors.Is(err, errPlan) {
				t.Fatalf("want errPlan, got %v\n%s", err, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q:\n%s", w, out)
				}
			}
		})
	}
}

// TestStrictAcceptsScanCovers checks that -strict accepts a move whose only
// source-scanning tests carry a scan-covers marker for the target, and still
// lists them as INFO.
func TestStrictAcceptsScanCovers(t *testing.T) {
	requireGo(t)
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "sourcescancovered", "in"), dir)
	var buf bytes.Buffer
	err := run(&Config{
		SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", "sub"),
		Files: []string{"move.go"}, NoGit: true, DryRun: true, Strict: true, Stdout: &buf,
	})
	out := buf.String()
	if err != nil {
		t.Fatalf("want success under -strict, got %v\n%s", err, out)
	}
	for _, w := range []string{
		`source-scanning test declares coverage of hub/sub (marker at hub/guard_test.go:18; reason: "the loop reads both the package directory and sub.")`,
		`source-scanning test declares coverage of hub/sub (marker at hub/walk_test.go:16; reason: "the walk covers the package directory and everything under it.")`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

func TestScanCoversMarker(t *testing.T) {
	for _, tc := range []struct {
		comment string
		target  string
		want    bool
	}{
		{"// pkgmove:scan-covers pkg/hub/sub", "pkg/hub/sub", true},
		{"//pkgmove:scan-covers pkg/hub/sub", "pkg/hub/sub", false}, // directive syntax
		{"//  pkgmove:scan-covers pkg/hub/sub", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers ./pkg/hub/sub/", "pkg/hub/sub", true},
		{"// pkgmove:scan-covers pkg/hub/...", "pkg/hub/sub", true},
		{"// pkgmove:scan-covers pkg/hub/...", "pkg/hub", true},
		{"// pkgmove:scan-covers pkg/hub/...", "pkg/hub/a/b", true},
		{"// pkgmove:scan-covers ./...", "pkg/hub/sub", true},
		{"// pkgmove:scan-covers ...", "pkg/hub/sub", true},
		{"// pkgmove:scan-covers pkg/hub", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers pkg/hub/su/...", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers pkg/hub/subx", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers pkg/hub/sub/x", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers pkg/hub/sub extra", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers /pkg/hub/sub", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers ../hub/sub", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers pkg/../pkg/hub/sub", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers pkg/hub/*", "pkg/hub/sub", false},
		{"// pkgmove:scan-covers pkg/.../sub", "pkg/hub/sub", false},
	} {
		src := "package p\n\n" + tc.comment + "\n"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		ms := scanCoversMarkers(fset, f, []byte(src))
		if len(ms) != 1 {
			t.Fatalf("%q: got %d markers, want 1", tc.comment, len(ms))
		}
		if got := ms[0].covers(tc.target); got != tc.want {
			t.Errorf("%q covers %q = %v, want %v", tc.comment, tc.target, got, tc.want)
		}
	}
	// Not markers: block comments and other words.
	src := "package p\n\n/* pkgmove:scan-covers pkg/hub/sub */\n// pkgmove:scan-coversx pkg/hub/sub\n// see pkgmove:scan-covers pkg/hub/sub\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if ms := scanCoversMarkers(fset, f, []byte(src)); len(ms) != 0 {
		t.Errorf("got %d markers, want 0: %+v", len(ms), ms)
	}

	// A marker must be a whole comment line; the next comment line is its
	// reason, unless it is another marker or not on the next line.
	src = "package p\n\nvar x = 1 // pkgmove:scan-covers pkg/hub/sub\n\n" +
		"func f() {\n\t// pkgmove:scan-covers pkg/hub/sub\n\t// walks the whole module.\n}\n\n" +
		"// pkgmove:scan-covers pkg/hub/a\n// pkgmove:scan-covers pkg/hub/b\n\n// unrelated\n"
	fset = token.NewFileSet()
	f, err = parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	ms := scanCoversMarkers(fset, f, []byte(src))
	if len(ms) != 4 {
		t.Fatalf("got %d markers, want 4: %+v", len(ms), ms)
	}
	for i, w := range []struct {
		covers, reason string
	}{
		{"", ""},
		{"pkg/hub/sub", "walks the whole module."},
		{"pkg/hub/a", ""},
		{"pkg/hub/b", ""},
	} {
		if w.covers == "" {
			if ms[i].covers("pkg/hub/sub") {
				t.Errorf("marker %d (trailing comment) covers pkg/hub/sub", i)
			}
		} else if !ms[i].covers(w.covers) {
			t.Errorf("marker %d does not cover %s", i, w.covers)
		}
		if ms[i].reason != w.reason {
			t.Errorf("marker %d reason = %q, want %q", i, ms[i].reason, w.reason)
		}
	}
}

// TestRollback makes execute fail after the files were moved and rewritten
// and checks that the tree and the git index are back to the original.
func TestRollback(t *testing.T) {
	requireGo(t)
	for _, c := range []goldenCase{goldenCases[0], {fixture: "basic", files: goldenCases[0].files}} {
		name := "git"
		if !c.git {
			name = "no-git"
		}
		t.Run(name, func(t *testing.T) {
			testHookBeforeStage = func() error { return errors.New("injected failure") }
			defer func() { testHookBeforeStage = nil }()
			dir, out, err := runFixture(t, c, false)
			if err == nil || !strings.Contains(err.Error(), "injected failure (all changes rolled back)") {
				t.Fatalf("want a rolled-back failure, got %v\n%s", err, out)
			}
			compareTrees(t, readTree(t, dir), readTree(t, filepath.Join("testdata", c.fixture, "in")))
			if c.git {
				cmd := exec.Command("git", "status", "--porcelain")
				cmd.Dir = dir
				b, err := cmd.Output()
				if err != nil || len(b) != 0 {
					t.Fatalf("git status not clean after rollback: %v\n%s", err, b)
				}
			}
		})
	}
}

// goTest runs `go test ./...` in a fixture tree.
func goTest(t *testing.T, dir, tags, env string) (string, error) {
	t.Helper()
	args := []string{"test", "-count=1"}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	cmd := exec.Command("go", append(args, "./...")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if env != "" {
		cmd.Env = append(cmd.Env, env)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestBehaviour runs every successful fixture's own tests before and after
// the move. Where a move changes behaviour (goldenCase.changes), the report
// must carry the explaining finding and the tests must fail afterwards (unless
// the change is silent to them); everywhere else the tests must still pass.
func TestBehaviour(t *testing.T) {
	requireGo(t)
	for _, c := range goldenCases {
		if c.wantErr {
			continue
		}
		t.Run(c.fixture, func(t *testing.T) {
			before := t.TempDir()
			copyTree(t, filepath.Join("testdata", c.fixture, "in"), before)
			if out, err := goTest(t, before, c.tags, c.env); err != nil {
				t.Fatalf("fixture tests fail before the move: %v\n%s", err, out)
			}
			after, report, err := runFixture(t, c, false)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, report)
			}
			out, testErr := goTest(t, after, c.tags, c.env)
			if c.changes == "" {
				if testErr != nil {
					t.Errorf("behaviour changed after the move without a declared finding: %v\n%s", testErr, out)
				}
			} else {
				if !strings.Contains(report, c.changes) {
					t.Errorf("report lacks %q:\n%s", c.changes, report)
				}
				if !c.afterPasses && testErr == nil {
					t.Errorf("expected the fixture's tests to detect the behaviour change after the move")
				}
			}
		})
	}
}

// TestRewriteAliasesAll rewrites the aliases of every package (no -to),
// including hand-written ones, which are never removed.
func TestRewriteAliasesAll(t *testing.T) {
	requireGo(t)
	_, out, err := runFixture(t, goldenCase{fixture: "rewritealiases", rewrite: true}, true)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"pkgmove alias rewrite: example.com/fx/hub (aliases of all packages)",
		"hub/clock.go:12: Clock -> other.Clock",
		"hand-written alias (never removed): hub/clock.go: Clock",
		"hub/users.go:8: code -> apierr.Code",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// aliasOnlyTree is a module whose generated alias file has one unexported
// entry, used by one staying file.
var aliasOnlyTree = map[string]string{
	"go.mod":         "module example.com/fx\n\ngo 1.26\n",
	"apierr/code.go": "package apierr\n\n// Code is an error code.\ntype Code string\n",
	"hub/zz_alias_apierr.go": `package hub

// Aliases for symbols moved to
// example.com/fx/apierr
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/apierr"
)

type (
	code = apierr.Code
)
`,
	"hub/use.go": "package hub\n\nfunc use() code { return \"x\" }\n",
}

// TestRewriteAliasesDeletesFile checks that an alias file left without
// entries is deleted (and staged as deleted), and that a failure restores it.
func TestRewriteAliasesDeletesFile(t *testing.T) {
	requireGo(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		writeTree(t, dir, aliasOnlyTree)
		gitRun(t, dir, "init", "-q")
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "fixture")
		return dir
	}
	cfg := func(dir string, buf *bytes.Buffer) *Config {
		return &Config{SrcDir: filepath.Join(dir, "hub"), RewriteAliases: true, Typecheck: true, Stdout: buf}
	}
	t.Run("delete", func(t *testing.T) {
		dir := setup(t)
		var buf bytes.Buffer
		if err := run(cfg(dir, &buf)); err != nil {
			t.Fatalf("run: %v\n%s", err, buf.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "hub", "zz_alias_apierr.go")); !os.IsNotExist(err) {
			t.Fatalf("alias file not deleted: %v", err)
		}
		cmd := exec.Command("git", "status", "--porcelain")
		cmd.Dir = dir
		b, _ := cmd.Output()
		for _, want := range []string{"D  hub/zz_alias_apierr.go", "M  hub/use.go"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("git status missing %q:\n%s", want, b)
			}
		}
		if !strings.Contains(buf.String(), "Files deleted (1):\n  hub/zz_alias_apierr.go") {
			t.Errorf("plan does not list the deletion:\n%s", buf.String())
		}
	})
	t.Run("rollback", func(t *testing.T) {
		dir := setup(t)
		testHookBeforeStage = func() error { return errors.New("injected failure") }
		defer func() { testHookBeforeStage = nil }()
		var buf bytes.Buffer
		err := run(cfg(dir, &buf))
		if err == nil || !strings.Contains(err.Error(), "injected failure (all changes rolled back)") {
			t.Fatalf("want a rolled-back failure, got %v\n%s", err, buf.String())
		}
		got := readTree(t, dir)
		for name, content := range aliasOnlyTree {
			if got[name] != content {
				t.Errorf("%s not restored:\n%s", name, got[name])
			}
		}
		cmd := exec.Command("git", "status", "--porcelain")
		cmd.Dir = dir
		if b, _ := cmd.Output(); len(b) != 0 {
			t.Errorf("git status not clean after rollback:\n%s", b)
		}
	})
}

// TestIntoExistingRejectsSource checks that only _test.go files may move
// into an existing package, and that -name must match it.
func TestIntoExistingRejectsSource(t *testing.T) {
	requireGo(t)
	for _, tc := range []struct {
		name  string
		files []string
		pkg   string
		want  string
	}{
		{"source", []string{"users.go"}, "", "supported only for _test.go file sets (and assets), but the move set includes users.go"},
		{"name", []string{"policy_a_test.go"}, "other", "is in package sub, but the move targets package other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			copyTree(t, filepath.Join("testdata", "intoexisting", "in"), dir)
			var buf bytes.Buffer
			err := run(&Config{SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", "sub"), PkgName: tc.pkg,
				Files: tc.files, NoGit: true, DryRun: true, Stdout: &buf})
			if err == nil || errors.Is(err, errPlan) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestHelperReuseIsSemantic checks that a staying helper whose text matches
// the target's is not reused when its identifiers denote different objects.
func TestHelperReuseIsSemantic(t *testing.T) {
	requireGo(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":                   "module example.com/fx\n\ngo 1.26\n",
		"hub/limits.go":            "package hub\n\nconst limit = 1\n",
		"hub/helpers_test.go":      "package hub\n\nfunc mkLimit() int { return limit }\n",
		"hub/limit_test.go":        "package hub\n\nimport \"testing\"\n\nfunc TestLimit(t *testing.T) {\n\tif mkLimit() != 1 {\n\t\tt.Fatal(\"limit\")\n\t}\n}\n",
		"hub/sub/limits.go":        "package sub\n\nconst limit = 2\n",
		"hub/sub/helpers_test.go":  "package sub\n\nfunc mkLimit() int { return limit }\n",
		"hub/sub/existing_test.go": "package sub\n\nimport \"testing\"\n\nfunc TestExisting(t *testing.T) { _ = mkLimit() }\n",
	})
	var buf bytes.Buffer
	err := run(&Config{SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", "sub"),
		Files: []string{"limit_test.go"}, NoGit: true, DryRun: true, Stdout: &buf})
	if !errors.Is(err, errPlan) || !strings.Contains(buf.String(), "moved test file uses func mkLimit declared in the staying test file helpers_test.go") {
		t.Fatalf("want a helper-separation error, got %v\n%s", err, buf.String())
	}
}

// TestIntoExistingGeneratedTestMain checks that a TestMain generated into an
// existing target without one warns that the target's existing tests run
// under it too.
func TestIntoExistingGeneratedTestMain(t *testing.T) {
	requireGo(t)
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "intoexisting", "in"), dir)
	if err := os.Remove(filepath.Join(dir, "hub", "sub", "main_test.go")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := run(&Config{SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", "sub"),
		Files: []string{"policy_a_test.go", "policy_x_test.go"}, TestMainSupport: "example.com/fx/hubtest",
		NoGit: true, DryRun: true, Stdout: &buf})
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, buf.String())
	}
	for _, want := range []string{
		"hub/sub/zz_testmain_test.go (generated",
		"the existing tests of sub, which ran without a TestMain, now also run under the generated one - check them too",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRenameFlag(t *testing.T) {
	r := renameFlag{}
	if err := r.Set("httpStatus=HTTPStatus, apiKey=APIKey"); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("httpStatus=HTTPStatus"); err != nil { // same pair again is fine
		t.Fatal(err)
	}
	if got, want := r.String(), "apiKey=APIKey,httpStatus=HTTPStatus"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	for _, bad := range []string{
		"httpStatus",            // no =
		"httpStatus=HttpStatus", // already HTTPStatus
		"HTTPStatus=Foo",        // old already exported
		"_x=X",                  // cannot be upper-cased
		"foo=bar",               // new not exported
		"foo=Fo o",              // not an identifier
		"func=Func",             // keyword
	} {
		if err := r.Set(bad); err == nil {
			t.Errorf("Set(%q) succeeded, want an error", bad)
		}
	}
	var out, errOut bytes.Buffer
	if code := runMain([]string{"-rewrite-aliases", "-from", ".", "-rename", "a=A"}, &out, &errOut); code != 2 {
		t.Errorf("-rename with -rewrite-aliases: exit code %d, want 2", code)
	}
}

func TestCommentHelpers(t *testing.T) {
	for text, want := range map[string]bool{
		"//go:generate x": true, "//go:build a": true, "//line a.go:1": true, "//export F": true,
		"//nolint:errcheck": true, "// go:generate": false, "// writeError does": false, "/* go:x */": false,
		"//pkgmove:scan-covers x": true, "//x:": false, "//:x": false,
	} {
		if got := isDirective(text); got != want {
			t.Errorf("isDirective(%q) = %v, want %v", text, got, want)
		}
	}
	for text, want := range map[string]string{
		"// writeError writes": "writeError", "//run starts": "run", "/* job is */": "job",
		"/*\nhandle does\n*/": "handle", "// [run] starts": "", "//": "",
	} {
		if got, _ := leadingWord(text); got != want {
			t.Errorf("leadingWord(%q) = %q, want %q", text, got, want)
		}
	}
	for text, want := range map[string]string{"see fmt.run": "fmt", "s.run": "s", "run": "", ".run": "", "a .run": ""} {
		if got := qualifierBefore(text, strings.LastIndex(text, "run")); got != want {
			t.Errorf("qualifierBefore(%q) = %q, want %q", text, got, want)
		}
	}
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"x.run y", true}, {"x run( y", true}, {"x [run] y", true}, {"x `run` y", true}, {"x run y", false}, {"x run, y", false},
	} {
		i := strings.Index(tc.text, "run")
		if got := codeContext(tc.text, i, i+3); got != tc.want {
			t.Errorf("codeContext(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
