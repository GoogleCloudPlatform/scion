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
	"strings"
	"testing"
)

const exportHelpers = hdr + `package hub

import (
	"sync/atomic"
	"testing"
)

// tid maps a name to an ID.
func tid(name string) string { return "id-" + name }

// mustHelp is a helper that marks itself.
func mustHelp(t testing.TB, s string) string {
	t.Helper()
	return tid(s)
}

// running is a state holder.
var running atomic.Bool

var keys = []string{"a", "b"}

const (
	capA = 1
	capB = "x"
)

// res is a result.
type res struct {
	total int
	other int
}

func scan() res { return res{total: 1} }

// Sum adds.
func (r *res) Sum() int { return r.total + r.other }

func onlyMoved() int { return capA }

func variadic(_ int, rest ...string) (n int) { return len(rest) + onlyMoved() }

func first[T any](xs []T) T { return xs[0] }

func use() { running.Store(true) }

func pair(a, b string) string { return a + b }
`

const exportUser = hdr + `package hub

import "testing"

func TestUser(t *testing.T) {
	_ = tid("a")
	_ = mustHelp(t, "b")
	if !running.Load() {
		t.Log(capB)
	}
	for _, k := range keys {
		_ = k
	}
	n := scan().total
	var r res
	_ = r.Sum() + n + variadic(1, "x") + first([]int{n})
	use()
	_ = pair("a", "b")
}
`

const exportStay = hdr + `package hub

// stays uses a production type, so it cannot move.
func stays() *Server { return &Server{} }
`

const exportProd = hdr + `package hub

// Server is production code.
type Server struct{}
`

func exportFixture(t *testing.T) string {
	t.Helper()
	return writePkg(t, map[string]string{
		"a_helpers_test.go":    exportHelpers,
		"user_test.go":         exportUser,
		"stay_helpers_test.go": exportStay,
		"server.go":            exportProd,
	})
}

func exportSpecOf(names ...string) exportSpec {
	return exportSpec{
		names:   names,
		renames: map[string]string{"tid": "TID"},
		fields:  map[string]map[string]string{"res": {"total": "Total"}},
	}
}

var allExportNames = []string{"tid", "mustHelp", "running", "keys", "capA", "capB", "res", "scan", "onlyMoved", "variadic", "first", "use", "pair"}

func runExportOn(t *testing.T, dir, target string, spec exportSpec) *exportPlan {
	t.Helper()
	p, err := loadPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := planExport(p, spec, target, "example.com/m/support", defaultAliasFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := ep.write(target); err != nil {
		t.Fatal(err)
	}
	return ep
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExportMovesAndAliases(t *testing.T) {
	dir := exportFixture(t)
	before := copyDir(t, dir)
	target := filepath.Join(t.TempDir(), "support")
	ep := runExportOn(t, dir, target, exportSpecOf(allExportNames...))

	if _, err := os.Stat(filepath.Join(dir, "a_helpers_test.go")); !os.IsNotExist(err) {
		t.Errorf("emptied helper file should be deleted, stat err = %v", err)
	}
	got := readFile(t, filepath.Join(target, "a.go"))
	for _, want := range []string{
		"package support",
		"// tid maps a name to an ID.\nfunc TID(name string) string { return \"id-\" + name }",
		"func MustHelp(t testing.TB, s string) string {\n\tt.Helper()\n\treturn TID(s)\n}",
		"var Running atomic.Bool",
		"var Keys = []string{\"a\", \"b\"}",
		"type Res struct {\n\tTotal int\n\tother int\n}",
		"func Scan() Res { return Res{Total: 1} }",
		"func (r *Res) Sum() int { return r.Total + r.other }",
		"func onlyMoved() int { return capA }",
		"func Use() { Running.Store(true) }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("target a.go lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OnlyMoved") {
		t.Errorf("a name only moved code uses must stay unexported:\n%s", got)
	}

	alias := readFile(t, filepath.Join(dir, defaultAliasFile))
	for _, want := range []string{
		"\"example.com/m/support\"",
		"type res = support.Res",
		"const capB = support.CapB",
		"var keys = support.Keys",
		"var running = &support.Running",
		"func tid(name string) string {\n\treturn support.TID(name)\n}",
		"func mustHelp(t testing.TB, s string) string {\n\tt.Helper()\n\treturn support.MustHelp(t, s)\n}",
		"func variadic(p0 int, rest ...string) (n int) {\n\treturn support.Variadic(p0, rest...)\n}",
		"func pair(a, b string) string {\n\treturn support.Pair(a, b)\n}",
		"func first[T any](xs []T) T {\n\treturn support.First[T](xs)\n}",
		"func use() {\n\tsupport.Use()\n}",
	} {
		if !strings.Contains(alias, want) {
			t.Errorf("alias file lacks %q:\n%s", want, alias)
		}
	}
	if !strings.Contains(alias, "\"testing\"") {
		t.Errorf("alias file must import testing for mustHelp's signature:\n%s", alias)
	}
	// capA is used only by moved code: exported (renamed) but not aliased? It
	// is used by onlyMoved, which stays unexported, so capA needs no alias.
	if strings.Contains(alias, "capA") {
		t.Errorf("capA has no staying user and must not be aliased:\n%s", alias)
	}
	if user := readFile(t, filepath.Join(dir, "user_test.go")); !strings.Contains(user, "n := scan().Total") {
		t.Errorf("field call site not rewritten:\n%s", user)
	}
	if len(ep.fieldSites) != 1 {
		t.Errorf("field sites = %v, want 1", ep.fieldSites)
	}

	pb, _ := loadPackage(before)
	pa, _ := loadPackage(dir)
	r, err := verifyExport(pb, pa, target, "example.com/m/support", exportSpecOf(allExportNames...), defaultAliasFile)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r.write(&out)
	if !r.ok() {
		t.Fatalf("verify-export failed:\n%s", out.String())
	}
}

func TestExportIsDeterministic(t *testing.T) {
	a, b := exportFixture(t), exportFixture(t)
	ta, tb := filepath.Join(t.TempDir(), "support"), filepath.Join(t.TempDir(), "support")
	runExportOn(t, a, ta, exportSpecOf(allExportNames...))
	runExportOn(t, b, tb, exportSpecOf(allExportNames...))
	for _, f := range []string{defaultAliasFile, "user_test.go"} {
		if readFile(t, filepath.Join(a, f)) != readFile(t, filepath.Join(b, f)) {
			t.Errorf("%s differs between two runs", f)
		}
	}
	if readFile(t, filepath.Join(ta, "a.go")) != readFile(t, filepath.Join(tb, "a.go")) {
		t.Error("target differs between two runs")
	}
}

func TestVerifyExportCatchesEdits(t *testing.T) {
	for name, tamper := range map[string]func(dir, target string){
		"moved body changed": func(_, target string) {
			p := filepath.Join(target, "a.go")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, bytes.Replace(b, []byte(`"id-"`), []byte(`"ID-"`), 1), 0o644)
		},
		"moved comment changed": func(_, target string) {
			p := filepath.Join(target, "a.go")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, bytes.Replace(b, []byte("// res is a result."), []byte("// res is the result."), 1), 0o644)
		},
		"staying test changed": func(dir, _ string) {
			p := filepath.Join(dir, "user_test.go")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, bytes.Replace(b, []byte(`tid("a")`), []byte(`tid("b")`), 1), 0o644)
		},
		"alias changed": func(dir, _ string) {
			p := filepath.Join(dir, defaultAliasFile)
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, bytes.Replace(b, []byte("&support.Running"), []byte("support.Running"), 1), 0o644)
		},
		"extra target decl": func(_, target string) {
			p := filepath.Join(target, "a.go")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, append(b, []byte("\nfunc extra() {}\n")...), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := exportFixture(t)
			before := copyDir(t, dir)
			target := filepath.Join(t.TempDir(), "support")
			runExportOn(t, dir, target, exportSpecOf(allExportNames...))
			tamper(dir, target)
			pb, _ := loadPackage(before)
			pa, _ := loadPackage(dir)
			r, err := verifyExport(pb, pa, target, "example.com/m/support", exportSpecOf(allExportNames...), defaultAliasFile)
			if err != nil {
				t.Fatal(err)
			}
			if r.ok() {
				var out bytes.Buffer
				r.write(&out)
				t.Fatalf("verify-export passed a tampered tree:\n%s", out.String())
			}
		})
	}
}

func TestTokenDiff(t *testing.T) {
	m := map[string]string{"tid": "TID"}
	if d := tokenDiff("func tid() {}\n", "func TID() {}", m); d != "" {
		t.Errorf("rename should be allowed: %s", d)
	}
	if d := tokenDiff("func tid() {}", "func Tid() {}", m); d == "" {
		t.Error("a rename outside the map must fail")
	}
	if d := tokenDiff("func f() { a := tid }", "func f() { a := TID }", map[string]string{}); d == "" {
		t.Error("an empty map must allow no rename")
	}
	if d := tokenDiff("// a\nfunc f() {}", "// b\nfunc f() {}", m); d == "" {
		t.Error("comments must be compared")
	}
}

func TestExportRefusals(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		names []string
		want  string
	}{
		"uses staying decl": {
			files: map[string]string{"a_helpers_test.go": exportStay, "server.go": exportProd},
			names: []string{"stays"},
			want:  "which stays",
		},
		"test entry point": {
			files: map[string]string{"a_helpers_test.go": hdr + "package hub\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"},
			names: []string{"TestX"},
			want:  "test entry points never move",
		},
		"not a helper file": {
			files: map[string]string{"a_test.go": hdr + "package hub\n\nfunc h() {}\n"},
			names: []string{"h"},
			want:  "not in a _helpers_test.go file",
		},
		"partial group": {
			files: map[string]string{"a_helpers_test.go": hdr + "package hub\n\nconst (\n\tx = 1\n\ty = 2\n)\n"},
			names: []string{"x"},
			want:  "shares a declaration group",
		},
		"assigned var": {
			files: map[string]string{
				"a_helpers_test.go": hdr + "package hub\n\nvar names = []string{\"a\"}\n",
				"b_test.go":         hdr + "package hub\n\nfunc reset() { names = nil }\n",
			},
			names: []string{"names"},
			want:  "is assigned",
		},
		"address of var": {
			files: map[string]string{
				"a_helpers_test.go": hdr + "package hub\n\nvar names = []string{\"a\"}\n",
				"b_test.go":         hdr + "package hub\n\nvar p = &names\n",
			},
			names: []string{"names"},
			want:  "is assigned",
		},
		"opaque var": {
			files: map[string]string{
				"a_helpers_test.go": hdr + "package hub\n\nvar v = mk()\n\nfunc mk() int { return 1 }\n",
				"b_test.go":         hdr + "package hub\n\nvar w = v\n",
			},
			names: []string{"v", "mk"},
			want:  "cannot tell whether a copy behaves",
		},
		"constrained dependency": {
			files: map[string]string{
				"a_helpers_test.go": hdr + "package hub\n\nfunc h() int { return k }\n",
				"k_test.go":         "//go:build linux\n\npackage hub\n\nconst k = 1\n",
			},
			names: []string{"h"},
			want:  "which stays",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writePkg(t, c.files)
			p, err := loadPackage(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, err = planExport(p, exportSpec{names: c.names}, filepath.Join(t.TempDir(), "support"), "example.com/m/support", defaultAliasFile)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestExportWidensConstrainedHelper(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a_helpers_test.go": hdr + "//go:build !no_sqlite\n\npackage hub\n\nimport \"sync/atomic\"\n\ntype sw struct{ armed atomic.Bool }\n\nfunc (s *sw) Arm() { s.armed.Store(true) }\n\nfunc keep() {}\n",
		"b_test.go":         hdr + "//go:build !no_sqlite\n\npackage hub\n\nfunc use() { (&sw{}).Arm(); keep() }\n",
		"c.go":              hdr + "package hub\n\nimport \"sync/atomic\"\n\nvar _ atomic.Bool\n",
	})
	target := filepath.Join(t.TempDir(), "support")
	runExportOn(t, dir, target, exportSpec{names: []string{"sw"}})
	got := readFile(t, filepath.Join(target, "a.go"))
	if strings.Contains(got, "go:build") {
		t.Errorf("target files are unconstrained:\n%s", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "a_helpers_test.go")), "func keep() {}") {
		t.Error("the staying helper must remain in its file")
	}
}

func TestReadExportSpec(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(p, []byte("# c\nrename tid=TID\nfield res.total=Total\n\ntid\nres # trailing\n"), 0o644)
	s, err := readExportSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.names, ",") != "tid,res" || s.renames["tid"] != "TID" || s.fields["res"]["total"] != "Total" {
		t.Fatalf("spec = %+v", s)
	}
	for _, bad := range []string{"rename tid=tid2\nx\n", "field res=Total\nx\n", "\n", "a b\n"} {
		_ = os.WriteFile(p, []byte(bad), 0o644)
		if _, err := readExportSpec(p); err == nil {
			t.Errorf("readExportSpec(%q) succeeded", bad)
		}
	}
}
