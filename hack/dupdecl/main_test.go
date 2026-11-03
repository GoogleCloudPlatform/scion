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
	"go/token"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	// x_windows_test.go declares a third a, compiled only on windows.
	specA := "package p: b declared at b_test.go:3 and p.go:3"
	if runtime.GOOS == "windows" {
		specA = "package p: a declared at p.go:3 and x_windows_test.go:4"
	}
	specWant := []string{"package p: T declared at b_test.go:5 and p.go:5", specA}
	if runtime.GOOS == "windows" {
		specWant = append(specWant, "package p: b declared at b_test.go:3 and p.go:3")
	}
	sort.Strings(specWant)
	cases := []struct {
		dir  string
		tags []string
		want []string
	}{
		{"testdata/dup", nil, []string{
			"package p: Server.helperForTest declared at a_test.go:5 and b_test.go:9",
			"package p: putWarnings declared at a_test.go:3 and b_test.go:7",
		}},
		// init, _, methods of the same name on different receivers, a func
		// named like a method, the external test package and files with
		// mutually exclusive constraints are not duplicates.
		{"testdata/clean", nil, nil},
		{"testdata/clean", []string{"integration"}, nil},
		{"testdata/tagged", nil, nil},
		{"testdata/tagged", []string{"no_sqlite"}, nil},
		{"testdata/tagged", []string{"integration"}, []string{
			"package p: mode declared at c_test.go:5 and d_test.go:5",
			"package p: setup declared at a_test.go:3 and b_test.go:5",
		}},
		{"testdata/tagged", []string{"no_sqlite", "integration"}, []string{
			"package p: setup declared at a_test.go:3 and b_test.go:5",
		}},
		// A multi-name var spec (b redeclared as a type), a duplicate type;
		// the //go:build ignore file, the package main file and (off
		// windows) the GOOS-suffixed file do not count.
		{"testdata/spec", nil, specWant},
		// An import's local name (explicit, last path element, /vN and
		// gopkg.in .vN stripped) against the package block of its package
		// clause. Blank and dot imports, methods and the external test
		// package are not clashes.
		{"testdata/imports", nil, []string{
			"package p: rand imported at a_test.go:6 and declared at b_test.go:9",
			"package p: str imported at a_test.go:7 and declared at b_test.go:11",
			"package p: strings imported at a_test.go:8 and b_test.go:3 and declared at b_test.go:7",
			"package p: yaml imported at a_test.go:10 and declared at b_test.go:13",
		}},
	}
	for _, c := range cases {
		got, _, err := check(c.dir, c.tags)
		if err != nil {
			t.Fatalf("%s %v: %v", c.dir, c.tags, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v:\n got %q\nwant %q", c.dir, c.tags, got, c.want)
		}
	}
}

func TestRun(t *testing.T) {
	cases := []struct {
		args    []string
		code    int
		in, out string
	}{
		{[]string{"testdata/clean"}, 0, "-tags (default): 4 files, no duplicate", ""},
		{[]string{"testdata/dup"}, 1, "putWarnings declared at a_test.go:3 and b_test.go:7", ""},
		{[]string{"testdata/imports"}, 1, "strings imported at a_test.go:8 and b_test.go:3 and declared at b_test.go:7", ""},
		// Every tag set is checked, and one failing set fails the run.
		{[]string{"-tags", "", "-tags", "integration,no_sqlite", "testdata/tagged"}, 1,
			"-tags integration,no_sqlite: package p: setup declared", "mode declared"},
		{[]string{"-tags", "", "-tags", "no_sqlite", "testdata/tagged"}, 0, "-tags no_sqlite: 1 files", ""},
		{nil, 2, "", ""},
		{[]string{"testdata/missing"}, 2, "", ""},
		// A tag set that matches no Go file is an error, not a clean pass.
		{[]string{"testdata/empty"}, 2, "", ""},
		{[]string{"-tags", "integration", "testdata/empty"}, 0, "1 files, no duplicate", ""},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		code := run(c.args, &stdout, &stderr)
		if code != c.code {
			t.Errorf("%q: exit %d, want %d\n%s%s", c.args, code, c.code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), c.in) {
			t.Errorf("%q: output lacks %q:\n%s", c.args, c.in, stdout.String())
		}
		if c.out != "" && strings.Contains(stdout.String(), c.out) {
			t.Errorf("%q: output has %q:\n%s", c.args, c.out, stdout.String())
		}
	}
}

func TestFirstPos(t *testing.T) {
	pos := []token.Position{{Filename: "d.go", Line: 1}, {Filename: "a.go", Line: 2}, {Filename: "c.go", Line: 3}, {Filename: "b.go", Line: 4}}
	if got, want := firstPos(pos, 2), "a.go:2 and b.go:4 and 2 more files"; got != want {
		t.Errorf("firstPos(4, 2) = %q, want %q", got, want)
	}
	if got, want := firstPos(pos[:2], 2), "a.go:2 and d.go:1"; got != want {
		t.Errorf("firstPos(2, 2) = %q, want %q", got, want)
	}
}
