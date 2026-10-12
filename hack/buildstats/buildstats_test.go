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
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestSummarizeActiongraph(t *testing.T) {
	s, err := summarizeActiongraphFile("testdata/actiongraph.json", 3)
	if err != nil {
		t.Fatal(err)
	}
	if s.Actions != 9 || s.Timed != 8 || s.RanTool != 5 {
		t.Errorf("actions/timed/ranTool = %d/%d/%d, want 9/8/5", s.Actions, s.Timed, s.RanTool)
	}
	if s.ByMode["build"] != 5 || s.ByMode["link"] != 1 || s.ByMode["vet"] != 0 {
		t.Errorf("by mode = %v", s.ByMode)
	}
	// 418.5 + 1.5 + 46.4 + 0.004 + 0.8
	if s.BuildSec != 467.2 {
		t.Errorf("build sum = %v, want 467.2", s.BuildSec)
	}
	if s.LinkSec != 23 {
		t.Errorf("link sum = %v, want 23", s.LinkSec)
	}
	if s.BuildOver1s != 3 || s.BuildOver1sSec != 466.4 {
		t.Errorf("build >1s = %d (%vs), want 3 (466.4s)", s.BuildOver1s, s.BuildOver1sSec)
	}
	want := []AGAction{
		{Mode: "build", Package: "example.com/m/pkg/big", WallSec: 418.5, UserSec: 470, SysSec: 9},
		{Mode: "build", Package: "example.com/m/pkg/ent", WallSec: 46.4, UserSec: 60, SysSec: 1},
		{Mode: "link", Package: "example.com/m/pkg/big.test", WallSec: 23, UserSec: 30.5, SysSec: 2},
	}
	if !reflect.DeepEqual(s.Top, want) {
		t.Errorf("top =\n%+v\nwant\n%+v", s.Top, want)
	}
}

func TestSummarizeActiongraphBadJSON(t *testing.T) {
	if _, err := parseActiongraph(strings.NewReader("{")); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseBench(t *testing.T) {
	recs, err := parseBenchFile("testdata/bench.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (package + test main)", len(recs))
	}
	r := recs[0]
	if r.Package != "github.com/GoogleCloudPlatform/scion/pkg/projectkeys" {
		t.Errorf("package = %q", r.Package)
	}
	if r.TotalSec != 0.087 {
		t.Errorf("total = %v, want 0.087", r.TotalSec)
	}
	if len(r.Phases) != 11 {
		t.Errorf("phases = %d, want 11", len(r.Phases))
	}
	var parse, cf BenchPhase
	for _, p := range r.Phases {
		switch p.Phase {
		case "fe:parse":
			parse = p
		case "be:compilefuncs":
			cf = p
		}
	}
	if parse.Count != 811 || parse.Unit != "lines" || parse.Percent != 14.16 {
		t.Errorf("fe:parse = %+v", parse)
	}
	if cf.Count != 68 || cf.Unit != "funcs" || cf.Seconds != 0.067 {
		t.Errorf("be:compilefuncs = %+v", cf)
	}
	if recs[1].Package != "main" || recs[1].TotalSec != 0.024 {
		t.Errorf("second record = %s %v", recs[1].Package, recs[1].TotalSec)
	}
}

func TestParseBenchSamePackageTwice(t *testing.T) {
	// Two invocations for the same package (e.g. the package and its
	// internal-test variant) must stay separate records.
	one := "commit: go1.26.1\nBenchmarkCompile:p:total 1 1000000000 ns/op 100.00 %\n"
	recs, err := parseBench(strings.NewReader(one + one))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].TotalSec != 1 {
		t.Fatalf("got %+v", recs)
	}
}

func TestParseBenchMalformed(t *testing.T) {
	if _, err := parseBench(strings.NewReader("BenchmarkCompile:p:total 1 xyz\n")); err == nil {
		t.Fatal("expected error")
	}
}

func TestSplitBenchName(t *testing.T) {
	for _, c := range []struct{ in, pkg, phase string }{
		{"example.com/m/p:fe:parse", "example.com/m/p", "fe:parse"},
		{"example.com/m/p:be:compilefuncs", "example.com/m/p", "be:compilefuncs"},
		{"main:total", "main", "total"},
		{"p:odd", "p", "odd"},
	} {
		pkg, phase := splitBenchName(c.in)
		if pkg != c.pkg || phase != c.phase {
			t.Errorf("splitBenchName(%q) = %q, %q", c.in, pkg, phase)
		}
	}
}

func TestParseTest2JSON(t *testing.T) {
	ts, err := parseTest2JSONFile("testdata/test2json.json", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 4 {
		t.Fatalf("got %d packages, want 4", len(ts))
	}
	a := ts[0]
	if a.Package != "example.com/m/pkg/a" || a.Result != "fail" || a.ElapsedSec != 20.6 {
		t.Errorf("pkg a header = %s %s %v", a.Package, a.Result, a.ElapsedSec)
	}
	if !reflect.DeepEqual(a.TopLevel, map[string]int{"pass": 2, "fail": 1, "skip": 1}) {
		t.Errorf("pkg a counts = %v", a.TopLevel)
	}
	if a.Subtests != 2 || a.SumSec != 20.5 || a.Over1s != 2 || a.Top20Pct != 100 {
		t.Errorf("pkg a = %+v", a)
	}
	wantSlow := []TJTest{{"TestSlow", "fail", 18, 1}, {"TestMedium", "pass", 2.5, 1}}
	if !reflect.DeepEqual(a.Slowest, wantSlow) {
		t.Errorf("slowest = %+v", a.Slowest)
	}
	b := ts[1]
	if b.Result != "pass" || b.TopLevel["pass"] != 2 || b.SumSec != 1 {
		t.Errorf("pkg b = %+v", b)
	}
	// Stream ended mid-package: no package result, and the test that was
	// still running is listed as incomplete, timed from its run event to the
	// package's last event (30s later).
	c := ts[2]
	if c.Result != "" || !reflect.DeepEqual(c.TopLevel, map[string]int{"pass": 1, "incomplete": 1}) {
		t.Errorf("pkg c = %+v", c)
	}
	if want := (TJTest{"TestHangs", "incomplete", 30, 1}); len(c.Slowest) == 0 || c.Slowest[0] != want {
		t.Errorf("pkg c slowest = %+v, want first %+v", c.Slowest, want)
	}
	// -count=3: one distinct test, elapsed summed, worst action kept.
	d := ts[3]
	if !reflect.DeepEqual(d.TopLevel, map[string]int{"fail": 1}) {
		t.Errorf("pkg d counts = %v", d.TopLevel)
	}
	if want := (TJTest{"TestRepeated", "fail", 1.5, 3}); len(d.Slowest) != 1 || d.Slowest[0] != want {
		t.Errorf("pkg d slowest = %+v, want %+v", d.Slowest, want)
	}
}

func TestTJTestMergeRanks(t *testing.T) {
	var tt TJTest
	tt.merge("skip", 0)
	tt.merge("pass", 1)
	tt.merge("skip", 0)
	if tt.Action != "pass" || tt.Runs != 3 || tt.Seconds != 1 {
		t.Errorf("got %+v", tt)
	}
	tt.merge("incomplete", 2)
	tt.merge("fail", 0)
	if tt.Action != "incomplete" {
		t.Errorf("incomplete must outrank fail: %+v", tt)
	}
}

func TestParseGoList(t *testing.T) {
	f, err := os.Open("testdata/golist.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := parseGoList(f, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []DepCount{
		{Package: "example.com/m/a", Total: 4, NonStd: 1},
		{Package: "example.com/m/b", Total: 2, NonStd: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseGoListTest(t *testing.T) {
	f, err := os.Open("testdata/golist_test.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := parseGoList(f, true)
	if err != nil {
		t.Fatal(err)
	}
	// a's test closure: dep, testhelp, testing, errors, internal/abi (a
	// itself, a_test and a.test excluded; a [a.test] folds into a). notests
	// has no a.test entry so its build closure is used.
	want := []DepCount{
		{Package: "example.com/m/a", Test: true, Total: 5, NonStd: 2},
		{Package: "example.com/m/notests", Test: true, Total: 2, NonStd: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseGoListMalformed(t *testing.T) {
	if _, err := parseGoList(strings.NewReader("{\"Standard\": true}\n"), false); err == nil {
		t.Fatal("expected error")
	}
}

func TestDepsArgs(t *testing.T) {
	got := depsArgs([]string{"./pkg/a"}, true)
	want := []string{"go", "list", "-deps", "-json=ImportPath,Standard,DepOnly,Deps", "-test", "./pkg/a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestInjectGoFlags(t *testing.T) {
	const hub = "example.com/m/pkg/hub"
	for _, c := range []struct {
		name string
		argv []string
		ij   injection
		want []string
		err  bool
	}{
		{
			name: "actiongraph only",
			argv: []string{"go", "test", "-c", "./pkg/hub"},
			ij:   injection{actiongraph: "/d/ag.json"},
			want: []string{"go", "test", "-debug-actiongraph=/d/ag.json", "-c", "./pkg/hub"},
		},
		{
			name: "bench inherits GOFLAGS gcflags",
			argv: []string{"go", "test", "-c", "-p", "1", "./pkg/hub"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt", goflags: "-mod=mod -gcflags=-c=1"},
			want: []string{"go", "test", "-gcflags=" + hub + "=-c=1 -bench=/d/b.txt", "-c", "-p", "1", "./pkg/hub"},
		},
		{
			name: "bench goes after command-line gcflags and inherits the last match",
			argv: []string{"/usr/local/go/bin/go", "build", "-gcflags=-N", "-gcflags", "other=-l", "-gcflags=all=-c=2", "-o", "x", "./pkg/hub"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt", goflags: "-gcflags=-c=1"},
			want: []string{"/usr/local/go/bin/go", "build", "-gcflags=-N", "-gcflags", "other=-l", "-gcflags=all=-c=2", "-gcflags=" + hub + "=-c=2 -bench=/d/b.txt", "-o", "x", "./pkg/hub"},
		},
		{
			name: "separate-value gcflags matching the bench package",
			argv: []string{"go", "test", "-gcflags", hub + "=-m", "-c", "./pkg/hub"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt"},
			want: []string{"go", "test", "-gcflags", hub + "=-m", "-gcflags=" + hub + "=-m -bench=/d/b.txt", "-c", "./pkg/hub"},
		},
		{
			name: "no inherited flags",
			argv: []string{"go", "test", "-c", "./pkg/hub"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt"},
			want: []string{"go", "test", "-gcflags=" + hub + "=-bench=/d/b.txt", "-c", "./pkg/hub"},
		},
		{
			name: "not a go build",
			argv: []string{"make", "build"},
			ij:   injection{actiongraph: "/d/ag.json"},
			err:  true,
		},
		{
			name: "duplicate actiongraph",
			argv: []string{"go", "build", "-debug-actiongraph=x.json", "./..."},
			ij:   injection{actiongraph: "/d/ag.json"},
			err:  true,
		},
		{
			name: "dangling gcflags",
			argv: []string{"go", "build", "-gcflags"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt"},
			err:  true,
		},
		{
			name: "unpatterned gcflags not inherited when the bench package is only a dependency",
			argv: []string{"go", "test", "-c", "./cmd"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt", goflags: "-gcflags=-c=1"},
			want: []string{"go", "test", "-gcflags=" + hub + "=-bench=/d/b.txt", "-c", "./cmd"},
		},
		{
			name: "unpatterned gcflags inherited when named through a /... pattern",
			argv: []string{"go", "test", "-c", "./pkg/hub/..."},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt", goflags: "-gcflags=-c=1"},
			want: []string{"go", "test", "-gcflags=" + hub + "=-c=1 -bench=/d/b.txt", "-c", "./pkg/hub/..."},
		},
		{
			name: "subpackage named directly",
			argv: []string{"go", "test", "-c", "./pkg/hub/sub"},
			ij:   injection{benchPkg: hub + "/sub", benchFile: "/d/b.txt", goflags: "-gcflags=-c=1"},
			want: []string{"go", "test", "-gcflags=" + hub + "/sub=-c=1 -bench=/d/b.txt", "-c", "./pkg/hub/sub"},
		},
		{
			name: "a flag value is not a package argument",
			argv: []string{"go", "test", "-c", "-o", "./pkg/hub", "./cmd"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt", goflags: "-gcflags=-c=1"},
			want: []string{"go", "test", "-gcflags=" + hub + "=-bench=/d/b.txt", "-c", "-o", "./pkg/hub", "./cmd"},
		},
		{
			name: "arguments after -args are not packages",
			argv: []string{"go", "test", "-c", "./cmd", "-args", "./pkg/hub"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt", goflags: "-gcflags=-c=1"},
			want: []string{"go", "test", "-gcflags=" + hub + "=-bench=/d/b.txt", "-c", "./cmd", "-args", "./pkg/hub"},
		},
		{
			name: "relative patterned gcflags resolve to the bench package",
			argv: []string{"go", "test", "-gcflags=./pkg/hub=-m", "-c", "./pkg/hub"},
			ij:   injection{benchPkg: hub, benchFile: "/d/b.txt"},
			want: []string{"go", "test", "-gcflags=./pkg/hub=-m", "-gcflags=" + hub + "=-m -bench=/d/b.txt", "-c", "./pkg/hub"},
		},
		{
			name: "bench file with a space is rejected",
			argv: []string{"go", "test", "-c", "./pkg/hub"},
			ij:   injection{benchPkg: hub, benchFile: "/my dir/b.txt"},
			err:  true,
		},
		{
			name: "nothing to inject passes any command through",
			argv: []string{"make", "build"},
			want: []string{"make", "build"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.ij.resolve == nil {
				c.ij.resolve = fakeResolve
			}
			got, err := injectGoFlags(c.argv, c.ij)
			if c.err {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got\n %q\nwant\n %q", got, c.want)
			}
		})
	}
}

// fakeResolve stands in for go.mod resolution: module example.com/m.
func fakeResolve(arg string) string {
	if rest, ok := strings.CutPrefix(arg, "./"); ok {
		return "example.com/m/" + rest
	}
	return arg
}

func TestGoPackageArgs(t *testing.T) {
	got := goPackageArgs([]string{"-c", "-p", "1", "-o", "x.test", "-gcflags", "all=-N", "-tags=a,b", "-race", "./a", "./b/...", "-run", "TestX", "-args", "./c"})
	want := []string{"./a", "./b/..."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMatchPattern(t *testing.T) {
	for _, c := range []struct {
		pat, pkg string
		want     bool
	}{
		{"m/p", "m/p", true},
		{"m/p", "m/p/sub", false},
		{"m/p/...", "m/p", true},
		{"m/p/...", "m/p/sub", true},
		{"m/p/...", "m/pq", false},
	} {
		if got := matchPattern(c.pat, c.pkg); got != c.want {
			t.Errorf("matchPattern(%q, %q) = %v", c.pat, c.pkg, got)
		}
	}
}

func TestResolveImportPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg", "hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(root, "pkg"))
	for in, want := range map[string]string{
		"./hub":            "example.com/m/pkg/hub",
		"./hub/...":        "example.com/m/pkg/hub/...",
		".":                "example.com/m/pkg",
		"..":               "example.com/m",
		"example.com/m/x":  "example.com/m/x",
		"github.com/a/b/c": "github.com/a/b/c",
	} {
		if got := resolveImportPath(in); got != want {
			t.Errorf("resolveImportPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"go", "test", "-gcflags=p=-c=1 -bench=/x", "it's", ""})
	want := `go test '-gcflags=p=-c=1 -bench=/x' 'it'\''s' ''`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestNormalize(t *testing.T) {
	recs, err := parseBenchFile("testdata/bench.txt")
	if err != nil {
		t.Fatal(err)
	}
	// projectkeys: 811 lines, 68 funcs, 0.087s; main is smaller.
	n := normalize(gib, recs)
	if n == nil || n.Package != "github.com/GoogleCloudPlatform/scion/pkg/projectkeys" || n.Lines != 811 || n.Funcs != 68 {
		t.Fatalf("normalize = %+v", n)
	}
	if n.PeakRSSGiBPer100kLines != 123.305 { // 1 GiB / 0.00811
		t.Errorf("peak per 100k lines = %v", n.PeakRSSGiBPer100kLines)
	}
	if n.PeakRSSGiBPer10kFuncs != 147.059 { // 1 GiB / 0.0068
		t.Errorf("peak per 10k funcs = %v", n.PeakRSSGiBPer10kFuncs)
	}
	if n.CompileSecPer100kLines != 10.727 || n.CompileSecPer10kFuncs != 12.794 {
		t.Errorf("compile per size = %v, %v", n.CompileSecPer100kLines, n.CompileSecPer10kFuncs)
	}
	if normalize(gib, nil) != nil {
		t.Error("no bench data should give nil")
	}
	if normalize(gib, []BenchRecord{{Package: "p", TotalSec: 1}}) != nil {
		t.Error("no size data should give nil")
	}
	// Unknown peak still yields the compile-time ratios.
	if n := normalize(0, recs); n.PeakRSSGiBPer100kLines != 0 || n.CompileSecPer100kLines == 0 {
		t.Errorf("zero peak = %+v", n)
	}
}

func TestCollectHostFormUnset(t *testing.T) {
	t.Setenv("GOGC", "40")
	t.Setenv("GOMEMLIMIT", "") // registers restore of the original value
	if err := os.Unsetenv("GOMEMLIMIT"); err != nil {
		t.Fatal(err)
	}
	h := collectHost()
	if h.Form.GOGC != "40" || h.Form.GOMEMLIMIT != "unset" {
		t.Errorf("form = %+v", h.Form)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("short: %q", got)
	}
	if got := truncate("abcdefgh", 3); !strings.HasPrefix(got, "abc... (5 more bytes") {
		t.Errorf("long: %q", got)
	}
}

func TestDiffRecords(t *testing.T) {
	old := &Record{
		Kind:   "compile",
		Host:   &HostInfo{NumCPU: 32, Form: RuntimeForm{GOGC: "40", GOMEMLIMIT: "6GiB"}},
		Rusage: &Rusage{WallSec: 458, UserSec: 808, PeakRSSBytes: 11 * gib},
		Bench: []BenchRecord{{Package: "p", TotalSec: 295, Phases: []BenchPhase{
			{Phase: "be:compilefuncs", Seconds: 210, Count: 379120, Unit: "funcs"},
		}}},
		Deps: []DepCount{{Package: "p", Total: 1524, NonStd: 900}},
	}
	nw := &Record{
		Kind:   "compile",
		Host:   &HostInfo{NumCPU: 32, Form: RuntimeForm{GOGC: "40", GOMEMLIMIT: "unset"}},
		Rusage: &Rusage{WallSec: 325, UserSec: 477, PeakRSSBytes: 11 * gib},
		Bench: []BenchRecord{{Package: "p", TotalSec: 200, Phases: []BenchPhase{
			{Phase: "be:compilefuncs", Seconds: 150, Count: 300000, Unit: "funcs"},
		}}},
		Deps: []DepCount{{Package: "p", Total: 1500, NonStd: 880}},
	}
	var buf bytes.Buffer
	if err := diffRecords(&buf, old, nw); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`WARNING: GOMEMLIMIT differs: "6GiB" vs "unset"`,
		"458.0s", "325.0s", "-133.0s", "-29.0%", "wall",
		"compile total p", "be:compilefuncs funcs",
		"1524", "1500", "deps p",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q:\n%s", want, out)
		}
	}
}

func TestRunAnalysisSubcommands(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"actiongraph", "-top", "2", "testdata/actiongraph.json"}, "example.com/m/pkg/big"},
		{[]string{"bench", "testdata/bench.txt"}, "be:compilefuncs"},
		{[]string{"tests", "testdata/test2json.json"}, "TestSlow"},
	} {
		var out, errb bytes.Buffer
		if code := run(c.args, &out, &errb); code != 0 {
			t.Fatalf("%v: exit %d: %s", c.args, code, errb.String())
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("%v: output missing %q:\n%s", c.args, c.want, out.String())
		}
	}
}

func TestRunJSONToStdoutIsPureJSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"tests", "-json", "-", "testdata/test2json.json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var rec Record
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("stdout is not a JSON record: %v\n%s", err, out.String())
	}
	if rec.Schema != schemaVersion || rec.Kind != "tests" || len(rec.Tests) != 4 {
		t.Errorf("record = %+v", rec)
	}
}

func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"actiongraph"},
		{"diff", "one.json"},
		{"run"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code == 0 {
			t.Errorf("%q: exit 0, want non-zero", args)
		}
	}
}

func TestMeasureRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	js := filepath.Join(dir, "rec.json")
	so := filepath.Join(dir, "out.txt")
	var out, errb bytes.Buffer
	code := run([]string{"run", "-label", "x", "-cgroup", "none", "-json", js, "-stdout", so, "--", "sh", "-c", "echo hi; exit 3"}, &out, &errb)
	if code != 3 {
		t.Fatalf("exit %d, want the child's 3; stderr:\n%s", code, errb.String())
	}
	rec, err := readRecord(js)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "run" || rec.Label != "x" || rec.Rusage == nil || rec.Rusage.ExitCode != 3 {
		t.Errorf("record = %+v", rec)
	}
	if rec.Rusage.PeakRSSBytes <= 0 {
		t.Errorf("peak RSS not captured: %+v", rec.Rusage)
	}
	if b, _ := os.ReadFile(so); string(b) != "hi\n" {
		t.Errorf("captured stdout = %q", b)
	}
}

func TestMeasureTestSubcommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses cat")
	}
	dir := t.TempDir()
	js := filepath.Join(dir, "rec.json")
	var out, errb bytes.Buffer
	code := run([]string{"test", "-cgroup", "none", "-json", js, "-out", filepath.Join(dir, "t.json"), "--", "cat", "testdata/test2json.json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	rec, err := readRecord(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Tests) != 4 || rec.Tests[0].Slowest[0].Name != "TestSlow" {
		t.Errorf("tests = %+v", rec.Tests)
	}
}

func TestComparabilityWarnings(t *testing.T) {
	base := func() *Record {
		return &Record{
			Kind: "compile",
			Command: []string{"/usr/local/go/bin/go", "test", "-debug-actiongraph=/a/actiongraph.json",
				"-gcflags=m/p=-c=1 -bench=/a/bench.txt", "-c", "-o", "/a/p.test", "./p"},
			Rusage:     &Rusage{},
			Bench:      []BenchRecord{{Package: "m/p"}, {Package: "main"}},
			Normalized: &Normalized{Package: "m/p"},
			Host:       &HostInfo{GoToolchain: "go1.26.1", Form: RuntimeForm{GOGC: "40"}},
		}
	}
	// Only artifact paths and the go binary's directory differ: no warning.
	same := base()
	same.Command = []string{"go", "test", "-debug-actiongraph=/b/actiongraph.json",
		"-gcflags=m/p=-c=1 -bench=/b/bench.txt", "-c", "-o", "/b/p.test", "./p"}
	if w := comparabilityWarnings(base(), same); len(w) != 0 {
		t.Errorf("unexpected warnings: %q", w)
	}
	for _, c := range []struct {
		name   string
		mutate func(r *Record)
		want   string
	}{
		{"different invocation", func(r *Record) { r.Command[len(r.Command)-1] = "./q" }, "command differs"},
		{"different compiler flags", func(r *Record) { r.Command[3] = "-gcflags=m/p=-c=4 -bench=/b/bench.txt" }, "command differs"},
		{"non-zero exit", func(r *Record) { r.Rusage.ExitCode = 2 }, "new run exited with rc=2"},
		{"normalised package", func(r *Record) { r.Normalized.Package = "m/q" }, "normalised package differs"},
		{"bench package set", func(r *Record) { r.Bench = []BenchRecord{{Package: "m/p"}} }, "compiler -bench packages differ"},
		{"toolchain", func(r *Record) { r.Host.GoToolchain = "go1.27.0" }, "go toolchain differs"},
		{"form", func(r *Record) { r.Host.Form.GOMEMLIMIT = "6GiB" }, "GOMEMLIMIT differs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := base()
			c.mutate(b)
			w := strings.Join(comparabilityWarnings(base(), b), "\n")
			if !strings.Contains(w, c.want) {
				t.Errorf("warnings %q do not contain %q", w, c.want)
			}
		})
	}
	old := base()
	old.Rusage.ExitCode = 1
	if w := strings.Join(comparabilityWarnings(old, base()), "\n"); !strings.Contains(w, "old run exited with rc=1") {
		t.Errorf("old exit not flagged: %q", w)
	}
}

func TestBenchKeysDuplicates(t *testing.T) {
	recs := []BenchRecord{{Package: "m/p", TotalSec: 1}, {Package: "main"}, {Package: "m/p", TotalSec: 2}}
	if got, want := benchKeys(recs), []string{"m/p", "main", "m/p (#2)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %q, want %q", got, want)
	}
	m := benchByKey(recs)
	if m["m/p"].TotalSec != 1 || m["m/p (#2)"].TotalSec != 2 {
		t.Errorf("records overwritten: %+v", m)
	}
	var buf bytes.Buffer
	if err := diffRecords(&buf, &Record{Bench: recs}, &Record{Bench: recs}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "compile total m/p (#2)") {
		t.Errorf("diff does not show the second record:\n%s", buf.String())
	}
}

func TestReadRecordBackfillsForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.json")
	old := `{"schema":1,"kind":"compile","time":"2026-10-10T14:35:00Z",
	  "host":{"goos":"linux","goarch":"amd64","num_cpu":2,"rlimit_as":"16000000 KiB",
	          "env":{"GOMAXPROCS":"2","GOGC":"40","GOFLAGS":"-gcflags=-c=1"},"buildstats_go_version":"go1.26.1"}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := readRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	want := RuntimeForm{GOMAXPROCS: "2", GOGC: "40", GOMEMLIMIT: "unset", GOFLAGS: "-gcflags=-c=1", RlimitAS: "16000000 KiB"}
	if rec.Host.Form != want {
		t.Errorf("form = %+v, want %+v", rec.Host.Form, want)
	}
	// A record that has a form keeps it.
	h := &HostInfo{Form: RuntimeForm{GOGC: "25"}, Env: map[string]string{"GOGC": "40"}}
	backfillForm(h)
	if h.Form.GOGC != "25" {
		t.Errorf("existing form overwritten: %+v", h.Form)
	}
	backfillForm(nil) // must not panic
}

func TestToolchain(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.99.9\ntime 2026-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOROOT", root)
	var h HostInfo
	h.setToolchain("go")
	if h.GoToolchain != "go1.99.9" || h.GoToolchainSource != "GOROOT/VERSION" {
		t.Errorf("toolchain = %q from %q", h.GoToolchain, h.GoToolchainSource)
	}
	if v := benchGoVersion("testdata/bench.txt"); v != "go1.26.1" {
		t.Errorf("benchGoVersion = %q", v)
	}
	if v := benchGoVersion(filepath.Join(root, "missing")); v != "" {
		t.Errorf("benchGoVersion(missing) = %q", v)
	}
	if got := goBinary([]string{"/opt/go/bin/go", "build"}); got != "/opt/go/bin/go" {
		t.Errorf("goBinary = %q", got)
	}
	if got := goBinary([]string{"npx", "vitest"}); got != "go" {
		t.Errorf("goBinary = %q", got)
	}
}

func TestCompileRejectsDirWithSpace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "has space")
	var out, errb bytes.Buffer
	code := run([]string{"compile", "-cgroup", "none", "-dir", dir, "-bench-pkg", "example.com/m/p", "--", "go", "test", "-c", "./p"}, &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "whitespace") {
		t.Errorf("exit %d, stderr %q; want a whitespace error before go runs", code, errb.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("rejected -dir was created (stat err %v)", err)
	}
}

// TestPeakCoversGrandchild checks that the wait4 peak includes a process two
// levels below buildstats, as the compiler is below go.
func TestPeakCoversGrandchild(t *testing.T) {
	for _, tool := range []string{"sh", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	// awk doubles a string to 2^26 bytes (64 MiB) inside sh inside sh.
	const minPeak = 64 << 20
	// The trailing ":" at each level stops a shell from exec'ing its last
	// command in place, so awk really is a grandchild of the measured child.
	script := `sh -c 'awk "BEGIN{s=\"x\"; for(i=0;i<26;i++) s=s s; print length(s)}"; :'; :`
	ru, err := measure([]string{"sh", "-c", script}, execOpts{stdout: filepath.Join(t.TempDir(), "out")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if ru.ExitCode != 0 {
		t.Fatalf("helper exited %d", ru.ExitCode)
	}
	if ru.PeakRSSBytes < minPeak {
		t.Errorf("peak RSS %d < %d: grandchild not covered", ru.PeakRSSBytes, minPeak)
	}
}

func TestAttachBench(t *testing.T) {
	rec := &Record{Rusage: &Rusage{PeakRSSBytes: gib}, Host: &HostInfo{GoToolchain: "go0.0", GoToolchainSource: "GOROOT/VERSION"}}
	if err := attachBench(rec, "testdata/bench.txt"); err != nil {
		t.Fatal(err)
	}
	if len(rec.Bench) != 2 || rec.Normalized == nil || rec.Artifacts["bench"] != "testdata/bench.txt" {
		t.Errorf("record = %+v", rec)
	}
	if rec.Host.GoToolchain != "go1.26.1" || rec.Host.GoToolchainSource != "bench" {
		t.Errorf("toolchain = %q from %q", rec.Host.GoToolchain, rec.Host.GoToolchainSource)
	}
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := attachBench(&Record{}, empty); err == nil {
		t.Error("empty bench file: expected error")
	}
	if err := attachBench(&Record{}, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing bench file: expected error")
	}
}

func TestDiffPhaseRowsNamedByBenchKey(t *testing.T) {
	recs := []BenchRecord{
		{Package: "m/p", TotalSec: 2, Phases: []BenchPhase{{Phase: "be:compilefuncs", Seconds: 1}}},
		{Package: "main", TotalSec: 1, Phases: []BenchPhase{{Phase: "be:compilefuncs", Seconds: 0.5}}},
	}
	var buf bytes.Buffer
	if err := diffRecords(&buf, &Record{Bench: recs}, &Record{Bench: recs}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"m/p be:compilefuncs", "main be:compilefuncs"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("diff missing %q:\n%s", want, buf.String())
		}
	}
}

func TestBenchSeveralFiles(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"bench", "-json", "-", "testdata/bench.txt", "testdata/bench.txt"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var rec Record
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Bench) != 4 || rec.Artifacts["bench"] == "" || rec.Artifacts["bench.2"] == "" {
		t.Errorf("record = %+v", rec)
	}
}

// failWriter fails every write, like a closed stderr pipe without SIGPIPE.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestFinishWritesJSONBeforeTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.json")
	rec := &Record{Schema: schemaVersion, Kind: "run", Label: "x", Rusage: &Rusage{}}
	if err := finish(rec, path, io.Discard, failWriter{}); err == nil {
		t.Error("expected the table write error to be reported")
	}
	if got, err := readRecord(path); err != nil || got.Label != "x" {
		t.Errorf("record not written: %+v, %v", got, err)
	}
}
