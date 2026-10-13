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

func bulk(t *testing.T, dir, glob string, dry bool) (*bulkStats, string) {
	t.Helper()
	return bulkBy(t, dir, glob, "area", dry)
}

func bulkBy(t *testing.T, dir, glob, by string, dry bool) (*bulkStats, string) {
	t.Helper()
	var sb strings.Builder
	st, err := runBulkOn(dir, glob, by, nil, dry, 10, &sb)
	if err != nil {
		t.Fatalf("bulk: %v\n%s", err, sb.String())
	}
	return st, sb.String()
}

func TestBulkFixpoint(t *testing.T) {
	dir := writePkg(t, basePkg)
	before := copyDir(t, dir)
	st, out := bulk(t, dir, "", false)

	// Round 1 moves helper (used by b_test.go) and inner (used only by
	// helper). Then shared is used by TestA and by the helper file, so
	// round 2 moves it too.
	if st.iterations != 2 || st.moved != 3 {
		t.Fatalf("rounds=%d moved=%d, want 2 and 3\n%s", st.iterations, st.moved, out)
	}
	h := read(t, dir, "a_helpers_test.go")
	for _, s := range []string{"func helper", "func inner", "const shared"} {
		if !strings.Contains(h, s) {
			t.Errorf("a_helpers_test.go lacks %q:\n%s", s, h)
		}
	}
	a := read(t, dir, "a_test.go")
	if !strings.Contains(a, "func TestA") || strings.Contains(a, "func helper") {
		t.Errorf("a_test.go:\n%s", a)
	}
	// lonely is used by nobody: it stays.
	if !strings.Contains(read(t, dir, "only_test.go"), "func lonely") {
		t.Error("unused lonely must stay")
	}
	mustVerify(t, before, dir)

	// A second run has nothing to do.
	snap := copyDir(t, dir)
	st, out = bulk(t, dir, "", false)
	if st.iterations != 0 || st.moved != 0 {
		t.Fatalf("rerun: rounds=%d moved=%d\n%s", st.iterations, st.moved, out)
	}
	mustVerify(t, snap, dir)
}

func TestBulkKeepsOriginConstraint(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nimport \"testing\"\n\nfunc h() int { return 1 }\n\nfunc TestA(t *testing.T) { _ = h() }\n",
		"b_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nvar _ = h()\n",
	})
	before := copyDir(t, dir)
	bulk(t, dir, "", false)
	if h := read(t, dir, "a_sqlite_helpers_test.go"); !strings.Contains(h, "//go:build !no_sqlite\n") || !strings.Contains(h, "func h()") {
		t.Errorf("a_sqlite_helpers_test.go:\n%s", h)
	}
	mustVerify(t, before, dir)
}

func TestBulkOriginsGlob(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a_test.go": hdr + "package p\n\nfunc ha() int { return hb() }\n",
		"b_test.go": hdr + "package p\n\nfunc hb() int { return 1 }\n\nvar _ = ha()\n",
	})
	bulk(t, dir, "b_*", false)
	if _, err := os.Stat(filepath.Join(dir, "a_helpers_test.go")); !os.IsNotExist(err) {
		t.Errorf("a_test.go is not an origin under -origins b_*: stat err=%v", err)
	}
	if !strings.Contains(read(t, dir, "b_helpers_test.go"), "func hb()") {
		t.Error("hb should move to b_helpers_test.go")
	}
}

func TestBulkGroupAndNotes(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a_test.go": hdr + `package p

import "testing"

var (
	x = 1
	y = 2
)

func TestShared(t *testing.T) {}

func TestA(t *testing.T) { _ = y }
`,
		"b_test.go":   hdr + "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { _ = x; TestShared(t); _ = osLimit }\n",
		"c_test.go":   hdr + "//go:build linux\n\npackage p\n\nconst osLimit = 1\n",
		"c_o_test.go": hdr + "//go:build !linux\n\npackage p\n\nconst osLimit = 2\n",
	})
	before := copyDir(t, dir)
	_, out := bulk(t, dir, "", false)
	h := read(t, dir, "a_helpers_test.go")
	if !strings.Contains(h, "x = 1") || !strings.Contains(h, "y = 2") {
		t.Errorf("the whole var block should move:\n%s", h)
	}
	for _, s := range []string{"TestShared is used by another test file but is a test entry point", "osLimit is declared 2 times"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks note %q:\n%s", s, out)
		}
	}
	if !strings.Contains(read(t, dir, "a_test.go"), "func TestShared") {
		t.Error("TestShared must stay")
	}
	mustVerify(t, before, dir)
}

func TestBulkDryRun(t *testing.T) {
	dir := writePkg(t, basePkg)
	before := copyDir(t, dir)
	st, out := bulk(t, dir, "", true)
	if st.moved == 0 || !strings.Contains(out, "move helper") {
		t.Fatalf("dry run should plan moves:\n%s", out)
	}
	ents, _ := os.ReadDir(dir)
	bents, _ := os.ReadDir(before)
	if len(ents) != len(bents) {
		t.Fatalf("dry run wrote files")
	}
	for _, e := range bents {
		if read(t, dir, e.Name()) != read(t, before, e.Name()) {
			t.Errorf("dry run changed %s", e.Name())
		}
	}
}

func TestBulkSkipsUnmovableOrigin(t *testing.T) {
	// The dest exists and is not helper-only clean (a free-floating comment),
	// so the origin is skipped and reported; the rest still moves.
	dir := writePkg(t, map[string]string{
		"a_test.go":         hdr + "package p\n\nfunc ha() int { return 1 }\n",
		"a_helpers_test.go": hdr + "package p\n\n// banner\n\nfunc old() {}\n",
		"b_test.go":         hdr + "package p\n\nfunc hb() int { return ha() }\n",
		"c_test.go":         hdr + "package p\n\nvar _ = hb()\n",
	})
	st, out := bulk(t, dir, "", false)
	if _, ok := st.skipped["a_test.go"]; !ok || !strings.Contains(out, "skip a_helpers_test.go (from a_test.go)") {
		t.Errorf("a_test.go should be skipped:\n%s", out)
	}
	if !strings.Contains(read(t, dir, "b_helpers_test.go"), "func hb()") {
		t.Errorf("hb should still move:\n%s", out)
	}
}

func TestBulkByArea(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"handlers_x_test.go": hdr + "package p\n\nfunc hx() int { return 1 }\n",
		"handlers_y_test.go": hdr + "package p\n\nfunc hy() int { return 2 }\n",
		"handlers_z_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nfunc hz() int { return 3 }\n",
		"web_test.go":        hdr + "//go:build !no_sqlite\n\npackage p\n\nvar _ = hx() + hy() + hz()\n",
	})
	before := copyDir(t, dir)
	bulk(t, dir, "", false)
	h := read(t, dir, "handlers_helpers_test.go")
	if !strings.Contains(h, "func hx()") || !strings.Contains(h, "func hy()") || strings.Contains(h, "go:build") {
		t.Errorf("handlers_helpers_test.go:\n%s", h)
	}
	hs := read(t, dir, "handlers_sqlite_helpers_test.go")
	if !strings.Contains(hs, "//go:build !no_sqlite\n") || !strings.Contains(hs, "func hz()") {
		t.Errorf("handlers_sqlite_helpers_test.go:\n%s", hs)
	}
	// The origins had nothing else, so they are deleted.
	for _, n := range []string{"handlers_x_test.go", "handlers_y_test.go", "handlers_z_test.go"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted, stat err=%v", n, err)
		}
	}
	mustVerify(t, before, dir)
}

func TestBulkByOrigin(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"handlers_x_test.go": hdr + "package p\n\nfunc hx() int { return 1 }\n",
		"web_test.go":        hdr + "package p\n\nvar _ = hx()\n",
	})
	bulkBy(t, dir, "", "origin", false)
	if !strings.Contains(read(t, dir, "handlers_x_helpers_test.go"), "func hx()") {
		t.Error("hx should move to handlers_x_helpers_test.go")
	}
}

func TestBulkDest(t *testing.T) {
	cases := []struct{ origin, constraint, by, want string }{
		{"handlers_agent_test.go", "", "area", "handlers_helpers_test.go"},
		{"handlers_agent_test.go", "!no_sqlite", "area", "handlers_sqlite_helpers_test.go"},
		{"web_test.go", "", "area", "web_helpers_test.go"},
		{"browser_rlimit_unix_test.go", "linux || darwin", "area", "browser_linuxdarwin_helpers_test.go"},
		{"x_test.go", "!windows", "area", "x_notwindows_helpers_test.go"},
		{"handlers_agent_test.go", "!no_sqlite", "origin", "handlers_agent_helpers_test.go"},
	}
	for _, c := range cases {
		if got := bulkDest(c.origin, c.constraint, c.by); got != c.want {
			t.Errorf("bulkDest(%q, %q, %q) = %q, want %q", c.origin, c.constraint, c.by, got, c.want)
		}
	}
}

func TestBulkSharedClosureMovesOnce(t *testing.T) { testBulkSharedClosure(t, false) }

// The dry run (-n) must print the same plan as the real run: each key once.
func TestBulkSharedClosureMovesOnceDryRun(t *testing.T) { testBulkSharedClosure(t, true) }

func testBulkSharedClosure(t *testing.T, dry bool) {
	// hA (area a) uses dA, declared in area b's origin and used by nothing
	// else, so the a family takes dA as its closure. dA is also cross-file
	// used (by hA), so the b family lists it too; it must not pull dA back
	// out of a_helpers_test.go. e lives in an existing helper file and is
	// used only by hB: the closure must leave it there.
	dir := writePkg(t, map[string]string{
		"a_x_test.go":       hdr + "package p\n\nfunc hA() int { return dA() }\n",
		"b_y_test.go":       hdr + "package p\n\nfunc dA() int { return 1 }\n\nfunc hB() int { return e() }\n",
		"h_helpers_test.go": hdr + "package p\n\nfunc e() int { return 2 }\n",
		"z_test.go":         hdr + "package p\n\nvar _ = hA() + hB()\n",
	})
	before := copyDir(t, dir)
	_, out := bulk(t, dir, "", dry)
	counts := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[0] == "move" {
			counts[f[1]]++
		}
	}
	for _, k := range []string{"hA", "dA", "hB"} {
		if counts[k] != 1 {
			t.Errorf("%s moved %d times, want 1\n%s", k, counts[k], out)
		}
	}
	if counts["e"] != 0 || !strings.Contains(read(t, dir, "h_helpers_test.go"), "func e()") {
		t.Errorf("e must stay in h_helpers_test.go\n%s", out)
	}
	if dry {
		if !strings.Contains(out, "move dA") || !strings.Contains(out, "-> a_helpers_test.go (dependency of hA)") {
			t.Errorf("dry run should plan dA as hA's closure\n%s", out)
		}
		return
	}
	if !strings.Contains(read(t, dir, "a_helpers_test.go"), "func dA()") {
		t.Errorf("dA should be in a_helpers_test.go (closure of hA)\n%s", out)
	}
	mustVerify(t, before, dir)
}

func TestBulkClosureKeepsOtherConstraint(t *testing.T) {
	// h (!no_sqlite) uses d, declared in an unconstrained file and used only
	// by h. Moving d into the !no_sqlite helper file would narrow it, so the
	// closure leaves it in place instead of failing the whole family.
	dir := writePkg(t, map[string]string{
		"a_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nfunc h() int { return d() }\n",
		"c_test.go": hdr + "package p\n\nfunc d() int { return 1 }\n\nvar _ = 0\n",
		"z_test.go": hdr + "//go:build !no_sqlite\n\npackage p\n\nvar _ = h()\n",
	})
	before := copyDir(t, dir)
	st, out := bulk(t, dir, "", false)
	if len(st.skipped) != 0 {
		t.Fatalf("nothing should be skipped:\n%s", out)
	}
	if !strings.Contains(read(t, dir, "a_sqlite_helpers_test.go"), "func h()") {
		t.Errorf("h should move\n%s", out)
	}
	mustVerify(t, before, dir)
}

func TestBulkCountsOnlyRoundsThatMove(t *testing.T) {
	// The only family is refused (free-floating comment in its dest), so no
	// round moves anything.
	dir := writePkg(t, map[string]string{
		"a_test.go":         hdr + "package p\n\nfunc ha() int { return 1 }\n",
		"a_helpers_test.go": hdr + "package p\n\n// banner\n\nfunc old() {}\n",
		"b_test.go":         hdr + "package p\n\nvar _ = ha()\n",
	})
	st, out := bulk(t, dir, "", false)
	if st.iterations != 0 || st.moved != 0 {
		t.Errorf("rounds=%d moved=%d, want 0 and 0\n%s", st.iterations, st.moved, out)
	}
}

func TestBulkKeep(t *testing.T) {
	// pinned is used by another file but must stay; inner is used only by
	// pinned, so nothing pulls it either. free moves as usual, and the
	// closure of free must not take pinned (only free uses it here too).
	dir := writePkg(t, map[string]string{
		"a_test.go": hdr + "package p\n\nfunc pinned() int { return inner() }\n\nfunc inner() int { return 1 }\n\nfunc free() int { return pinned() + only() }\n\nfunc only() int { return 2 }\n",
		"b_test.go": hdr + "package p\n\nvar _ = pinned() + free()\n",
	})
	before := copyDir(t, dir)
	var sb strings.Builder
	if _, err := runBulkOn(dir, "", "area", map[string]bool{"pinned": true}, false, 10, &sb); err != nil {
		t.Fatalf("bulk: %v\n%s", err, sb.String())
	}
	a := read(t, dir, "a_test.go")
	for _, s := range []string{"func pinned()", "func inner()"} {
		if !strings.Contains(a, s) {
			t.Errorf("a_test.go lost %q:\n%s", s, a)
		}
	}
	h := read(t, dir, "a_helpers_test.go")
	if !strings.Contains(h, "func free()") || !strings.Contains(h, "func only()") || strings.Contains(h, "func pinned()") {
		t.Errorf("a_helpers_test.go:\n%s", h)
	}
	mustVerify(t, before, dir)
}

func TestReadKeep(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(p, []byte("# guard X\nalpha\n\n  beta  # trailing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	k, err := readKeep(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 2 || !k["alpha"] || !k["beta"] {
		t.Errorf("readKeep = %v", k)
	}
}
