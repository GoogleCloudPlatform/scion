package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const fixtureRoot = "testdata/src"

var fixtureDirs = []string{"pkg/hub", "pkg/store"}

var wantRE = regexp.MustCompile(`// want ([a-z-]+)`)

// wantFindings reads the "// want <rule>" annotations from the fixtures.
func wantFindings(t *testing.T) []string {
	t.Helper()
	var want []string
	for _, d := range fixtureDirs {
		err := filepath.WalkDir(filepath.Join(fixtureRoot, d), func(path string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			rel, _ := filepath.Rel(fixtureRoot, path)
			sc := bufio.NewScanner(f)
			for n := 1; sc.Scan(); n++ {
				for _, m := range wantRE.FindAllStringSubmatch(sc.Text(), -1) {
					want = append(want, fmt.Sprintf("%s:%d:%s", filepath.ToSlash(rel), n, m[1]))
				}
			}
			return sc.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(want)
	return want
}

func got(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, fmt.Sprintf("%s:%d:%s", f.File, f.Line, f.Rule))
	}
	sort.Strings(out)
	return out
}

func TestScanFixtures(t *testing.T) {
	findings, n, err := Scan(fixtureRoot, fixtureDirs)
	if err != nil {
		t.Fatal(err)
	}
	// format.go, webchannel_store.go, webchannel_store_postgres.go and
	// predicates.go; the generated file and the _test.go file are skipped.
	if n != 4 {
		t.Errorf("scanned %d files, want 4", n)
	}
	want := wantFindings(t)
	if len(want) < 10 {
		t.Fatalf("only %d want annotations found; fixture parsing is broken", len(want))
	}
	g := got(findings)
	if strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings mismatch\n got:\n  %s\nwant:\n  %s", strings.Join(g, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestFindingText(t *testing.T) {
	findings, _, err := Scan(fixtureRoot, fixtureDirs)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]bool{}
	for _, f := range findings {
		texts[f.File+"|"+f.Func+"|"+f.Text] = true
	}
	for _, k := range []string{
		"pkg/hub/format.go|formatViolations|t.Format(time.RFC3339)",
		"pkg/hub/format.go|(package)|time.Now().Format(time.RFC3339)",
		"pkg/hub/webchannel_store.go|store.violations|ExecContext arg at",
		"pkg/hub/webchannel_store.go|store.violations|ExecContext arg now",
		"pkg/store/predicates.go|predicates|sql.GT(\"created\", cursor)",
	} {
		if !texts[k] {
			t.Errorf("missing finding %q", k)
		}
	}
}

func TestAllowlist(t *testing.T) {
	findings, _, err := Scan(fixtureRoot, fixtureDirs)
	if err != nil {
		t.Fatal(err)
	}
	allow, errs := ParseAllowlist(`
# comment
pkg/hub/format.go | formatViolations | t.Format(time.RFC3339) | fixture: allowed
pkg/hub/format.go | formatViolations | t.Format(  time.RFC3339Nano ) | fixture: stale, matches nothing
pkg/hub/format.go | formatViolations | t.Format(time.RFC3339) |
pkg/hub/format.go | only three fields
`)
	if len(errs) != 2 {
		t.Fatalf("parse errors = %v, want 2 (empty justification, too few fields)", errs)
	}
	if len(allow) != 2 {
		t.Fatalf("entries = %d, want 2", len(allow))
	}
	remaining, stale := ApplyAllowlist(findings, allow)
	// One entry covers every identical finding in the function: format.go
	// has t.Format(time.RFC3339) twice in formatViolations (one split over
	// three lines).
	if len(remaining) != len(findings)-2 {
		t.Errorf("remaining = %d, want %d", len(remaining), len(findings)-2)
	}
	if len(stale) != 1 || stale[0].Text != "t.Format(time.RFC3339Nano)" {
		t.Errorf("stale = %+v, want the RFC3339Nano entry", stale)
	}
}

func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/a/a.go", "package a\n\nimport \"time\"\n\nfunc f(t time.Time) string { return t.Format(time.RFC3339) }\n")
	write("allow.txt", "pkg/a/a.go | f | t.Format(time.RFC3339) | fixture\n")
	write("stale.txt", "pkg/a/a.go | g | t.Format(time.RFC3339) | fixture\n")
	write("empty/README", "")

	cases := []struct {
		name, allow string
		dirs        []string
		want        int
	}{
		{"violation", "", []string{"pkg/a"}, 1},
		{"allowlisted", "allow.txt", []string{"pkg/a"}, 0},
		{"stale entry", "stale.txt", []string{"pkg/a"}, 1},
		{"missing allowlist", "nope.txt", []string{"pkg/a"}, 3},
		{"missing dir", "", []string{"pkg/nope"}, 3},
		{"no go files", "", []string{"empty"}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(dir, tc.allow, tc.dirs); got != tc.want {
				t.Errorf("run = %d, want %d", got, tc.want)
			}
		})
	}
}
