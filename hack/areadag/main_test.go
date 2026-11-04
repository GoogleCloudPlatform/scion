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

var okLine = regexp.MustCompile(`^areadag: OK: ([1-9][0-9]*) module packages, ([1-9][0-9]*) pkg/hub packages, ([1-9][0-9]*) declared \(\d+ leaf, \d+ area, ([1-9][0-9]*) testsupport\), ([1-9][0-9]*) alias files, ([1-9][0-9]*) pkg/hub edges checked\n$`)

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
			if !okLine.MatchString(stdout) {
				t.Fatalf("stdout %q does not match the OK line with nonzero counts", stdout)
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
		{"stale", "declared area gone is not a package in the list"},
		{"emptylist", "no packages"},
		{"nohub", "example.com/m/pkg/hub is not in the list"},
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
