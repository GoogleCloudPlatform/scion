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

// fixturePaths mirrors DefaultPaths inside the fixture tree.
var fixturePaths = []string{"cmd", "pkg/agent/list.go"}

var wantRE = regexp.MustCompile(`// want ([a-z-]+)`)

func inFixtureRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(fixtureRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// wantFindings reads the "// want <rule>" annotations from the scanned
// fixtures.
func wantFindings(t *testing.T, files []string) []string {
	t.Helper()
	var want []string
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			for _, m := range wantRE.FindAllStringSubmatch(sc.Text(), -1) {
				want = append(want, fmt.Sprintf("%s:%d:%s", path, n, m[1]))
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(want)
	return want
}

func TestScanFixtures(t *testing.T) {
	inFixtureRoot(t)
	files, findings, err := Scan(fixturePaths)
	if err != nil {
		t.Fatal(err)
	}

	wantFiles := []string{
		"cmd/clean.go",
		"cmd/sub/alias.go",
		"cmd/sub/notime.go",
		"cmd/violations.go",
		"pkg/agent/list.go",
	}
	if strings.Join(files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("scanned files = %v, want %v (test files and unlisted files must be skipped)", files, wantFiles)
	}

	var got []string
	for _, f := range findings {
		got = append(got, fmt.Sprintf("%s:%d:%s", f.File, f.Line, f.Rule))
	}
	sort.Strings(got)
	want := wantFindings(t, files)
	if len(want) == 0 {
		t.Fatal("fixtures have no // want annotations")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings mismatch\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestNoCandidateFiles(t *testing.T) {
	dir := t.TempDir()
	files, findings, err := Scan([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 || len(findings) != 0 {
		t.Errorf("empty dir: files=%v findings=%v", files, findings)
	}
}

func TestMissingPathIsAnError(t *testing.T) {
	if _, _, err := Scan([]string{filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("Scan(missing path) returned no error")
	}
}

func TestParseErrorIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package x\nfunc {"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Scan([]string{dir}); err == nil {
		t.Error("Scan(unparseable file) returned no error")
	}
}

func TestIsZonelessLayout(t *testing.T) {
	tests := map[string]bool{
		"2006-01-02":                true,
		"2006-01-02 15:04":          true,
		"15:04:05":                  true,
		"3:04PM":                    true,
		"2006-01-02 15:04:05 MST":   false,
		"2006-01-02T15:04:05Z07:00": false,
		"15:04 -0700":               false,
		"15:04 -07:00":              false,
		"15:04 -07":                 false,
		"15:04 Z0700":               false,
		"15:04 Z07":                 false,
		"hello world":               false,
		"port 8080":                 false,
	}
	for s, want := range tests {
		if got := IsZonelessLayout(s); got != want {
			t.Errorf("IsZonelessLayout(%q) = %v, want %v", s, got, want)
		}
	}
}
