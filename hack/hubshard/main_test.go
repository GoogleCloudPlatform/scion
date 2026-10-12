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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const header = "// Copyright 2026 Google LLC\n\npackage hub\n\n"

const license = "// Copyright 2026 Google LLC\n//\n// Licensed under the Apache License, Version 2.0.\n\n"

var fixture = map[string]string{
	// TestMain and the helper it needs: common.
	"main_test.go":         header + "import \"testing\"\n\nfunc TestMain(m *testing.M) { setup(); m.Run() }\n",
	"setup_helper_test.go": header + "func setup() {}\n",
	// helpers_test.go (helperA) is used by a and b. Their group is larger
	// than total/maxGroupDiv lines, so the size cut moves helpers_test.go,
	// the member used by the most files per line, to common.
	"helpers_test.go": header + "func helperA() int { return 1 }\n",
	"a_test.go":       header + "import \"testing\"\n\nfunc TestA(t *testing.T) { _ = helperA() }\n",
	"b_test.go":       "//go:build !no_sqlite\n\n" + header + "import \"testing\"\n\nfunc TestB(t *testing.T) { _ = helperA() }\n",
	// c uses a fake type declared in d, d adds no method: c and d form a group.
	"c_test.go": header + "import \"testing\"\n\nfunc TestC(t *testing.T) { var f fakeD; f.Do() }\n",
	"d_test.go": header + "type fakeD struct{}\n\nfunc (fakeD) Do() {}\n",
	// Same name as a local in e: a local must not create a dependency.
	"e_test.go": header + "import \"testing\"\n\nfunc TestE(t *testing.T) { helperF := 1; _ = helperF }\n",
	"f_test.go": header + "import \"testing\"\n\nfunc helperF() {}\n\nfunc TestF(t *testing.T) { helperF() }\n",
	// Existing OR constraint must be parenthesised when combined.
	"g_test.go": "// Copyright 2026 Google LLC\n\n//go:build !unix || openbsd\n\npackage hub\n",
	// An inert init does not make a file common.
	"h_test.go": header + "import \"io\"\n\nfunc init() { _ = io.Discard }\n",
	// A /* */ license block before the build line: the existing line is
	// extended, not duplicated.
	"i_test.go": "/*\n * Copyright 2026 Google LLC\n */\n\n//go:build !no_sqlite\n\npackage hub\n",
	"j_test.go": "/* Copyright 2026 Google LLC */\n//go:build linux\n\npackage hub\n",
	// Code after a closing */ ends the header: a build line is added on top.
	"k_test.go": "/* Copyright 2026 Google LLC */ package hub\n",
	// Several blocks, one closing and reopening on the same line.
	"l_test.go": "/* a */ /* b\n */ /* c */\n//go:build linux\n\npackage hub\n",
	// A license header with an empty // line: the shard clause replaces it,
	// so the line count (and every line number) stays the same.
	"m_test.go": license + "package hub\n\nimport \"testing\"\n\nfunc TestM(t *testing.T) {}\n",
	// A *_helpers_test.go file is common even when no test file uses it.
	"unused_helpers_test.go": header + "func unusedHelper() {}\n",
}

func writeFixture(t *testing.T) (root, dir string) {
	t.Helper()
	return writeFiles(t, fixture)
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// baseArgs points the tool at a fixture tree.
func baseArgs(root, dir string, extra ...string) []string {
	return append([]string{"-root", root, "-dir", dir}, extra...)
}

// overlayFiles runs -overlay and returns the overlay's files by base name
// (overlay.json excluded) and the parsed Replace map.
func overlayFiles(t *testing.T, args []string) (map[string]string, map[string]string) {
	t.Helper()
	out := t.TempDir()
	if err := run(append(args, "-overlay", out), io.Discard); err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Replace map[string]string }
	if err := json.Unmarshal([]byte(read(t, out, "overlay.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for from, to := range cfg.Replace {
		if filepath.Dir(to) != out {
			t.Errorf("overlay target %s outside %s", to, out)
		}
		if filepath.Base(from) != filepath.Base(to) {
			t.Errorf("overlay maps %s to %s", from, to)
		}
		files[filepath.Base(from)] = read(t, out, filepath.Base(to))
	}
	return files, cfg.Replace
}

// shardOfOverlay returns the shard of each tagged file in the overlay.
func shardOfOverlay(t *testing.T, files map[string]string) map[string]int {
	t.Helper()
	as := map[string]int{}
	for name, src := range files {
		_, e, err := headerBuildLine([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if e != nil {
			as[name] = shardOf(e)
		}
	}
	return as
}

func TestOverlay(t *testing.T) {
	root, dir := writeFixture(t)
	files, replace := overlayFiles(t, baseArgs(root, dir, "-n", "2"))

	// The tree is untouched.
	for name, src := range fixture {
		if got := read(t, dir, name); got != src {
			t.Errorf("%s changed in the tree:\n%s", name, got)
		}
	}
	for from := range replace {
		if !filepath.IsAbs(from) || filepath.Dir(from) != dir {
			t.Errorf("overlay key %s: want an absolute path in %s", from, dir)
		}
	}
	for _, common := range []string{"main_test.go", "setup_helper_test.go", "helpers_test.go", "unused_helpers_test.go"} {
		if _, ok := files[common]; ok {
			t.Errorf("%s: common file in the overlay", common)
		}
	}
	as := shardOfOverlay(t, files)
	for _, sharded := range []string{"a_test.go", "b_test.go", "c_test.go", "d_test.go", "e_test.go", "f_test.go", "g_test.go", "h_test.go", "i_test.go", "j_test.go", "k_test.go", "l_test.go", "m_test.go"} {
		if as[sharded] < 1 || as[sharded] > 2 {
			t.Errorf("%s: want shard 1 or 2, got %d", sharded, as[sharded])
		}
	}
	if as["c_test.go"] != as["d_test.go"] {
		t.Errorf("c and d must share a shard: %d vs %d", as["c_test.go"], as["d_test.go"])
	}
	if got := files["a_test.go"]; !strings.HasPrefix(got, "//go:build !hubshard || hubshard_") || !strings.HasSuffix(got, fixture["a_test.go"]) {
		t.Errorf("a_test.go: unexpected header:\n%s", got)
	}
	if got := files["b_test.go"]; !strings.HasPrefix(got, "//go:build !no_sqlite && (!hubshard || hubshard_") {
		t.Errorf("b_test.go: constraint not combined:\n%s", got)
	}
	if got := files["g_test.go"]; !strings.Contains(got, "//go:build (!unix || openbsd) && (!hubshard || hubshard_") {
		t.Errorf("g_test.go: OR constraint not parenthesised:\n%s", got)
	}
	for name, want := range map[string]string{
		"i_test.go": "//go:build !no_sqlite && (!hubshard || hubshard_",
		"j_test.go": "//go:build linux && (!hubshard || hubshard_",
		"k_test.go": "//go:build !hubshard || hubshard_",
		"l_test.go": "//go:build linux && (!hubshard || hubshard_",
		"m_test.go": "//go:build !hubshard || hubshard_",
	} {
		got := files[name]
		if n := strings.Count(got, "//go:build"); n != 1 || !strings.Contains(got, want) {
			t.Errorf("%s: want one build line containing %q, got %d:\n%s", name, want, n, got)
		}
	}
	// m_test.go keeps its line count: the clause took the empty // line.
	if got, want := strings.Count(files["m_test.go"], "\n"), strings.Count(fixture["m_test.go"], "\n"); got != want {
		t.Errorf("m_test.go: %d lines in the overlay, %d in the tree:\n%s", got, want, files["m_test.go"])
	}
	if !strings.HasPrefix(files["m_test.go"], "// Copyright 2026 Google LLC\n//go:build ") {
		t.Errorf("m_test.go: clause not on the empty // line:\n%s", files["m_test.go"])
	}

	// Every rewritten file compiles, per go/build, in its own shard only.
	for name, src := range files {
		if isImportsFile(name) {
			continue
		}
		for k := 1; k <= 2; k++ {
			tags := shardTags(k, nil)
			want, err := matchSrc(name, []byte(fixture[name]), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := matchSrc(name, []byte(src), tags)
			if err != nil {
				t.Fatal(err)
			}
			if want && got != (as[name] == k) {
				t.Errorf("%s: go/build compiles it in shard %d = %v, assigned to %d", name, k, got, as[name])
			}
		}
	}

	// Deterministic: a second run writes the same overlay.
	again, _ := overlayFiles(t, baseArgs(root, dir, "-n", "2"))
	if len(again) != len(files) {
		t.Fatalf("second run: %d files, first %d", len(again), len(files))
	}
	for name, src := range files {
		if again[name] != src {
			t.Errorf("%s differs between runs", name)
		}
	}
}

func TestParity(t *testing.T) {
	root, dir := writeFixture(t)
	var out bytes.Buffer
	if err := run(baseArgs(root, dir, "-n", "2", "-parity"), &out); err != nil {
		t.Fatalf("-parity: %v\n%s", err, out.String())
	}
	for _, want := range []string{"parity default: ", "parity no_sqlite: ", "parity integration: ", "parity vet-integration: ", "parity vet: ", "parity ok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("-parity output lacks %q:\n%s", want, out.String())
		}
	}
}

// fixtureSharding builds the sharding of the fixture without the CLI, so
// a test can doctor it.
func fixtureSharding(t *testing.T, n int) *sharding {
	t.Helper()
	_, dir := writeFixture(t)
	files, err := loadFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	an := analyze(files)
	s, err := newSharding(dir, files, an, assignShards(an, files, n), n)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParityCatchesIgnoredClause(t *testing.T) {
	// A sharded file whose clause go/build does not see compiles in every
	// shard. Simulate it with the untagged source.
	s := fixtureSharding(t, 2)
	s.genFiles["a_test.go"] = s.files["a_test.go"]
	err := s.parity(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "a_test.go: assigned to shard") {
		t.Fatalf("want a tag placement problem for a_test.go, got %v", err)
	}
}

func TestParityCatchesCrossShardUse(t *testing.T) {
	s := fixtureSharding(t, 2)
	other := 3 - s.assign["f_test.go"]
	for _, n := range []string{"a_test.go", "b_test.go", "e_test.go", "g_test.go", "h_test.go", "m_test.go"} {
		if s.assign[n] != other {
			continue
		}
		s.an.uses[n] = map[string]bool{"f_test.go": true}
		err := s.parity(io.Discard)
		if err == nil || !strings.Contains(err.Error(), n+" uses f_test.go") {
			t.Fatalf("want a cross-shard use problem, got %v", err)
		}
		return
	}
	t.Fatal("fixture: no file in the other shard")
}

func TestBreakage(t *testing.T) {
	s := fixtureSharding(t, 2)
	retag := func(name, line string) map[string]*fileInfo {
		t.Helper()
		m := map[string]*fileInfo{}
		for n, f := range s.genFiles {
			m[n] = f
		}
		src := string(s.genFiles[name].src)
		if _, e, _ := headerBuildLine([]byte(src)); e != nil {
			src = src[strings.Index(src, "\n")+1:]
		}
		g, err := parseFile(name, []byte(line+"\n"+src))
		if err != nil {
			t.Fatal(err)
		}
		m[name] = g
		return m
	}
	other := 3 - s.assign["a_test.go"]
	for _, tc := range []struct{ name, line, want string }{
		{"tag for a shard that does not exist", "//go:build !hubshard || hubshard_9", "a_test.go: hubshard tag is not"},
		{"wrong shard", fmt.Sprintf("//go:build !hubshard || hubshard_%d", other), "a_test.go: tagged for shard"},
		{"hubshard_01", fmt.Sprintf("//go:build !hubshard || hubshard_0%d", s.assign["a_test.go"]), "a_test.go: hubshard tag is not"},
		{"two clauses", "//go:build (!hubshard || hubshard_1) && (!hubshard || hubshard_2)", "a_test.go: hubshard tag is not"},
		{"bare !hubshard", "//go:build !hubshard", "a_test.go: hubshard tag outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(breakage(s.an, retag("a_test.go", tc.line), s.assign, 2), "\n")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
	if got := breakage(s.an, s.genFiles, s.assign, 2); len(got) != 0 {
		t.Errorf("generated files: %v", got)
	}
}

func TestListSkipLocate(t *testing.T) {
	root, dir := writeFixture(t)
	args := baseArgs(root, dir, "-n", "2")
	files, _ := overlayFiles(t, args)
	as := shardOfOverlay(t, files)
	runOut := func(extra ...string) (string, error) {
		var b bytes.Buffer
		err := run(append(append([]string(nil), args...), extra...), &b)
		return b.String(), err
	}

	for k := 1; k <= 2; k++ {
		got, err := runOut("-list", fmt.Sprint(k))
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for name, src := range fixture {
			if as[name] != 0 && as[name] != k {
				continue
			}
			if ok, _ := matchSrc(name, []byte(src), nil); !ok {
				continue
			}
			for _, l := range strings.Split(src, "\n") {
				if strings.HasPrefix(l, "func Test") && !strings.HasPrefix(l, "func TestMain") {
					want = append(want, strings.TrimPrefix(l[:strings.Index(l, "(")], "func "))
				}
			}
		}
		sort.Strings(want)
		wantOut := ""
		if len(want) > 0 {
			wantOut = strings.Join(want, "\n") + "\n"
		}
		if got != wantOut {
			t.Errorf("-list %d = %q, want %v", k, got, want)
		}
	}

	// helpers_test.go (common) declares no test; add one to see -skip.
	if err := os.WriteFile(filepath.Join(dir, "common_helpers_test.go"), []byte(header+"import \"testing\"\n\nfunc TestCommon(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := runOut("-skip", "1"); err != nil || got != "\n" {
		t.Errorf("-skip 1 = %q, %v; want an empty line", got, err)
	}
	if got, err := runOut("-skip", "2"); err != nil || got != "^(TestCommon)$\n" {
		t.Errorf("-skip 2 = %q, %v", got, err)
	}

	got, err := runOut("-locate", "TestA", "TestCommon")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("TestA %d\nTestCommon 1\n", as["a_test.go"]); got != want {
		t.Errorf("-locate = %q, want %q", got, want)
	}
	if _, err := runOut("-locate", "TestNoSuch"); err == nil || !strings.Contains(err.Error(), "TestNoSuch") {
		t.Errorf("-locate of a missing test: %v", err)
	}
	// TestB is !no_sqlite: not in a no_sqlite build.
	if _, err := runOut("-tags", "no_sqlite", "-locate", "TestB"); err == nil {
		t.Error("-locate TestB under no_sqlite should fail")
	}
}

func TestShards(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "1\n2\n3\n4\n5\n6\n"},
		{[]string{"-n", "2"}, "1\n2\n"},
	} {
		var b bytes.Buffer
		// -shards reads no files: a missing -dir must not matter.
		if err := run(append(append([]string{"-dir", "/nonexistent"}, tc.args...), "-shards"), &b); err != nil {
			t.Fatal(err)
		}
		if b.String() != tc.want {
			t.Errorf("-shards %v = %q, want %q", tc.args, b.String(), tc.want)
		}
	}
}

func TestRejectsCommittedTags(t *testing.T) {
	root, dir := writeFixture(t)
	files, _ := overlayFiles(t, baseArgs(root, dir, "-n", "2"))
	// Commit the overlay into the tree, as an older generator did.
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := run(baseArgs(root, dir, "-parity"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-untag") || !strings.Contains(err.Error(), "a_test.go") {
		t.Fatalf("want the committed tags rejected, got %v", err)
	}
	var out bytes.Buffer
	if err := run(baseArgs(root, dir, "-untag"), &out); err != nil {
		t.Fatal(err)
	}
	// -untag restores the tree byte for byte, both clause forms included.
	got, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fixture) {
		t.Errorf("after -untag: %d files, want %d", len(got), len(fixture))
	}
	for name, src := range fixture {
		if g := read(t, dir, name); g != src {
			t.Errorf("%s: untag round trip:\n got %q\nwant %q", name, g, src)
		}
	}
	if err := run(baseArgs(root, dir, "-parity"), io.Discard); err != nil {
		t.Fatalf("-parity after -untag: %v", err)
	}
}

func TestStrayShardClauses(t *testing.T) {
	// A tagged file outside -dir (in a subpackage or elsewhere) fails every
	// mode, -untag included (it does not touch them).
	root, dir := writeFixture(t)
	tagged := "// Copyright 2026 Google LLC\n\n//go:build !no_sqlite && (!hubshard || hubshard_2)\n\npackage sub\n"
	for _, p := range []string{filepath.Join(dir, "sub", "moved_test.go"), filepath.Join(root, "other", "moved_test.go")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(tagged), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Not reported: hidden directories, and a hubshard word that is not a tag.
	hidden := filepath.Join(root, ".hidden", "x_test.go")
	if err := os.MkdirAll(filepath.Dir(hidden), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hidden, []byte(tagged), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "word_test.go"), []byte("//go:build hubsharding\n\npackage other\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"-parity", "-untag"} {
		err := run(baseArgs(root, dir, mode), io.Discard)
		if err == nil {
			t.Fatalf("%s should report hubshard tags outside -dir", mode)
		}
		for _, want := range []string{filepath.Join("hub", "sub", "moved_test.go"), filepath.Join("other", "moved_test.go")} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error does not name %s: %v", mode, want, err)
			}
		}
		for _, unwanted := range []string{".hidden", "word_test.go", "a_test.go"} {
			if strings.Contains(err.Error(), unwanted) {
				t.Errorf("%s: error names %s: %v", mode, unwanted, err)
			}
		}
	}
}

func TestSpareCommentLine(t *testing.T) {
	for src, want := range map[string]int{
		license + "package hub\n":                    1,
		"// a\n//\n// b\npackage hub\n":              -1, // doc comment: no blank line after it
		"// a\n\n// doc\n//\n// more\npackage hub\n": -1,
		"// a\n//\n\n// doc\n//\npackage hub\n":      1,
		"/* a */\n//\n\npackage hub\n":               -1,
		"// Copyright\n\npackage hub\n":              -1,
		"package hub\n//\n\n":                        -1,
	} {
		if got := spareCommentLine(strings.SplitAfter(src, "\n")); got != want {
			t.Errorf("spareCommentLine(%q) = %d, want %d", src, got, want)
		}
	}
}

func TestIsListedTest(t *testing.T) {
	src := header + `import "testing"

func Test(t *testing.T)            {}
func TestA(t *testing.T)           {}
func Testify(t *testing.T)         {}
func TestMain(m *testing.M)        {}
func Test_x(t *testing.T)          {}
func BenchmarkB(b *testing.B)      {}
func FuzzF(f *testing.F)           {}
func ExampleWith() {
	// Output: x
}
func ExampleWithout() {}
func ExampleUnordered() {
	// some code
	x := 1
	_ = x
	// Unordered output:
	// x
}
func ExampleNotLast() {
	// Output: x

	// a later comment group: not an output comment
}

type T struct{}

func (T) TestMethod(t *testing.T) {}
`
	f, err := parseFile("x_test.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Test", "TestA", "Test_x", "BenchmarkB", "FuzzF", "ExampleWith", "ExampleUnordered"}
	if strings.Join(f.tests, " ") != strings.Join(want, " ") {
		t.Errorf("tests = %v, want %v", f.tests, want)
	}
}

func TestHeaderBuildLine(t *testing.T) {
	for src, want := range map[string]int{
		"//go:build linux\n\npackage hub\n":                  0,
		"// c\n\n//go:build linux\npackage hub\n":            2,
		"/*\n//go:build linux\n*/\npackage hub\n":            -1, // inside a block
		"/* a */ /* b */\n//go:build linux\npackage hub\n":   1,
		"/* a\n*/ /* b\n*/\n//go:build linux\npackage hub\n": 3,
		"/* a */ // b\n//go:build linux\npackage hub\n":      1,
		"/* a */ package hub\n//go:build linux\n":            -1, // after the package clause
		"package hub\n//go:build linux\n":                    -1,
	} {
		got, _, err := headerBuildLine([]byte(src))
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if got != want {
			t.Errorf("headerBuildLine(%q) = %d, want %d", src, got, want)
		}
	}
}

func TestGuessImportName(t *testing.T) {
	for in, want := range map[string]string{
		"net/http":                            "http",
		"github.com/stretchr/testify/require": "require",
		"gopkg.in/yaml.v3":                    "yaml",
		"github.com/knadh/koanf/v2":           "koanf",
		"github.com/go-chi/chi":               "chi",
	} {
		if got := guessImportName(in); got != want {
			t.Errorf("guessImportName(%q) = %q, want %q", in, got, want)
		}
	}
}
