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
	wantErr    bool
}

var goldenCases = []goldenCase{
	{fixture: "basic", files: []string{"maint.go", "maint_test.go"}, git: true},
	{fixture: "tags", files: []string{"move_sqlite.go", "move_plain.go"}},
	{fixture: "xtest", files: []string{"move.go", "move_test.go", "onlymoved_test.go", "data.txt"}},
	{fixture: "embed", files: []string{"move.go"}},
	{fixture: "iface", files: []string{"move.go"}},
	{fixture: "fields", files: []string{"move.go"}, allowField: true},
	// Rejections.
	{fixture: "methods", files: []string{"move.go"}, wantErr: true},
	{fixture: "backref", files: []string{"move.go"}, wantErr: true},
	{fixture: "testsep", files: []string{"a.go", "a_test.go"}, wantErr: true},
	{fixture: "collide", files: []string{"move.go"}, wantErr: true},
	{fixture: "dynamic", files: []string{"move.go"}, wantErr: true},
	{fixture: "cgo", files: []string{"move.go"}, wantErr: true},
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
	cfg := &Config{
		SrcDir:           filepath.Join(dir, "hub"),
		DstDir:           filepath.Join(dir, "hub", "sub"),
		Files:            c.files,
		NoGit:            !c.git,
		DryRun:           dryRun,
		Typecheck:        true,
		AllowFieldExport: c.allowField,
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

// TestDeterministic runs the same move twice on fresh copies and requires
// byte-identical trees and output.
func TestDeterministic(t *testing.T) {
	requireGo(t)
	for _, c := range []goldenCase{goldenCases[0], goldenCases[1], goldenCases[4]} {
		t.Run(c.fixture, func(t *testing.T) {
			dir1, out1, err1 := runFixture(t, c, false)
			dir2, out2, err2 := runFixture(t, c, false)
			if err1 != nil || err2 != nil {
				t.Fatalf("run: %v / %v", err1, err2)
			}
			if out1 != out2 {
				t.Errorf("output differs between runs:\n%s\n---\n%s", out1, out2)
			}
			compareTrees(t, readTree(t, dir1), readTree(t, dir2))
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
