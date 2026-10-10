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
	"os"
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
	if len(ts) != 3 {
		t.Fatalf("got %d packages, want 3", len(ts))
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
	wantSlow := []TJTest{{"TestSlow", "fail", 18}, {"TestMedium", "pass", 2.5}}
	if !reflect.DeepEqual(a.Slowest, wantSlow) {
		t.Errorf("slowest = %+v", a.Slowest)
	}
	b := ts[1]
	if b.Result != "pass" || b.TopLevel["pass"] != 2 || b.SumSec != 1 {
		t.Errorf("pkg b = %+v", b)
	}
	// Stream ended mid-package: no package result, and the running test
	// is not counted.
	c := ts[2]
	if c.Result != "" || c.TopLevel["pass"] != 1 || len(c.Slowest) != 1 {
		t.Errorf("pkg c = %+v", c)
	}
}

func TestParseGoList(t *testing.T) {
	f, err := os.Open("testdata/golist.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
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
	defer f.Close()
	got, err := parseGoList(f, true)
	if err != nil {
		t.Fatal(err)
	}
	// a's test closure: a_test, dep, testhelp, testing, errors, internal/abi
	// (a itself and a.test excluded; a [a.test] folds into a). notests has
	// no a.test entry so its build closure is used.
	want := []DepCount{
		{Package: "example.com/m/a", Test: true, Total: 6, NonStd: 3},
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
			name: "nothing to inject passes any command through",
			argv: []string{"make", "build"},
			want: []string{"make", "build"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
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

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"go", "test", "-gcflags=p=-c=1 -bench=/x", "it's", ""})
	want := `go test '-gcflags=p=-c=1 -bench=/x' 'it'\''s' ''`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestDiffRecords(t *testing.T) {
	old := &Record{
		Kind:   "compile",
		Host:   &HostInfo{NumCPU: 32, Env: map[string]string{"GOGC": "40", "GOMEMLIMIT": "6GiB"}},
		Rusage: &Rusage{WallSec: 458, UserSec: 808, PeakRSSBytes: 11 * gib},
		Bench: []BenchRecord{{Package: "p", TotalSec: 295, Phases: []BenchPhase{
			{Phase: "be:compilefuncs", Seconds: 210, Count: 379120, Unit: "funcs"},
		}}},
		Deps: []DepCount{{Package: "p", Total: 1524, NonStd: 900}},
	}
	nw := &Record{
		Kind:   "compile",
		Host:   &HostInfo{NumCPU: 32, Env: map[string]string{"GOGC": "40"}},
		Rusage: &Rusage{WallSec: 325, UserSec: 477, PeakRSSBytes: 11 * gib},
		Bench: []BenchRecord{{Package: "p", TotalSec: 200, Phases: []BenchPhase{
			{Phase: "be:compilefuncs", Seconds: 150, Count: 300000, Unit: "funcs"},
		}}},
		Deps: []DepCount{{Package: "p", Total: 1500, NonStd: 880}},
	}
	var buf bytes.Buffer
	diffRecords(&buf, old, nw)
	out := buf.String()
	for _, want := range []string{
		`WARNING: GOMEMLIMIT differs: "6GiB" vs ""`,
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
	if rec.Schema != schemaVersion || rec.Kind != "tests" || len(rec.Tests) != 3 {
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
	if len(rec.Tests) != 3 || rec.Tests[0].Slowest[0].Name != "TestSlow" {
		t.Errorf("tests = %+v", rec.Tests)
	}
}
