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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hdr = "// header\n\n"

func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func run(t *testing.T, dir string, fam family) *result {
	t.Helper()
	p, err := loadPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan(p, fam)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(p, res); err != nil {
		t.Fatal(err)
	}
	return res
}

func planErr(t *testing.T, dir string, fam family) error {
	t.Helper()
	p, err := loadPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = plan(p, fam)
	return err
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustVerify(t *testing.T, before, after string) *verifyReport {
	t.Helper()
	pb, err := loadPackage(before)
	if err != nil {
		t.Fatal(err)
	}
	pa, err := loadPackage(after)
	if err != nil {
		t.Fatal(err)
	}
	r := verify(pb, pa)
	if !r.ok() {
		var sb strings.Builder
		r.write(&sb)
		t.Fatalf("verify failed:\n%s", sb.String())
	}
	return r
}

var basePkg = map[string]string{
	"code.go": hdr + "package p\n\ntype Resp struct{ IDs []string }\n",
	"a_test.go": hdr + `package p

import (
	"strings"
	"testing"
)

// helper is shared.
func helper(s string) string { return strings.ToUpper(inner(s)) }

// inner is used only by helper.
func inner(s string) string { return s + shared }

// shared is also used by TestA.
const shared = "x"

func TestA(t *testing.T) {
	if helper("a") != "AX" || shared != "x" {
		t.Fatal()
	}
}
`,
	"b_test.go": hdr + `package p

import "testing"

func TestB(t *testing.T) { _ = helper("b") }
`,
	"only_test.go": hdr + `package p

// lonely is the only declaration here.
func lonely() int { return 1 }
`,
}

func TestMoveBasic(t *testing.T) {
	dir := writePkg(t, basePkg)
	before := copyDir(t, dir)
	res := run(t, dir, family{dest: "x_helpers_test.go", names: []string{"helper", "lonely"}})

	var got []string
	for _, m := range res.moves {
		got = append(got, m.d.key+"/"+m.reason)
	}
	want := "helper/requested lonely/requested inner/dependency of helper"
	if strings.Join(got, " ") != want {
		t.Fatalf("moves = %q, want %q", strings.Join(got, " "), want)
	}

	dest := read(t, dir, "x_helpers_test.go")
	for _, s := range []string{"import (\n\t\"strings\"\n)", "// helper is shared.\nfunc helper", "// inner is used only by helper.\nfunc inner", "func lonely"} {
		if !strings.Contains(dest, s) {
			t.Errorf("dest lacks %q:\n%s", s, dest)
		}
	}
	a := read(t, dir, "a_test.go")
	if strings.Contains(a, "strings") || strings.Contains(a, "func helper") || strings.Contains(a, "func inner") {
		t.Errorf("a_test.go still has moved code or unused import:\n%s", a)
	}
	if !strings.Contains(a, "const shared") {
		t.Errorf("shared (used elsewhere) must stay:\n%s", a)
	}
	if _, err := os.Stat(filepath.Join(dir, "only_test.go")); !os.IsNotExist(err) {
		t.Errorf("only_test.go should be deleted, stat err=%v", err)
	}
	r := mustVerify(t, before, dir)
	if len(r.moved) != 3 {
		t.Errorf("verify moved = %v", r.moved)
	}

	// Re-running is a no-op.
	res = run(t, dir, family{dest: "x_helpers_test.go", names: []string{"helper", "lonely"}})
	if len(res.moves) != 0 || len(res.changed) != 0 {
		t.Errorf("rerun moved %d, changed %d", len(res.moves), len(res.changed))
	}

	// Adding a name to an existing dest keeps its declarations verbatim.
	snap := copyDir(t, dir)
	run(t, dir, family{dest: "x_helpers_test.go", names: []string{"helper", "lonely", "shared"}})
	mustVerify(t, snap, dir)
	if !strings.Contains(read(t, dir, "x_helpers_test.go"), "const shared") {
		t.Error("shared not appended to existing dest")
	}
}

func TestMoveTypeBringsMethods(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a_test.go": hdr + "package p\n\ntype fake struct{}\n\nfunc (fake) Get() int { return 1 }\n\nfunc (f *fake) Set() {}\n",
		"b_test.go": hdr + "package p\n\nvar _ = fake{}.Get\n",
	})
	before := copyDir(t, dir)
	res := run(t, dir, family{dest: "f_helpers_test.go", names: []string{"fake"}})
	if len(res.moves) != 3 {
		t.Fatalf("moves = %d, want type + 2 methods", len(res.moves))
	}
	mustVerify(t, before, dir)
}

func TestMoveRejects(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		fam   family
		want  string
	}{
		{
			name:  "bad dest name",
			files: basePkg,
			fam:   family{dest: "x_test.go", names: []string{"helper"}},
			want:  "_helpers_test.go",
		},
		{
			name:  "unknown name",
			files: basePkg,
			fam:   family{dest: "x_helpers_test.go", names: []string{"nope"}},
			want:  "declared 0 times",
		},
		{
			name: "non-test file",
			files: map[string]string{
				"code.go": hdr + "package p\n\nfunc prod() {}\n",
			},
			fam:  family{dest: "x_helpers_test.go", names: []string{"prod"}},
			want: "non-test file",
		},
		{
			name: "partial group",
			files: map[string]string{
				"a_test.go": hdr + "package p\n\nvar (\n\ta = 1\n\tb = 2\n)\n",
			},
			fam:  family{dest: "x_helpers_test.go", names: []string{"a"}},
			want: "declaration group",
		},
		{
			name: "uses constrained decl",
			files: map[string]string{
				"a_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nfunc h() int { return sq() }\n",
				"s_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nfunc sq() int { return 1 }\n\nfunc other() int { return sq() }\n",
			},
			fam:  family{dest: "x_helpers_test.go", names: []string{"h"}},
			want: "uses sq",
		},
		{
			name: "new import in unconstrained build",
			files: map[string]string{
				"a_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nimport \"strings\"\n\nfunc h() string { return strings.ToUpper(\"a\") }\n",
			},
			fam:  family{dest: "x_helpers_test.go", names: []string{"h"}},
			want: "would add import",
		},
		{
			name: "not gofmt-clean",
			files: map[string]string{
				"a_test.go": hdr + "package p\n\nfunc h()  {}\n\nfunc k() {}\n",
			},
			fam:  family{dest: "x_helpers_test.go", names: []string{"h"}},
			want: "gofmt-clean",
		},
		{
			name: "unguessable import name",
			files: map[string]string{
				"a_test.go": hdr + "package p\n\nimport \"example.com/weird-lib\"\n\nfunc h() {}\n\nvar _ = lib.X\n",
			},
			fam:  family{dest: "x_helpers_test.go", names: []string{"h"}},
			want: "cannot tell the package name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := planErr(t, writePkg(t, tc.files), tc.fam)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestMoveConstrainedSourceSameDest(t *testing.T) {
	// A dest with the same constraint as the source is fine.
	dir := writePkg(t, map[string]string{
		"a_test.go":         hdr + "//go:build !no_sqlite\n\npackage p\n\nfunc h() int { return 1 }\n\nfunc k() int { return h() }\n",
		"x_helpers_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nfunc existing() {}\n",
	})
	before := copyDir(t, dir)
	run(t, dir, family{dest: "x_helpers_test.go", names: []string{"h"}})
	mustVerify(t, before, dir)
	if !strings.HasPrefix(strings.SplitN(read(t, dir, "x_helpers_test.go"), "\n\n", 3)[1], "//go:build !no_sqlite") {
		t.Errorf("dest constraint lost:\n%s", read(t, dir, "x_helpers_test.go"))
	}
}

func TestVerifyDetectsChanges(t *testing.T) {
	before := writePkg(t, basePkg)
	after := copyDir(t, before)
	a := strings.Replace(read(t, after, "a_test.go"), `"AX"`, `"AY"`, 1)
	a = strings.Replace(a, "func TestA", "func TestRenamed", 1)
	if err := os.WriteFile(filepath.Join(after, "a_test.go"), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	pb, _ := loadPackage(before)
	pa, _ := loadPackage(after)
	r := verify(pb, pa)
	if r.ok() || len(r.testDiffs) != 2 || len(r.missing) != 1 || len(r.added) != 1 {
		t.Fatalf("verify did not flag the rename: %+v", r)
	}

	// A text change under the same name.
	after2 := copyDir(t, before)
	b := strings.Replace(read(t, after2, "b_test.go"), `helper("b")`, `helper("c")`, 1)
	if err := os.WriteFile(filepath.Join(after2, "b_test.go"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	pa2, _ := loadPackage(after2)
	if r := verify(pb, pa2); r.ok() || len(r.textDiffs) != 1 {
		t.Fatalf("verify did not flag the text change: %+v", r)
	}
}

func TestReadFamily(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("# c\ndest a_helpers_test.go\n\nfoo # trailing\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fam, err := readFamily(p)
	if err != nil {
		t.Fatal(err)
	}
	if fam.dest != "a_helpers_test.go" || strings.Join(fam.names, ",") != "foo,bar" {
		t.Fatalf("got %+v", fam)
	}
}

func TestIDsFamilyFileParses(t *testing.T) {
	fam, err := readFamily("families/ids.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fam.dest != "id_helpers_test.go" || len(fam.names) == 0 {
		t.Fatalf("got %+v", fam)
	}
}
