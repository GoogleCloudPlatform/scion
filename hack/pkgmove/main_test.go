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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
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

// TestScanCoversGuardScanSubdirs checks the "pkgmove:scan-covers
// guardScanSubdirs" marker: it covers the target exactly when the source
// package's guardScanSubdirs list names the target (relative to -from), and
// otherwise the HIGH stays (an error under -strict) and says why.
func TestScanCoversGuardScanSubdirs(t *testing.T) {
	requireGo(t)
	const guard = "package hub\n\nimport (\n\t\"go/parser\"\n\t\"go/token\"\n\t\"os\"\n\t\"testing\"\n)\n\n" +
		"func TestGuard(t *testing.T) {\n" +
		"\t// pkgmove:scan-covers guardScanSubdirs\n" +
		"\t// reads the package directory and every guardScanSubdirs entry.\n" +
		"\tfor _, dir := range []string{\".\", \"sub\"} {\n" +
		"\t\tentries, _ := os.ReadDir(dir)\n" +
		"\t\tfor _, e := range entries {\n" +
		"\t\t\t_, _ = parser.ParseFile(token.NewFileSet(), e.Name(), nil, 0)\n" +
		"\t\t}\n\t}\n}\n"
	const list = "package hub\n\nvar guardScanSubdirs = []string{\n\t\"apierr\",\n\t\"sub/\",\n}\n"
	// mixed adds a recursive walk claiming hub/... to the same file: it
	// must not clear the file while the guardScanSubdirs site misses the
	// target.
	const walk = "\nfunc TestWalk(t *testing.T) {\n" +
		"\t// pkgmove:scan-covers hub/...\n" +
		"\t// walks the package directory recursively.\n" +
		"\t_ = filepath.WalkDir(\".\", nil)\n}\n"
	mixed := strings.Replace(guard, "\t\"os\"\n", "\t\"os\"\n\t\"path/filepath\"\n", 1) + walk
	for _, tc := range []struct {
		name    string
		helpers string // hub/helpers_test.go
		dst     string // relative to hub
		mixed   bool
		wantErr bool
		want    string
	}{
		{"listed", list, "sub", false, false,
			`source-scanning test declares coverage of hub/sub (marker at hub/guard_test.go:11, through the guardScanSubdirs entry "sub" declared at hub/helpers_test.go:3; reason: "reads the package directory and every guardScanSubdirs entry.")`},
		{"nested", "package hub\n\nvar guardScanSubdirs = []string{\"internal/sub\"}\n", "internal/sub", false, false,
			`through the guardScanSubdirs entry "internal/sub" declared at hub/helpers_test.go:3`},
		{"unlisted", list, "other", false, true,
			`marker at hub/guard_test.go:11 claims the guardScanSubdirs entries, but the list (declared at hub/helpers_test.go:3) does not include other: add "other" to guardScanSubdirs`},
		{"nolist", "package hub\n\nvar scanDirs = []string{\"sub\"}\n", "sub", false, true,
			"marker at hub/guard_test.go:11 claims the guardScanSubdirs entries, but no test file of hub declares guardScanSubdirs"},
		{"notliteral", "package hub\n\nvar guardScanSubdirs = append([]string{}, \"sub\")\n", "sub", false, true,
			"but guardScanSubdirs at hub/helpers_test.go:3 is not a []string literal"},
		{"badentry", "package hub\n\nvar guardScanSubdirs = []string{\"sub\", \"../x\"}\n", "sub", false, true,
			"but guardScanSubdirs entry at hub/helpers_test.go:3 is not a string literal naming a subdirectory"},
		{"mixedlisted", list, "sub", true, false,
			`source-scanning test declares coverage of hub/sub (marker at hub/guard_test.go:12, through the guardScanSubdirs entry "sub"`},
		{"mixedunlisted", list, "other", true, true,
			`marker at hub/guard_test.go:12 claims the guardScanSubdirs entries, but the list (declared at hub/helpers_test.go:3) does not include other: add "other" to guardScanSubdirs; the marker at hub/guard_test.go:23 does not clear the file while the guardScanSubdirs marker at hub/guard_test.go:12 misses the target`},
		// The helpers read each entry one level deep, so a listed parent does not cover a target below it.
		{"belowentry", list, "sub/deeper", false, true,
			`does not include sub/deeper: add "sub/deeper" to guardScanSubdirs`},
		{"outsidefrom", list, "../other", false, true,
			"marker at hub/guard_test.go:11 claims the guardScanSubdirs entries, but the target is not below the source directory"},
		{"dotentry", "package hub\n\nvar guardScanSubdirs = []string{\"sub\", \".\"}\n", "sub", false, true,
			"but guardScanSubdirs entry at hub/helpers_test.go:3 is not a string literal naming a subdirectory"},
		{"recursiveentry", "package hub\n\nvar guardScanSubdirs = []string{\"sub/...\"}\n", "sub", false, true,
			"but guardScanSubdirs entry at hub/helpers_test.go:3 is not a string literal naming a subdirectory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := guard
			if tc.mixed {
				g = mixed
			}
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{
				"go.mod":              "module example.com/fx\n\ngo 1.26\n",
				"hub/move.go":         "package hub\n\nfunc Handler() {}\n",
				"hub/stay.go":         "package hub\n\nconst version = 1\n",
				"hub/helpers_test.go": tc.helpers,
				"hub/guard_test.go":   g,
			})
			var buf bytes.Buffer
			err := run(&Config{
				SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", filepath.FromSlash(tc.dst)),
				Files: []string{"move.go"}, NoGit: true, DryRun: true, Strict: true, Stdout: &buf,
			})
			out := buf.String()
			switch {
			case tc.wantErr && !errors.Is(err, errPlan):
				t.Fatalf("want errPlan, got %v\n%s", err, out)
			case !tc.wantErr && err != nil:
				t.Fatalf("want success under -strict, got %v\n%s", err, out)
			}
			if tc.wantErr && !strings.Contains(out, "hub/guard_test.go: -strict: source-scanning test does not cover the target") {
				t.Errorf("missing the -strict error:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("missing %q:\n%s", tc.want, out)
			}
		})
	}

	// Two declarations (build-tag variants, so the package still compiles)
	// and a listed target whose marker has no reason line: both stay HIGH.
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"duplicate", map[string]string{
			"hub/helpers_test.go":             "//go:build !integration\n\npackage hub\n\nvar guardScanSubdirs = []string{\"sub\"}\n",
			"hub/helpers_integration_test.go": "//go:build integration\n\npackage hub\n\nvar guardScanSubdirs = []string{\"sub\"}\n",
			"hub/guard_test.go":               guard,
		}, "but guardScanSubdirs is declared more than once (hub/helpers_integration_test.go:5, hub/helpers_test.go:5)"},
		{"noreason", map[string]string{
			"hub/helpers_test.go": list,
			"hub/guard_test.go":   strings.Replace(guard, "\t// reads the package directory and every guardScanSubdirs entry.\n", "", 1),
		}, "marker at hub/guard_test.go:11 has no reason line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"go.mod":      "module example.com/fx\n\ngo 1.26\n",
				"hub/move.go": "package hub\n\nfunc Handler() {}\n",
				"hub/stay.go": "package hub\n\nconst version = 1\n",
			}
			for k, v := range tc.files {
				files[k] = v
			}
			writeTree(t, dir, files)
			var buf bytes.Buffer
			err := run(&Config{
				SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", "sub"),
				Files: []string{"move.go"}, NoGit: true, DryRun: true, Strict: true, Stdout: &buf,
			})
			out := buf.String()
			if !errors.Is(err, errPlan) {
				t.Fatalf("want errPlan, got %v\n%s", err, out)
			}
			for _, w := range []string{"hub/guard_test.go: -strict: source-scanning test does not cover the target", tc.want} {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q:\n%s", w, out)
				}
			}
		})
	}
}

// TestOutsideRefs checks the outside-reference findings: references to a
// moved file from outside the move set, by repo path, by path elements and by
// base name, in Go string literals (the source package's staying tests
// included), shell guards, the Makefile and workflows. The tree reproduces
// the two references the maintenance move had to fix by hand: the authzop
// MutationClassifications entry keyed on the repo path, and the logging
// TestRequestPathSites table keyed on the base name.
func TestOutsideRefs(t *testing.T) {
	requireGo(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":                           "module example.com/fx\n\ngo 1.26\n",
		"pkg/hub/maintenance_executors.go": "package hub\n\n// CheckForReleaseUpdates is moved; \"maintenance_executors.go\" here is the moved file itself.\nfunc CheckForReleaseUpdates() string { return \"maintenance_executors.go\" }\n",
		"pkg/hub/stay.go":                  "package hub\n\nconst version = 1\n",
		"pkg/hub/stay_test.go": "package hub\n\n" +
			"var censusSites = map[string]bool{\"maintenance_executors.go:Run:GetSecretValue\": true}\n\n" + // line 3: base name in a staying test
			"// pkgmove:ref-ok the history table keeps the old path on purpose.\n" +
			"var oldPath = \"pkg/hub/maintenance_executors.go\"\n\n" + // line 6: accepted
			"// pkgmove:ref-ok\n" +
			"var noReason = `maintenance_executors.go`\n", // line 9: marker without a reason
		"pkg/hub/authzop/catalog.go": "package authzop\n\n" +
			"type MutationClassification struct{ File, Function, Symbol, Scope string }\n\n" +
			"var MutationClassifications = []MutationClassification{\n" +
			"\t// pkg/hub/maintenance_executors.go - maintenance operations (a comment: not reported)\n" +
			"\t{File: \"pkg/hub/maintenance_executors.go\", Function: \"Run\", Symbol: \"GetSecretValue\", Scope: \"pkg/hub/maintenance_executors.go\"},\n" + // line 7
			"}\n",
		"pkg/util/logging/request_path_sites_test.go": "package logging\n\nimport \"path/filepath\"\n\n" +
			"var requestPathSites = []struct{ file, fn string }{\n" +
			"\t{\"maintenance_executors.go\", \"CheckForReleaseUpdates\"},\n" + // line 6
			"\t{\"premaintenance_executors.go\", \"Other\"},\n" +
			"\t{\"maintenance_executors.gox\", \"Other\"},\n" +
			"}\n\n" +
			"var hubFiles, _ = filepath.Glob(filepath.Join(\"..\", \"..\", \"hub\", \"*.go\"))\n",
		"pkg/secret/backend_test.go": "package secret\n\nimport \"path/filepath\"\n\n" +
			"var keyFile = filepath.Join(\"pkg\", \"hub\",\n\t\"maintenance_executors.go\")\n", // line 5
		"hack/check-guard.sh":      "#!/bin/sh\nexempt=\"pkg/hub/maintenance_executors.go\"\n# pkgmove:ref-ok fixture path for the guard's self-test.\nfixture=\"pkg/hub/maintenance_executors.go\"\n",
		"Makefile":                 "check:\n\t@grep -q maintenance_executors.go pkg/hub/*.go\n",
		".github/workflows/ci.yml": "jobs:\n  check:\n    steps:\n      - run: test -f pkg/hub/maintenance_executors.go\n",
		"scripts/other.sh":         "echo pkg/hub/maintenance_executors.go\n",
		// A base-name-only call argument (not path elements), an unparsable Go file (matched as text;
		// testdata is scanned), skipped vendor and node_modules directories, and a base name after
		// another directory (another file of the same name).
		"pkg/hub/stay2_test.go":               "package hub\n\nimport \"os\"\n\nvar movedSrc, _ = os.ReadFile(\"maintenance_executors.go\")\n",
		"pkg/util/logging/testdata/broken.go": "package broken\n\nfunc {\nvar f = \"pkg/hub/maintenance_executors.go\"\n",
		"pkg/util/vendor/x.go":                "package vendor\n\nvar f = \"pkg/hub/maintenance_executors.go\"\n",
		"web/node_modules/x/x.go":             "package x\n\nvar f = \"pkg/hub/maintenance_executors.go\"\n",
		"pkg/other/other.go":                  "package other\n\nvar f = []string{\"src/maintenance_executors.go\", \"$fixture_dir/maintenance_executors.go\"}\n",
		"docs/notes.md":                       "pkg/hub/maintenance_executors.go\n",
		".design/x.go":                        "package x\n\nvar f = \"pkg/hub/maintenance_executors.go\"\n",
		"docs-site/x.go":                      "package x\n\nvar f = \"pkg/hub/maintenance_executors.go\"\n",
	})
	cfg := func(strict bool, out *bytes.Buffer) *Config {
		return &Config{
			SrcDir: filepath.Join(dir, "pkg", "hub"), DstDir: filepath.Join(dir, "pkg", "hub", "maintenance"),
			Files: []string{"maintenance_executors.go"}, NoGit: true, DryRun: true, Strict: strict, Stdout: out,
		}
	}
	const dst = "update it to pkg/hub/maintenance/maintenance_executors.go"
	const base = "check what it matches after the move"
	high := []string{
		`pkg/hub/authzop/catalog.go:7: Go string literal "pkg/hub/maintenance_executors.go" names moved pkg/hub/maintenance_executors.go by its repo path: after the move it no longer matches the file - ` + dst,
		`pkg/util/logging/request_path_sites_test.go:6: Go string literal "maintenance_executors.go" names moved pkg/hub/maintenance_executors.go by its base name: after the move it no longer matches the file - ` + base,
		`pkg/secret/backend_test.go:5: Go string literals "pkg", "hub", "maintenance_executors.go" names moved pkg/hub/maintenance_executors.go by its path elements: after the move it no longer matches the file - update the elements to pkg/hub/maintenance/maintenance_executors.go`,
		`pkg/hub/stay_test.go:3: Go string literal "maintenance_executors.go:Run:GetSecretValue" names moved pkg/hub/maintenance_executors.go by its base name`,
		"pkg/hub/stay_test.go:9: Go string literal `maintenance_executors.go` names moved pkg/hub/maintenance_executors.go by its base name: after the move it no longer matches the file - " + base,
		"(the pkgmove:ref-ok marker at pkg/hub/stay_test.go:8 has no reason, so it accepts nothing)",
		`hack/check-guard.sh:2: line "exempt=\"pkg/hub/maintenance_executors.go\"" names moved pkg/hub/maintenance_executors.go by its repo path: after the move it no longer matches the file - ` + dst,
		`Makefile:2: line "@grep -q maintenance_executors.go pkg/hub/*.go" names moved pkg/hub/maintenance_executors.go by its base name`,
		`.github/workflows/ci.yml:4: line "- run: test -f pkg/hub/maintenance_executors.go" names moved pkg/hub/maintenance_executors.go by its repo path`,
		`pkg/hub/stay2_test.go:5: Go string literal "maintenance_executors.go" names moved pkg/hub/maintenance_executors.go by its base name`,
		`pkg/util/logging/testdata/broken.go:4: line "var f = \"pkg/hub/maintenance_executors.go\"" names moved pkg/hub/maintenance_executors.go by its repo path`,
	}
	info := []string{
		`pkg/hub/stay_test.go:6: Go string literal "pkg/hub/maintenance_executors.go" names moved pkg/hub/maintenance_executors.go by its repo path; ref-ok: "the history table keeps the old path on purpose."`,
		`hack/check-guard.sh:4: line "fixture=\"pkg/hub/maintenance_executors.go\"" names moved pkg/hub/maintenance_executors.go by its repo path; ref-ok: "fixture path for the guard's self-test."`,
	}
	absent := []string{"catalog.go:6", "premaintenance", ".gox", "scripts/other.sh", "notes.md", ".design/", "docs-site/", "pkg/hub/maintenance_executors.go:4: Go string literal",
		"vendor/x.go", "node_modules/", "pkg/other/"}

	// Without -strict the references are HIGH findings and the plan stands.
	var buf bytes.Buffer
	if err := run(cfg(false, &buf)); err != nil {
		t.Fatalf("dry run without -strict: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if n := strings.Count(out, "\n== HIGH: outside reference to a moved file ==\n"); n != 1 {
		t.Errorf("want one HIGH outside-reference section, got %d:\n%s", n, out)
	}
	for _, w := range append(append(high, info...), "== INFO: outside reference to a moved file accepted (pkgmove:ref-ok) ==") {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	for _, w := range absent {
		if strings.Contains(out, w) {
			t.Errorf("unexpected %q:\n%s", w, out)
		}
	}
	// Exactly the expected sites: 10 HIGH lines (one per line of code, the
	// catalog entry's two literals included) and 2 INFO lines.
	if got := strings.Count(out, "after the move it no longer matches the file"); got != 10 {
		t.Errorf("got %d HIGH outside references, want 10:\n%s", got, out)
	}
	if got := strings.Count(out, "; ref-ok: "); got != 2 {
		t.Errorf("got %d accepted outside references, want 2:\n%s", got, out)
	}

	// Under -strict each one is an error.
	buf.Reset()
	if err := run(cfg(true, &buf)); !errors.Is(err, errPlan) {
		t.Fatalf("want errPlan under -strict, got %v\n%s", err, buf.String())
	}
	out = buf.String()
	if got := strings.Count(out, ": -strict: outside reference to a moved file: "); got != 10 {
		t.Errorf("got %d -strict outside-reference errors, want 10:\n%s", got, out)
	}
	if !strings.Contains(out, "pkg/hub/authzop/catalog.go:7: -strict: outside reference to a moved file: ") {
		t.Errorf("missing the catalog error:\n%s", out)
	}
}

// outRefTree is the tree of TestRewriteOutRefs: pkg/hub/authorize.go moves
// to pkg/hub/authorize, exporting authorizeAgentCreate (staying code calls
// it) and the method gate.check, while the package func check stays
// unexported. Outside references cover each form -rewrite-outrefs rewrites
// and each it leaves report-only.
func outRefTree() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/fx\n\ngo 1.26\n",
		"pkg/hub/authorize.go": "package hub\n\n" +
			"func authorizeAgentCreate() bool { return authorizeHelper() && check() }\n\n" +
			"func authorizeHelper() bool { return true }\n\n" +
			"func check() bool { return true }\n\n" +
			"type gate struct{}\n\n" +
			"func (gate) check() bool { return true }\n\n" +
			"// extraCheckFor is only text here: authorize.go does not use extraCheck.\n",
		"pkg/hub/stay.go": "package hub\n\n" +
			"func create() bool { return authorizeAgentCreate() }\n\n" +
			"func useGate() bool { var g gate; return g.check() }\n\n" +
			"var authorizeFile = \"pkg/hub/authorize.go\"\n\n" + // line 7: staying source file
			"func extra() bool { return extraCheck() }\n",
		"pkg/hub/authorize_extra.go": "package hub\n\nfunc extraCheck() bool { return true }\n",
		// A moved file asset (not Go).
		"pkg/hub/authorize_rules.json": "{}\n",
		// An outside Go file whose alignment depends on the literal's length.
		"pkg/other/table.go": "package other\n\nvar (\n\ta  = \"pkg/hub/authorize.go\" // moved\n\tbb = \"x\"                    // kept\n)\n",
		// A moved file outside the type-checked unit (an external test).
		"pkg/hub/authorize_ext_test.go": "package hub_test\n\n// extraCheckFor is only text here.\nfunc neverFn() bool { return true }\n",
		// The permissions registry shape: "path" and "path:symbol".
		"pkg/hub/permissions/registry.go": "package permissions\n\n" +
			"var Enforcement = []string{\n" +
			"\t\"pkg/hub/authorize.go:authorizeAgentCreate\",\n" + // line 4: renamed symbol
			"\t\"pkg/hub/authorize.go:authorizeHelper\",\n" + // line 5: symbol kept
			"\t\"pkg/hub/authorize.go\",\n" + // line 6
			"\t`pkg/hub/authorize.go:authorizeAgentCreate`,\n" + // line 7: raw literal
			"\t\"pkg/hub/authorize.go:check\",\n" + // line 8: ambiguous (method renamed, func kept)
			"\t\"pkg/hub/authorize.go:extraCheck\",\n" + // line 9: renamed only in authorize_extra.go
			"\t\"pkg/hub/authorize_ext_test.go:extraCheck\",\n" + // line 10: named file not type-checked
			"\t\"pkg/hub/authorize_ext_test.go:neverFn\",\n" + // line 11: not type-checked, renamed nowhere: kept
			"\t\"pkg/hub/authorize.go:extraCheckFor\",\n" + // line 12: not used in the file, renamed nowhere: kept
			"}\n",
		// The authzop catalog shape: File and Scope are repo paths; Function
		// names a function of File, rewritten only when the move renames it there.
		"pkg/hub/authzop/catalog.go": "package authzop\n\n" +
			"type MutationClassification struct{ File, Function, Symbol, Scope string }\n\n" +
			"const otherName = \"authorizeAgentCreate\"\n\n" +
			"var MutationClassifications = []MutationClassification{\n" +
			"\t{File: \"pkg/hub/authorize.go\", Function: \"authorizeHelper\", Symbol: \"GetSecretValue\", Scope: \"pkg/hub/authorize.go\"},\n" + // line 8: not renamed
			"\t{File: \"pkg/hub/authorize.go\", Function: `authorizeAgentCreate`, Symbol: \"CreateX\"},\n" + // line 9: renamed in File (raw literal)
			"\t{File: \"pkg/hub/authorize.go\", Function: otherName, Symbol: \"CreateX\"},\n" + // line 10: not a literal
			"\t{File: \"pkg/hub/authorize.go\", Function: \"extraCheck\", Symbol: \"CreateX\"},\n" + // line 11: renamed in another file
			"\t{File: \"pkg/hub/authorize.go\", Function: \"check\", Symbol: \"CreateX\"},\n" + // line 12: ambiguous
			"\t{File: \"pkg/hub/authorize_ext_test.go\", Function: \"neverFn\", Symbol: \"CreateX\"},\n" + // line 13: not type-checked
			"\t{File: \"pkg/hub/authorize_rules.json\", Function: \"authorizeAgentCreate\", Symbol: \"CreateX\"},\n" + // line 14: File is a moved asset
			"}\n",
		// Negatives in Go: base name, path elements, a longer literal, a
		// second colon, a ref-ok'd literal.
		"pkg/other/other.go": "package other\n\nimport \"path/filepath\"\n\n" +
			"var base = \"authorize.go\"\n" + // line 5
			"var elems = filepath.Join(\"pkg\", \"hub\", \"authorize.go\")\n" + // line 6
			"var longer = \"../../pkg/hub/authorize.go\"\n" + // line 7
			"var census = \"pkg/hub/authorize.go:authorizeAgentCreate:GetSecretValue\"\n" + // line 8
			"// pkgmove:ref-ok the history table keeps the old path.\n" +
			"var history = \"pkg/hub/authorize.go\"\n", // line 10
		// hack/**/*.txt: the baseline shapes, a ref-ok line and negatives.
		"hack/hub-store-reassign-baseline.txt": "# Baseline for hack/check.sh.\n" +
			"#   <file> | <count>\n" +
			"# see pkg/hub/authorize.go\n" + // line 3: not at line start
			"pkg/hub/authorize.go | 2\n" + // line 4
			"pkg/hub/zz.go | 1\n",
		"hack/winsec-unprotected.txt": "pkg/hub/authorize.go unprotected\r\n" + // line 1: CRLF kept
			"pkg/hub/authorize.go:authorizeAgentCreate allowed\n" + // line 2
			"pkg/hub/authorize.go\n" + // line 3: path alone
			"pkg/hub/authorize.go\r\n", // line 4: path alone, CRLF
		"hack/sub/allow.txt": "# pkgmove:ref-ok kept on purpose for the self-test.\n" +
			"pkg/hub/authorize.go | 1\n" + // line 2: accepted
			"pkg/hub/authorize.go:12 x\n" + // line 3: not an identifier
			"pkg/hub/authorize.go|3\n" + // line 4: no blank after the path
			"# pkgmove:ref-ok\n" + // line 5: a marker without a reason
			"pkg/hub/authorize.go pkg/hub/authorize.go\n", // line 6: named twice
		// A function column: rewritten only when the move keeps that name.
		"hack/store-delete-callers-allowlist.txt": "pkg/hub/authorize.go | authorizeAgentCreate | 1\n" + // line 1: renamed, HIGH
			"pkg/hub/authorize.go | authorizeHelper | 1\n", // line 2: kept, rewritten
		// Text under testdata is out of scope (golden files cannot take a marker).
		"hack/tool/testdata/case/want/report.txt": "pkg/hub/authorize.go | 1\n",
		// Shell lines stay report-only, even at line start.
		"hack/check.sh": "pkg/hub/authorize.go | 9\n",
	}
}

// outRefFiles are the files TestRewriteOutRefs moves.
var outRefFiles = []string{"authorize.go", "authorize_extra.go", "authorize_ext_test.go", "authorize_rules.json"}

// enforcementOK mirrors assertEnforcementReferenceExists in
// pkg/hub/permission_registry_test.go: the file part of each entry must
// exist (relative to the module root) and contain the symbol part.
func enforcementOK(t *testing.T, root string) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "pkg/hub/permissions/registry.go"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "registry.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		v, _ := strconv.Unquote(bl.Value)
		file, sym, _ := strings.Cut(v, ":")
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil || !strings.Contains(string(content), sym) {
			bad = append(bad, v)
		}
		return true
	})
	return bad
}

// TestRewriteOutRefs checks -rewrite-outrefs: the exact repo-path forms are
// rewritten to the new path (and a renamed symbol to its new name) in Go
// string literals and at the start of hack/**/*.txt lines; base names, path
// elements, longer literals, ref-ok'd references, shell lines and ambiguous
// symbols stay report-only. The registry check passes before and after.
func TestRewriteOutRefs(t *testing.T) {
	requireGo(t)
	dir := t.TempDir()
	writeTree(t, dir, outRefTree())
	cfg := func(dryRun, strict bool, out *bytes.Buffer) *Config {
		return &Config{
			SrcDir: filepath.Join(dir, "pkg", "hub"), DstDir: filepath.Join(dir, "pkg", "hub", "authorize"),
			Files: outRefFiles, NoGit: true, DryRun: dryRun, Strict: strict, Typecheck: true,
			RewriteOutRefs: true, Stdout: out,
		}
	}
	if bad := enforcementOK(t, dir); len(bad) != 0 {
		t.Fatalf("registry check fails before the move: %v", bad)
	}
	before := readTree(t, dir)

	// Dry run: the plan lists the rewrites, nothing changes.
	var buf bytes.Buffer
	if err := run(cfg(true, false, &buf)); err != nil {
		t.Fatalf("dry run: %v\n%s", err, buf.String())
	}
	dry := buf.String()
	compareTrees(t, readTree(t, dir), before)

	const newPath = "pkg/hub/authorize/authorize.go"
	rewritten := []string{
		`pkg/hub/stay.go:7: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		`pkg/hub/permissions/registry.go:4: "pkg/hub/authorize.go:authorizeAgentCreate" -> "` + newPath + `:AuthorizeAgentCreate"`,
		`pkg/hub/permissions/registry.go:5: "pkg/hub/authorize.go:authorizeHelper" -> "` + newPath + `:authorizeHelper"`,
		`pkg/hub/permissions/registry.go:6: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		"pkg/hub/permissions/registry.go:7: `pkg/hub/authorize.go:authorizeAgentCreate` -> `" + newPath + ":AuthorizeAgentCreate`",
		`pkg/hub/authzop/catalog.go:8: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		`pkg/hub/authzop/catalog.go:9: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		"pkg/hub/authzop/catalog.go:9: `authorizeAgentCreate` -> `AuthorizeAgentCreate`",
		`pkg/hub/authzop/catalog.go:10: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		`pkg/hub/authzop/catalog.go:11: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		`pkg/hub/authzop/catalog.go:12: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		`hack/hub-store-reassign-baseline.txt:4: "pkg/hub/authorize.go | 2" -> "` + newPath + ` | 2"`,
		`hack/winsec-unprotected.txt:1: "pkg/hub/authorize.go unprotected" -> "` + newPath + ` unprotected"`,
		`hack/winsec-unprotected.txt:2: "pkg/hub/authorize.go:authorizeAgentCreate allowed" -> "` + newPath + `:AuthorizeAgentCreate allowed"`,
		`hack/winsec-unprotected.txt:3: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		`hack/winsec-unprotected.txt:4: "pkg/hub/authorize.go" -> "` + newPath + `"`,
		`hack/store-delete-callers-allowlist.txt:2: "pkg/hub/authorize.go | authorizeHelper | 1" -> "` + newPath + ` | authorizeHelper | 1"`,
		`pkg/hub/authzop/catalog.go:13: "pkg/hub/authorize_ext_test.go" -> "pkg/hub/authorize/authorize_ext_test.go"`,
		`pkg/hub/authzop/catalog.go:14: "pkg/hub/authorize_rules.json" -> "pkg/hub/authorize/authorize_rules.json"`,
		`pkg/hub/permissions/registry.go:11: "pkg/hub/authorize_ext_test.go:neverFn" -> "pkg/hub/authorize/authorize_ext_test.go:neverFn"`,
		`pkg/hub/permissions/registry.go:12: "pkg/hub/authorize.go:extraCheckFor" -> "` + newPath + `:extraCheckFor"`,
		`pkg/other/table.go:4: "pkg/hub/authorize.go" -> "` + newPath + `"`,
	}
	// The first catalog entry has two path literals, so 23 rewrites in all.
	if !strings.Contains(dry, "\nOutside references to moved files rewritten (-rewrite-outrefs) (23):\n") {
		t.Errorf("want 23 rewrites in the plan:\n%s", dry)
	}
	for _, w := range rewritten {
		if !strings.Contains(dry, "  "+w+"\n") {
			t.Errorf("plan is missing rewrite %q:\n%s", w, dry)
		}
	}
	high := []string{
		`pkg/hub/permissions/registry.go:8: Go string literal "pkg/hub/authorize.go:check" names moved pkg/hub/authorize.go by its repo path: after the move it no longer matches the file - update it to ` + newPath + ", or accept it with a pkgmove:ref-ok <reason> comment on the line above (not rewritten: check is ambiguous after the move, it becomes Check or check)",
		`pkg/other/other.go:5: Go string literal "authorize.go" names moved pkg/hub/authorize.go by its base name`,
		`pkg/other/other.go:6: Go string literals "pkg", "hub", "authorize.go" names moved pkg/hub/authorize.go by its path elements`,
		`pkg/other/other.go:7: Go string literal "../../pkg/hub/authorize.go" names moved pkg/hub/authorize.go by its repo path`,
		`pkg/other/other.go:8: Go string literal "pkg/hub/authorize.go:authorizeAgentCreate:GetSecretValue" names moved pkg/hub/authorize.go by its repo path`,
		`hack/hub-store-reassign-baseline.txt:3: line "# see pkg/hub/authorize.go" names moved pkg/hub/authorize.go by its repo path`,
		`hack/sub/allow.txt:3: line "pkg/hub/authorize.go:12 x" names moved`,
		`hack/sub/allow.txt:4: line "pkg/hub/authorize.go|3" names moved`,
		`hack/sub/allow.txt:6: line "pkg/hub/authorize.go pkg/hub/authorize.go" names moved pkg/hub/authorize.go by its repo path: after the move it no longer matches the file - update it to ` + newPath + ", or accept it with a pkgmove:ref-ok <reason> comment on the line above (not rewritten: the line names the path more than once) (the pkgmove:ref-ok marker at hack/sub/allow.txt:5 has no reason, so it accepts nothing)\n",
		`hack/check.sh:1: line "pkg/hub/authorize.go | 9" names moved`,
		`pkg/hub/permissions/registry.go:9: Go string literal "pkg/hub/authorize.go:extraCheck" names moved pkg/hub/authorize.go by its repo path: after the move it no longer matches the file - update it to ` + newPath + ", or accept it with a pkgmove:ref-ok <reason> comment on the line above (not rewritten: extraCheck is renamed in pkg/hub/authorize_extra.go, not in pkg/hub/authorize.go)",
		`pkg/hub/permissions/registry.go:10: Go string literal "pkg/hub/authorize_ext_test.go:extraCheck" names moved pkg/hub/authorize_ext_test.go by its repo path: after the move it no longer matches the file - update it to pkg/hub/authorize/authorize_ext_test.go, or accept it with a pkgmove:ref-ok <reason> comment on the line above (not rewritten: pkg/hub/authorize_ext_test.go was not type-checked, and the move renames extraCheck in pkg/hub/authorize_extra.go)`,
		`hack/store-delete-callers-allowlist.txt:1: line "pkg/hub/authorize.go | authorizeAgentCreate | 1" names moved pkg/hub/authorize.go by its repo path: after the move it no longer matches the file - update it to ` + newPath + ", or accept it with a pkgmove:ref-ok <reason> comment on the line above (not rewritten: the line also names authorizeAgentCreate, which the move renames to AuthorizeAgentCreate)",
	}
	for _, w := range high {
		if !strings.Contains(dry, w) {
			t.Errorf("missing HIGH %q:\n%s", w, dry)
		}
	}
	if got := strings.Count(dry, "after the move it no longer matches the file"); got != len(high) {
		t.Errorf("got %d HIGH outside references, want %d:\n%s", got, len(high), dry)
	}
	for _, w := range []string{
		`pkg/other/other.go:10: Go string literal "pkg/hub/authorize.go" names moved pkg/hub/authorize.go by its repo path; ref-ok: "the history table keeps the old path."`,
		`hack/sub/allow.txt:2: line "pkg/hub/authorize.go | 1" names moved pkg/hub/authorize.go by its repo path; ref-ok: "kept on purpose for the self-test."`,
		"\n== INFO: outside reference to a moved file rewritten (-rewrite-outrefs) ==\n",
		`pkg/hub/permissions/registry.go:4: Go string literal "pkg/hub/authorize.go:authorizeAgentCreate" names moved pkg/hub/authorize.go by its repo path; rewritten to "` + newPath + `:AuthorizeAgentCreate"`,
	} {
		if !strings.Contains(dry, w) {
			t.Errorf("missing INFO %q:\n%s", w, dry)
		}
	}
	// Identical findings print once: the first catalog entry's File and
	// Scope literals share one INFO line.
	if got := strings.Count(dry, "; rewritten to "); got != 22 {
		t.Errorf("got %d rewritten INFO lines, want 22:\n%s", got, dry)
	}
	if strings.Contains(dry, "testdata/case/want/report.txt") {
		t.Errorf("a .txt file under testdata is scanned:\n%s", dry)
	}
	// Function fields that may go stale and are not rewritten: HIGH.
	const fnCat = "Function field next to a rewritten File field not rewritten (-rewrite-outrefs)"
	fnHigh := []string{
		`pkg/hub/authzop/catalog.go:10: Function otherName of the entry whose File names moved pkg/hub/authorize.go: the value is not a string literal; check it by hand`,
		`pkg/hub/authzop/catalog.go:11: Function "extraCheck" of the entry whose File names moved pkg/hub/authorize.go: extraCheck is not declared in pkg/hub/authorize.go; the move renames it in pkg/hub/authorize_extra.go; check it by hand`,
		`pkg/hub/authzop/catalog.go:12: Function "check" of the entry whose File names moved pkg/hub/authorize.go: check is ambiguous after the move, it becomes Check or check; check it by hand`,
		`pkg/hub/authzop/catalog.go:13: Function "neverFn" of the entry whose File names moved pkg/hub/authorize_ext_test.go: neverFn in pkg/hub/authorize_ext_test.go was not type-checked (build tags or an external test), so its rename is unknown; check it by hand`,
	}
	for _, w := range append(fnHigh, "\n== HIGH: "+fnCat+" ==\n", "pkg/hub/authzop/catalog.go:9: Function `authorizeAgentCreate` of the entry whose File names moved pkg/hub/authorize.go; rewritten to `AuthorizeAgentCreate` (renamed by the move)") {
		if !strings.Contains(dry, w) {
			t.Errorf("missing %q:\n%s", w, dry)
		}
	}
	if got := strings.Count(dry, "check it by hand"); got != len(fnHigh) {
		t.Errorf("got %d Function HIGH lines, want %d:\n%s", got, len(fnHigh), dry)
	}
	if strings.Contains(dry, `Function "authorizeHelper"`) {
		t.Errorf("a Function the move does not rename is reported:\n%s", dry)
	}
	// Next to a File naming a moved asset (no Go declarations), Function is
	// neither rewritten nor reported.
	if strings.Contains(dry, "catalog.go:14: Function") {
		t.Errorf("the Function next to a moved asset is reported or rewritten:\n%s", dry)
	}

	// Under -strict the rewritten references are no errors; the rest are.
	buf.Reset()
	if err := run(cfg(true, true, &buf)); !errors.Is(err, errPlan) {
		t.Fatalf("want errPlan under -strict, got %v\n%s", err, buf.String())
	}
	if got := strings.Count(buf.String(), ": -strict: outside reference to a moved file: "); got != len(high) {
		t.Errorf("got %d -strict outside-reference errors, want %d:\n%s", got, len(high), buf.String())
	}
	if got := strings.Count(buf.String(), ": -strict: "+fnCat+": "); got != len(fnHigh) {
		t.Errorf("got %d -strict Function errors, want %d:\n%s", got, len(fnHigh), buf.String())
	}

	// The real run: the plan equals the dry run's, and the files change as listed.
	buf.Reset()
	if err := run(cfg(false, false, &buf)); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if gen := buf.String(); !strings.HasPrefix(gen, dry) {
		t.Errorf("the run's plan differs from the dry run's:\n--- dry run\n%s\n--- run\n%s", dry, gen)
	}
	got := readTree(t, dir)
	want := map[string]string{
		// The move renames g.check (exported method); -rewrite-outrefs the literal.
		"pkg/hub/stay.go": strings.Replace(strings.Replace(before["pkg/hub/stay.go"], `"pkg/hub/authorize.go"`, `"`+newPath+`"`, 1), "g.check()", "g.Check()", 1),
		"pkg/hub/permissions/registry.go": "package permissions\n\n" +
			"var Enforcement = []string{\n" +
			"\t\"" + newPath + ":AuthorizeAgentCreate\",\n" +
			"\t\"" + newPath + ":authorizeHelper\",\n" +
			"\t\"" + newPath + "\",\n" +
			"\t`" + newPath + ":AuthorizeAgentCreate`,\n" +
			"\t\"pkg/hub/authorize.go:check\",\n" +
			"\t\"pkg/hub/authorize.go:extraCheck\",\n" +
			"\t\"pkg/hub/authorize_ext_test.go:extraCheck\",\n" +
			"\t\"pkg/hub/authorize/authorize_ext_test.go:neverFn\",\n" +
			"\t\"" + newPath + ":extraCheckFor\",\n" +
			"}\n",
		"pkg/hub/authzop/catalog.go": strings.NewReplacer(`"pkg/hub/authorize.go"`, `"`+newPath+`"`, `"pkg/hub/authorize_ext_test.go"`, `"pkg/hub/authorize/authorize_ext_test.go"`, `"pkg/hub/authorize_rules.json"`, `"pkg/hub/authorize/authorize_rules.json"`,
			"Function: `authorizeAgentCreate`", "Function: `AuthorizeAgentCreate`").Replace(before["pkg/hub/authzop/catalog.go"]),
		"hack/hub-store-reassign-baseline.txt": strings.Replace(before["hack/hub-store-reassign-baseline.txt"], "pkg/hub/authorize.go | 2", newPath+" | 2", 1),
		"hack/winsec-unprotected.txt": newPath + " unprotected\r\n" +
			newPath + ":AuthorizeAgentCreate allowed\n" +
			newPath + "\n" +
			newPath + "\r\n",
		"hack/store-delete-callers-allowlist.txt": "pkg/hub/authorize.go | authorizeAgentCreate | 1\n" + newPath + " | authorizeHelper | 1\n",
		// gofmt realigns the comments to the longer literal.
		"pkg/other/table.go": "package other\n\nvar (\n\ta  = \"" + newPath + "\" // moved\n\tbb = \"x\"                              // kept\n)\n",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s:\n got %q\nwant %q", name, got[name], w)
		}
	}
	for _, name := range []string{"pkg/other/other.go", "hack/sub/allow.txt", "hack/check.sh", "hack/tool/testdata/case/want/report.txt", "go.mod"} {
		if got[name] != before[name] {
			t.Errorf("%s changed:\n got %q\nwant %q", name, got[name], before[name])
		}
	}
	if !strings.Contains(got["pkg/hub/authorize/authorize.go"], "func AuthorizeAgentCreate() bool") {
		t.Fatalf("authorizeAgentCreate was not exported:\n%s", got["pkg/hub/authorize/authorize.go"])
	}
	// The registry check passes as before, except the three entries left as
	// HIGH findings for the reviewer (ambiguous, renamed in another file,
	// named file not type-checked).
	if bad := enforcementOK(t, dir); strings.Join(bad, ",") != "pkg/hub/authorize.go:check,pkg/hub/authorize.go:extraCheck,pkg/hub/authorize_ext_test.go:extraCheck" {
		t.Errorf("registry check after the move: failing entries %v, want only the three HIGH ones", bad)
	}
}

// TestRewriteOutRefsOffByDefault proves the flag is opt-in: without it the
// run rewrites no outside reference (every outside file keeps its bytes),
// prints no rewrite section, and reports the references (the hack/**/*.txt
// lines included) as before.
func TestRewriteOutRefsOffByDefault(t *testing.T) {
	requireGo(t)
	dir := t.TempDir()
	writeTree(t, dir, outRefTree())
	before := readTree(t, dir)
	var buf bytes.Buffer
	err := run(&Config{
		SrcDir: filepath.Join(dir, "pkg", "hub"), DstDir: filepath.Join(dir, "pkg", "hub", "authorize"),
		Files: outRefFiles, NoGit: true, Typecheck: true, Stdout: &buf,
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	got := readTree(t, dir)
	for name, content := range before {
		if strings.HasPrefix(name, "pkg/hub/") && !strings.HasPrefix(name, "pkg/hub/permissions/") && !strings.HasPrefix(name, "pkg/hub/authzop/") {
			continue // the source package itself is rewritten by the move
		}
		if got[name] != content {
			t.Errorf("%s changed without -rewrite-outrefs:\n got %q\nwant %q", name, got[name], content)
		}
	}
	if !strings.Contains(got["pkg/hub/stay.go"], `var authorizeFile = "pkg/hub/authorize.go"`) {
		t.Errorf("staying literal rewritten without -rewrite-outrefs:\n%s", got["pkg/hub/stay.go"])
	}
	for _, w := range []string{"-rewrite-outrefs", "rewritten to", "not rewritten", "Function"} {
		if strings.Contains(out, w) {
			t.Errorf("unexpected %q without -rewrite-outrefs:\n%s", w, out)
		}
	}
	// 13 HIGH in the dry-run list, plus the 21 lines of rewritable path
	// references (the first catalog entry's two literals print once).
	if got := strings.Count(out, "after the move it no longer matches the file"); got != 34 {
		t.Errorf("got %d HIGH outside references, want 34:\n%s", got, out)
	}
	for _, w := range []string{
		`hack/hub-store-reassign-baseline.txt:4: line "pkg/hub/authorize.go | 2" names moved pkg/hub/authorize.go by its repo path: after the move it no longer matches the file - update it to pkg/hub/authorize/authorize.go, or accept it with a pkgmove:ref-ok <reason> comment on the line above` + "\n",
		`hack/sub/allow.txt:2: line "pkg/hub/authorize.go | 1" names moved pkg/hub/authorize.go by its repo path; ref-ok: "kept on purpose for the self-test."`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	// The registry entries now point at a missing file: -rewrite-outrefs is
	// what keeps the check passing.
	if bad := enforcementOK(t, dir); len(bad) != 9 {
		t.Errorf("registry check after a move without -rewrite-outrefs: %d failing entries %v, want 9", len(bad), bad)
	}

	// -rewrite-outrefs does not combine with -rewrite-aliases.
	if err := run(&Config{SrcDir: filepath.Join(dir, "pkg", "hub"), RewriteAliases: true, RewriteOutRefs: true, Stdout: &buf}); err == nil || !strings.Contains(err.Error(), "-rewrite-outrefs does not apply to -rewrite-aliases") {
		t.Errorf("-rewrite-outrefs with -rewrite-aliases: got %v", err)
	}
}

// TestRewriteOutRefsRollback checks that a failed move restores the outside
// files -rewrite-outrefs edited (and, with git, unstages them), and that a
// staged change to an outside file it would edit stops the move before
// anything is touched.
func TestRewriteOutRefsRollback(t *testing.T) {
	requireGo(t)
	cfg := func(dir string, git bool, out *bytes.Buffer) *Config {
		return &Config{
			SrcDir: filepath.Join(dir, "pkg", "hub"), DstDir: filepath.Join(dir, "pkg", "hub", "authorize"),
			Files: outRefFiles, NoGit: !git, Typecheck: true, RewriteOutRefs: true, Stdout: out,
		}
	}
	for _, git := range []bool{false, true} {
		name := "no-git"
		if git {
			name = "git"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, outRefTree())
			if git {
				if _, err := exec.LookPath("git"); err != nil {
					t.Skip("git not in PATH")
				}
				gitRun(t, dir, "init", "-q")
				gitRun(t, dir, "add", "-A")
				gitRun(t, dir, "commit", "-q", "-m", "fixture")
			}
			before := readTree(t, dir)
			testHookBeforeStage = func() error { return errors.New("injected failure") }
			defer func() { testHookBeforeStage = nil }()
			var buf bytes.Buffer
			err := run(cfg(dir, git, &buf))
			if err == nil || !strings.Contains(err.Error(), "injected failure (all changes rolled back)") {
				t.Fatalf("want a rolled-back failure, got %v\n%s", err, buf.String())
			}
			// The edits were planned (so the rollback had outside files to restore).
			if !strings.Contains(buf.String(), "Outside references to moved files rewritten (-rewrite-outrefs) (23):") {
				t.Fatalf("no outside rewrites planned:\n%s", buf.String())
			}
			compareTrees(t, readTree(t, dir), before)
			if git {
				cmd := exec.Command("git", "status", "--porcelain")
				cmd.Dir = dir
				b, err := cmd.Output()
				if err != nil || len(b) != 0 {
					t.Fatalf("git status not clean after rollback: %v\n%s", err, b)
				}
			}
		})
	}
	t.Run("staged outside file", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not in PATH")
		}
		dir := t.TempDir()
		writeTree(t, dir, outRefTree())
		gitRun(t, dir, "init", "-q")
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "fixture")
		reg := filepath.Join(dir, "pkg", "hub", "permissions", "registry.go")
		b, err := os.ReadFile(reg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reg, append(b, []byte("\n// staged edit\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "pkg/hub/permissions/registry.go")
		before := readTree(t, dir)
		var buf bytes.Buffer
		err = run(cfg(dir, true, &buf))
		if err == nil || !strings.Contains(err.Error(), "files touched by the move have staged changes") {
			t.Fatalf("want the staged-changes refusal, got %v\n%s", err, buf.String())
		}
		compareTrees(t, readTree(t, dir), before)
	})
}

// TestTruncateUTF8 checks that the report truncation (shortenRef, shorten)
// never splits a multi-byte UTF-8 sequence, keeps its byte limit, and cuts
// ASCII exactly at the limit.
func TestTruncateUTF8(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    func(string) string
		max  int
	}{
		{"shortenRef", shortenRef, 120},
		{"shorten", shorten, 60},
	} {
		// ASCII: unchanged up to the limit; one byte more is cut to max-3 plus "...".
		if a := strings.Repeat("a", tc.max); tc.f(a) != a {
			t.Errorf("%s: %d ASCII bytes changed to %q", tc.name, tc.max, tc.f(a))
		}
		if a, want := strings.Repeat("a", tc.max+1), strings.Repeat("a", tc.max-3)+"..."; tc.f(a) != want {
			t.Errorf("%s: %d ASCII bytes gave %q, want %q", tc.name, tc.max+1, tc.f(a), want)
		}
		for _, s := range []string{
			strings.Repeat("é", 200), // 2-byte runes
			strings.Repeat("a", tc.max-4) + strings.Repeat("日本語", 10), // a 3-byte rune across the cut at max-3
			strings.Repeat("😀", 50),                                   // 4-byte runes
			"x" + strings.Repeat("😀", 50),                             // 4-byte runes, offset by one
		} {
			got := tc.f(s)
			if !utf8.ValidString(got) {
				t.Errorf("%s(%q...) = %q: not valid UTF-8", tc.name, s[:8], got)
			}
			if len(got) > tc.max {
				t.Errorf("%s: result is %d bytes, want at most %d", tc.name, len(got), tc.max)
			}
			if !strings.HasSuffix(got, "...") || !strings.HasPrefix(s, strings.TrimSuffix(got, "...")) {
				t.Errorf("%s(%q...) = %q: want a prefix of the input plus \"...\"", tc.name, s[:8], got)
			}
			// The back-off to a rune boundary is at most 3 bytes.
			if len(got) < tc.max-3 {
				t.Errorf("%s: result is %d bytes, cut more than one rune short of %d", tc.name, len(got), tc.max)
			}
		}
		if short := "日本"; tc.f(short) != short {
			t.Errorf("%s(%q) = %q, want it unchanged", tc.name, short, tc.f(short))
		}
	}
}

func TestContainsBaseRef(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"x.go", true},
		{"x.go:fn:callee", true},
		{"../../hub/x.go", true},
		{"hub/x.go", true},
		{"src/x.go", false},
		{"$fixture_dir/x.go", false},
		{"pkg/hub/sub/x.go", false},
		{"./x.go", false},
		{"subhub/x.go", false},
		{"src/x.go hub/x.go", true},
		{"src/x.go, x.go", true},
		{"fix.go", false},
	} {
		if got := containsBaseRef(tc.s, "x.go", "hub"); got != tc.want {
			t.Errorf("containsBaseRef(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestContainsPathElem(t *testing.T) {
	for _, tc := range []struct {
		s, p string
		want bool
	}{
		{"x.go", "x.go", true},
		{"hub/x.go", "x.go", true},
		{"../../hub/x.go", "x.go", true},
		{"x.go:fn:callee", "x.go", true},
		{"a x.go b", "x.go", true},
		{"fix.go", "x.go", false},
		{"a-x.go", "x.go", false},
		{"a.x.go", "x.go", false},
		{"x.gox", "x.go", false},
		{"x.go_test", "x.go", false},
		{"fix.go x.go", "x.go", true},
		{"pkg/hub/x.go", "pkg/hub/x.go", true},
		{"./pkg/hub/x.go", "pkg/hub/x.go", true},
		{"xpkg/hub/x.go", "pkg/hub/x.go", false},
		{"pkg/hub/sub/x.go", "pkg/hub/x.go", false},
	} {
		if got := containsPathElem(tc.s, tc.p); got != tc.want {
			t.Errorf("containsPathElem(%q, %q) = %v, want %v", tc.s, tc.p, got, tc.want)
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
		{"// pkgmove:scan-covers guardScanSubdirs", "pkg/hub/sub", false}, // list marker: resolved against the list, not as a directory
		{"// pkgmove:scan-covers guardscansubdirs", "pkg/hub/guardscansubdirs", false},
		{"// pkgmove:scan-covers guardscansubdirs", "guardscansubdirs", true}, // any other word is a directory
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
		if got, want := ms[0].list, strings.HasSuffix(tc.comment, " guardScanSubdirs"); got != want {
			t.Errorf("%q list = %v, want %v", tc.comment, got, want)
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

// extractFixture is a module whose package hub has the alias files that
// hack/hubshard/extract export generates (its marker, its wrapper shapes).
// zz_alias_hubtestutil_chatdb_test.go mirrors pkg/hub's file of that name.
var extractFixture = map[string]string{
	"go.mod": "module example.com/fx\n\ngo 1.26\n",
	"hubtestutil/chatdb.go": `package hubtestutil

import (
	"database/sql"
	"testing"
	"time"
)

const ConversationsTableDDL = "CREATE TABLE conversations (id TEXT)"

var EnvKeys = []string{"GOOGLE_CLOUD_PROJECT"}

func GetTopicConvID(t *testing.T, db *sql.DB, topicID string) string {
	t.Helper()
	_ = db
	return "conv-" + topicID
}

func RawText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	_, _ = db, args
	return query
}

func RequirePostgresDSN(t *testing.T) string {
	t.Helper()
	return "dsn"
}

func RequireUTCInstant(t *testing.T, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func Seed(tb testing.TB, n int) {
	tb.Helper()
	_ = n
}

func NoHelper(t testing.TB) string { return t.Name() }

func Pair(t, u *testing.T) {
	t.Helper()
	_ = u
}

func Extra(t *testing.T) string {
	t.Helper()
	return t.Name()
}

func Reassign(t *testing.T) string {
	t.Helper()
	t = nil
	return "r"
}

// FakeTB has a Helper method but is not a testing type.
type FakeTB struct{}

func (FakeTB) Helper() {}

func Fake(f FakeTB) { f.Helper() }

func Bench(b *testing.B) { b.Helper() }

func Cond(t *testing.T, ok bool) {
	if ok {
		t.Helper()
	}
}

// T and TB have the names of the testing types but are not them.
type T struct{}

func (*T) Helper() {}

func FakeT(t *T) { t.Helper() }

type TB struct{}

func (TB) Helper() {}

func FakeTBValue(tb TB) { tb.Helper() }

func Unnamed(*testing.T) {}

func Marked(t *testing.T) { t.Helper() }

// SafeGo calls recover() only inside a func literal, which a wrapper does
// not affect.
func SafeGo(f func()) {
	func() {
		defer func() { _ = recover() }()
		f()
	}()
}

// Ping and pong recurse into each other with no hazard behind them.
func Ping(n int) int {
	if n <= 0 {
		return 0
	}
	return pong(n - 1)
}

func pong(n int) int { return Ping(n) }

// Deferred recovers only through a callee, which a wrapper does not affect.
func Deferred(f func()) {
	defer handle()
	f()
}

func handle() { _ = recover() }

// GitStub and StubGit mirror hubtestutil's GitLsRemoteStub and
// StubGitLsRemote: the alias file aliases the type, and the wrapper's
// signature uses that alias.
type GitStub struct{ Output string }

func StubGit(t *testing.T) *GitStub {
	t.Helper()
	return &GitStub{}
}

// FarCaller reaches a stack inspection in another file of the package,
// which imports runtime under another name.
func FarCaller(t *testing.T) string {
	t.Helper()
	return locateFar()
}
`,
	"hubtestutil/where2.go": `package hubtestutil

import rt "runtime"

func locateFar() string {
	_, file, _, _ := rt.Caller(2)
	return file
}
`,
	// A dot-import of runtime, in its own package (a dot-import in
	// hubtestutil would clash with its Caller).
	"dotrt/dotrt.go": `package dotrt

import . "runtime"

func DotRT() string {
	_, file, _, _ := Caller(1)
	return file
}
`,
	// Targets a wrapper frame would change (recover, call-stack inspection).
	"hubtestutil/hazard.go": `package hubtestutil

import (
	"log"
	"runtime"
	"testing"
)

func Recover(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Log(r)
	}
}

func Caller(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(1)
	return file
}

func Transitive(t *testing.T) string {
	t.Helper()
	return where()
}

func where() string {
	_, file, _, _ := runtime.Caller(2)
	return file
}

func LogOut() { _ = log.Output(2, "x") }

func LoggerOut(l *log.Logger) { _ = l.Output(2, "x") }

func InLit(t *testing.T) {
	t.Helper()
	func() { _, _, _, _ = runtime.Caller(1) }()
}

func DefaultOut() { _ = log.Default().Output(2, "x") }

func CallerValue() int {
	f := runtime.Callers
	return f(0, nil)
}

func Deep(t *testing.T) int {
	t.Helper()
	return deep1()
}

func deep1() int { return deep2() }

func deep2() int {
	pc := make([]uintptr, 4)
	return runtime.Callers(1, pc)
}

func Cyc(t *testing.T) {
	t.Helper()
	cycA(2)
}

func cycA(n int) {
	if n > 0 {
		cycB(n - 1)
	}
}

func cycB(n int) {
	cycA(n)
	cycC()
}

func cycC() { _, _, _, _ = runtime.Caller(0) }

type rec struct{}

func (rec) locate() string {
	_, file, _, _ := runtime.Caller(2)
	return file
}

// rec2 has a hazard-free method of the same name, declared after rec's, so
// every same-name method is followed, not only the last.
type rec2 struct{}

func (rec2) locate() string { return "" }

func ViaMethod(t *testing.T) string {
	t.Helper()
	var r rec
	return r.locate()
}

func ViaValue(t *testing.T) {
	t.Helper()
	apply(where)
}

func apply(f func() string) { _ = f() }
`,
	"hubtestutil/aliased.go": `package hubtestutil

import (
	rt "runtime"
	"testing"
)

func Aliased(t *testing.T) string {
	t.Helper()
	_, file, _, _ := rt.Caller(1)
	return file
}
`,
	"hubtestutil/dot.go": `package hubtestutil

import . "log"

func DotLog() { _ = Output(2, "x") }
`,
	// A hand-edited Helper wrapper in a pkgmove alias file (pkgmove never
	// generates one): checked like an extract wrapper.
	"hub/zz_alias_pm_test.go": `package hub

// Aliases for symbols moved to
// example.com/fx/hubtestutil
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"testing"

	"example.com/fx/hubtestutil"
)

func pmCaller(t *testing.T) string {
	t.Helper()
	return hubtestutil.Caller(t)
}
`,
	"hub/hub.go":     "package hub\n",
	"hub/sub/sub.go": "package sub\n",
	"hub/zz_alias_hubtestutil_chatdb_test.go": `package hub

import (
	"database/sql"
	"testing"
	"time"

	"example.com/fx/hubtestutil"
)

// The declarations below keep the names of test helpers that moved to
// example.com/fx/hubtestutil
// (generated by hack/hubshard/extract export), so the tests of this
// package need no edits. Code outside this package uses hubtestutil directly.

const conversationsTableDDL = hubtestutil.ConversationsTableDDL

func getTopicConvID(t *testing.T, db *sql.DB, topicID string) string {
	t.Helper()
	return hubtestutil.GetTopicConvID(t, db, topicID)
}

func rawText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	return hubtestutil.RawText(t, db, query, args...)
}

func requirePostgresDSN(t *testing.T) string {
	t.Helper()
	return hubtestutil.RequirePostgresDSN(t)
}

func requireUTCInstant(t *testing.T, got, want time.Time) {
	t.Helper()
	hubtestutil.RequireUTCInstant(t, got, want)
}
`,
	"hub/zz_alias_hubtestutil_more_test.go": `package hub

import (
	"log"
	"testing"

	"example.com/fx/dotrt"
	"example.com/fx/hubtestutil"
)

// The declarations below keep the names of test helpers that moved to
// example.com/fx/hubtestutil
// (generated by hack/hubshard/extract export), so the tests of this
// package need no edits. Code outside this package uses hubtestutil directly.

var envKeys = hubtestutil.EnvKeys

// gitStub is referenced only by the signature of the wrapper stubGit, so
// -rewrite-aliases may remove it only together with stubGit.
type gitStub = hubtestutil.GitStub

func stubGit(t *testing.T) *gitStub {
	t.Helper()
	return hubtestutil.StubGit(t)
}

func seed(tb testing.TB, n int) {
	tb.Helper()
	hubtestutil.Seed(tb, n)
}

func noHelperTarget(t testing.TB) string {
	t.Helper()
	return hubtestutil.NoHelper(t)
}

func otherParam(t, u *testing.T) {
	u.Helper()
	hubtestutil.Pair(t, u)
}

func extraStmt(t *testing.T) string {
	t.Helper()
	t.Log("extra")
	return hubtestutil.Extra(t)
}

func reassigned(t *testing.T) string {
	t.Helper()
	return hubtestutil.Reassign(t)
}

func fake(f hubtestutil.FakeTB) {
	f.Helper()
	hubtestutil.Fake(f)
}

func bench(b *testing.B) {
	b.Helper()
	hubtestutil.Bench(b)
}

func cond(t *testing.T, ok bool) {
	t.Helper()
	hubtestutil.Cond(t, ok)
}

func seedFirst(tb testing.TB, n int) {
	_ = n
	hubtestutil.Seed(tb, n)
}

func fakeT(t *hubtestutil.T) {
	t.Helper()
	hubtestutil.FakeT(t)
}

func fakeTB(tb hubtestutil.TB) {
	tb.Helper()
	hubtestutil.FakeTBValue(tb)
}

func unnamed(t *testing.T) {
	t.Helper()
	hubtestutil.Unnamed(t)
}

func recoverW(t *testing.T) {
	t.Helper()
	hubtestutil.Recover(t)
}

func callerW(t *testing.T) string {
	t.Helper()
	return hubtestutil.Caller(t)
}

func transitiveW(t *testing.T) string {
	t.Helper()
	return hubtestutil.Transitive(t)
}

func logOut() {
	hubtestutil.LogOut()
}

func loggerOut(l *log.Logger) {
	hubtestutil.LoggerOut(l)
}

func marked(t *testing.T) {
	hubtestutil.Marked(t)
}

func safeGo(f func()) {
	hubtestutil.SafeGo(f)
}

func pingW(n int) int {
	return hubtestutil.Ping(n)
}

func deferredW(f func()) {
	hubtestutil.Deferred(f)
}

func aliasedW(t *testing.T) string {
	t.Helper()
	return hubtestutil.Aliased(t)
}

func deepW(t *testing.T) int {
	t.Helper()
	return hubtestutil.Deep(t)
}

func cycW(t *testing.T) {
	t.Helper()
	hubtestutil.Cyc(t)
}

func viaMethodW(t *testing.T) string {
	t.Helper()
	return hubtestutil.ViaMethod(t)
}

func viaValueW(t *testing.T) {
	t.Helper()
	hubtestutil.ViaValue(t)
}

func inLitW(t *testing.T) {
	t.Helper()
	hubtestutil.InLit(t)
}

func defaultOutW() {
	hubtestutil.DefaultOut()
}

func callerValueW() int {
	return hubtestutil.CallerValue()
}

func dotLogW() {
	hubtestutil.DotLog()
}

func farW(t *testing.T) string {
	t.Helper()
	return hubtestutil.FarCaller(t)
}

func dotRTW() string {
	return dotrt.DotRT()
}

func condPlain(t *testing.T, ok bool) {
	hubtestutil.Cond(t, ok)
}
`,
	// Not generated: the marker is only quoted in a string literal.
	"hub/zz_alias_quoted_test.go": `package hub

import (
	"testing"

	"example.com/fx/hubtestutil"
)

const quotedMarker = "(generated by hack/hubshard/extract export)"

func quoted(t *testing.T) string {
	t.Helper()
	return hubtestutil.RequirePostgresDSN(t)
}
`,
	// Not generated: a hand-written file (no zz_alias_ prefix).
	"hub/handwritten_test.go": `package hub

import (
	"testing"

	"example.com/fx/hubtestutil"
)

func handSeed(tb testing.TB, n int) {
	tb.Helper()
	hubtestutil.Seed(tb, n)
}
`,
	"hub/chat_test.go": `package hub

import (
	"testing"
	"time"
)

func TestChat(t *testing.T) {
	_ = requirePostgresDSN(t)
	_ = conversationsTableDDL
	_ = getTopicConvID(t, nil, "topic")
	_ = rawText(t, nil, "select 1", 1)
	now := time.Now().UTC()
	requireUTCInstant(t, now, now)
}
`,
	// A staying user of stubGit (the move subtests never move it).
	"hub/stub_test.go": `package hub

import "testing"

func TestStub(t *testing.T) { _ = stubGit(t) }
`,
	"hub/seed_test.go": `package hub

import "testing"

func TestSeed(t *testing.T) { seed(t, 1) }
`,
	"hub/safe_test.go": `package hub

import "testing"

func TestSafe(t *testing.T) {
	safeGo(func() {})
	_ = pingW(3)
	deferredW(func() {})
}
`,
	"hub/neg_test.go": `package hub

import (
	"log"
	"testing"

	"example.com/fx/hubtestutil"
)

func TestNeg(t *testing.T) {
	_ = noHelperTarget(t)
	otherParam(t, t)
	_ = extraStmt(t)
	_ = reassigned(t)
	_ = envKeys
	fake(hubtestutil.FakeTB{})
	cond(t, true)
	seedFirst(t, 1)
	_ = quoted(t)
	_ = quotedMarker
	handSeed(t, 1)
	fakeT(&hubtestutil.T{})
	fakeTB(hubtestutil.TB{})
	unnamed(t)
	recoverW(t)
	_ = callerW(t)
	_ = transitiveW(t)
	logOut()
	loggerOut(log.Default())
	marked(t)
	_ = aliasedW(t)
	_ = deepW(t)
	cycW(t)
	_ = viaMethodW(t)
	viaValueW(t)
	inLitW(t)
	defaultOutW()
	_ = callerValueW()
	dotLogW()
	_ = pmCaller(t)
	_ = farW(t)
	_ = dotRTW()
	condPlain(t, true)
}

func BenchmarkNeg(b *testing.B) { bench(b) }
`,
}

// TestExtractAliasWrappers checks that references to the alias files of
// hack/hubshard/extract export resolve to their targets, including wrappers
// that start with p.Helper() when the target calls Helper() on the same
// parameter, that every other shape stays unresolved, and that
// -rewrite-aliases prunes those files like pkgmove's own.
func TestExtractAliasWrappers(t *testing.T) {
	requireGo(t)
	setup := func(t *testing.T, extra map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		files := map[string]string{}
		for k, v := range extractFixture {
			files[k] = v
		}
		for k, v := range extra {
			files[k] = v
		}
		writeTree(t, dir, files)
		return dir
	}
	move := func(t *testing.T, files ...string) (string, error) {
		t.Helper()
		dir := setup(t, nil)
		var buf bytes.Buffer
		err := run(&Config{SrcDir: filepath.Join(dir, "hub"), DstDir: filepath.Join(dir, "hub", "sub"),
			Files: files, NoGit: true, DryRun: true, Stdout: &buf})
		return buf.String(), err
	}
	rewrite := func(t *testing.T, extra map[string]string) (string, string) {
		t.Helper()
		dir := setup(t, extra)
		var buf bytes.Buffer
		if err := run(&Config{SrcDir: filepath.Join(dir, "hub"), RewriteAliases: true, NoGit: true, Typecheck: true, Stdout: &buf}); err != nil {
			t.Fatalf("run: %v\n%s", err, buf.String())
		}
		return dir, buf.String()
	}
	wantAll := func(t *testing.T, out string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	}

	t.Run("resolved", func(t *testing.T) {
		out, err := move(t, "chat_test.go", "seed_test.go", "safe_test.go")
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		// All 5 declarations of the chatdb alias file (1 const, 4 Helper
		// wrappers, 3 return and 1 statement shape), a testing.TB wrapper,
		// and plain wrappers whose targets recover only in a func literal
		// or only through a callee, or recurse with no hazard behind them.
		wantAll(t, out,
			"Alias references resolved to their targets (9):",
			"hub/chat_test.go:9: requirePostgresDSN -> hubtestutil.RequirePostgresDSN",
			"hub/chat_test.go:10: conversationsTableDDL -> hubtestutil.ConversationsTableDDL",
			"hub/chat_test.go:11: getTopicConvID -> hubtestutil.GetTopicConvID",
			"hub/chat_test.go:12: rawText -> hubtestutil.RawText",
			"hub/chat_test.go:14: requireUTCInstant -> hubtestutil.RequireUTCInstant",
			"hub/seed_test.go:5: seed -> hubtestutil.Seed",
			"hub/safe_test.go:6: safeGo -> hubtestutil.SafeGo",
			"hub/safe_test.go:7: pingW -> hubtestutil.Ping",
			"hub/safe_test.go:8: deferredW -> hubtestutil.Deferred",
		)
	})

	t.Run("unresolved", func(t *testing.T) {
		out, err := move(t, "neg_test.go")
		if !errors.Is(err, errPlan) {
			t.Fatalf("want a plan error, got %v\n%s", err, out)
		}
		uses := func(kind, name, file string) string {
			return "moved test file uses " + kind + " " + name + " declared in the staying test file " + file + ";"
		}
		const more = "zz_alias_hubtestutil_more_test.go"
		wantAll(t, out,
			uses("func", "noHelperTarget", more),
			"func noHelperTarget calls Helper() on a parameter before forwarding to example.com/fx/hubtestutil.NoHelper, but the target does not call t.Helper() as a top-level statement of its body",
			uses("func", "otherParam", more),
			"but the target does not call u.Helper() as a top-level statement of its body",
			uses("func", "extraStmt", more),
			uses("func", "reassigned", more),
			"but the target assigns its parameter t (or takes its address)",
			uses("var", "envKeys", more),
			// Helper on a parameter that is not testing.TB or *testing.T.
			uses("func", "fake", more),
			uses("func", "bench", more),
			// Helper only inside a nested block of the target.
			uses("func", "cond", more),
			"func cond calls Helper() on a parameter before forwarding to example.com/fx/hubtestutil.Cond, but the target does not call t.Helper() as a top-level statement of its body",
			// Two statements, the first not Helper().
			uses("func", "seedFirst", more),
			// The marker quoted in a string literal does not make a file generated.
			uses("func", "quoted", "zz_alias_quoted_test.go"),
			"func quoted forwards to example.com/fx/hubtestutil.RequirePostgresDSN but is not in a generated alias file (pkgmove or hubshard extract) (a hand-written wrapper is not resolved",
			// A hand-written Helper wrapper.
			uses("func", "handSeed", "handwritten_test.go"),
			"func handSeed forwards to example.com/fx/hubtestutil.Seed but is not in a generated alias file (pkgmove or hubshard extract) (a hand-written wrapper is not resolved",
			// Helper on look-alikes of testing.T and testing.TB from another package.
			uses("func", "fakeT", more),
			uses("func", "fakeTB", more),
			// The target's parameter that receives p is unnamed.
			uses("func", "unnamed", more),
			"func unnamed calls Helper() on a parameter before forwarding to example.com/fx/hubtestutil.Unnamed, but the target's parameter that receives it is unnamed",
			// Targets a wrapper frame would change.
			uses("func", "recoverW", more),
			"func recoverW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Recover, but the target calls recover() directly",
			uses("func", "callerW", more),
			"func callerW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Caller, but the target inspects its call stack (runtime.Caller;",
			uses("func", "transitiveW", more),
			"func transitiveW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Transitive, but the target reaches where, which inspects its call stack (runtime.Caller;",
			uses("func", "logOut", more),
			"func logOut (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.LogOut, but the target uses log.Output call depth",
			uses("func", "loggerOut", more),
			"func loggerOut (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.LoggerOut, but the target may call (*log.Logger).Output",
			uses("func", "marked", more),
			"func marked (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Marked, but the target calls Helper() but the wrapper does not",
			// An aliased runtime import, a chain of depth 2 (runtime.Callers),
			// a cycle with a hazard behind it, a method, a func value, a func
			// literal, a Logger from a call, a runtime.Callers value, a
			// dot-import of log.
			"func aliasedW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Aliased, but the target inspects its call stack (runtime.Caller;",
			"func deepW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Deep, but the target reaches deep2, which inspects its call stack (runtime.Callers;",
			"func cycW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Cyc, but the target reaches cycC, which inspects its call stack (runtime.Caller;",
			"func viaMethodW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.ViaMethod, but the target reaches method locate, which inspects its call stack (runtime.Caller;",
			"func viaValueW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.ViaValue, but the target reaches where, which inspects its call stack (runtime.Caller;",
			"func inLitW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.InLit, but the target inspects its call stack (runtime.Caller;",
			"func defaultOutW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.DefaultOut, but the target may call (*log.Logger).Output",
			"func callerValueW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.CallerValue, but the target inspects its call stack (runtime.Callers;",
			"func dotLogW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.DotLog, but the target is in a file that dot-imports log",
			// A hand-edited Helper wrapper in a pkgmove alias file.
			uses("func", "pmCaller", "zz_alias_pm_test.go"),
			"func pmCaller (a Helper wrapper in a pkgmove alias file) forwards to example.com/fx/hubtestutil.Caller, but the target inspects its call stack (runtime.Caller;",
			// A callee in another file, which imports runtime under another
			// name; a dot-import of runtime; Helper() only in a nested block
			// of the target of a wrapper without Helper().
			"func farW (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.FarCaller, but the target reaches locateFar, which inspects its call stack (runtime.Caller;",
			"func dotRTW (a hubshard extract wrapper) forwards to example.com/fx/dotrt.DotRT, but the target is in a file that dot-imports runtime",
			"func condPlain (a hubshard extract wrapper) forwards to example.com/fx/hubtestutil.Cond, but the target calls Helper() but the wrapper does not",
		)
		if strings.Contains(out, "Alias references resolved") {
			t.Errorf("no reference should resolve:\n%s", out)
		}
		// These are not alias-shaped at all, so they get no reason.
		for _, line := range strings.Split(out, "\n") {
			for _, name := range []string{"extraStmt", "envKeys", "fake", "bench", "seedFirst", "fakeT", "fakeTB"} {
				if strings.Contains(line, "uses func "+name+" ") || strings.Contains(line, "uses var "+name+" ") {
					if strings.Contains(line, "not resolved as an alias") {
						t.Errorf("%s is not alias-shaped, so it needs no reason: %s", name, line)
					}
				}
			}
		}
	})

	t.Run("unmovable", func(t *testing.T) {
		out, err := move(t, "chat_test.go", "zz_alias_hubtestutil_chatdb_test.go")
		wantAll(t, out, "hub/zz_alias_hubtestutil_chatdb_test.go is a generated alias file (pkgmove or hubshard extract); it cannot be moved")
		if !errors.Is(err, errPlan) {
			t.Fatalf("want a plan error, got %v\n%s", err, out)
		}
	})

	// A reference from a file the analysis tags exclude keeps its entry, and
	// an entry used by a kept entry is kept too (stubGit keeps gitStub).
	t.Run("rewrite-prune", func(t *testing.T) {
		dir, out := rewrite(t, map[string]string{
			"hub/integ_test.go": "//go:build integration\n\npackage hub\n\nimport \"testing\"\n\nfunc TestInteg(t *testing.T) { _ = rawText(t, nil, \"q\"); _ = stubGit(t) }\n",
		})
		wantAll(t, out,
			"Alias references resolved to their targets (10):",
			"hub/chat_test.go:11: getTopicConvID -> hubtestutil.GetTopicConvID",
			"hub/seed_test.go:5: seed -> hubtestutil.Seed",
			"hub/safe_test.go:6: safeGo -> hubtestutil.SafeGo",
			"hub/stub_test.go:5: stubGit -> hubtestutil.StubGit",
			"Alias entries removed (8):",
			// stubGit is named in the tag-excluded file, so it stays, and so
			// does gitStub, which only stubGit uses.
			"their entries are kept): rawText, stubGit",
			"gitStub: used by the kept alias entry stubGit",
			"const conversationsTableDDL = example.com/fx/hubtestutil.ConversationsTableDDL",
			"func getTopicConvID = example.com/fx/hubtestutil.GetTopicConvID",
			"func requirePostgresDSN = example.com/fx/hubtestutil.RequirePostgresDSN",
			"func requireUTCInstant = example.com/fx/hubtestutil.RequireUTCInstant",
			"func seed = example.com/fx/hubtestutil.Seed",
			"func safeGo = example.com/fx/hubtestutil.SafeGo",
			"func pingW = example.com/fx/hubtestutil.Ping",
			"func deferredW = example.com/fx/hubtestutil.Deferred",
			"their entries are kept): rawText",
		)
		if strings.Contains(out, "func rawText = ") || strings.Contains(out, "Files deleted") {
			t.Errorf("rawText (named in a tag-excluded file) must be kept, and no file deleted:\n%s", out)
		}
		if strings.Contains(out, "func stubGit = ") || strings.Contains(out, "type gitStub = ") {
			t.Errorf("stubGit (named in a tag-excluded file) and gitStub (used by stubGit) must be kept:\n%s", out)
		}
		b, err := os.ReadFile(filepath.Join(dir, "hub", "zz_alias_hubtestutil_chatdb_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		got := string(b)
		if !strings.Contains(got, "func rawText(") || strings.Contains(got, "func getTopicConvID(") || strings.Contains(got, "\"time\"") {
			t.Errorf("chatdb alias file should keep only rawText, with the unused time import dropped:\n%s", got)
		}
	})

	t.Run("rewrite-delete", func(t *testing.T) {
		dir, out := rewrite(t, nil)
		wantAll(t, out,
			"Alias entries removed (11):",
			"func rawText = example.com/fx/hubtestutil.RawText",
			"type gitStub = example.com/fx/hubtestutil.GitStub",
			"Files deleted (1):\n  hub/zz_alias_hubtestutil_chatdb_test.go",
		)
		if _, err := os.Stat(filepath.Join(dir, "hub", "zz_alias_hubtestutil_chatdb_test.go")); !os.IsNotExist(err) {
			t.Errorf("chatdb alias file not deleted: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "hub", "zz_alias_hubtestutil_more_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, gone := range []string{"func seed(", "func safeGo(", "func pingW(", "func deferredW(", "func stubGit(", "type gitStub ="} {
			if strings.Contains(string(b), gone) {
				t.Errorf("more alias file still has the resolved entry %q:\n%s", gone, b)
			}
		}
		for _, kept := range []string{"func noHelperTarget(", "func recoverW(", "func cycW("} {
			if !strings.Contains(string(b), kept) {
				t.Errorf("more alias file lost the unresolved entry %q:\n%s", kept, b)
			}
		}
	})
}
