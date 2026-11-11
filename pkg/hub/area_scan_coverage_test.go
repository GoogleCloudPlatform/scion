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

package hub

// Scanner coverage of the pkg/hub area directories (ptone/scion#4275).
// hack/area-dirs.txt lists the directories; these tests prove the pkg/hub
// guard-scan helpers and the permissions Enforcement registry keep up with it.

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/hack/areadirs"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// areaCoverageModuleRoot returns the module root, two levels above pkg/hub.
func areaCoverageModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join(findHubDir(t), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// areaDirsMissingFromGuardScan returns the listed directories that
// guardScanSubdirs (subdirs) does not name, in list order.
func areaDirsMissingFromGuardScan(entries []areadirs.Entry, subdirs []string) []string {
	var missing []string
	for _, e := range entries {
		if !slices.Contains(subdirs, e.Dir) {
			missing = append(missing, string(e.Kind)+" "+e.Dir)
		}
	}
	return missing
}

// TestGuardScanSubdirsCoverAreas: every area and testsupport directory in
// hack/area-dirs.txt is scanned by the guard tests, so a move that lists its
// directory there must add it to guardScanSubdirs in the same PR.
func TestGuardScanSubdirsCoverAreas(t *testing.T) {
	entries, err := areadirs.Load(areaCoverageModuleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if missing := areaDirsMissingFromGuardScan(entries, guardScanSubdirs); len(missing) != 0 {
		t.Errorf("hack/area-dirs.txt entries missing from guardScanSubdirs (ast_test_helpers_test.go): %v", missing)
	}

	// The check must fail for a list with an unscanned area.
	extra, err := areadirs.ReadFile(filepath.Join("testdata", "guardscan", "area-extra.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := areaDirsMissingFromGuardScan(extra, guardScanSubdirs)
	if want := []string{"area zz_not_in_guard_scan_subdirs"}; !slices.Equal(got, want) {
		t.Errorf("extra-area fixture: missing = %v, want %v", got, want)
	}
}

// TestGuardScanHelpersReachArea plants a non-test file in each listed area
// directory of a temporary pkg/hub and checks that the guard-scan helpers
// return it: guardScanDirs lists the directory, readGuardScanDirs names the
// file by its slash path, and parseNonTestGoFiles parses it.
func TestGuardScanHelpersReachArea(t *testing.T) {
	entries, err := areadirs.Load(areaCoverageModuleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	for _, e := range entries {
		dir := filepath.Join(base, filepath.FromSlash(e.Dir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := "package " + path.Base(e.Dir) + "\n\nfunc zzPlantedGuard() {}\n"
		if err := os.WriteFile(filepath.Join(dir, "zz_planted_guard.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dirs := mustGuardScanDirs(t, base)
	read, err := readGuardScanDirs(base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range read {
		names = append(names, d.Name())
	}
	for _, e := range entries {
		rel := filepath.FromSlash(e.Dir)
		if !slices.Contains(dirs, rel) {
			t.Errorf("%s %s: guardScanDirs = %v, does not list it", e.Kind, e.Dir, dirs)
			continue
		}
		if want := e.Dir + "/zz_planted_guard.go"; !slices.Contains(names, want) {
			t.Errorf("%s %s: readGuardScanDirs does not return %s", e.Kind, e.Dir, want)
		}
		files, err := parseNonTestGoFiles(filepath.Join(base, rel))
		if err != nil {
			t.Errorf("%s %s: parseNonTestGoFiles: %v", e.Kind, e.Dir, err)
			continue
		}
		if len(files) != 1 || files[0].Name.Name != path.Base(e.Dir) {
			t.Errorf("%s %s: parseNonTestGoFiles returned %d file(s), want the planted file", e.Kind, e.Dir, len(files))
		}
	}
}

// staleEnforcementRef returns why the Enforcement reference ref
// ("<file>[:<symbol>]", file relative to the module root) is stale, or ""
// when its file exists. A pkg/hub file that is gone but whose name now
// exists in a listed area directory is reported as moved, naming the new
// path the entry must be rewritten to; any other absent file is missing.
func staleEnforcementRef(root, ref string, areas []string) string {
	file, _, _ := strings.Cut(ref, ":")
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
	if err == nil {
		return ""
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "unreadable file: " + err.Error()
	}
	if dir, name := path.Split(file); dir == "pkg/hub/" {
		for _, area := range areas {
			moved := "pkg/hub/" + area + "/" + name
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(moved))); err == nil {
				return "moved file: now " + moved
			}
		}
	}
	return "missing file"
}

// TestEnforcementPathsNotStale: no permissions Enforcement entry names a
// file that has moved to an area directory or is missing.
func TestEnforcementPathsNotStale(t *testing.T) {
	root := areaCoverageModuleRoot(t)
	entries, err := areadirs.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	areas := areadirs.Dirs(entries)
	refs := 0
	for _, p := range permissions.Registry {
		for _, ref := range p.Enforcement {
			refs++
			if why := staleEnforcementRef(root, ref, areas); why != "" {
				t.Errorf("%s: Enforcement entry %q: %s", p.ID, ref, why)
			}
		}
	}
	if refs == 0 {
		t.Fatal("permissions.Registry has no Enforcement entries; is the registry where this test expects it?")
	}

	t.Run("planted", func(t *testing.T) {
		tmp := t.TempDir()
		write := func(rel string) {
			p := filepath.Join(tmp, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("pkg/hub/kept.go")
		write("pkg/hub/" + areas[0] + "/zz_planted_moved.go")
		for ref, want := range map[string]string{
			"pkg/hub/kept.go:Evaluate":             "",
			"pkg/hub/zz_planted_moved.go:Evaluate": "moved file: now pkg/hub/" + areas[0] + "/zz_planted_moved.go",
			// Certainly absent, also in the real tree: no source file has
			// this name.
			"pkg/hub/zz_planted_absent_authz_kernel.go:Evaluate": "missing file",
		} {
			if got := staleEnforcementRef(tmp, ref, areas); got != want {
				t.Errorf("staleEnforcementRef(%q) = %q, want %q", ref, got, want)
			}
		}
		if got := staleEnforcementRef(root, "pkg/hub/zz_planted_absent_authz_kernel.go:Evaluate", areas); got != "missing file" {
			t.Errorf("planted absent entry in the real tree: got %q, want \"missing file\"", got)
		}
	})
}
