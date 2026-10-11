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
	// helperA is used by two files, so it is common.
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
	if err := run([]string{"-dir", dir, "-assign", assign, "-n", "2"}, io.Discard); err != nil {
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
	for _, sharded := range []string{"a_test.go", "b_test.go", "c_test.go", "d_test.go", "e_test.go", "f_test.go", "g_test.go", "h_test.go"} {
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

	// Idempotent: a second run changes nothing, and -check passes.
	if err := run([]string{"-dir", dir, "-assign", assign, "-check"}, io.Discard); err != nil {
		t.Fatalf("check after generate: %v", err)
	}
}

func TestStableAssignment(t *testing.T) {
	dir, assign := writeFixture(t)
	if err := run([]string{"-dir", dir, "-assign", assign, "-n", "2"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	_, before, _ := readAssignment(assign)

	// A new, unrelated file must not move existing files.
	src := header + "import \"testing\"\n\nfunc TestNew(t *testing.T) {}\n" + strings.Repeat("// pad\n", 50)
	if err := os.WriteFile(filepath.Join(dir, "z_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-dir", dir, "-assign", assign, "-check"}, io.Discard); err == nil {
		t.Fatal("-check should report the new untagged file")
	}
	if err := run([]string{"-dir", dir, "-assign", assign}, io.Discard); err != nil {
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

func TestUntagRoundTrip(t *testing.T) {
	// A file that becomes common gets its original bytes back.
	dir, assign := writeFixture(t)
	if err := run([]string{"-dir", dir, "-assign", assign, "-n", "2"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a_test.go", "b_test.go", "g_test.go"} {
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
