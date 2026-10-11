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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const header = "// Copyright 2026 Google LLC\n\npackage hub\n\n"

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
	// A *_helpers_test.go file is common even when no test file uses it.
	"unused_helpers_test.go": header + "func unusedHelper() {}\n",
}

func writeFixture(t *testing.T) (dir, assign string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "hub")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range fixture {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, filepath.Join(root, "assignment.txt")
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGenerate(t *testing.T) {
	dir, assign := writeFixture(t)
	if err := run([]string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign, "-n", "2"}, io.Discard); err != nil {
		t.Fatal(err)
	}

	for _, common := range []string{"main_test.go", "setup_helper_test.go", "helpers_test.go", "unused_helpers_test.go"} {
		if got := read(t, dir, common); got != fixture[common] {
			t.Errorf("%s: common file changed:\n%s", common, got)
		}
	}
	_, as, err := readAssignment(assign)
	if err != nil {
		t.Fatal(err)
	}
	for _, sharded := range []string{"a_test.go", "b_test.go", "c_test.go", "d_test.go", "e_test.go", "f_test.go", "g_test.go", "h_test.go", "i_test.go", "j_test.go", "k_test.go", "l_test.go"} {
		if as[sharded] < 1 || as[sharded] > 2 {
			t.Errorf("%s: want shard 1 or 2, got %d", sharded, as[sharded])
		}
	}
	if as["c_test.go"] != as["d_test.go"] {
		t.Errorf("c and d must share a shard: %d vs %d", as["c_test.go"], as["d_test.go"])
	}

	if got := read(t, dir, "a_test.go"); !strings.HasPrefix(got, "//go:build !hubshard || hubshard_") || !strings.HasSuffix(got, fixture["a_test.go"]) {
		t.Errorf("a_test.go: unexpected header:\n%s", got)
	}
	if got := read(t, dir, "b_test.go"); !strings.HasPrefix(got, "//go:build !no_sqlite && (!hubshard || hubshard_") {
		t.Errorf("b_test.go: constraint not combined:\n%s", got)
	}
	if got := read(t, dir, "g_test.go"); !strings.Contains(got, "//go:build (!unix || openbsd) && (!hubshard || hubshard_") {
		t.Errorf("g_test.go: OR constraint not parenthesised:\n%s", got)
	}

	for name, want := range map[string]string{
		"i_test.go": "//go:build !no_sqlite && (!hubshard || hubshard_",
		"j_test.go": "//go:build linux && (!hubshard || hubshard_",
		"k_test.go": "//go:build !hubshard || hubshard_",
		"l_test.go": "//go:build linux && (!hubshard || hubshard_",
	} {
		got := read(t, dir, name)
		if n := strings.Count(got, "//go:build"); n != 1 || !strings.Contains(got, want) {
			t.Errorf("%s: want one build line containing %q, got %d:\n%s", name, want, n, got)
		}
	}

	// Idempotent: a second run changes nothing, and -check passes.
	if err := run([]string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign, "-check"}, io.Discard); err != nil {
		t.Fatalf("check after generate: %v", err)
	}
}

func TestStableAssignment(t *testing.T) {
	dir, assign := writeFixture(t)
	if err := run([]string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign, "-n", "2"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	_, before, _ := readAssignment(assign)

	// A new, unrelated file must not move existing files.
	src := header + "import \"testing\"\n\nfunc TestNew(t *testing.T) {}\n" + strings.Repeat("// pad\n", 50)
	if err := os.WriteFile(filepath.Join(dir, "z_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign, "-check"}, io.Discard); err != nil {
		t.Fatalf("-check should accept a new self-contained untagged file: %v", err)
	}
	if err := run([]string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign, "-check", "-strict"}, io.Discard); err == nil {
		t.Fatal("-check -strict should report the new untagged file")
	}
	if err := run([]string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign}, io.Discard); err != nil {
		t.Fatal(err)
	}
	_, after, _ := readAssignment(assign)
	for f, k := range before {
		if after[f] != k {
			t.Errorf("%s moved from shard %d to %d", f, k, after[f])
		}
	}
	if after["z_test.go"] == 0 {
		t.Error("new file was not assigned")
	}
}

func TestCheckBreakage(t *testing.T) {
	// Each case starts from a freshly generated fixture (N=2), edits it, and
	// runs -check. want is "" when -check must pass, else a substring of
	// the error.
	retag := func(t *testing.T, dir, name, line string) {
		t.Helper()
		src := read(t, dir, name)
		i := strings.Index(src, "\n")
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line+src[i:]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	add := func(t *testing.T, dir, name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	other := func(k int) int { return 3 - k }
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, dir string, as map[string]int)
		want string
	}{
		{"new file using a common helper", func(t *testing.T, dir string, _ map[string]int) {
			add(t, dir, "new_test.go", header+"import \"testing\"\n\nfunc TestNew(t *testing.T) { _ = helperA() }\n")
		}, ""},
		{"new file using a sharded file", func(t *testing.T, dir string, as map[string]int) {
			add(t, dir, "new_test.go", header+"import \"testing\"\n\nfunc TestNew(t *testing.T) { helperF() }\n")
		}, "new_test.go uses f_test.go"},
		{"new file adding a method to a sharded file's type", func(t *testing.T, dir string, _ map[string]int) {
			add(t, dir, "new_test.go", header+"func (fakeD) Other() {}\n")
		}, "new_test.go uses d_test.go"},
		{"sharded file using another shard", func(t *testing.T, dir string, as map[string]int) {
			// Find a sharded file in the shard f is not in, and make it use helperF.
			for _, n := range []string{"a_test.go", "b_test.go", "e_test.go", "g_test.go", "h_test.go"} {
				if as[n] == other(as["f_test.go"]) {
					src := read(t, dir, n)
					add(t, dir, n, src+"\nfunc useF() { helperF() }\n")
					return
				}
			}
			t.Skip("fixture: no file in the other shard")
		}, "uses f_test.go"},
		{"deleted file", func(t *testing.T, dir string, _ map[string]int) {
			if err := os.Remove(filepath.Join(dir, "e_test.go")); err != nil {
				t.Fatal(err)
			}
		}, "e_test.go: in the assignment file but does not exist"},
		{"tag for a shard that does not exist", func(t *testing.T, dir string, _ map[string]int) {
			retag(t, dir, "a_test.go", "//go:build !hubshard || hubshard_9")
		}, "a_test.go: hubshard tag is not"},
		{"hand-edited tag", func(t *testing.T, dir string, as map[string]int) {
			retag(t, dir, "a_test.go", fmt.Sprintf("//go:build !hubshard || hubshard_%d", other(as["a_test.go"])))
		}, "a_test.go: tagged for shard"},
		{"tagged but not listed (renamed)", func(t *testing.T, dir string, _ map[string]int) {
			if err := os.Rename(filepath.Join(dir, "e_test.go"), filepath.Join(dir, "e2_test.go")); err != nil {
				t.Fatal(err)
			}
		}, "e2_test.go: tagged for shard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, assign := writeFixture(t)
			args := []string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign}
			if err := run(append(args, "-n", "2"), io.Discard); err != nil {
				t.Fatal(err)
			}
			_, as, _ := readAssignment(assign)
			tc.edit(t, dir, as)
			err := run(append(args, "-check"), io.Discard)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("-check: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("-check: want an error containing %q, got %v", tc.want, err)
			}
			// make hubshard fixes every case.
			if err := run(args, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := run(append(args, "-check", "-strict"), io.Discard); err != nil {
				t.Fatalf("-check -strict after make hubshard: %v", err)
			}
		})
	}
}

func TestNewFileJoinsGroups(t *testing.T) {
	// p and q start in different shards. A new file that uses both joins
	// them into one group, which moves to the shard holding most of its
	// lines (p's).
	root := t.TempDir()
	dir := filepath.Join(root, "hub")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	assign := filepath.Join(root, "assignment.txt")
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("p_test.go", header+"func helperP() {}\n"+strings.Repeat("// pad\n", 100))
	write("q_test.go", header+"func helperQ() {}\n"+strings.Repeat("// pad\n", 10))
	// -maxgroupdiv 1: no size cut, so nothing becomes common.
	args := []string{"-root", root, "-dir", dir, "-assign", assign, "-maxgroupdiv", "1"}
	if err := run(append(args, "-n", "2"), io.Discard); err != nil {
		t.Fatal(err)
	}
	_, before, _ := readAssignment(assign)
	if before["p_test.go"] == before["q_test.go"] || before["p_test.go"] == 0 || before["q_test.go"] == 0 {
		t.Fatalf("setup: want p and q in different shards, got %v", before)
	}
	write("x_test.go", header+"import \"testing\"\n\nfunc TestX(t *testing.T) { helperP(); helperQ() }\n")
	if err := run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	_, after, _ := readAssignment(assign)
	for _, name := range []string{"p_test.go", "q_test.go", "x_test.go"} {
		if after[name] != before["p_test.go"] {
			t.Errorf("%s: want shard %d (p's), got %d", name, before["p_test.go"], after[name])
		}
	}
	if got := read(t, dir, "q_test.go"); !strings.HasPrefix(got, fmt.Sprintf("//go:build !hubshard || hubshard_%d\n", before["p_test.go"])) {
		t.Errorf("q_test.go not retagged:\n%s", got)
	}
}

func TestStrayShardClauses(t *testing.T) {
	// A tagged file outside -dir (in a subpackage or elsewhere) fails the
	// run; -dir's own tagged files do not.
	dir, assign := writeFixture(t)
	root := filepath.Dir(dir)
	if err := run([]string{"-root", root, "-dir", dir, "-assign", assign, "-n", "2"}, io.Discard); err != nil {
		t.Fatal(err)
	}
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

	err := run([]string{"-root", root, "-dir", dir, "-assign", assign, "-check"}, io.Discard)
	if err == nil {
		t.Fatal("-check should report hubshard tags outside -dir")
	}
	for _, want := range []string{filepath.Join("hub", "sub", "moved_test.go"), filepath.Join("other", "moved_test.go")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	for _, unwanted := range []string{".hidden", "word_test.go", "a_test.go"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error names %s: %v", unwanted, err)
		}
	}
}

func TestUntagRoundTrip(t *testing.T) {
	// A file that becomes common gets its original bytes back.
	dir, assign := writeFixture(t)
	if err := run([]string{"-root", filepath.Dir(dir), "-dir", dir, "-assign", assign, "-n", "2"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a_test.go", "b_test.go", "g_test.go", "i_test.go", "j_test.go", "k_test.go", "l_test.go"} {
		f, err := loadFiles(dir)
		if err != nil {
			t.Fatal(err)
		}
		out, err := rewrite(f[name], 0)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != fixture[name] {
			t.Errorf("%s: untag round trip:\n got %q\nwant %q", name, out, fixture[name])
		}
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
