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

//go:build !windows

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func writeSkillTestFile(t *testing.T, dir, name string, perm os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(name), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
}

func publishFilesByPath(files []publishFile) map[string]publishFile {
	got := map[string]publishFile{}
	for _, f := range files {
		got[f.path] = f
	}
	return got
}

// TestCollectPublishFiles_RegularFiles pins path, size and normalised mode
// for regular files, and that the publish excludes still apply.
func TestCollectPublishFiles_RegularFiles(t *testing.T) {
	dir := t.TempDir()
	writeSkillTestFile(t, dir, "SKILL.md", 0o644)
	writeSkillTestFile(t, dir, "scripts/run.sh", 0o755)
	writeSkillTestFile(t, dir, "data/group.txt", 0o664)
	writeSkillTestFile(t, dir, ".gitignore", 0o644)
	writeSkillTestFile(t, dir, ".DS_Store", 0o644)
	writeSkillTestFile(t, dir, ".git/config", 0o644)
	writeSkillTestFile(t, dir, "scripts/__pycache__/x.pyc", 0o644)

	var warn bytes.Buffer
	_, files, err := collectPublishFiles(dir, &warn)
	if err != nil {
		t.Fatal(err)
	}
	got := publishFilesByPath(files)
	want := map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755", "data/group.txt": "0644"}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for p, m := range want {
		f, ok := got[p]
		if !ok {
			t.Fatalf("%s not collected", p)
		}
		if f.mode != m {
			t.Errorf("%s: mode %q, want %q", p, f.mode, m)
		}
		if f.size != int64(len(p)) {
			t.Errorf("%s: size %d, want %d", p, f.size, len(p))
		}
		if want := transfer.HashBytes([]byte(p)); f.hash != want {
			t.Errorf("%s: hash %q, want %q", p, f.hash, want)
		}
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warnings: %q", warn.String())
	}
}

// TestCollectPublishFiles_SkipsSymlinks pins that only regular files inside
// the skill directory are published: a symlink to a file outside the skill
// directory, a symlink to a file inside it, and a symlinked directory are
// all skipped with a warning, as transfer.CollectFiles skips them.
func TestCollectPublishFiles_SkipsSymlinks(t *testing.T) {
	outside := t.TempDir()
	writeSkillTestFile(t, outside, "data.txt", 0o644)
	writeSkillTestFile(t, outside, "dir/inner.txt", 0o644)

	dir := t.TempDir()
	writeSkillTestFile(t, dir, "SKILL.md", 0o644)
	writeSkillTestFile(t, dir, "scripts/run.sh", 0o755)
	links := map[string]string{
		"outside.txt":   filepath.Join(outside, "data.txt"),
		"outside-rel":   filepath.Join("..", filepath.Base(outside), "data.txt"),
		"outside-dir":   filepath.Join(outside, "dir"),
		"inside.sh":     "scripts/run.sh",
		"scripts/up.md": "../SKILL.md",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	// The outside links really resolve to the outside file.
	outsideInfo, err := os.Stat(filepath.Join(outside, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"outside.txt", "outside-rel"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s does not resolve: %v", name, err)
		}
		if !os.SameFile(info, outsideInfo) {
			t.Fatalf("%s does not resolve to the outside file", name)
		}
	}

	var warn bytes.Buffer
	_, files, err := collectPublishFiles(dir, &warn)
	if err != nil {
		t.Fatal(err)
	}
	got := publishFilesByPath(files)
	if len(got) != 2 || got["SKILL.md"].path == "" || got["scripts/run.sh"].path == "" {
		t.Fatalf("collected %v, want only SKILL.md and scripts/run.sh", got)
	}
	if got["scripts/run.sh"].mode != "0755" {
		t.Errorf("scripts/run.sh: mode %q, want 0755", got["scripts/run.sh"].mode)
	}

	// Same file set as the template upload collector.
	ref, err := transfer.CollectFiles(dir, skillPublishExcludes)
	if err != nil {
		t.Fatal(err)
	}
	if len(ref) != len(files) {
		t.Errorf("collected %d files, transfer.CollectFiles collected %d", len(files), len(ref))
	}
	for i := range ref {
		if i < len(files) && (ref[i].Path != files[i].path || ref[i].Size != files[i].size) {
			t.Errorf("entry %d: got %s/%d, transfer.CollectFiles %s/%d", i, files[i].path, files[i].size, ref[i].Path, ref[i].Size)
		}
	}

	var warned []string
	for _, line := range strings.Split(strings.TrimSpace(warn.String()), "\n") {
		warned = append(warned, strings.TrimPrefix(line, "Warning: skipped non-regular file "))
	}
	sort.Strings(warned)
	want := make([]string, 0, len(links))
	for name := range links {
		want = append(want, strconv.Quote(name))
	}
	sort.Strings(want)
	if strings.Join(warned, ",") != strings.Join(want, ",") {
		t.Errorf("warnings for %v, want %v (output %q)", warned, want, warn.String())
	}
}

// TestCollectPublishFiles_SkillMDMustBeRegular pins that a symlinked
// SKILL.md is rejected rather than published without its content.
func TestCollectPublishFiles_SkillMDMustBeRegular(t *testing.T) {
	outside := t.TempDir()
	writeSkillTestFile(t, outside, "SKILL.md", 0o644)

	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	_, _, err := collectPublishFiles(dir, nil)
	if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("err = %v, want a regular-file error", err)
	}

	if _, _, err := collectPublishFiles(t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "SKILL.md not found") {
		t.Fatalf("err = %v, want SKILL.md not found", err)
	}
}

// TestCollectPublishFiles_SymlinkedRootArgument pins that the skill
// directory argument may itself be a symlink to the skill directory.
func TestCollectPublishFiles_SymlinkedRootArgument(t *testing.T) {
	realDir := t.TempDir()
	writeSkillTestFile(t, realDir, "SKILL.md", 0o644)
	writeSkillTestFile(t, realDir, "scripts/run.sh", 0o755)

	link := filepath.Join(t.TempDir(), "my-skill")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	root, files, err := collectPublishFiles(link, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if root != wantRoot {
		t.Errorf("root %q, want the resolved directory %q", root, wantRoot)
	}
	got := publishFilesByPath(files)
	if len(got) != 2 || got["scripts/run.sh"].mode != "0755" {
		t.Fatalf("collected %v, want SKILL.md and scripts/run.sh (0755)", got)
	}
}

// TestCollectPublishFiles_WarningQuotesPath pins that the skipped-file
// warning prints the relative path quoted, so control characters in a file
// name are escaped and each warning stays on one line.
func TestCollectPublishFiles_WarningQuotesPath(t *testing.T) {
	dir := t.TempDir()
	writeSkillTestFile(t, dir, "SKILL.md", 0o644)
	name := "odd\nname\x1b[0m"
	if err := os.Symlink("SKILL.md", filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	if _, _, err := collectPublishFiles(dir, &warn); err != nil {
		t.Fatal(err)
	}
	want := "Warning: skipped non-regular file " + strconv.Quote(name) + "\n"
	if warn.String() != want {
		t.Errorf("warning %q, want %q", warn.String(), want)
	}
}

// TestCollectPublishFiles_ExcludesTopLevelDotScion pins that a top-level
// .scion entry in a skill directory is not published (the shared
// transfer.DefaultExcludePatterns rule), while a nested .scion directory is
// ordinary skill content.
func TestCollectPublishFiles_ExcludesTopLevelDotScion(t *testing.T) {
	dir := t.TempDir()
	writeSkillTestFile(t, dir, "SKILL.md", 0o644)
	writeSkillTestFile(t, dir, ".scion/settings.yaml", 0o644)
	writeSkillTestFile(t, dir, "docs/.scion/notes.md", 0o644)

	_, files, err := collectPublishFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := publishFilesByPath(files)
	if len(got) != 2 || got["SKILL.md"].path == "" || got["docs/.scion/notes.md"].path == "" {
		t.Fatalf("collected %v, want SKILL.md and docs/.scion/notes.md only", got)
	}
}
